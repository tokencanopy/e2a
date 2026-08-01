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
