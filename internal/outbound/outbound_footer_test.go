package outbound

import (
	"bytes"
	"context"
	"net/mail"
	"regexp"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/dkim"
	"github.com/tokencanopy/e2a/internal/identity"
)

// The footer content deliberately avoids '=' (quoted-printable escapes it)
// so raw-substring assertions stay robust against body encoding.
const (
	testFooterText = "Created with Example, email for AI agents - https://example.com/footer"
	testFooterHTML = `<p>Created with <a href="https://example.com/footer">Example</a></p>`
)

func footerSender() *Sender {
	s := NewSender(nil, "send.example.com")
	s.SetOutboundFooter(testFooterText, testFooterHTML)
	return s
}

func footerAgent() *identity.AgentIdentity {
	return &identity.AgentIdentity{ID: "bot@example.com", Email: "bot@example.com", Domain: "example.com"}
}

// TestComposeOutboundFooterAppendsToBothParts: the flag appends the text
// footer to the text part and the HTML fragment to the HTML part; without the
// flag the composed bytes are footer-free.
func TestComposeOutboundFooterAppendsToBothParts(t *testing.T) {
	s := footerSender()
	req := SendRequest{To: []string{"user@example.net"}, Subject: "hi", Body: "plain body", HTMLBody: "<p>html body</p>"}

	req.AppendOutboundFooter = true
	on, err := s.ComposeForAccept(footerAgent(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(on.Raw), "Created with"); got != 2 {
		t.Fatalf("footer occurrences = %d, want 2 (text + html part):\n%s", got, on.Raw)
	}
	if !strings.Contains(string(on.Raw), `href="https://example.com/footer"`) {
		t.Fatalf("HTML fragment missing from HTML part:\n%s", on.Raw)
	}

	req.AppendOutboundFooter = false
	off, err := s.ComposeForAccept(footerAgent(), req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(off.Raw), "Created with") {
		t.Fatalf("flag off must not append the footer:\n%s", off.Raw)
	}
}

// TestComposeOutboundFooterEmptyContentIsNoOp: enabled flag with no configured
// content must compose byte-identically to the flag-off message — never error.
func TestComposeOutboundFooterEmptyContentIsNoOp(t *testing.T) {
	s := NewSender(nil, "send.example.com") // no SetOutboundFooter
	req := SendRequest{To: []string{"user@example.net"}, Subject: "hi", Body: "plain body", HTMLBody: "<p>html body</p>"}

	req.AppendOutboundFooter = true
	on, err := s.ComposeForAccept(footerAgent(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.AppendOutboundFooter = false
	off, err := s.ComposeForAccept(footerAgent(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Strip the per-compose Date/Message-ID variance by comparing bodies only.
	onBody := string(on.Raw[strings.Index(string(on.Raw), "\r\n\r\n"):])
	offBody := string(off.Raw[strings.Index(string(off.Raw), "\r\n\r\n"):])
	// Multipart boundaries are random per compose; compare after removing them.
	if stripBoundaries(onBody) != stripBoundaries(offBody) {
		t.Fatalf("empty footer content must be a byte-level no-op:\non=%q\noff=%q", onBody, offBody)
	}
}

var boundaryRe = regexp.MustCompile(`[A-Za-z0-9+/=_-]{16,}`)

func stripBoundaries(s string) string { return boundaryRe.ReplaceAllString(s, "") }

// TestComposeOutboundFooterOrderingAboveUnsubscribe: when both footers apply,
// the branding footer sits ABOVE the managed-unsubscribe line in both parts —
// the unsubscribe line keeps its compliance position as the last body content.
func TestComposeOutboundFooterOrderingAboveUnsubscribe(t *testing.T) {
	s := footerSender()
	got, err := s.ComposeForAccept(footerAgent(), SendRequest{
		To: []string{"user@example.net"}, Subject: "hi", Body: "plain body", HTMLBody: "<p>html body</p>",
		Unsubscribe:          &UnsubscribeOptions{Mode: "managed", URL: "https://api.example.com/u/u1_token"},
		AppendOutboundFooter: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := string(got.Raw)
	const unsub = "Unsubscribe from emails sent by"
	if strings.Count(raw, "Created with") != 2 || strings.Count(raw, unsub) != 2 {
		t.Fatalf("expected both footers in both parts:\n%s", raw)
	}
	// Text part: first occurrences; HTML part: last occurrences.
	if strings.Index(raw, "Created with") > strings.Index(raw, unsub) {
		t.Errorf("text part: branding footer must precede the unsubscribe line:\n%s", raw)
	}
	if strings.LastIndex(raw, "Created with") > strings.LastIndex(raw, unsub) {
		t.Errorf("html part: branding footer must precede the unsubscribe line:\n%s", raw)
	}
}

// TestComposeOutboundFooterWithAttachments: the attachments MIME shape gets the
// footer in both alternative parts too.
func TestComposeOutboundFooterWithAttachments(t *testing.T) {
	s := footerSender()
	got, err := s.ComposeForAccept(footerAgent(), SendRequest{
		To: []string{"user@example.net"}, Subject: "hi", Body: "plain body", HTMLBody: "<p>html body</p>",
		Attachments:          []Attachment{{Filename: "note.txt", ContentType: "text/plain", Data: "aGk="}},
		AppendOutboundFooter: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := string(got.Raw)
	if strings.Count(raw, "Created with") != 2 {
		t.Fatalf("footer occurrences = %d, want 2 in the attachment shape:\n%s", strings.Count(raw, "Created with"), raw)
	}
	if !strings.Contains(raw, `filename="note.txt"`) {
		t.Fatalf("attachment lost:\n%s", raw)
	}
}

// TestComposeOutboundFooterHTMLOnlySend: an HTML-only send (empty text body)
// still gets the text footer in its (otherwise empty) text part, plus the HTML
// fragment — disclosure stays visible to text-part readers.
func TestComposeOutboundFooterHTMLOnlySend(t *testing.T) {
	s := footerSender()
	got, err := s.ComposeForAccept(footerAgent(), SendRequest{
		To: []string{"user@example.net"}, Subject: "hi", HTMLBody: "<p>html only</p>",
		AppendOutboundFooter: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got.Raw), "Created with") != 2 {
		t.Fatalf("HTML-only send must carry the footer in both parts:\n%s", got.Raw)
	}
}

// TestComposeOutboundFooterTextOnlySendSkipsHTMLFragment: a text-only send
// must not grow an HTML part just to carry the HTML fragment.
func TestComposeOutboundFooterTextOnlySendSkipsHTMLFragment(t *testing.T) {
	s := footerSender()
	got, err := s.ComposeForAccept(footerAgent(), SendRequest{
		To: []string{"user@example.net"}, Subject: "hi", Body: "plain body",
		AppendOutboundFooter: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := string(got.Raw)
	if strings.Count(raw, "Created with") != 1 {
		t.Fatalf("text-only send footer occurrences = %d, want 1:\n%s", strings.Count(raw, "Created with"), raw)
	}
	if strings.Contains(raw, "<a href") {
		t.Fatalf("text-only send must not gain an HTML part:\n%s", raw)
	}
	// RFC 3676 signature separator: dash-dash-SPACE-newline, so mail clients
	// trim the footer when quoting a reply. The trailing space is the point.
	if !strings.Contains(raw, "-- \nCreated with") && !strings.Contains(raw, "-- \r\nCreated with") {
		t.Fatalf("footer must follow the RFC 3676 %q separator:\n%q", "-- \\n", raw)
	}
}

// TestComposeOutboundFooterRejectsPostFooterComposedSize: the composed-size
// ceiling is checked AFTER the footer append (same contract as the
// managed-unsubscribe footer) — a message that only fits without the footer
// is rejected.
func TestComposeOutboundFooterRejectsPostFooterComposedSize(t *testing.T) {
	s := footerSender()
	subject := "s"
	req := SendRequest{
		To: []string{"user@example.net"}, Subject: subject,
		Body:                 strings.Repeat("x", MaxComposedMessageBytes-len(subject)),
		AppendOutboundFooter: true,
	}
	if before := ComposedSize(req.Subject, req.Body, req.HTMLBody, req.Attachments); before != MaxComposedMessageBytes {
		t.Fatalf("pre-footer size=%d, want %d", before, MaxComposedMessageBytes)
	}
	if _, err := s.ComposeForAccept(footerAgent(), req); !IsComposedSizeError(err) {
		t.Fatalf("error=%T %v, want composed-size error", err, err)
	}
	// Same message without the flag stays accepted — the rejection above is
	// attributable to the footer bytes alone.
	req.AppendOutboundFooter = false
	if _, err := s.ComposeForAccept(footerAgent(), req); err != nil {
		t.Fatalf("flag-off message at cap rejected: %v", err)
	}
}

// TestComposeOutboundFooterIsDKIMSigned: the footer is appended before DKIM
// signing, so the signed body hash (bh=) covers it — proven by the hash
// changing when the footer is toggled on an otherwise identical message.
func TestComposeOutboundFooterIsDKIMSigned(t *testing.T) {
	keypair, err := dkim.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	lookup := &fakeDKIMLookup{get: func(_ context.Context, _ string) (string, []byte, error) {
		return keypair.Selector, keypair.PrivateKeyDER, nil
	}}
	s := NewSenderWithDKIM(nil, "example.com", lookup)
	s.SetOutboundFooter(testFooterText, testFooterHTML)
	req := SendRequest{To: []string{"user@example.net"}, Subject: "hi", Body: "plain body"}

	bodyHash := func(t *testing.T, raw []byte) string {
		t.Helper()
		m, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		sig := m.Header.Get("DKIM-Signature")
		if sig == "" {
			t.Fatalf("message is not DKIM-signed:\n%s", raw)
		}
		match := regexp.MustCompile(`bh=([^;]+)`).FindStringSubmatch(sig)
		if match == nil {
			t.Fatalf("DKIM-Signature missing bh=: %q", sig)
		}
		return match[1]
	}

	req.AppendOutboundFooter = true
	on, err := s.ComposeForAccept(footerAgent(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(on.Raw), "Created with") {
		t.Fatalf("signed message lost the footer:\n%s", on.Raw)
	}
	req.AppendOutboundFooter = false
	off, err := s.ComposeForAccept(footerAgent(), req)
	if err != nil {
		t.Fatal(err)
	}
	if bodyHash(t, on.Raw) == bodyHash(t, off.Raw) {
		t.Error("DKIM bh= identical with and without the footer — the signature does not cover the footer bytes")
	}
}

// TestComposeOutboundFooterForAcceptMatchesSyncComposeBytes: the async/sync
// stored-bytes invariant holds with the footer applied — ComposeForAccept
// returns byte-identical Sent-folder bytes to the sync compose path (the same
// contract TestComposeForAccept_MatchesSyncComposeBytes pins footer-free).
func TestComposeOutboundFooterForAcceptMatchesSyncComposeBytes(t *testing.T) {
	s := footerSender()
	req := SendRequest{To: []string{"user@example.net"}, Subject: "hi", Body: "plain body", AppendOutboundFooter: true}
	c, err := s.compose(footerAgent(), req)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := s.ComposeForAccept(footerAgent(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cr.Raw, c.sentBody) {
		t.Errorf("ComposeForAccept.Raw (%d B) != compose().sentBody (%d B) — async/sync stored-bytes drift with the footer on", len(cr.Raw), len(c.sentBody))
	}
	if !bytes.Contains(cr.Raw, []byte("Created with")) {
		t.Fatal("compared bytes do not carry the footer — the equality above would be vacuous")
	}
}
