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

// The regression this package exists to prevent. Signing oversigned
// Message-ID even though the composer omits it; SES then added the header
// and every delivered message carried a permanently-broken signature.
func TestSign_SurvivesDownstreamMessageIDInjection(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	signed, err := Sign([]byte(composerShapedMessage), "example.com", kp.Selector, kp.PrivateKeyDER)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// SES prepends its own Message-ID to the message we handed it.
	delivered := append([]byte(sesMessageIDHeader), signed...)

	verifications := verifySigned(t, delivered, "example.com", kp.Selector, kp.PublicKeyDNS)
	if len(verifications) != 1 {
		t.Fatalf("expected 1 verification result, got %d", len(verifications))
	}
	if verifications[0].Err != nil {
		t.Errorf("signature broke when a downstream MTA added Message-ID: %v", verifications[0].Err)
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
		"Date":                  "Sat, 23 May 2026 12:00:00 +0000",
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

func TestSign_OmitsAbsentHeadersFromH(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	signed, err := Sign([]byte(composerShapedMessage), "example.com", kp.Selector, kp.PrivateKeyDER)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	h := signedHTag(t, signed)
	for _, absent := range []string{"message-id", "in-reply-to", "references", "list-unsubscribe", "list-unsubscribe-post", "cc", "reply-to"} {
		if hasHeader(h, absent) {
			t.Errorf("h= covers %q, which the message does not carry: %v", absent, h)
		}
	}
	// The headers that ARE present must still be covered.
	for _, want := range []string{"from", "to", "subject", "date", "mime-version", "content-type"} {
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
			name:    "composer shape drops absent headers",
			message: composerShapedMessage,
			want:    []string{"From", "To", "Subject", "Date", "MIME-Version", "Content-Type"},
		},
		{
			name:    "unsubscribe headers covered when set",
			message: "From: a@b.test\r\nList-Unsubscribe: <https://x/u>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n\r\nbody",
			want:    []string{"From", "List-Unsubscribe", "List-Unsubscribe-Post"},
		},
		{
			name:    "From always covered even when missing",
			message: "Subject: no from here\r\n\r\nbody",
			want:    []string{"From", "Subject"},
		},
		{
			name:    "case-insensitive header match",
			message: "from: a@b.test\r\nCONTENT-TYPE: text/plain\r\n\r\nbody",
			want:    []string{"From", "Content-Type"},
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
