package outbound_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// These tests drive the real gate against real Postgres and a real (fake)
// SMTP listener. Every "no network call" claim is asserted against a socket
// counter, never against the absence of an error, because the failure that
// matters is a connection that happened anyway. Every address is synthetic.

const (
	psHMAC     = `{"active":1,"keys":{"1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}`
	psOperator = `{"commitment_key":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI","recipients":{"1":"submit-operator@example.test"}}`
)

type gateFixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	gate   sendingpolicy.Gate
	userID string
	agent  string
	tenant string
}

var psSeq int

func psID(prefix string) string {
	psSeq++
	return fmt.Sprintf("%s_ps_%d_%x", prefix, psSeq, rand.Uint32())
}

// newGateFixture builds an enforcing config-source gate with one standard
// account owning one shared-domain agent. mutate adjusts the policy.
func newGateFixture(t *testing.T, mutate func(*sendingpolicy.RuntimePolicy)) *gateFixture {
	t.Helper()
	ctx := context.Background()
	pool := testutil.TestDB(t)
	keyring, err := sendingpolicy.LoadKeyring(psHMAC)
	if err != nil {
		t.Fatalf("load keyring: %v", err)
	}
	recipients, err := sendingpolicy.LoadOperatorRecipients(psOperator)
	if err != nil {
		t.Fatalf("load operator map: %v", err)
	}
	secrets := sendingpolicy.Secrets{Keyring: keyring, Recipients: recipients}
	if _, err := sendingpolicy.NewModule(pool, secrets).RegisterOperatorRecipients(ctx, "fixture", "submit test bootstrap"); err != nil {
		t.Fatalf("register operator recipients: %v", err)
	}
	policy := sendingpolicy.DisabledPolicy()
	policy.BudgetMode = sendingpolicy.ModeEnforce
	if mutate != nil {
		mutate(&policy)
	}
	f := &gateFixture{
		t: t, ctx: ctx, pool: pool,
		gate:   sendingpolicy.NewGate(pool, secrets, sendingpolicy.PolicySourceConfig, policy),
		userID: psID("usr"),
		agent:  psID("agt"),
		tenant: psID("tenant"),
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, google_subject, account_class) VALUES ($1, $2, $3, 'standard')`,
		f.userID, f.userID+"@example.test", "sub_"+f.userID,
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO account_sending_controls (user_id, ses_tenant_name, ses_tenant_ready, ses_tenant_ready_at)
		VALUES ($1, $2, true, now())
		ON CONFLICT (user_id) DO UPDATE
		    SET ses_tenant_name = EXCLUDED.ses_tenant_name, ses_tenant_ready = true, ses_tenant_ready_at = now()`,
		f.userID, f.tenant,
	); err != nil {
		t.Fatalf("provision tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO agent_identities (id, user_id, registered_domain, name) VALUES ($1, $2, 'agents.e2a.dev', $1)`,
		f.agent, f.userID,
	); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	return f
}

// message inserts an outbound message with `count` distinct recipients.
func (f *gateFixture) message(count int) (string, []string) {
	f.t.Helper()
	id := psID("msg")
	to := make([]string, count)
	for i := range to {
		to[i] = fmt.Sprintf("rcpt-%s-%d@example.test", id, i)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO messages (id, agent_id, direction, to_recipients, sent_as, status)
		 VALUES ($1, $2, 'outbound', $3, 'own_address', 'sent')`, id, f.agent, to,
	); err != nil {
		f.t.Fatalf("insert message: %v", err)
	}
	return id, to
}

// prepare runs the acceptance half the way an API handler does.
func (f *gateFixture) prepare(messageID string) sendingpolicy.OperationRef {
	f.t.Helper()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	accept, ref, err := f.gate.PrepareExternalTx(f.ctx, tx, messageID)
	if err != nil {
		_ = tx.Rollback(f.ctx)
		f.t.Fatalf("prepare: %v", err)
	}
	if accept != sendingpolicy.AcceptanceAccept {
		_ = tx.Rollback(f.ctx)
		f.t.Fatalf("prepare decision = %v, want accept", accept)
	}
	if err := tx.Commit(f.ctx); err != nil {
		f.t.Fatalf("commit: %v", err)
	}
	return ref
}

// authorize runs the worker's sequence — Reserve, then ConsumeAttempt — and
// fails the test on a hold, because every test here is about what happens
// AFTER a token exists.
func (f *gateFixture) authorize(ref sendingpolicy.OperationRef) sendingpolicy.ProviderAuthorization {
	f.t.Helper()
	early, attempt, err := f.gate.Reserve(f.ctx, ref)
	if err != nil {
		f.t.Fatalf("reserve: %v", err)
	}
	if !early.Allow {
		f.t.Fatalf("reserve held: %+v", early)
	}
	decision, auth, err := f.gate.ConsumeAttempt(f.ctx, attempt)
	if err != nil {
		f.t.Fatalf("consume: %v", err)
	}
	if !decision.Allow || auth == nil {
		f.t.Fatalf("consume held: %+v (token=%v)", decision, auth != nil)
	}
	return *auth
}

func (f *gateFixture) callState(operationID string, attempt int) string {
	f.t.Helper()
	var state string
	if err := f.pool.QueryRow(f.ctx, `
		SELECT call_state FROM sending_budget_reservations
		 WHERE operation_id = $1 AND submission_attempt = $2`, operationID, attempt,
	).Scan(&state); err != nil {
		f.t.Fatalf("read call_state: %v", err)
	}
	return state
}

func (f *gateFixture) correlation(operationID string, attempt int) (correlationID string, providerMessageID *string) {
	f.t.Helper()
	err := f.pool.QueryRow(f.ctx, `
		SELECT correlation_id, provider_message_id FROM sending_feedback_correlations
		 WHERE operation_id = $1 AND submission_attempt = $2`, operationID, attempt,
	).Scan(&correlationID, &providerMessageID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		f.t.Fatalf("read correlation: %v", err)
	}
	return correlationID, providerMessageID
}

// countingListener accepts and immediately drops connections, counting them.
// A relay pointed at it can never complete a transaction, so any nonzero
// count is a socket that should not have been opened.
func countingListener(t *testing.T) (*outbound.SMTPRelay, func() int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var mu sync.Mutex
	count := 0
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			count++
			mu.Unlock()
			_ = conn.Close()
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: addr.IP.String(), Port: addr.Port})
	return relay, func() int { mu.Lock(); defer mu.Unlock(); return count }
}

// acceptingRelay fronts testutil's fake SMTP server.
func acceptingRelay(t *testing.T) (*outbound.SMTPRelay, func() []testutil.SMTPMessage) {
	t.Helper()
	addr, messages := testutil.FakeSMTPServer(t)
	return outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: addr.Host, Port: addr.Port}), messages
}

// rejectingRelay fronts a server that answers every RCPT TO with a 550.
func rejectingRelay(t *testing.T) *outbound.SMTPRelay {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				fmt.Fprint(conn, "220 reject ready\r\n")
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					switch upper := strings.ToUpper(strings.TrimSpace(line)); {
					case strings.HasPrefix(upper, "RCPT TO:"):
						fmt.Fprint(conn, "550 5.1.1 no such user\r\n")
					case upper == "QUIT":
						fmt.Fprint(conn, "221 Bye\r\n")
						return
					default:
						fmt.Fprint(conn, "250 OK\r\n")
					}
				}
			}(conn)
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	return outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: addr.IP.String(), Port: addr.Port})
}

// headerValues returns every value of `name` in the header section of a
// captured message, case-insensitively, with folded continuations unfolded.
func headerValues(data, name string) []string {
	var out []string
	current := -1
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if current >= 0 {
				out[current] += " " + strings.TrimSpace(line)
			}
			continue
		}
		current = -1
		if i := strings.IndexByte(line, ':'); i > 0 && strings.EqualFold(strings.TrimSpace(line[:i]), name) {
			out = append(out, strings.TrimSpace(line[i+1:]))
			current = len(out) - 1
		}
	}
	return out
}

func body(data string) string {
	if i := strings.Index(data, "\n\n"); i >= 0 {
		return data[i+2:]
	}
	return ""
}

func TestProviderSubmitterZeroNetworkWithoutAuthorization(t *testing.T) {
	f := newGateFixture(t, nil)
	relay, sockets := countingListener(t)
	s := outbound.NewProviderSubmitter(relay, f.gate)

	_, err := s.SubmitOnce(f.ctx, sendingpolicy.ProviderAuthorization{}, outbound.Envelope{
		From: "agent@agents.e2a.dev", Recipients: []string{"someone@example.test"}, Message: []byte("Subject: x\r\n\r\nbody"),
	})
	if !errors.Is(err, outbound.ErrAuthorizationRequired) {
		t.Fatalf("err = %v, want outbound.ErrAuthorizationRequired", err)
	}
	if sockets() != 0 {
		t.Fatalf("sockets = %d, want 0", sockets())
	}
}

// TestProviderSubmitterZeroNetworkOnEnvelopeMismatch proves the envelope check
// runs before redemption: a wrong envelope costs nothing, sends nothing, and
// leaves the token spendable for the right one.
func TestProviderSubmitterZeroNetworkOnEnvelopeMismatch(t *testing.T) {
	f := newGateFixture(t, nil)
	relay, sockets := countingListener(t)
	s := outbound.NewProviderSubmitter(relay, f.gate)
	messageID, to := f.message(2)
	ref := f.prepare(messageID)
	auth := f.authorize(ref)

	for name, envelope := range map[string][]string{
		"extra recipient":    append(append([]string(nil), to...), "attacker@example.test"),
		"swapped recipient":  {to[0], "attacker@example.test"},
		"dropped recipient":  {to[0]},
		"duplicated mailbox": {to[0], strings.ToUpper(to[0]), to[1]},
		"empty":              nil,
	} {
		_, err := s.SubmitOnce(f.ctx, auth, outbound.Envelope{From: "agent@agents.e2a.dev", Recipients: envelope, Message: []byte("Subject: x\r\n\r\nbody")})
		if err == nil {
			t.Fatalf("%s: submitted, want refusal", name)
		}
		if state := f.callState(ref.ID(), 1); state != "authorized" {
			t.Fatalf("%s: call_state = %s, want authorized (token must survive a mismatch)", name, state)
		}
	}
	if sockets() != 0 {
		t.Fatalf("sockets = %d, want 0", sockets())
	}
}

func TestProviderSubmitterZeroNetworkWhenRelayUnconfigured(t *testing.T) {
	f := newGateFixture(t, nil)
	s := outbound.NewProviderSubmitter(outbound.NewSMTPRelay(&config.OutboundSMTPConfig{}), f.gate)
	messageID, to := f.message(1)
	ref := f.prepare(messageID)
	auth := f.authorize(ref)

	if _, err := s.SubmitOnce(f.ctx, auth, outbound.Envelope{From: "agent@agents.e2a.dev", Recipients: to, Message: []byte("Subject: x\r\n\r\nbody")}); err == nil {
		t.Fatal("submitted through an unconfigured relay")
	}
	// A misconfigured relay is not the message's fault: the token is intact.
	if state := f.callState(ref.ID(), 1); state != "authorized" {
		t.Fatalf("call_state = %s, want authorized", state)
	}
}

// TestProviderSubmitterAuthorizedTokenIsSingleUse proves the token is spent by
// the call, not merely checked: a second submission with the same token opens
// no socket.
func TestProviderSubmitterAuthorizedTokenIsSingleUse(t *testing.T) {
	f := newGateFixture(t, nil)
	relay, captured := acceptingRelay(t)
	s := outbound.NewProviderSubmitter(relay, f.gate)
	messageID, to := f.message(1)
	ref := f.prepare(messageID)
	auth := f.authorize(ref)
	env := outbound.Envelope{MessageID: messageID, From: "agent@agents.e2a.dev", Recipients: to, Message: []byte("Subject: x\r\n\r\nbody")}

	res, err := s.SubmitOnce(f.ctx, auth, env)
	if err != nil || res.SettlementErr != nil || res.ProviderMessageID == "" {
		t.Fatalf("first submit: res=%+v err=%v", res, err)
	}
	if state := f.callState(ref.ID(), 1); state != "started" {
		t.Fatalf("call_state = %s, want started", state)
	}

	_, err = s.SubmitOnce(f.ctx, auth, env)
	if !errors.Is(err, sendingpolicy.ErrAuthorizationInvalid) {
		t.Fatalf("second submit err = %v, want ErrAuthorizationInvalid", err)
	}
	if n := len(captured()); n != 1 {
		t.Fatalf("provider received %d messages for one token, want 1", n)
	}
}

// TestProviderSubmitterZeroNetworkForStaleAttempt: a token whose ordinal has
// been superseded — the worker died and a later execution re-reserved — opens
// no socket.
func TestProviderSubmitterZeroNetworkForStaleAttempt(t *testing.T) {
	f := newGateFixture(t, nil)
	relay, sockets := countingListener(t)
	s := outbound.NewProviderSubmitter(relay, f.gate)
	messageID, to := f.message(1)
	ref := f.prepare(messageID)
	stale := f.authorize(ref)
	if _, next, err := f.gate.Reserve(f.ctx, ref); err != nil || next.Attempt() != 2 {
		t.Fatalf("re-reserve: attempt=%v err=%v", next, err)
	}

	_, err := s.SubmitOnce(f.ctx, stale, outbound.Envelope{From: "agent@agents.e2a.dev", Recipients: to, Message: []byte("Subject: x\r\n\r\nbody")})
	if !errors.Is(err, sendingpolicy.ErrAuthorizationInvalid) {
		t.Fatalf("err = %v, want ErrAuthorizationInvalid", err)
	}
	if sockets() != 0 {
		t.Fatalf("sockets = %d, want 0", sockets())
	}
}

// TestProviderSubmitterAttemptHeaderDerivesOnlyFromToken proves the wire
// carries exactly one attempt header, its value is the gate's correlation id,
// every smuggled spelling is gone, and the body is untouched.
func TestProviderSubmitterAttemptHeaderDerivesOnlyFromToken(t *testing.T) {
	f := newGateFixture(t, nil)
	relay, captured := acceptingRelay(t)
	s := outbound.NewProviderSubmitter(relay, f.gate)
	s.SetSESConfigurationSet("e2a-delivery-test")
	messageID, to := f.message(1)
	ref := f.prepare(messageID)
	auth := f.authorize(ref)

	res, err := s.SubmitOnce(f.ctx, auth, outbound.Envelope{MessageID: messageID, From: "agent@agents.e2a.dev", Recipients: to, Message: outbound.SmuggledMIME()})
	if err != nil || res.SettlementErr != nil {
		t.Fatalf("submit: res=%+v err=%v", res, err)
	}
	msgs := captured()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	data := msgs[0].Data
	corr, _ := f.correlation(ref.ID(), 1)
	if corr == "" {
		t.Fatal("no correlation row for the attempt")
	}
	for name, want := range map[string][]string{
		outbound.ProviderAttemptHeader:     {corr},
		outbound.SESConfigurationSetHeader: {"e2a-delivery-test"},
		delivery.MessageIDHeader:           {messageID},
		outbound.SESTenantHeader:           nil, // policy has the tenant header disabled
	} {
		if got := headerValues(data, name); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := headerValues(data, "Subject"); len(got) != 1 || got[0] != "hello" {
		t.Errorf("customer headers disturbed: Subject = %q", got)
	}
	if b := body(data); !strings.Contains(b, "X-SES-TENANT: body-decoy") || !strings.Contains(b, "body line") {
		t.Errorf("body was rewritten: %q", b)
	}
	if strings.Contains(data, "smuggled") || strings.Contains(data, "forged") || strings.Contains(data, "attacker") {
		t.Errorf("a smuggled header value survived:\n%s", data)
	}
}

// TestProviderSubmitterTenantHeaderIsExactAndSingle: under an enforcing tenant
// policy the wire carries exactly one X-SES-TENANT, whose value is the tenant
// the gate read under lock — not anything the MIME said.
func TestProviderSubmitterTenantHeaderIsExactAndSingle(t *testing.T) {
	f := newGateFixture(t, func(p *sendingpolicy.RuntimePolicy) {
		p.TenantHeaderMode = sendingpolicy.TenantHeaderEnforce
	})
	relay, captured := acceptingRelay(t)
	s := outbound.NewProviderSubmitter(relay, f.gate)
	messageID, to := f.message(1)
	ref := f.prepare(messageID)
	auth := f.authorize(ref)

	if _, err := s.SubmitOnce(f.ctx, auth, outbound.Envelope{From: "agent@agents.e2a.dev", Recipients: to, Message: outbound.SmuggledMIME()}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	msgs := captured()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	if got := headerValues(msgs[0].Data, outbound.SESTenantHeader); len(got) != 1 || got[0] != f.tenant {
		t.Fatalf("%s = %q, want exactly [%q]", outbound.SESTenantHeader, got, f.tenant)
	}
}

// TestProviderSubmitterRetryRedeemsADistinctAttempt: a physical retry is a new
// ordinal with a new token and a new attempt header, never a resubmission
// under the old one.
func TestProviderSubmitterRetryRedeemsADistinctAttempt(t *testing.T) {
	f := newGateFixture(t, nil)
	relay, captured := acceptingRelay(t)
	s := outbound.NewProviderSubmitter(relay, f.gate)
	messageID, to := f.message(1)
	ref := f.prepare(messageID)
	env := outbound.Envelope{MessageID: messageID, From: "agent@agents.e2a.dev", Recipients: to, Message: []byte("Subject: x\r\n\r\nbody")}

	first := f.authorize(ref)
	if _, err := s.SubmitOnce(f.ctx, first, env); err != nil {
		t.Fatalf("first: %v", err)
	}
	second := f.authorize(ref)
	if second.Attempt().Attempt() != 2 {
		t.Fatalf("second ordinal = %d, want 2", second.Attempt().Attempt())
	}
	if _, err := s.SubmitOnce(f.ctx, second, env); err != nil {
		t.Fatalf("second: %v", err)
	}
	msgs := captured()
	if len(msgs) != 2 {
		t.Fatalf("captured %d messages, want 2", len(msgs))
	}
	c1, _ := f.correlation(ref.ID(), 1)
	c2, _ := f.correlation(ref.ID(), 2)
	h1 := headerValues(msgs[0].Data, outbound.ProviderAttemptHeader)
	h2 := headerValues(msgs[1].Data, outbound.ProviderAttemptHeader)
	if len(h1) != 1 || len(h2) != 1 || h1[0] != c1 || h2[0] != c2 || c1 == c2 {
		t.Fatalf("attempt headers %v / %v, want distinct correlations %q / %q", h1, h2, c1, c2)
	}
}

func TestProviderSubmitterBindsProviderMessageIDOnAcceptance(t *testing.T) {
	f := newGateFixture(t, nil)
	relay, _ := acceptingRelay(t)
	s := outbound.NewProviderSubmitter(relay, f.gate)
	messageID, to := f.message(1)
	ref := f.prepare(messageID)
	auth := f.authorize(ref)

	res, err := s.SubmitOnce(f.ctx, auth, outbound.Envelope{From: "agent@agents.e2a.dev", Recipients: to, Message: []byte("Subject: x\r\n\r\nbody")})
	if err != nil || res.SettlementErr != nil {
		t.Fatalf("submit: res=%+v err=%v", res, err)
	}
	if res.Attempt.Attempt() != 1 {
		t.Errorf("result attempt = %d, want 1", res.Attempt.Attempt())
	}
	_, bound := f.correlation(ref.ID(), 1)
	if bound == nil || *bound != res.ProviderMessageID {
		t.Fatalf("correlation provider_message_id = %v, want %q", bound, res.ProviderMessageID)
	}
}

// TestProviderSubmitterPermanentRejectionIsSettledNotRetried: a definite 5xx
// consumed the attempt (the socket opened), settles as rejected, binds no
// provider id, and surfaces as a permanent error the worker can classify.
func TestProviderSubmitterPermanentRejectionIsSettledNotRetried(t *testing.T) {
	f := newGateFixture(t, nil)
	s := outbound.NewProviderSubmitter(rejectingRelay(t), f.gate)
	messageID, to := f.message(1)
	ref := f.prepare(messageID)
	auth := f.authorize(ref)

	_, err := s.SubmitOnce(f.ctx, auth, outbound.Envelope{From: "agent@agents.e2a.dev", Recipients: to, Message: []byte("Subject: x\r\n\r\nbody")})
	if err == nil || !outbound.IsPermanentSMTPError(err) {
		t.Fatalf("err = %v, want a permanent SMTP error", err)
	}
	if state := f.callState(ref.ID(), 1); state != "started" {
		t.Fatalf("call_state = %s, want started (the socket did open)", state)
	}
	if _, bound := f.correlation(ref.ID(), 1); bound != nil {
		t.Fatalf("provider_message_id = %q bound on a rejection", *bound)
	}
}
