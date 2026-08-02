package identity

import (
	"context"
	"testing"
	"unicode/utf8"
)

// TestUpdateAgentInboundPolicyInvalid verifies the policy is validated before
// any DB access (the inboundpolicy.Valid short-circuit), so a bogus policy
// returns an error without a pool. Mirrors the gate the API maps to 400.
func TestUpdateAgentInboundPolicyInvalid(t *testing.T) {
	s := &Store{} // nil pool: invalid policy must return before touching it
	err := s.UpdateAgentInboundPolicy(context.Background(), "bot@acme.com", "u_1", "bogus", nil)
	if err == nil {
		t.Fatal("expected error for invalid inbound_policy, got nil")
	}
}

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"already lowercase", "alice@example.com", "alice@example.com"},
		{"mixed case", "Alice@Example.COM", "alice@example.com"},
		{"all uppercase", "ALICE@EXAMPLE.COM", "alice@example.com"},
		{"leading whitespace", "  alice@example.com", "alice@example.com"},
		{"trailing whitespace", "alice@example.com  ", "alice@example.com"},
		{"surrounding whitespace + case", "  Alice@Example.COM  ", "alice@example.com"},
		{"tab whitespace", "\talice@example.com\t", "alice@example.com"},
		{"local-part with plus", "Alice+Filter@Example.com", "alice+filter@example.com"},
		// Inner whitespace is NOT a valid email anyway; we just confirm we
		// don't strip inside the address — the validator catches it later.
		{"inner space preserved", "alice @example.com", "alice @example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeEmail(tt.in); got != tt.want {
				t.Errorf("NormalizeEmail(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNormalizeEmailIdempotent guards against silent regressions where a
// future change adds non-idempotent behavior (e.g. URL-decoding, IDN
// punycode conversion) without updating the contract: calling
// NormalizeEmail on its own output must return the same string.
func TestNormalizeEmailIdempotent(t *testing.T) {
	for _, in := range []string{
		"alice@example.com",
		"Alice@Example.COM",
		"  Alice@Example.COM  ",
		"",
	} {
		once := NormalizeEmail(in)
		twice := NormalizeEmail(once)
		if once != twice {
			t.Errorf("not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}
}

// TestNormalizeEmailOutputIsAlwaysValidUTF8 is load-bearing for the two raw
// chi `/v1` routes that sit outside BOTH request-content seams in
// internal/httpapi (the WebSocket handshake `/v1/agents/{email}/ws` and the
// attachment download `/v1/agents/{email}/messages/{id}/attachments/{index}/download`).
// Neither is a Huma operation, so neither the raw-body format guard nor the
// registerOp walk covers its `{email}` path param — a percent-encoded raw byte
// reaches the handler intact. What keeps that byte out of Postgres (where it
// would be SQLSTATE 22021) is this function: strings.ToLower routes non-ASCII
// input through strings.Map, which decodes ill-formed bytes as U+FFFD and
// re-encodes them, so the lookup key is always well-formed and simply misses.
//
// That makes both routes safe WITHOUT their own guard — but only for as long
// as this holds. A plausible "optimization" (a byte-wise ASCII lowercase fast
// path) would pass those bytes straight through and silently reopen the class
// on two routes with no other line of defense, so the property is pinned here
// rather than left as a comment.
func TestNormalizeEmailOutputIsAlwaysValidUTF8(t *testing.T) {
	for _, in := range []string{
		"a\xffb@x.dev",
		"A\xffB@X.dev",
		"\xff",
		"\xc3\x28@x.dev", // truncated two-byte sequence
		"\xed\xa0\x80",   // encoded surrogate half
		"ok@x.dev",
		"日本語@x.dev",
	} {
		out := NormalizeEmail(in)
		if !utf8.ValidString(out) {
			t.Errorf("NormalizeEmail(%q) = %q, which is not valid UTF-8 — the raw chi /v1 routes rely on this", in, out)
		}
	}
}
