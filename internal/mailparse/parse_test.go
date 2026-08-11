package mailparse

import (
	"strings"
	"testing"
	"time"
)

func TestParsedBody(t *testing.T) {
	t.Run("plain text passes through", func(t *testing.T) {
		raw := "From: a@x.com\r\nTo: b@y.com\r\nSubject: Hi\r\nContent-Type: text/plain\r\n\r\nHello there.\r\n"
		got, trunc := ParsedBody([]byte(raw), 0)
		if got != "Hello there." || trunc {
			t.Fatalf("got %q trunc=%v", got, trunc)
		}
	})

	t.Run("html rendered to text", func(t *testing.T) {
		raw := "From: a@x.com\r\nContent-Type: text/html\r\n\r\n<html><body><p>Hello</p><script>evil()</script><p>World</p></body></html>"
		got, _ := ParsedBody([]byte(raw), 0)
		if !strings.Contains(got, "Hello") || !strings.Contains(got, "World") {
			t.Fatalf("expected Hello/World, got %q", got)
		}
		if strings.Contains(got, "evil") {
			t.Fatalf("script content must be dropped, got %q", got)
		}
	})

	t.Run("multipart prefers text/plain", func(t *testing.T) {
		raw := "From: a@x.com\r\nContent-Type: multipart/alternative; boundary=B\r\n\r\n" +
			"--B\r\nContent-Type: text/plain\r\n\r\nPlain version.\r\n" +
			"--B\r\nContent-Type: text/html\r\n\r\n<p>HTML version</p>\r\n--B--\r\n"
		got, _ := ParsedBody([]byte(raw), 0)
		if got != "Plain version." {
			t.Fatalf("expected plain part, got %q", got)
		}
	})

	t.Run("quoted-printable decoded", func(t *testing.T) {
		raw := "From: a@x.com\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nCaf=C3=A9 time"
		got, _ := ParsedBody([]byte(raw), 0)
		if got != "Café time" {
			t.Fatalf("expected decoded café, got %q", got)
		}
	})

	t.Run("strips quoted reply (On ... wrote:)", func(t *testing.T) {
		raw := "From: a@x.com\r\nContent-Type: text/plain\r\n\r\nMy reply.\r\n\r\nOn Mon, Jan 1, 2026, Bob <b@y.com> wrote:\r\n> original\r\n> more original\r\n"
		got, _ := ParsedBody([]byte(raw), 0)
		if got != "My reply." {
			t.Fatalf("expected only the reply, got %q", got)
		}
	})

	t.Run("strips Outlook original-message block", func(t *testing.T) {
		raw := "From: a@x.com\r\nContent-Type: text/plain\r\n\r\nTop post.\r\n\r\n-----Original Message-----\r\nFrom: someone\r\nblah\r\n"
		got, _ := ParsedBody([]byte(raw), 0)
		if got != "Top post." {
			t.Fatalf("expected top post, got %q", got)
		}
	})

	t.Run("length cap truncates", func(t *testing.T) {
		body := strings.Repeat("a", 100)
		raw := "From: a@x.com\r\nContent-Type: text/plain\r\n\r\n" + body
		got, trunc := ParsedBody([]byte(raw), 20)
		if !trunc || len(got) > 20 {
			t.Fatalf("expected truncation to <=20, got len=%d trunc=%v", len(got), trunc)
		}
	})

	t.Run("malformed message degrades to raw, no panic", func(t *testing.T) {
		got, _ := ParsedBody([]byte("not a real email at all"), 0)
		if got == "" {
			t.Fatal("expected best-effort text, got empty")
		}
	})
}

// TestDeepNestingBounded pins the adversarial DoS fix: a deeply nested
// multipart message must parse quickly (depth-capped), not blow up O(depth²).
func TestDeepNestingBounded(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("From: a@x.com\r\nContent-Type: multipart/mixed; boundary=b0\r\n\r\n")
	for i := 0; i < 5000; i++ {
		sb.WriteString("--b0\r\nContent-Type: multipart/mixed; boundary=b0\r\n\r\n")
	}
	sb.WriteString("--b0\r\nContent-Type: text/plain\r\n\r\ndeep\r\n")
	done := make(chan struct{})
	go func() { _, _ = ParsedBody([]byte(sb.String()), 0); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ParsedBody did not return within 3s on deeply nested multipart (DoS guard missing)")
	}
}

// TestNestedMultipartRecoversInnerText: mixed wrapping alternative → inner plain.
func TestNestedMultipartRecoversInnerText(t *testing.T) {
	raw := "From: a@x.com\r\nContent-Type: multipart/mixed; boundary=OUT\r\n\r\n" +
		"--OUT\r\nContent-Type: multipart/alternative; boundary=IN\r\n\r\n" +
		"--IN\r\nContent-Type: text/plain\r\n\r\ninner plain\r\n" +
		"--IN\r\nContent-Type: text/html\r\n\r\n<p>inner html</p>\r\n--IN--\r\n" +
		"--OUT--\r\n"
	got, _ := ParsedBody([]byte(raw), 0)
	if got != "inner plain" {
		t.Fatalf("nested multipart: got %q, want %q", got, "inner plain")
	}
}

// TestBase64Decoded: a base64 text/plain part is decoded.
func TestBase64Decoded(t *testing.T) {
	// "Hello base64" base64 = SGVsbG8gYmFzZTY0
	raw := "From: a@x.com\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nSGVsbG8gYmFzZTY0\r\n"
	got, _ := ParsedBody([]byte(raw), 0)
	if got != "Hello base64" {
		t.Fatalf("base64 decode: got %q", got)
	}
}

func TestParseHTML(t *testing.T) {
	t.Run("text/html part surfaces decoded HTML, text flattened", func(t *testing.T) {
		raw := "From: a@x.com\r\nContent-Type: text/html\r\n\r\n<div>Hello<br><b>World</b></div>"
		v := Parse([]byte(raw), 0)
		if v.HTML != "<div>Hello<br><b>World</b></div>" {
			t.Fatalf("HTML not preserved verbatim, got %q", v.HTML)
		}
		// Text view stays the flattened, tag-free representation.
		if strings.Contains(v.Text, "<") || !strings.Contains(v.Text, "World") {
			t.Fatalf("text should be flattened, got %q", v.Text)
		}
	})

	t.Run("multipart/alternative: text from plain, html from html part", func(t *testing.T) {
		raw := "From: a@x.com\r\nContent-Type: multipart/alternative; boundary=B\r\n\r\n" +
			"--B\r\nContent-Type: text/plain\r\n\r\nPlain version.\r\n" +
			"--B\r\nContent-Type: text/html\r\n\r\n<p>HTML <i>version</i></p>\r\n--B--\r\n"
		v := Parse([]byte(raw), 0)
		if v.Text != "Plain version." {
			t.Fatalf("text: got %q, want plain part", v.Text)
		}
		if v.HTML != "<p>HTML <i>version</i></p>" {
			t.Fatalf("html: got %q", v.HTML)
		}
	})

	t.Run("quoted-printable HTML decoded", func(t *testing.T) {
		// Mirrors the screenshot bug: soft-break '=' and entity must decode.
		raw := "From: a@x.com\r\nContent-Type: text/html\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<div>Caf=C3=A9 =\r\ntime</div>"
		v := Parse([]byte(raw), 0)
		if v.HTML != "<div>Café time</div>" {
			t.Fatalf("QP HTML decode: got %q", v.HTML)
		}
	})

	t.Run("plain-text-only message has no HTML", func(t *testing.T) {
		raw := "From: a@x.com\r\nContent-Type: text/plain\r\n\r\nJust text.\r\n"
		v := Parse([]byte(raw), 0)
		if v.HTML != "" {
			t.Fatalf("expected empty HTML for text-only message, got %q", v.HTML)
		}
	})

	t.Run("attached .html file is not surfaced as the display body", func(t *testing.T) {
		// multipart/mixed: a text/plain body + a text/html ATTACHMENT. The
		// attachment must not become parsed.html (it's a file, not the body).
		raw := "From: a@x.com\r\nContent-Type: multipart/mixed; boundary=M\r\n\r\n" +
			"--M\r\nContent-Type: text/plain\r\n\r\nThe real body.\r\n" +
			"--M\r\nContent-Type: text/html\r\nContent-Disposition: attachment; filename=report.html\r\n\r\n" +
			"<h1>Attached report</h1>\r\n--M--\r\n"
		v := Parse([]byte(raw), 0)
		if v.Text != "The real body." {
			t.Fatalf("text: got %q, want the plain body", v.Text)
		}
		if v.HTML != "" {
			t.Fatalf("html attachment must not surface as display body, got %q", v.HTML)
		}
	})

	t.Run("inline text/html sibling of an attachment still wins", func(t *testing.T) {
		// multipart/mixed: text/html body (no disposition) + a non-HTML
		// attachment. The body HTML must still be selected.
		raw := "From: a@x.com\r\nContent-Type: multipart/mixed; boundary=M\r\n\r\n" +
			"--M\r\nContent-Type: text/html\r\n\r\n<p>Body here</p>\r\n" +
			"--M\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=x.pdf\r\n\r\n%PDF-1.4\r\n--M--\r\n"
		v := Parse([]byte(raw), 0)
		if v.HTML != "<p>Body here</p>" {
			t.Fatalf("html body: got %q", v.HTML)
		}
	})

	t.Run("HTML capped at MaxHTMLBytes", func(t *testing.T) {
		body := "<p>" + strings.Repeat("a", MaxHTMLBytes+1000) + "</p>"
		raw := "From: a@x.com\r\nContent-Type: text/html\r\n\r\n" + body
		v := Parse([]byte(raw), 0)
		if len(v.HTML) > MaxHTMLBytes {
			t.Fatalf("HTML not capped: len=%d", len(v.HTML))
		}
	})
}

// HTMLToText is the exported rendering seam used by outbound reply quoting
// (internal/outbound/quote.go) to quote an HTML-only parent into a text/plain
// body. Pinned here because that caller depends on the normalization, not
// just the tokenization.
func TestHTMLToText(t *testing.T) {
	t.Run("decodes entities and breaks at block boundaries", func(t *testing.T) {
		got := HTMLToText("<p>first &amp; foremost</p><p>second</p>")
		if !strings.Contains(got, "first & foremost") || !strings.Contains(got, "second") {
			t.Fatalf("got %q", got)
		}
		if strings.Contains(got, "&amp;") {
			t.Fatalf("entities must be decoded, got %q", got)
		}
	})

	t.Run("drops script and style content", func(t *testing.T) {
		got := HTMLToText("<style>p{color:red}</style><script>evil()</script><p>visible</p>")
		if strings.Contains(got, "evil()") || strings.Contains(got, "color:red") {
			t.Fatalf("script/style must not surface, got %q", got)
		}
		if !strings.Contains(got, "visible") {
			t.Fatalf("body text missing, got %q", got)
		}
	})

	t.Run("collapses the blank-line runs block tags produce", func(t *testing.T) {
		got := HTMLToText("<div><div><p>one</p></div></div><p>two</p>")
		if strings.Contains(got, "\n\n\n") {
			t.Fatalf("blank runs must collapse, got %q", got)
		}
	})

	t.Run("markup with no text renders empty", func(t *testing.T) {
		if got := strings.TrimSpace(HTMLToText(`<img src="cid:logo"><br>`)); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
}
