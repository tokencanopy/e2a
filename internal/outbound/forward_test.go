package outbound

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestBuildForwardSubject(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Hello", "Fwd: Hello"},
		{"  Hello  ", "Fwd: Hello"},
		{"", "Fwd: (no subject)"},
		{"Fwd: Hello", "Fwd: Hello"},
		{"fwd: lower", "fwd: lower"},
		{"Fw: short form", "Fw: short form"},
		{"FW: caps", "FW: caps"},
	}
	for _, c := range cases {
		got := BuildForwardSubject(c.in)
		if got != c.want {
			t.Errorf("BuildForwardSubject(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExtractForwardContext_TextPlain(t *testing.T) {
	raw := []byte("From: alice@example.com\r\n" +
		"To: agent@e2a.dev\r\n" +
		"Subject: Hi\r\n" +
		"Date: Mon, 1 Jan 2026 10:00:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"hello world")

	ctx := ExtractForwardContext(raw)
	if ctx.From != "alice@example.com" {
		t.Errorf("From = %q", ctx.From)
	}
	if ctx.Subject != "Hi" {
		t.Errorf("Subject = %q", ctx.Subject)
	}
	if ctx.To != "agent@e2a.dev" {
		t.Errorf("To = %q", ctx.To)
	}
	if ctx.Text != "hello world" {
		t.Errorf("Text = %q", ctx.Text)
	}
	if ctx.HTML != "" {
		t.Errorf("HTML = %q, want empty", ctx.HTML)
	}
}

func TestExtractForwardContext_MultipartAlternative(t *testing.T) {
	raw := []byte("From: alice@example.com\r\n" +
		"Subject: Hi\r\n" +
		"Content-Type: multipart/alternative; boundary=\"BOUND\"\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"plain body\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>html body</p>\r\n" +
		"--BOUND--\r\n")

	ctx := ExtractForwardContext(raw)
	if !strings.Contains(ctx.Text, "plain body") {
		t.Errorf("Text = %q, want contains 'plain body'", ctx.Text)
	}
	if !strings.Contains(ctx.HTML, "<p>html body</p>") {
		t.Errorf("HTML = %q, want contains '<p>html body</p>'", ctx.HTML)
	}
}

func TestExtractForwardContext_QuotedPrintable(t *testing.T) {
	raw := []byte("From: alice@example.com\r\n" +
		"Subject: Hi\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"caf=C3=A9")

	ctx := ExtractForwardContext(raw)
	if ctx.Text != "café" {
		t.Errorf("Text = %q, want 'café'", ctx.Text)
	}
}

func TestExtractForwardContext_MultipartMixedWrappingAlternative(t *testing.T) {
	raw := []byte("From: alice@example.com\r\n" +
		"Subject: Hi\r\n" +
		"Content-Type: multipart/mixed; boundary=\"OUTER\"\r\n" +
		"\r\n" +
		"--OUTER\r\n" +
		"Content-Type: multipart/alternative; boundary=\"INNER\"\r\n" +
		"\r\n" +
		"--INNER\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"inner text\r\n" +
		"--INNER\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>inner html</p>\r\n" +
		"--INNER--\r\n" +
		"--OUTER\r\n" +
		"Content-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=\"a.pdf\"\r\n" +
		"\r\n" +
		"PDFBINARY\r\n" +
		"--OUTER--\r\n")

	ctx := ExtractForwardContext(raw)
	if !strings.Contains(ctx.Text, "inner text") {
		t.Errorf("Text = %q, want contains 'inner text'", ctx.Text)
	}
	if !strings.Contains(ctx.HTML, "inner html") {
		t.Errorf("HTML = %q, want contains 'inner html'", ctx.HTML)
	}
}

func TestForwardAttachments_CarriesOriginalParts(t *testing.T) {
	pdf := []byte("PDF-DATA-BYTES")
	raw := []byte("From: alice@example.com\r\n" +
		"Subject: Invoice\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"see attached\r\n" +
		"--B\r\n" +
		"Content-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=\"invoice.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		base64.StdEncoding.EncodeToString(pdf) + "\r\n" +
		"--B--\r\n")

	atts := ForwardAttachments(raw)
	if len(atts) != 1 {
		t.Fatalf("ForwardAttachments returned %d parts, want 1", len(atts))
	}
	if atts[0].Filename != "invoice.pdf" {
		t.Errorf("Filename = %q, want invoice.pdf", atts[0].Filename)
	}
	if atts[0].ContentType != "application/pdf" {
		t.Errorf("ContentType = %q, want application/pdf", atts[0].ContentType)
	}
	// Data must be base64 of the decoded original bytes (re-encoded for the
	// outbound Attachment shape).
	if got, want := atts[0].Data, base64.StdEncoding.EncodeToString(pdf); got != want {
		t.Errorf("Data = %q, want %q", got, want)
	}
}

func TestForwardAttachments_NoneReturnsNil(t *testing.T) {
	raw := []byte("From: alice@example.com\r\n" +
		"Subject: Plain\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"no attachments here\r\n")
	if atts := ForwardAttachments(raw); atts != nil {
		t.Errorf("ForwardAttachments = %+v, want nil", atts)
	}
}

func TestExtractForwardContext_MalformedReturnsEmpty(t *testing.T) {
	ctx := ExtractForwardContext([]byte("not an email"))
	if ctx.From != "" || ctx.Text != "" || ctx.HTML != "" {
		t.Errorf("malformed input produced non-empty fields: %+v", ctx)
	}
}

func TestExtractForwardContext_EmptyInput(t *testing.T) {
	ctx := ExtractForwardContext(nil)
	if ctx != (ForwardContext{}) {
		t.Errorf("empty input produced non-zero context: %+v", ctx)
	}
}

func TestBuildForwardBody_WithCommentAndContext(t *testing.T) {
	ctx := ForwardContext{
		From:    "alice@example.com",
		Date:    "Mon, 1 Jan 2026 10:00:00 +0000",
		Subject: "Hi",
		To:      "agent@e2a.dev",
		Text:    "hello world",
	}
	body := BuildForwardBody("FYI", ctx)
	wantSubstrings := []string{
		"FYI",
		"---------- Forwarded message ---------",
		"From: alice@example.com",
		"Date: Mon, 1 Jan 2026 10:00:00 +0000",
		"Subject: Hi",
		"To: agent@e2a.dev",
		"hello world",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(body, s) {
			t.Errorf("body missing %q\nfull body:\n%s", s, body)
		}
	}
	if strings.Contains(body, "Cc:") {
		t.Errorf("body has Cc line when ctx.Cc is empty")
	}
}

func TestBuildForwardBody_PreservesCRLFLineEndings(t *testing.T) {
	// Regression: a naive strings.ReplaceAll("\n","\r\n") turns
	// already-CRLF input into "\r\r\n", which renders as a literal
	// CR character in some mail clients. Real inbound bodies are
	// CRLF-terminated, so this would fire on most forwards.
	ctx := ForwardContext{From: "alice@example.com", Text: "line1\r\nline2\r\nline3\r\n"}
	body := BuildForwardBody("", ctx)
	if strings.Contains(body, "\r\r\n") {
		t.Errorf("body contains malformed \\r\\r\\n sequence:\n%q", body)
	}
	if !strings.Contains(body, "line1\r\nline2\r\nline3\r\n") {
		t.Errorf("body lost CRLF line endings:\n%q", body)
	}
}

func TestBuildForwardBody_NormalizesLFOnlyInput(t *testing.T) {
	// LF-only input must still be re-emitted as CRLF (SMTP requirement).
	ctx := ForwardContext{From: "alice@example.com", Text: "line1\nline2\nline3"}
	body := BuildForwardBody("", ctx)
	if strings.Contains(body, "\r\r\n") {
		t.Errorf("body contains malformed \\r\\r\\n sequence:\n%q", body)
	}
	if !strings.Contains(body, "line1\r\nline2\r\nline3\r\n") {
		t.Errorf("body did not convert LF to CRLF or add trailing CRLF:\n%q", body)
	}
}

func TestBuildForwardBody_NoCommentNoOriginalBody(t *testing.T) {
	ctx := ForwardContext{From: "alice@example.com", Subject: "Hi"}
	body := BuildForwardBody("", ctx)
	if !strings.HasPrefix(body, "---------- Forwarded message ---------") {
		t.Errorf("body should start with divider when no comment, got: %q", body)
	}
}

func TestBuildForwardHTMLBody_PrefersHTMLOverText(t *testing.T) {
	ctx := ForwardContext{
		From: "alice@example.com",
		HTML: "<p>original html</p>",
		Text: "plain fallback",
	}
	html := BuildForwardHTMLBody("<p>FYI</p>", ctx)
	if !strings.Contains(html, "<p>original html</p>") {
		t.Errorf("html body missing original HTML: %s", html)
	}
	if strings.Contains(html, "plain fallback") {
		t.Errorf("html body shouldn't fall back to text when HTML present")
	}
	if !strings.Contains(html, "<p>FYI</p>") {
		t.Errorf("html body missing caller comment")
	}
}

func TestBuildForwardHTMLBody_FallsBackToTextInPre(t *testing.T) {
	ctx := ForwardContext{From: "alice@example.com", Text: "plain only"}
	html := BuildForwardHTMLBody("", ctx)
	if !strings.Contains(html, "<pre>plain only</pre>") {
		t.Errorf("html body should wrap text fallback in <pre>: %s", html)
	}
}

func TestBuildForwardHTMLBody_EscapesHTMLSpecialsInHeaders(t *testing.T) {
	ctx := ForwardContext{From: `"Eve <evil@x>" <alice@example.com>`, Subject: "<script>alert(1)</script>"}
	html := BuildForwardHTMLBody("", ctx)
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Errorf("html body must escape <script> in subject: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Errorf("html body should contain escaped &lt;script&gt;: %s", html)
	}
}

func TestBuildForwardBodyDepthBounded(t *testing.T) {
	// maxExtractMIMEDepth caps the multipart recursion the forward quote shares
	// with the reply quote path. Kept in forward_test.go — not quote_test.go —
	// so the GA forward hardening stays covered even if the experimental
	// reply-quote feature is ever reverted. A pathologically nested parent
	// still yields the header block (no crash, no quoted body); nesting within
	// the bound quotes normally.
	nest := func(depth int) string {
		body := "Content-Type: text/plain\r\n\r\ndeep\r\n"
		for i := depth - 1; i >= 0; i-- {
			b := fmt.Sprintf("b%d", i)
			body = "Content-Type: multipart/mixed; boundary=" + b + "\r\n\r\n" +
				"--" + b + "\r\n" + body + "--" + b + "--\r\n"
		}
		return body
	}

	deep := "From: a@x.com\r\n" + nest(200)
	if got := BuildForwardBody("", ExtractForwardContext([]byte(deep))); strings.Contains(got, "deep") {
		t.Fatalf("depth bound must cut the quoted body, got:\n%q", got)
	}

	shallow := "From: a@x.com\r\n" + nest(3)
	if got := BuildForwardBody("", ExtractForwardContext([]byte(shallow))); !strings.Contains(got, "deep") {
		t.Fatalf("shallow nesting must quote the body, got:\n%q", got)
	}
}
