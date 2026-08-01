package outbound

import (
	"fmt"
	"strings"
	"testing"
)

func TestBuildReplyQuoteBodyBasic(t *testing.T) {
	ctx := ForwardContext{
		From: "gretta@example.com",
		Date: "Thu, 31 Jul 2026 03:30:47 +0000",
		Text: "line one\r\nline two",
	}
	got := BuildReplyQuoteBody("Thanks, agreed.", ctx)
	want := "Thanks, agreed.\r\n\r\nOn Thu, 31 Jul 2026 03:30:47 +0000, gretta@example.com wrote:\r\n> line one\r\n> line two\r\n"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestBuildReplyQuoteBodyNestsExistingQuotes(t *testing.T) {
	ctx := ForwardContext{
		From: "a@example.com",
		Text: "new text\n> older text\n>> oldest",
	}
	got := BuildReplyQuoteBody("Top.", ctx)
	for _, want := range []string{"> new text\r\n", ">> older text\r\n", ">>> oldest\r\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%q", want, got)
		}
	}
}

func TestBuildReplyQuoteBodyEmptyParentIsVerbatim(t *testing.T) {
	if got := BuildReplyQuoteBody("Body only.", ForwardContext{From: "x@example.com"}); got != "Body only." {
		t.Fatalf("expected verbatim body, got %q", got)
	}
}

func TestBuildReplyQuoteHTMLBodyPrefersParentHTML(t *testing.T) {
	ctx := ForwardContext{
		From: "a@example.com",
		Date: "Thu, 31 Jul 2026 03:30:47 +0000",
		Text: "plain",
		HTML: "<p>rich &amp; original</p>",
	}
	got := BuildReplyQuoteHTMLBody("<p>reply</p>", ctx)
	if !strings.Contains(got, "<blockquote") || !strings.Contains(got, "<p>rich &amp; original</p>") {
		t.Fatalf("expected blockquote with parent HTML, got:\n%q", got)
	}
	if !strings.Contains(got, "On Thu, 31 Jul 2026 03:30:47 +0000, a@example.com wrote:") {
		t.Fatalf("missing attribution, got:\n%q", got)
	}
}

func TestBuildReplyQuoteHTMLBodyFallsBackToEscapedText(t *testing.T) {
	ctx := ForwardContext{From: "a@example.com", Text: "1 < 2 & 3"}
	got := BuildReplyQuoteHTMLBody("<p>reply</p>", ctx)
	if !strings.Contains(got, "<pre>1 &lt; 2 &amp; 3</pre>") {
		t.Fatalf("expected escaped text fallback, got:\n%q", got)
	}
}

func TestBuildReplyQuoteAttributionDegrades(t *testing.T) {
	if got := BuildReplyQuoteAttribution(ForwardContext{}); got != "the sender wrote:" {
		t.Fatalf("got %q", got)
	}
	if got := BuildReplyQuoteAttribution(ForwardContext{From: "b@example.com"}); got != "b@example.com wrote:" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractBodyPartsDepthBounded(t *testing.T) {
	// A pathologically nested multipart must terminate at the depth bound
	// rather than recursing per level. Build 200 genuinely nested layers
	// (distinct boundaries), innermost a text part.
	nest := func(depth int) string {
		body := "Content-Type: text/plain\r\n\r\ndeep\r\n"
		for i := depth - 1; i >= 0; i-- {
			b := fmt.Sprintf("b%d", i)
			body = "Content-Type: multipart/mixed; boundary=" + b + "\r\n\r\n" +
				"--" + b + "\r\n" + body + "--" + b + "--\r\n"
		}
		return body
	}
	deep := []byte("From: a@x.com\r\n" + nest(200))
	ctx := ExtractForwardContext(deep)
	if ctx.Text != "" {
		t.Fatalf("expected depth bound to cut extraction, got Text=%q", ctx.Text)
	}
	// Sanity: the same shape within the bound still extracts.
	shallow := []byte("From: a@x.com\r\n" + nest(3))
	if got := ExtractForwardContext(shallow).Text; !strings.Contains(got, "deep") {
		t.Fatalf("shallow nesting should extract, got %q", got)
	}
}
