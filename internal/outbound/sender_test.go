package outbound

import (
	"bytes"
	"context"
	"errors"
	"net/mail"
	"reflect"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/dkim"
	"github.com/tokencanopy/e2a/internal/identity"
)

// TestComposeReplyToOverride pins the Reply-To behavior: absent a caller value the
// header defaults to the agent's own address; a req.ReplyTo overrides it verbatim
// (display name preserved). This is the seam the whole feature rides on — a
// regression here silently misroutes every recipient's replies.
func TestComposeReplyToOverride(t *testing.T) {
	s := NewSender(nil, "example.com")
	agent := &identity.AgentIdentity{ID: "bot@example.com", Domain: "example.com"}

	replyToHeader := func(t *testing.T, req SendRequest) string {
		t.Helper()
		c, err := s.compose(agent, req)
		if err != nil {
			t.Fatalf("compose: %v", err)
		}
		m, err := mail.ReadMessage(bytes.NewReader(c.sentBody))
		if err != nil {
			t.Fatalf("parse composed message: %v", err)
		}
		return m.Header.Get("Reply-To")
	}

	base := SendRequest{To: []string{"x@y.com"}, Subject: "hi", Body: "body text"}

	if got := replyToHeader(t, base); got != agent.EmailAddress() {
		t.Errorf("default Reply-To = %q, want agent address %q", got, agent.EmailAddress())
	}

	withOverride := base
	withOverride.ReplyTo = "Support <support@acme.com>"
	if got := replyToHeader(t, withOverride); got != "Support <support@acme.com>" {
		t.Errorf("overridden Reply-To = %q, want %q", got, "Support <support@acme.com>")
	}
}

// TestComposeForAccept_MatchesSyncComposeBytes pins the load-bearing async/sync
// invariant: the accept path (ComposeForAccept) stores the SAME Sent-folder bytes
// the sync path stores (Send returns compose().sentBody as Raw), and the SES
// config-set header lives only on the wire, never in the stored copy — on BOTH
// paths. A drift here would send different bytes than we retained (or vice-versa).
func TestComposeForAccept_MatchesSyncComposeBytes(t *testing.T) {
	s := NewSender(nil, "example.com")
	agent := &identity.AgentIdentity{ID: "bot@example.com", Domain: "example.com"}
	req := SendRequest{To: []string{"x@y.com"}, Subject: "hi", Body: "body text"}

	c, err := s.compose(agent, req) // the shared compose path
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	cr, err := s.ComposeForAccept(agent, req)
	if err != nil {
		t.Fatalf("ComposeForAccept: %v", err)
	}
	if !bytes.Equal(cr.Raw, c.sentBody) {
		t.Errorf("ComposeForAccept.Raw (%d B) != compose().sentBody (%d B) — async/sync stored-bytes drift", len(cr.Raw), len(c.sentBody))
	}
	if cr.EnvelopeFrom != c.envelopeFrom || cr.SentAs != c.sentAs {
		t.Errorf("envelope/sentAs drift: %q/%q vs %q/%q", cr.EnvelopeFrom, cr.SentAs, c.envelopeFrom, c.sentAs)
	}
	if !bytes.Equal(c.wire, c.sentBody) {
		t.Error("no SES config-set → wire must equal the stored sentBody")
	}

	// With a config-set: the wire gains the header, the stored copy does NOT — on
	// both paths. SubmitOnce re-attaches the same header at submit time.
	s.SetSESConfigurationSet("cfg-set-1")
	c2, err := s.compose(agent, req)
	if err != nil {
		t.Fatalf("compose (cfg): %v", err)
	}
	cr2, err := s.ComposeForAccept(agent, req)
	if err != nil {
		t.Fatalf("ComposeForAccept (cfg): %v", err)
	}
	if !bytes.Equal(cr2.Raw, c2.sentBody) {
		t.Error("with config-set, ComposeForAccept.Raw must still equal the header-free sentBody")
	}
	if bytes.Equal(c2.wire, c2.sentBody) {
		t.Error("with config-set, the wire must carry the X-SES-CONFIGURATION-SET header (differ from stored)")
	}
	if !bytes.Contains(c2.wire, []byte("cfg-set-1")) {
		t.Error("wire missing the config-set header value")
	}
}

// TestApplyCorrelationHeader pins the §3.1 correlation marker: SubmitOnce
// stamps X-E2A-Message-ID at submit time (post-DKIM, wire-only relative to the
// stored raw_message) so SES can echo it back in notifications — the marker
// that makes SMTP-accept↔mark-sent crash-window feedback correlatable.
func TestApplyCorrelationHeader(t *testing.T) {
	body := []byte("From: a@x.com\r\n\r\nhello")

	stamped := applyCorrelationHeader(body, "msg_abc123")
	if !bytes.HasPrefix(stamped, []byte("X-E2A-Message-ID: msg_abc123\r\n")) {
		t.Errorf("stamped message must start with the correlation header, got %q", stamped[:40])
	}
	if !bytes.HasSuffix(stamped, body) {
		t.Error("stamping must prepend, leaving the (DKIM-signed) body untouched")
	}

	// Empty id (defensive): no header.
	if got := applyCorrelationHeader(body, ""); !bytes.Equal(got, body) {
		t.Error("empty message id must not stamp a header")
	}

	// Header-injection defense: CR/LF in the id is neutralized.
	inj := applyCorrelationHeader(body, "msg_a\r\nBcc: leak@evil.com")
	if bytes.Contains(inj, []byte("\r\nBcc:")) {
		t.Error("CR/LF in the message id must be stripped, not smuggled as a header")
	}
}

func TestNormalizeAddrs(t *testing.T) {
	got, err := normalizeAddrs([]string{"Alice@Gmail.COM", " bob@test.com ", ""})
	if err != nil {
		t.Fatalf("normalizeAddrs: %v", err)
	}
	want := []string{"alice@gmail.com", "bob@test.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestNormalizeAddrsDisplayName(t *testing.T) {
	got, err := normalizeAddrs([]string{"Alice <alice@GMAIL.com>"})
	if err != nil {
		t.Fatalf("normalizeAddrs: %v", err)
	}
	want := []string{"alice@gmail.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestNormalizeAddrsInvalid(t *testing.T) {
	_, err := normalizeAddrs([]string{"not-an-email"})
	if err == nil {
		t.Error("expected error for invalid address")
	}
}

func TestDedupe(t *testing.T) {
	got := dedupe([]string{"a@b.com", "c@d.com", "A@B.com", "c@d.com"})
	want := []string{"a@b.com", "c@d.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRemoveAddrs(t *testing.T) {
	got := removeAddrs([]string{"a@b.com", "c@d.com", "e@f.com"}, []string{"c@d.com"})
	want := []string{"a@b.com", "e@f.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCrossFieldDedupe(t *testing.T) {
	// Simulate To > CC > BCC priority
	to := []string{"alice@test.com", "bob@test.com"}
	cc := []string{"bob@test.com", "carol@test.com"}
	bcc := []string{"carol@test.com", "dave@test.com"}

	cc = removeAddrs(cc, to)
	bcc = removeAddrs(bcc, to)
	bcc = removeAddrs(bcc, cc)

	wantCC := []string{"carol@test.com"}
	wantBCC := []string{"dave@test.com"}

	if !reflect.DeepEqual(cc, wantCC) {
		t.Errorf("cc = %v, want %v", cc, wantCC)
	}
	if !reflect.DeepEqual(bcc, wantBCC) {
		t.Errorf("bcc = %v, want %v", bcc, wantBCC)
	}
}

// --- DKIM signing path (Sender.signMessage) ---
//
// The dkim package's unit tests cover keygen, sign/verify roundtrip,
// and TXT extraction. These tests exercise the wiring in
// Sender.signMessage — the four branches that decide whether a
// message gets signed, returns unsigned, or fails open with a log.
//
// The contract is "fail open": ANY problem fetching or applying the
// key must return (nil, false) so the caller proceeds with the
// unsigned message. A bug here that returns an error or panics would
// break outbound mail for every customer with a custom domain.

// fakeDKIMLookup is a test double for DKIMKeyLookup. The function
// fields let each test pick its own response shape without needing
// per-test struct definitions.
type fakeDKIMLookup struct {
	get func(ctx context.Context, domain string) (string, []byte, error)
}

func TestComposeManagedUnsubscribeHeadersAreDKIMSigned(t *testing.T) {
	keypair, err := dkim.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	lookup := &fakeDKIMLookup{get: func(_ context.Context, domain string) (string, []byte, error) {
		if domain != "example.com" {
			t.Fatalf("DKIM lookup domain = %q, want example.com", domain)
		}
		return keypair.Selector, keypair.PrivateKeyDER, nil
	}}
	sender := NewSenderWithDKIM(nil, "example.com", lookup)
	agent := &identity.AgentIdentity{ID: "bot@example.com", Domain: "example.com"}
	composed, err := sender.ComposeForAccept(agent, SendRequest{
		To:      []string{"recipient@example.net"},
		Subject: "managed",
		Body:    "body",
		Unsubscribe: &UnsubscribeOptions{
			Mode: "managed",
			URL:  "https://api.example.com/u/u1_token",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(composed.Raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := message.Header.Get("List-Unsubscribe"); got != "<https://api.example.com/u/u1_token>" {
		t.Fatalf("List-Unsubscribe = %q", got)
	}
	signature := strings.ToLower(message.Header.Get("DKIM-Signature"))
	if !strings.Contains(signature, "list-unsubscribe:list-unsubscribe-post") {
		t.Fatalf("managed headers were not present before signing; DKIM h= %q", signature)
	}
}

func (f *fakeDKIMLookup) GetDKIMKeyInternal(ctx context.Context, domain string) (string, []byte, error) {
	return f.get(ctx, domain)
}

// validTestMessage is an RFC 5322 message stub that the DKIM signer accepts.
// It includes a composer-shaped Date (intentionally unsigned because SES
// replaces it) and a Message-ID (signed when present). The body is short so
// signature canonicalization is cheap to inspect when debugging a failure.
const validTestMessage = "From: bot@example.com\r\n" +
	"To: alice@elsewhere.test\r\n" +
	"Subject: hi\r\n" +
	"Date: Fri, 22 May 2026 12:00:00 +0000\r\n" +
	"Message-ID: <abc@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"hello\r\n"

func TestSignMessage_NoLookupConfigured(t *testing.T) {
	// Plain NewSender → dkimLookup is nil. Must return (nil, false)
	// without touching the message — older deployments and unit
	// tests construct Senders this way and shouldn't break.
	s := NewSender(nil, "example.com")
	signed, ok := s.signMessage([]byte(validTestMessage), "example.com")
	if ok {
		t.Errorf("ok = true, want false (no lookup configured)")
	}
	if signed != nil {
		t.Errorf("signed = %d bytes, want nil", len(signed))
	}
}

func TestSignMessage_EmptyDomainSkipsLookup(t *testing.T) {
	// A pathological caller passing "" as the signing domain must
	// short-circuit rather than calling GetDKIMKey(ctx, ""), which
	// would scan all rows. Defensive guard at the entry point.
	calls := 0
	lookup := &fakeDKIMLookup{
		get: func(_ context.Context, _ string) (string, []byte, error) {
			calls++
			return "sel", []byte("not-a-real-key"), nil
		},
	}
	s := NewSenderWithDKIM(nil, "example.com", lookup)
	_, ok := s.signMessage([]byte(validTestMessage), "")
	if ok {
		t.Errorf("ok = true, want false (empty domain)")
	}
	if calls != 0 {
		t.Errorf("GetDKIMKey called %d times for empty domain, want 0", calls)
	}
}

func TestSignMessage_LookupErrorFailsOpen(t *testing.T) {
	// DB transient error → log + proceed unsigned. The caller treats
	// (nil, false) as "use the original message," so outbound mail
	// keeps flowing while alerting kicks in on the log line.
	lookup := &fakeDKIMLookup{
		get: func(_ context.Context, _ string) (string, []byte, error) {
			return "", nil, errors.New("connection refused")
		},
	}
	s := NewSenderWithDKIM(nil, "example.com", lookup)
	signed, ok := s.signMessage([]byte(validTestMessage), "example.com")
	if ok {
		t.Errorf("ok = true, want false (lookup errored)")
	}
	if signed != nil {
		t.Errorf("signed != nil on lookup error; signMessage must not return half-applied data")
	}
}

func TestSignMessage_NoKeyStoredReturnsUnsigned(t *testing.T) {
	// Pre-migration domain: row exists but DKIM columns are NULL.
	// Store returns ("", nil, nil) — distinct from an error, distinct
	// from a successful lookup. signMessage must treat it the same
	// way it treats the no-lookup case: silently fall through.
	lookup := &fakeDKIMLookup{
		get: func(_ context.Context, _ string) (string, []byte, error) {
			return "", nil, nil
		},
	}
	s := NewSenderWithDKIM(nil, "example.com", lookup)
	signed, ok := s.signMessage([]byte(validTestMessage), "example.com")
	if ok {
		t.Errorf("ok = true, want false (no keypair stored)")
	}
	if signed != nil {
		t.Errorf("signed != nil on missing keypair")
	}
}

func TestSignMessage_SignErrorFailsOpen(t *testing.T) {
	// Store returned a selector + bytes, but the bytes aren't a
	// valid PKCS#1 RSA private key (corruption, partial-write, etc).
	// dkim.Sign returns an error; we log and proceed unsigned. A
	// panic here would crash the outbound goroutine for every send.
	lookup := &fakeDKIMLookup{
		get: func(_ context.Context, _ string) (string, []byte, error) {
			return "e2a202605", []byte("definitely-not-a-key"), nil
		},
	}
	s := NewSenderWithDKIM(nil, "example.com", lookup)
	signed, ok := s.signMessage([]byte(validTestMessage), "example.com")
	if ok {
		t.Errorf("ok = true, want false (sign should fail on garbage key)")
	}
	if signed != nil {
		t.Errorf("signed != nil on sign error")
	}
}

func TestSignMessage_HappyPathPrependsSignatureHeader(t *testing.T) {
	// Generate a real keypair, hand it to the fake lookup, sign the
	// validTestMessage. signMessage must return (signed, true) and
	// the signed bytes must start with "DKIM-Signature:" — the
	// header dkim.Sign prepends per RFC 6376 §3.5.
	kp, err := dkim.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	lookup := &fakeDKIMLookup{
		get: func(_ context.Context, _ string) (string, []byte, error) {
			return kp.Selector, kp.PrivateKeyDER, nil
		},
	}
	s := NewSenderWithDKIM(nil, "example.com", lookup)
	signed, ok := s.signMessage([]byte(validTestMessage), "example.com")
	if !ok {
		t.Fatalf("ok = false, want true on happy path")
	}
	if !bytes.HasPrefix(signed, []byte("DKIM-Signature:")) {
		head := signed
		if len(head) > 80 {
			head = head[:80]
		}
		t.Errorf("signed bytes must begin with DKIM-Signature header; first 80 bytes:\n%s", head)
	}
	// Defense-in-depth: signed message must still contain the
	// original headers + body — the signer prepends, doesn't
	// rewrite.
	if !bytes.Contains(signed, []byte("From: bot@example.com")) {
		t.Errorf("signed bytes lost the original From header")
	}
	if !bytes.Contains(signed, []byte("hello")) {
		t.Errorf("signed bytes lost the original body")
	}
}
