package delegated

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The dual code-point/byte limits for every configured string claim, so a
// single parametric matrix covers iss/aud/azp/scope-as-string/sub/jti and
// the required context claims (workspace_id, membership_id, workspace_role
// all share the subject bound). Every pair here has maxBytes == 4*maxCP,
// which is exactly the largest a maxCP-code-point string can encode
// (4-byte supplementary runes) — so the code-point rule is the binding
// one and a byte-only shorthand would wrongly accept far too many code
// points.
var claimLimitPairs = []struct {
	name          string
	maxCodePoints int
	maxBytes      int
}{
	{"iss", MaxIssuerCodePoints, MaxIssuerBytes},               // 512 / 2048
	{"aud", MaxAudienceCodePoints, MaxAudienceBytes},           // 256 / 1024
	{"azp", MaxAzpCodePoints, MaxAzpBytes},                     // 128 / 512
	{"sub/jti/context", MaxSubjectCodePoints, MaxSubjectBytes}, // 128 / 512
}

func TestBoundedUTF8DualLimits(t *testing.T) {
	ascii := func(n int) string { return strings.Repeat("a", n) }
	twoByte := func(n int) string { return strings.Repeat("é", n) }       // U+00E9, 2 bytes
	supp := func(n int) string { return strings.Repeat("\U00020000", n) } // 4 bytes, 1 code point

	for _, p := range claimLimitPairs {
		t.Run(p.name, func(t *testing.T) {
			// Empty is never valid.
			if boundedUTF8("", p.maxCodePoints, p.maxBytes) {
				t.Fatal("empty must be rejected")
			}
			// ASCII at the code-point limit accepts; one more rejects.
			if !boundedUTF8(ascii(p.maxCodePoints), p.maxCodePoints, p.maxBytes) {
				t.Fatalf("%d ASCII code points must be accepted", p.maxCodePoints)
			}
			if boundedUTF8(ascii(p.maxCodePoints+1), p.maxCodePoints, p.maxBytes) {
				t.Fatalf("%d ASCII code points must be rejected", p.maxCodePoints+1)
			}
			// Multibyte (2-byte) at the code-point limit accepts (bytes still
			// within maxBytes); one more rejects on the code-point rule even
			// though bytes remain under the byte cap.
			if !boundedUTF8(twoByte(p.maxCodePoints), p.maxCodePoints, p.maxBytes) {
				t.Fatalf("%d two-byte code points must be accepted", p.maxCodePoints)
			}
			if boundedUTF8(twoByte(p.maxCodePoints+1), p.maxCodePoints, p.maxBytes) {
				t.Fatalf("%d two-byte code points must be rejected (code-point rule)", p.maxCodePoints+1)
			}
			// Supplementary-plane (4-byte) at the code-point limit sits at the
			// byte limit too (4*maxCP == maxBytes): accepted; one more rejects.
			if !boundedUTF8(supp(p.maxCodePoints), p.maxCodePoints, p.maxBytes) {
				t.Fatalf("%d supplementary code points must be accepted (== %d bytes)", p.maxCodePoints, p.maxBytes)
			}
			if boundedUTF8(supp(p.maxCodePoints+1), p.maxCodePoints, p.maxBytes) {
				t.Fatalf("%d supplementary code points must be rejected", p.maxCodePoints+1)
			}
			// Byte-only-shorthand foil: maxBytes ASCII chars is within the byte
			// cap but is maxBytes code points (> maxCP), so the code-point rule
			// must reject it — proving byte-only can't replace code-point.
			if boundedUTF8(ascii(p.maxBytes), p.maxCodePoints, p.maxBytes) {
				t.Fatalf("%d ASCII code points (at the byte cap) must be rejected by the code-point rule", p.maxBytes)
			}
		})
	}
}

func TestBoundedASCIILimits(t *testing.T) {
	// scope and kid both use boundedASCII with a byte cap.
	for _, tc := range []struct {
		name string
		max  int
	}{
		{"scope", MaxScopeBytes}, // 128
		{"kid", MaxKidBytes},     // 128
	} {
		t.Run(tc.name, func(t *testing.T) {
			if boundedASCII("", tc.max) {
				t.Fatal("empty must be rejected")
			}
			if !boundedASCII(strings.Repeat("a", tc.max), tc.max) {
				t.Fatalf("%d ASCII bytes must be accepted", tc.max)
			}
			if boundedASCII(strings.Repeat("a", tc.max+1), tc.max) {
				t.Fatalf("%d ASCII bytes must be rejected", tc.max+1)
			}
			if boundedASCII("a"+"é", tc.max) { // non-ASCII
				t.Fatal("non-ASCII must be rejected")
			}
			if boundedASCII("a\x01b", tc.max) { // C0 control
				t.Fatal("control character must be rejected")
			}
			if boundedASCII("a\x7fb", tc.max) { // DEL
				t.Fatal("DEL must be rejected")
			}
		})
	}
}

func TestDecodeSegmentBoundaries(t *testing.T) {
	enc := func(n int) string {
		return base64.RawURLEncoding.EncodeToString(make([]byte, n))
	}
	for _, tc := range []struct {
		name string
		max  int
	}{
		{"protected header", MaxProtectedHeaderBytes}, // 1024
		{"payload", MaxPayloadBytes},                  // 8192
		{"signature", MaxSignatureBytes},              // 1024
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := decodeSegment(enc(tc.max), tc.max); !ok {
				t.Fatalf("a %d-byte decoded segment must be accepted", tc.max)
			}
			if _, ok := decodeSegment(enc(tc.max+1), tc.max); ok {
				t.Fatalf("a %d-byte decoded segment must be rejected", tc.max+1)
			}
		})
	}
}
