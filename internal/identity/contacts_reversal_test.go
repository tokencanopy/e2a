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
