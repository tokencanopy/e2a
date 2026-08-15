package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// pathRecordingIdem captures the route (path) handed to Claim so a test can
// assert that two agents land under distinct idempotency routes.
type pathRecordingIdem struct{ paths []string }

func (f *pathRecordingIdem) Claim(ctx context.Context, userID, key, path, bodyHash string) (idempotency.ClaimResult, error) {
	f.paths = append(f.paths, path)
	return idempotency.ClaimResult{Outcome: idempotency.OutcomeAcquired}, nil
}
func (f *pathRecordingIdem) Complete(ctx context.Context, userID, key string, _ idempotency.ClaimToken, resp idempotency.CachedResponse) error {
	return nil
}
func (f *pathRecordingIdem) CompleteTx(ctx context.Context, tx pgx.Tx, userID, key string, _ idempotency.ClaimToken, resp idempotency.CachedResponse) error {
	return nil
}
func (f *pathRecordingIdem) Release(ctx context.Context, userID, key string, _ idempotency.ClaimToken) error {
	return nil
}

// TestSendIdempotencyRouteIncludesAgent is the regression for the cross-agent
// replay hole introduced by moving the sender from the body (`from`) to the
// path: two agents owned by the same user, reusing ONE Idempotency-Key + an
// identical body, must not collide. Since the agent is no longer in the body,
// the dedup body-hash alone wouldn't separate them — handleCreateMessage folds
// the agent id into the idempotency route, so the two claims land under
// different routes (→ different hash → a mismatch/422, never agent A's cached
// response replayed for agent B).
func TestSendIdempotencyRouteIncludesAgent(t *testing.T) {
	rec := &pathRecordingIdem{}
	agents := map[string]*identity.AgentIdentity{
		"a@acme.com": {ID: "a@acme.com", Email: "a@acme.com", UserID: "u_1", DomainVerified: true},
		"b@acme.com": {ID: "b@acme.com", Email: "b@acme.com", UserID: "u_1", DomainVerified: true},
	}
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) { return &identity.User{ID: "u_1"}, nil },
		GetAgent: func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			if a, ok := agents[address]; ok {
				return a, nil
			}
			return nil, errors.New("not found")
		},
		Idempotency: rec,
		DeliverOutbound: func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, req outbound.SendRequest, mt, rt string, ref *identity.Message, ic agent.AcceptIdemCompleter) (*agent.OutboundResult, *agent.OutboundError) {
			return &agent.OutboundResult{MessageID: "m", Method: "smtp"}, nil
		},
	}))
	t.Cleanup(srv.Close)

	send := func(addr string) {
		b, _ := json.Marshal(map[string]any{"to": []string{"x@y.com"}, "subject": "Hi", "text": "hello"})
		req, _ := http.NewRequest("POST", srv.URL+"/v1/agents/"+addr+"/messages", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer good")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "dupkey") // SAME key for both agents
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	send("a%40acme.com")
	send("b%40acme.com")

	if len(rec.paths) != 2 {
		t.Fatalf("want 2 idempotency claims, got %d (%v)", len(rec.paths), rec.paths)
	}
	if rec.paths[0] == rec.paths[1] {
		t.Fatalf("two agents reusing one key must claim under DIFFERENT routes; both = %q", rec.paths[0])
	}
	if !strings.Contains(rec.paths[0], "a@acme.com") || !strings.Contains(rec.paths[1], "b@acme.com") {
		t.Errorf("idempotency route should fold in the agent id, got %v", rec.paths)
	}
}

// memIdem is an in-memory IdemStore with the real Claim/Complete/Release
// semantics (acquired → in-flight → completed; replay on hash match) so tests
// can exercise a genuine keyed retry end to end.
type memIdem struct {
	mu   sync.Mutex
	rows map[string]*memIdemRow
}

type memIdemRow struct {
	hash string
	done bool
	resp idempotency.CachedResponse
}

func newMemIdem() *memIdem { return &memIdem{rows: map[string]*memIdemRow{}} }

func (m *memIdem) Claim(ctx context.Context, userID, key, path, bodyHash string) (idempotency.ClaimResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := userID + "\x00" + key
	if row, ok := m.rows[k]; ok {
		switch {
		case !row.done:
			return idempotency.ClaimResult{Outcome: idempotency.OutcomeInFlight}, nil
		case row.hash != bodyHash:
			return idempotency.ClaimResult{Outcome: idempotency.OutcomeMismatch}, nil
		default:
			return idempotency.ClaimResult{Outcome: idempotency.OutcomeReplay, Cached: row.resp}, nil
		}
	}
	m.rows[k] = &memIdemRow{hash: bodyHash}
	return idempotency.ClaimResult{Outcome: idempotency.OutcomeAcquired}, nil
}

func (m *memIdem) Complete(ctx context.Context, userID, key string, _ idempotency.ClaimToken, resp idempotency.CachedResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if row, ok := m.rows[userID+"\x00"+key]; ok && !row.done {
		row.done, row.resp = true, resp
	}
	return nil
}

// CompleteTx mirrors Complete for the in-memory fake (tx is irrelevant here).
// Like the real store, completion is in_progress→completed only: a later
// post-hoc Complete cannot overwrite the response committed in the business tx.
func (m *memIdem) CompleteTx(ctx context.Context, tx pgx.Tx, userID, key string, token idempotency.ClaimToken, resp idempotency.CachedResponse) error {
	return m.Complete(ctx, userID, key, token, resp)
}

func (m *memIdem) Release(ctx context.Context, userID, key string, _ idempotency.ClaimToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, userID+"\x00"+key)
	return nil
}

// TestSendTemplateIdempotentRetryAfterTemplateDelete pins the claim-before-
// resolve ordering: a keyed templated send succeeds, the template is then
// deleted, and a byte-identical retry with the same key must REPLAY the
// cached success — never 404 template_not_found, never a second delivery.
// (Template resolution consults mutable state, so it must run inside the
// idempotent execution, after the Claim/replay handshake.)
func TestSendTemplateIdempotentRetryAfterTemplateDelete(t *testing.T) {
	templates := map[string]*identity.Template{
		"tmpl_1": {ID: "tmpl_1", UserID: "u_1", Name: "T", Subject: "Hello {{name}}", Body: "Hi {{name}}."},
	}
	deliveries := 0
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) { return &identity.User{ID: "u_1"}, nil },
		GetAgent: func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			return &identity.AgentIdentity{ID: address, Email: address, UserID: "u_1", DomainVerified: true}, nil
		},
		GetTemplate: func(ctx context.Context, templateID, userID string) (*identity.Template, error) {
			if tp, ok := templates[templateID]; ok && userID == "u_1" {
				cp := *tp
				return &cp, nil
			}
			return nil, identity.ErrTemplateNotFound
		},
		GetTemplateByAlias: func(ctx context.Context, alias, userID string) (*identity.Template, error) {
			return nil, identity.ErrTemplateNotFound
		},
		Idempotency: newMemIdem(),
		DeliverOutbound: func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, req outbound.SendRequest, mt, rt string, ref *identity.Message, ic agent.AcceptIdemCompleter) (*agent.OutboundResult, *agent.OutboundError) {
			deliveries++
			return &agent.OutboundResult{MessageID: "msg_first", Method: "smtp"}, nil
		},
	}))
	t.Cleanup(srv.Close)

	// The retry must be byte-identical, so build the body once.
	rawBody := []byte(`{"to":["alice@x.com"],"template_id":"tmpl_1","template_data":{"name":"Zoe"}}`)
	send := func() (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/agents/a%40acme.com/messages", bytes.NewReader(rawBody))
		req.Header.Set("Authorization", "Bearer good")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "retry-key-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	code, body := send()
	if code != 200 || body["status"] != "sent" || body["message_id"] != "msg_first" {
		t.Fatalf("first send: want 200 sent msg_first, got %d %v", code, body)
	}

	// Simulate the template being deleted between attempts.
	delete(templates, "tmpl_1")

	code, body = send()
	if code != 200 || body["status"] != "sent" || body["message_id"] != "msg_first" {
		t.Fatalf("keyed retry after template delete: want replayed 200 msg_first, got %d %v", code, body)
	}
	if deliveries != 1 {
		t.Fatalf("DeliverOutbound ran %d times, want exactly 1 (retry must replay, not re-send)", deliveries)
	}
}

// TestSendAccepted202AndIdempotentReplay pins the async-accept convention: an
// enqueued (status=accepted) send returns 202 Accepted — not 200 — and a
// byte-identical keyed retry REPLAYS the cached 202 (never a stale 200). The
// 200-for-accepted status was baked into the idempotency cache, so this guards
// both the live response and the cached-status replay.
func TestSendAccepted202AndIdempotentReplay(t *testing.T) {
	deliveries := 0
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) { return &identity.User{ID: "u_1"}, nil },
		GetAgent: func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			return &identity.AgentIdentity{ID: address, Email: address, UserID: "u_1", DomainVerified: true}, nil
		},
		Idempotency: newMemIdem(),
		DeliverOutbound: func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, req outbound.SendRequest, mt, rt string, ref *identity.Message, ic agent.AcceptIdemCompleter) (*agent.OutboundResult, *agent.OutboundError) {
			deliveries++
			// Async pipeline: durably persisted + queued, terminal outcome later.
			return &agent.OutboundResult{MessageID: "msg_acc", Status: "accepted", Method: "smtp"}, nil
		},
	}))
	t.Cleanup(srv.Close)

	rawBody := []byte(`{"to":["alice@x.com"],"subject":"Hi","text":"hello"}`)
	send := func() (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/agents/a%40acme.com/messages", bytes.NewReader(rawBody))
		req.Header.Set("Authorization", "Bearer good")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "acc-key-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	code, body := send()
	if code != http.StatusAccepted || body["status"] != "accepted" || body["message_id"] != "msg_acc" {
		t.Fatalf("first send: want 202 accepted msg_acc, got %d %v", code, body)
	}

	// Byte-identical retry must replay the cached 202 — not a stale 200.
	code, body = send()
	if code != http.StatusAccepted || body["status"] != "accepted" || body["message_id"] != "msg_acc" {
		t.Fatalf("keyed retry: want replayed 202 accepted, got %d %v", code, body)
	}
	if deliveries != 1 {
		t.Fatalf("DeliverOutbound ran %d times, want exactly 1 (retry must replay, not re-enqueue)", deliveries)
	}
}

// A loopback send reaches its terminal local state in the request transaction.
// Its in-transaction idempotency completion must cache that exact 200/sent
// resource response, not the external queue's 202/accepted shape.
func TestSelfSendIdempotentReplayPreservesSentResult(t *testing.T) {
	deliveries := 0
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) { return &identity.User{ID: "u_1"}, nil },
		GetAgent: func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			return &identity.AgentIdentity{ID: address, Email: address, UserID: "u_1", DomainVerified: true}, nil
		},
		Idempotency: newMemIdem(),
		DeliverOutbound: func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, req outbound.SendRequest, mt, rt string, ref *identity.Message, ic agent.AcceptIdemCompleter) (*agent.OutboundResult, *agent.OutboundError) {
			deliveries++
			if err := ic(ctx, nil, &agent.OutboundResult{
				MessageID: "msg_local", Method: "loopback", SentAs: "own_address",
			}); err != nil {
				t.Fatalf("complete loopback idempotency: %v", err)
			}
			return &agent.OutboundResult{MessageID: "msg_local", Method: "loopback", SentAs: "own_address"}, nil
		},
	}))
	t.Cleanup(srv.Close)

	rawBody := []byte(`{"to":["a@acme.com"],"subject":"Note","text":"hello"}`)
	send := func() (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/agents/a%40acme.com/messages", bytes.NewReader(rawBody))
		req.Header.Set("Authorization", "Bearer good")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "local-key-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	for attempt := 1; attempt <= 2; attempt++ {
		code, body := send()
		if code != http.StatusOK || body["status"] != "sent" || body["message_id"] != "msg_local" || body["method"] != "loopback" {
			t.Fatalf("attempt %d: want replayed 200 sent msg_local loopback, got %d %v", attempt, code, body)
		}
	}
	if deliveries != 1 {
		t.Fatalf("DeliverOutbound ran %d times, want exactly 1", deliveries)
	}
}
