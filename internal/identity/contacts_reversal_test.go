package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// The tests in this file pin the reversal safety contract of
// Store.DeleteImportBatch: undoing an import removes ONLY what is verifiably
// untouched and attributable solely to that batch. Anything the account has
// since edited, enrolled elsewhere, or corresponded with survives — an undo
// addressed only by batch id must never destroy state the caller cannot see.

// newReversalRig builds an account with one live agent, the shape every
// reversal test needs. The pool is returned as well so a test can stage stale
// provenance directly; it must come from the SAME TestDB handle, because each
// TestDB call truncates.
func newReversalRig(t *testing.T, tag string) (*pgxpool.Pool, *identity.Store, *identity.User, string) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, tag)
	domain := tag + ".example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("claim domain: %v", err)
	}
	agent := "raise@" + domain
	if _, err := store.CreateAgent(ctx, agent, domain, "", "", "", user.ID); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return pool, store, user, agent
}

// TestDeleteImportBatchRetainsPatchedContact: a contact edited through PATCH
// after the import no longer belongs solely to the batch, so reversal must
// keep it while still removing the untouched row from the same upload.
func TestDeleteImportBatchRetainsPatchedContact(t *testing.T) {
	_, store, user, _ := newReversalRig(t, "patched")
	ctx := context.Background()

	if _, err := store.ImportContacts(ctx, user.ID, "imp_patched", []identity.ContactImportRow{
		{Address: "edited@patched.vc"}, {Address: "untouched@patched.vc"},
	}, true); err != nil {
		t.Fatalf("import: %v", err)
	}

	name := "A. Partner"
	if _, err := store.UpdateContact(ctx, user.ID, "edited@patched.vc", &name, nil); err != nil {
		t.Fatalf("patch: %v", err)
	}

	deleted, retained, _, err := store.DeleteImportBatch(ctx, user.ID, "imp_patched")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if deleted != 1 || retained != 1 {
		t.Errorf("receipt deleted=%d retained=%d, want 1 and 1 — "+
			"an edited contact must survive its batch's reversal", deleted, retained)
	}
	if _, err := store.GetContactByAddress(ctx, user.ID, "edited@patched.vc"); err != nil {
		t.Errorf("edited contact deleted by reversal: %v", err)
	}
	if _, err := store.GetContactByAddress(ctx, user.ID, "untouched@patched.vc"); !errors.Is(err, identity.ErrContactNotFound) {
		t.Errorf("untouched contact survived reversal: %v", err)
	}
}

// TestDeleteImportBatchRetainsEditedEngagement: an engagement the agent moved
// forward after the import (stage change, new next action) is live outreach
// state, not an import artifact. Reversal must leave it — and its contact —
// alone, while still removing the batch's untouched enrolments.
func TestDeleteImportBatchRetainsEditedEngagement(t *testing.T) {
	_, store, user, agent := newReversalRig(t, "engedit")
	ctx := context.Background()

	if _, err := store.ImportContactsWithOptions(ctx, user.ID, "imp_engedit",
		[]identity.ContactImportRow{
			{Address: "advanced@engedit.vc"}, {Address: "idle@engedit.vc"},
		}, identity.ContactImportOptions{Merge: true, AgentID: agent, Stage: "new"}); err != nil {
		t.Fatalf("import: %v", err)
	}

	advanced := "contacted"
	if _, _, err := store.UpsertEngagement(ctx, user.ID, agent, "advanced@engedit.vc",
		&advanced, nil, nil); err != nil {
		t.Fatalf("advance stage: %v", err)
	}

	deleted, retained, engagementsDeleted, err := store.DeleteImportBatch(ctx, user.ID, "imp_engedit")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if engagementsDeleted != 1 {
		t.Errorf("engagements_deleted=%d, want 1 — an edited enrolment was removed", engagementsDeleted)
	}
	if deleted != 1 || retained != 1 {
		t.Errorf("receipt deleted=%d retained=%d, want 1 and 1", deleted, retained)
	}
	engagement, err := store.GetEngagement(ctx, user.ID, agent, "advanced@engedit.vc")
	if err != nil {
		t.Fatalf("edited engagement deleted by reversal: %v", err)
	}
	if engagement.Stage != advanced {
		t.Errorf("stage = %q, want %q — reversal clobbered live outreach state", engagement.Stage, advanced)
	}
	if _, err := store.GetEngagement(ctx, user.ID, agent, "idle@engedit.vc"); !errors.Is(err, identity.ErrEngagementNotFound) {
		t.Errorf("untouched enrolment survived reversal: %v", err)
	}
}

// TestDeleteImportBatchKeepsIndependentEngagement is the cascade-hazard
// regression: the import created the contact but NOT this engagement.
// Reversing the batch must not let the contacts→engagements ON DELETE CASCADE
// take the independent engagement with it; the contact is retained because it
// still has a live relationship.
func TestDeleteImportBatchKeepsIndependentEngagement(t *testing.T) {
	_, store, user, agent := newReversalRig(t, "indepen")
	ctx := context.Background()

	if _, err := store.ImportContacts(ctx, user.ID, "imp_independent", []identity.ContactImportRow{
		{Address: "partner@indepen.vc"},
	}, true); err != nil {
		t.Fatalf("import: %v", err)
	}
	stage := "discovery"
	if _, created, err := store.UpsertEngagement(ctx, user.ID, agent, "partner@indepen.vc",
		&stage, nil, nil); err != nil || !created {
		t.Fatalf("independent enroll created=%v err=%v", created, err)
	}

	deleted, retained, engagementsDeleted, err := store.DeleteImportBatch(ctx, user.ID, "imp_independent")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if deleted != 0 || retained != 1 || engagementsDeleted != 0 {
		t.Errorf("receipt deleted=%d retained=%d engagements=%d, want 0/1/0 — "+
			"a contact with a surviving engagement must never be reversed",
			deleted, retained, engagementsDeleted)
	}
	engagement, err := store.GetEngagement(ctx, user.ID, agent, "partner@indepen.vc")
	if err != nil {
		t.Fatalf("independent engagement destroyed by reversal (cascade): %v", err)
	}
	if engagement.Stage != stage {
		t.Errorf("stage = %q, want %q", engagement.Stage, stage)
	}
	if _, err := store.GetContactByAddress(ctx, user.ID, "partner@indepen.vc"); err != nil {
		t.Errorf("contact with live outreach deleted by reversal: %v", err)
	}
}

// TestDeleteImportBatchRetainsEngagementWithActivity: wire activity recorded
// against a batch-created engagement means the relationship is real. Reversal
// must keep the engagement and its contact even though the batch created both.
func TestDeleteImportBatchRetainsEngagementWithActivity(t *testing.T) {
	_, store, user, agent := newReversalRig(t, "engwire")
	ctx := context.Background()

	if _, err := store.ImportContactsWithOptions(ctx, user.ID, "imp_engwire",
		[]identity.ContactImportRow{{Address: "mailed@engwire.vc"}},
		identity.ContactImportOptions{Merge: true, AgentID: agent, Stage: "new"}); err != nil {
		t.Fatalf("import: %v", err)
	}
	if ok, err := store.RecordOutboundActivity(ctx, user.ID, agent, "mailed@engwire.vc",
		"conv-wire", time.Now()); err != nil || !ok {
		t.Fatalf("record activity ok=%v err=%v", ok, err)
	}

	deleted, retained, engagementsDeleted, err := store.DeleteImportBatch(ctx, user.ID, "imp_engwire")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if deleted != 0 || retained != 1 || engagementsDeleted != 0 {
		t.Errorf("receipt deleted=%d retained=%d engagements=%d, want 0/1/0 — "+
			"an engagement with real outreach was reversed", deleted, retained, engagementsDeleted)
	}
	engagement, err := store.GetEngagement(ctx, user.ID, agent, "mailed@engwire.vc")
	if err != nil {
		t.Fatalf("engagement with wire activity deleted by reversal: %v", err)
	}
	if engagement.FirstOutboundAt == nil {
		t.Errorf("first_outbound_at lost — reversal destroyed derived activity")
	}
}

// TestDeleteImportBatchReceiptCounts pins the exact receipt arithmetic on a
// mixed batch: one untouched row reversed, one edited row retained, one row
// retained for correspondence history — so a caller can reconcile
// created == deleted + retained.
func TestDeleteImportBatchReceiptCounts(t *testing.T) {
	_, store, user, agent := newReversalRig(t, "receipt")
	ctx := context.Background()

	if _, err := store.ImportContactsWithOptions(ctx, user.ID, "imp_receipt",
		[]identity.ContactImportRow{
			{Address: "untouched@receipt.vc"},
			{Address: "edited@receipt.vc"},
			{Address: "mailed@receipt.vc"},
		}, identity.ContactImportOptions{Merge: true, AgentID: agent}); err != nil {
		t.Fatalf("import: %v", err)
	}
	name := "Renamed"
	if _, err := store.UpdateContact(ctx, user.ID, "edited@receipt.vc", &name, nil); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if _, err := store.CreateOutboundMessage(ctx, agent, []string{"mailed@receipt.vc"}, nil, nil,
		"Intro", "send", "smtp", "", "conv-receipt", []byte("raw")); err != nil {
		t.Fatalf("send: %v", err)
	}

	deleted, retained, engagementsDeleted, err := store.DeleteImportBatch(ctx, user.ID, "imp_receipt")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if deleted != 1 || retained != 2 {
		t.Errorf("contacts deleted=%d retained=%d, want 1 and 2", deleted, retained)
	}
	// The untouched row's enrolment and the edited row's enrolment are removed:
	// both engagements stayed untouched, and the contact PATCH is not an
	// engagement mutation. The mailed row's engagement survives on its message
	// history even though the test shortcut never recorded derived activity.
	if engagementsDeleted != 2 {
		t.Errorf("engagements_deleted=%d, want 2", engagementsDeleted)
	}
}

// TestDeleteImportBatchStaleProvenanceCannotDelete simulates a row whose
// import_batch_id points at a batch even though the row was mutated after the
// import — stale provenance from any past or future write path. The reversal's
// defensive predicates, not the tag alone, must decide what is removable.
func TestDeleteImportBatchStaleProvenanceCannotDelete(t *testing.T) {
	pool, store, user, _ := newReversalRig(t, "stale")
	ctx := context.Background()

	if _, err := store.ImportContacts(ctx, user.ID, "imp_stale", []identity.ContactImportRow{
		{Address: "partner@stale.vc"},
	}, true); err != nil {
		t.Fatalf("import: %v", err)
	}
	name := "A. Partner"
	if _, err := store.UpdateContact(ctx, user.ID, "partner@stale.vc", &name, nil); err != nil {
		t.Fatalf("patch: %v", err)
	}
	// Re-point the provenance at the batch directly, as a stale writer would.
	if _, err := pool.Exec(ctx,
		`UPDATE contacts SET import_batch_id = 'imp_stale'
		  WHERE user_id = $1 AND address = 'partner@stale.vc'`, user.ID); err != nil {
		t.Fatalf("re-tag: %v", err)
	}

	deleted, retained, _, err := store.DeleteImportBatch(ctx, user.ID, "imp_stale")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if deleted != 0 || retained != 1 {
		t.Errorf("receipt deleted=%d retained=%d, want 0 and 1 — "+
			"stale provenance caused data loss", deleted, retained)
	}
	if _, err := store.GetContactByAddress(ctx, user.ID, "partner@stale.vc"); err != nil {
		t.Errorf("mutated contact deleted on stale provenance: %v", err)
	}
}

// TestDeleteImportBatchRetainsCCOnlyHistory: correspondence where the contact
// appeared only on the Cc/Bcc line is still message history. A reversal that
// looked only at To would destroy a record the account has genuinely mailed.
func TestDeleteImportBatchRetainsCCOnlyHistory(t *testing.T) {
	_, store, user, agent := newReversalRig(t, "cconly")
	ctx := context.Background()

	if _, err := store.ImportContacts(ctx, user.ID, "imp_cc", []identity.ContactImportRow{
		{Address: "cced@cconly.vc"},
	}, true); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := store.CreateOutboundMessage(ctx, agent, []string{"primary@cconly.vc"},
		[]string{"cced@cconly.vc"}, nil, "Intro", "send", "smtp", "", "conv-cc", []byte("raw")); err != nil {
		t.Fatalf("send: %v", err)
	}

	deleted, retained, _, err := store.DeleteImportBatch(ctx, user.ID, "imp_cc")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if deleted != 0 || retained != 1 {
		t.Errorf("receipt deleted=%d retained=%d, want 0 and 1 — cc-only history was ignored", deleted, retained)
	}
	if _, err := store.GetContactByAddress(ctx, user.ID, "cced@cconly.vc"); err != nil {
		t.Errorf("contact with cc-only correspondence deleted: %v", err)
	}
}

// TestDeleteImportBatchRetainsInboundActivity: a reply recorded against a
// batch-created engagement is live state even when the agent never edited the
// row. The reversal must keep both the engagement and the contact.
func TestDeleteImportBatchRetainsInboundActivity(t *testing.T) {
	_, store, user, agent := newReversalRig(t, "inbact")
	ctx := context.Background()

	if _, err := store.ImportContactsWithOptions(ctx, user.ID, "imp_inbact",
		[]identity.ContactImportRow{{Address: "replier@inbact.vc"}},
		identity.ContactImportOptions{Merge: true, AgentID: agent}); err != nil {
		t.Fatalf("import: %v", err)
	}
	if ok, err := store.RecordInboundActivity(ctx, user.ID, agent, "replier@inbact.vc",
		"conv-in", time.Now()); err != nil || !ok {
		t.Fatalf("record inbound ok=%v err=%v", ok, err)
	}

	_, retained, engagementsDeleted, err := store.DeleteImportBatch(ctx, user.ID, "imp_inbact")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if retained != 1 || engagementsDeleted != 0 {
		t.Errorf("receipt retained=%d engagements=%d, want 1 and 0", retained, engagementsDeleted)
	}
	engagement, err := store.GetEngagement(ctx, user.ID, agent, "replier@inbact.vc")
	if err != nil {
		t.Fatalf("engagement with a recorded reply deleted: %v", err)
	}
	if engagement.LastInboundAt == nil {
		t.Errorf("last_inbound_at lost")
	}
}

// TestDeleteImportBatchDefensivePredicatesStandAlone isolates the two
// engagement guards no production path can set without also bumping
// updated_at: a due-notification marker and a conversation pointer. Staged
// directly in SQL with updated_at = created_at preserved, each alone must
// still protect the row — they exist so NO stale state combination can make
// an in-use engagement look like an import artifact.
func TestDeleteImportBatchDefensivePredicatesStandAlone(t *testing.T) {
	pool, store, user, agent := newReversalRig(t, "predicates")
	ctx := context.Background()

	if _, err := store.ImportContactsWithOptions(ctx, user.ID, "imp_pred",
		[]identity.ContactImportRow{
			{Address: "notified@predicates.vc"},
			{Address: "linked@predicates.vc"},
			{Address: "untouched@predicates.vc"},
		}, identity.ContactImportOptions{Merge: true, AgentID: agent}); err != nil {
		t.Fatalf("import: %v", err)
	}
	for address, stmt := range map[string]string{
		"notified@predicates.vc": `UPDATE contact_engagements
		    SET notified_next_action_at = now()
		  WHERE user_id = $1 AND address = $2`,
		"linked@predicates.vc": `UPDATE contact_engagements
		    SET last_conversation_id = 'conv_staged'
		  WHERE user_id = $1 AND address = $2`,
	} {
		if _, err := pool.Exec(ctx, stmt, user.ID, address); err != nil {
			t.Fatalf("stage %s: %v", address, err)
		}
	}

	_, _, engagementsDeleted, err := store.DeleteImportBatch(ctx, user.ID, "imp_pred")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if engagementsDeleted != 1 {
		t.Errorf("engagements_deleted=%d, want 1 — a due-notified or conversation-linked "+
			"engagement was treated as untouched", engagementsDeleted)
	}
	for _, address := range []string{"notified@predicates.vc", "linked@predicates.vc"} {
		if _, err := store.GetEngagement(ctx, user.ID, agent, address); err != nil {
			t.Errorf("%s engagement deleted: %v", address, err)
		}
	}
	if _, err := store.GetEngagement(ctx, user.ID, agent, "untouched@predicates.vc"); !errors.Is(err, identity.ErrEngagementNotFound) {
		t.Errorf("untouched engagement survived: %v", err)
	}
}

// TestDeleteImportBatchWaitsForConcurrentPatch is the regression for the
// statement-snapshot race: a PATCH holding the contact's row lock commits
// WHILE the reversal is in flight. The reversal must wait on the lock and
// then judge the row by its newest version (patched → retained), not by the
// stale "untouched" snapshot its guard CTE started with.
func TestDeleteImportBatchWaitsForConcurrentPatch(t *testing.T) {
	pool, store, user, _ := newReversalRig(t, "racepatch")
	ctx := context.Background()

	if _, err := store.ImportContacts(ctx, user.ID, "imp_racepatch", []identity.ContactImportRow{
		{Address: "partner@racepatch.vc"},
	}, true); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Hold the row lock with an uncommitted mutation.
	patch, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin patch: %v", err)
	}
	if _, err := patch.Exec(ctx,
		`UPDATE contacts SET display_name = 'Raced', updated_at = now()
		  WHERE user_id = $1 AND address = 'partner@racepatch.vc'`, user.ID); err != nil {
		t.Fatalf("patch exec: %v", err)
	}

	type receipt struct {
		deleted, retained int
		err               error
	}
	done := make(chan receipt, 1)
	go func() {
		d, r, _, err := store.DeleteImportBatch(ctx, user.ID, "imp_racepatch")
		done <- receipt{d, r, err}
	}()

	// The reversal cannot finish while the patch holds the row lock; wait
	// until it is visibly blocked so the interleaving is real, not hoped for.
	waitForBlockedLock(t, pool)
	if err := patch.Commit(ctx); err != nil {
		t.Fatalf("patch commit: %v", err)
	}

	got := <-done
	if got.err != nil {
		t.Fatalf("reverse: %v", got.err)
	}
	if got.deleted != 0 || got.retained != 1 {
		t.Errorf("receipt deleted=%d retained=%d, want 0 and 1 — a committed PATCH "+
			"lost to a stale reversal snapshot", got.deleted, got.retained)
	}
	got2, err := store.GetContactByAddress(ctx, user.ID, "partner@racepatch.vc")
	if err != nil {
		t.Fatalf("patched contact deleted by racing reversal: %v", err)
	}
	if got2.DisplayName != "Raced" {
		t.Errorf("display_name = %q, want Raced", got2.DisplayName)
	}
}

// TestDeleteImportBatchWaitsForConcurrentEngagement is the cascade-race
// regression: an independent enrolment whose transaction is in flight while
// the reversal runs must survive. Before the lock-first fix the engagement
// insert's FK check raced the contact delete and the fresh engagement was
// cascade-deleted (or the insert failed with an FK violation).
func TestDeleteImportBatchWaitsForConcurrentEngagement(t *testing.T) {
	pool, store, user, agent := newReversalRig(t, "raceeng")
	ctx := context.Background()

	if _, err := store.ImportContacts(ctx, user.ID, "imp_raceeng", []identity.ContactImportRow{
		{Address: "partner@raceeng.vc"},
	}, true); err != nil {
		t.Fatalf("import: %v", err)
	}
	contact, err := store.GetContactByAddress(ctx, user.ID, "partner@raceeng.vc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// An in-flight independent enrolment: lock the contact row (as
	// UpsertEngagement's conflict-update does) and stage the engagement,
	// uncommitted.
	enroll, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin enroll: %v", err)
	}
	if _, err := enroll.Exec(ctx,
		`UPDATE contacts SET updated_at = contacts.updated_at WHERE id = $1`, contact.ID); err != nil {
		t.Fatalf("enroll contact lock: %v", err)
	}
	if _, err := enroll.Exec(ctx,
		`INSERT INTO contact_engagements
		     (id, user_id, contact_id, agent_id, address, stage, metadata)
		  VALUES ($1, $2, $3, $4, $5, 'discovery', '{}'::jsonb)`,
		identity.NewEngagementID(), user.ID, contact.ID, agent, "partner@raceeng.vc"); err != nil {
		t.Fatalf("enroll insert: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, _, err := store.DeleteImportBatch(ctx, user.ID, "imp_raceeng")
		done <- err
	}()

	waitForBlockedLock(t, pool)
	if err := enroll.Commit(ctx); err != nil {
		t.Fatalf("enroll commit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("reverse: %v", err)
	}

	engagement, err := store.GetEngagement(ctx, user.ID, agent, "partner@raceeng.vc")
	if err != nil {
		t.Fatalf("concurrent independent engagement destroyed: %v", err)
	}
	if engagement.Stage != "discovery" {
		t.Errorf("stage = %q, want discovery", engagement.Stage)
	}
	if _, err := store.GetContactByAddress(ctx, user.ID, "partner@raceeng.vc"); err != nil {
		t.Errorf("contact with a racing enrolment deleted: %v", err)
	}
}

// waitForBlockedLock polls until some backend in THIS test database is
// waiting on a lock — proof the racing reversal has reached its lock point —
// so the test's commit lands mid-reversal rather than before it starts. The
// database filter matters under `make test -p 4`: per-package derived DBs
// share one server, and an unrelated blocked lock in another package would
// satisfy an unscoped wait early, silently un-testing the interleaving.
func waitForBlockedLock(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)
			   FROM pg_locks l
			   JOIN pg_stat_activity a ON a.pid = l.pid
			  WHERE NOT l.granted
			    AND a.datname = current_database()`).Scan(&n); err != nil {
			t.Fatalf("poll locks: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("reversal never blocked on the racing transaction's lock")
}

// TestDeleteImportBatchWaitsForKeyShareEnrolment covers the OTHER enrolment
// interleaving: the skip-mode import path (ON CONFLICT DO NOTHING + an
// unlocked read) never takes a contact row lock, so its engagement insert
// holds only the FK's FOR KEY SHARE on the contact. That still conflicts with
// the reversal's FOR UPDATE, so the reversal must wait and then retain both
// rows — same guarantee as the update-lock variant, through a weaker lock.
func TestDeleteImportBatchWaitsForKeyShareEnrolment(t *testing.T) {
	pool, store, user, agent := newReversalRig(t, "keyshare")
	ctx := context.Background()

	if _, err := store.ImportContacts(ctx, user.ID, "imp_keyshare", []identity.ContactImportRow{
		{Address: "partner@keyshare.vc"},
	}, true); err != nil {
		t.Fatalf("import: %v", err)
	}
	contact, err := store.GetContactByAddress(ctx, user.ID, "partner@keyshare.vc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Stage the engagement WITHOUT touching the contact row — the FK check's
	// KEY SHARE is the only lock this transaction holds on it.
	enroll, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin enroll: %v", err)
	}
	if _, err := enroll.Exec(ctx,
		`INSERT INTO contact_engagements
		     (id, user_id, contact_id, agent_id, address, stage, metadata)
		  VALUES ($1, $2, $3, $4, $5, 'discovery', '{}'::jsonb)`,
		identity.NewEngagementID(), user.ID, contact.ID, agent, "partner@keyshare.vc"); err != nil {
		t.Fatalf("enroll insert: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, _, err := store.DeleteImportBatch(ctx, user.ID, "imp_keyshare")
		done <- err
	}()

	waitForBlockedLock(t, pool)
	if err := enroll.Commit(ctx); err != nil {
		t.Fatalf("enroll commit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("reverse: %v", err)
	}

	if _, err := store.GetEngagement(ctx, user.ID, agent, "partner@keyshare.vc"); err != nil {
		t.Fatalf("key-share-only enrolment destroyed by racing reversal: %v", err)
	}
	if _, err := store.GetContactByAddress(ctx, user.ID, "partner@keyshare.vc"); err != nil {
		t.Errorf("contact with a racing key-share enrolment deleted: %v", err)
	}
}
