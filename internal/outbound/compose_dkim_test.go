package outbound

import (
	"bytes"
	"context"
	"strings"
	"testing"

	msgauth "github.com/emersion/go-msgauth/dkim"

	"github.com/tokencanopy/e2a/internal/dkim"
	"github.com/tokencanopy/e2a/internal/identity"
)

// These tests assert the property the encoding work exists to protect, end
// to end rather than per-unit: a message this package produces must still
// carry a VALID DKIM signature after a downstream MTA has done the things
// MTAs are entitled to do to it.
//
// They drive Sender.ComposeForAccept rather than the Compose* functions
// directly, so the real ordering is covered — body composition, then
// managed List-Unsubscribe headers, then signing. Signing last is what
// makes this fragile: anything emitted that a relay feels obliged to
// "fix" mutates the payload after the body hash is computed.
//
// The two mutations below are NOT equivalent, which is the subtlety worth
// locking down. Relaxed body canonicalisation (RFC 6376 § 3.4.4, what
// dkim.Sign uses) absorbs a REWRITTEN line ending, so bare LF -> CRLF is
// harmless. It does not absorb an INSERTED break, so a wrap mid-line
// changes the canonical body and the signature dies with "body hash did
// not verify". Only the second is a real hazard, and the defence is that
// the composer never emits a line long enough to need wrapping.

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

// composeViaSender runs the production accept path: compose, apply managed
// unsubscribe headers, sign. Returns the exact bytes handed to the relay.
func composeViaSender(t *testing.T, kp *dkim.Keypair, req SendRequest) []byte {
	t.Helper()
	lookup := &fakeDKIMLookup{get: func(_ context.Context, domain string) (string, []byte, error) {
		if domain != dkimTestDomain {
			t.Fatalf("DKIM lookup domain = %q, want %q", domain, dkimTestDomain)
		}
		return kp.Selector, kp.PrivateKeyDER, nil
	}}
	sender := NewSenderWithDKIM(nil, dkimTestDomain, lookup)
	agent := &identity.AgentIdentity{ID: "bot@" + dkimTestDomain, Domain: dkimTestDomain}

	res, err := sender.ComposeForAccept(agent, req)
	if err != nil {
		t.Fatalf("ComposeForAccept: %v", err)
	}
	if len(res.Raw) == 0 {
		t.Fatal("ComposeForAccept returned an empty message")
	}
	return res.Raw
}

func TestComposedMessageSurvivesMTAMutation(t *testing.T) {
	kp, err := dkim.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	base := func(body, html string) SendRequest {
		return SendRequest{
			To:       []string{"alice@elsewhere.test"},
			Subject:  "Subject line",
			Body:     body,
			HTMLBody: html,
		}
	}

	cases := []struct {
		name string
		req  SendRequest
	}{
		{"short ascii, unix newlines", base("Line one\nLine two\nLine three\n", "")},
		{"short ascii, crlf newlines", base("Line one\r\nLine two\r\n", "")},
		{"non-ascii", base("It's GA today — stable /v1 API.\nSecond line.\n", "")},
		{"over-length ascii line", base(strings.Repeat("a", 4000), "")},
		{"over-length non-ascii line", base(strings.Repeat("é", 2000), "")},
		{"multipart with both", base("Plain — text\nline two\n", "<p>HTML — body</p>")},
		{"body ending without newline", base("no trailing newline", "")},
		{"body with trailing spaces", base("trailing spaces here   \nand more   \n", "")},
		{"long url on one line", base("See https://example.com/"+strings.Repeat("x", 1500), "")},
		{"with attachment", func() SendRequest {
			r := base("Body — with attachment\n", "")
			r.Attachments = []Attachment{{
				Filename: "a.txt", ContentType: "text/plain", Data: strings.Repeat("aGk=", 500),
			}}
			return r
		}()},
		{"managed unsubscribe headers", func() SendRequest {
			r := base("Newsletter — body\n", "<p>Newsletter — body</p>")
			r.Unsubscribe = &UnsubscribeOptions{
				Mode: "managed",
				URL:  "https://api.example.com/u/u1_token",
			}
			return r
		}()},
		{"reply with a references chain", func() SendRequest {
			r := base("Reply — body\n", "")
			r.ReplyToMessageID = "<parent@example.com>"
			r.References = []string{"<a@example.com>", "<b@example.com>", "<parent@example.com>"}
			return r
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signed := composeViaSender(t, kp, tc.req)

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

// Every body part on the production path must declare a transfer encoding.
// A part without one falls back to the RFC 2045 § 6.1 default of 7bit,
// which is the defect these changes exist to remove.
func TestSenderPathDeclaresEncodingOnEveryPart(t *testing.T) {
	kp, err := dkim.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	raw := composeViaSender(t, kp, SendRequest{
		To:       []string{"alice@elsewhere.test"},
		Subject:  "Subject — line",
		Body:     "Plain — text",
		HTMLBody: "<p>HTML — body</p>",
	})

	if !bytes.Contains(raw, []byte("Content-Transfer-Encoding: quoted-printable")) {
		t.Errorf("non-ASCII parts did not declare quoted-printable:\n%s", raw)
	}
	// text/plain and text/html are the two leaf parts here.
	if n := bytes.Count(raw, []byte("Content-Transfer-Encoding: ")); n != 2 {
		t.Errorf("got %d Content-Transfer-Encoding headers, want 2\n%s", n, raw)
	}
	// No 8-bit byte may survive in a message that declares quoted-printable.
	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	for i, b := range raw[idx:] {
		if b > 127 {
			t.Fatalf("body byte %d = %#x is 8-bit on the production path", i, b)
		}
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
