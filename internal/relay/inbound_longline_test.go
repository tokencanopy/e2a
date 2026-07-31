package relay_test

import (
	"context"
	"net/smtp"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/relay"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/ws"
)

// Regression test for the go-smtp MaxLineLength default (2000 bytes).
// Agent-generated mail — single-line JSON, unfolded HTML, unwrapped
// base64 — routinely exceeds that, and the relay used to reject it at
// DATA with "554 ... too long a line in input stream". This bit a real
// customer whose two agents messaged each other (prod, 2026-07-18..20,
// 30 bounces). The relay now allows lines up to 1MiB; the whole message
// stays capped by MaxMessageBytes.
//
// Same integration shape as inbound_limit_test.go: needs a real DB and
// an actual SMTP socket bind, so skipped under -short.
func TestRelay_Data_AcceptsLongLines(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SMTP integration test under -short")
	}
	pool := testutil.TestDB(t)
	ctx := context.Background()
	store := identity.NewStore(pool)

	user, err := store.CreateOrGetUser(ctx, "relay-longline@test.com", "Test", "google-relay-longline@test.com")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, "relay-longline.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE domains SET verified = true WHERE domain = $1`, "relay-longline.example.com"); err != nil {
		t.Fatalf("verify domain: %v", err)
	}
	agentEmail := "bot@relay-longline.example.com"
	if _, err := store.CreateAgent(ctx, agentEmail, "relay-longline.example.com", "", "https://example.com/w", "cloud", user.ID); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	cfg := &config.Config{
		SMTP: config.SMTPConfig{ListenAddr: "127.0.0.1:" + port, Domain: "test.relay"},
		Env:  "development",
	}
	server := relay.NewServer(cfg, store, usage.NewNoopUsageTracker(), ws.NewHub())
	go func() { _ = server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Close() })
	waitForSMTP(t, cfg.SMTP.ListenAddr)

	// A single ~64KiB body line — the shape real agent payloads take
	// (one JSON object, one unfolded HTML document). Well past the old
	// 2000-byte default, well under the new 1MiB cap.
	longLine := strings.Repeat("x", 64*1024)
	body := "From: sender@elsewhere.test\r\nTo: " + agentEmail + "\r\n" +
		"Subject: long line\r\nContent-Type: application/json\r\n\r\n" + longLine

	c, err := smtp.Dial(cfg.SMTP.ListenAddr)
	if err != nil {
		t.Fatalf("smtp.Dial: %v", err)
	}
	defer c.Close()
	if err := c.Mail("sender@elsewhere.test"); err != nil {
		t.Fatalf("MAIL FROM: %v", err)
	}
	if err := c.Rcpt(agentEmail); err != nil {
		t.Fatalf("RCPT TO: %v", err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close DATA with a 64KiB line: %v (long-line rejection regressed?)", err)
	}
	_ = c.Quit()

	// The message must actually land in the agent's inbox, not merely
	// be accepted at the SMTP layer.
	if !waitFor(func() bool {
		msgs, err := store.GetMessagesByAgent(ctx, identity.MessageListFilter{
			AgentID: agentEmail, Direction: "inbound", Status: "all", Limit: 10,
		})
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.Subject == "long line" {
				return true
			}
		}
		return false
	}) {
		t.Error("long-line message never appeared in the agent inbox")
	}
}
