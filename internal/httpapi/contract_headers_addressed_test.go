package httpapi

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// TestETagAndLocationHeadersAreDescribed pins that every response promising an
// ETag or a Location describes the convention a client needs to use it. Both
// were previously bare `type: string`, which told a client the header exists
// and nothing about how to compare or replay it.
func TestETagAndLocationHeadersAreDescribed(t *testing.T) {
	raw, err := json.Marshal(New(Deps{}).API.OpenAPI())
	if err != nil {
		t.Fatalf("render OpenAPI: %v", err)
	}
	var doc struct {
		Components struct {
			Headers map[string]map[string]any `json:"headers"`
		} `json:"components"`
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Responses   map[string]struct {
				Headers map[string]map[string]any `json:"headers"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}

	for name, want := range map[string][]string{
		// The ETag description must say opaque, strong, and replay-verbatim.
		"ETag": {"opaque", "STRONG", "VERBATIM", "If-Match", "412"},
		// The Location description must warn that '@' may be raw and that the
		// comparison is decode-then-compare, which is what our own conformance
		// suite had to do because neither form was promised.
		"ResourceLocation": {"percent-encoded", "unescaped", "percent-decode the final path segment"},
	} {
		header, ok := doc.Components.Headers[name]
		if !ok {
			t.Errorf("components.headers.%s is missing", name)
			continue
		}
		description, _ := header["description"].(string)
		for _, fragment := range want {
			if !strings.Contains(description, fragment) {
				t.Errorf("components.headers.%s description does not mention %q", name, fragment)
			}
		}
	}

	found := map[string]int{}
	for path, methods := range doc.Paths {
		for method, op := range methods {
			for status, response := range op.Responses {
				for header, value := range response.Headers {
					if header != "ETag" && header != "Location" {
						continue
					}
					found[header]++
					ref, _ := value["$ref"].(string)
					want := "#/components/headers/ETag"
					if header == "Location" {
						want = "#/components/headers/ResourceLocation"
					}
					if ref != want {
						t.Errorf("%s %s %s header %s = %#v, want $ref %s", method, path, status, header, value, want)
					}
				}
			}
		}
	}
	// Guard the guard: if the enrichment pass silently stopped matching, the
	// loop above would pass vacuously.
	if found["ETag"] == 0 || found["Location"] == 0 {
		t.Fatalf("no ETag/Location response headers found (%v) — the header contract pass is not reaching them", found)
	}
}

// TestLocationEncodingDescriptionMatchesTheImplementation keeps the published
// convention honest: the description promises that percent-DECODING the final
// path segment recovers the canonical address and that '@' may be raw. Both are
// properties of url.PathEscape, which is what the handlers use, so assert them
// against the real function rather than trusting the prose.
func TestLocationEncodingDescriptionMatchesTheImplementation(t *testing.T) {
	for _, address := range []string{
		"a.partner@fund.vc",
		"a+tag@fund.vc",
		"o'brien@fund.vc",
		"a/b@fund.vc",
		"üser@fünd.vc",
	} {
		escaped := url.PathEscape(address)
		decoded, err := url.PathUnescape(escaped)
		if err != nil || decoded != address {
			t.Errorf("PathEscape(%q) = %q does not percent-decode back to the address (%q, %v)", address, escaped, decoded, err)
		}
	}
	// The specific claim a client will act on: '@' survives unescaped, so a
	// naive comparison against a locally %40-encoded URL would not match.
	if got := url.PathEscape("a.partner@fund.vc"); !strings.Contains(got, "@") {
		t.Errorf("PathEscape left no raw '@' (%q) — the published Location convention would be wrong", got)
	}
}
