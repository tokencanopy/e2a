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

// A composed message must never contain a line a compliant MTA would have
// to wrap. DKIM signs after composition, and relaxed body canonicalisation
// absorbs rewritten line endings but NOT a break inserted mid-line — an
// over-length line therefore fails verification with "body hash did not
// verify" once the MTA wraps it.
func TestComposedMessageHasNoOverlongLine(t *testing.T) {
	longLine := strings.Repeat("a", 5000)

	cases := []struct {
		name string
		raw  func() ([]byte, error)
	}{
		{"single part", func() ([]byte, error) {
			return ComposeMessage("agent@bot.example.com", []string{"a@b.test"}, nil,
				"Hello", longLine, "text/plain", "", nil, "relay.e2a.dev", "", "")
		}},
		{"multipart", func() ([]byte, error) {
			return ComposeMultipartMessage("agent@bot.example.com", []string{"a@b.test"}, nil,
				"Hello", longLine, "<p>"+longLine+"</p>", "", nil, "relay.e2a.dev", "", "")
		}},
		{"with attachments", func() ([]byte, error) {
			return ComposeMessageWithAttachments("agent@bot.example.com", []string{"a@b.test"}, nil,
				"Hello", longLine, "", "", nil, "relay.e2a.dev", "", "",
				[]Attachment{{Filename: "a.txt", ContentType: "text/plain", Data: strings.Repeat("aGk=", 400)}})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := tc.raw()
			if err != nil {
				t.Fatalf("compose failed: %v", err)
			}
			for i, line := range strings.Split(string(raw), "\r\n") {
				if len(line) > maxLineOctets {
					t.Errorf("line %d is %d octets, exceeds the %d limit", i, len(line), maxLineOctets)
				}
			}
		})
	}
}

// The long-line body must still be recoverable end to end.
func TestComposeLongAsciiLineRoundTrips(t *testing.T) {
	longLine := strings.Repeat("a", 5000)
	raw, err := ComposeMessage("agent@bot.example.com", []string{"a@b.test"}, nil,
		"Hello", longLine, "text/plain", "", nil, "relay.e2a.dev", "", "")
	if err != nil {
		t.Fatalf("ComposeMessage failed: %v", err)
	}
	if got := strings.TrimSpace(mailparse.Parse(raw, 1<<20).Text); got != longLine {
		t.Errorf("round trip lost data: got %d chars, want %d", len(got), len(longLine))
	}
}

func TestHasOverlongLine(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"empty", "", false},
		{"exactly at the limit", strings.Repeat("a", maxLineOctets), false},
		{"one octet over", strings.Repeat("a", maxLineOctets+1), true},
		{"at the limit with CRLF", strings.Repeat("a", maxLineOctets) + "\r\n", false},
		{"over the limit with CRLF", strings.Repeat("a", maxLineOctets+1) + "\r\n", true},
		{"CR is discounted, not counted", strings.Repeat("a", maxLineOctets) + "\r\nshort", false},
		{"many short lines", strings.Repeat("short\r\n", 1000), false},
		{"long line in the middle", "ok\r\n" + strings.Repeat("a", 2000) + "\r\nok", true},
		{"long line last, no trailing newline", "ok\r\n" + strings.Repeat("a", 2000), true},
		{"bare LF endings still measured", "ok\n" + strings.Repeat("a", 2000) + "\nok", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasOverlongLine(tc.body); got != tc.want {
				t.Errorf("hasOverlongLine = %v, want %v", got, tc.want)
			}
		})
	}
}

// Quoted-printable has escaping rules of its own beyond the 8-bit bytes
// that trigger it: '=' is the escape character, and trailing whitespace
// must be encoded so transports that strip it cannot alter the content.
func TestEncodeBodyQuotedPrintableFidelity(t *testing.T) {
	// The accent forces QP; everything else exercises QP's escaping.
	body := "café\nequals = sign\ttab\ntrailing spaces   \nend"

	encoding, encoded := encodeBody(body)
	if encoding != "quoted-printable" {
		t.Fatalf("encoding = %q, want quoted-printable", encoding)
	}

	// QP normalises bare LF to CRLF. That is the canonical wire form, and
	// relaxed DKIM canonicalisation absorbs it — see compose_dkim_test.go.
	want := strings.ReplaceAll(body, "\n", "\r\n")
	if got := decodeQP(t, encoded); got != want {
		t.Errorf("round trip = %q, want %q", got, want)
	}

	if !strings.Contains(encoded, "=20") {
		t.Errorf("trailing space was not encoded, so a transport could strip it:\n%s", encoded)
	}
	// Soft wraps keep every emitted line comfortably inside the RFC 2045
	// 76-octet guidance, which is what makes an MTA wrap unnecessary.
	for i, line := range strings.Split(encoded, "\r\n") {
		if len(line) > 76 {
			t.Errorf("QP line %d is %d octets, want <= 76", i, len(line))
		}
	}
}

func TestComposeEmptyBodyIsWellFormed(t *testing.T) {
	raw, err := ComposeMessage(
		"agent@bot.example.com", []string{"alice@gmail.com"}, nil,
		"Hello", "", "text/plain", "", nil, "relay.e2a.dev", "", "",
	)
	if err != nil {
		t.Fatalf("ComposeMessage failed: %v", err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("empty-body message does not parse: %v", err)
	}
	if got := msg.Header.Get("Content-Transfer-Encoding"); got != "7bit" {
		t.Errorf("Content-Transfer-Encoding = %q, want 7bit", got)
	}
	if got := splitBody(t, raw); got != "" {
		t.Errorf("body = %q, want empty", got)
	}
}

// Every body part must carry an explicit Content-Transfer-Encoding. A part
// without one falls back to the RFC 2045 § 6.1 default of 7bit, which is
// the exact defect this package was fixing.
func TestEveryBodyPartDeclaresAnEncoding(t *testing.T) {
	raw, err := ComposeMessageWithAttachments(
		"agent@bot.example.com", []string{"alice@gmail.com"}, nil,
		"Hello", nonASCIIBody, nonASCIIHTML, "", nil, "relay.e2a.dev", "", "",
		[]Attachment{{Filename: "a.txt", ContentType: "text/plain", Data: "aGk="}},
	)
	if err != nil {
		t.Fatalf("ComposeMessageWithAttachments failed: %v", err)
	}

	body := splitBody(t, raw)
	contentTypes := strings.Count(body, "Content-Type: text/") + strings.Count(body, "Content-Type: application/")
	encodings := strings.Count(body, "Content-Transfer-Encoding: ")
	// text/plain, text/html and the attachment each declare one; the
	// nested multipart/alternative container legitimately does not.
	if encodings != 3 {
		t.Errorf("got %d Content-Transfer-Encoding headers for %d leaf parts, want 3\n%s",
			encodings, contentTypes, body)
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
		{"canonical CRLF", "line one\r\nline two", "7bit"},
		{"NUL is not valid 7bit data", "before\x00after", "quoted-printable"},
		{"bare CR is not valid 7bit data", "line one\rline two", "quoted-printable"},
		{"encoded byte between CR and LF", "\r\x00\n", "quoted-printable"},
		{"bare LF is canonicalised by SMTP", "line one\nline two", "7bit"},
		{"em dash", "a — b", "quoted-printable"},
		{"curly apostrophe", "it’s", "quoted-printable"},
		{"emoji", "ship it \U0001F680", "quoted-printable"},
		{"latin-1 accent", "café", "quoted-printable"},
		// RFC 5322 § 2.1.1 caps a line at 998 octets. At the limit the
		// 7bit fast path is still safe; one octet over, a downstream MTA
		// would wrap and break the DKIM body hash, so encode instead.
		{"ascii line exactly at the limit", strings.Repeat("a", 998), "7bit"},
		{"ascii line one octet over", strings.Repeat("a", 999), "quoted-printable"},
		{"long line among short ones", "ok\r\n" + strings.Repeat("b", 1200) + "\r\nok", "quoted-printable"},
		{"many short ascii lines stay 7bit", strings.Repeat("short line\r\n", 500), "7bit"},
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
			canonical := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(tc.body)
			canonical = strings.ReplaceAll(canonical, "\n", "\r\n")
			if got := decodeQP(t, encoded); got != canonical {
				t.Errorf("round trip = %q, want %q", got, canonical)
			}
		})
	}
}
