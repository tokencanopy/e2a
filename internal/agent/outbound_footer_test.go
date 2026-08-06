package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

const footerMarker = "Created with Example - https://example.com/footer"

// setupFooterAPI mirrors setupAsyncAPI but keeps the sender handle so the
// footer content can be configured, and wires a REAL enforcer (cacheTTL 0)
// whose Defaults carry default_enabled=true — the hosted posture where
// row-less accounts are entitled.
func setupFooterAPI(t *testing.T) (*agent.API, *identity.Store, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	sender.SetSendingStatusLookup(store)
	sender.SetOutboundFooter(footerMarker, `<p>Created with <a href="https://example.com/footer">Example</a></p>`)
	api := agent.NewAPI(store, sender, smtpRelay, nil, usage.NewNoopUsageTracker(),
		"e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.SetOutbox(webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)))
	enq := &fakeOutboundEnqueuer{jobID: 997}
	api.SetOutboundEnqueuer(enq)
	store.SetOutboundJobCanceller(enq)
	api.SetEnforcer(limits.NewEnforcer(limits.NewStore(pool), usage.NewStore(pool), limits.Defaults{
		PlanCode: "default", MaxAgents: 100, MaxDomains: 10,
		MaxMessagesMonth: 1_000_000, MaxStorageBytes: 1 << 40,
		OutboundFooterEnabled: true, // outbound_footer.default_enabled
	}, 0))
	api.SetOutboundFooterEnabled(true)
	return api, store, pool
}

func acceptedRaw(t *testing.T, store *identity.Store, messageID string) string {
	t.Helper()
	var raw []byte
	if err := store.WithTx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT raw_message FROM messages WHERE id=$1`, messageID).Scan(&raw)
	}); err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func deliverExternal(t *testing.T, api *agent.API, user *identity.User, ag *identity.AgentIdentity, subject string) *agent.OutboundResult {
	t.Helper()
	res, oerr := api.DeliverOutbound(context.Background(), user, ag, outbound.SendRequest{
		To: []string{"user@example.net"}, Subject: subject, Body: "body text",
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: status=%d code=%s msg=%s", oerr.Status, oerr.Code, oerr.Msg)
	}
	return res
}

// TestDeliverOutboundFooterGatingMatrix drives the real accept path against a
// real DB + enforcer through the states the hosted deployment will see:
// row-less standard account (default_enabled applies), explicit row false /
// true (row wins), config master switch off, and a non-standard account class.
func TestDeliverOutboundFooterGatingMatrix(t *testing.T) {
	api, store, pool := setupFooterAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "footergate")
	// AccountClass is loaded at auth in production (API-key path); the test
	// fixture user carries none, so stamp the standard class explicitly.
	user.AccountClass = "standard"

	// 1. No account_limits row → default_enabled (true) applies.
	res := deliverExternal(t, api, user, ag, "footer rowless")
	if raw := acceptedRaw(t, store, res.MessageID); !strings.Contains(raw, footerMarker) {
		t.Fatalf("row-less standard account: footer missing:\n%s", raw)
	}

	// 2. Row present with outbound_footer_enabled=false (the paid-plan shape,
	// and the column default) → row wins over the true default.
	if err := limits.NewStore(pool).Upsert(ctx, user.ID, limits.Limits{
		PlanCode: "pro", MaxAgents: 100, MaxDomains: 10,
		MaxMessagesMonth: 1_000_000, MaxStorageBytes: 1 << 40,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	res = deliverExternal(t, api, user, ag, "footer row off")
	if raw := acceptedRaw(t, store, res.MessageID); strings.Contains(raw, footerMarker) {
		t.Fatalf("row-off account still got the footer:\n%s", raw)
	}

	// 3. Provisioner flips the row on (the Free-plan shape) → footer returns.
	if _, err := pool.Exec(ctx, `UPDATE account_limits SET outbound_footer_enabled = TRUE WHERE user_id = $1`, user.ID); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	res = deliverExternal(t, api, user, ag, "footer row on")
	if raw := acceptedRaw(t, store, res.MessageID); !strings.Contains(raw, footerMarker) {
		t.Fatalf("row-on account missing the footer:\n%s", raw)
	}

	// 4. Non-standard account class never gets the footer, even entitled.
	internalUser := *user
	internalUser.AccountClass = "internal"
	res = deliverExternal(t, api, &internalUser, ag, "footer internal class")
	if raw := acceptedRaw(t, store, res.MessageID); strings.Contains(raw, footerMarker) {
		t.Fatalf("internal-class account got the footer:\n%s", raw)
	}

	// 5. Config master switch off → no footer regardless of entitlement.
	api.SetOutboundFooterEnabled(false)
	res = deliverExternal(t, api, user, ag, "footer config off")
	if raw := acceptedRaw(t, store, res.MessageID); strings.Contains(raw, footerMarker) {
		t.Fatalf("config-off deployment appended the footer:\n%s", raw)
	}
}

// TestApprovePendingFooterResolvedAtApprovalTime: held mail composes at
// approval time, so the footer decision is resolved by the approve funnel
// (dashboard ApprovePendingCore → approveOutboundAsyncComposed, shared with
// the magic-link path) via the owning account's DB user — not by any flag on
// the held row. The held draft itself must stay footer-free.
func TestApprovePendingFooterResolvedAtApprovalTime(t *testing.T) {
	api, store, pool := setupFooterAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "footerapprove")
	user.AccountClass = "standard"
	// Force a review hold for the external send.
	ag.OutboundPolicy = identity.OutboundPolicyAllowlist
	ag.OutboundPolicyAction = "review"
	ag.OutboundScan = identity.ScanOff

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"user@example.net"}, Subject: "held then footered", Body: "body text",
	}, "send", "", nil, nil)
	if oerr != nil || res == nil || !res.Held {
		t.Fatalf("result=%+v error=%+v, want held", res, oerr)
	}
	var heldBody string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(body_text,'') FROM messages WHERE id=$1`, res.PendingMessageID).Scan(&heldBody); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(heldBody, footerMarker) {
		t.Fatalf("held draft must not carry the footer (it composes at approval):\n%s", heldBody)
	}

	// Dashboard approve: the funnel resolves the footer from the DB user
	// (account_class defaults to 'standard') + the row-less entitlement default.
	sent, aerr := api.ApprovePendingCore(ctx, user.ID, res.PendingMessageID, "", agent.ApproveOverrides{}, nil)
	if aerr != nil {
		t.Fatalf("ApprovePendingCore: status=%d code=%s msg=%s", aerr.Status, aerr.Code, aerr.Msg)
	}
	if raw := acceptedRaw(t, store, sent.ID); !strings.Contains(raw, footerMarker) {
		t.Fatalf("approved held mail missing the footer:\n%s", raw)
	}

	// Same funnel, non-standard owner: flip the DB class and approve a second
	// hold — no footer.
	if err := store.SetAccountClass(ctx, user.ID, "internal"); err != nil {
		t.Fatal(err)
	}
	res2, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"user@example.net"}, Subject: "held internal class", Body: "body text",
	}, "send", "", nil, nil)
	if oerr != nil || !res2.Held {
		t.Fatalf("result=%+v error=%+v, want held", res2, oerr)
	}
	sent2, aerr := api.ApprovePendingCore(ctx, user.ID, res2.PendingMessageID, "", agent.ApproveOverrides{}, nil)
	if aerr != nil {
		t.Fatalf("ApprovePendingCore (internal): %+v", aerr)
	}
	if raw := acceptedRaw(t, store, sent2.ID); strings.Contains(raw, footerMarker) {
		t.Fatalf("internal-class owner's approved mail carries the footer:\n%s", raw)
	}
}

// TestDeliverOutboundFooterSelfSendNeverFootered: self-send loopback bypasses
// Sender.compose entirely, so an entitled account's note-to-self carries no
// footer — in either stored copy.
func TestDeliverOutboundFooterSelfSendNeverFootered(t *testing.T) {
	api, store, pool := setupFooterAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "footerself")
	user.AccountClass = "standard"

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{ag.EmailAddress()}, Subject: "footer self send", Body: "note to self",
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %v", oerr)
	}
	if res.Method != "loopback" {
		t.Fatalf("method=%q want loopback", res.Method)
	}
	var footered int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE agent_id=$1
		   AND (convert_from(COALESCE(raw_message, ''::bytea), 'UTF8') LIKE '%'||$2||'%'
		     OR COALESCE(body_text, '') LIKE '%'||$2||'%')`,
		ag.ID, footerMarker,
	).Scan(&footered); err != nil {
		t.Fatal(err)
	}
	if footered != 0 {
		t.Errorf("%d self-send message rows carry the footer, want 0", footered)
	}
}
