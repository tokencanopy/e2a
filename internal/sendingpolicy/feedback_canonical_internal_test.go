package sendingpolicy

import "testing"

// TestCanonicalRecipient pins the one HMAC subject both sides of the
// provider compute: IDNA-ASCII domain, lower-cased, so the Unicode spelling
// authorization accepts and the A-label spelling a provider reports agree.
func TestCanonicalRecipient(t *testing.T) {
	for in, want := range map[string]string{
		"Leser@Bücher.Example":        "leser@xn--bcher-kva.example",
		"leser@xn--bcher-kva.example": "leser@xn--bcher-kva.example",
		" Plain@Example.TEST ":        "plain@example.test",
		"under_score@my_host.example": "under_score@my_host.example", // lookup refuses: raw, deterministic
		"trailing@example.test.":      "trailing@example.test",
		"no-at-sign":                  "no-at-sign",
	} {
		if got := canonicalRecipient(in); got != want {
			t.Errorf("canonicalRecipient(%q) = %q, want %q", in, got, want)
		}
	}
	if canonicalRecipient("a@bücher.example") != canonicalRecipient("A@XN--BCHER-KVA.EXAMPLE") {
		t.Fatal("Unicode and A-label spellings must canonicalize identically")
	}
}
