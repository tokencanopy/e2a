package hitlworker_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/hitlworker"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

const footerMarker = "Created with Example - https://example.com/footer"

// TestWorkerAutoApproveAsync_AppendsOutboundFooter pins the TTL sweep's footer
// resolution end to end THROUGH THE PRODUCTION WIRING: the worker's resolver
// is agent.API.OutboundFooterForAccount (exactly what main wires), backed by a
// real enforcer whose row-less default entitles the account. An
// expires-to-approve hold must ship with the same footer a human approval
// would have added — and a non-standard owner must not.
func TestWorkerAutoApproveAsync_AppendsOutboundFooter(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	sender.SetOutboundFooter(footerMarker, `<p>Created with <a href="https://example.com/footer">Example</a></p>`)

	api := agent.NewAPI(store, sender, smtpRelay, nil, usage.NewNoopUsageTracker(),
		"e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.SetEnforcer(limits.NewEnforcer(limits.NewStore(pool), usage.NewStore(pool), limits.Defaults{
		PlanCode: "default", MaxAgents: 100, MaxDomains: 10,
		MaxMessagesMonth: 1_000_000, MaxStorageBytes: 1 << 40,
		OutboundFooterEnabled: true, // outbound_footer.default_enabled
	}, 0))
	api.SetOutboundFooterEnabled(true)

	w := hitlworker.New(store, sender, usage.NewUsageTracker(usage.NewStore(pool)), "test.e2a.dev")
	w.SetOutbox(webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)))
	w.SetOutboundEnqueuer(&fakeEnq{})
	w.SetOutboundFooterResolver(api.OutboundFooterForAccount)

	ctx := context.Background()
	ag := prepareAgent(t, store, "approve-footer", identity.HITLExpirationApprove)

	msg, err := store.CreatePendingOutboundMessage(ctx, ag.ID,
		[]string{"alice@external.test"}, nil, nil,
		"Held footered", "body", "<p>html</p>", nil, "send", "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	backdateExpiry(t, pool, msg.ID)

	w.RunOnce(ctx)

	var status, raw string
	if err := pool.QueryRow(ctx,
		`SELECT status, convert_from(COALESCE(raw_message, ''::bytea), 'UTF8') FROM messages WHERE id=$1`, msg.ID,
	).Scan(&status, &raw); err != nil {
		t.Fatal(err)
	}
	if status != identity.MessageStatusReviewExpiredApproved {
		t.Fatalf("status = %q, want %q", status, identity.MessageStatusReviewExpiredApproved)
	}
	if !strings.Contains(raw, footerMarker) {
		t.Fatalf("TTL auto-approved mail missing the footer:\n%s", raw)
	}

	// Non-standard owner: same sweep, no footer.
	if err := store.SetAccountClass(ctx, ag.UserID, "internal"); err != nil {
		t.Fatal(err)
	}
	msg2, err := store.CreatePendingOutboundMessage(ctx, ag.ID,
		[]string{"alice@external.test"}, nil, nil,
		"Held internal owner", "body", "<p>html</p>", nil, "send", "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	backdateExpiry(t, pool, msg2.ID)

	w.RunOnce(ctx)

	var raw2 string
	if err := pool.QueryRow(ctx,
		`SELECT convert_from(COALESCE(raw_message, ''::bytea), 'UTF8') FROM messages WHERE id=$1`, msg2.ID,
	).Scan(&raw2); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw2, footerMarker) {
		t.Fatalf("internal-class owner's auto-approved mail carries the footer:\n%s", raw2)
	}
}

// TestWorkerAutoApproveAsync_NilFooterResolverNeverFooters pins the fail-closed
// default: a worker without a resolver (self-host, feature off) appends nothing
// even when the sender has footer content configured.
func TestWorkerAutoApproveAsync_NilFooterResolverNeverFooters(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	sender.SetOutboundFooter(footerMarker, "")

	w := hitlworker.New(store, sender, usage.NewUsageTracker(usage.NewStore(pool)), "test.e2a.dev")
	w.SetOutbox(webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)))
	w.SetOutboundEnqueuer(&fakeEnq{})
	// No SetOutboundFooterResolver.

	ctx := context.Background()
	ag := prepareAgent(t, store, "approve-nofooter", identity.HITLExpirationApprove)
	msg, err := store.CreatePendingOutboundMessage(ctx, ag.ID,
		[]string{"alice@external.test"}, nil, nil,
		"Held unfootered", "body", "", nil, "send", "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	backdateExpiry(t, pool, msg.ID)

	w.RunOnce(ctx)

	var raw string
	if err := pool.QueryRow(ctx,
		`SELECT convert_from(COALESCE(raw_message, ''::bytea), 'UTF8') FROM messages WHERE id=$1`, msg.ID,
	).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, footerMarker) {
		t.Fatalf("nil resolver must never footer:\n%s", raw)
	}
}
