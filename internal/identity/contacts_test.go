package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// newContactOwner creates an isolated account for a contact test.
func newContactOwner(t *testing.T, store *identity.Store, tag string) *identity.User {
	t.Helper()
	user, err := store.CreateOrGetUser(context.Background(),
		"owner-"+tag+"@example.com", "Owner "+tag, "google-"+tag)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user
}

// TestContactKeyMatchesSuppressionKey is the parity gate from design §6.2 and
// the single most important test in the contacts feature.
//
// Contacts and suppressions are separate tables joined only by the address
// string. If contact storage canonicalized addresses differently from the
// suppression lookup, a contact would appear sendable in the contact list and
// then be blocked at send time — or, worse, an address the user believes is
// suppressed would not match its contact and would be mailed.
//
// This asserts the two agree on every canonicalization case that matters,
// comparing what CreateContact PERSISTS against what the suppression path uses
// as its lookup key. It is deliberately not a test of NormalizeMailboxAddress
// in isolation: the risk is the two call sites drifting apart, not the
// function being wrong.
func TestContactKeyMatchesSuppressionKey(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "parity")

	// Each case carries its own domain so rows cannot collide on the
	// (user_id, address) unique key. Written out longhand rather than
	// generated: the inputs ARE the specification of what canonicalization
	// must collapse, and rewriting them in the test would obscure that.
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"bare address", "partner@a-fund.vc", "partner@a-fund.vc"},
		{"mixed case", "Partner@B-Fund.VC", "partner@b-fund.vc"},
		{"surrounding whitespace", "  partner@c-fund.vc  ", "partner@c-fund.vc"},
		{"display-name form", "A. Partner <partner@d-fund.vc>", "partner@d-fund.vc"},
		{"quoted display name", `"Partner, A." <partner@e-fund.vc>`, "partner@e-fund.vc"},
		{"display name mixed case", "A. Partner <Partner@F-FUND.vc>", "partner@f-fund.vc"},
		// Plus-tags are deliberately NOT folded: they are distinct mailboxes.
		{"plus tag preserved", "partner+vc@g-fund.vc", "partner+vc@g-fund.vc"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := store.CreateContact(ctx, user.ID, tc.input, "", nil, identity.ContactSourceManual, "")
			if err != nil {
				t.Fatalf("CreateContact(%q): %v", tc.input, err)
			}
			if c.Address != tc.want {
				t.Errorf("contact stored address = %q, want %q", c.Address, tc.want)
			}

			// The suppression path's canonicalization of the SAME input must
			// produce the SAME key, or a contact and its suppression can never
			// be matched to each other.
			if got := identity.NormalizeMailboxAddress(tc.input); got != c.Address {
				t.Errorf("suppression lookup key = %q but contact key = %q — "+
					"contacts and suppressions would not match", got, c.Address)
			}
		})
	}
}

// TestCreateContactCollapsesDisplayNameForm proves the CSV case that motivated
// the design: exports routinely carry "Name <addr>" in one column, and
// re-importing the bare address must hit the SAME row rather than duplicating.
func TestCreateContactCollapsesDisplayNameForm(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "collapse")

	first, err := store.CreateContact(ctx, user.ID, "A. Partner <partner@collapse.vc>",
		"A. Partner", nil, identity.ContactSourceImport, "imp_1")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err = store.CreateContact(ctx, user.ID, "partner@collapse.vc", "", nil,
		identity.ContactSourceManual, "")
	if !errors.Is(err, identity.ErrContactExists) {
		t.Fatalf("second create err = %v, want ErrContactExists (rows must collapse)", err)
	}

	got, err := store.GetContactByAddress(ctx, user.ID, "PARTNER@collapse.vc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != first.ID {
		t.Errorf("lookup returned %s, want the original %s", got.ID, first.ID)
	}
}

// TestContactTenantIsolation pins that one account cannot read, update, or
// delete another's contact, and that "not mine" is indistinguishable from
// "does not exist".
func TestContactTenantIsolation(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	alice := newContactOwner(t, store, "alice")
	bob := newContactOwner(t, store, "bob")

	if _, err := store.CreateContact(ctx, alice.ID, "shared@iso.vc", "Shared", nil,
		identity.ContactSourceManual, ""); err != nil {
		t.Fatalf("alice create: %v", err)
	}

	if _, err := store.GetContactByAddress(ctx, bob.ID, "shared@iso.vc"); !errors.Is(err, identity.ErrContactNotFound) {
		t.Errorf("bob get err = %v, want ErrContactNotFound", err)
	}
	if _, err := store.UpdateContact(ctx, bob.ID, "shared@iso.vc", strPtr("Hijacked"), nil); !errors.Is(err, identity.ErrContactNotFound) {
		t.Errorf("bob update err = %v, want ErrContactNotFound", err)
	}
	if removed, err := store.DeleteContact(ctx, bob.ID, "shared@iso.vc"); err != nil || removed {
		t.Errorf("bob delete removed=%v err=%v, want removed=false", removed, err)
	}

	// Alice's row must be untouched by all of the above.
	got, err := store.GetContactByAddress(ctx, alice.ID, "shared@iso.vc")
	if err != nil {
		t.Fatalf("alice get after bob's attempts: %v", err)
	}
	if got.DisplayName != "Shared" {
		t.Errorf("display_name = %q, want %q — bob mutated alice's row", got.DisplayName, "Shared")
	}
}

// TestUpdateContactPreservesOmittedFields pins PATCH semantics: omitting a
// field must leave it alone. Getting this wrong would mean a display-name edit
// silently erasing the metadata an import populated.
func TestUpdateContactPreservesOmittedFields(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "patch")

	meta := map[string]any{"fund": "Example Capital", "check": "1-3M"}
	if _, err := store.CreateContact(ctx, user.ID, "partner@patch.vc", "A. Partner", meta,
		identity.ContactSourceImport, "imp_patch"); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Update ONLY display_name; metadata must survive.
	updated, err := store.UpdateContact(ctx, user.ID, "partner@patch.vc", strPtr("Renamed"), nil)
	if err != nil {
		t.Fatalf("update name: %v", err)
	}
	if updated.DisplayName != "Renamed" {
		t.Errorf("display_name = %q, want %q", updated.DisplayName, "Renamed")
	}
	if updated.Metadata["fund"] != "Example Capital" {
		t.Errorf("metadata lost on name-only update: %#v", updated.Metadata)
	}

	// Update ONLY metadata; display_name must survive.
	updated, err = store.UpdateContact(ctx, user.ID, "partner@patch.vc", nil,
		map[string]any{"fund": "Other Capital"})
	if err != nil {
		t.Fatalf("update metadata: %v", err)
	}
	if updated.DisplayName != "Renamed" {
		t.Errorf("display_name clobbered by metadata-only update: %q", updated.DisplayName)
	}
	if updated.Metadata["fund"] != "Other Capital" {
		t.Errorf("metadata = %#v, want fund=Other Capital", updated.Metadata)
	}

	// Provenance is immutable through the update path.
	if updated.Source != identity.ContactSourceImport || updated.ImportBatchID != "imp_patch" {
		t.Errorf("provenance changed: source=%q batch=%q", updated.Source, updated.ImportBatchID)
	}
}

// TestDeleteContactLeavesSuppressionIntact pins design §8.6 invariant 5:
// removing a contact must never resurrect sendability for that address.
func TestDeleteContactLeavesSuppressionIntact(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "supp")

	if _, err := store.CreateContact(ctx, user.ID, "blocked@supp.vc", "", nil,
		identity.ContactSourceImport, "imp_supp"); err != nil {
		t.Fatalf("create contact: %v", err)
	}
	if _, err := store.AddSuppression(ctx, user.ID, "blocked@supp.vc", "unsubscribed", "manual", ""); err != nil {
		t.Fatalf("add suppression: %v", err)
	}

	removed, err := store.DeleteContact(ctx, user.ID, "blocked@supp.vc")
	if err != nil || !removed {
		t.Fatalf("delete contact removed=%v err=%v", removed, err)
	}

	blocked, err := store.EffectiveSuppressions(ctx, user.ID, "", []string{"blocked@supp.vc"})
	if err != nil {
		t.Fatalf("suppression lookup: %v", err)
	}
	if len(blocked) != 1 {
		t.Fatalf("suppression lookup returned %v after contact delete — "+
			"deleting a contact must not make a blocked address sendable", blocked)
	}
}

// TestListContactsKeysetPagination pins stable ordering and non-overlapping
// pages, the property an unordered list would quietly violate.
func TestListContactsKeysetPagination(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "page")

	const total = 7
	for i := 0; i < total; i++ {
		addr := string(rune('a'+i)) + "@page.vc"
		if _, err := store.CreateContact(ctx, user.ID, addr, "", nil,
			identity.ContactSourceManual, ""); err != nil {
			t.Fatalf("create %s: %v", addr, err)
		}
	}

	seen := map[string]bool{}
	var afterCreated time.Time
	var afterID string
	pages := 0
	for {
		rows, err := store.ListContacts(ctx, user.ID, identity.ContactFilter{}, 3, afterCreated, afterID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) == 0 {
			break
		}
		pages++
		for _, r := range rows {
			if seen[r.ID] {
				t.Fatalf("contact %s (%s) returned on more than one page", r.ID, r.Address)
			}
			seen[r.ID] = true
		}
		last := rows[len(rows)-1]
		afterCreated, afterID = last.CreatedAt, last.ID
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Errorf("paged over %d contacts, want %d", len(seen), total)
	}
}

// TestListContactsFilters pins that filters narrow rather than silently match
// everything — the failure mode of an unset filter matching year-1 timestamps.
func TestListContactsFilters(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "filter")

	if _, err := store.CreateContact(ctx, user.ID, "imported@filter.vc", "", nil,
		identity.ContactSourceImport, "imp_x"); err != nil {
		t.Fatalf("create imported: %v", err)
	}
	if _, err := store.CreateContact(ctx, user.ID, "manual@filter.vc", "", nil,
		identity.ContactSourceManual, ""); err != nil {
		t.Fatalf("create manual: %v", err)
	}

	all, err := store.ListContacts(ctx, user.ID, identity.ContactFilter{}, 10, time.Time{}, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("unfiltered list = %d rows (err %v), want 2", len(all), err)
	}

	bySource, err := store.ListContacts(ctx, user.ID,
		identity.ContactFilter{Source: identity.ContactSourceImport}, 10, time.Time{}, "")
	if err != nil || len(bySource) != 1 || bySource[0].Address != "imported@filter.vc" {
		t.Fatalf("source filter = %#v (err %v), want only imported@filter.vc", bySource, err)
	}

	byBatch, err := store.ListContacts(ctx, user.ID,
		identity.ContactFilter{ImportBatchID: "imp_x"}, 10, time.Time{}, "")
	if err != nil || len(byBatch) != 1 {
		t.Fatalf("batch filter = %d rows (err %v), want 1", len(byBatch), err)
	}

	none, err := store.ListContacts(ctx, user.ID,
		identity.ContactFilter{ImportBatchID: "imp_absent"}, 10, time.Time{}, "")
	if err != nil || len(none) != 0 {
		t.Fatalf("absent batch filter = %d rows (err %v), want 0", len(none), err)
	}
}

func strPtr(s string) *string { return &s }

// TestImportContactsMergePreservesProvenance is the re-import invariant at the
// storage layer. Uploading a corrected spreadsheet must refresh identity while
// leaving where the contact came from alone — the same rule that will protect
// outreach state once engagements hang off these rows.
func TestImportContactsMergePreservesProvenance(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "impmerge")

	first, err := store.ImportContacts(ctx, user.ID, "imp_first", []identity.ContactImportRow{
		{Address: "partner@impmerge.vc", DisplayName: strPtr("Original"),
			Metadata: map[string]any{"fund": "Example Capital"}},
	}, true)
	if err != nil || first[0].Status != identity.ImportStatusCreated {
		t.Fatalf("first import = %#v err=%v", first, err)
	}

	second, err := store.ImportContacts(ctx, user.ID, "imp_second", []identity.ContactImportRow{
		{Address: "partner@impmerge.vc", DisplayName: strPtr("Corrected")},
	}, true)
	if err != nil || second[0].Status != identity.ImportStatusUpdated {
		t.Fatalf("re-import = %#v err=%v", second, err)
	}

	got, err := store.GetContactByAddress(ctx, user.ID, "partner@impmerge.vc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DisplayName != "Corrected" {
		t.Errorf("display_name = %q, want Corrected", got.DisplayName)
	}
	if got.ImportBatchID != "imp_first" {
		t.Errorf("import_batch_id = %q, want imp_first — a merge must not rewrite provenance", got.ImportBatchID)
	}
}

// TestImportContactsSkipsIntraBatchDuplicates pins that a spreadsheet listing
// the same person twice (in any case form) reports the second row rather than
// racing itself inside the transaction.
func TestImportContactsSkipsIntraBatchDuplicates(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "impdupe")

	out, err := store.ImportContacts(ctx, user.ID, "imp_dupe", []identity.ContactImportRow{
		{Address: "dupe@impdupe.vc", DisplayName: strPtr("First")},
		{Address: "A. Dupe <DUPE@impdupe.vc>", DisplayName: strPtr("Same person")},
	}, true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if out[0].Status != identity.ImportStatusCreated {
		t.Errorf("row 0 = %#v, want created", out[0])
	}
	if out[1].Status != identity.ImportStatusSkipped || out[1].Code != "duplicate_in_batch" {
		t.Errorf("row 1 = %#v, want skipped/duplicate_in_batch", out[1])
	}
}

// TestDeleteImportBatchIsTenantScoped pins that a batch id from one account is
// meaningless in another, and reports not-found rather than deleting nothing
// silently.
func TestDeleteImportBatchIsTenantScoped(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	alice := newContactOwner(t, store, "impalice")
	bob := newContactOwner(t, store, "impbob")

	if _, err := store.ImportContacts(ctx, alice.ID, "imp_shared", []identity.ContactImportRow{
		{Address: "partner@impiso.vc"},
	}, true); err != nil {
		t.Fatalf("alice import: %v", err)
	}

	if _, _, err := store.DeleteImportBatch(ctx, bob.ID, "imp_shared"); !errors.Is(err, identity.ErrImportBatchNotFound) {
		t.Errorf("bob reversal err = %v, want ErrImportBatchNotFound", err)
	}
	if _, err := store.GetContactByAddress(ctx, alice.ID, "partner@impiso.vc"); err != nil {
		t.Errorf("alice's contact removed by bob's reversal: %v", err)
	}

	deleted, _, err := store.DeleteImportBatch(ctx, alice.ID, "imp_shared")
	if err != nil || deleted != 1 {
		t.Errorf("alice reversal deleted=%d err=%v, want 1", deleted, err)
	}
}

// TestImportMergeOmittedMetadataIsPreserved pins the behaviour a live e2e
// caught the hard way: re-importing a corrected spreadsheet that no longer
// carries a metadata column must NOT erase what the first import wrote.
//
// Omitted means "leave alone" (matching PATCH on the same resource); an
// explicitly supplied object — including an empty one — replaces.
func TestImportMergeOmittedMetadataIsPreserved(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "impmeta")

	if _, err := store.ImportContacts(ctx, user.ID, "imp_a", []identity.ContactImportRow{
		{Address: "partner@impmeta.vc", DisplayName: strPtr("Original"),
			Metadata: map[string]any{"fund": "Example Capital", "check": "1-3M"}},
	}, true); err != nil {
		t.Fatalf("first import: %v", err)
	}

	// Re-import with NO metadata key at all.
	if _, err := store.ImportContacts(ctx, user.ID, "imp_b", []identity.ContactImportRow{
		{Address: "partner@impmeta.vc", DisplayName: strPtr("Corrected")},
	}, true); err != nil {
		t.Fatalf("re-import: %v", err)
	}

	got, err := store.GetContactByAddress(ctx, user.ID, "partner@impmeta.vc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DisplayName != "Corrected" {
		t.Errorf("display_name = %q, want Corrected", got.DisplayName)
	}
	if got.Metadata["fund"] != "Example Capital" || got.Metadata["check"] != "1-3M" {
		t.Errorf("metadata = %#v — a narrower re-import erased columns it did not carry", got.Metadata)
	}

	// An explicit object still replaces.
	if _, err := store.ImportContacts(ctx, user.ID, "imp_c", []identity.ContactImportRow{
		{Address: "partner@impmeta.vc", Metadata: map[string]any{"fund": "Other Capital"}},
	}, true); err != nil {
		t.Fatalf("explicit-metadata import: %v", err)
	}
	got, err = store.GetContactByAddress(ctx, user.ID, "partner@impmeta.vc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DisplayName != "Corrected" {
		t.Errorf("display_name = %q — an import row without a name column erased the name", got.DisplayName)
	}
	if got.Metadata["fund"] != "Other Capital" {
		t.Errorf("metadata = %#v, want fund=Other Capital", got.Metadata)
	}
	if _, stale := got.Metadata["check"]; stale {
		t.Errorf("metadata = %#v — an explicit object must REPLACE, not merge key-by-key", got.Metadata)
	}
}

// TestDeleteImportBatchRetainsContactsWithHistory pins the safety rule that
// makes an import reversal an undo rather than a destructive operation.
//
// The reversal is addressed by batch id, so the caller cannot see which rows
// it would remove. If it deleted indiscriminately, undoing a months-old import
// would silently destroy contacts the account has since corresponded with —
// and the receipt would report success. The design named this rule and the
// response has always carried contacts_retained; the condition itself was
// missing, so retained was always zero.
func TestDeleteImportBatchRetainsContactsWithHistory(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := newContactOwner(t, store, "retain")

	if _, err := store.ClaimOrCreateDomain(ctx, "retain.example.com", user.ID); err != nil {
		t.Fatalf("claim domain: %v", err)
	}
	const agent = "raise@retain.example.com"
	if _, err := store.CreateAgent(ctx, agent, "retain.example.com", "",
		"https://example.com/webhook", "", user.ID); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	const contacted = "contacted@retain.vc"
	const untouched = "untouched@retain.vc"
	if _, err := store.ImportContacts(ctx, user.ID, "imp_retain", []identity.ContactImportRow{
		{Address: contacted}, {Address: untouched},
	}, true); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Correspond with exactly one of them.
	if _, err := store.CreateOutboundMessage(ctx, agent, []string{contacted}, nil, nil,
		"Intro", "send", "smtp", "", "conv-retain", []byte("raw")); err != nil {
		t.Fatalf("send: %v", err)
	}

	deleted, retained, err := store.DeleteImportBatch(ctx, user.ID, "imp_retain")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if deleted != 1 || retained != 1 {
		t.Errorf("reversal deleted=%d retained=%d, want 1 and 1 — a contact with "+
			"message history was destroyed by an undo", deleted, retained)
	}
	if _, err := store.GetContactByAddress(ctx, user.ID, contacted); err != nil {
		t.Errorf("a contact this account has corresponded with was deleted by the reversal: %v", err)
	}
	if _, err := store.GetContactByAddress(ctx, user.ID, untouched); !errors.Is(err, identity.ErrContactNotFound) {
		t.Errorf("an untouched imported contact survived the reversal: %v", err)
	}
}
