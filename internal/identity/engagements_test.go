package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func boolPtr(b bool) *bool { return &b }

// enroll is a terse helper for tests that only need an engagement to exist.
func enroll(t *testing.T, store *identity.Store, userID, agentID, address, stage string) identity.ContactEngagement {
	t.Helper()
	s := stage
	e, _, err := store.UpsertEngagement(context.Background(), userID, agentID, address, &s, nil, nil)
	if err != nil {
		t.Fatalf("enroll %s: %v", address, err)
	}
	return e
}

// TestUpsertEngagementLeavesOmittedFieldsAlone pins the partial-update contract
// the outreach loop depends on: advancing the stage after a send must not
// disturb the next action, and setting a new next action must not reset the
// stage. Getting this wrong means every touch silently rewinds the other field.
func TestUpsertEngagementLeavesOmittedFieldsAlone(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "engupsert")

	due := time.Now().Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
	stage := "touch1"
	dueP := &due
	e, created, err := store.UpsertEngagement(ctx, user.ID, "raise@e.com", "partner@up.vc", &stage, &dueP, nil)
	if err != nil || !created {
		t.Fatalf("first upsert created=%v err=%v", created, err)
	}
	if e.Stage != "touch1" || e.NextActionAt == nil || !e.NextActionAt.Equal(due) {
		t.Fatalf("initial state = %+v", e)
	}

	// Stage only — next_action_at must survive.
	stage2 := "touch2"
	e, created, err = store.UpsertEngagement(ctx, user.ID, "raise@e.com", "partner@up.vc", &stage2, nil, nil)
	if err != nil || created {
		t.Fatalf("stage-only upsert created=%v err=%v", created, err)
	}
	if e.Stage != "touch2" {
		t.Errorf("stage = %q, want touch2", e.Stage)
	}
	if e.NextActionAt == nil || !e.NextActionAt.Equal(due) {
		t.Errorf("next_action_at = %v — a stage-only update disturbed it", e.NextActionAt)
	}

	// next_action_at only — stage must survive.
	later := due.Add(7 * 24 * time.Hour)
	laterP := &later
	e, _, err = store.UpsertEngagement(ctx, user.ID, "raise@e.com", "partner@up.vc", nil, &laterP, nil)
	if err != nil {
		t.Fatalf("next-only upsert: %v", err)
	}
	if e.Stage != "touch2" {
		t.Errorf("stage = %q — a next_action_at-only update reset it", e.Stage)
	}
	if e.NextActionAt == nil || !e.NextActionAt.Equal(later) {
		t.Errorf("next_action_at = %v, want %v", e.NextActionAt, later)
	}

	// An explicit nil clears the schedule (distinct from omitting it).
	var cleared *time.Time
	e, _, err = store.UpsertEngagement(ctx, user.ID, "raise@e.com", "partner@up.vc", nil, &cleared, nil)
	if err != nil {
		t.Fatalf("clear upsert: %v", err)
	}
	if e.NextActionAt != nil {
		t.Errorf("next_action_at = %v, want nil after an explicit clear", e.NextActionAt)
	}
}

// TestEngagementsAreIndependentPerAgent is the reason engagements exist as a
// separate table. The same human worked by two agents must have two independent
// states — if these collided, one agent advancing its stage would corrupt the
// other's outreach.
func TestEngagementsAreIndependentPerAgent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "engmulti")

	enroll(t, store, user.ID, "raise@e.com", "partner@multi.vc", "touch3")
	enroll(t, store, user.ID, "support@e.com", "partner@multi.vc", "new")

	raise, err := store.GetEngagement(ctx, user.ID, "raise@e.com", "partner@multi.vc")
	if err != nil {
		t.Fatalf("raise get: %v", err)
	}
	support, err := store.GetEngagement(ctx, user.ID, "support@e.com", "partner@multi.vc")
	if err != nil {
		t.Fatalf("support get: %v", err)
	}
	if raise.Stage != "touch3" || support.Stage != "new" {
		t.Errorf("stages collided: raise=%q support=%q", raise.Stage, support.Stage)
	}
	// One person, one contact row — identity is account-level.
	if raise.ContactID != support.ContactID {
		t.Errorf("two contact rows for one human: %s vs %s", raise.ContactID, support.ContactID)
	}
}

// TestEngagementSuppressionIsPerAgent pins that consent joins per-agent. An
// investor unsubscribing from raise@ has NOT unsubscribed from support@, and
// the reason must be visible so a user can tell "bad address" from "said no".
func TestEngagementSuppressionIsPerAgent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "engsupp")

	// Real agents: AddAgentSuppression enforces live ownership, and this
	// behaviour is too load-bearing to leave behind a skip.
	if _, err := store.ClaimOrCreateDomain(ctx, "supp.example.com", user.ID); err != nil {
		t.Fatalf("claim domain: %v", err)
	}
	for _, addr := range []string{"raise@supp.example.com", "support@supp.example.com"} {
		if _, err := store.CreateAgent(ctx, addr, "supp.example.com", "",
			"https://example.com/webhook", "", user.ID); err != nil {
			t.Fatalf("create agent %s: %v", addr, err)
		}
	}

	enroll(t, store, user.ID, "raise@supp.example.com", "partner@supp.vc", "touch1")
	enroll(t, store, user.ID, "support@supp.example.com", "partner@supp.vc", "new")

	if _, _, err := store.AddAgentSuppression(ctx, user.ID, "raise@supp.example.com", "partner@supp.vc",
		"asked to stop", "unsubscribe", nil); err != nil {
		t.Fatalf("add agent suppression: %v", err)
	}

	raise, err := store.GetEngagement(ctx, user.ID, "raise@supp.example.com", "partner@supp.vc")
	if err != nil {
		t.Fatalf("raise get: %v", err)
	}
	if !raise.Suppressed {
		t.Error("raise@ engagement not marked suppressed after an unsubscribe")
	}
	if raise.SuppressionSource != "unsubscribe" || raise.SuppressionReason != "asked to stop" {
		t.Errorf("suppression detail = %q/%q — a user cannot tell a bounce from a refusal",
			raise.SuppressionSource, raise.SuppressionReason)
	}

	support, err := store.GetEngagement(ctx, user.ID, "support@supp.example.com", "partner@supp.vc")
	if err != nil {
		t.Fatalf("support get: %v", err)
	}
	if support.Suppressed {
		t.Error("unsubscribing from raise@ also suppressed support@ — consent is per-relationship")
	}
}

// TestListEngagementsAnswersTheOutreachQuery is the query the whole feature
// exists for: "who has not replied, is due, and is still mailable?" in one
// round trip. Each excluded row is excluded for a different reason, so a
// dropped filter shows up as a specific extra rather than a count mismatch.
func TestListEngagementsAnswersTheOutreachQuery(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "engquery")
	const agent = "raise@e.com"

	past := time.Now().Add(-24 * time.Hour).UTC()
	future := time.Now().Add(24 * time.Hour).UTC()

	// Due and unreplied — the one row that should come back.
	pastP := &past
	stage := "touch1"
	if _, _, err := store.UpsertEngagement(ctx, user.ID, agent, "due@q.vc", &stage, &pastP, nil); err != nil {
		t.Fatalf("due: %v", err)
	}
	// Not due yet.
	futureP := &future
	if _, _, err := store.UpsertEngagement(ctx, user.ID, agent, "notdue@q.vc", &stage, &futureP, nil); err != nil {
		t.Fatalf("notdue: %v", err)
	}
	// Never scheduled.
	enroll(t, store, user.ID, agent, "unscheduled@q.vc", "touch1")

	got, err := store.ListEngagements(ctx, user.ID, agent, identity.EngagementFilter{
		NextActionBefore: time.Now().UTC(),
		Replied:          boolPtr(false),
	}, 50, time.Time{}, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var addrs []string
	for _, e := range got {
		addrs = append(addrs, e.Address)
	}
	if len(addrs) != 1 || addrs[0] != "due@q.vc" {
		t.Errorf("due+unreplied query returned %v, want [due@q.vc]", addrs)
	}

	// Stage narrows further.
	other := "touch9"
	if _, _, err := store.UpsertEngagement(ctx, user.ID, agent, "due@q.vc", &other, nil, nil); err != nil {
		t.Fatalf("restage: %v", err)
	}
	got, err = store.ListEngagements(ctx, user.ID, agent,
		identity.EngagementFilter{Stage: "touch1"}, 50, time.Time{}, "")
	if err != nil {
		t.Fatalf("stage list: %v", err)
	}
	for _, e := range got {
		if e.Address == "due@q.vc" {
			t.Error("stage filter returned a row whose stage changed — filter not applied")
		}
	}
}

// TestListEngagementsIsScopedToOneAgent pins that an agent's outreach listing
// cannot see a sibling agent's engagements.
func TestListEngagementsIsScopedToOneAgent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "engscope")

	enroll(t, store, user.ID, "raise@e.com", "a@scope.vc", "touch1")
	enroll(t, store, user.ID, "support@e.com", "b@scope.vc", "new")

	got, err := store.ListEngagements(ctx, user.ID, "raise@e.com",
		identity.EngagementFilter{}, 50, time.Time{}, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Address != "a@scope.vc" {
		var addrs []string
		for _, e := range got {
			addrs = append(addrs, e.Address)
		}
		t.Errorf("raise@ sees %v, want only [a@scope.vc]", addrs)
	}
}

// TestDeleteEngagementLeavesContactAndConsent pins design §8.6 invariant 5 on
// the engagement path: un-enrolling is not consent. Removing outreach state must
// not delete the person, and must never make a blocked address mailable again.
func TestDeleteEngagementLeavesContactAndConsent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "engdel")

	enroll(t, store, user.ID, "raise@e.com", "partner@del.vc", "touch1")
	if _, err := store.AddSuppression(ctx, user.ID, "partner@del.vc", "bounced", "manual", ""); err != nil {
		t.Fatalf("suppress: %v", err)
	}

	removed, err := store.DeleteEngagement(ctx, user.ID, "raise@e.com", "partner@del.vc")
	if err != nil || !removed {
		t.Fatalf("delete removed=%v err=%v", removed, err)
	}
	if _, err := store.GetEngagement(ctx, user.ID, "raise@e.com", "partner@del.vc"); !errors.Is(err, identity.ErrEngagementNotFound) {
		t.Errorf("engagement still present: %v", err)
	}
	// The person survives...
	if _, err := store.GetContactByAddress(ctx, user.ID, "partner@del.vc"); err != nil {
		t.Errorf("un-enrolling deleted the contact: %v", err)
	}
	// ...and so does the block.
	blocked, err := store.EffectiveSuppressions(ctx, user.ID, "raise@e.com", []string{"partner@del.vc"})
	if err != nil || len(blocked) != 1 {
		t.Errorf("suppression lookup = %v err=%v — un-enrolling must not restore sendability", blocked, err)
	}
}

// TestRecordActivityNeverCreatesAnEngagement is the rule that keeps the
// outreach list meaningful. An agent sends mail for all sorts of reasons —
// replies, one-off notes, transactional messages — and if every recipient were
// auto-enrolled the due list would fill with people nobody is running a
// campaign against, which is exactly the noise this feature exists to remove.
func TestRecordActivityNeverCreatesAnEngagement(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "actcreate")
	now := time.Now().UTC()

	updated, err := store.RecordOutboundActivity(ctx, user.ID, "raise@e.com", "stranger@act.vc", "conv-1", now)
	if err != nil {
		t.Fatalf("outbound: %v", err)
	}
	if updated {
		t.Error("outbound activity created an engagement for an un-enrolled recipient")
	}
	updated, err = store.RecordInboundActivity(ctx, user.ID, "raise@e.com", "stranger@act.vc", "conv-1", now)
	if err != nil {
		t.Fatalf("inbound: %v", err)
	}
	if updated {
		t.Error("inbound activity created an engagement for an unknown sender")
	}
	if _, err := store.GetEngagement(ctx, user.ID, "raise@e.com", "stranger@act.vc"); !errors.Is(err, identity.ErrEngagementNotFound) {
		t.Errorf("an engagement appeared from activity alone: %v", err)
	}
}

// TestRecordOutboundPinsFirstContact pins that first_outbound_at is set once
// and never moves. `replied` is defined against it, so moving it on a later
// send would silently un-reply everyone who had already answered.
func TestRecordOutboundPinsFirstContact(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "actfirst")
	enroll(t, store, user.ID, "raise@e.com", "partner@act.vc", "touch1")

	first := time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Second)
	reply := first.Add(time.Hour)
	second := first.Add(48 * time.Hour)

	if _, err := store.RecordOutboundActivity(ctx, user.ID, "raise@e.com", "partner@act.vc", "conv-1", first); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if _, err := store.RecordInboundActivity(ctx, user.ID, "raise@e.com", "partner@act.vc", "conv-1", reply); err != nil {
		t.Fatalf("reply: %v", err)
	}
	e, err := store.GetEngagement(ctx, user.ID, "raise@e.com", "partner@act.vc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !e.Replied() {
		t.Fatal("a reply after the first send did not register as replied")
	}

	// A later send must not rewrite history and un-reply them.
	if _, err := store.RecordOutboundActivity(ctx, user.ID, "raise@e.com", "partner@act.vc", "conv-1", second); err != nil {
		t.Fatalf("second send: %v", err)
	}
	e, err = store.GetEngagement(ctx, user.ID, "raise@e.com", "partner@act.vc")
	if err != nil {
		t.Fatalf("get after second send: %v", err)
	}
	if e.FirstOutboundAt == nil || !e.FirstOutboundAt.Equal(first) {
		t.Errorf("first_outbound_at = %v, want %v — it must be pinned to the first contact",
			e.FirstOutboundAt, first)
	}
	if !e.Replied() {
		t.Error("a later send un-replied a contact who had already answered")
	}
}

// TestRecordActivityIgnoresOutOfOrderTimestamps pins that a late-arriving or
// retried record cannot move a timestamp backwards. Delivery is at-least-once
// and workers retry, so out-of-order arrival is normal rather than exceptional.
func TestRecordActivityIgnoresOutOfOrderTimestamps(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "actorder")
	enroll(t, store, user.ID, "raise@e.com", "partner@order.vc", "touch1")

	recent := time.Now().UTC().Truncate(time.Second)
	stale := recent.Add(-24 * time.Hour)

	if _, err := store.RecordOutboundActivity(ctx, user.ID, "raise@e.com", "partner@order.vc", "", recent); err != nil {
		t.Fatalf("recent: %v", err)
	}
	if _, err := store.RecordOutboundActivity(ctx, user.ID, "raise@e.com", "partner@order.vc", "", stale); err != nil {
		t.Fatalf("stale: %v", err)
	}
	e, err := store.GetEngagement(ctx, user.ID, "raise@e.com", "partner@order.vc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if e.LastOutboundAt == nil || !e.LastOutboundAt.Equal(recent) {
		t.Errorf("last_outbound_at = %v, want %v — a stale record moved it backwards",
			e.LastOutboundAt, recent)
	}
}

// TestRecordActivityIsScopedPerAgent pins that a send from one agent does not
// touch a sibling agent's record of the same person.
func TestRecordActivityIsScopedPerAgent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "actscope")
	enroll(t, store, user.ID, "raise@e.com", "partner@scope.vc", "touch1")
	enroll(t, store, user.ID, "support@e.com", "partner@scope.vc", "new")

	if _, err := store.RecordOutboundActivity(ctx, user.ID, "raise@e.com", "partner@scope.vc", "", time.Now().UTC()); err != nil {
		t.Fatalf("record: %v", err)
	}

	support, err := store.GetEngagement(ctx, user.ID, "support@e.com", "partner@scope.vc")
	if err != nil {
		t.Fatalf("support get: %v", err)
	}
	if support.OutboundCount != 0 || support.LastOutboundAt != nil {
		t.Errorf("raise@'s send updated support@'s record: count=%d last=%v",
			support.OutboundCount, support.LastOutboundAt)
	}
}

// TestPurgeDeletedAgentsRemovesEngagements drives the REAL hard-delete path
// the janitor runs.
//
// A helper that did this in isolation used to exist alongside it, green-tested
// and called by nothing, while the production invariant went unenforced. The
// helper is gone; this is the only test of the behaviour, and it reaches it the
// way production does.
//
// It also pins the asymmetry: outreach state dies with the agent, consent
// survives it.
func TestPurgeDeletedAgentsRemovesEngagements(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "purgepath")

	if _, err := store.ClaimOrCreateDomain(ctx, "purgepath.example.com", user.ID); err != nil {
		t.Fatalf("claim domain: %v", err)
	}
	const agentAddr = "raise@purgepath.example.com"
	if _, err := store.CreateAgent(ctx, agentAddr, "purgepath.example.com", "",
		"https://example.com/webhook", "", user.ID); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	enroll(t, store, user.ID, agentAddr, "partner@purgepath.vc", "touch3")
	if _, err := store.AddSuppression(ctx, user.ID, "partner@purgepath.vc", "unsubscribed", "manual", ""); err != nil {
		t.Fatalf("suppress: %v", err)
	}

	// Trash the agent and age it past retention so the purge sweep claims it.
	if err := store.SoftDeleteAgent(ctx, agentAddr, user.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE agent_identities SET deleted_at = now() - interval '400 days' WHERE id = $1`,
		agentAddr); err != nil {
		t.Fatalf("age the trash: %v", err)
	}

	if _, err := store.PurgeDeletedAgents(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// A recreated agent at the same address must inherit nothing operational.
	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM contact_engagements WHERE agent_id = $1`, agentAddr).Scan(&remaining); err != nil {
		t.Fatalf("count engagements: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d engagement(s) survived the agent purge — a recreated agent would resume a dead campaign", remaining)
	}

	// ...but every prior refusal still stands.
	blocked, err := store.EffectiveSuppressions(ctx, user.ID, agentAddr, []string{"partner@purgepath.vc"})
	if err != nil {
		t.Fatalf("suppression lookup: %v", err)
	}
	if len(blocked) != 1 {
		t.Errorf("suppression lookup = %v — consent must survive agent deletion", blocked)
	}
}

// TestClaimDueEngagementsFiresOncePerSchedule pins the dedupe contract: a due
// engagement wakes the agent exactly once for a given next_action_at, and
// re-arms when a new one is written. Without this the sweep would re-fire every
// few minutes forever.
func TestClaimDueEngagementsFiresOncePerSchedule(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, agent := dueFixture(t, store, "duefire")
	armDue(t, store, user.ID, agent, "partner@duefire.vc")

	first, err := store.ClaimDueEngagements(ctx, time.Now().UTC(), 50)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim = %d rows err=%v, want 1", len(first), err)
	}
	if first[0].Address != "partner@duefire.vc" || first[0].Stage != "touch1" {
		t.Errorf("claimed payload = %+v", first[0])
	}

	second, err := store.ClaimDueEngagements(ctx, time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second sweep re-fired %d engagement(s) — the wake-up must fire once per schedule", len(second))
	}

	// Writing a new schedule re-arms it.
	later := time.Now().Add(-time.Minute).UTC()
	lp := &later
	if _, _, err := store.UpsertEngagement(ctx, user.ID, agent, "partner@duefire.vc", nil, &lp, nil); err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	third, err := store.ClaimDueEngagements(ctx, time.Now().UTC(), 50)
	if err != nil || len(third) != 1 {
		t.Errorf("re-armed claim = %d rows err=%v, want 1", len(third), err)
	}
}

// TestClaimDueEngagementsSkipsSuppressed is the most important guard in the
// feature. A due-event is an invitation to send; waking an agent to mail
// someone who unsubscribed is the worst thing this could do.
func TestClaimDueEngagementsSkipsSuppressed(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, agent := dueFixture(t, store, "duesupp")
	armDue(t, store, user.ID, agent, "ok@duesupp.vc")
	armDue(t, store, user.ID, agent, "unsubscribed@duesupp.vc")
	armDue(t, store, user.ID, agent, "accountblocked@duesupp.vc")

	if _, _, err := store.AddAgentSuppression(ctx, user.ID, agent, "unsubscribed@duesupp.vc",
		"asked to stop", "unsubscribe", nil); err != nil {
		t.Fatalf("agent suppression: %v", err)
	}
	if _, err := store.AddSuppression(ctx, user.ID, "accountblocked@duesupp.vc", "bounced", "manual", ""); err != nil {
		t.Fatalf("account suppression: %v", err)
	}

	due, err := store.ClaimDueEngagements(ctx, time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	var addrs []string
	for _, d := range due {
		addrs = append(addrs, d.Address)
	}
	if len(addrs) != 1 || addrs[0] != "ok@duesupp.vc" {
		t.Errorf("due = %v, want only [ok@duesupp.vc] — a suppressed contact must never trigger a wake-up", addrs)
	}
}

// TestClaimDueEngagementsSkipsTrashedAgents pins that deleting an agent stops
// its outreach immediately, rather than 30 days later when trash retention
// expires and the rows are finally purged.
func TestClaimDueEngagementsSkipsTrashedAgents(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, agent := dueFixture(t, store, "duetrash")
	armDue(t, store, user.ID, agent, "partner@duetrash.vc")

	if err := store.SoftDeleteAgent(ctx, agent, user.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	due, err := store.ClaimDueEngagements(ctx, time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("a trashed agent emitted %d due-event(s) — deletion must stop outreach at once", len(due))
	}

	// Restoring the agent brings its outreach back.
	if err := store.RestoreAgent(ctx, agent, user.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	due, err = store.ClaimDueEngagements(ctx, time.Now().UTC(), 50)
	if err != nil || len(due) != 1 {
		t.Errorf("restored agent claim = %d err=%v, want 1", len(due), err)
	}
}

// TestClaimDueEngagementsIgnoresFutureSchedules pins the obvious-but-critical
// case: nothing fires early.
func TestClaimDueEngagementsIgnoresFutureSchedules(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, agent := dueFixture(t, store, "duefuture")

	future := time.Now().Add(48 * time.Hour).UTC()
	fp := &future
	stage := "touch1"
	if _, _, err := store.UpsertEngagement(ctx, user.ID, agent, "later@duefuture.vc", &stage, &fp, nil); err != nil {
		t.Fatalf("arm future: %v", err)
	}
	due, err := store.ClaimDueEngagements(ctx, time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("a future schedule fired %d event(s) early", len(due))
	}
}

// dueFixture builds a live agent plus an enrolled, past-due contact.
func dueFixture(t *testing.T, store *identity.Store, tag string) (*identity.User, string) {
	t.Helper()
	ctx := context.Background()
	user := newContactOwner(t, store, tag)
	domain := tag + ".example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("claim domain: %v", err)
	}
	agent := "raise@" + domain
	if _, err := store.CreateAgent(ctx, agent, domain, "", "https://example.com/webhook", "", user.ID); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return user, agent
}

func armDue(t *testing.T, store *identity.Store, userID, agent, address string) {
	t.Helper()
	past := time.Now().Add(-time.Hour).UTC()
	p := &past
	stage := "touch1"
	if _, _, err := store.UpsertEngagement(context.Background(), userID, agent, address, &stage, &p, nil); err != nil {
		t.Fatalf("arm %s: %v", address, err)
	}
}

// TestRecordOutboundConvergesOnEarliestSend pins that first_outbound_at holds
// the EARLIEST send rather than whichever event happened to land first.
//
// Under at-least-once delivery a retried or re-driven job can settle out of
// order. With set-once semantics that pins first_outbound_at to the LATER
// time, and because replied is last_inbound_at > first_outbound_at, a genuine
// reply then reads as no reply — leaving a contact who already answered in the
// follow-up queue to be chased again.
func TestRecordOutboundConvergesOnEarliestSend(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "earliest")
	enroll(t, store, user.ID, "raise@e.com", "partner@earliest.vc", "touch1")

	early := time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Second)
	late := early.Add(48 * time.Hour)
	reply := early.Add(time.Hour) // answered the FIRST send

	// The later send is recorded first — the out-of-order case.
	if _, err := store.RecordOutboundActivity(ctx, user.ID, "raise@e.com", "partner@earliest.vc", "", late); err != nil {
		t.Fatalf("late send: %v", err)
	}
	if _, err := store.RecordOutboundActivity(ctx, user.ID, "raise@e.com", "partner@earliest.vc", "", early); err != nil {
		t.Fatalf("early send: %v", err)
	}
	if _, err := store.RecordInboundActivity(ctx, user.ID, "raise@e.com", "partner@earliest.vc", "", reply); err != nil {
		t.Fatalf("reply: %v", err)
	}

	e, err := store.GetEngagement(ctx, user.ID, "raise@e.com", "partner@earliest.vc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if e.FirstOutboundAt == nil || !e.FirstOutboundAt.Equal(early) {
		t.Errorf("first_outbound_at = %v, want %v — it must converge on the earliest "+
			"send regardless of the order events arrive", e.FirstOutboundAt, early)
	}
	if !e.Replied() {
		t.Error("a contact who answered the first send reads as unreplied — they would " +
			"be chased again after already replying")
	}
	// last_outbound_at still tracks the most recent send.
	if e.LastOutboundAt == nil || !e.LastOutboundAt.Equal(late) {
		t.Errorf("last_outbound_at = %v, want %v", e.LastOutboundAt, late)
	}
}
