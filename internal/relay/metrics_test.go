package relay_test

import (
	"context"
	"net/smtp"
	"strings"
	"sync"
	"testing"

	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/relay"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
	"github.com/tokencanopy/e2a/internal/ws"
)

// These tests cover the SMTP acceptance SLI (docs/observability.md): every
// terminal SMTP intake decision must record EXACTLY ONE SMTPInbound observation
// with the right outcome label — per transaction, not per recipient — and a
// relay without a wired recorder must stay nil-safe.

// fakeSMTPMetrics is a concurrency-safe recorder satisfying relay.Metrics.
type fakeSMTPMetrics struct {
	mu    sync.Mutex
	calls []smtpInboundCall
}

type smtpInboundCall struct {
	outcome string
	seconds float64
}

func (f *fakeSMTPMetrics) SMTPInbound(outcome string, seconds float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, smtpInboundCall{outcome: outcome, seconds: seconds})
}

func (f *fakeSMTPMetrics) snapshot() []smtpInboundCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]smtpInboundCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// assertSingleOutcome fails unless the recorder saw exactly one observation
// with the given outcome. wantZeroSeconds pins the RCPT-stage contract
// (rejections have no DATA phase, so the duration must be exactly 0).
func assertSingleOutcome(t *testing.T, m *fakeSMTPMetrics, outcome string, wantZeroSeconds bool) {
	t.Helper()
	calls := m.snapshot()
	if len(calls) != 1 {
		t.Fatalf("SMTPInbound calls = %v, want exactly 1 with outcome %q", calls, outcome)
	}
	if calls[0].outcome != outcome {
		t.Fatalf("SMTPInbound outcome = %q, want %q", calls[0].outcome, outcome)
	}
	if wantZeroSeconds && calls[0].seconds != 0 {
		t.Fatalf("SMTPInbound seconds = %v, want 0 for an RCPT-stage rejection", calls[0].seconds)
	}
	if calls[0].seconds < 0 {
		t.Fatalf("SMTPInbound seconds = %v, want >= 0", calls[0].seconds)
	}
}

// startMetricsRelay boots an already-configured relay.Server, waits for the
// SMTP listener, and returns the listen address.
func startMetricsRelay(t *testing.T, cfg *config.Config, server *relay.Server) string {
	t.Helper()
	go func() { _ = server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Close() })
	waitForSMTP(t, cfg.SMTP.ListenAddr)
	return cfg.SMTP.ListenAddr
}

func TestSMTPInboundMetric_RejectedUnknownRecipient(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)

	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	cfg := &config.Config{SMTP: config.SMTPConfig{ListenAddr: "127.0.0.1:" + port, Domain: "metrics.example.com"}, Env: "development"}
	server := relay.NewServer(cfg, store, usage.NewNoopUsageTracker(), ws.NewHub())
	metrics := &fakeSMTPMetrics{}
	server.SetMetrics(metrics)
	addr := startMetricsRelay(t, cfg, server)

	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatalf("smtp.Dial: %v", err)
	}
	defer c.Close()
	if err := c.Mail("sender@ext.test"); err != nil {
		t.Fatalf("MAIL FROM: %v", err)
	}
	rerr := c.Rcpt("nobody@metrics.example.com")
	if rerr == nil {
		t.Fatal("RCPT TO an unknown recipient succeeded; want 550")
	}
	if !strings.Contains(rerr.Error(), "550") {
		t.Fatalf("RCPT TO error = %v, want 550", rerr)
	}
	_ = c.Quit()

	assertSingleOutcome(t, metrics, "rejected_unknown_recipient", true)
}

func TestSMTPInboundMetric_RejectedUnverifiedDomain(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	const domain = "metrics-unverified.example.com"
	const agentEmail = "bot@" + domain
	user, err := store.CreateOrGetUser(ctx, "owner@"+domain, "O", "g-metrics-unverified")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("domain: %v", err)
	}
	// Deliberately NOT verified — the Rcpt gate must fire the unverified branch.
	if _, err := store.CreateAgent(ctx, agentEmail, domain, "", "", "", user.ID); err != nil {
		t.Fatalf("agent: %v", err)
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	cfg := &config.Config{SMTP: config.SMTPConfig{ListenAddr: "127.0.0.1:" + port, Domain: domain}, Env: "development"}
	server := relay.NewServer(cfg, store, usage.NewNoopUsageTracker(), ws.NewHub())
	metrics := &fakeSMTPMetrics{}
	server.SetMetrics(metrics)
	addr := startMetricsRelay(t, cfg, server)

	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatalf("smtp.Dial: %v", err)
	}
	defer c.Close()
	if err := c.Mail("sender@ext.test"); err != nil {
		t.Fatalf("MAIL FROM: %v", err)
	}
	if rerr := c.Rcpt(agentEmail); rerr == nil {
		t.Fatal("RCPT TO an unverified-domain recipient succeeded; want 550")
	}
	_ = c.Quit()

	assertSingleOutcome(t, metrics, "rejected_unverified_domain", true)
}

func TestSMTPInboundMetric_RejectedQuota(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	const domain = "metrics-quota.example.com"
	const agentEmail = "bot@" + domain
	user, err := store.CreateOrGetUser(ctx, "owner@"+domain, "O", "g-metrics-quota")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("domain: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE domains SET verified=true WHERE domain=$1`, domain); err != nil {
		t.Fatalf("verify domain: %v", err)
	}
	if _, err := store.CreateAgent(ctx, agentEmail, domain, "", "", "", user.ID); err != nil {
		t.Fatalf("agent: %v", err)
	}
	// max_messages_month=0 trips CheckMessageSend immediately (same setup as
	// TestRelay_RcptTo_Rejects552WhenOverCap).
	lstore := limits.NewStore(pool)
	if err := lstore.Upsert(ctx, user.ID, limits.Limits{
		PlanCode: "free_test", MaxAgents: 100, MaxDomains: 100, MaxMessagesMonth: 0, MaxStorageBytes: 1 << 40,
	}); err != nil {
		t.Fatalf("Upsert limits: %v", err)
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	cfg := &config.Config{SMTP: config.SMTPConfig{ListenAddr: "127.0.0.1:" + port, Domain: domain}, Env: "development"}
	server := relay.NewServer(cfg, store, usage.NewNoopUsageTracker(), ws.NewHub())
	server.SetEnforcer(limits.NewEnforcer(lstore, usage.NewStore(pool), limits.Defaults{
		PlanCode: "default", MaxAgents: 1, MaxDomains: 1, MaxMessagesMonth: 1, MaxStorageBytes: 1,
	}, 0))
	metrics := &fakeSMTPMetrics{}
	server.SetMetrics(metrics)
	addr := startMetricsRelay(t, cfg, server)

	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatalf("smtp.Dial: %v", err)
	}
	defer c.Close()
	if err := c.Mail("sender@ext.test"); err != nil {
		t.Fatalf("MAIL FROM: %v", err)
	}
	rerr := c.Rcpt(agentEmail)
	if rerr == nil {
		t.Fatal("RCPT TO succeeded; want 552 cap rejection")
	}
	if !strings.Contains(rerr.Error(), "552") {
		t.Fatalf("RCPT TO error = %v, want 552", rerr)
	}
	_ = c.Quit()

	assertSingleOutcome(t, metrics, "rejected_quota", true)
}

// TestSMTPInboundMetric_AcceptedSync drives the synchronous DATA path with TWO
// recipients in one transaction: exactly ONE "accepted" observation must be
// recorded (the SLI counts transactions, not the per-recipient loop).
func TestSMTPInboundMetric_AcceptedSync(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	const domain = "metrics-accept.example.com"
	agents := []string{"bot-a@" + domain, "bot-b@" + domain}
	user, err := store.CreateOrGetUser(ctx, "owner@"+domain, "O", "g-metrics-accept")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("domain: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE domains SET verified=true WHERE domain=$1`, domain); err != nil {
		t.Fatalf("verify domain: %v", err)
	}
	for _, a := range agents {
		if _, err := store.CreateAgent(ctx, a, domain, "", "", "", user.ID); err != nil {
			t.Fatalf("agent %s: %v", a, err)
		}
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	cfg := &config.Config{SMTP: config.SMTPConfig{ListenAddr: "127.0.0.1:" + port, Domain: domain}, Env: "development"}
	server := relay.NewServer(cfg, store, usage.NewNoopUsageTracker(), ws.NewHub())
	server.SetOutbox(webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)))
	metrics := &fakeSMTPMetrics{}
	server.SetMetrics(metrics)
	addr := startMetricsRelay(t, cfg, server)

	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatalf("smtp.Dial: %v", err)
	}
	defer c.Close()
	if err := c.Mail("sender@ext.test"); err != nil {
		t.Fatalf("MAIL FROM: %v", err)
	}
	for _, a := range agents {
		if err := c.Rcpt(a); err != nil {
			t.Fatalf("RCPT TO %s: %v", a, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	body := "From: sender@ext.test\r\nTo: " + strings.Join(agents, ", ") + "\r\nSubject: metrics accept\r\n\r\nhello"
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close DATA: %v", err)
	}
	_ = c.Quit()

	assertSingleOutcome(t, metrics, "accepted", false)
}

// TestSMTPInboundMetric_TempfailPersistFailure forces the sync persist to fail
// (same trigger technique as TestInbound_PersistFailure_Returns451) and asserts
// the 451 records exactly one "tempfail" observation.
func TestSMTPInboundMetric_TempfailPersistFailure(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	const domain = "metrics-tempfail.example.com"
	const agentEmail = "bot@" + domain
	user, err := store.CreateOrGetUser(ctx, "owner@"+domain, "O", "g-metrics-tempfail")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("domain: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE domains SET verified=true WHERE domain=$1`, domain); err != nil {
		t.Fatalf("verify domain: %v", err)
	}
	if _, err := store.CreateAgent(ctx, agentEmail, domain, "", "", "", user.ID); err != nil {
		t.Fatalf("agent: %v", err)
	}

	// Force the inbound insert for THIS agent to fail. Scoped by agent_id and
	// dropped on cleanup — see the slice-1 regression test for the rationale.
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION test_fail_persist_smtp_metric() RETURNS trigger AS $f$
		BEGIN
			IF NEW.agent_id = '`+agentEmail+`' THEN
				RAISE EXCEPTION 'forced persist failure (SMTP metric test)';
			END IF;
			RETURN NEW;
		END; $f$ LANGUAGE plpgsql;`); err != nil {
		t.Fatalf("create trigger fn: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DROP TRIGGER IF EXISTS test_fail_persist_smtp_metric ON messages;
		CREATE TRIGGER test_fail_persist_smtp_metric BEFORE INSERT ON messages
			FOR EACH ROW EXECUTE FUNCTION test_fail_persist_smtp_metric();`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DROP TRIGGER IF EXISTS test_fail_persist_smtp_metric ON messages; DROP FUNCTION IF EXISTS test_fail_persist_smtp_metric();`)
	})

	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	cfg := &config.Config{SMTP: config.SMTPConfig{ListenAddr: "127.0.0.1:" + port, Domain: domain}, Env: "development"}
	server := relay.NewServer(cfg, store, usage.NewNoopUsageTracker(), ws.NewHub())
	server.SetOutbox(webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)))
	metrics := &fakeSMTPMetrics{}
	server.SetMetrics(metrics)
	addr := startMetricsRelay(t, cfg, server)

	body := "From: sender@ext.test\r\nTo: " + agentEmail + "\r\nSubject: hi\r\n\r\nhello"
	derr := sendSMTPCaptureErr(addr, "sender@ext.test", agentEmail, body)
	if derr == nil || !strings.Contains(derr.Error(), "451") {
		t.Fatalf("expected a 451 transient error, got: %v", derr)
	}

	assertSingleOutcome(t, metrics, "tempfail", false)
}

// TestSMTPInboundMetric_AsyncAcceptedAndDedup covers the queue-first accept-tx:
// the first send records "accepted"; a byte-identical resend (lost-ack MTA
// retry) hits the intake dedup and records "accepted_dedup".
func TestSMTPInboundMetric_AsyncAcceptedAndDedup(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	const domain = "metrics-async.example.com"
	const agentEmail = "bot@" + domain
	user, err := store.CreateOrGetUser(ctx, "owner@"+domain, "O", "g-metrics-async")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("domain: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE domains SET verified=true WHERE domain=$1`, domain); err != nil {
		t.Fatalf("verify domain: %v", err)
	}
	if _, err := store.CreateAgent(ctx, agentEmail, domain, "", "", "", user.ID); err != nil {
		t.Fatalf("agent: %v", err)
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	cfg := &config.Config{
		SMTP:    config.SMTPConfig{ListenAddr: "127.0.0.1:" + port, Domain: domain},
		Inbound: config.InboundConfig{Mode: "async"},
		Env:     "development",
	}
	server := relay.NewServer(cfg, store, usage.NewNoopUsageTracker(), ws.NewHub())
	server.SetOutbox(webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)))
	server.SetInboundEnqueuer(&fakeInboundEnq{})
	metrics := &fakeSMTPMetrics{}
	server.SetMetrics(metrics)
	addr := startMetricsRelay(t, cfg, server)

	body := "From: sender@ext.test\r\nTo: " + agentEmail + "\r\nMessage-ID: <metric-dedup-1@ext.test>\r\nSubject: hi\r\n\r\nhello"
	sendSMTP(t, addr, "sender@ext.test", agentEmail, body)
	sendSMTP(t, addr, "sender@ext.test", agentEmail, body) // identical resend → dedup

	calls := metrics.snapshot()
	if len(calls) != 2 {
		t.Fatalf("SMTPInbound calls = %v, want exactly 2 (accepted, accepted_dedup)", calls)
	}
	if calls[0].outcome != "accepted" {
		t.Errorf("first send outcome = %q, want %q", calls[0].outcome, "accepted")
	}
	if calls[1].outcome != "accepted_dedup" {
		t.Errorf("resend outcome = %q, want %q", calls[1].outcome, "accepted_dedup")
	}
	for _, c := range calls {
		if c.seconds < 0 {
			t.Errorf("SMTPInbound seconds = %v, want >= 0", c.seconds)
		}
	}
}

// TestSMTPInboundMetric_RejectedLineTooLong drives a DATA payload with a single
// line over the relay's 128KiB MaxLineLength and asserts the 554 abort records
// exactly one "rejected_line_too_long" observation — the original long-line
// incident surfaced as a customer complaint precisely because this path
// recorded nothing.
func TestSMTPInboundMetric_RejectedLineTooLong(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	const domain = "metrics-longline.example.com"
	const agentEmail = "bot@" + domain
	user, err := store.CreateOrGetUser(ctx, "owner@"+domain, "O", "g-metrics-longline")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("domain: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE domains SET verified=true WHERE domain=$1`, domain); err != nil {
		t.Fatalf("verify domain: %v", err)
	}
	if _, err := store.CreateAgent(ctx, agentEmail, domain, "", "", "", user.ID); err != nil {
		t.Fatalf("agent: %v", err)
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	cfg := &config.Config{SMTP: config.SMTPConfig{ListenAddr: "127.0.0.1:" + port, Domain: domain}, Env: "development"}
	server := relay.NewServer(cfg, store, usage.NewNoopUsageTracker(), ws.NewHub())
	metrics := &fakeSMTPMetrics{}
	server.SetMetrics(metrics)
	addr := startMetricsRelay(t, cfg, server)

	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatalf("smtp.Dial: %v", err)
	}
	defer c.Close()
	if err := c.Mail("sender@ext.test"); err != nil {
		t.Fatalf("MAIL FROM: %v", err)
	}
	if err := c.Rcpt(agentEmail); err != nil {
		t.Fatalf("RCPT TO: %v", err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	// A single 160KiB line — over the 128KiB cap, small enough that the
	// unread remainder after the server aborts fits in loopback socket
	// buffers (the server reads ~128KiB before erroring).
	body := "From: sender@ext.test\r\nTo: " + agentEmail + "\r\nSubject: too long\r\n\r\n" +
		strings.Repeat("y", 160*1024)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("write body: %v", err)
	}
	cerr := w.Close()
	if cerr == nil {
		t.Fatal("DATA with a 160KiB line succeeded; want 554 too-long-line rejection")
	}
	if !strings.Contains(cerr.Error(), "554") || !strings.Contains(cerr.Error(), "too long a line") {
		t.Fatalf("DATA close error = %v, want 554 too-long-line", cerr)
	}

	assertSingleOutcome(t, metrics, "rejected_line_too_long", false)
}

// TestSMTPInboundMetric_NilMetricsSafe proves the default (no SetMetrics call)
// stays nil-safe on both the RCPT rejection path and the DATA accept path.
func TestSMTPInboundMetric_NilMetricsSafe(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	const domain = "metrics-nil.example.com"
	const agentEmail = "bot@" + domain
	user, err := store.CreateOrGetUser(ctx, "owner@"+domain, "O", "g-metrics-nil")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("domain: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE domains SET verified=true WHERE domain=$1`, domain); err != nil {
		t.Fatalf("verify domain: %v", err)
	}
	if _, err := store.CreateAgent(ctx, agentEmail, domain, "", "", "", user.ID); err != nil {
		t.Fatalf("agent: %v", err)
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	cfg := &config.Config{SMTP: config.SMTPConfig{ListenAddr: "127.0.0.1:" + port, Domain: domain}, Env: "development"}
	server := relay.NewServer(cfg, store, usage.NewNoopUsageTracker(), ws.NewHub())
	server.SetOutbox(webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)))
	// NO SetMetrics — the relay must not panic on any instrumented path.
	addr := startMetricsRelay(t, cfg, server)

	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatalf("smtp.Dial: %v", err)
	}
	if err := c.Mail("sender@ext.test"); err != nil {
		t.Fatalf("MAIL FROM: %v", err)
	}
	if rerr := c.Rcpt("nobody@" + domain); rerr == nil {
		t.Fatal("RCPT TO an unknown recipient succeeded; want 550")
	}
	_ = c.Quit()
	c.Close()

	body := "From: sender@ext.test\r\nTo: " + agentEmail + "\r\nSubject: nil metrics\r\n\r\nhello"
	sendSMTP(t, addr, "sender@ext.test", agentEmail, body) // fails the test on any SMTP error
}
