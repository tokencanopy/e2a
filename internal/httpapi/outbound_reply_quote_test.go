package httpapi

import (
	"strings"
	"testing"
)

// The quote_history flag rewrites the delivered body at accept time; these
// tests pin the composed shape at the handler boundary via the captured
// SendRequest (fixture msg_in1: From alice@x.com, no Date header, body "hi").

func TestReplyQuoteHistoryComposesBody(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/reply", "good",
		map[string]any{"text": "thanks", "quote_history": true})
	if code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	got := lastDeliveredReq().Body
	want := "thanks\r\n\r\nalice@x.com wrote:\r\n> hi\r\n"
	if got != want {
		t.Fatalf("delivered Body = %q, want %q", got, want)
	}
}

func TestReplyQuoteHistoryHTMLFallsBackToEscapedText(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/reply", "good",
		map[string]any{"text": "thanks", "html": "<p>thanks</p>", "quote_history": true})
	if code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	html := lastDeliveredReq().HTMLBody
	if !strings.Contains(html, "<p>thanks</p>") || !strings.Contains(html, "<blockquote") || !strings.Contains(html, "<pre>hi</pre>") {
		t.Fatalf("delivered HTMLBody missing quoted block, got %q", html)
	}
}

// An HTML-only parent must quote into BOTH alternatives of the reply. The
// text part is derived from the parent's HTML; without that derivation the
// two alternatives disagree and whether the recipient sees the thread depends
// on which one their client renders.
func TestReplyQuoteHistoryHTMLOnlyParentQuotesBothParts(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in_htmlonly/reply", "good",
		map[string]any{"text": "thanks", "html": "<p>thanks</p>", "quote_history": true})
	if code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	req := lastDeliveredReq()
	// Entities are decoded on the way into the text part ("&amp;" → "&").
	wantText := "thanks\r\n\r\nalice@x.com wrote:\r\n> hi & hello\r\n"
	if req.Body != wantText {
		t.Fatalf("delivered Body = %q, want %q", req.Body, wantText)
	}
	// The HTML part still quotes the parent's own markup verbatim.
	if !strings.Contains(req.HTMLBody, "<blockquote") || !strings.Contains(req.HTMLBody, "<p>hi &amp; hello</p>") {
		t.Fatalf("delivered HTMLBody missing quoted parent markup, got %q", req.HTMLBody)
	}
}

// A text-only reply to an HTML-only parent still gets its history — the
// derivation is not conditional on the caller supplying html.
func TestReplyQuoteHistoryHTMLOnlyParentTextOnlyReply(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in_htmlonly/reply", "good",
		map[string]any{"text": "thanks", "quote_history": true})
	if code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	req := lastDeliveredReq()
	if !strings.Contains(req.Body, "> hi & hello") {
		t.Fatalf("delivered Body missing derived quote, got %q", req.Body)
	}
	if req.HTMLBody != "" {
		t.Fatalf("a text-only reply must stay text-only, got HTMLBody %q", req.HTMLBody)
	}
}

func TestReplyWithoutQuoteHistoryIsVerbatim(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1/reply", "good",
		map[string]any{"text": "thanks"})
	if code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	if got := lastDeliveredReq().Body; got != "thanks" {
		t.Fatalf("delivered Body = %q, want verbatim %q", got, "thanks")
	}
	if got := lastDeliveredReq().HTMLBody; got != "" {
		t.Fatalf("delivered HTMLBody = %q, want empty", got)
	}
}

func TestReplyQuoteHistoryDateParentComposesAttribution(t *testing.T) {
	// A Date-bearing parent produces the full "On <date>, <sender> wrote:"
	// attribution form. That is the shape mailparse.quoteMarker matches on the
	// receiving side, so a receiving agent's parsed.text drops the quoted block
	// with no residue — the common real-mail round trip that no other quote
	// test exercises (all other fixtures are Date-less and pin the residue
	// path).
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/agents/support%40acme.com/messages/msg_in1dated/reply", "good",
		map[string]any{"text": "thanks", "quote_history": true})
	if code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	want := "thanks\r\n\r\nOn Thu, 31 Jul 2026 03:30:47 +0000, alice@x.com wrote:\r\n> hi\r\n"
	if got := lastDeliveredReq().Body; got != want {
		t.Fatalf("delivered Body = %q, want %q", got, want)
	}
}
