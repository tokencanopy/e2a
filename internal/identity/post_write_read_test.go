package identity_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// This file holds the regressions for one bug class: a write path that reads
// its own result back OUTSIDE the write's transaction. PR #775 fixed the
// severe form (a data-modifying CTE whose outer SELECT ran on the pre-update
// statement snapshot, so an accepted conditional update returned the OLD row
// and a permanently dead ETag). These are its weaker cousins — "write, commit,
// then re-read from the pool" — where the gap between the commit and the
// re-read is a real window another transaction can commit into. The result is
// a response that describes somebody else's write, or a spurious 404/412/500
// on a write that actually succeeded.
//
// Every test uses the same deterministic interleaving: a holder transaction
// takes the row (or advisory) lock the write path needs, the write path is
// started and confirmed blocked on it, a RACER statement is then started and
// confirmed blocked BEHIND it (Postgres lock waits are FIFO), and the holder
// commits. The write path therefore runs first and the racer runs the instant
// the write path releases the lock — i.e. precisely in the window the buggy
// shape leaves open. Each racer is one statement carrying no bind parameters,
// so it goes out over the simple protocol as a single implicit transaction:
// once woken it completes and commits entirely server-side, while the buggy
// re-read still owes a client round-trip and loses.
//
// Each test also asserts the racer really did land afterwards, so a run where
// the interleaving failed to happen cannot pass silently.

// lockWaiters counts backends currently blocked on a lock in the test database.
func lockWaiters(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)
		   FROM pg_locks l
		   JOIN pg_stat_activity a ON a.pid = l.pid
		  WHERE NOT l.granted
		    AND a.datname = current_database()`).Scan(&n); err != nil {
		t.Fatalf("poll locks: %v", err)
	}
	return n
}

// waitForExtraLockWaiter waits until one MORE backend is blocked than at
// baseline. Counting relative to a baseline rather than absolutely keeps the
// interleaving honest if anything else is using the same database.
func waitForExtraLockWaiter(t *testing.T, pool *pgxpool.Pool, baseline int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if lockWaiters(t, pool) > baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the write path never blocked on the holder's lock")
}

// waitForBlockedRacer waits until the racer carrying this marker is itself
// blocked on a lock. Identifying the racer by its own query text (rather than
// by a global waiter count) is what guarantees it is queued BEHIND the write
// path when the holder lets go — the whole point of the interleaving.
func waitForBlockedRacer(t *testing.T, pool *pgxpool.Pool, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*)
			   FROM pg_stat_activity a
			   JOIN pg_locks l ON l.pid = a.pid AND NOT l.granted
			  WHERE a.datname = current_database()
			    AND a.query LIKE '%' || $1 || '%'`, marker).Scan(&n); err != nil {
			t.Fatalf("poll racer lock: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the racer never queued behind the write path")
}

// sqlLit quotes a Go string as a SQL literal. The racers carry no bind
// parameters on purpose: pgx then sends them over the simple protocol as a
// single implicit transaction, so once the lock frees they run AND commit
// entirely server-side, with no client round-trip to lose to the buggy
// re-read. (Test-controlled values only.)
func sqlLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// doBlock wraps body in an anonymous code block, for the one racer that has to
// take a lock before its write. Like a bare statement it goes out with no bind
// parameters, so it is one simple-protocol statement in one implicit
// transaction that PostgreSQL runs and commits before answering the client.
func doBlock(body string) string { return "DO $racer$ BEGIN\n" + body + "\nEND $racer$;" }

// startRacer runs sql on its own pooled connection, tagged with a unique
// marker comment so waitForBlockedRacer can find that exact backend. Returns
// the marker and a channel yielding the racer's error.
func startRacer(t *testing.T, pool *pgxpool.Pool, sql string) (string, <-chan error) {
	t.Helper()
	marker := "racer-" + identity.NewMessageID()
	done := make(chan error, 1)
	go func() {
		_, err := pool.Exec(context.Background(), "/* "+marker+" */ "+sql)
		done <- err
	}()
	return marker, done
}

// TestUpsertEngagementReturnsTheRowItCommitted is the site-1 regression.
//
// UpsertEngagement committed its transaction and then called GetEngagement on
// the POOL. A DeleteEngagement, a contact delete or an import reversal
// committing in that gap turned a write that had already succeeded into
// ErrEngagementNotFound — which the HTTP layer reports as 412
// precondition_failed on a PUT that sent no If-Match at all. Reading the row
// inside the write's own transaction closes the window: the upsert still holds
// the engagement row lock, so no deletion can be observed before the read.
func TestUpsertEngagementReturnsTheRowItCommitted(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newEngagementOwner(t, store, "engtorn")
	const agent, address = "raise@e.com", "partner@engtorn.vc"
	enroll(t, store, user.ID, agent, address, "prospect")

	// Hold the contact-capacity advisory lock the upsert takes before any of
	// its writes — the one deterministic pause point inside that transaction.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"contact-cap:"+user.ID); err != nil {
		t.Fatalf("hold contact-cap lock: %v", err)
	}

	type upsertResult struct {
		e   identity.ContactEngagement
		err error
	}
	baseline := lockWaiters(t, pool)
	upserted := make(chan upsertResult, 1)
	go func() {
		stage := "nurture"
		e, _, err := store.UpsertEngagement(ctx, user.ID, agent, address, &stage, nil, nil)
		upserted <- upsertResult{e, err}
	}()
	waitForExtraLockWaiter(t, pool, baseline)

	// Queue the un-enrolment behind the upsert. It takes the same advisory
	// lock, so it cannot start until the upsert's transaction commits — which
	// is exactly the moment the old post-commit re-read was reaching for the
	// row.
	marker, racer := startRacer(t, pool, doBlock(
		`PERFORM pg_advisory_xact_lock(hashtextextended(`+sqlLit("contact-cap:"+user.ID)+`, 0));`+
			"\n  DELETE FROM contact_engagements WHERE user_id = "+sqlLit(user.ID)+
			" AND agent_id = "+sqlLit(agent)+" AND address = "+sqlLit(address)+";"))
	waitForBlockedRacer(t, pool, marker)

	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release contact-cap lock: %v", err)
	}
	got := <-upserted
	if err := <-racer; err != nil {
		t.Fatalf("racing un-enrolment: %v", err)
	}

	if got.err != nil {
		t.Fatalf("UpsertEngagement returned %v on a write that committed — a "+
			"concurrent un-enrolment must not turn a committed enrolment into "+
			"an error (the handler reports ErrEngagementNotFound here as 412 "+
			"precondition_failed on a request that sent no If-Match)", got.err)
	}
	if got.e.Stage != "nurture" || got.e.Address != address {
		t.Errorf("returned engagement = %+v, want the row this call wrote (stage nurture, %s)", got.e, address)
	}

	// Prove the interleaving actually happened: the racer's delete must be the
	// last write, otherwise this test asserted nothing.
	if _, err := store.GetEngagement(ctx, user.ID, agent, address); err == nil {
		t.Fatal("racing un-enrolment never landed — the interleaving did not happen, so this run proves nothing")
	}
}

// TestUpdateWebhookReturnsTheRowItWrote is the site-4 regression.
//
// UpdateWebhook ran `UPDATE … RETURNING id` in autocommit and then re-read the
// row with a separate GetWebhookByID. A concurrent PATCH committing between
// the two handed the caller the OTHER writer's configuration as the result of
// their own request; a concurrent DELETE turned a committed update into a 404.
// RETURNING the full row from the UPDATE itself is atomic by construction.
func TestUpdateWebhookReturnsTheRowItWrote(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID := webhookTestUser(t, store, "wh-torn")

	w, err := store.CreateWebhook(ctx, userID, "https://example.com/hook", "original",
		[]string{"email.received"}, identity.WebhookFilters{})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT id FROM webhooks WHERE id = $1 FOR UPDATE`, w.ID); err != nil {
		t.Fatalf("hold webhook row: %v", err)
	}

	type updateResult struct {
		w   *identity.Webhook
		err error
	}
	baseline := lockWaiters(t, pool)
	updated := make(chan updateResult, 1)
	go func() {
		mine := "mine"
		got, err := store.UpdateWebhook(ctx, w.ID, userID, identity.WebhookUpdate{Description: &mine})
		updated <- updateResult{got, err}
	}()
	waitForExtraLockWaiter(t, pool, baseline)

	marker, racer := startRacer(t, pool,
		"UPDATE webhooks SET description = 'racer-won' WHERE id = "+sqlLit(w.ID))
	waitForBlockedRacer(t, pool, marker)

	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release webhook row: %v", err)
	}
	got := <-updated
	if err := <-racer; err != nil {
		t.Fatalf("racing webhook PATCH: %v", err)
	}

	if got.err != nil {
		t.Fatalf("UpdateWebhook: %v", got.err)
	}
	if got.w.Description != "mine" {
		t.Errorf("UpdateWebhook returned description %q, want %q — the response described "+
			"a concurrent writer's update instead of this caller's", got.w.Description, "mine")
	}

	after, err := store.GetWebhookByID(ctx, w.ID, userID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if after.Description != "racer-won" {
		t.Fatal("racing PATCH never landed — the interleaving did not happen, so this run proves nothing")
	}
}

// TestUpdateTemplateReturnsTheRowItWrote is the site-5 regression — the same
// shape as UpdateWebhook, and fixed the same way.
func TestUpdateTemplateReturnsTheRowItWrote(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID := templateTestUser(t, store, "tmpl-torn")

	tpl, err := store.CreateTemplate(ctx, userID, identity.TemplateCreate{
		Name: "original", Subject: "Hi", Body: "Hello",
	})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT id FROM templates WHERE id = $1 FOR UPDATE`, tpl.ID); err != nil {
		t.Fatalf("hold template row: %v", err)
	}

	type updateResult struct {
		t   *identity.Template
		err error
	}
	baseline := lockWaiters(t, pool)
	updated := make(chan updateResult, 1)
	go func() {
		mine := "mine"
		got, err := store.UpdateTemplate(ctx, tpl.ID, userID, identity.TemplateUpdate{Name: &mine})
		updated <- updateResult{got, err}
	}()
	waitForExtraLockWaiter(t, pool, baseline)

	marker, racer := startRacer(t, pool,
		"UPDATE templates SET name = 'racer-won' WHERE id = "+sqlLit(tpl.ID))
	waitForBlockedRacer(t, pool, marker)

	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release template row: %v", err)
	}
	got := <-updated
	if err := <-racer; err != nil {
		t.Fatalf("racing template PATCH: %v", err)
	}

	if got.err != nil {
		t.Fatalf("UpdateTemplate: %v", got.err)
	}
	if got.t.Name != "mine" {
		t.Errorf("UpdateTemplate returned name %q, want %q — the response described "+
			"a concurrent writer's update instead of this caller's", got.t.Name, "mine")
	}

	after, err := store.GetTemplateByID(ctx, tpl.ID, userID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if after.Name != "racer-won" {
		t.Fatal("racing PATCH never landed — the interleaving did not happen, so this run proves nothing")
	}
}

// TestUpdateAgentNameReturnsTheRowItWrote is the site-3 (PATCH) regression.
// The handler used to re-read with GetAgent after the write committed: a
// concurrent rename showed the caller the other writer's name as the result of
// their own PATCH, and a concurrent trash answered 500 "failed to reload
// agent" on a rename that had committed.
func TestUpdateAgentNameReturnsTheRowItWrote(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := trashTestSetup(t, store, "agentname-torn")

	holder := lockAgentRow(t, pool, agentID)
	type nameResult struct {
		a   *identity.AgentIdentity
		err error
	}
	baseline := lockWaiters(t, pool)
	renamed := make(chan nameResult, 1)
	go func() {
		a, err := store.UpdateAgentName(ctx, agentID, userID, "mine")
		renamed <- nameResult{a, err}
	}()
	waitForExtraLockWaiter(t, pool, baseline)

	marker, racer := startRacer(t, pool,
		"UPDATE agent_identities SET name = 'racer-won' WHERE id = "+sqlLit(agentID))
	waitForBlockedRacer(t, pool, marker)

	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release agent row: %v", err)
	}
	got := <-renamed
	if err := <-racer; err != nil {
		t.Fatalf("racing rename: %v", err)
	}

	if got.err != nil {
		t.Fatalf("UpdateAgentName: %v", got.err)
	}
	if got.a == nil || got.a.Name != "mine" {
		t.Errorf("UpdateAgentName returned %+v, want the name this call wrote", got.a)
	}
	assertAgentName(t, pool, agentID, "racer-won")
}

// TestUpdateAgentProtectionReturnsTheRowItWrote is the site-2 regression. The
// protection PUT is a full replace, which makes the torn read especially bad:
// the caller was handed a concurrent writer's entire posture as the
// authoritative-looking result of their own request.
func TestUpdateAgentProtectionReturnsTheRowItWrote(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := trashTestSetup(t, store, "protection-torn")

	cfg := identity.ProtectionConfig{
		InboundGatePolicy: "allowlist", InboundAllowlist: []string{"partner@example.com"},
		InboundGateAction: "review", InboundScanSensitivity: identity.SensitivityOff,
		OutboundGatePolicy: "open", OutboundGateAction: "flag",
		OutboundScanSensitivity: identity.SensitivityOff,
		HITLTTLSeconds:          3600, HITLExpirationAction: identity.HITLExpirationReject,
	}

	holder := lockAgentRow(t, pool, agentID)
	type protResult struct {
		a   *identity.AgentIdentity
		err error
	}
	baseline := lockWaiters(t, pool)
	written := make(chan protResult, 1)
	go func() {
		a, err := store.UpdateAgentProtection(ctx, agentID, userID, cfg)
		written <- protResult{a, err}
	}()
	waitForExtraLockWaiter(t, pool, baseline)

	// A concurrent full-replace PUT landing right behind this one.
	marker, racer := startRacer(t, pool,
		"UPDATE agent_identities SET inbound_policy = 'open', inbound_policy_action = 'block' WHERE id = "+sqlLit(agentID))
	waitForBlockedRacer(t, pool, marker)

	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release agent row: %v", err)
	}
	got := <-written
	if err := <-racer; err != nil {
		t.Fatalf("racing protection PUT: %v", err)
	}

	if got.err != nil {
		t.Fatalf("UpdateAgentProtection: %v", got.err)
	}
	if got.a == nil || got.a.InboundPolicy != "allowlist" || got.a.InboundPolicyAction != "review" {
		t.Errorf("UpdateAgentProtection returned inbound policy %v/%v, want allowlist/review — "+
			"the response echoed a concurrent writer's posture as this caller's result",
			got.a.InboundPolicy, got.a.InboundPolicyAction)
	}

	var policy, action string
	if err := pool.QueryRow(ctx,
		`SELECT inbound_policy, inbound_policy_action FROM agent_identities WHERE id = $1`,
		agentID).Scan(&policy, &action); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if policy != "open" || action != "block" {
		t.Fatal("racing PUT never landed — the interleaving did not happen, so this run proves nothing")
	}
}

// TestRestoreAgentReturnsTheRestoredAgent is the site-3 (restore) regression.
// A re-trash committing between the restore and the handler's re-read answered
// 500 "failed to reload agent" on a restore that had committed.
func TestRestoreAgentReturnsTheRestoredAgent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	userID, agentID := trashTestSetup(t, store, "agentrestore-torn")
	if err := store.SoftDeleteAgent(ctx, agentID, userID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	holder := lockAgentRow(t, pool, agentID)
	type restoreResult struct {
		a   *identity.AgentIdentity
		err error
	}
	baseline := lockWaiters(t, pool)
	restored := make(chan restoreResult, 1)
	go func() {
		a, err := store.RestoreAgent(ctx, agentID, userID)
		restored <- restoreResult{a, err}
	}()
	waitForExtraLockWaiter(t, pool, baseline)

	marker, racer := startRacer(t, pool,
		"UPDATE agent_identities SET deleted_at = now() WHERE id = "+sqlLit(agentID))
	waitForBlockedRacer(t, pool, marker)

	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release agent row: %v", err)
	}
	got := <-restored
	if err := <-racer; err != nil {
		t.Fatalf("racing re-trash: %v", err)
	}

	if got.err != nil {
		t.Fatalf("RestoreAgent returned %v on a restore that committed", got.err)
	}
	if got.a == nil || got.a.DeletedAt != nil {
		t.Errorf("RestoreAgent returned %+v, want the live agent this call restored", got.a)
	}

	var deletedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at FROM agent_identities WHERE id = $1`, agentID).Scan(&deletedAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if deletedAt == nil {
		t.Fatal("racing re-trash never landed — the interleaving did not happen, so this run proves nothing")
	}
}

// TestRestoreMessageReturnsTheRestoredMessage is the site-6 regression. A
// re-trash or purge committing between the restore and the handler's
// GetMessage answered 500 "failed to reload message" on a committed restore.
func TestRestoreMessageReturnsTheRestoredMessage(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	_, agentID := trashTestSetup(t, store, "msgrestore-torn")
	msg := trashInbound(t, store, agentID, agentID, "restore me")
	if err := store.SoftDeleteMessage(ctx, msg.ID, agentID); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT id FROM messages WHERE id = $1 FOR UPDATE`, msg.ID); err != nil {
		t.Fatalf("hold message row: %v", err)
	}

	type restoreResult struct {
		m   *identity.Message
		err error
	}
	baseline := lockWaiters(t, pool)
	restored := make(chan restoreResult, 1)
	go func() {
		m, err := store.RestoreMessage(ctx, msg.ID, agentID)
		restored <- restoreResult{m, err}
	}()
	waitForExtraLockWaiter(t, pool, baseline)

	marker, racer := startRacer(t, pool,
		"UPDATE messages SET deleted_at = now() WHERE id = "+sqlLit(msg.ID))
	waitForBlockedRacer(t, pool, marker)

	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release message row: %v", err)
	}
	got := <-restored
	if err := <-racer; err != nil {
		t.Fatalf("racing re-trash: %v", err)
	}

	if got.err != nil {
		t.Fatalf("RestoreMessage returned %v on a restore that committed", got.err)
	}
	if got.m == nil || got.m.DeletedAt != nil || got.m.ID != msg.ID {
		t.Errorf("RestoreMessage returned %+v, want the live message this call restored", got.m)
	}

	var deletedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at FROM messages WHERE id = $1`, msg.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if deletedAt == nil {
		t.Fatal("racing re-trash never landed — the interleaving did not happen, so this run proves nothing")
	}
}

func lockAgentRow(t *testing.T, pool *pgxpool.Pool, agentID string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, `SELECT id FROM agent_identities WHERE id = $1 FOR UPDATE`, agentID); err != nil {
		t.Fatalf("hold agent row: %v", err)
	}
	return holder
}

func assertAgentName(t *testing.T, pool *pgxpool.Pool, agentID, want string) {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(),
		`SELECT name FROM agent_identities WHERE id = $1`, agentID).Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != want {
		t.Fatalf("racing write never landed (name = %q, want %q) — "+
			"the interleaving did not happen, so this run proves nothing", name, want)
	}
}
