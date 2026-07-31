package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
)

// These tests encode the account-level contact contract from
// docs/design/2026-07-24-contacts-and-outreach-state.md §8. They are written
// against the HTTP surface only — status codes, envelopes, headers, and
// canonicalization — so they constrain the wire contract rather than the
// handler's internals, and survive any reimplementation behind it.
//
// sendJSON drives a real httptest.Server over the loopback transport, so path
// encoding and routing are genuinely exercised (design §6.3).

// contactFixture is an in-memory stand-in for the contact store. It is a FAKE,
// not a mock: it holds real state and enforces the same canonicalization and
// tenancy rules the real store does, so these tests assert on outcomes rather
// than on which store method was called.
type contactFixture struct {
	mu   sync.Mutex
	rows map[string]identity.Contact // key: userID \x00 normalized address
}

func (f *contactFixture) key(userID, address string) string {
	return userID + "\x00" + identity.NormalizeMailboxAddress(address)
}

func newContactsServer(t *testing.T, mutate func(*Deps, *contactFixture)) *httptest.Server {
	t.Helper()
	fixture := &contactFixture{rows: map[string]identity.Contact{}}
	user := &identity.User{ID: "u_1", Email: "owner@example.com"}
	otherUser := &identity.User{ID: "u_2", Email: "stranger@example.com"}
	clock := time.Unix(1700000000, 0).UTC()

	deps := Deps{
		PrincipalAuthenticator: func(r *http.Request) (*identity.Principal, error) {
			switch r.Header.Get("Authorization") {
			case "Bearer account":
				return &identity.Principal{User: user, Scope: identity.ScopeAccount}, nil
			// A SECOND, unrelated account. Present so tenant isolation is
			// testable at the HTTP layer: with only one user, a handler that
			// forgot to thread p.User.ID into the store would pass every test.
			case "Bearer other-account":
				return &identity.Principal{User: otherUser, Scope: identity.ScopeAccount}, nil
			case "Bearer agent":
				return &identity.Principal{User: user, Scope: identity.ScopeAgent, AgentID: "raise@example.com"}, nil
			default:
				return nil, errors.New("unauthorized")
			}
		},
		CreateContact: func(_ context.Context, userID, address, displayName string, metadata map[string]any, source, batch string) (identity.Contact, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			k := fixture.key(userID, address)
			if _, exists := fixture.rows[k]; exists {
				return identity.Contact{}, identity.ErrContactExists
			}
			if metadata == nil {
				metadata = map[string]any{}
			}
			c := identity.Contact{
				ID:            "cnt_" + identity.NormalizeMailboxAddress(address),
				Address:       identity.NormalizeMailboxAddress(address),
				DisplayName:   displayName,
				Metadata:      metadata,
				Source:        source,
				ImportBatchID: batch,
				CreatedAt:     clock,
				UpdatedAt:     clock,
			}
			fixture.rows[k] = c
			return c, nil
		},
		GetContact: func(_ context.Context, userID, address string) (identity.Contact, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			c, ok := fixture.rows[fixture.key(userID, address)]
			if !ok {
				return identity.Contact{}, identity.ErrContactNotFound
			}
			return c, nil
		},
		ListContacts: func(_ context.Context, userID string, f identity.ContactFilter, limit int, after time.Time, afterID string) ([]identity.Contact, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			var out []identity.Contact
			for k, c := range fixture.rows {
				// Match the full tenant segment, not a prefix: "u_1" must not
				// match rows belonging to "u_10".
				if !strings.HasPrefix(k, userID+"\x00") {
					continue
				}
				if f.Source != "" && c.Source != f.Source {
					continue
				}
				if f.ImportBatchID != "" && c.ImportBatchID != f.ImportBatchID {
					continue
				}
				out = append(out, c)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
			if afterID != "" {
				for i := range out {
					if out[i].ID == afterID {
						out = out[i+1:]
						break
					}
				}
			}
			if limit > 0 && len(out) > limit {
				out = out[:limit]
			}
			return out, nil
		},
		UpdateContact: func(_ context.Context, userID, address string, displayName *string, metadata map[string]any) (identity.Contact, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			k := fixture.key(userID, address)
			c, ok := fixture.rows[k]
			if !ok {
				return identity.Contact{}, identity.ErrContactNotFound
			}
			if displayName != nil {
				c.DisplayName = *displayName
			}
			if metadata != nil {
				c.Metadata = metadata
			}
			c.UpdatedAt = c.UpdatedAt.Add(time.Second)
			fixture.rows[k] = c
			return c, nil
		},
		UpdateContactIfUnchanged: func(_ context.Context, userID, address string, displayName *string, metadata map[string]any, expected time.Time) (identity.Contact, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			k := fixture.key(userID, address)
			c, ok := fixture.rows[k]
			if !ok {
				return identity.Contact{}, identity.ErrContactNotFound
			}
			if !c.UpdatedAt.Equal(expected) {
				return identity.Contact{}, identity.ErrContactPreconditionFailed
			}
			if displayName != nil {
				c.DisplayName = *displayName
			}
			if metadata != nil {
				c.Metadata = metadata
			}
			c.UpdatedAt = c.UpdatedAt.Add(time.Second)
			fixture.rows[k] = c
			return c, nil
		},
		DeleteContact: func(_ context.Context, userID, address string) (bool, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			k := fixture.key(userID, address)
			_, ok := fixture.rows[k]
			delete(fixture.rows, k)
			return ok, nil
		},
		ImportContacts: func(_ context.Context, userID, batchID string, rows []identity.ContactImportRow, merge bool) ([]identity.ContactImportOutcome, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			outcomes := make([]identity.ContactImportOutcome, len(rows))
			seen := map[string]int{}
			for i, row := range rows {
				address := identity.NormalizeMailboxAddress(row.Address)
				if first, dup := seen[address]; dup {
					outcomes[i] = identity.ContactImportOutcome{
						Index: i, Address: address, Status: identity.ImportStatusSkipped,
						Code:    "duplicate_in_batch",
						Message: fmt.Sprintf("duplicate of row %d in this batch", first),
					}
					continue
				}
				seen[address] = i
				k := fixture.key(userID, address)
				existing, exists := fixture.rows[k]
				if exists && !merge {
					outcomes[i] = identity.ContactImportOutcome{
						Index: i, Address: address, Status: identity.ImportStatusSkipped,
						Code: "already_exists",
					}
					continue
				}
				metadata := row.Metadata
				if metadata == nil {
					metadata = map[string]any{}
				}
				status := identity.ImportStatusCreated
				name := ""
				if row.DisplayName != nil {
					name = *row.DisplayName
				} else if exists {
					name = existing.DisplayName
				}
				c := identity.Contact{
					ID: "cnt_" + address, Address: address, DisplayName: name,
					Metadata: metadata, Source: identity.ContactSourceImport,
					ImportBatchID: batchID, CreatedAt: clock, UpdatedAt: clock,
				}
				if exists {
					// merge: refresh identity, keep provenance where it was.
					status = identity.ImportStatusUpdated
					c.ID = existing.ID
					c.Source = existing.Source
					c.ImportBatchID = existing.ImportBatchID
					c.CreatedAt = existing.CreatedAt
				}
				fixture.rows[k] = c
				outcomes[i] = identity.ContactImportOutcome{Index: i, Address: address, Status: status}
			}
			return outcomes, nil
		},
		DeleteImportBatch: func(_ context.Context, userID, batchID string) (int, int, int, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			var keys []string
			for k, c := range fixture.rows {
				if strings.HasPrefix(k, userID+"\x00") && c.ImportBatchID == batchID {
					keys = append(keys, k)
				}
			}
			if len(keys) == 0 {
				return 0, 0, 0, identity.ErrImportBatchNotFound
			}
			for _, k := range keys {
				delete(fixture.rows, k)
			}
			return len(keys), 0, 0, nil
		},
		CursorSecret: "contacts-test-secret",
	}
	deps.ImportContactsWithOptions = func(ctx context.Context, userID, batchID string, rows []identity.ContactImportRow, options identity.ContactImportOptions) ([]identity.ContactImportOutcome, error) {
		return deps.ImportContacts(ctx, userID, batchID, rows, options.Merge)
	}
	if mutate != nil {
		mutate(&deps, fixture)
	}
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)
	return srv
}

// seedContact creates one contact through the API so tests exercise only the
// public surface.
func seedContact(t *testing.T, srv *httptest.Server, address, displayName string) map[string]any {
	t.Helper()
	code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/contacts", "account", map[string]any{
		"address": address, "display_name": displayName,
	})
	if code != http.StatusCreated {
		t.Fatalf("seed %s = %d %v; want 201", address, code, body)
	}
	return body
}

// TestContactsRequireAccountScope pins design §8.6 invariant 10. An
// agent-scoped credential reaching account-wide contact identity would be a
// scope escalation: it would expose every person any sibling agent corresponds
// with.
func TestContactsRequireAccountScope(t *testing.T) {
	srv := newContactsServer(t, nil)
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/contacts", nil},
		{http.MethodPost, "/v1/contacts", map[string]any{"address": "partner@fund.vc"}},
		{http.MethodGet, "/v1/contacts/partner%40fund.vc", nil},
		{http.MethodPatch, "/v1/contacts/partner%40fund.vc", map[string]any{"display_name": "X"}},
		{http.MethodDelete, "/v1/contacts/partner%40fund.vc?confirm=DELETE", nil},
	} {
		code, body := sendJSON(t, tc.method, srv.URL+tc.path, "agent", tc.body)
		if code != http.StatusForbidden || errCode(body) != "forbidden" {
			t.Errorf("%s %s = %d %v; want 403 forbidden", tc.method, tc.path, code, body)
		}
	}
}

// TestContactsRejectUnauthenticated pins that no contact route is public.
func TestContactsRejectUnauthenticated(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, _ := sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts", "", nil)
	if code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /v1/contacts = %d; want 401", code)
	}
}

// TestCreateContactReturns201WithCanonicalBody pins the creation status and
// response shape. The Location header is asserted separately in
// TestCreateContactSetsLocationHeader, which uses a header-aware client —
// sendJSON discards headers, so a name promising Location here would lie.
func TestCreateContactReturns201WithCanonicalBody(t *testing.T) {
	srv := newContactsServer(t, nil)
	body := seedContact(t, srv, "A. Partner <Partner@Fund.VC>", "A. Partner")

	if body["address"] != "partner@fund.vc" {
		t.Errorf("address = %v; want canonicalized partner@fund.vc", body["address"])
	}
	if body["display_name"] != "A. Partner" {
		t.Errorf("display_name = %v", body["display_name"])
	}
	if body["source"] != "manual" {
		t.Errorf("source = %v; want manual for an API-created contact", body["source"])
	}
	if body["created_at"] == nil || body["updated_at"] == nil {
		t.Errorf("missing timestamps: %v", body)
	}
	if _, ok := body["metadata"]; !ok {
		t.Errorf("metadata must always be present (never omitted): %v", body)
	}
}

// TestCreateContactCanonicalizesAddress is the wire-level half of the parity
// invariant (§8.6 #1): a display-name form from a spreadsheet export and the
// bare address must resolve to ONE resource, addressable by either.
func TestCreateContactCanonicalizesAddress(t *testing.T) {
	srv := newContactsServer(t, nil)
	seedContact(t, srv, `"Partner, A." <Partner@Fund.VC>`, "A. Partner")

	// The bare, lower-cased address must now be a duplicate, not a new row.
	code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/contacts", "account", map[string]any{
		"address": "partner@fund.vc",
	})
	if code != http.StatusConflict || errCode(body) != "conflict" {
		t.Fatalf("duplicate create = %d %v; want 409 conflict", code, body)
	}

	// And the same resource must be fetchable under a case variant.
	code, got := sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts/PARTNER%40FUND.VC", "account", nil)
	if code != http.StatusOK {
		t.Fatalf("GET case-variant = %d %v; want 200", code, got)
	}
	if got["address"] != "partner@fund.vc" {
		t.Errorf("address = %v; want partner@fund.vc", got["address"])
	}
}

// TestContactAddressPathEncoding is the §6.3 gate. Address-keyed paths mean @
// and + arrive percent-encoded, and this repo has shipped a routing bug of
// exactly this class before (framework-bypass routes with encoded params).
// sendJSON issues a real HTTP request, so this exercises the actual router.
func TestContactAddressPathEncoding(t *testing.T) {
	srv := newContactsServer(t, nil)
	seedContact(t, srv, "partner+vc@fund.vc", "Plus Tagged")

	for _, encoded := range []string{
		"partner%2Bvc%40fund.vc", // both @ and + percent-encoded
		"partner+vc%40fund.vc",   // bare + in path, @ encoded
	} {
		code, body := sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts/"+encoded, "account", nil)
		if code != http.StatusOK {
			t.Errorf("GET /v1/contacts/%s = %d %v; want 200 — encoded-address routing", encoded, code, body)
			continue
		}
		if body["address"] != "partner+vc@fund.vc" {
			t.Errorf("GET /v1/contacts/%s address = %v; want partner+vc@fund.vc (plus-tag must NOT be folded)", encoded, body["address"])
		}
	}
}

// TestCreateContactRejectsInvalidAddress pins boundary validation.
func TestCreateContactRejectsInvalidAddress(t *testing.T) {
	srv := newContactsServer(t, nil)
	for _, addr := range []string{"", "not-an-email", "missing@", "@nodomain.vc"} {
		code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/contacts", "account",
			map[string]any{"address": addr})
		if code != http.StatusBadRequest {
			t.Errorf("POST address=%q = %d %v; want 400", addr, code, body)
		}
	}
}

// TestGetMissingContactReturnsContactNotFound pins the dedicated error code
// from §8.4 rather than a bare not_found, matching the template_not_found /
// attachment_not_found convention.
func TestGetMissingContactReturnsContactNotFound(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts/absent%40fund.vc", "account", nil)
	if code != http.StatusNotFound || errCode(body) != "contact_not_found" {
		t.Errorf("GET absent = %d %v; want 404 contact_not_found", code, body)
	}
}

// TestPatchContactPreservesOmittedFields pins PATCH semantics at the wire
// level: sending only display_name must not erase metadata an import wrote.
func TestPatchContactPreservesOmittedFields(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/contacts", "account", map[string]any{
		"address":      "partner@patch.vc",
		"display_name": "Original",
		"metadata":     map[string]any{"fund": "Example Capital"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, body)
	}

	code, patched := sendJSON(t, http.MethodPatch, srv.URL+"/v1/contacts/partner%40patch.vc", "account",
		map[string]any{"display_name": "Renamed"})
	if code != http.StatusOK {
		t.Fatalf("PATCH = %d %v; want 200", code, patched)
	}
	if patched["display_name"] != "Renamed" {
		t.Errorf("display_name = %v; want Renamed", patched["display_name"])
	}
	meta, _ := patched["metadata"].(map[string]any)
	if meta["fund"] != "Example Capital" {
		t.Errorf("metadata erased by a display_name-only PATCH: %v", patched["metadata"])
	}
}

// TestPatchContactRejectsImmutableFields pins that identity and provenance are
// not client-writable — address IS the key, source and import_batch_id are
// provenance (§8.2, server-owned fields reject writes).
func TestPatchContactRejectsImmutableFields(t *testing.T) {
	srv := newContactsServer(t, nil)
	seedContact(t, srv, "partner@immutable.vc", "Original")

	// 422 is the repo's schema-validation status: the request struct is
	// registered strict, so Huma rejects an unexpected property before the
	// handler runs (same contract as account_extra_test.go).
	for _, field := range []string{"address", "source", "import_batch_id", "created_at"} {
		code, body := sendJSON(t, http.MethodPatch, srv.URL+"/v1/contacts/partner%40immutable.vc", "account",
			map[string]any{field: "hijacked"})
		if code != http.StatusUnprocessableEntity || errCode(body) != "invalid_request" {
			t.Errorf("PATCH %s = %d %v; want 422 invalid_request (immutable field rejected)", field, code, body)
		}
	}
}

// TestDeleteContactRequiresConfirm pins the DeleteConfirm convention shared
// with every other destructive v1 operation.
func TestDeleteContactRequiresConfirm(t *testing.T) {
	srv := newContactsServer(t, nil)
	seedContact(t, srv, "partner@del.vc", "Doomed")

	// confirm is a required query param, so Huma rejects its absence with 422
	// before the handler runs.
	code, body := sendJSON(t, http.MethodDelete, srv.URL+"/v1/contacts/partner%40del.vc", "account", nil)
	if code != http.StatusUnprocessableEntity || errCode(body) != "invalid_request" {
		t.Errorf("DELETE without confirm = %d %v; want 422 invalid_request", code, body)
	}

	code, body = sendJSON(t, http.MethodDelete, srv.URL+"/v1/contacts/partner%40del.vc?confirm=DELETE", "account", nil)
	if code != http.StatusOK {
		t.Fatalf("DELETE with confirm = %d %v; want 200", code, body)
	}
	if body["deleted"] != true || body["address"] != "partner@del.vc" {
		t.Errorf("delete result = %v; want {deleted:true, address:partner@del.vc}", body)
	}

	code, _ = sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts/partner%40del.vc", "account", nil)
	if code != http.StatusNotFound {
		t.Errorf("GET after delete = %d; want 404", code)
	}
}

// TestListContactsPageEnvelope pins the one list shape used across v1:
// {items, next_cursor}, with items always an array and never null.
func TestListContactsPageEnvelope(t *testing.T) {
	srv := newContactsServer(t, nil)

	code, body := sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts", "account", nil)
	if code != http.StatusOK {
		t.Fatalf("empty list = %d %v; want 200", code, body)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items = %#v; want [] even when empty (never null)", body["items"])
	}
	if len(items) != 0 {
		t.Errorf("items = %v; want empty", items)
	}
	if nc, present := body["next_cursor"]; !present || nc != nil {
		t.Errorf("next_cursor = %v (present=%v); want explicit null on the last page", nc, present)
	}

	seedContact(t, srv, "a@list.vc", "A")
	seedContact(t, srv, "b@list.vc", "B")

	code, body = sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts", "account", nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d %v", code, body)
	}
	if items, _ = body["items"].([]any); len(items) != 2 {
		t.Errorf("items = %d; want 2", len(items))
	}
}

// TestListContactsRejectsBadCursor pins that a tampered continuation is a
// clean 400 rather than a 500 or a silent first page.
func TestListContactsRejectsBadCursor(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts?cursor=not-a-real-cursor", "account", nil)
	if code != http.StatusBadRequest || errCode(body) != "invalid_cursor" {
		t.Errorf("bad cursor = %d %v; want 400 invalid_cursor", code, body)
	}
}

// TestListContactsRejectsOversizeLimit pins the server-side cap so a client
// cannot request an unbounded page.
func TestListContactsRejectsOversizeLimit(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts?limit=5000", "account", nil)
	if code != http.StatusUnprocessableEntity && code != http.StatusBadRequest {
		t.Errorf("limit=5000 = %d %v; want 4xx (limit capped at 100)", code, body)
	}
}

// sendJSONFull is sendJSON plus response headers. Needed because the contract
// makes promises the body cannot express — Location on 201, Cache-Control on
// GETs, ETag/If-Match concurrency — and sendJSON discards headers entirely.
func sendJSONFull(t *testing.T, method, url, bearer string, body any, extra map[string]string) (int, map[string]any, http.Header) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

// TestContactsTenantIsolation is the object-level authorization gate. Every
// other handler test runs as a single account, so a handler that failed to
// thread the caller's user ID into the store would pass all of them while
// exposing every tenant's contacts to every other tenant.
//
// This drives the SAME address from two unrelated accounts and asserts that
// neither can observe or mutate the other's row, and that "not yours" is
// indistinguishable from "does not exist".
func TestContactsTenantIsolation(t *testing.T) {
	srv := newContactsServer(t, nil)
	seedContact(t, srv, "partner@shared.vc", "Owned by u_1")

	const path = "/v1/contacts/partner%40shared.vc"

	code, body := sendJSON(t, http.MethodGet, srv.URL+path, "other-account", nil)
	if code != http.StatusNotFound || errCode(body) != "contact_not_found" {
		t.Errorf("cross-tenant GET = %d %v; want 404 contact_not_found", code, body)
	}

	code, body = sendJSON(t, http.MethodPatch, srv.URL+path, "other-account",
		map[string]any{"display_name": "Hijacked"})
	if code != http.StatusNotFound {
		t.Errorf("cross-tenant PATCH = %d %v; want 404", code, body)
	}

	code, body = sendJSON(t, http.MethodDelete, srv.URL+path+"?confirm=DELETE", "other-account", nil)
	if code != http.StatusNotFound {
		t.Errorf("cross-tenant DELETE = %d %v; want 404", code, body)
	}

	// The stranger's list must not contain the owner's contact.
	code, body = sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts", "other-account", nil)
	if code != http.StatusOK {
		t.Fatalf("stranger list = %d %v", code, body)
	}
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Errorf("stranger sees %d contacts; want 0 — cross-tenant leak", len(items))
	}

	// And the owner's row survived every attempt above, unmodified.
	code, owner := sendJSON(t, http.MethodGet, srv.URL+path, "account", nil)
	if code != http.StatusOK {
		t.Fatalf("owner GET after cross-tenant attempts = %d %v", code, owner)
	}
	if owner["display_name"] != "Owned by u_1" {
		t.Errorf("display_name = %v; a stranger mutated the owner's row", owner["display_name"])
	}
}

// TestCreateContactSetsLocationHeader pins the 201 + Location contract. The
// header must point at the CANONICAL address, so a client that created a
// contact from a spreadsheet's display-name form can follow the URL directly.
func TestCreateContactSetsLocationHeader(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body, hdr := sendJSONFull(t, http.MethodPost, srv.URL+"/v1/contacts", "account",
		map[string]any{"address": "A. Partner <Partner@Loc.VC>"}, nil)
	if code != http.StatusCreated {
		t.Fatalf("POST = %d %v; want 201", code, body)
	}
	loc := hdr.Get("Location")
	if loc == "" {
		t.Fatal("201 response has no Location header")
	}
	if !strings.Contains(loc, url.PathEscape("partner@loc.vc")) && !strings.Contains(loc, "partner%40loc.vc") {
		t.Errorf("Location = %q; want it to point at the canonical partner@loc.vc", loc)
	}
}

// TestContactReadsAreNotCached pins Cache-Control: no-store on every contact
// read. Contact lists are personal data; an intermediary caching them would be
// a privacy failure, and leaving cacheability to defaults is how that happens.
func TestContactReadsAreNotCached(t *testing.T) {
	srv := newContactsServer(t, nil)
	seedContact(t, srv, "partner@cache.vc", "Cached?")

	for _, path := range []string{"/v1/contacts", "/v1/contacts/partner%40cache.vc"} {
		code, _, hdr := sendJSONFull(t, http.MethodGet, srv.URL+path, "account", nil, nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
		if cc := hdr.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("GET %s Cache-Control = %q; want no-store", path, cc)
		}
	}
}

// TestPatchContactHonorsIfMatch pins optimistic concurrency: a stale ETag must
// 412 rather than silently clobbering a concurrent edit (lost update).
func TestPatchContactHonorsIfMatch(t *testing.T) {
	srv := newContactsServer(t, nil)
	seedContact(t, srv, "partner@etag.vc", "Original")
	const path = "/v1/contacts/partner%40etag.vc"

	code, _, hdr := sendJSONFull(t, http.MethodGet, srv.URL+path, "account", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET = %d", code)
	}
	etag := hdr.Get("ETag")
	if etag == "" {
		t.Fatal("GET of a single contact returned no ETag")
	}

	// Matching ETag → the write applies.
	code, body, _ := sendJSONFull(t, http.MethodPatch, srv.URL+path, "account",
		map[string]any{"display_name": "First writer"}, map[string]string{"If-Match": etag})
	if code != http.StatusOK {
		t.Fatalf("PATCH with current ETag = %d %v; want 200", code, body)
	}

	// The same (now stale) ETag → 412, not a silent overwrite.
	code, body, _ = sendJSONFull(t, http.MethodPatch, srv.URL+path, "account",
		map[string]any{"display_name": "Second writer"}, map[string]string{"If-Match": etag})
	if code != http.StatusPreconditionFailed {
		t.Errorf("PATCH with stale ETag = %d %v; want 412", code, body)
	}

	code, final := sendJSON(t, http.MethodGet, srv.URL+path, "account", nil)
	if code != http.StatusOK || final["display_name"] != "First writer" {
		t.Errorf("display_name = %v; the stale write must not have landed", final["display_name"])
	}
}

// TestPatchContactRejectsAWriteThatRacesAfterTheETagRead covers the narrow
// compare/write window: the first handler read sees a valid ETag, then another
// transaction wins before the conditional UPDATE.
func TestPatchContactRejectsAWriteThatRacesAfterTheETagRead(t *testing.T) {
	srv := newContactsServer(t, func(d *Deps, _ *contactFixture) {
		d.UpdateContactIfUnchanged = func(context.Context, string, string, *string, map[string]any, time.Time) (identity.Contact, error) {
			return identity.Contact{}, identity.ErrContactPreconditionFailed
		}
	})
	seedContact(t, srv, "partner@etag-race.vc", "Original")
	const path = "/v1/contacts/partner%40etag-race.vc"

	code, _, headers := sendJSONFull(t, http.MethodGet, srv.URL+path, "account", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET = %d", code)
	}
	code, body, _ := sendJSONFull(t, http.MethodPatch, srv.URL+path, "account",
		map[string]any{"display_name": "Losing writer"},
		map[string]string{"If-Match": headers.Get("ETag")})
	if code != http.StatusPreconditionFailed {
		t.Fatalf("racing PATCH = %d %v, want 412", code, body)
	}
	if errCode(body) != "precondition_failed" {
		t.Errorf("error=%v, want precondition_failed", body)
	}
}

// TestCreateContactRejectsUnknownFields pins the unknown-field policy on the
// CREATE path too, not just PATCH. A client typo ("displayName") must be a
// loud 400, not a silently dropped field.
func TestCreateContactRejectsUnknownFields(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/contacts", "account", map[string]any{
		"address":     "partner@unknown.vc",
		"displayName": "camelCase typo",
	})
	if code != http.StatusUnprocessableEntity || errCode(body) != "invalid_request" {
		t.Errorf("POST with unknown field = %d %v; want 422 invalid_request", code, body)
	}
}

// TestContactMetadataCaps pins the §7 Q2 limits. metadata is unbounded
// client-supplied input landing in JSONB, so this is both a contract gate and
// a resource-exhaustion gate. The literals here ARE the specification —
// deliberately not shared with the implementation's constants, so a wrong
// constant cannot satisfy its own test.
func TestContactMetadataCaps(t *testing.T) {
	srv := newContactsServer(t, nil)

	tooManyKeys := map[string]any{}
	for i := 0; i < 51; i++ { // cap is 50
		tooManyKeys[fmt.Sprintf("k%02d", i)] = "v"
	}

	cases := []struct {
		name     string
		metadata map[string]any
	}{
		{"serialized blob over 16KB", map[string]any{"blob": strings.Repeat("x", 17*1024)}},
		{"more than 50 keys", tooManyKeys},
		{"single value over 4KB", map[string]any{"note": strings.Repeat("y", 5*1024)}},
		{"key longer than 128 bytes", map[string]any{strings.Repeat("k", 129): "v"}},
		{"nested object", map[string]any{"fund": map[string]any{"name": "Example"}}},
		{"nested array of objects", map[string]any{"rounds": []any{map[string]any{"a": 1}}}},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/contacts", "account", map[string]any{
				"address":  fmt.Sprintf("meta%d@caps.vc", i),
				"metadata": tc.metadata,
			})
			if code != http.StatusBadRequest {
				t.Errorf("POST metadata (%s) = %d %v; want 400", tc.name, code, body)
			}
		})
	}
}

// TestContactMetadataAcceptsRealisticPayload is the positive counterpart: the
// caps must not reject the data this feature exists to carry. A wide investor
// CSV row with scalars and short strings has to fit comfortably.
func TestContactMetadataAcceptsRealisticPayload(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/contacts", "account", map[string]any{
		"address": "partner@realistic.vc",
		"metadata": map[string]any{
			"fund": "Example Capital", "check_size": "1-3M", "stage": "seed",
			"warm_intro": "via a mutual portfolio founder", "priority": 1,
			"partner_focus": "infrastructure", "responded_before": false,
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("realistic metadata = %d %v; want 201 — caps must not reject the real use case", code, body)
	}
	meta, _ := body["metadata"].(map[string]any)
	if meta["fund"] != "Example Capital" {
		t.Errorf("metadata round-trip lost data: %v", body["metadata"])
	}
}

// TestListContactsFiltersOverHTTP pins that filters actually narrow at the
// wire level. The store has its own filter test; this catches a handler that
// parses the query param and then forgets to pass it down.
func TestListContactsFiltersOverHTTP(t *testing.T) {
	srv := newContactsServer(t, nil)
	seedContact(t, srv, "manual@filter.vc", "Manual")

	code, body := sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts?source=manual", "account", nil)
	if code != http.StatusOK {
		t.Fatalf("source filter = %d %v", code, body)
	}
	if items, _ := body["items"].([]any); len(items) != 1 {
		t.Errorf("source=manual returned %d items; want 1", len(items))
	}

	code, body = sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts?source=import", "account", nil)
	if code != http.StatusOK {
		t.Fatalf("source=import = %d %v", code, body)
	}
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Errorf("source=import returned %d items; want 0 — filter is not being applied", len(items))
	}
}

// TestListContactsCursorContinuation walks a real multi-page listing over
// HTTP. The envelope test only proves the shape; this proves the cursor
// actually advances and that pages do not overlap or drop rows.
func TestListContactsCursorContinuation(t *testing.T) {
	srv := newContactsServer(t, nil)
	const total = 5
	for i := 0; i < total; i++ {
		seedContact(t, srv, fmt.Sprintf("p%d@page.vc", i), fmt.Sprintf("P%d", i))
	}

	seen := map[string]bool{}
	cursor := ""
	for page := 0; page <= total; page++ {
		u := srv.URL + "/v1/contacts?limit=2"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		code, body := sendJSON(t, http.MethodGet, u, "account", nil)
		if code != http.StatusOK {
			t.Fatalf("page %d = %d %v", page, code, body)
		}
		items, _ := body["items"].([]any)
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			addr, _ := item["address"].(string)
			if seen[addr] {
				t.Fatalf("address %s appeared on more than one page", addr)
			}
			seen[addr] = true
		}
		next, _ := body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != total {
		t.Errorf("paged over %d contacts; want %d", len(seen), total)
	}
}

// TestListContactsCursorPinsFilters pins §8.1: a continuation that changes the
// filter set must be rejected rather than silently returning rows from a
// different query, which would make a paginating client's results incoherent.
func TestListContactsCursorPinsFilters(t *testing.T) {
	srv := newContactsServer(t, nil)
	for i := 0; i < 3; i++ {
		seedContact(t, srv, fmt.Sprintf("pin%d@pin.vc", i), "Pinned")
	}

	code, body := sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts?limit=1&source=manual", "account", nil)
	if code != http.StatusOK {
		t.Fatalf("first page = %d %v", code, body)
	}
	cursor, _ := body["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("expected a continuation cursor for a 3-row/limit-1 listing")
	}

	// Same cursor, different filter — must not be honored.
	code, body = sendJSON(t, http.MethodGet,
		srv.URL+"/v1/contacts?limit=1&source=import&cursor="+url.QueryEscape(cursor), "account", nil)
	if code != http.StatusBadRequest || errCode(body) != "invalid_cursor" {
		t.Errorf("continuation with changed filter = %d %v; want 400 invalid_cursor", code, body)
	}
}

// TestPatchContactRejectsEmptyIfMatch is the regression for the guard that
// silently wasn't one. On staging sha-32ce45dc, `If-Match: ` (present, no
// value) on PATCH /v1/contacts/{address} returned 200 with a NEW ETag
// (req_91af2b0f6fe551c8df501fda, confirmed over a raw TLS socket so it is not a
// client artifact) — the conditional write the caller asked for silently became
// an unconditional one, and the 200 told the caller the guard had held.
//
// RFC 9110 §13.1.1 requires at least one member in the field value, so an empty
// one is a malformed request (400), not a precondition that failed (412). The
// quoted-empty form `""` IS a syntactically valid validator and keeps its 412.
func TestPatchContactRejectsEmptyIfMatch(t *testing.T) {
	srv := newContactsServer(t, nil)
	seedContact(t, srv, "empty-ifmatch@example.com", "Before")
	path := "/v1/contacts/empty-ifmatch%40example.com"

	code, body, _ := sendJSONFull(t, http.MethodPatch, srv.URL+path, "account",
		map[string]any{"display_name": "After"}, map[string]string{"If-Match": ""})
	if code != http.StatusBadRequest || errCode(body) != "invalid_request" {
		t.Fatalf("PATCH with empty If-Match = %d %v; want 400 invalid_request", code, body)
	}

	// The write must not have happened.
	code, after := sendJSON(t, http.MethodGet, srv.URL+path, "account", nil)
	if code != http.StatusOK || after["display_name"] != "Before" {
		t.Fatalf("GET after refused write = %d %v; want the untouched row", code, after)
	}

	// A quoted-empty validator is well-formed and simply does not match.
	code, body, _ = sendJSONFull(t, http.MethodPatch, srv.URL+path, "account",
		map[string]any{"display_name": "After"}, map[string]string{"If-Match": `""`})
	if code != http.StatusPreconditionFailed || errCode(body) != "precondition_failed" {
		t.Fatalf(`PATCH with If-Match: "" = %d %v; want 412 precondition_failed`, code, body)
	}

	// Omitting the header entirely is still an unconditional write.
	code, body, _ = sendJSONFull(t, http.MethodPatch, srv.URL+path, "account",
		map[string]any{"display_name": "After"}, nil)
	if code != http.StatusOK {
		t.Fatalf("PATCH without If-Match = %d %v; want 200", code, body)
	}
}

// TestETagMatchesToleratesTheWeakPrefix pins a deviation from RFC 9110 §13.1.1
// that is deliberate and load-bearing, so that a future reader who checks only
// the RFC does not "fix" it into an outage.
//
// api.e2a.dev is behind a Cloudflare proxy that actively transforms responses
// (br compression is confirmed live; our origin sets no `encode` directive, so
// the compression is the edge's). Cloudflare downgrades a strong ETag to a weak
// one whenever it transforms a response, and "Respect Strong ETags" is
// Enterprise-only while e2a.dev is on the Free plan. A client can therefore be
// handed `W/"abc"` for a row whose origin validator is `"abc"`, echo back
// exactly what it received, and — under a strict comparison — get a PERMANENT
// 412 no retry clears. Staging is DNS-only (unproxied), so the conformance gate
// structurally cannot catch that; it would be a prod-only break.
//
// The guard is not weakened by this: the compared body is the full validator,
// which moves on every accepted write, so a stale tag never matches.
func TestETagMatchesToleratesTheWeakPrefix(t *testing.T) {
	const current = `"88eba810c0136ae642cf65a30025858b"`
	cases := []struct {
		name    string
		ifMatch string
		want    bool
	}{
		{"exact strong validator", current, true},
		{"weak form of the current validator", `W/` + current, true},
		{"weak form inside a comma list", `W/"other", W/` + current, true},
		{"strong form inside a comma list", `"other", ` + current, true},
		{"mixed weak and strong in a list", `W/"other", ` + current, true},
		// Tolerating the prefix must not tolerate the wrong validator: a stale
		// tag stays stale however it is dressed.
		{"weak form of a different validator", `W/"deadbeef"`, false},
		{"weak list with no matching member", `W/"aaa", W/"bbb"`, false},
		{"unrelated strong validator", `"deadbeef"`, false},
		// RFC 9110 spells the weak prefix "W/" exactly, so lowercase is not a
		// weak validator and does not match.
		{"lowercase w/ is not a weak prefix", `w/` + current, false},
		{"wildcard", "*", true},
		{"wildcard with surrounding space", "  *  ", true},
		{"garbage", "garbage", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := etagMatches(tc.ifMatch, current); got != tc.want {
				t.Fatalf("etagMatches(%q) = %v, want %v", tc.ifMatch, got, tc.want)
			}
		})
	}
}

// TestPatchContactAcceptsAWeakenedValidator is the wire-level half: a client
// whose validator was weakened in transit must still be able to complete its
// conditional write, and a stale one must still be refused.
func TestPatchContactAcceptsAWeakenedValidator(t *testing.T) {
	srv := newContactsServer(t, nil)
	seedContact(t, srv, "weak-etag@example.com", "Before")
	path := "/v1/contacts/weak-etag%40example.com"

	code, _, headers := sendJSONFull(t, http.MethodGet, srv.URL+path, "account", nil, nil)
	etag := headers.Get("ETag")
	if code != http.StatusOK || etag == "" {
		t.Fatalf("GET = %d ETag=%q; want 200 with an ETag", code, etag)
	}

	// The weak form of the CURRENT validator — what a client sees through a
	// transforming CDN — still completes the write.
	code, body, _ := sendJSONFull(t, http.MethodPatch, srv.URL+path, "account",
		map[string]any{"display_name": "After"}, map[string]string{"If-Match": "W/" + etag})
	if code != http.StatusOK {
		t.Fatalf("PATCH with a weakened current validator = %d %v; want 200 — a CDN-weakened ETag must not permanently 412", code, body)
	}

	// That write moved the validator, so re-sending the old one — weak or not —
	// is now stale and must be refused.
	code, body, _ = sendJSONFull(t, http.MethodPatch, srv.URL+path, "account",
		map[string]any{"display_name": "Stale"}, map[string]string{"If-Match": "W/" + etag})
	if code != http.StatusPreconditionFailed || errCode(body) != "precondition_failed" {
		t.Fatalf("PATCH with a weakened STALE validator = %d %v; want 412", code, body)
	}
	code, after := sendJSON(t, http.MethodGet, srv.URL+path, "account", nil)
	if code != http.StatusOK || after["display_name"] != "After" {
		t.Fatalf("GET after refused write = %d %v; want the row left at \"After\"", code, after)
	}

	// `*` matches any existing representation.
	code, body, _ = sendJSONFull(t, http.MethodPatch, srv.URL+path, "account",
		map[string]any{"display_name": "Star"}, map[string]string{"If-Match": "*"})
	if code != http.StatusOK {
		t.Fatalf("PATCH with If-Match: * on an existing contact = %d %v; want 200", code, body)
	}
}
