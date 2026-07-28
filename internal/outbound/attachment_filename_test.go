package outbound

import (
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/mailparse"
)

func composeWithAttachment(t *testing.T, filename string) []byte {
	t.Helper()
	raw, err := ComposeMessageWithAttachments(
		"agent@bot.example.com", []string{"alice@gmail.com"}, nil,
		"Hello", "body", "", "", nil, "relay.e2a.dev", "", "",
		[]Attachment{{Filename: filename, ContentType: "application/pdf", Data: "aGk="}},
	)
	if err != nil {
		t.Fatalf("ComposeMessageWithAttachments(%q) failed: %v", filename, err)
	}
	return raw
}

// A MIME parameter value is ASCII-only. A non-ASCII filename must go out
// RFC 2231 encoded; emitting the raw UTF-8 bytes puts an 8-bit value in a
// header field, which receivers may mangle or reject.
func TestAttachmentFilenameNonASCIIIsRFC2231Encoded(t *testing.T) {
	raw := composeWithAttachment(t, "résumé.pdf")

	if !strings.Contains(string(raw), "filename*=utf-8''r%C3%A9sum%C3%A9.pdf") {
		t.Errorf("filename was not RFC 2231 encoded:\n%s", raw)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] > 127 {
			t.Fatalf("byte %d = %#x is 8-bit; the message must be 7-bit clean", i, raw[i])
		}
	}
}

// The receive side must recover the original name, or the encoding has
// simply moved the corruption somewhere else.
func TestAttachmentFilenameRoundTrips(t *testing.T) {
	names := []string{
		"résumé.pdf",
		"报告.pdf",
		"plain.pdf",
		"with space.pdf",
		"naïve — dash.pdf",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			atts := mailparse.Attachments(composeWithAttachment(t, name))
			if len(atts) != 1 {
				t.Fatalf("got %d attachments, want 1", len(atts))
			}
			if atts[0].Filename != name {
				t.Errorf("filename round trip = %q, want %q", atts[0].Filename, name)
			}
		})
	}
}

func TestAttachmentFilenameRFC2231EncodingRespectsLineLimit(t *testing.T) {
	filename := strings.Repeat("界", 120) + ".pdf"
	raw := composeWithAttachment(t, filename)

	for i, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > maxLineOctets {
			t.Fatalf("line %d is %d octets, exceeds the %d-octet limit:\n%s",
				i+1, len(line), maxLineOctets, line)
		}
	}

	atts := mailparse.Attachments(raw)
	if len(atts) != 1 || atts[0].Filename != filename {
		t.Fatalf("continued filename round trip = %+v, want %q", atts, filename)
	}
}

// Values containing MIME specials must stay parseable rather than
// terminating the parameter list early.
func TestAttachmentFilenameWithSpecialsStaysParseable(t *testing.T) {
	for _, name := range []string{`we"ird.pdf`, "semi;colon.pdf", "comma,name.pdf", "equals=name.pdf"} {
		t.Run(name, func(t *testing.T) {
			atts := mailparse.Attachments(composeWithAttachment(t, name))
			if len(atts) != 1 {
				t.Fatalf("got %d attachments, want 1", len(atts))
			}
			if atts[0].Filename != name {
				t.Errorf("filename round trip = %q, want %q", atts[0].Filename, name)
			}
		})
	}
}

// The CR/LF injection guard predates this change and must survive it —
// attachmentDisposition's fallback path writes the filename unencoded.
func TestAttachmentFilenameCRLFStillRejected(t *testing.T) {
	for _, bad := range []string{"a\r\nBcc: evil@example.com", "a\nX-Injected: 1", "a\rb"} {
		_, err := ComposeMessageWithAttachments(
			"agent@bot.example.com", []string{"alice@gmail.com"}, nil,
			"Hello", "body", "", "", nil, "relay.e2a.dev", "", "",
			[]Attachment{{Filename: bad, ContentType: "application/pdf", Data: "aGk="}},
		)
		if err == nil {
			t.Errorf("filename %q was accepted; header injection guard regressed", bad)
		}
	}
}

func TestAttachmentDisposition(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string
	}{
		{"plain ascii", "report.pdf", `attachment; filename="report.pdf"`},
		{"non-ascii is encoded", "résumé.pdf", "attachment; filename*=utf-8''r%C3%A9sum%C3%A9.pdf"},
		{"space is quoted", "my report.pdf", `attachment; filename="my report.pdf"`},
		{"quote is escaped", `we"ird.pdf`, `attachment; filename="we\"ird.pdf"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := attachmentDisposition(tc.filename); got != tc.want {
				t.Errorf("attachmentDisposition(%q) = %q, want %q", tc.filename, got, tc.want)
			}
		})
	}
}
