//go:build integration

package contract

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// Runner-level regressions for the raw-bytes escape hatches
// (raw_body_base64 / headers_base64). They exist because the scenario schema
// otherwise cannot express a request that is not well-formed UTF-8: YAML is
// UTF-8 by definition and its \xNN escape denotes a CODEPOINT (\xFF is U+00FF,
// which encodes to the perfectly valid two-byte C3 BF). Parity tests in the
// TypeScript and Python runners assert the same two properties.

const (
	rawInvalidUTF8Body   = "{\"address\":\"a\xffb@example.com\"}"
	rawInvalidUTF8Header = "k\xffz"
)

// TestRunnerSendsRawBytesVerbatim is the property the whole extension rests
// on: whatever the base64 decodes to must arrive at the server BYTE-FOR-BYTE.
// A real HTTP round trip (not a stubbed RoundTripper) is deliberate — the
// bytes have to survive Go's request writer and the server's parser, which is
// exactly the seam a JSON encoder or a charset conversion would corrupt.
func TestRunnerSendsRawBytesVerbatim(t *testing.T) {
	var gotBody []byte
	var gotHeader, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeader = r.Header.Get("Idempotency-Key")
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request"}}`))
	}))
	defer srv.Close()

	r := newRunner(&testEnv{baseURL: srv.URL, apiKey: "key"}, scenario{})
	err := r.execRequestError(&step{
		ID:            "raw",
		Action:        "request",
		Method:        http.MethodPost,
		Path:          "/v1/contacts",
		RawBodyBase64: b64(rawInvalidUTF8Body),
		HeadersBase64: map[string]string{
			"Idempotency-Key": base64.StdEncoding.EncodeToString([]byte(rawInvalidUTF8Header)),
		},
		Expect: &expectation{
			Status:    http.StatusBadRequest,
			BodyMatch: map[string]interface{}{"error.code": "invalid_request"},
		},
	})
	if err != nil {
		t.Fatalf("raw-bytes step: %v", err)
	}

	if !bytes.Equal(gotBody, []byte(rawInvalidUTF8Body)) {
		t.Errorf("body on the wire = %q, want %q (raw_body_base64 must not be re-encoded)", gotBody, rawInvalidUTF8Body)
	}
	if gotHeader != rawInvalidUTF8Header {
		t.Errorf("Idempotency-Key on the wire = %q, want %q", gotHeader, rawInvalidUTF8Header)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json for a raw body", gotContentType)
	}
}

// TestRunnerRejectsBothBodySources: a step declaring two body sources is a
// scenario-authoring bug, and silently preferring one would make the other
// vacuous.
func TestRunnerRejectsBothBodySources(t *testing.T) {
	_, err := stepRawBody(&step{
		ID:            "both",
		Body:          map[string]interface{}{"address": "a@example.com"},
		RawBodyBase64: b64("{}"),
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want a mutual-exclusion error", err)
	}
}

// TestRunnerRejectsBadRawBodyBase64 pins the two ways a raw_body_base64 value
// can be wrong, and is the Go arm of a three-runner parity contract: a fixture
// that is not exactly-canonical base64, or is present but empty, must FAIL
// LOUDLY in every language. The failure mode this guards against is worse than
// a wrong test — a permissive decoder (Node's Buffer.from(x, "base64") drops
// non-alphabet characters instead of erroring) makes a typo'd fixture send
// DIFFERENT BYTES in one runner while the other two report a clean error, so
// the same scenario silently tests something else.
func TestRunnerRejectsBadRawBodyBase64(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"non-alphabet characters", "not base64!!"},
		{"missing padding", "eyJhIjoxfQ"},
		{"embedded whitespace", "eyJhIjox\nfQ=="},
		{"present but empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := stepRawBody(&step{ID: "bad", RawBodyBase64: &tc.value}); err == nil {
				t.Fatalf("raw_body_base64 %q accepted; want an explicit error", tc.value)
			}
		})
	}

	// Control: absent is not an error — it simply means "no body".
	raw, err := stepRawBody(&step{ID: "none"})
	if err != nil || raw != nil {
		t.Fatalf("absent raw_body_base64 = (%v, %v); want (nil, nil)", raw, err)
	}
}

// TestRunnerRejectsBadHeadersBase64 is the same contract for the header
// escape hatch, which shares the decode path's strictness requirement.
func TestRunnerRejectsBadHeadersBase64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	r := newRunner(&testEnv{baseURL: srv.URL, apiKey: "key"}, scenario{})
	err := r.execRequestError(&step{
		ID: "bad-header", Action: "request", Method: http.MethodGet, Path: "/v1/contacts",
		HeadersBase64: map[string]string{"Idempotency-Key": "not base64!!"},
	})
	if err == nil || !strings.Contains(err.Error(), "headers_base64") {
		t.Fatalf("err = %v, want a headers_base64 decode error", err)
	}
}

// TestRunnerResponseExcludesScansWholeBody pins that response_excludes is a
// RAW-TEXT check over the entire response, not the top-level property-name
// check body_excludes performs. The distinction is the whole reason the key
// exists: the leak it must catch — a server echoing the offending input back
// inside an error envelope — lives at a nested path, where body_excludes is
// vacuously satisfied.
func TestRunnerResponseExcludesScansWholeBody(t *testing.T) {
	const leak = `{"error":{"code":"invalid_request","details":{"fields":[{"location":"body","value":"a�b@example.com"}]}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(leak))
	}))
	defer srv.Close()
	r := newRunner(&testEnv{baseURL: srv.URL, apiKey: "key"}, scenario{})

	base := func(ex *expectation) *step {
		return &step{ID: "leak", Action: "request", Method: http.MethodGet, Path: "/v1/contacts", Expect: ex}
	}

	// body_excludes passes on the leaking response — the weakness being fixed.
	if err := r.execRequestError(base(&expectation{
		Status: http.StatusBadRequest, BodyExcludes: []string{"address"},
	})); err != nil {
		t.Fatalf("body_excludes unexpectedly failed: %v", err)
	}
	// response_excludes catches it.
	if err := r.execRequestError(base(&expectation{
		Status: http.StatusBadRequest, ResponseExcludes: []string{"example.com"},
	})); err == nil {
		t.Fatal("response_excludes passed on a response that echoes the offending value")
	}
	// And it is not trigger-happy: a substring that really is absent passes,
	// including on a step making no other body claim.
	if err := r.execRequestError(base(&expectation{
		Status: http.StatusBadRequest, ResponseExcludes: []string{"not-in-the-response"},
	})); err != nil {
		t.Fatalf("response_excludes false positive: %v", err)
	}
}

// TestInvalidUTF8ScenarioCarriesTrulyInvalidBytes is the non-vacuity guard for
// the shared scenario: if someone "fixes" the fixtures into valid UTF-8, the
// 400s would still arrive (for a different reason) and the scenario would
// quietly stop testing the rule. Assert the parsed bytes really are ill-formed.
func TestInvalidUTF8ScenarioCarriesTrulyInvalidBytes(t *testing.T) {
	var sc *scenario
	for _, candidate := range loadScenarios(t) {
		if candidate.Name == "invalid_utf8_rejected" {
			c := candidate
			sc = &c
			break
		}
	}
	if sc == nil {
		t.Fatal("scenario invalid_utf8_rejected not found")
	}

	steps := make(map[string]step, len(sc.Steps))
	for _, s := range sc.Steps {
		steps[s.ID] = s
	}

	body, err := stepRawBody(ptr(steps["body_rejected"]))
	if err != nil {
		t.Fatalf("body_rejected raw_body_base64: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("body_rejected declares no raw_body_base64 — the body arm is unexercised")
	}
	// The offending-bytes echo check must be the whole-response form. A
	// top-level body_excludes here would be vacuous on an error envelope.
	if len(steps["body_rejected"].Expect.ResponseExcludes) == 0 {
		t.Error("body_rejected declares no response_excludes — nothing pins that the server does not echo the offending bytes")
	}
	if utf8.Valid(body) {
		t.Errorf("body_rejected payload is valid UTF-8 (%q) — it cannot exercise the rule", body)
	}

	header := steps["header_rejected"].HeadersBase64["Idempotency-Key"]
	if header == "" {
		t.Fatal("header_rejected declares no headers_base64 Idempotency-Key")
	}
	value, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		t.Fatalf("header_rejected headers_base64: %v", err)
	}
	if utf8.Valid(value) {
		t.Errorf("header_rejected value is valid UTF-8 (%q) — it cannot exercise the rule", value)
	}

	// The scenario must stay self-cleaning by construction: nothing is created,
	// so nothing may need deleting.
	if len(sc.Cleanup) != 0 {
		t.Errorf("invalid_utf8_rejected declares cleanup steps; every request is a rejection, so nothing exists to clean")
	}
	for _, s := range sc.Steps {
		if s.Expect == nil || (s.Expect.Status != http.StatusBadRequest && s.Expect.Status != http.StatusNotFound) {
			t.Errorf("step %s expects status %d; every step must be a rejection (400) or a not-created probe (404)", s.ID, s.Expect.Status)
		}
	}
}

func ptr(s step) *step { return &s }

// b64 returns a *string so a step can distinguish an absent raw_body_base64
// from an empty one.
func b64(s string) *string {
	encoded := base64.StdEncoding.EncodeToString([]byte(s))
	return &encoded
}
