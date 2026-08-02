package httpapi

import (
	"bytes"
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

// Regression suite for the request-content rules: NUL (U+0000) and invalid
// UTF-8.
//
// Every NUL case below returned HTTP 500 on the live staging build
// sha-32ce45dc: Postgres cannot store a NUL in a text column, so a caller
// string carrying one reached the driver and came back as internal_error. A
// permanent client error dressed as a 5xx is the worst possible answer — it is
// exactly what SDK retry logic hammers — so the rule is now enforced at the
// edge for every operation. The invalid-UTF-8 cases (further down) are the
// same bug class, found by Schemathesis against staging: 22021 500s on the
// header and path-param routes, and — worse — silent U+FFFD corruption on the
// body route, because encoding/json launders invalid bytes instead of
// erroring.
//
// The rules: no client-supplied string anywhere in a request (body at any
// depth, including object KEYS, plus path, query, and header params) may
// contain U+0000, and every client-supplied byte sequence must be well-formed
// UTF-8. Violations are 400 invalid_request with the offending location.

// violationLocation pulls the single field location out of a validation envelope.
func violationLocation(t *testing.T, body map[string]any) string {
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

func assertContentRejected(t *testing.T, code int, body map[string]any, wantLocation string) {
	t.Helper()
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (a NUL or invalid UTF-8 is a permanent client error, never a 5xx); body=%v", code, body)
	}
	if errCode(body) != "invalid_request" {
		t.Fatalf("error code = %q, want invalid_request; body=%v", errCode(body), body)
	}
	if got := violationLocation(t, body); got != wantLocation {
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
			assertContentRejected(t, code, body, tc.location)

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
	assertContentRejected(t, code, body, "body.stage")

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
			assertContentRejected(t, code, body, tc.location)
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
	assertContentRejected(t, code, body, "query.import_batch_id")

	// Headers are covered by the same walk. Go's own HTTP client refuses to
	// transmit a header value containing a NUL, so this arm is asserted against
	// the bound input directly rather than over the wire — a hand-rolled client
	// or a raw socket can still send one.
	in := createContactInput{IdempotencyKey: "a\x00b"}
	bad := scanInput(reflect.ValueOf(&in))
	if bad == nil || bad.Location != "header.Idempotency-Key" {
		t.Fatalf("header violation = %+v; want header.Idempotency-Key", bad)
	}
	clean := createContactInput{IdempotencyKey: "idem-1"}
	if bad := scanInput(reflect.ValueOf(&clean)); bad != nil {
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
	assertContentRejected(t, code, body, "body.metadata."+strings.Repeat("k", 32)+"…")

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
	assertContentRejected(t, code, body, "body.contacts[1].display_name")
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
			scanner := contentScanner{path: []locSegment{{name: "body"}}}
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
	keyScanner := contentScanner{path: []locSegment{{name: "body"}}}
	got := keyScanner.scanValue(reflect.ValueOf(body{Items: []nested{{Attrs: map[string]string{dirty: "v"}}}}))
	if got == nil || got.Location != "body.items[0].attrs" || !strings.Contains(got.Message, "keys") {
		t.Fatalf("map-key violation = %+v; want the map location with a key-specific message", got)
	}
}

// sendRawBody posts raw, unmarshaled bytes. The invalid-UTF-8 body cases need
// it: json.Marshal would launder the invalid bytes into U+FFFD on the way OUT
// exactly the way json.Unmarshal does on the way IN, so a map-based helper can
// never put a broken byte sequence on the wire.
func sendRawBody(t *testing.T, method, url, bearer string, raw []byte) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestInvalidUTF8InBodyIsRejectedNotLaundered is the silent-corruption
// reproduction (Schemathesis vs staging): POST /v1/contacts with a raw 0xFF
// byte inside the address returned 201 and stored the address with U+FFFD in
// place of the byte — json.Unmarshal replaces invalid UTF-8 instead of
// erroring, so the post-bind walk alone can never see it. The raw-bytes format
// guard (requireUTF8Body) must reject it before decoding.
func TestInvalidUTF8InBodyIsRejectedNotLaundered(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := sendRawBody(t, http.MethodPost, srv.URL+"/v1/contacts", "account",
		[]byte("{\"address\":\"a\xffb@example.com\"}"))
	assertContentRejected(t, code, body, "body")

	// The response must not echo the offending body back (huma's ErrorDetail
	// carries a Value field with the raw body; the envelope must drop it).
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "example.com") {
		t.Errorf("error response echoes the request body: %s", raw)
	}

	// Nothing may be stored — neither the raw address nor its U+FFFD-laundered
	// double, which is what the pre-fix 201 persisted.
	code, _ = sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts/"+urlEsc("a�b@example.com"), "account", nil)
	if code != http.StatusNotFound {
		t.Errorf("GET of the laundered address = %d, want 404 — a refused create must leave no row", code)
	}
}

// TestInvalidUTF8InHeaderIsRejected is the header reproduction: POST
// /v1/contacts with `Idempotency-Key: k\xffz` returned 500 "idempotency store
// error" — Postgres rejects the invalid byte sequence in the key column
// (SQLSTATE 22021). Unlike a NUL, Go's HTTP client transmits 0xFF in a header
// value (it is legal obs-text), so this arm runs over the wire.
func TestInvalidUTF8InHeaderIsRejected(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body, _ := sendJSONFull(t, http.MethodPost, srv.URL+"/v1/contacts", "account",
		map[string]any{"address": "hdr@example.com"},
		map[string]string{"Idempotency-Key": "k\xffz"})
	assertContentRejected(t, code, body, "header.Idempotency-Key")

	code, _ = sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts/"+urlEsc("hdr@example.com"), "account", nil)
	if code != http.StatusNotFound {
		t.Errorf("GET after rejected create = %d, want 404 — a refused request must leave no row", code)
	}
}

// TestInvalidUTF8InPathParamIsRejected is the path-param reproduction: POST
// /v1/webhooks/%FF/rotate-secret returned 500 "failed to rotate webhook
// secret" (the raw byte reached the store's UPDATE), while the read path
// happened to answer 404. Both now answer 400 at the edge, uniformly.
func TestInvalidUTF8InPathParamIsRejected(t *testing.T) {
	srv := testServer(t)
	code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/webhooks/%FF/rotate-secret", "good", nil)
	assertContentRejected(t, code, body, "path.id")

	// The read path previously masked the same defect as a 404; the blanket
	// rule makes it the same 400 a client can actually act on.
	code, body = sendJSON(t, http.MethodGet, srv.URL+"/v1/webhooks/%FF", "good", nil)
	assertContentRejected(t, code, body, "path.id")
}

// TestInvalidUTF8InQueryParamIsRejected pins that query params share the rule:
// the same raw byte in a query filter reaches the same text comparison in
// Postgres that the header and path params did.
func TestInvalidUTF8InQueryParamIsRejected(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := sendJSON(t, http.MethodGet,
		srv.URL+"/v1/contacts?import_batch_id=%FF", "account", nil)
	assertContentRejected(t, code, body, "query.import_batch_id")
}

// TestImportRejectsInvalidUTF8AtRequestLevel mirrors the NUL seam test: like a
// NUL, a broken byte sequence is not row CONTENT — the request document may
// not contain it at all (RFC 8259 §8.1) — so it rejects the whole import
// rather than failing one row.
func TestImportRejectsInvalidUTF8AtRequestLevel(t *testing.T) {
	srv := newContactsServer(t, nil)
	code, body := sendRawBody(t, http.MethodPost, srv.URL+"/v1/contacts/import", "account",
		[]byte("{\"contacts\":[{\"address\":\"good@imp.vc\"},{\"address\":\"b\xffd@imp.vc\"}]}"))
	assertContentRejected(t, code, body, "body")
}

// TestValidMultiByteUTF8IsAccepted is the negative control the rule lives or
// dies by: rejecting invalid byte SEQUENCES must not reject a single byte of
// legitimate international text. Multi-byte CJK, a symbol, an emoji (a
// surrogate-pair character in JSON's \u escape world), and a properly ENCODED
// U+FFFD — a client is allowed to send the replacement character itself; only
// raw invalid bytes are refused.
func TestValidMultiByteUTF8IsAccepted(t *testing.T) {
	cases := []struct {
		name        string
		displayName string
	}{
		{"cjk + symbol + emoji", "日本語 ✉ 😀"},
		{"encoded U+FFFD is legal", "a�b"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newContactsServer(t, nil)
			address := fmt.Sprintf("intl%d@example.com", i)
			code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/contacts", "account",
				map[string]any{"address": address, "display_name": tc.displayName})
			if code != http.StatusCreated {
				t.Fatalf("create with %s = %d %v; want 201", tc.name, code, body)
			}
			// Round-trip intact: stored and returned byte-for-byte.
			code, got := sendJSON(t, http.MethodGet, srv.URL+"/v1/contacts/"+urlEsc(address), "account", nil)
			if code != http.StatusOK || got["display_name"] != tc.displayName {
				t.Fatalf("round-trip = %d %v; want 200 with display_name %q", code, got, tc.displayName)
			}
		})
	}

	// Header arm of the control: a valid multi-byte header value passes the
	// walk (asserted on the bound input — sending non-ASCII header bytes over
	// the wire is legal but exotic, and the walk is where the decision lives).
	in := createContactInput{IdempotencyKey: "clé-日本語-🔑"}
	if bad := scanInput(reflect.ValueOf(&in)); bad != nil {
		t.Fatalf("valid multi-byte header flagged at %s", bad.Location)
	}
}

// TestScanValueRejectsInvalidUTF8Shapes exercises the walker's UTF-8 arm
// directly on the shapes HTTP cannot reach (JSON decoding launders body
// strings, so only non-body params hit this arm in production — but the walk
// is shape-generic and must stay that way).
func TestScanValueRejectsInvalidUTF8Shapes(t *testing.T) {
	type body struct {
		Name  string            `json:"name"`
		Raw   []byte            `json:"raw"`
		Attrs map[string]string `json:"attrs"`
	}
	dirty := "bad\xff"

	scanner := contentScanner{path: []locSegment{{name: "body"}}}
	if got := scanner.scanValue(reflect.ValueOf(body{Name: dirty})); got == nil ||
		got.Location != "body.name" || got.Message != utf8ValueMessage {
		t.Fatalf("invalid UTF-8 string = %+v; want body.name %q", got, utf8ValueMessage)
	}

	// A byte slice stays opaque: raw payload bytes are not caller strings.
	scanner = contentScanner{path: []locSegment{{name: "body"}}}
	if got := scanner.scanValue(reflect.ValueOf(body{Raw: []byte{0xff}})); got != nil {
		t.Fatalf("opaque byte slice flagged at %s", got.Location)
	}

	// An invalid map KEY reports the map, never echoing the key.
	scanner = contentScanner{path: []locSegment{{name: "body"}}}
	got := scanner.scanValue(reflect.ValueOf(body{Attrs: map[string]string{dirty: "v"}}))
	if got == nil || got.Location != "body.attrs" || got.Message != utf8KeyMessage {
		t.Fatalf("map-key violation = %+v; want body.attrs %q", got, utf8KeyMessage)
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
var humaRegistrationPattern = regexp.MustCompile(`huma\.(Register|AutoRegister|Get|Head|Post|Put|Patch|Delete)\s*[\[(]`)

// TestEveryOperationGoesThroughRegisterOp is the guard that keeps the request
// -content rule global. registerOp is the only place the guard runs, so an
// operation wired past it would silently opt out — and the opt-out would be
// invisible until someone found the 500 in production, which is exactly how the
// five NUL vectors above were found.
//
// It covers every registration entry point huma exports, not just Register:
// Get/Head/Post/Put/Patch/Delete are convenience wrappers that call Register
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
// One more known limit: the walk starts at ".", the package directory under
// `go test`, so only internal/httpapi and its sub-packages are covered — a
// registration in a different package tree would be invisible. No such
// registration exists today (repo-wide grep confirms), and every handler lives
// here by construction.
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
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		hits := humaRegistrationPattern.FindAll(source, -1)
		if path == "request_content.go" {
			// The wrapper itself is the one legitimate caller: exactly the
			// single huma.Register call inside registerOp. Anything beyond
			// that one call — a second registration, or a convenience wrapper
			// like huma.Post — is a bypass even in this file, so the file is
			// checked rather than skipped wholesale.
			if len(hits) == 1 && strings.HasPrefix(string(hits[0]), "huma.Register") {
				return nil
			}
			t.Errorf("request_content.go must contain exactly the one huma.Register call inside registerOp; found %d registration calls", len(hits))
			return nil
		}
		for _, hit := range hits {
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
		`huma.Head(api, "/x", h)`,
		`huma.Head[In, Out](api, "/x", h)`,
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
// clean request, which is why contentScanner defers rendering.
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
		if bad := scanInput(value); bad != nil {
			b.Fatalf("clean body flagged at %s", bad.Location)
		}
	}
}
