package limits_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
)

// These tests exercise the real Postgres trigger + account_limits + the
// enforcer end to end. They share the test database with other DB-using
// packages, so we skip under `go test -short` (used by `make test-unit`)
// to avoid trampling concurrent-package state. `make test-integration`
// runs without -short and serializes packages with -p 1.

func setupLimitsUser(t *testing.T, name string) (*pgxpool.Pool, *identity.Store, *usage.Store, *identity.AgentIdentity, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping DB-backed limits test under -short")
	}
	pool := testutil.TestDB(t)
	idStore := identity.NewStore(pool)
	usageStore := usage.NewStore(pool)
	ctx := context.Background()

	user, err := idStore.CreateOrGetUser(ctx, name+"@limits.test", name, "google-sub-"+name)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := idStore.ClaimOrCreateDomain(ctx, name+".example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	agent, err := idStore.CreateAgent(ctx, "bot@"+name+".example.com", name+".example.com", "", "https://example.com/w", "cloud", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return pool, idStore, usageStore, agent, user.ID
}

func TestStorageTrigger_IncrementsOnInsert(t *testing.T) {
	_, idStore, usageStore, agent, userID := setupLimitsUser(t, "stg1")
	ctx := context.Background()

	// Outbound row has no raw_message / body_*, so storage delta is 0
	// for the subject column (subject isn't in the trigger's size sum).
	// Use the HITL pending-outbound path to exercise body_text/body_html.
	attachJSON := []byte(`[{"filename":"a.txt","content_type":"text/plain","data":"aGVsbG8="}]`)
	_, err := idStore.CreatePendingOutboundMessage(ctx, agent.ID,
		[]string{"alice@example.com"}, nil, nil,
		"hi", "body text here", "<p>body html here</p>", attachJSON,
		"send", "", "", "", 60)
	if err != nil {
		t.Fatalf("CreatePendingOutboundMessage: %v", err)
	}

	got, err := usageStore.GetStorageBytes(ctx, userID)
	if err != nil {
		t.Fatalf("GetStorageBytes: %v", err)
	}
	wantAtLeast := int64(len("body text here") + len("<p>body html here</p>") + len(attachJSON))
	if got < wantAtLeast {
		t.Errorf("storage_bytes = %d, want >= %d", got, wantAtLeast)
	}
}

func TestStorageTrigger_DecrementsOnDelete(t *testing.T) {
	pool, idStore, usageStore, agent, userID := setupLimitsUser(t, "stg2")
	ctx := context.Background()

	msg, err := idStore.CreatePendingOutboundMessage(ctx, agent.ID,
		[]string{"alice@example.com"}, nil, nil,
		"hi", "some body text", "", nil,
		"send", "", "", "", 60)
	if err != nil {
		t.Fatalf("CreatePendingOutboundMessage: %v", err)
	}

	before, _ := usageStore.GetStorageBytes(ctx, userID)
	if before == 0 {
		t.Fatalf("storage_bytes did not increment on insert (got 0)")
	}

	// Hard-delete the message to fire the AFTER DELETE trigger. We use
	// raw SQL because the public store API only soft-expires; the
	// trigger correctness for hard deletes (retention sweep, cascade)
	// is what we want to verify.
	if _, err := pool.Exec(ctx, `DELETE FROM messages WHERE id = $1`, msg.ID); err != nil {
		t.Fatalf("DELETE: %v", err)
	}

	after, _ := usageStore.GetStorageBytes(ctx, userID)
	if after >= before {
		t.Errorf("storage_bytes = %d after delete, want < %d", after, before)
	}
}

// TestUpsert_PreservesColumnsOutsideSetList_RealDB pins Upsert's
// preserve-on-conflict semantics for columns OUTSIDE its SET list (per the
// CLAUDE.md schema-change rule: every package writing direct SQL against a
// reshaped table needs a DB-backed test). account_limits has since grown
// columns Upsert doesn't know about (max_webhooks, max_templates, …); an
// Upsert refresh of the plan fields must never clobber them back to
// defaults.
func TestUpsert_PreservesColumnsOutsideSetList_RealDB(t *testing.T) {
	pool, _, _, _, userID := setupLimitsUser(t, "upsertpreserve")
	ctx := context.Background()
	limitsStore := limits.NewStore(pool)

	// Fresh insert: the column default applies.
	if err := limitsStore.Upsert(ctx, userID, limits.Limits{
		PlanCode: "test", MaxAgents: 1, MaxDomains: 1,
		MaxMessagesMonth: 100, MaxStorageBytes: 1 << 20,
	}); err != nil {
		t.Fatalf("Upsert (insert): %v", err)
	}
	var maxTemplates int
	if err := pool.QueryRow(ctx,
		`SELECT max_templates FROM account_limits WHERE user_id = $1`, userID,
	).Scan(&maxTemplates); err != nil {
		t.Fatalf("read max_templates: %v", err)
	}
	if maxTemplates != 10 {
		t.Fatalf("fresh row max_templates = %d, want column default 10", maxTemplates)
	}

	// An external provisioner raises the out-of-set-list column…
	if _, err := pool.Exec(ctx,
		`UPDATE account_limits SET max_templates = 500 WHERE user_id = $1`, userID,
	); err != nil {
		t.Fatalf("manual UPDATE: %v", err)
	}

	// …and a later plan refresh via Upsert must not clobber it.
	if err := limitsStore.Upsert(ctx, userID, limits.Limits{
		PlanCode: "pro", MaxAgents: 50, MaxDomains: 10,
		MaxMessagesMonth: 100000, MaxStorageBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("Upsert (conflict): %v", err)
	}
	var planCode string
	if err := pool.QueryRow(ctx,
		`SELECT plan_code, max_templates FROM account_limits WHERE user_id = $1`, userID,
	).Scan(&planCode, &maxTemplates); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if planCode != "pro" {
		t.Errorf("plan_code = %q, want pro (SET-list columns must update)", planCode)
	}
	if maxTemplates != 500 {
		t.Errorf("max_templates = %d after Upsert, want 500 preserved (outside the SET list)", maxTemplates)
	}
}

// TestStoreGet_OutboundFooterColumn_RealDB pins the migration-095 column read:
// a fresh row carries the column default FALSE; an external provisioner
// flipping it to TRUE is visible through Store.Get. (Upsert deliberately does
// NOT include the column in its SET list — the preserve-on-conflict test above
// covers that class of column.)
func TestStoreGet_OutboundFooterColumn_RealDB(t *testing.T) {
	pool, _, _, _, userID := setupLimitsUser(t, "footercol")
	ctx := context.Background()
	limitsStore := limits.NewStore(pool)

	if err := limitsStore.Upsert(ctx, userID, limits.Limits{
		PlanCode: "free", MaxAgents: 1, MaxDomains: 1,
		MaxMessagesMonth: 100, MaxStorageBytes: 1 << 20,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, found, err := limitsStore.Get(ctx, userID)
	if err != nil || !found {
		t.Fatalf("Get: err=%v found=%v", err, found)
	}
	if got.OutboundFooterEnabled {
		t.Error("fresh row OutboundFooterEnabled = true, want column default false")
	}

	if _, err := pool.Exec(ctx,
		`UPDATE account_limits SET outbound_footer_enabled = TRUE WHERE user_id = $1`, userID,
	); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	got, found, err = limitsStore.Get(ctx, userID)
	if err != nil || !found {
		t.Fatalf("Get (after flip): err=%v found=%v", err, found)
	}
	if !got.OutboundFooterEnabled {
		t.Error("OutboundFooterEnabled = false after provisioner flip, want true")
	}
}

func TestEnforcer_BlocksAtAgentCap_RealDB(t *testing.T) {
	pool, idStore, usageStore, _, userID := setupLimitsUser(t, "agentcap")
	ctx := context.Background()

	limitsStore := limits.NewStore(pool)
	// Write a tight cap directly: the user already has 1 agent (from
	// fixture). max_agents=1 means the next create should be blocked.
	if err := limitsStore.Upsert(ctx, userID, limits.Limits{
		PlanCode: "test", MaxAgents: 1, MaxDomains: 100,
		MaxMessagesMonth: 1_000_000, MaxStorageBytes: 1 << 40,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	enf := limits.NewEnforcer(limitsStore, usageStore, limits.Defaults{
		PlanCode: "default", MaxAgents: 1_000_000, MaxDomains: 1_000_000,
		MaxMessagesMonth: 1_000_000_000, MaxStorageBytes: 1 << 50,
	}, 0)

	err := enf.CheckAgentCreate(ctx, userID)
	le, ok := limits.IsLimitExceeded(err)
	if !ok {
		t.Fatalf("CheckAgentCreate at 1/1 cap: got %v, want LimitExceededError", err)
	}
	if le.Resource != "agents" || le.Limit != 1 || le.Current != 1 {
		t.Errorf("LimitExceeded = %+v, want agents 1/1", le)
	}
	if le.Limits.PlanCode != "test" {
		t.Errorf("PlanCode = %q, want test", le.Limits.PlanCode)
	}

	// Sanity: a fresh agent insert via the real store should succeed
	// when limits are loosened.
	if err := limitsStore.Upsert(ctx, userID, limits.Limits{
		PlanCode: "test", MaxAgents: 5, MaxDomains: 100,
		MaxMessagesMonth: 1_000_000, MaxStorageBytes: 1 << 40,
	}); err != nil {
		t.Fatalf("Upsert (loosen): %v", err)
	}
	enf.Invalidate(userID)
	if err := enf.CheckAgentCreate(ctx, userID); err != nil {
		t.Errorf("CheckAgentCreate after loosen: %v, want nil", err)
	}

	// Side check: limits row is visible via Get.
	got, found, err := limitsStore.Get(ctx, userID)
	if err != nil || !found {
		t.Fatalf("Get: err=%v found=%v", err, found)
	}
	if got.MaxAgents != 5 {
		t.Errorf("Get MaxAgents = %d, want 5", got.MaxAgents)
	}
	_ = idStore // keeps the fixture-imported store referenced
}
