package outbound

import (
	"bytes"
	"strings"
	"testing"

	msgauth "github.com/emersion/go-msgauth/dkim"

	"github.com/tokencanopy/e2a/internal/dkim"
)

// These tests assert the property the encoding work exists to protect, end
// to end rather than per-unit: a message this package composes must still
// carry a VALID DKIM signature after a downstream MTA has done the things
// MTAs are entitled to do to it.
//
// Signing happens after composition (see Sender.signMessage), so anything
// the composer emits that an MTA feels obliged to "fix" — an over-length
// line it must wrap, a bare LF it must normalise — mutates the payload
// after the body hash is computed.
//
// The two mutations are NOT equivalent, which is the subtlety worth
// locking down. Relaxed body canonicalisation (RFC 6376 § 3.4.4, what
// dkim.Sign uses) absorbs a REWRITTEN line ending, so LF -> CRLF is
// harmless. It does not absorb an INSERTED break, so a wrap mid-line
// changes the canonical body and the signature dies with "body hash did
// not verify". Only the second is a real hazard, and the fix is to make
// sure the composer never emits a line long enough to need wrapping.

const dkimTestDomain = "example.com"

// mtaNormaliseLineEndings rewrites every bare LF in the body to CRLF, the
// way an RFC-compliant relay does before SMTP transmission. Headers are
// left alone.
func mtaNormaliseLineEndings(t *testing.T, raw []byte) []byte {
	t.Helper()
	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	if idx < 0 {
		t.Fatal("no header/body separator")
	}
	head := raw[:idx+4]
	body := string(raw[idx+4:])
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	return append(append([]byte{}, head...), []byte(body)...)
}

// mtaHardWrap simulates a relay enforcing the RFC 5322 § 2.1.1 998-octet
// limit by inserting a CRLF into any longer line. If the composer did its
// job there is nothing to wrap and the message comes back unchanged.
func mtaHardWrap(t *testing.T, raw []byte) ([]byte, bool) {
	t.Helper()
	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	if idx < 0 {
		t.Fatal("no header/body separator")
	}
	head := raw[:idx+4]

	var out []string
	wrapped := false
	for _, line := range strings.Split(string(raw[idx+4:]), "\r\n") {
		for len(line) > maxLineOctets {
			out = append(out, line[:maxLineOctets])
			line = line[maxLineOctets:]
			wrapped = true
		}
		out = append(out, line)
	}
	return append(append([]byte{}, head...), []byte(strings.Join(out, "\r\n"))...), wrapped
}

func verifyDKIM(t *testing.T, raw []byte, kp *dkim.Keypair) error {
	t.Helper()
	v, err := msgauth.VerifyWithOptions(bytes.NewReader(raw), &msgauth.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) {
			parts := strings.SplitN(name, "._domainkey.", 2)
			if len(parts) != 2 || parts[0] != kp.Selector || parts[1] != dkimTestDomain {
				return nil, nil
			}
			return []string{"v=DKIM1; k=rsa; p=" + kp.PublicKeyDNS}, nil
		},
	})
	if err != nil {
		t.Fatalf("VerifyWithOptions: %v", err)
	}
	if len(v) != 1 {
		t.Fatalf("expected 1 verification, got %d", len(v))
	}
	return v[0].Err
}

// composeAndSign builds a message the way Sender does — compose, then sign.
func composeAndSign(t *testing.T, kp *dkim.Keypair, text, html string) []byte {
	t.Helper()
	raw, err := ComposeMultipartMessage(
		"agent@example.com", []string{"alice@elsewhere.test"}, nil,
		"Subject line", text, html, "", nil, "relay.e2a.dev", "", "",
	)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	signed, err := dkim.Sign(raw, dkimTestDomain, kp.Selector, kp.PrivateKeyDER)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func TestComposedMessageSurvivesMTAMutation(t *testing.T) {
	kp, err := dkim.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	bodies := []struct {
		name string
		text string
		html string
	}{
		{"short ascii, unix newlines", "Line one\nLine two\nLine three\n", ""},
		{"short ascii, crlf newlines", "Line one\r\nLine two\r\n", ""},
		{"non-ascii", "It's GA today — stable /v1 API.\nSecond line.\n", ""},
		{"over-length ascii line", strings.Repeat("a", 4000), ""},
		{"over-length non-ascii line", strings.Repeat("é", 2000), ""},
		{"multipart with both", "Plain — text\nline two\n", "<p>HTML — body</p>"},
		{"empty text with html", "", "<p>only html</p>"},
		{"body ending without newline", "no trailing newline", ""},
		{"body with trailing spaces", "trailing spaces here   \nand more   \n", ""},
		{"long url on one line", "See https://example.com/" + strings.Repeat("x", 1500), ""},
	}

	for _, b := range bodies {
		t.Run(b.name, func(t *testing.T) {
			signed := composeAndSign(t, kp, b.text, b.html)

			if err := verifyDKIM(t, signed, kp); err != nil {
				t.Fatalf("signature invalid before any mutation: %v", err)
			}

			// An MTA must never need to wrap what we emit.
			wrappedMsg, didWrap := mtaHardWrap(t, signed)
			if didWrap {
				t.Errorf("composer emitted a line longer than %d octets, forcing an MTA wrap", maxLineOctets)
			}
			if err := verifyDKIM(t, wrappedMsg, kp); err != nil {
				t.Errorf("signature broke after MTA hard-wrap: %v", err)
			}

			// Line-ending normalisation must be absorbed by relaxed canonicalisation.
			if err := verifyDKIM(t, mtaNormaliseLineEndings(t, signed), kp); err != nil {
				t.Errorf("signature broke after MTA line-ending normalisation: %v", err)
			}
		})
	}
}

// Guard the guard: if the composer ever regresses to emitting an
// over-length line, mtaHardWrap must actually detect and break it.
// Without this, TestComposedMessageSurvivesMTAMutation could pass because
// the simulation is inert rather than because the composer is correct.
func TestMTAHardWrapDetectsAnOverlongLine(t *testing.T) {
	kp, err := dkim.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	// Hand-built message that bypasses the composer's encoding choice.
	raw := "From: agent@example.com\r\nTo: alice@elsewhere.test\r\n" +
		"Subject: hi\r\nDate: Fri, 22 May 2026 12:00:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: 7bit\r\n\r\n" +
		strings.Repeat("a", 1500) + "\r\n"

	signed, err := dkim.Sign([]byte(raw), dkimTestDomain, kp.Selector, kp.PrivateKeyDER)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := verifyDKIM(t, signed, kp); err != nil {
		t.Fatalf("control: signature should be valid before mutation: %v", err)
	}

	mutated, didWrap := mtaHardWrap(t, signed)
	if !didWrap {
		t.Fatal("simulation failed to notice a 1500-octet line")
	}
	if err := verifyDKIM(t, mutated, kp); err == nil {
		t.Fatal("expected the wrap to break the body hash, but the signature verified — " +
			"the mutation simulation is not exercising anything")
	}
}
