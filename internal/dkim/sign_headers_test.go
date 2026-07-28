package dkim

import (
	"bytes"
	"strings"
	"testing"

	msgauth "github.com/emersion/go-msgauth/dkim"
)

// composerShapedMessage mirrors what internal/outbound actually emits:
// no Message-ID (SES assigns one and we capture it from the SMTP
// response), no In-Reply-To/References on a fresh send, no
// List-Unsubscribe unless the caller opted in.
const composerShapedMessage = "From: bot@example.com\r\n" +
	"To: alice@elsewhere.test\r\n" +
	"Subject: hello\r\n" +
	"Date: Fri, 22 May 2026 12:00:00 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"hi there\r\n"

// sesMessageIDHeader is the header SES prepends after we hand it the
// message — the exact mutation that used to invalidate our signature.
const sesMessageIDHeader = "Message-ID: <010f019f-0000@us-east-2.amazonses.com>\r\n"

// signedHTag returns the lowercased header names listed in the signature's
// h= tag. Parsed rather than substring-matched: the base64 b= blob readily
// contains sequences like "cc", so grepping the whole header block for a
// header name gives false positives.
func signedHTag(t *testing.T, signed []byte) []string {
	t.Helper()
	idx := bytes.Index(signed, []byte("\r\n\r\n"))
	if idx < 0 {
		t.Fatal("no header/body separator in signed message")
	}
	// Unfold continuation lines so the signature is one logical line.
	head := strings.NewReplacer("\r\n ", "", "\r\n\t", "").Replace(string(signed[:idx]))

	for _, line := range strings.Split(head, "\r\n") {
		if !strings.HasPrefix(strings.ToLower(line), "dkim-signature:") {
			continue
		}
		for _, tag := range strings.Split(line, ";") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(tag), "h="); ok {
				return strings.Split(strings.ToLower(strings.TrimSpace(v)), ":")
			}
		}
	}
	t.Fatalf("no h= tag in DKIM-Signature:\n%s", head)
	return nil
}

func hasHeader(list []string, name string) bool {
	for _, h := range list {
		if h == name {
			return true
		}
	}
	return false
}

// verifySigned verifies every DKIM signature on the message against an
// in-memory public key, bypassing DNS.
func verifySigned(t *testing.T, signed []byte, domain, selector, publicKeyDNS string) []*msgauth.Verification {
	t.Helper()
	v, err := msgauth.VerifyWithOptions(bytes.NewReader(signed), &msgauth.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) {
			parts := strings.SplitN(name, "._domainkey.", 2)
			if len(parts) != 2 || parts[0] != selector || parts[1] != domain {
				return nil, nil
			}
			return []string{"v=DKIM1; k=rsa; p=" + publicKeyDNS}, nil
		},
	})
	if err != nil {
		t.Fatalf("VerifyWithOptions: %v", err)
	}
	return v
}

// The regression this package exists to prevent. SES owns Message-ID and
// Date on delivered mail: it adds Message-ID when the composer omits it and
// replaces the supplied Date. Neither mutation may break our signature.
func TestSign_SurvivesSESHeaderRewrites(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	signed, err := Sign([]byte(composerShapedMessage), "example.com", kp.Selector, kp.PrivateKeyDER)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// SES replaces Date and prepends its own Message-ID.
	delivered := bytes.Replace(
		signed,
		[]byte("Date: Fri, 22 May 2026 12:00:00 +0000"),
		[]byte("Date: Fri, 22 May 2026 12:00:01 +0000"),
		1,
	)
	if bytes.Equal(delivered, signed) {
		t.Fatal("Date replacement target not found — test is inert")
	}
	delivered = append([]byte(sesMessageIDHeader), delivered...)

	verifications := verifySigned(t, delivered, "example.com", kp.Selector, kp.PublicKeyDNS)
	if len(verifications) != 1 {
		t.Fatalf("expected 1 verification result, got %d", len(verifications))
	}
	if verifications[0].Err != nil {
		t.Errorf("signature broke after SES rewrote Message-ID/Date: %v", verifications[0].Err)
	}
}

// Message-ID is the header SES actually adds, but nothing about the bug
// was specific to it: ANY candidate header absent at signing time and
// added downstream would have broken the signature the same way. This
// generalises the regression across the whole candidate list so a future
// relay that stamps, say, Reply-To cannot reintroduce it.
func TestSign_AnyAbsentCandidateMayBeAddedDownstream(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	// Plausible downstream values per header. From is excluded: it is
	// always present and always signed.
	added := map[string]string{
		"To":                    "alice@elsewhere.test",
		"Cc":                    "bob@elsewhere.test",
		"Subject":               "stamped subject",
		"Message-ID":            "<ses@us-east-2.amazonses.com>",
		"In-Reply-To":           "<parent@example.com>",
		"References":            "<a@example.com> <b@example.com>",
		"MIME-Version":          "1.0",
		"Content-Type":          "text/plain; charset=utf-8",
		"Reply-To":              "noreply@example.com",
		"List-Unsubscribe":      "<https://example.com/u/x>",
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
	}

	for _, header := range signedHeaderCandidates {
		if header == "From" {
			continue
		}
		value, ok := added[header]
		if !ok {
			t.Fatalf("candidate %q has no downstream value in this test — add one", header)
		}

		t.Run(header, func(t *testing.T) {
			// Minimal message carrying only From, so `header` is absent.
			msg := "From: bot@example.com\r\n\r\nbody text\r\n"

			signed, err := Sign([]byte(msg), "example.com", kp.Selector, kp.PrivateKeyDER)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if h := signedHTag(t, signed); hasHeader(h, strings.ToLower(header)) {
				t.Fatalf("h= covers absent header %q: %v", header, h)
			}

			delivered := append([]byte(header+": "+value+"\r\n"), signed...)
			v := verifySigned(t, delivered, "example.com", kp.Selector, kp.PublicKeyDNS)
			if len(v) != 1 {
				t.Fatalf("expected 1 verification, got %d", len(v))
			}
			if v[0].Err != nil {
				t.Errorf("signature broke when a relay added %q: %v", header, v[0].Err)
			}
		})
	}
}

// The mirror of the above: a header that IS present must genuinely be
// protected. Without this, narrowing h= could degrade into signing
// nothing meaningful while every test still passed.
func TestSign_TamperingWithASignedHeaderIsDetected(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	signed, err := Sign([]byte(composerShapedMessage), "example.com", kp.Selector, kp.PrivateKeyDER)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	tests := []struct {
		name string
		from string
		to   string
	}{
		{"subject rewritten", "Subject: hello", "Subject: you have won"},
		{"recipient rewritten", "To: alice@elsewhere.test", "To: mallory@evil.test"},
		{"body rewritten", "hi there", "send money"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tampered := bytes.Replace(signed, []byte(tc.from), []byte(tc.to), 1)
			if bytes.Equal(tampered, signed) {
				t.Fatalf("tamper target %q not found — test is inert", tc.from)
			}
			v := verifySigned(t, tampered, "example.com", kp.Selector, kp.PublicKeyDNS)
			if len(v) == 1 && v[0].Err == nil {
				t.Errorf("tampering with %s went undetected", tc.name)
			}
		})
	}
}

// The attack N+1 oversigning exists to stop. A hop prepends a second
// Subject; a verifier binds header instances from the bottom up, so with
// the header listed only once it matches the original and reports pass —
// while the MUA displays the attacker's copy at the top.
func TestSign_PrependedDuplicateHeaderIsDetected(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	spoofs := map[string]string{
		"Subject":    "Subject: YOU HAVE WON",
		"From":       "From: ceo@example.com",
		"To":         "To: mallory@evil.test",
		"Reply-To":   "Reply-To: mallory@evil.test",
		"Message-ID": "Message-ID: <spoof@evil.test>",
	}

	// Message carries every header the spoofs target, so each is a
	// duplicate rather than a fresh addition.
	msg := "From: bot@example.com\r\n" +
		"To: alice@elsewhere.test\r\n" +
		"Reply-To: bot@example.com\r\n" +
		"Subject: real subject\r\n" +
		"Message-ID: <real@example.com>\r\n" +
		"Date: Fri, 22 May 2026 12:00:00 +0000\r\n\r\nbody\r\n"

	signed, err := Sign([]byte(msg), "example.com", kp.Selector, kp.PrivateKeyDER)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if v := verifySigned(t, signed, "example.com", kp.Selector, kp.PublicKeyDNS); v[0].Err != nil {
		t.Fatalf("control: unmutated signature must verify: %v", v[0].Err)
	}

	for header, spoof := range spoofs {
		t.Run(header, func(t *testing.T) {
			mutated := append([]byte(spoof+"\r\n"), signed...)
			v := verifySigned(t, mutated, "example.com", kp.Selector, kp.PublicKeyDNS)
			if len(v) == 1 && v[0].Err == nil {
				t.Errorf("a prepended duplicate %s went undetected", header)
			}
		})
	}
}

// N+1 must not come at the cost of the fix it builds on: an ABSENT
// candidate is still omitted entirely, so a relay adding one for the
// first time (SES and Message-ID) still verifies.
func TestSign_NPlusOneDoesNotResurrectOversigning(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	signed, err := Sign([]byte(composerShapedMessage), "example.com", kp.Selector, kp.PrivateKeyDER)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if h := signedHTag(t, signed); hasHeader(h, "message-id") {
		t.Fatalf("h= claims an absent Message-ID: %v", h)
	}

	delivered := append([]byte(sesMessageIDHeader), signed...)
	v := verifySigned(t, delivered, "example.com", kp.Selector, kp.PublicKeyDNS)
	if len(v) != 1 || v[0].Err != nil {
		t.Errorf("SES stamping Message-ID broke the signature again: %+v", v)
	}
}

func TestSign_CoversOnlyStablePresentHeaders(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	signed, err := Sign([]byte(composerShapedMessage), "example.com", kp.Selector, kp.PrivateKeyDER)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	h := signedHTag(t, signed)
	for _, notSigned := range []string{"date", "message-id", "in-reply-to", "references", "list-unsubscribe", "list-unsubscribe-post", "cc", "reply-to"} {
		if hasHeader(h, notSigned) {
			t.Errorf("h= unexpectedly covers %q: %v", notSigned, h)
		}
	}
	// Stable headers that are present must still be covered.
	for _, want := range []string{"from", "to", "subject", "mime-version", "content-type"} {
		if !hasHeader(h, want) {
			t.Errorf("h= is missing present header %q: %v", want, h)
		}
	}
}

func TestSign_CoversMessageIDWhenPresent(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	msg := strings.Replace(
		composerShapedMessage,
		"MIME-Version: 1.0\r\n",
		"Message-ID: <own@example.com>\r\nMIME-Version: 1.0\r\n",
		1,
	)
	signed, err := Sign([]byte(msg), "example.com", kp.Selector, kp.PrivateKeyDER)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if h := signedHTag(t, signed); !hasHeader(h, "message-id") {
		t.Errorf("h= must cover Message-ID when the message sets it: %v", h)
	}

	verifications := verifySigned(t, signed, "example.com", kp.Selector, kp.PublicKeyDNS)
	if len(verifications) != 1 || verifications[0].Err != nil {
		t.Errorf("signature over a present Message-ID did not verify: %+v", verifications)
	}
}

func TestSignedHeaderKeys(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    []string
	}{
		{
			// Each present header appears n+1 times: N+1 oversigning, so
			// a relay cannot append another instance unnoticed.
			name:    "composer shape drops absent headers",
			message: composerShapedMessage,
			want: []string{
				"From", "From", "To", "To", "Subject", "Subject",
				"MIME-Version", "MIME-Version", "Content-Type", "Content-Type",
			},
		},
		{
			name:    "unsubscribe headers covered when set",
			message: "From: a@b.test\r\nList-Unsubscribe: <https://x/u>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n\r\nbody",
			want: []string{
				"From", "From", "List-Unsubscribe", "List-Unsubscribe",
				"List-Unsubscribe-Post", "List-Unsubscribe-Post",
			},
		},
		{
			// Absent From is listed exactly once: RFC 6376 § 5.4 requires
			// it in h=, and there is no instance to count.
			name:    "From always covered even when missing",
			message: "Subject: no from here\r\n\r\nbody",
			want:    []string{"From", "Subject", "Subject"},
		},
		{
			name:    "case-insensitive header match",
			message: "from: a@b.test\r\nCONTENT-TYPE: text/plain\r\n\r\nbody",
			want:    []string{"From", "From", "Content-Type", "Content-Type"},
		},
		{
			// Already duplicated in the source message: listed n+1 = 3.
			name:    "a header present twice is listed three times",
			message: "From: a@b.test\r\nSubject: one\r\nSubject: two\r\n\r\nbody",
			want:    []string{"From", "From", "Subject", "Subject", "Subject"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := signedHeaderKeys([]byte(tc.message))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("signedHeaderKeys = %v, want %v", got, tc.want)
			}
		})
	}
}

// An unparseable header block must not silently produce a signature over
// nothing; falling back to the full candidate list preserves the previous
// behavior (msgauth fails, the caller sends unsigned).
func TestSignedHeaderKeys_UnparseableFallsBackToFullList(t *testing.T) {
	got := signedHeaderKeys([]byte("this is not a header block at all"))
	if len(got) != len(signedHeaderCandidates) {
		t.Errorf("got %d keys, want the full candidate list (%d)", len(got), len(signedHeaderCandidates))
	}
}

// Odd header blocks must narrow h= rather than widen it, and the resulting
// signature must still verify. Claiming a header the signer cannot see is
// the oversigning bug this package exists to prevent; missing one the
// signer can see only leaves it uncovered.
//
// Both shapes below come from review and are unreachable from the composer
// (headerWriter emits "Key: value" and strips CR/LF), so this pins the
// degradation direction for any future caller that hands Sign raw bytes.
func TestSign_OddHeaderBlockNarrowsRatherThanWidens(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	tests := []struct {
		name    string
		message string
		absent  string // must NOT appear in h=
	}{
		{
			// textproto aborts at the colon-less line; Subject after it is lost.
			name:    "line missing a colon aborts parsing",
			message: "From: bot@example.com\r\nX-Broken-Header\r\nSubject: hi\r\n\r\nbody",
			absent:  "subject",
		},
		{
			// "Subject : hi" parses to the key "Subject " (space retained).
			name:    "space before colon",
			message: "From: bot@example.com\r\nSubject : hi\r\n\r\nbody",
			absent:  "subject",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			signed, err := Sign([]byte(tc.message), "example.com", kp.Selector, kp.PrivateKeyDER)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			h := signedHTag(t, signed)
			if hasHeader(h, tc.absent) {
				t.Errorf("h= claims %q the signer may not see, risking oversigning: %v", tc.absent, h)
			}
			if !hasHeader(h, "from") {
				t.Errorf("h= must always cover From: %v", h)
			}
			// The whole point: a narrower h= still yields a VALID signature.
			v := verifySigned(t, signed, "example.com", kp.Selector, kp.PublicKeyDNS)
			if len(v) != 1 || v[0].Err != nil {
				t.Errorf("narrowed signature failed to verify: %+v", v)
			}
		})
	}
}
