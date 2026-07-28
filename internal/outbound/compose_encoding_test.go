package outbound

import (
	"io"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/mailparse"
)

// Bodies below deliberately contain an em dash and a curly apostrophe: the
// exact characters that shipped in real outbound mail and tripped
// SpamAssassin's CTE_8BIT_MISMATCH (+1.0) because the composer declared no
// Content-Transfer-Encoding and RFC 2045 § 6.1 defaults that to 7bit.
const (
	asciiBody    = "Plain ASCII body, nothing clever."
	nonASCIIBody = "It's GA today - stable /v1 API — and an MCP server."
	nonASCIIHTML = "<p>It’s GA today — stable /v1 API.</p>"
)

func decodeQP(t *testing.T, s string) string {
	t.Helper()
	got, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(s)))
	if err != nil {
		t.Fatalf("quoted-printable decode failed: %v", err)
	}
	return string(got)
}

// splitBody returns the raw body bytes after the header/body separator.
func splitBody(t *testing.T, raw []byte) string {
	t.Helper()
	_, body, ok := strings.Cut(string(raw), "\r\n\r\n")
	if !ok {
		t.Fatal("no header/body separator")
	}
	return body
}

func TestComposeMessageASCIIBodyDeclares7bit(t *testing.T) {
	raw, err := ComposeMessage(
		"agent@bot.example.com", []string{"alice@gmail.com"}, nil,
		"Hello", asciiBody, "text/plain", "", nil, "relay.e2a.dev", "", "",
	)
	if err != nil {
		t.Fatalf("ComposeMessage failed: %v", err)
	}

	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse composed message: %v", err)
	}
	if got := msg.Header.Get("Content-Transfer-Encoding"); got != "7bit" {
		t.Errorf("Content-Transfer-Encoding = %q, want 7bit", got)
	}
	// An ASCII body must go out byte-identical — the common path is unchanged.
	if got := splitBody(t, raw); got != asciiBody {
		t.Errorf("body = %q, want it unmodified (%q)", got, asciiBody)
	}
}

func TestComposeMessageNonASCIIBodyIsQuotedPrintable(t *testing.T) {
	raw, err := ComposeMessage(
		"agent@bot.example.com", []string{"alice@gmail.com"}, nil,
		"Hello", nonASCIIBody, "text/plain", "", nil, "relay.e2a.dev", "", "",
	)
	if err != nil {
		t.Fatalf("ComposeMessage failed: %v", err)
	}

	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse composed message: %v", err)
	}
	if got := msg.Header.Get("Content-Transfer-Encoding"); got != "quoted-printable" {
		t.Fatalf("Content-Transfer-Encoding = %q, want quoted-printable", got)
	}

	body := splitBody(t, raw)
	// The declared encoding must be truthful: no 8-bit bytes may survive.
	for i := 0; i < len(body); i++ {
		if body[i] > 127 {
			t.Fatalf("body byte %d = %#x is 8-bit despite quoted-printable", i, body[i])
		}
	}
	if got := decodeQP(t, body); got != nonASCIIBody {
		t.Errorf("decoded body = %q, want %q", got, nonASCIIBody)
	}
}

// TestComposeMessageNonASCIIRoundTripsThroughMailparse proves the receive
// side is unaffected: the parser that handles inbound mail (and the loopback
// self-send path) decodes what the composer now emits.
func TestComposeMessageNonASCIIRoundTripsThroughMailparse(t *testing.T) {
	raw, err := ComposeMessage(
		"agent@bot.example.com", []string{"alice@gmail.com"}, nil,
		"Hello", nonASCIIBody, "text/plain", "", nil, "relay.e2a.dev", "", "",
	)
	if err != nil {
		t.Fatalf("ComposeMessage failed: %v", err)
	}
	if got := strings.TrimSpace(mailparse.Parse(raw, 64*1024).Text); got != nonASCIIBody {
		t.Errorf("mailparse.Text = %q, want %q", got, nonASCIIBody)
	}
}

func TestComposeMultipartEncodesBothParts(t *testing.T) {
	raw, err := ComposeMultipartMessage(
		"agent@bot.example.com", []string{"alice@gmail.com"}, nil,
		"Hello", nonASCIIBody, nonASCIIHTML, "", nil, "relay.e2a.dev", "", "",
	)
	if err != nil {
		t.Fatalf("ComposeMultipartMessage failed: %v", err)
	}

	body := splitBody(t, raw)
	if n := strings.Count(body, "Content-Transfer-Encoding: quoted-printable"); n != 2 {
		t.Errorf("got %d quoted-printable parts, want 2 (text + html)\n%s", n, body)
	}
	for i := 0; i < len(body); i++ {
		if body[i] > 127 {
			t.Fatalf("multipart body byte %d = %#x is 8-bit", i, body[i])
		}
	}

	view := mailparse.Parse(raw, 64*1024)
	if got := strings.TrimSpace(view.Text); got != nonASCIIBody {
		t.Errorf("mailparse.Text = %q, want %q", got, nonASCIIBody)
	}
	if got := strings.TrimSpace(view.HTML); got != nonASCIIHTML {
		t.Errorf("mailparse.HTML = %q, want %q", got, nonASCIIHTML)
	}
}

// A mixed ASCII/non-ASCII pair must be encoded independently — the HTML part
// carrying a curly quote must not force the plain part off 7bit, and vice versa.
func TestComposeMultipartEncodesPartsIndependently(t *testing.T) {
	raw, err := ComposeMultipartMessage(
		"agent@bot.example.com", []string{"alice@gmail.com"}, nil,
		"Hello", asciiBody, nonASCIIHTML, "", nil, "relay.e2a.dev", "", "",
	)
	if err != nil {
		t.Fatalf("ComposeMultipartMessage failed: %v", err)
	}

	body := splitBody(t, raw)
	if n := strings.Count(body, "Content-Transfer-Encoding: 7bit"); n != 1 {
		t.Errorf("got %d 7bit parts, want 1 (the ASCII text part)\n%s", n, body)
	}
	if n := strings.Count(body, "Content-Transfer-Encoding: quoted-printable"); n != 1 {
		t.Errorf("got %d quoted-printable parts, want 1 (the HTML part)\n%s", n, body)
	}
}

func TestComposeWithAttachmentsEncodesNonASCIIBody(t *testing.T) {
	raw, err := ComposeMessageWithAttachments(
		"agent@bot.example.com", []string{"alice@gmail.com"}, nil,
		"Hello", nonASCIIBody, "", "", nil, "relay.e2a.dev", "", "",
		[]Attachment{{Filename: "a.txt", ContentType: "text/plain", Data: "aGk="}},
	)
	if err != nil {
		t.Fatalf("ComposeMessageWithAttachments failed: %v", err)
	}

	body := splitBody(t, raw)
	if !strings.Contains(body, "Content-Transfer-Encoding: quoted-printable") {
		t.Errorf("text part missing quoted-printable encoding\n%s", body)
	}
	// Attachments were already base64 and must stay that way.
	if !strings.Contains(body, "Content-Transfer-Encoding: base64") {
		t.Errorf("attachment part lost its base64 encoding\n%s", body)
	}
	for i := 0; i < len(body); i++ {
		if body[i] > 127 {
			t.Fatalf("body byte %d = %#x is 8-bit", i, body[i])
		}
	}
}

func TestEncodeBody(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		encoding string
	}{
		{"empty", "", "7bit"},
		{"ascii", asciiBody, "7bit"},
		{"em dash", "a — b", "quoted-printable"},
		{"curly apostrophe", "it’s", "quoted-printable"},
		{"emoji", "ship it \U0001F680", "quoted-printable"},
		{"latin-1 accent", "café", "quoted-printable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encoding, encoded := encodeBody(tc.body)
			if encoding != tc.encoding {
				t.Fatalf("encoding = %q, want %q", encoding, tc.encoding)
			}
			if encoding == "7bit" {
				if encoded != tc.body {
					t.Errorf("7bit body was modified: %q", encoded)
				}
				return
			}
			if got := decodeQP(t, encoded); got != tc.body {
				t.Errorf("round trip = %q, want %q", got, tc.body)
			}
		})
	}
}
