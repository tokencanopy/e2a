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
		RawBodyBase64: base64.StdEncoding.EncodeToString([]byte(rawInvalidUTF8Body)),
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
		RawBodyBase64: base64.StdEncoding.EncodeToString([]byte("{}")),
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want a mutual-exclusion error", err)
	}
}

func TestRunnerRejectsMalformedRawBodyBase64(t *testing.T) {
	if _, err := stepRawBody(&step{ID: "bad", RawBodyBase64: "not base64!!"}); err == nil {
		t.Fatal("malformed raw_body_base64 accepted; want a decode error")
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
