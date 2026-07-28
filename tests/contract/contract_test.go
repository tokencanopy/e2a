//go:build integration

// Package contract contains behavioral contract tests for the public /v1 API.
//
// These tests verify that the Go API server behaves as documented: correct
// response shapes, read-state transitions, auth enforcement, domain verification
// flow, and WebSocket notification protocol.
//
// The authoritative contract scenarios are defined in scenarios.yaml (language-agnostic
// YAML fixture). This Go runner loads and executes that file directly; TypeScript and
// Python SDK runners will consume the same scenarios.yaml in later phases.
//
// Requires a running test database (same as integration tests).
package contract

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/webhookpub"
	"github.com/tokencanopy/e2a/internal/ws"
	"gopkg.in/yaml.v3"
	"nhooyr.io/websocket"
)

// ── YAML schema ────────────────────────────────────────────────────

type scenarioFile struct {
	Scenarios []scenario `yaml:"scenarios"`
}

type scenario struct {
	Name         string      `yaml:"name"`
	Description  string      `yaml:"description"`
	AuthOverride *string     `yaml:"auth_override,omitempty"`
	Setup        []setupStep `yaml:"setup,omitempty"`
	Steps        []step      `yaml:"steps"`
	Cleanup      []step      `yaml:"cleanup,omitempty"`
}

type setupStep struct {
	RegisterDomain string        `yaml:"register_domain,omitempty"`
	VerifyDomain   string        `yaml:"verify_domain,omitempty"`
	RegisterAgent  *agentSetup   `yaml:"register_agent,omitempty"`
	InjectMessage  *messageSetup `yaml:"inject_message,omitempty"`
}

type agentSetup struct {
	Email string `yaml:"email"`
}

type messageSetup struct {
	AgentEmail string `yaml:"agent_email"`
	From       string `yaml:"from"`
	Subject    string `yaml:"subject"`
}

type step struct {
	ID           string                 `yaml:"id"`
	Action       string                 `yaml:"action"`
	Method       string                 `yaml:"method,omitempty"`
	Path         string                 `yaml:"path,omitempty"`
	Body         map[string]interface{} `yaml:"body,omitempty"`
	AuthOverride *string                `yaml:"auth_override,omitempty"`
	AgentEmail   string                 `yaml:"agent_email,omitempty"`
	From         string                 `yaml:"from,omitempty"`
	Subject      string                 `yaml:"subject,omitempty"`
	VerifyDomain string                 `yaml:"verify_domain,omitempty"`
	Expect       *expectation           `yaml:"expect,omitempty"`
	// Capture extracts values from the response and stores them as
	// placeholders for later steps. Keys are the placeholder names
	// (without curly braces); values are dotted JSON paths into the
	// response body. After this step runs, later steps can reference
	// the captured value as `{name}` in `path` / `body` / `expect`.
	Capture map[string]string `yaml:"capture,omitempty"`
}

type expectation struct {
	Status            int                               `yaml:"status,omitempty"`
	BodyContains      []string                          `yaml:"body_contains,omitempty"`
	BodyMatch         map[string]interface{}            `yaml:"body_match,omitempty"`
	BodyArrayContains map[string]map[string]interface{} `yaml:"body_array_contains,omitempty"`
	BodyExcludes      []string                          `yaml:"body_excludes,omitempty"`
	// WS-specific
	FieldsPresent []string               `yaml:"fields_present,omitempty"`
	FieldsAbsent  []string               `yaml:"fields_absent,omitempty"`
	FieldMatch    map[string]interface{} `yaml:"field_match,omitempty"`
}

// ── Test environment ───────────────────────────────────────────────

type testEnv struct {
	baseURL string
	store   *identity.Store
	outbox  webhookpub.Outbox
	wsHub   *ws.Hub
	apiKey  string
	userID  string
}

func setupEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()

	// Use the /v1-wrapped contract harness (chi root owning /v1 with the
	// legacy mux as fallback) so scenarios exercise the real typed /v1
	// surface — the same handler the production binary serves. It owns its
	// own DB pool + user + API key; Close truncates and tears everything down.
	cs, err := testutil.StartContractServer(ctx, testutil.TestDBURL())
	if err != nil {
		t.Skipf("contract server not available: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close(context.Background()) })

	return &testEnv{
		baseURL: cs.BaseURL,
		store:   cs.Store,
		outbox:  webhookpub.NewOutbox(cs.DBPool, webhookpub.StaticFlag(true)),
		wsHub:   cs.WSHub,
		apiKey:  cs.APIKey,
		userID:  cs.UserID,
	}
}

func (e *testEnv) waitWSConnected(t *testing.T, agentID string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !e.wsHub.IsConnected(agentID) {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for agent %s to register in WS hub", agentID)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (e *testEnv) waitWSDisconnected(t *testing.T, agentID string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for e.wsHub.IsConnected(agentID) {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for agent %s to unregister from WS hub", agentID)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (e *testEnv) injectInboundMessage(t *testing.T, agentEmail, from, subject string) string {
	t.Helper()
	ctx := context.Background()
	ag, err := e.store.GetAgentByEmail(ctx, agentEmail)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	// Populate canonical SMTP authentication evidence so the contract exercises
	// the same shape as a real received message.
	raw := []byte("From: " + from + "\r\nTo: " + agentEmail + "\r\nSubject: " + subject + "\r\n\r\nbody\r\n")
	messageID := identity.NewMessageID()
	fromDomain := strings.SplitN(from, "@", 2)[1]
	aligned := true
	policy := emailauth.DMARCPolicyReject
	authentication := &emailauth.Authentication{
		SPF:   emailauth.SPFResult{Status: emailauth.StatusPass, Domain: &fromDomain, Aligned: &aligned},
		DKIM:  []emailauth.DKIMResult{},
		DMARC: emailauth.DMARCResult{Status: emailauth.StatusPass, Domain: &fromDomain, Policy: &policy, AlignedBy: []emailauth.AlignmentMechanism{emailauth.AlignedBySPF}},
	}
	to := []string{agentEmail}
	var msg *identity.Message
	// Match the production inbound invariant: the inbox row and its canonical
	// email.received envelope become durable in the same transaction.
	err = e.store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		msg, txErr = e.store.CreateInboundMessageAuthenticatedInTx(
			ctx, tx, messageID, ag.ID, identity.InboundAuth{HeaderFrom: from, EnvelopeFrom: from, Authentication: authentication}, agentEmail, "", subject, "", "unread",
			raw, false, "", to, nil, nil, identity.InboundScreening{},
		)
		if txErr != nil {
			return txErr
		}

		event := webhookpub.NewEvent(webhookpub.EventEmailReceived, ag.UserID, eventpayload.EmailReceivedData{
			MessageID:      msg.ID,
			AgentEmail:     agentEmail,
			Direction:      "inbound",
			HeaderFrom:     &from,
			EnvelopeFrom:   &from,
			VerifiedDomain: authentication.VerifiedDomain(),
			To:             to,
			CC:             []string{},
			ReplyTo:        []string{},
			Authentication: authentication,
			DeliveredTo:    agentEmail,
			Subject:        subject,
			ReceivedAt:     msg.CreatedAt.UTC(),
			Attachments:    eventpayload.AttachmentMetadata(raw),
		})
		event.ID = webhookpub.DeterministicEventID(msg.ID, webhookpub.EventEmailReceived)
		event.AgentID = ag.ID
		event.MessageID = msg.ID
		return e.outbox.PublishTx(ctx, tx, event)
	})
	if err != nil {
		t.Fatalf("store message: %v", err)
	}
	return msg.ID
}

func TestInjectInboundMessagePersistsDurableEnvelope(t *testing.T) {
	env := setupEnv(t)
	r := newRunner(env, scenario{})

	if _, err := env.store.ClaimOrCreateDomain(context.Background(), "ws-envelope.test.dev", env.userID); err != nil {
		t.Fatalf("claim domain: %v", err)
	}
	if err := env.store.VerifyDomain(context.Background(), "ws-envelope.test.dev", env.userID); err != nil {
		t.Fatalf("verify domain: %v", err)
	}
	r.setupRegisterAgent(t, &agentSetup{Email: "bot@ws-envelope.test.dev"})

	msgID := env.injectInboundMessage(t, "bot@ws-envelope.test.dev", "alice@example.com", "WS envelope test")
	envelope, err := env.store.GetEventEnvelope(context.Background(), msgID, webhookpub.EventEmailReceived)
	if err != nil {
		t.Fatalf("get durable email.received envelope: %v", err)
	}
	if len(envelope) == 0 {
		t.Fatal("durable email.received envelope is empty")
	}
	var event struct {
		Data struct {
			MessageID      string                    `json:"message_id"`
			HeaderFrom     *string                   `json:"header_from"`
			Authentication *emailauth.Authentication `json:"authentication"`
		} `json:"data"`
	}
	if err := json.Unmarshal(envelope, &event); err != nil {
		t.Fatalf("decode durable email.received envelope: %v", err)
	}
	if event.Data.MessageID != msgID {
		t.Fatalf("event message_id = %q, want %q", event.Data.MessageID, msgID)
	}
	if event.Data.HeaderFrom == nil || *event.Data.HeaderFrom != "alice@example.com" {
		t.Fatalf("header_from = %v", event.Data.HeaderFrom)
	}
	if event.Data.Authentication == nil || event.Data.Authentication.DMARC.Status != emailauth.StatusPass {
		t.Fatalf("authentication = %#v, want DMARC pass", event.Data.Authentication)
	}
}

// ── Scenario runner ────────────────────────────────────────────────

type runner struct {
	env     *testEnv
	sc      scenario
	vars    map[string]string
	wsConn  *websocket.Conn
	wsAgent string // agent ID for hub polling
}

func newRunner(env *testEnv, sc scenario) *runner {
	tokenBytes := make([]byte, 6)
	if _, err := rand.Read(tokenBytes); err != nil {
		panic(fmt.Sprintf("generate contract scenario token: %v", err))
	}
	future := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	return &runner{
		env: env,
		sc:  sc,
		vars: map[string]string{
			"future_rfc3339": future.Format(time.RFC3339),
			"scenario_token": hex.EncodeToString(tokenBytes),
		},
	}
}

func TestRunnerInitializesDynamicScenarioVars(t *testing.T) {
	before := time.Now().UTC()
	env := &testEnv{baseURL: "https://contract.test", apiKey: "key"}
	first := newRunner(env, scenario{})
	second := newRunner(env, scenario{})

	futureValue := first.resolve("{future_rfc3339}")
	future, err := time.Parse(time.RFC3339, futureValue)
	if err != nil {
		t.Fatalf("future_rfc3339 = %q: %v", futureValue, err)
	}
	if futureValue != first.resolve("{future_rfc3339}") {
		t.Fatal("future_rfc3339 changed within one scenario")
	}
	if future.Before(before.Add(4*time.Minute)) || future.After(time.Now().UTC().Add(6*time.Minute)) {
		t.Fatalf("future_rfc3339 = %s, want approximately five minutes ahead", futureValue)
	}
	if future.Nanosecond() != 0 {
		t.Fatalf("future_rfc3339 = %s, want whole-second precision", futureValue)
	}

	firstToken := first.resolve("{scenario_token}")
	secondToken := second.resolve("{scenario_token}")
	if len(firstToken) != 12 {
		t.Fatalf("scenario_token length = %d, want 12", len(firstToken))
	}
	if _, err := strconv.ParseUint(firstToken, 16, 64); err != nil {
		t.Fatalf("scenario_token = %q, want lowercase hex: %v", firstToken, err)
	}
	if firstToken == secondToken {
		t.Fatalf("scenario_token collision: %q", firstToken)
	}
}

func (r *runner) resolve(s string) string {
	s = strings.ReplaceAll(s, "{base_url}", r.env.baseURL)
	s = strings.ReplaceAll(s, "{api_key}", r.env.apiKey)
	for k, v := range r.vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

func (r *runner) resolveValue(v interface{}) interface{} {
	switch x := v.(type) {
	case string:
		return r.resolve(x)
	case []interface{}:
		resolved := make([]interface{}, len(x))
		for i, item := range x {
			resolved[i] = r.resolveValue(item)
		}
		return resolved
	case map[string]interface{}:
		resolved := make(map[string]interface{}, len(x))
		for k, item := range x {
			resolved[k] = r.resolveValue(item)
		}
		return resolved
	case map[interface{}]interface{}:
		resolved := make(map[string]interface{}, len(x))
		for k, item := range x {
			resolved[fmt.Sprint(k)] = r.resolveValue(item)
		}
		return resolved
	default:
		return v
	}
}

// authHeader returns the Authorization header value for this step.
// Returns ("", false) for no auth, (value, true) for a specific header.
func (r *runner) authHeader(s *step) (string, bool) {
	override := r.sc.AuthOverride
	if s.AuthOverride != nil {
		override = s.AuthOverride
	}
	if override != nil {
		if *override == "none" {
			return "", false
		}
		return r.resolve(*override), true
	}
	return "Bearer " + r.env.apiKey, true
}

func (r *runner) executeSetup(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, s := range r.sc.Setup {
		switch {
		case s.RegisterDomain != "":
			domain := r.resolve(s.RegisterDomain)
			if _, err := r.env.store.ClaimOrCreateDomain(ctx, domain, r.env.userID); err != nil {
				t.Fatalf("setup register_domain %s: %v", domain, err)
			}
		case s.VerifyDomain != "":
			domain := r.resolve(s.VerifyDomain)
			if err := r.env.store.VerifyDomain(ctx, domain, r.env.userID); err != nil {
				t.Fatalf("setup verify_domain %s: %v", domain, err)
			}
		case s.RegisterAgent != nil:
			r.setupRegisterAgent(t, s.RegisterAgent)
		case s.InjectMessage != nil:
			msgID := r.env.injectInboundMessage(
				t,
				r.resolve(s.InjectMessage.AgentEmail),
				r.resolve(s.InjectMessage.From),
				r.resolve(s.InjectMessage.Subject),
			)
			r.vars["injected_message_id"] = msgID
		}
	}
}

func (r *runner) setupRegisterAgent(t *testing.T, a *agentSetup) {
	t.Helper()
	email := r.resolve(a.Email)
	body, _ := json.Marshal(map[string]string{
		"email": email,
	})
	req, _ := http.NewRequest("POST", r.env.baseURL+"/v1/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+r.env.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("setup register_agent: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup register_agent: status %d; body: %s", resp.StatusCode, respBody)
	}
	r.vars["agent_email"] = email
}

func (r *runner) executeSteps(t *testing.T) {
	for _, s := range r.sc.Steps {
		s := s
		switch s.Action {
		case "request":
			r.execRequest(t, &s)
		case "inject_message":
			r.execInjectMessage(t, &s)
		case "ws_connect":
			r.execWSConnect(t, &s)
		case "ws_reconnect":
			r.execWSReconnect(t, &s)
		case "ws_read":
			r.execWSRead(t, &s)
		case "verify_and_retry":
			r.execVerifyAndRetry(t, &s)
		default:
			t.Fatalf("step %s: unknown action %q", s.ID, s.Action)
		}
	}
}

func (r *runner) cleanup(t *testing.T) {
	t.Helper()
	for _, err := range r.cleanupErrors() {
		t.Errorf("cleanup %v", err)
	}
}

func (r *runner) cleanupErrors() []error {
	var errors []error
	if r.wsConn != nil {
		r.wsConn.Close(websocket.StatusNormalClosure, "")
		r.wsConn = nil
	}
	for i := range r.sc.Cleanup {
		s := &r.sc.Cleanup[i]
		if s.Action != "request" {
			errors = append(errors, fmt.Errorf("step %s: only request actions are supported", s.ID))
			continue
		}
		if err := r.execRequestError(s); err != nil {
			errors = append(errors, err)
		}
	}
	return errors
}

// ── Step executors ─────────────────────────────────────────────────

func (r *runner) execRequest(t *testing.T, s *step) {
	t.Helper()
	if err := r.execRequestError(s); err != nil {
		t.Fatal(err)
	}
}

func (r *runner) execRequestError(s *step) error {
	path := r.resolve(s.Path)

	var bodyReader io.Reader
	if s.Body != nil {
		data, _ := json.Marshal(r.resolveValue(s.Body))
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(s.Method, r.env.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("step %s: create request: %w", s.ID, err)
	}
	if auth, ok := r.authHeader(s); ok {
		req.Header.Set("Authorization", auth)
	}
	if s.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("step %s: %s %s: %w", s.ID, s.Method, path, err)
	}
	defer resp.Body.Close()

	rawBody, _ := io.ReadAll(resp.Body)

	if s.Expect == nil {
		return nil
	}

	// Status assertion
	if s.Expect.Status != 0 && resp.StatusCode != s.Expect.Status {
		return fmt.Errorf("step %s: status = %d, want %d; body: %s", s.ID, resp.StatusCode, s.Expect.Status, rawBody)
	}

	// JSON body assertions
	if len(s.Expect.BodyContains) == 0 && len(s.Expect.BodyMatch) == 0 && len(s.Expect.BodyArrayContains) == 0 && len(s.Expect.BodyExcludes) == 0 && len(s.Capture) == 0 {
		return nil
	}

	var jsonBody map[string]interface{}
	if err := json.Unmarshal(rawBody, &jsonBody); err != nil {
		return fmt.Errorf("step %s: unmarshal response: %w; raw: %s", s.ID, err, rawBody)
	}

	for _, key := range s.Expect.BodyContains {
		resolvedKey := r.resolve(key)
		if _, ok := jsonBody[resolvedKey]; !ok {
			return fmt.Errorf("step %s: expected key %q in response", s.ID, resolvedKey)
		}
	}
	for _, key := range s.Expect.BodyExcludes {
		resolvedKey := r.resolve(key)
		if _, ok := jsonBody[resolvedKey]; ok {
			return fmt.Errorf("step %s: unexpected key %q in response", s.ID, resolvedKey)
		}
	}
	for path, expected := range s.Expect.BodyMatch {
		resolvedPath := r.resolve(path)
		actual, found := jsonPathGet(jsonBody, resolvedPath)
		if !found {
			return fmt.Errorf("step %s: path %q not found in response: %s", s.ID, resolvedPath, rawBody)
		}
		resolvedExpected := r.resolveValue(expected)
		if !valuesEqual(actual, resolvedExpected) {
			return fmt.Errorf("step %s: path %q = %v (%T), want %v (%T)", s.ID, resolvedPath, actual, actual, resolvedExpected, resolvedExpected)
		}
	}
	for path, expectedFields := range s.Expect.BodyArrayContains {
		resolvedPath := r.resolve(path)
		rawItems, found := jsonPathGet(jsonBody, resolvedPath)
		if !found {
			return fmt.Errorf("step %s: array path %q not found in response: %s", s.ID, resolvedPath, rawBody)
		}
		items, ok := rawItems.([]interface{})
		if !ok {
			return fmt.Errorf("step %s: path %q is not an array", s.ID, resolvedPath)
		}
		matched := false
		for _, rawItem := range items {
			item, ok := rawItem.(map[string]interface{})
			if !ok {
				continue
			}
			itemMatches := true
			for field, expected := range expectedFields {
				actual, found := jsonPathGet(item, r.resolve(field))
				if !found || !valuesEqual(actual, r.resolveValue(expected)) {
					itemMatches = false
					break
				}
			}
			if itemMatches {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("step %s: path %q has no item matching %v", s.ID, resolvedPath, expectedFields)
		}
	}

	// Capture phase — done after assertions so a captured value is
	// only stored when the step's contract held. Values are stringified
	// because that's the form the placeholder resolver works with;
	// downstream `body_match` comparisons coerce via valuesEqual so
	// types still round-trip correctly when needed.
	for name, srcPath := range s.Capture {
		resolvedSrc := r.resolve(srcPath)
		raw, found := jsonPathGet(jsonBody, resolvedSrc)
		if !found {
			return fmt.Errorf("step %s: capture path %q not found in response: %s", s.ID, resolvedSrc, rawBody)
		}
		r.vars[name] = fmt.Sprint(raw)
	}
	return nil
}

func (r *runner) execInjectMessage(t *testing.T, s *step) {
	t.Helper()
	msgID := r.env.injectInboundMessage(t, r.resolve(s.AgentEmail), r.resolve(s.From), r.resolve(s.Subject))
	r.vars["injected_message_id"] = msgID
}

func (r *runner) execWSConnect(t *testing.T, s *step) {
	t.Helper()

	path := r.resolve(s.Path)

	// Extract agent email from WS path (e.g., /v1/agents/bot@ws.test.dev/ws)
	agentEmail := extractEmailFromWSPath(path)
	ag, err := r.env.store.GetAgentByEmail(context.Background(), agentEmail)
	if err != nil {
		t.Fatalf("step %s: get agent %s: %v", s.ID, agentEmail, err)
	}
	r.wsAgent = ag.ID

	wsURL := "ws" + strings.TrimPrefix(r.env.baseURL, "http") + path
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, wsDialOpts(r.env.apiKey))
	if err != nil {
		t.Fatalf("step %s: ws dial: %v", s.ID, err)
	}
	r.wsConn = conn
	r.env.waitWSConnected(t, r.wsAgent)
}

// wsDialOpts authenticates the WS handshake with Authorization: Bearer (the
// credential is never in the URL — header-only auth).
func wsDialOpts(apiKey string) *websocket.DialOptions {
	return &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + apiKey}}}
}

func (r *runner) execWSReconnect(t *testing.T, s *step) {
	t.Helper()
	if r.wsConn != nil {
		r.wsConn.Close(websocket.StatusNormalClosure, "")
		r.wsConn = nil
	}
	r.env.waitWSDisconnected(t, r.wsAgent)

	path := r.resolve(s.Path)
	wsURL := "ws" + strings.TrimPrefix(r.env.baseURL, "http") + path
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, wsDialOpts(r.env.apiKey))
	if err != nil {
		t.Fatalf("step %s: ws reconnect: %v", s.ID, err)
	}
	r.wsConn = conn
}

func (r *runner) execWSRead(t *testing.T, s *step) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, data, err := r.wsConn.Read(ctx)
	if err != nil {
		t.Fatalf("step %s: ws read: %v", s.ID, err)
	}

	var notif map[string]interface{}
	if err := json.Unmarshal(data, &notif); err != nil {
		t.Fatalf("step %s: unmarshal notification: %v", s.ID, err)
	}

	if s.Expect == nil {
		return
	}
	// WS frames are the versioned event envelope; assertions use dot-paths
	// ("data.message_id") resolved with the same evaluator as body_match.
	for _, field := range s.Expect.FieldsPresent {
		resolvedField := r.resolve(field)
		if _, ok := jsonPathGet(notif, resolvedField); !ok {
			t.Fatalf("step %s: expected field %q in notification: %s", s.ID, resolvedField, data)
		}
	}
	for _, field := range s.Expect.FieldsAbsent {
		resolvedField := r.resolve(field)
		if _, ok := jsonPathGet(notif, resolvedField); ok {
			t.Fatalf("step %s: unexpected field %q in notification", s.ID, resolvedField)
		}
	}
	for key, expected := range s.Expect.FieldMatch {
		resolvedKey := r.resolve(key)
		actual, ok := jsonPathGet(notif, resolvedKey)
		if !ok {
			t.Fatalf("step %s: field %q not found in notification", s.ID, resolvedKey)
		}
		resolvedExpected := r.resolveValue(expected)
		if !valuesEqual(actual, resolvedExpected) {
			t.Fatalf("step %s: field %q = %v, want %v", s.ID, resolvedKey, actual, resolvedExpected)
		}
	}
}

func (r *runner) execVerifyAndRetry(t *testing.T, s *step) {
	t.Helper()
	ctx := context.Background()
	domain := r.resolve(s.VerifyDomain)
	if err := r.env.store.VerifyDomain(ctx, domain, r.env.userID); err != nil {
		t.Fatalf("step %s: verify domain %s: %v", s.ID, domain, err)
	}
	r.execRequest(t, &step{
		ID:     s.ID,
		Action: "request",
		Method: s.Method,
		Path:   s.Path,
		Body:   s.Body,
		Expect: s.Expect,
	})
}

// ── JSON path evaluator ───────────────────────────────────────────

// jsonPathGet resolves dot-notation paths like "agents[0].email" or "agents.length".
func jsonPathGet(obj map[string]interface{}, path string) (interface{}, bool) {
	parts := strings.Split(path, ".")
	var current interface{} = obj

	for _, part := range parts {
		if part == "length" {
			arr, ok := current.([]interface{})
			if !ok {
				return nil, false
			}
			return len(arr), true
		}

		if idx := strings.Index(part, "["); idx != -1 {
			name := part[:idx]
			idxStr := part[idx+1 : len(part)-1]
			arrIdx, _ := strconv.Atoi(idxStr)

			m, ok := current.(map[string]interface{})
			if !ok {
				return nil, false
			}
			arr, ok := m[name].([]interface{})
			if !ok || arrIdx >= len(arr) {
				return nil, false
			}
			current = arr[arrIdx]
		} else {
			m, ok := current.(map[string]interface{})
			if !ok {
				return nil, false
			}
			val, exists := m[part]
			if !exists {
				return nil, false
			}
			current = val
		}
	}
	return current, true
}

// valuesEqual compares a JSON-decoded value with a YAML-decoded expected value,
// handling cross-type numeric comparison (JSON float64 vs YAML int).
func valuesEqual(jsonVal, yamlVal interface{}) bool {
	switch yv := yamlVal.(type) {
	case int:
		switch jv := jsonVal.(type) {
		case float64:
			return jv == float64(yv)
		case int:
			return jv == yv
		}
	case bool:
		jv, ok := jsonVal.(bool)
		return ok && jv == yv
	case string:
		jv, ok := jsonVal.(string)
		return ok && jv == yv
	}
	return fmt.Sprintf("%v", jsonVal) == fmt.Sprintf("%v", yamlVal)
}

// extractEmailFromWSPath extracts the agent email from a WS path like
// /v1/agents/bot@ws.test.dev/ws
func extractEmailFromWSPath(path string) string {
	// Path format: /v1/agents/{email}/ws (the legacy /api/v1 surface is gone).
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == "agents" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// ── Entry point ────────────────────────────────────────────────────

func loadScenarios(t *testing.T) []scenario {
	t.Helper()
	data, err := os.ReadFile("scenarios.yaml")
	if err != nil {
		t.Fatalf("load scenarios.yaml: %v", err)
	}
	var sf scenarioFile
	if err := yaml.Unmarshal(data, &sf); err != nil {
		t.Fatalf("parse scenarios.yaml: %v", err)
	}
	return sf.Scenarios
}

func TestScenarios(t *testing.T) {
	scenarios := loadScenarios(t)
	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			env := setupEnv(t)
			r := newRunner(env, sc)
			t.Cleanup(func() { r.cleanup(t) })
			r.executeSetup(t)
			r.executeSteps(t)
		})
	}
}
