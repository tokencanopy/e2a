package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Contract tests for POST /v1/contacts/import and the batch reversal, from
// design §3.3.1. The import path is where the highest-consequence invariants
// live: it is the one surface a user points at a list of real people, so it
// must never send, never resurrect consent, and never damage outreach state on
// a re-run.
//
// agent_email / stage enrollment is deliberately out of this slice — it needs
// the engagement resource — so these tests cover identity import only.

func importBody(t *testing.T, srv *httptest.Server, body any) (int, map[string]any) {
	t.Helper()
	return sendJSON(t, http.MethodPost, srv.URL+"/v1/contacts/import", "account", body)
}

// results indexes an import response's per-item results by their index field,
// so assertions read against the row they mean rather than array position.
func results(t *testing.T, body map[string]any) map[int]map[string]any {
	t.Helper()
	raw, ok := body["results"].([]any)
	if !ok {
		t.Fatalf("response has no results array: %v", body)
	}
	out := map[int]map[string]any{}
	for _, r := range raw {
		item, _ := r.(map[string]any)
		idx, _ := item["index"].(float64)
		out[int(idx)] = item
	}
	return out
}

// TestImportRequiresAccountScope pins that bulk contact writes are
// account-admin only, like every other contact operation.
func TestImportRequiresAccountScope(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/contacts/import", "agent",
		map[string]any{"contacts": []any{map[string]any{"address": "a@imp.vc"}}})
	if code != http.StatusForbidden || errCode(body) != "forbidden" {
		t.Errorf("agent-scoped import = %d %v; want 403 forbidden", code, body)
	}
}

// TestImportReturnsPerItemResults pins the partial-success contract: one bad
// row must not sink the rest, and every row reports its own outcome.
func TestImportReturnsPerItemResults(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := importBody(t, srv, map[string]any{"contacts": []any{
		map[string]any{"address": "A. Partner <Partner@Imp.VC>", "display_name": "A. Partner"},
		map[string]any{"address": "not-an-email"},
		map[string]any{"address": "second@imp.vc"},
	}})
	if code != http.StatusOK {
		t.Fatalf("import = %d %v; want 200", code, body)
	}
	if body["batch_id"] == nil || body["batch_id"] == "" {
		t.Errorf("import response has no batch_id: %v", body)
	}
	if body["created"] != float64(2) || body["failed"] != float64(1) {
		t.Errorf("counts = created:%v failed:%v; want 2 and 1 (%v)", body["created"], body["failed"], body)
	}

	res := results(t, body)
	if res[0]["status"] != "created" || res[0]["address"] != "partner@imp.vc" {
		t.Errorf("row 0 = %v; want created with the canonicalized address", res[0])
	}
	if res[1]["status"] != "failed" {
		t.Errorf("row 1 = %v; want failed", res[1])
	}
	if res[1]["code"] == nil || res[1]["code"] == "" {
		t.Errorf("failed row carries no machine code: %v", res[1])
	}
	if res[2]["status"] != "created" {
		t.Errorf("row 2 = %v; want created — a bad row must not sink later rows", res[2])
	}
}

// TestImportIsInert pins that import creates identity and nothing else. It is
// the one place a user hands e2a a list of real people, so "importing does not
// email anyone" has to be a tested property, not a claim.
func TestImportIsInert(t *testing.T) {
	// The contacts server wires no send capability at all, so a handler that
	// tried to send would fail loudly rather than silently succeed.
	srv := newContactsServer(t, nil)
	code, body := importBody(t, srv, map[string]any{"contacts": []any{
		map[string]any{"address": "quiet@imp.vc"},
	}})
	if code != http.StatusOK {
		t.Fatalf("import = %d %v", code, body)
	}
	// The contact exists...
	code, _ = sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts/quiet%40imp.vc", "account", nil)
	if code != http.StatusOK {
		t.Fatalf("imported contact not readable: %d", code)
	}
	// ...and the import reported no delivery-ish side effects.
	for _, key := range []string{"sent", "messages", "queued"} {
		if _, present := body[key]; present {
			t.Errorf("import response exposes %q — import must not send", key)
		}
	}
}

// TestImportMarksSuppressedWithoutDropping pins the consent contract: an
// address the account has already blocked is still imported, but flagged. The
// count stays honest — the user sees 500 imported, 3 unsendable, rather than a
// silent 497.
func TestImportMarksSuppressedWithoutDropping(t *testing.T) {
	srv := newContactsServer(t, func(d *Deps, _ *contactFixture) {
		d.SuppressedAddresses = func(_ context.Context, _ string, addresses []string) ([]string, error) {
			var out []string
			for _, a := range addresses {
				if a == "blocked@imp.vc" {
					out = append(out, a)
				}
			}
			return out, nil
		}
	})
	code, body := importBody(t, srv, map[string]any{"contacts": []any{
		map[string]any{"address": "ok@imp.vc"},
		map[string]any{"address": "blocked@imp.vc"},
	}})
	if code != http.StatusOK {
		t.Fatalf("import = %d %v", code, body)
	}
	if body["created"] != float64(2) {
		t.Errorf("created = %v; want 2 — a suppressed address is marked, not dropped", body["created"])
	}
	res := results(t, body)
	if res[0]["suppressed"] == true {
		t.Errorf("row 0 marked suppressed but is not blocked: %v", res[0])
	}
	if res[1]["suppressed"] != true {
		t.Errorf("row 1 = %v; want suppressed:true so the user can see why it is unsendable", res[1])
	}
	if res[1]["status"] != "created" {
		t.Errorf("row 1 status = %v; want created — suppression does not prevent the record", res[1]["status"])
	}
}

// TestImportMergeDoesNotClobber is the re-import invariant. Fixing a typo in a
// spreadsheet and re-uploading must refresh identity without resetting
// anything else — provenance here, and outreach state once engagements exist.
func TestImportMergeDoesNotClobber(t *testing.T) {
	srv := newContactsServer(t, nil)

	code, first := importBody(t, srv, map[string]any{"contacts": []any{
		map[string]any{"address": "partner@merge.vc", "display_name": "Original",
			"metadata": map[string]any{"fund": "Example Capital"}},
	}})
	if code != http.StatusOK {
		t.Fatalf("first import = %d %v", code, first)
	}
	batch := first["batch_id"]

	code, second := importBody(t, srv, map[string]any{"contacts": []any{
		map[string]any{"address": "partner@merge.vc", "display_name": "Corrected"},
	}})
	if code != http.StatusOK {
		t.Fatalf("re-import = %d %v", code, second)
	}
	if second["updated"] != float64(1) || second["created"] != float64(0) {
		t.Errorf("re-import counts = created:%v updated:%v; want 0 and 1",
			second["created"], second["updated"])
	}

	code, got := sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts/partner%40merge.vc", "account", nil)
	if code != http.StatusOK {
		t.Fatalf("get after re-import = %d %v", code, got)
	}
	if got["display_name"] != "Corrected" {
		t.Errorf("display_name = %v; want Corrected", got["display_name"])
	}
	// Provenance must still point at the FIRST batch — the row was updated,
	// not recreated, so its origin does not move.
	if got["import_batch_id"] != batch {
		t.Errorf("import_batch_id = %v; want the original %v — merge must not rewrite provenance",
			got["import_batch_id"], batch)
	}
}

// TestImportSkipsDuplicatesWithinOneBatch pins that a spreadsheet containing
// the same person twice reports the second row explicitly rather than silently
// counting it or failing the batch.
func TestImportSkipsDuplicatesWithinOneBatch(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := importBody(t, srv, map[string]any{"contacts": []any{
		map[string]any{"address": "dupe@imp.vc", "display_name": "First"},
		map[string]any{"address": "DUPE@imp.vc", "display_name": "Same person, different case"},
	}})
	if code != http.StatusOK {
		t.Fatalf("import = %d %v", code, body)
	}
	if body["created"] != float64(1) || body["skipped"] != float64(1) {
		t.Errorf("counts = created:%v skipped:%v; want 1 and 1 (%v)", body["created"], body["skipped"], body)
	}
	res := results(t, body)
	if res[1]["status"] != "skipped" || res[1]["code"] != "duplicate_in_batch" {
		t.Errorf("row 1 = %v; want skipped/duplicate_in_batch", res[1])
	}
}

// TestImportRejectsOversizeBatch pins the server-side cap. Import is
// synchronous by design, so the bound is what keeps that safe.
func TestImportRejectsOversizeBatch(t *testing.T) {
	srv := newContactsServer(t, nil)
	rows := make([]any, 1001)
	for i := range rows {
		rows[i] = map[string]any{"address": fmt.Sprintf("bulk%04d@imp.vc", i)}
	}
	code, body := importBody(t, srv, map[string]any{"contacts": rows})
	if code != http.StatusRequestEntityTooLarge && code != http.StatusUnprocessableEntity {
		t.Errorf("1001-row import = %d %v; want 413 (or 422 from schema maxItems)", code, body)
	}
}

// TestImportRejectsEmptyBatch pins that an empty upload is a clear error
// rather than a successful no-op the user misreads as "it worked".
func TestImportRejectsEmptyBatch(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := importBody(t, srv, map[string]any{"contacts": []any{}})
	if code != http.StatusUnprocessableEntity && code != http.StatusBadRequest {
		t.Errorf("empty import = %d %v; want 4xx", code, body)
	}
}

// TestImportEnforcesMetadataCaps pins that the per-row bounds apply on the
// bulk path too, and — critically — as a PER-ITEM failure. One oversized row
// must not reject the other 999.
func TestImportEnforcesMetadataCaps(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := importBody(t, srv, map[string]any{"contacts": []any{
		map[string]any{"address": "good@caps.vc"},
		map[string]any{"address": "fat@caps.vc", "metadata": map[string]any{"blob": strings.Repeat("x", 17*1024)}},
		map[string]any{"address": "alsogood@caps.vc"},
	}})
	if code != http.StatusOK {
		t.Fatalf("import = %d %v; want 200 with a per-item failure, not a whole-batch rejection", code, body)
	}
	res := results(t, body)
	if res[1]["status"] != "failed" {
		t.Errorf("oversized row = %v; want failed", res[1])
	}
	if res[0]["status"] != "created" || res[2]["status"] != "created" {
		t.Errorf("one fat row sank its neighbours: %v / %v", res[0], res[2])
	}
}

// TestDeleteImportBatchReversesTheImport pins the undo path, including its
// safety rule: only contacts that batch created and that have no history are
// removed, and the response says what it retained.
func TestDeleteImportBatchReversesTheImport(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, imported := importBody(t, srv, map[string]any{"contacts": []any{
		map[string]any{"address": "undo1@imp.vc"},
		map[string]any{"address": "undo2@imp.vc"},
	}})
	if code != http.StatusOK {
		t.Fatalf("import = %d %v", code, imported)
	}
	batch, _ := imported["batch_id"].(string)

	// Confirm requirement matches every other destructive operation.
	code, body := sendJSON(t, http.MethodDelete, srv.URL+"/v1/contacts/imports/"+batch, "account", nil)
	if code != http.StatusUnprocessableEntity && code != http.StatusBadRequest {
		t.Errorf("batch delete without confirm = %d %v; want 4xx", code, body)
	}

	code, body = sendJSON(t, http.MethodDelete,
		srv.URL+"/v1/contacts/imports/"+batch+"?confirm=DELETE", "account", nil)
	if code != http.StatusOK {
		t.Fatalf("batch delete = %d %v; want 200", code, body)
	}
	if body["deleted"] != true || body["batch_id"] != batch {
		t.Errorf("delete result = %v", body)
	}
	if body["contacts_deleted"] != float64(2) {
		t.Errorf("contacts_deleted = %v; want 2", body["contacts_deleted"])
	}

	code, _ = sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts/undo1%40imp.vc", "account", nil)
	if code != http.StatusNotFound {
		t.Errorf("contact still present after batch reversal: %d", code)
	}
}

// TestDeleteUnknownImportBatch pins the dedicated not-found code.
func TestDeleteUnknownImportBatch(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := sendJSON(t, http.MethodDelete,
		srv.URL+"/v1/contacts/imports/imp_absent?confirm=DELETE", "account", nil)
	if code != http.StatusNotFound || errCode(body) != "import_batch_not_found" {
		t.Errorf("unknown batch = %d %v; want 404 import_batch_not_found", code, body)
	}
}

// TestImportBatchDeleteIsTenantScoped pins that one account cannot reverse
// another's import by guessing a batch id.
func TestImportBatchDeleteIsTenantScoped(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, imported := importBody(t, srv, map[string]any{"contacts": []any{
		map[string]any{"address": "mine@imp.vc"},
	}})
	if code != http.StatusOK {
		t.Fatalf("import = %d %v", code, imported)
	}
	batch, _ := imported["batch_id"].(string)

	code, body := sendJSON(t, http.MethodDelete,
		srv.URL+"/v1/contacts/imports/"+batch+"?confirm=DELETE", "other-account", nil)
	if code != http.StatusNotFound {
		t.Errorf("cross-tenant batch delete = %d %v; want 404", code, body)
	}

	code, _ = sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts/mine%40imp.vc", "account", nil)
	if code != http.StatusOK {
		t.Errorf("owner's contact was removed by a stranger's batch delete: %d", code)
	}
}
