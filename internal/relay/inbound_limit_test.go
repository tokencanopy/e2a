package relay_test

import (
	"context"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/relay"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/ws"
)

// Integration tests for the SMTP-level enforcement path. Usage-based
// pricing v1: message-flow caps are OUTBOUND-only — a recipient's
// exhausted send allowance must never bounce a stranger's inbound mail.
// Only the STORAGE cap still rejects at RCPT time, with SMTP 552
// ("mailbox quota exceeded") so the upstream MTA bounces the message
// back to the original sender.
//
// This is the inbound counterpart to the HTTP 402 wiring tests in
// internal/agent/. Skipped under -short because it needs a real DB
// and an actual SMTP socket bind.

func TestRelay_RcptTo_Rejects552WhenStorageExhausted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SMTP integration test under -short")
	}
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)

	// Seed a user + verified domain + agent. The agent's owner is the
	// user_id the enforcer will look up.
	user, err := store.CreateOrGetUser(ctx, "relay-cap@test.com", "Test", "google-relay-cap@test.com")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, "relay-cap.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	// Flip verified so the domain-not-verified guard doesn't fire
	// first (Rcpt() checks DomainVerified before the limits check).
	if _, err := pool.Exec(ctx, `UPDATE domains SET verified = true WHERE domain = $1`, "relay-cap.example.com"); err != nil {
		t.Fatalf("verify domain: %v", err)
	}
	agentEmail := "bot@relay-cap.example.com"
	if _, err := store.CreateAgent(ctx, agentEmail, "relay-cap.example.com", "", "https://example.com/w", "cloud", user.ID); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Block the user via account_limits — max_storage_bytes=0 makes the
	// next CheckInboundMessage trip immediately (storage 0 >= cap 0). The
	// flow cap is deliberately generous: it must play no part here.
	lstore := limits.NewStore(pool)
	if err := lstore.Upsert(ctx, user.ID, limits.Limits{
		PlanCode:         "free_test",
		MaxAgents:        100,
		MaxDomains:       100,
		MaxMessagesMonth: 100_000,
		MaxStorageBytes:  0,
	}); err != nil {
		t.Fatalf("Upsert limits: %v", err)
	}

	// Stand up a real relay.Server on an ephemeral port.
	cfg := &config.Config{
		SMTP: config.SMTPConfig{
			ListenAddr: "127.0.0.1:0", // ephemeral
			Domain:     "test.relay",
		},
		Env: "development",
	}
	// usage tracker + ws hub aren't relevant for the RCPT path under
	// test; pass benign defaults.
	server := relay.NewServer(cfg, store, usage.NewNoopUsageTracker(), ws.NewHub())
	enf := limits.NewEnforcer(lstore, usage.NewStore(pool), limits.Defaults{
		PlanCode: "default", MaxAgents: 1, MaxDomains: 1, MaxMessagesMonth: 1, MaxStorageBytes: 1,
	}, 0)
	server.SetEnforcer(enf)

	// ListenAndServe blocks; run it in a goroutine. We don't have a
	// public accessor for the port the OS picked, so we open the
	// listener ourselves, hand it to the server, and read the addr.
	// Simpler: bind a free port up front and pass via config.
	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	cfg.SMTP.ListenAddr = "127.0.0.1:" + port
	// Rebuild server with the resolved port (NewServer copies ListenAddr).
	server = relay.NewServer(cfg, store, usage.NewNoopUsageTracker(), ws.NewHub())
	server.SetEnforcer(enf)

	listenErrCh := make(chan error, 1)
	go func() { listenErrCh <- server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Close() })

	// Wait for the server to be ready to accept. Poll the port up to
	// 2s — the SMTP banner write is fast once the listener is bound.
	deadline := time.Now().Add(2 * time.Second)
	var conn net.Conn
	for time.Now().Before(deadline) {
		conn, err = net.Dial("tcp", cfg.SMTP.ListenAddr)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never accepted on %s: %v", cfg.SMTP.ListenAddr, err)
	}

	// Open an SMTP client and run the handshake. Rcpt() should reject
	// with 552 because the user is at message cap.
	c, err := smtp.Dial(cfg.SMTP.ListenAddr)
	if err != nil {
		t.Fatalf("smtp.Dial: %v", err)
	}
	defer c.Close()

	if err := c.Mail("sender@elsewhere.test"); err != nil {
		t.Fatalf("MAIL FROM: %v", err)
	}
	err = c.Rcpt(agentEmail)
	if err == nil {
		t.Fatal("RCPT TO succeeded; expected 552 storage-cap rejection")
	}
	// net/smtp error message looks like "552 5.2.2 mailbox quota exceeded".
	msg := err.Error()
	if !strings.Contains(msg, "552") {
		t.Errorf("RCPT TO error = %q, want it to contain '552'", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "quota") {
		t.Errorf("RCPT TO error = %q, want it to mention 'quota'", msg)
	}
}

// freePort asks the OS for a port, closes the listener so the relay
// can bind to it. Race-prone in theory (another process could grab
// it between us closing and the relay binding) but fine for tests.
func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	_, p, err := net.SplitHostPort(l.Addr().String())
	return p, err
}

// TestRelay_RcptTo_AcceptsWhenUnderCap is the happy-path baseline —
// proves the test setup doesn't falsely accept everything.
func TestRelay_RcptTo_AcceptsWhenUnderCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SMTP integration test under -short")
	}
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	user, _ := store.CreateOrGetUser(ctx, "relay-ok@test.com", "Test", "google-relay-ok@test.com")
	if _, err := store.ClaimOrCreateDomain(ctx, "relay-ok.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE domains SET verified = true WHERE domain = $1`, "relay-ok.example.com"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	agentEmail := "bot@relay-ok.example.com"
	if _, err := store.CreateAgent(ctx, agentEmail, "relay-ok.example.com", "", "https://example.com/w", "cloud", user.ID); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	// Generous caps — RCPT should succeed.
	lstore := limits.NewStore(pool)
	if err := lstore.Upsert(ctx, user.ID, limits.Limits{
		PlanCode:  "pro_test",
		MaxAgents: 100, MaxDomains: 100, MaxMessagesMonth: 100_000, MaxStorageBytes: 1 << 40,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	port, _ := freePort()
	cfg := &config.Config{SMTP: config.SMTPConfig{ListenAddr: "127.0.0.1:" + port, Domain: "test.relay"}, Env: "development"}
	server := relay.NewServer(cfg, store, usage.NewNoopUsageTracker(), ws.NewHub())
	enf := limits.NewEnforcer(lstore, usage.NewStore(pool),
		limits.Defaults{PlanCode: "default", MaxAgents: 1, MaxDomains: 1, MaxMessagesMonth: 1, MaxStorageBytes: 1}, 0)
	server.SetEnforcer(enf)
	go func() { _ = server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Close() })

	// Wait-for-ready
	deadline := time.Now().Add(2 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		var conn net.Conn
		conn, err = net.Dial("tcp", cfg.SMTP.ListenAddr)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server not ready: %v", err)
	}

	c, err := smtp.Dial(cfg.SMTP.ListenAddr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.Mail("sender@elsewhere.test"); err != nil {
		t.Fatalf("MAIL FROM: %v", err)
	}
	if err := c.Rcpt(agentEmail); err != nil {
		t.Errorf("RCPT TO under-cap returned %v; want nil (accepted)", err)
	}
}

// TestRelay_RcptTo_AcceptsWhenFlowCapExhausted pins the usage-based
// pricing v1 semantics: an exhausted message-flow allowance (outbound
// quota) must NOT bounce inbound mail. max_messages_month=0 blocks every
// outbound send for this account, yet RCPT TO must still succeed because
// only the storage cap guards the inbound path.
func TestRelay_RcptTo_AcceptsWhenFlowCapExhausted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SMTP integration test under -short")
	}
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	user, _ := store.CreateOrGetUser(ctx, "relay-flow@test.com", "Test", "google-relay-flow@test.com")
	if _, err := store.ClaimOrCreateDomain(ctx, "relay-flow.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE domains SET verified = true WHERE domain = $1`, "relay-flow.example.com"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	agentEmail := "bot@relay-flow.example.com"
	if _, err := store.CreateAgent(ctx, agentEmail, "relay-flow.example.com", "", "https://example.com/w", "cloud", user.ID); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	// Flow cap exhausted (0 = hard-block for sends), storage generous.
	lstore := limits.NewStore(pool)
	if err := lstore.Upsert(ctx, user.ID, limits.Limits{
		PlanCode:  "free_test",
		MaxAgents: 100, MaxDomains: 100, MaxMessagesMonth: 0, MaxStorageBytes: 1 << 40,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	port, _ := freePort()
	cfg := &config.Config{SMTP: config.SMTPConfig{ListenAddr: "127.0.0.1:" + port, Domain: "test.relay"}, Env: "development"}
	server := relay.NewServer(cfg, store, usage.NewNoopUsageTracker(), ws.NewHub())
	enf := limits.NewEnforcer(lstore, usage.NewStore(pool),
		limits.Defaults{PlanCode: "default", MaxAgents: 1, MaxDomains: 1, MaxMessagesMonth: 1, MaxStorageBytes: 1}, 0)
	server.SetEnforcer(enf)

	go func() { _ = server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Close() })
	deadline := time.Now().Add(2 * time.Second)
	var conn net.Conn
	var err error
	for time.Now().Before(deadline) {
		conn, err = net.Dial("tcp", cfg.SMTP.ListenAddr)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never accepted on %s: %v", cfg.SMTP.ListenAddr, err)
	}

	c, err := smtp.Dial(cfg.SMTP.ListenAddr)
	if err != nil {
		t.Fatalf("smtp.Dial: %v", err)
	}
	defer c.Close()
	if err := c.Mail("sender@elsewhere.test"); err != nil {
		t.Fatalf("MAIL FROM: %v", err)
	}
	if err := c.Rcpt(agentEmail); err != nil {
		t.Fatalf("RCPT TO rejected (%v); an exhausted flow cap must not bounce inbound mail", err)
	}
}
