package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Regression suite for the NUL (U+0000) rule.
//
// Every case below returned HTTP 500 on the live staging build sha-32ce45dc:
// Postgres cannot store a NUL in a text column, so a caller string carrying one
// reached the driver and came back as internal_error. A permanent client error
// dressed as a 5xx is the worst possible answer — it is exactly what SDK retry
// logic hammers — so the rule is now enforced at the edge for every operation.
//
// The rule: no client-supplied string anywhere in a request (body at any depth,
// including object KEYS, plus path, query, and header params) may contain
// U+0000. Violations are 400 invalid_request with the offending location.

// nulLocation pulls the single field location out of a validation envelope.
func nulLocation(t *testing.T, body map[string]any) string {
	t.Helper()
	envelope, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response carries no error envelope: %v", body)
	}
	details, ok := envelope["details"].(map[string]any)
	if !ok {
		t.Fatalf("error carries no details: %v", body)
	}
	fields, ok := details["fields"].([]any)
	if !ok || len(fields) != 1 {
		t.Fatalf("want exactly one field error, got: %v", body)
	}
	field, _ := fields[0].(map[string]any)
	location, _ := field["location"].(string)
	return location
}

func assertNULRejected(t *testing.T, code int, body map[string]any, wantLocation string) {
	t.Helper()
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (a NUL is a permanent client error, never a 5xx); body=%v", code, body)
	}
	if errCode(body) != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request; body=%v", errCode(body), body)
	}
	if got := nulLocation(t, body); got != wantLocation {
		t.Errorf("location = %q, want %q", got, wantLocation)
	}
}

// TestNULInContactStringsIsRejectedNotStored covers three of the five staging
// vectors: display_name (req_6264c471b04b1f8f1f1091f1), a metadata VALUE
// (req_8935d9a809f83acf898dfe2c) and a metadata KEY
// (req_0f538473c1df984b86fb3d97). All three returned 500 "failed to create
// contact".
func TestNULInContactStringsIsRejectedNotStored(t *testing.T) {
	cases := []struct {
		name     string
		body     map[string]any
		location string
	}{
		{
			name:     "display_name",
			body:     map[string]any{"address": "nul-dn@example.com", "display_name": "a\x00b"},
			location: "body.display_name",
		},
		{
			name:     "metadata value",
			body:     map[string]any{"address": "nul-mv@example.com", "metadata": map[string]any{"k": "a\x00b"}},
			location: "body.metadata.k",
		},
		{
			// The offending key is deliberately NOT echoed into the location:
			// FieldError never puts raw bad input in the response.
			name:     "metadata key",
			body:     map[string]any{"address": "nul-mk@example.com", "metadata": map[string]any{"a\x00b": "v"}},
			location: "body.metadata",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newContactsServer(t, nil)
			code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/contacts", "account", tc.body)
			assertNULRejected(t, code, body, tc.location)

			// The rejection happens before the store is touched, so the
			// address must not exist afterwards. (The staging sweep confirmed
			// no partial writes; this keeps it that way.)
			address, _ := tc.body["address"].(string)
			code, _ = sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts/"+urlEsc(address), "account", nil)
			if code != http.StatusNotFound {
				t.Errorf("GET after rejected create = %d, want 404 — a refused request must leave no row", code)
			}
		})
	}
}

// TestNULInEngagementStageIsRejected covers the fifth staging vector:
// PUT /v1/agents/{email}/contacts/{address} with a NUL stage returned 500
// "failed to save outreach state" (req_d42087f5fc0b7fc7072c0e9e).
func TestNULInEngagementStageIsRejected(t *testing.T) {
	srv := newEngagementsServer(t, nil)
	code, body := sendJSON(t, http.MethodPut, srv.URL+raisePath+"/partner%40fund.vc", "account",
		map[string]any{"stage": "a\x00b"})
	assertNULRejected(t, code, body, "body.stage")

	code, _ = sendJSON(t, http.MethodGet, srv.URL+raisePath+"/partner%40fund.vc", "account", nil)
	if code != http.StatusNotFound {
		t.Errorf("GET after rejected upsert = %d, want 404 — a refused enrol must leave no row", code)
	}
}

// TestNULInOutboundStringsIsRejected covers the send path. subject was the
// confirmed 500 (req_e8636da9e69ea87cc227e85f); text, html and the attachment
// filename were ACCEPTED with 202 on staging, which is the inconsistency this
// rule removes. A caller cannot tell which of these lands in a text column, so
// all of them answer the same way.
func TestNULInOutboundStringsIsRejected(t *testing.T) {
	cases := []struct {
		name     string
		body     map[string]any
		location string
	}{
		{
			name:     "subject",
			body:     map[string]any{"to": []string{"alice@x.com"}, "subject": "nul\x00sub", "text": "clean"},
			location: "body.subject",
		},
		{
			name:     "text",
			body:     map[string]any{"to": []string{"alice@x.com"}, "subject": "clean", "text": "nul\x00body"},
			location: "body.text",
		},
		{
			name:     "html",
			body:     map[string]any{"to": []string{"alice@x.com"}, "subject": "clean", "text": "t", "html": "<p>a\x00b</p>"},
			location: "body.html",
		},
		{
			name: "attachment filename",
			body: map[string]any{"to": []string{"alice@x.com"}, "subject": "clean", "text": "t",
				"attachments": attField(map[string]any{
					"filename": "ok\x00.txt", "content_type": "text/plain", "data": base64.StdEncoding.EncodeToString([]byte("x")),
				})},
			location: "body.attachments[0].filename",
		},
		{
			name:     "recipient",
			body:     map[string]any{"to": []string{"al\x00ice@x.com"}, "subject": "clean", "text": "t"},
			location: "body.to[0]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			code, body := postJSON(t, srv.URL+sendURL, "good", tc.body)
			assertNULRejected(t, code, body, tc.location)
		})
	}
}

// TestNULInNonBodyParamsIsRejected pins that the rule is about client-supplied
// strings, not about the request body specifically. A NUL in a query filter
// reaches the same text comparison in Postgres that a body field would.
func TestNULInNonBodyParamsIsRejected(t *testing.T) {
	srv := newContactsServer(t, nil)

	code, body := sendJSON(t, http.MethodGet,
		srv.URL+"/v1/contacts?import_batch_id=%00imp", "account", nil)
	assertNULRejected(t, code, body, "query.import_batch_id")

	// Headers are covered by the same walk. Go's own HTTP client refuses to
	// transmit a header value containing a NUL, so this arm is asserted against
	// the bound input directly rather than over the wire — a hand-rolled client
	// or a raw socket can still send one.
	in := createContactInput{IdempotencyKey: "a\x00b"}
	bad := scanInputForNUL(reflect.ValueOf(&in))
	if bad == nil || bad.Location != "header.Idempotency-Key" {
		t.Fatalf("header violation = %+v; want header.Idempotency-Key", bad)
	}
	clean := createContactInput{IdempotencyKey: "idem-1"}
	if bad := scanInputForNUL(reflect.ValueOf(&clean)); bad != nil {
		t.Fatalf("clean header flagged at %s", bad.Location)
	}
}

// TestLongMapKeyInErrorLocationIsTruncated pins that the error envelope stays
// bounded when the offending location runs through a caller-controlled map
// key. The location is echoed into BOTH error.message and
// details.fields[0].location, and Huma binds the body before requirePrincipal
// runs — so without truncation a multi-megabyte metadata key comes back ~2x in
// the response, unauthenticated. Keys are capped at render time the same way
// truncateForError bounds them in validateContactMetadata messages.
func TestLongMapKeyInErrorLocationIsTruncated(t *testing.T) {
	srv := newContactsServer(t, nil)
	longKey := strings.Repeat("k", 64<<10)
	code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/contacts", "account", map[string]any{
		"address":  "long-key@example.com",
		"metadata": map[string]any{longKey: "a\x00b"},
	})
	assertNULRejected(t, code, body, "body.metadata."+strings.Repeat("k", 32)+"…")

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 1024 {
		t.Errorf("error response is %d bytes for a %d-byte key; the location must not reflect the key",
			len(raw), len(longKey))
	}
}

// TestNULRuleLeavesCleanRequestsAlone is the control: the guard must be a
// no-op for every request that does not carry a NUL, including ones with
// Unicode, other control characters, and deep nesting.
func TestNULRuleLeavesCleanRequestsAlone(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/contacts", "account", map[string]any{
		"address":      "clean@example.com",
		"display_name": "Zoë \t Partner — ünïcode ✉",
		"metadata":     map[string]any{"tier": "seed", "score": 3, "ok": true, "note": nil},
	})
	if code != http.StatusCreated {
		t.Fatalf("clean create = %d %v; want 201", code, body)
	}
}

// TestImportRejectsNULAtRequestLevel documents the one deliberate seam between
// the NUL rule and the import per-row isolation contract. Row CONTENT bounds
// (address/display_name length, metadata caps) fail their own row. A NUL is not
// a content bound — it is a byte the request document may not contain at all,
// the same class as malformed JSON — so it rejects the whole request. Stated
// once and applied everywhere beats a per-endpoint exception.
func TestImportRejectsNULAtRequestLevel(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := importBody(t, srv, map[string]any{"contacts": []any{
		map[string]any{"address": "good@imp.vc"},
		map[string]any{"address": "bad@imp.vc", "display_name": "a\x00b"},
	}})
	assertNULRejected(t, code, body, "body.contacts[1].display_name")
}

// TestScanValueForNULWalksEveryShape exercises the walker directly on shapes
// the HTTP tests cannot reach cheaply: nil pointers, byte slices (opaque
// payload, never a caller string), and time fields.
func TestScanValueForNULWalksEveryShape(t *testing.T) {
	type nested struct {
		Name  string            `json:"name"`
		Tags  []string          `json:"tags"`
		Attrs map[string]string `json:"attrs"`
	}
	type body struct {
		Ptr    *string  `json:"ptr"`
		Raw    []byte   `json:"raw"`
		Items  []nested `json:"items"`
		Ignore string   `json:"-"`
	}
	clean := "fine"
	dirty := "bad\x00"

	cases := []struct {
		name string
		in   body
		want string
	}{
		{"clean", body{Ptr: &clean, Raw: []byte{0, 1, 2}, Items: []nested{{Name: "ok"}}}, ""},
		{"nil pointer", body{}, ""},
		{"byte slice is not a string", body{Raw: []byte{0}}, ""},
		{"json:\"-\" is skipped", body{Ignore: dirty}, ""},
		{"pointer", body{Ptr: &dirty}, "body.ptr"},
		{"nested slice element", body{Items: []nested{{Name: "ok"}, {Name: dirty}}}, "body.items[1].name"},
		{"nested string slice", body{Items: []nested{{Tags: []string{"a", dirty}}}}, "body.items[0].tags[1]"},
		{"nested map value", body{Items: []nested{{Attrs: map[string]string{"k": dirty}}}}, "body.items[0].attrs.k"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scanner := nulScanner{path: []locSegment{{name: "body"}}}
			got := scanner.scanValue(reflect.ValueOf(tc.in))
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("want clean, got violation at %s", got.Location)
			case tc.want != "" && got == nil:
				t.Fatalf("want violation at %s, got clean", tc.want)
			case tc.want != "" && got.Location != tc.want:
				t.Fatalf("location = %q, want %q", got.Location, tc.want)
			}
		})
	}

	// A NUL in a map KEY reports the map, not the key, so raw bad input never
	// lands in the response.
	keyScanner := nulScanner{path: []locSegment{{name: "body"}}}
	got := keyScanner.scanValue(reflect.ValueOf(body{Items: []nested{{Attrs: map[string]string{dirty: "v"}}}}))
	if got == nil || got.Location != "body.items[0].attrs" || !strings.Contains(got.Message, "keys") {
		t.Fatalf("map-key violation = %+v; want the map location with a key-specific message", got)
	}
}

// humaRegistrationPattern matches every huma entry point that ends in a
// registered operation, in both call forms.
//
// The `[\[(]` alternation is the load-bearing part. Every one of these is a
// GENERIC function, so explicit type instantiation is valid Go —
// `huma.Post[In, Out](api, "/x", handler)` compiles, and the substring
// `huma.Post(` does not appear anywhere in it. A literal-substring guard
// therefore passes an operation that bypasses the NUL check entirely. (That
// hole predates this change: the original guard looked for `huma.Register(`
// and had it too.)
var humaRegistrationPattern = regexp.MustCompile(`huma\.(Register|AutoRegister|Get|Post|Put|Patch|Delete)\s*[\[(]`)

// TestEveryOperationGoesThroughRegisterOp is the guard that keeps the request
// -content rule global. registerOp is the only place the guard runs, so an
// operation wired past it would silently opt out — and the opt-out would be
// invisible until someone found the 500 in production, which is exactly how the
// five NUL vectors above were found.
//
// It covers every registration entry point huma exports, not just Register:
// Get/Post/Put/Patch/Delete are convenience wrappers that call Register
// internally, and AutoRegister reflects over a struct's methods to register
// whatever it finds. The walk is recursive, so a future sub-package is covered.
//
// LIMITS — this is a substring/regex guard over source text, not a type-aware
// analysis, so it is best-effort against deliberate obfuscation. It does NOT
// catch an aliased import (`h.Register(`), a dot-import (a bare `Register(`),
// or a sub-package that wraps huma itself and is called through that wrapper.
// Those were considered and left uncovered on purpose: closing them needs
// go/analysis, and the guard's job is to stop an ordinary mistake by someone
// who does not know registerOp exists, not to defeat someone routing around it.
func TestEveryOperationGoesThroughRegisterOp(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		if path == "request_content.go" {
			// The wrapper itself is the one legitimate caller.
			return nil
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, hit := range humaRegistrationPattern.FindAll(source, -1) {
			t.Errorf("%s calls %s directly; use registerOp so the operation inherits the shared request-content guards",
				path, strings.TrimRight(string(hit), "[( \t"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestHumaRegistrationPatternCatchesBothCallForms pins the regex itself. The
// bracket form is the one a literal-substring guard misses, so it is asserted
// explicitly and cannot regress.
func TestHumaRegistrationPatternCatchesBothCallForms(t *testing.T) {
	caught := []string{
		`huma.Register(s.API, huma.Operation{...}, s.handleX)`,
		`huma.Register[listInput, listOutput](s.API, huma.Operation{...}, s.handleX)`,
		`huma.Post(api, "/explicit-post", handler)`,
		`huma.Post[In, Out](api, "/explicit-post", handler)`,
		`huma.AutoRegister(api, server)`,
		`huma.AutoRegister[*Server](api, server)`,
		`huma.Get[In, Out](api, "/x", h)`,
		`huma.Put(api, "/x", h)`,
		`huma.Patch[In, Out](api, "/x", h)`,
		`huma.Delete(api, "/x", h)`,
		"huma.Register\t(api, op, h)", // gofmt would not write this, but a human might
	}
	for _, src := range caught {
		if !humaRegistrationPattern.MatchString(src) {
			t.Errorf("pattern missed a registration call: %s", src)
		}
	}

	// Things that merely mention huma must not trip the guard, or the test
	// becomes noise everyone learns to ignore.
	ignored := []string{
		`registerOp(s.API, huma.Operation{...}, s.handleX)`,
		`var op huma.Operation`,
		`func headerRef(name string) *huma.Param`,
		`humanErrorMessage(err)`,
		`s.API.OpenAPI()`,
		`huma.NewError(status, msg)`,
	}
	for _, src := range ignored {
		if humaRegistrationPattern.MatchString(src) {
			t.Errorf("pattern false-positived on: %s", src)
		}
	}
}

// BenchmarkScanCleanImportBody measures the guard on the shape it must never
// slow down: a clean 1000-row contact import, the largest body the API accepts
// by row count. Every location string the walk could build is thrown away on a
// clean request, which is why nulScanner defers rendering.
func BenchmarkScanCleanImportBody(b *testing.B) {
	rows := make([]ContactImportRow, 1000)
	for i := range rows {
		name := fmt.Sprintf("Partner %d", i)
		rows[i] = ContactImportRow{
			Address:     fmt.Sprintf("partner%d@fund.vc", i),
			DisplayName: &name,
			Metadata:    map[string]any{"tier": "seed", "score": float64(i)},
		}
	}
	in := importContactsInput{
		IdempotencyKey: "contacts:upload:sha256",
		Body:           ImportContactsRequest{Contacts: rows, OnConflict: "merge"},
	}
	value := reflect.ValueOf(&in)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if bad := scanInputForNUL(value); bad != nil {
			b.Fatalf("clean body flagged at %s", bad.Location)
		}
	}
}
