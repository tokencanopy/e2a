package webhooknotify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/dkim"
	"github.com/tokencanopy/e2a/internal/identity"
)

type stubStore struct {
	owner    *identity.User
	ownerErr error
	stats    identity.WebhookFailureStats
	statsErr error
}

func (s *stubStore) GetUserByID(_ context.Context, _ string) (*identity.User, error) {
	return s.owner, s.ownerErr
}

func (s *stubStore) RecentWebhookFailureStats(_ context.Context, _ string, _ time.Duration) (identity.WebhookFailureStats, error) {
	return s.stats, s.statsErr
}

type captureRelay struct {
	from    string
	to      []string
	message []byte
	err     error
}

func (r *captureRelay) SendOnce(from string, to []string, msg []byte) (string, error) {
	r.from, r.to, r.message = from, to, msg
	return "queued-id", r.err
}

func testWebhook() *identity.Webhook {
	warned := time.Now()
	disabled := time.Now()
	return &identity.Webhook{
		ID:                "wh_deadbeef",
		UserID:            "user_1",
		URL:               "https://hooks.example.com/inbox",
		Enabled:           false,
		AutoDisabledAt:    &disabled,
		AutoDisableReason: "HTTP 404",
		WarnNotifiedAt:    &warned,
	}
}

func okStore() *stubStore {
	return &stubStore{
		owner: &identity.User{ID: "user_1", Email: "owner@example.com"},
		stats: identity.WebhookFailureStats{FailedAttempts: 42, LastError: "HTTP 404"},
	}
}

func TestNotifier_DisabledEmailContent(t *testing.T) {
	relay := &captureRelay{}
	n := New(okStore(), relay, "send.example.com", "", "https://app.example.com")

	out := n.Deliver(context.Background(), testWebhook(), KindDisabled)
	if out.Err != nil {
		t.Fatalf("Deliver: %v", out.Err)
	}
	if len(relay.to) != 1 || relay.to[0] != "owner@example.com" {
		t.Errorf("recipients = %v, want the account owner", relay.to)
	}
	if relay.from != "webhooks-noreply@send.example.com" {
		t.Errorf("envelope from = %q, want the fallback local part on from_domain", relay.from)
	}
	msg := string(relay.message)
	for _, want := range []string{
		"https://hooks.example.com/inbox", // the endpoint
		"HTTP 404",                        // the concrete reason
		"42",                              // failure count
		"https://app.example.com/webhooks/detail?id=wh_deadbeef", // the re-enable path
		"cannot be replayed", // honest about forward loss
		"Message-ID: <webhook-health-disabled-wh_deadbeef-",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("disabled email missing %q", want)
		}
	}
	// Deliberately NO Reply-To: replies follow From (the hosted deployment
	// points from_address at a real mailbox); a second address would only
	// create a way for the two to disagree later.
	if strings.Contains(msg, "Reply-To:") {
		t.Errorf("unexpected Reply-To header — replies must follow From")
	}
	if !strings.Contains(msg, "Subject: [e2a] webhook disabled: hooks.example.com") {
		t.Errorf("subject missing/wrong; message headers:\n%s", msg[:min(len(msg), 600)])
	}
}

func TestNotifier_WarningEmailContent(t *testing.T) {
	relay := &captureRelay{}
	n := New(okStore(), relay, "send.example.com", "", "https://app.example.com")

	wh := testWebhook()
	wh.Enabled = true
	wh.AutoDisabledAt = nil
	wh.AutoDisableReason = ""
	out := n.Deliver(context.Background(), wh, KindWarning)
	if out.Err != nil {
		t.Fatalf("Deliver: %v", out.Err)
	}
	msg := string(relay.message)
	for _, want := range []string{
		"Subject: [e2a] webhook delivery failing: hooks.example.com",
		"HTTP 404",
		"disabled automatically", // the warning names the consequence
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning email missing %q", want)
		}
	}
	if strings.Contains(msg, "has disabled") {
		t.Errorf("warning email must not claim the webhook was disabled")
	}
}

// The sender address MUST be configuration: a configured
// notifications.from_address wins over the fallback, and replies follow
// From (no Reply-To), so the configured address is where replies land.
func TestNotifier_ConfiguredFromAddress(t *testing.T) {
	relay := &captureRelay{}
	n := New(okStore(), relay, "send.example.com", "support@corp.example", "")

	if got := n.FromAddress(); got != "support@corp.example" {
		t.Fatalf("FromAddress = %q", got)
	}
	out := n.Deliver(context.Background(), testWebhook(), KindDisabled)
	if out.Err != nil {
		t.Fatalf("Deliver: %v", out.Err)
	}
	if relay.from != "support@corp.example" {
		t.Errorf("envelope from = %q, want configured address", relay.from)
	}
	msg := string(relay.message)
	if !strings.Contains(msg, "From: e2a <support@corp.example>") {
		t.Errorf("From header must carry the configured address")
	}
	if strings.Contains(msg, "Reply-To:") {
		t.Errorf("unexpected Reply-To header — replies must follow From")
	}
	// Message-ID domain follows the configured address's domain.
	if !strings.Contains(msg, "@corp.example>") {
		t.Errorf("Message-ID should be minted on the sending address's domain")
	}
	// Without a public URL the email still sends, with generic dashboard copy.
	if strings.Contains(msg, "/webhooks/detail?id=") {
		t.Errorf("no dashboard link expected when public_url is unset")
	}
}

func TestNotifier_NoOwnerEmailIsPermanent(t *testing.T) {
	st := okStore()
	st.owner = &identity.User{ID: "user_1", Email: ""}
	n := New(st, &captureRelay{}, "send.example.com", "", "")

	out := n.Deliver(context.Background(), testWebhook(), KindDisabled)
	if out.Err == nil {
		t.Fatal("expected an error for a missing owner email")
	}
	if !out.Permanent {
		t.Errorf("missing owner email must classify Permanent (no retry loop)")
	}
}

func TestNotifier_TransientStoreErrorIsRetryable(t *testing.T) {
	st := okStore()
	st.statsErr = errors.New("db blip")
	n := New(st, &captureRelay{}, "send.example.com", "", "")

	out := n.Deliver(context.Background(), testWebhook(), KindDisabled)
	if out.Err == nil {
		t.Fatal("expected an error")
	}
	if out.Permanent || out.Outage {
		t.Errorf("a stats read blip must stay transient, got %+v", out)
	}
}

// fakeDKIM is a DKIMKeyLookup test double.
type fakeDKIM struct {
	get func(ctx context.Context, domain string) (string, []byte, error)
}

func (f *fakeDKIM) GetDKIMKeyInternal(ctx context.Context, domain string) (string, []byte, error) {
	return f.get(ctx, domain)
}

// The relay (SES SMTP) never DKIM-signs on e2a's behalf for a custom
// notifications.from_address domain (BYODKIM: e2a holds the private key),
// so the notifier must sign in-process when a key exists for the From
// domain — otherwise the one email telling a customer their integration
// broke would carry no DKIM leg and die in spam on any SPF-breaking
// forward.
func TestNotifier_SignsWithDKIMWhenKeyExists(t *testing.T) {
	keypair, err := dkim.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	lookup := &fakeDKIM{get: func(_ context.Context, domain string) (string, []byte, error) {
		if domain != "corp.example" {
			t.Fatalf("DKIM lookup domain = %q, want the From-header domain corp.example", domain)
		}
		return keypair.Selector, keypair.PrivateKeyDER, nil
	}}
	relay := &captureRelay{}
	n := New(okStore(), relay, "send.example.com", "support@corp.example", "").WithDKIM(lookup)

	if out := n.Deliver(context.Background(), testWebhook(), KindDisabled); out.Err != nil {
		t.Fatalf("Deliver: %v", out.Err)
	}
	msg := string(relay.message)
	if !strings.Contains(msg, "DKIM-Signature:") {
		t.Fatalf("no DKIM-Signature header on the composed message:\n%s", msg[:min(len(msg), 400)])
	}
	if !strings.Contains(msg, "d=corp.example") {
		t.Errorf("DKIM-Signature must be for the From-header domain (d=corp.example)")
	}
}

// The self-host path: no key for the domain → send unsigned, never fail.
func TestNotifier_SendsUnsignedWhenNoDKIMKey(t *testing.T) {
	lookup := &fakeDKIM{get: func(_ context.Context, _ string) (string, []byte, error) {
		return "", nil, nil // no key stored
	}}
	relay := &captureRelay{}
	n := New(okStore(), relay, "send.example.com", "", "").WithDKIM(lookup)

	if out := n.Deliver(context.Background(), testWebhook(), KindDisabled); out.Err != nil {
		t.Fatalf("Deliver must succeed unsigned: %v", out.Err)
	}
	if strings.Contains(string(relay.message), "DKIM-Signature:") {
		t.Errorf("unexpected DKIM-Signature without a stored key")
	}
	// And with no lookup wired at all (zero-config self-host).
	relay2 := &captureRelay{}
	n2 := New(okStore(), relay2, "send.example.com", "", "")
	if out := n2.Deliver(context.Background(), testWebhook(), KindDisabled); out.Err != nil {
		t.Fatalf("Deliver must succeed without a DKIM lookup: %v", out.Err)
	}
}

// The reason string is echoed into an HTML body — it must be escaped even
// though the upstream source is already sanitized (defense in depth).
func TestNotifier_ReasonIsHTMLEscaped(t *testing.T) {
	st := okStore()
	st.stats.LastError = `<script>alert(1)</script>`
	relay := &captureRelay{}
	n := New(st, relay, "send.example.com", "", "")

	wh := testWebhook()
	wh.AutoDisableReason = ""
	if out := n.Deliver(context.Background(), wh, KindDisabled); out.Err != nil {
		t.Fatalf("Deliver: %v", out.Err)
	}
	// The text/plain part may carry the raw string (harmless in plain
	// text); the text/html part must escape it. Split at the html part
	// marker and assert only the entity form appears there.
	msg := string(relay.message)
	_, htmlPart, found := strings.Cut(msg, "text/html")
	if !found {
		t.Fatalf("no text/html part in composed message")
	}
	if strings.Contains(htmlPart, "<script>") {
		t.Errorf("raw script tag leaked into the HTML part")
	}
	if !strings.Contains(htmlPart, "&lt;script&gt;") {
		t.Errorf("escaped reason not found in the HTML part")
	}
}
