//go:build integration

package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

const (
	evalActorAddress        = "actor@eval.test"
	evalTargetAddress       = "target@eval.test"
	evalUnauthorizedAddress = "unauthorized@outside.test"
	evalSubject             = "Question about fictional order ord_example_123"
	evalRequiredFact        = "Refunds are available within 30 days"
	maxSMTPCommandLine      = 4 << 10
	maxSMTPDataLine         = 64 << 10
	maxSMTPMessageBytes     = 2 << 20
	maxSMTPRecipients       = 10
)

type forwardedSMTPMessage struct {
	From              string
	Recipients        []string
	Data              []byte
	ProviderMessageID string
}

type smtpForwarder struct {
	t        *testing.T
	listener net.Listener
	host     string
	port     int

	mu          sync.Mutex
	destination string
	messages    []forwardedSMTPMessage
	connections map[net.Conn]struct{}
	sequence    int
	wg          sync.WaitGroup
}

func newSMTPForwarder(t *testing.T) *smtpForwarder {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start local SMTP fixture")
	}
	address := listener.Addr().(*net.TCPAddr)
	forwarder := &smtpForwarder{
		t:           t,
		listener:    listener,
		host:        "127.0.0.1",
		port:        address.Port,
		connections: make(map[net.Conn]struct{}),
	}
	forwarder.wg.Add(1)
	go forwarder.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		forwarder.mu.Lock()
		for connection := range forwarder.connections {
			_ = connection.Close()
		}
		forwarder.mu.Unlock()
		forwarder.wg.Wait()
	})
	return forwarder
}

func (f *smtpForwarder) Host() string { return f.host }
func (f *smtpForwarder) Port() int    { return f.port }

func (f *smtpForwarder) SetDestination(address string) {
	f.t.Helper()
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		f.t.Fatal("SMTP destination is not an explicit address")
	}
	ip := net.ParseIP(host)
	port, portErr := strconv.Atoi(portText)
	if ip == nil || !ip.IsLoopback() || portErr != nil || port < 1 || port > 65535 {
		f.t.Fatal("SMTP destination must be an explicit loopback address")
	}
	f.mu.Lock()
	f.destination = net.JoinHostPort(ip.String(), strconv.Itoa(port))
	f.mu.Unlock()
}

func (f *smtpForwarder) serve() {
	defer f.wg.Done()
	for {
		connection, err := f.listener.Accept()
		if err != nil {
			return
		}
		remote, ok := connection.RemoteAddr().(*net.TCPAddr)
		if !ok || remote.IP == nil || !remote.IP.IsLoopback() {
			_ = connection.Close()
			continue
		}
		f.mu.Lock()
		f.connections[connection] = struct{}{}
		f.mu.Unlock()
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer func() {
				f.mu.Lock()
				delete(f.connections, connection)
				f.mu.Unlock()
				_ = connection.Close()
			}()
			f.handle(connection)
		}()
	}
}

func smtpReply(writer io.Writer, code int, message string) bool {
	_, err := fmt.Fprintf(writer, "%d %s\r\n", code, message)
	return err == nil
}

func readSMTPLine(reader *bufio.Reader, maximum int) (string, error) {
	line, err := reader.ReadString('\n')
	if len(line) > maximum {
		return "", errors.New("SMTP line too large")
	}
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(line, "\n") {
		return "", errors.New("incomplete SMTP line")
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

func smtpPath(line, prefix string, allowEmpty bool) (string, bool) {
	if len(line) < len(prefix) || !strings.EqualFold(line[:len(prefix)], prefix) {
		return "", false
	}
	rest := strings.TrimSpace(line[len(prefix):])
	if len(rest) < 2 || rest[0] != '<' || rest[len(rest)-1] != '>' {
		return "", false
	}
	address := rest[1 : len(rest)-1]
	if address == "" {
		return "", allowEmpty
	}
	if strings.ContainsAny(address, "<>\r\n \t") || strings.Count(address, "@") != 1 {
		return "", false
	}
	return address, true
}

func testRecipient(address string) bool {
	at := strings.LastIndexByte(address, '@')
	return at > 0 && strings.HasSuffix(strings.ToLower(address[at+1:]), ".test")
}

func (f *smtpForwarder) handle(connection net.Conn) {
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReaderSize(connection, maxSMTPDataLine+2)
	if !smtpReply(connection, 220, "smtp.agents.localhost ready") {
		return
	}
	hello := false
	mailFrom := ""
	mailStarted := false
	var recipients []string

	for {
		line, err := readSMTPLine(reader, maxSMTPCommandLine)
		if err != nil {
			return
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO ") || strings.HasPrefix(upper, "HELO "):
			hello = true
			mailStarted = false
			mailFrom = ""
			recipients = nil
			if !smtpReply(connection, 250, "smtp.agents.localhost") {
				return
			}
		case strings.HasPrefix(upper, "MAIL FROM:"):
			from, valid := smtpPath(line, "MAIL FROM:", true)
			if !hello || mailStarted || !valid {
				if !smtpReply(connection, 503, "bad sequence") {
					return
				}
				continue
			}
			mailStarted = true
			mailFrom = from
			recipients = nil
			if !smtpReply(connection, 250, "sender accepted") {
				return
			}
		case strings.HasPrefix(upper, "RCPT TO:"):
			recipient, valid := smtpPath(line, "RCPT TO:", false)
			if !hello || !mailStarted || !valid {
				if !smtpReply(connection, 503, "bad sequence") {
					return
				}
				continue
			}
			if !testRecipient(recipient) {
				if !smtpReply(connection, 550, "test recipients only") {
					return
				}
				continue
			}
			if len(recipients) >= maxSMTPRecipients {
				if !smtpReply(connection, 452, "too many recipients") {
					return
				}
				continue
			}
			duplicate := false
			for _, existing := range recipients {
				duplicate = duplicate || strings.EqualFold(existing, recipient)
			}
			if duplicate {
				if !smtpReply(connection, 550, "duplicate recipient") {
					return
				}
				continue
			}
			recipients = append(recipients, recipient)
			if !smtpReply(connection, 250, "recipient accepted") {
				return
			}
		case upper == "DATA":
			if !hello || !mailStarted || len(recipients) == 0 {
				if !smtpReply(connection, 503, "bad sequence") {
					return
				}
				continue
			}
			if !smtpReply(connection, 354, "send data") {
				return
			}
			data, dataErr := readSMTPData(reader)
			if dataErr != nil {
				_ = smtpReply(connection, 552, "message too large")
				return
			}
			f.mu.Lock()
			f.sequence++
			providerMessageID := fmt.Sprintf("<eval-synthetic-%d@agents.localhost>", f.sequence)
			f.mu.Unlock()
			data, dataErr = stampSyntheticProviderMessageID(data, providerMessageID)
			if dataErr != nil {
				_ = smtpReply(connection, 554, "invalid message identity")
				return
			}
			message := forwardedSMTPMessage{
				From: mailFrom, Recipients: append([]string(nil), recipients...), Data: data, ProviderMessageID: providerMessageID,
			}
			f.mu.Lock()
			f.messages = append(f.messages, message)
			destination := f.destination
			f.mu.Unlock()
			if destination == "" {
				_ = smtpReply(connection, 451, "destination unavailable")
				return
			}
			if err := smtp.SendMail(destination, nil, mailFrom, recipients, data); err != nil {
				_ = smtpReply(connection, 451, "local forwarding failed")
				return
			}
			mailStarted = false
			mailFrom = ""
			recipients = nil
			if !smtpReply(connection, 250, "Ok "+providerMessageID) {
				return
			}
		case upper == "RSET":
			if !hello {
				if !smtpReply(connection, 503, "bad sequence") {
					return
				}
				continue
			}
			mailStarted = false
			mailFrom = ""
			recipients = nil
			if !smtpReply(connection, 250, "reset") {
				return
			}
		case upper == "QUIT":
			_ = smtpReply(connection, 221, "bye")
			return
		default:
			if !smtpReply(connection, 502, "unsupported command") {
				return
			}
		}
	}
}

func readSMTPData(reader *bufio.Reader) ([]byte, error) {
	var data bytes.Buffer
	for {
		line, err := readSMTPLine(reader, maxSMTPDataLine)
		if err != nil {
			return nil, err
		}
		if line == "." {
			return append([]byte(nil), data.Bytes()...), nil
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		if data.Len()+len(line)+2 > maxSMTPMessageBytes {
			return nil, errors.New("SMTP message too large")
		}
		data.WriteString(line)
		data.WriteString("\r\n")
	}
}

func stampSyntheticProviderMessageID(data []byte, providerMessageID string) ([]byte, error) {
	message, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("invalid SMTP fixture message")
	}
	if len(message.Header["Message-Id"]) != 0 {
		return nil, errors.New("SMTP fixture refuses duplicate Message-ID")
	}
	if !strings.HasPrefix(providerMessageID, "<eval-synthetic-") || !strings.HasSuffix(providerMessageID, "@agents.localhost>") ||
		strings.ContainsAny(providerMessageID, "\r\n\x00") {
		return nil, errors.New("invalid synthetic provider Message-ID")
	}
	stamped := make([]byte, 0, len(data)+len(providerMessageID)+14)
	stamped = append(stamped, "Message-ID: "...)
	stamped = append(stamped, providerMessageID...)
	stamped = append(stamped, "\r\n"...)
	stamped = append(stamped, data...)
	return stamped, nil
}

func (f *smtpForwarder) countRecipient(address string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, message := range f.messages {
		for _, recipient := range message.Recipients {
			if strings.EqualFold(recipient, address) {
				count++
			}
		}
	}
	return count
}

func (f *smtpForwarder) assertProviderMessageIDs(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	messages := append([]forwardedSMTPMessage(nil), f.messages...)
	f.mu.Unlock()
	if len(messages) < 2 {
		t.Fatal("SMTP fixture did not capture both round-trip deliveries")
	}
	for _, forwarded := range messages {
		parsed, err := mail.ReadMessage(bytes.NewReader(forwarded.Data))
		if err != nil {
			t.Fatal("SMTP fixture captured invalid delivered MIME")
		}
		values := parsed.Header["Message-Id"]
		if len(values) != 1 || values[0] != forwarded.ProviderMessageID {
			t.Fatal("SMTP fixture provider response and delivered Message-ID diverged")
		}
	}
}

func (f *smtpForwarder) latestMessageFor(t *testing.T, recipient string) forwardedSMTPMessage {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := len(f.messages) - 1; index >= 0; index-- {
		for _, candidate := range f.messages[index].Recipients {
			if strings.EqualFold(candidate, recipient) {
				message := f.messages[index]
				message.Recipients = append([]string(nil), message.Recipients...)
				message.Data = append([]byte(nil), message.Data...)
				return message
			}
		}
	}
	t.Fatal("SMTP fixture did not capture the expected recipient")
	return forwardedSMTPMessage{}
}

func assertOutboundIdentity(t *testing.T, forwarded forwardedSMTPMessage, envelopeFrom, headerAddress, displayName, replyTo string) {
	t.Helper()
	if forwarded.From != envelopeFrom {
		t.Fatalf("SMTP envelope sender = %q, want %q", forwarded.From, envelopeFrom)
	}
	message, err := mail.ReadMessage(bytes.NewReader(forwarded.Data))
	if err != nil {
		t.Fatal("SMTP fixture captured invalid MIME")
	}
	from, err := mail.ParseAddress(message.Header.Get("From"))
	if err != nil || from.Address != headerAddress || from.Name != displayName {
		t.Fatalf("From header = %q, want %q <%s>", message.Header.Get("From"), displayName, headerAddress)
	}
	parsedReplyTo, err := mail.ParseAddress(message.Header.Get("Reply-To"))
	if err != nil || parsedReplyTo.Address != replyTo {
		t.Fatalf("Reply-To header = %q, want %q", message.Header.Get("Reply-To"), replyTo)
	}
}

func dmarcPassAuthentication() *emailauth.Authentication {
	return passingAuthentication("eval.test")
}

func seedEvalAccount(t *testing.T, ts *testutil.E2ATestServer, actorAddress, targetAddress string) (*identity.APIKey, *identity.AgentIdentity, *identity.AgentIdentity) {
	t.Helper()
	ctx := context.Background()
	user, err := ts.Store.CreateOrGetUser(ctx, "owner@example.test", "Eval Owner", "synthetic-eval-owner")
	if err != nil {
		t.Fatal("seed synthetic eval user")
	}
	if _, err := ts.Store.ClaimOrCreateDomain(ctx, "eval.test", user.ID); err != nil {
		t.Fatal("seed synthetic eval domain")
	}
	if err := ts.Store.VerifyDomain(ctx, "eval.test", user.ID); err != nil {
		t.Fatal("verify synthetic eval domain")
	}
	if err := ts.Store.SetSendingStatus(ctx, "eval.test", "verified", "verified", "verified", "", nil); err != nil {
		t.Fatal("verify synthetic eval sending identity")
	}
	actor, err := ts.Store.CreateAgent(ctx, actorAddress, "eval.test", "Eval Actor", "", "local", user.ID)
	if err != nil {
		t.Fatal("seed synthetic actor")
	}
	target, err := ts.Store.CreateAgent(ctx, targetAddress, "eval.test", "Eval Target", "", "local", user.ID)
	if err != nil {
		t.Fatal("seed synthetic target")
	}
	key, err := ts.Store.CreateAPIKey(ctx, user.ID, "email-eval-round-trip", nil)
	if err != nil {
		t.Fatal("seed synthetic account key")
	}
	return key, actor, target
}

func setExactOutboundGate(t *testing.T, ts *testutil.E2ATestServer, agent *identity.AgentIdentity, allowlist []string) {
	t.Helper()
	_, err := ts.Store.UpdateAgentProtection(context.Background(), agent.ID, agent.UserID, identity.ProtectionConfig{
		InboundGatePolicy:       "open",
		InboundGateAction:       "flag",
		InboundScanSensitivity:  identity.SensitivityOff,
		OutboundGatePolicy:      "allowlist",
		OutboundAllowlist:       append([]string(nil), allowlist...),
		OutboundGateAction:      "block",
		OutboundScanSensitivity: identity.SensitivityOff,
		HITLTTLSeconds:          identity.HITLDefaultTTLSeconds,
		HITLExpirationAction:    identity.HITLExpirationReject,
	})
	if err != nil {
		t.Fatal("set exact synthetic outbound gate")
	}
}

func writeEvalSuite(t *testing.T, baseURL, actorAddress, targetAddress string) string {
	t.Helper()
	directory := t.TempDir()
	suite := fmt.Sprintf(`version: 1
name: local-email-round-trip
target: { email: %q }
actor: { email: %q }
transport:
  adapter: e2a
  api_key: ${E2A_EVAL_API_KEY}
  base_url: %q
  allowed_envelope_recipients: [%q, %q]
defaults:
  timeout: 20s
  settle: 300ms
  poll_interval: 1s
cases: [case.yaml]
`, targetAddress, actorAddress, baseURL, actorAddress, targetAddress)
	testCase := fmt.Sprintf(`id: deterministic-reply
send:
  subject: %q
  text: Can fictional order ord_example_123 be refunded?
expect:
  action: { kind: reply, count: 1 }
  sender:
    exactly: %q
    reply_to: { exactly: [%q] }
  recipients:
    to: { exactly: [%q] }
    cc: { exactly: [] }
    bcc: { exactly: [] }
    envelope: { exactly: [%q] }
  thread:
    in_reply_to: original
    references: contains_original
  subject: { policy: preserve }
  body:
    required_facts: [%q]
    plain_text: required
  attachments: { exactly: [] }
  timing: { reply_within: 15s }
  lifecycle:
    submission: sent
    actor_received: true
`, evalSubject, targetAddress, targetAddress, actorAddress, actorAddress, evalRequiredFact)
	if err := os.WriteFile(filepath.Join(directory, "suite.yaml"), []byte(suite), 0o600); err != nil {
		t.Fatal("write synthetic eval suite")
	}
	if err := os.WriteFile(filepath.Join(directory, "case.yaml"), []byte(testCase), 0o600); err != nil {
		t.Fatal("write synthetic eval case")
	}
	return filepath.Join(directory, "suite.yaml")
}

func emailEvalRuntimeDirectory(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve email eval runtime")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "plugins", "e2a", "skills", "email-evals", "runtime")
}

type boundedChildBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	remaining int
}

func newBoundedChildBuffer(limit int) *boundedChildBuffer {
	return &boundedChildBuffer{remaining: limit}
}

func (b *boundedChildBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(data)
	if len(data) > b.remaining {
		data = data[:b.remaining]
	}
	_, _ = b.buffer.Write(data)
	b.remaining -= len(data)
	return original, nil
}

func (b *boundedChildBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

type deterministicResponder struct {
	t      *testing.T
	cancel context.CancelFunc
	done   chan error
	stdout *boundedChildBuffer
	once   sync.Once
}

func startDeterministicResponder(t *testing.T, baseURL, apiKey, targetAddress string) *deterministicResponder {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required for the local eval responder")
	}
	ctx, cancel := context.WithCancel(context.Background())
	stdout := newBoundedChildBuffer(8 << 10)
	stderr := newBoundedChildBuffer(8 << 10)
	command := exec.CommandContext(ctx, node, filepath.Join(emailEvalRuntimeDirectory(t), "test", "live-responder.mjs"))
	command.Env = []string{
		"E2A_EVAL_API_KEY=" + apiKey,
		"E2A_EVAL_BASE_URL=" + baseURL,
		"E2A_EVAL_ACTOR=" + evalActorAddress,
		"E2A_EVAL_TARGET=" + targetAddress,
	}
	command.Stdout = stdout
	command.Stderr = stderr
	done := make(chan error, 1)
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal("start deterministic responder")
	}
	go func() { done <- command.Wait() }()
	responder := &deterministicResponder{t: t, cancel: cancel, done: done, stdout: stdout}
	t.Cleanup(func() { responder.stop() })
	return responder
}

func (r *deterministicResponder) stop() {
	r.once.Do(func() {
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(2 * time.Second):
		}
	})
}

func (r *deterministicResponder) WaitForReply(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.done:
		r.once.Do(r.cancel)
		if err != nil || r.stdout.String() != "reply submitted\n" {
			t.Fatal("deterministic responder did not complete safely")
		}
	case <-time.After(25 * time.Second):
		r.stop()
		t.Fatal("deterministic responder timed out")
	}
}

type evalCLIResult struct {
	ExitCode     int
	Stderr       string
	RunDirectory string
}

func runEmailEvalCLI(t *testing.T, suite, apiKey string) evalCLIResult {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required for the local eval runtime")
	}
	outputRoot := filepath.Join(filepath.Dir(suite), "results")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	stdout := newBoundedChildBuffer(64 << 10)
	stderr := newBoundedChildBuffer(16 << 10)
	command := exec.CommandContext(ctx, node,
		filepath.Join(emailEvalRuntimeDirectory(t), "cli.mjs"),
		"run", "--suite", suite, "--output", outputRoot, "--json")
	command.Env = []string{"E2A_EVAL_API_KEY=" + apiKey}
	command.Dir = filepath.Dir(suite)
	command.Stdout = stdout
	command.Stderr = stderr
	runErr := command.Run()
	result := evalCLIResult{Stderr: "email eval runtime failed safely"}
	if runErr == nil {
		result.ExitCode = 0
	} else if exitError, ok := runErr.(*exec.ExitError); ok {
		result.ExitCode = exitError.ExitCode()
	} else {
		result.ExitCode = 4
	}
	if ctx.Err() != nil {
		result.ExitCode = 4
		result.Stderr = "email eval runtime timed out safely"
	}
	switch stderr.String() {
	case "email-evals: assertion_failure\n":
		result.Stderr = "email eval runtime reported a fixed assertion-failure diagnostic"
	case "email-evals: transport_error\n":
		result.Stderr = "email eval runtime reported a fixed transport-error diagnostic"
	case "email-evals: target_timeout\n":
		result.Stderr = "email eval runtime reported a fixed target-timeout diagnostic"
	case "email-evals: grader_error\n":
		result.Stderr = "email eval runtime reported a fixed grader-error diagnostic"
	case "email-evals: unexpected runner failure\n":
		result.Stderr = "email eval runtime reported a fixed unexpected-runner diagnostic"
	}
	var output struct {
		Command string `json:"command"`
		Summary struct {
			Status string `json:"status"`
		} `json:"summary"`
	}
	parsedOutput := json.Unmarshal([]byte(stdout.String()), &output) == nil && output.Command == "run"
	if result.ExitCode == 0 && (!parsedOutput || output.Summary.Status != "pass") {
		result.ExitCode = 4
		result.Stderr = "email eval runtime returned an invalid result"
	}
	entries, readErr := os.ReadDir(outputRoot)
	if readErr == nil {
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "run_") {
				if result.RunDirectory != "" {
					result.ExitCode = 4
					result.Stderr = "email eval runtime produced ambiguous output"
					break
				}
				result.RunDirectory = filepath.Join(outputRoot, entry.Name())
			}
		}
	}
	if result.ExitCode == 0 && result.RunDirectory == "" {
		result.ExitCode = 4
		result.Stderr = "email eval runtime omitted its report"
	}
	return result
}

func assertReport(t *testing.T, runDirectory, wantStatus string, assertionIDs ...string) {
	t.Helper()
	entries, err := os.ReadDir(runDirectory)
	if err != nil {
		t.Fatal("read email eval report directory")
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	wantNames := []string{"cases.jsonl", "report.md", "summary.json"}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("email eval artifact set = %v, want %v", names, wantNames)
	}
	summaryData, err := os.ReadFile(filepath.Join(runDirectory, "summary.json"))
	if err != nil {
		t.Fatal("read email eval summary")
	}
	var summary struct {
		Status   string `json:"status"`
		Complete bool   `json:"complete"`
	}
	if json.Unmarshal(summaryData, &summary) != nil || summary.Status != wantStatus || !summary.Complete {
		t.Fatal("email eval summary is not a complete pass")
	}
	casesData, err := os.ReadFile(filepath.Join(runDirectory, "cases.jsonl"))
	if err != nil {
		t.Fatal("read email eval case artifact")
	}
	var record struct {
		Status     string `json:"status"`
		Assertions []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"assertions"`
	}
	if json.Unmarshal(bytes.TrimSpace(casesData), &record) != nil || record.Status != wantStatus {
		t.Fatal("email eval case artifact is not a pass")
	}
	statuses := make(map[string]string, len(record.Assertions))
	for _, assertion := range record.Assertions {
		statuses[assertion.ID] = assertion.Status
	}
	for _, id := range assertionIDs {
		if statuses[id] != "pass" {
			t.Fatalf("email eval assertion %s = %q, want pass", id, statuses[id])
		}
	}
	if report, err := os.ReadFile(filepath.Join(runDirectory, "report.md")); err != nil || !bytes.Contains(report, []byte("- Status: **pass**")) {
		t.Fatal("email eval Markdown report is not a pass")
	}
}

func TestEmailEvalRunnerRoundTrip(t *testing.T) {
	pool := testutil.TestDB(t)
	forwarder := newSMTPForwarder(t)
	ts := testutil.TestServer(t, pool,
		testutil.WithOutboundSMTP(forwarder.Host(), forwarder.Port(), "agents.localhost"),
		testutil.WithInboundAuthentication(dmarcPassAuthentication()),
	)
	forwarder.SetDestination(ts.SMTPAddr)

	apiKey, actor, target := seedEvalAccount(t, ts, evalActorAddress, evalTargetAddress)
	setExactOutboundGate(t, ts, actor, []string{target.EmailAddress()})
	setExactOutboundGate(t, ts, target, []string{actor.EmailAddress()})

	var suite string
	t.Run("actor to target reply round trip", func(t *testing.T) {
		suite = writeEvalSuite(t, ts.HTTPServer.URL, actor.EmailAddress(), target.EmailAddress())
		responder := startDeterministicResponder(t, ts.HTTPServer.URL, apiKey.PlaintextKey, target.EmailAddress())
		result := runEmailEvalCLI(t, suite, apiKey.PlaintextKey)
		responder.WaitForReply(t)
		if result.ExitCode != 0 {
			t.Fatalf("email eval failed: %s", result.Stderr)
		}
		assertReport(t, result.RunDirectory, "pass", "recipients.bcc",
			"thread.in_reply_to", "subject.policy", "body.required_facts")
		forwarder.assertProviderMessageIDs(t)
		assertOutboundIdentity(t, forwarder.latestMessageFor(t, target.EmailAddress()),
			"bounces@bounce.eval.test", actor.EmailAddress(), "Eval Actor", actor.EmailAddress())
		if got := forwarder.countRecipient(actor.EmailAddress()); got != 1 {
			t.Fatalf("reply SMTP egress count = %d, want 1", got)
		}
	})

	t.Run("blocked unauthorized target has zero SMTP egress", func(t *testing.T) {
		before := forwarder.countRecipient(evalUnauthorizedAddress)
		status, body := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, target.EmailAddress()), apiKey.PlaintextKey,
			fmt.Sprintf(`{"to":[%q],"subject":"Synthetic unauthorized attempt","text":"Synthetic only"}`, evalUnauthorizedAddress))
		if status != 403 || !bytes.Contains(body, []byte(`"code":"blocked_by_policy"`)) {
			t.Fatalf("unauthorized target attempt status = %d, want blocked_by_policy", status)
		}
		if after := forwarder.countRecipient(evalUnauthorizedAddress); after != before {
			t.Fatalf("unauthorized SMTP egress count changed from %d to %d", before, after)
		}
	})

	t.Run("stable reply idempotency has no duplicate or unauthorized SMTP egress", func(t *testing.T) {
		beforeActor := forwarder.countRecipient(actor.EmailAddress())
		beforeUnauthorized := forwarder.countRecipient(evalUnauthorizedAddress)
		responder := startDeterministicResponder(t, ts.HTTPServer.URL, apiKey.PlaintextKey, target.EmailAddress())
		responder.WaitForReply(t)
		if after := forwarder.countRecipient(actor.EmailAddress()); after != beforeActor {
			t.Fatalf("idempotent replay changed reply SMTP egress from %d to %d", beforeActor, after)
		}
		if after := forwarder.countRecipient(evalUnauthorizedAddress); after != beforeUnauthorized {
			t.Fatalf("idempotent replay changed unauthorized SMTP egress from %d to %d", beforeUnauthorized, after)
		}
	})

	t.Run("unverified sending status retains relay identity fallback", func(t *testing.T) {
		if err := ts.Store.SetSendingStatus(context.Background(), "eval.test", "pending", "", "", "", nil); err != nil {
			t.Fatal("set synthetic sending identity pending")
		}
		before := forwarder.countRecipient(target.EmailAddress())
		status, body := authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, actor.EmailAddress()), apiKey.PlaintextKey,
			fmt.Sprintf(`{"to":[%q],"subject":"Synthetic relay fallback","text":"Synthetic only"}`, target.EmailAddress()))
		if status != 200 || !bytes.Contains(body, []byte(`"status":"sent"`)) {
			t.Fatalf("relay fallback send status = %d, want sent", status)
		}
		if after := forwarder.countRecipient(target.EmailAddress()); after != before+1 {
			t.Fatalf("relay fallback SMTP egress count = %d, want %d", after, before+1)
		}
		assertOutboundIdentity(t, forwarder.latestMessageFor(t, target.EmailAddress()),
			"agent@agents.localhost", "agent@agents.localhost", "Eval Actor via e2a", actor.EmailAddress())
	})
}
