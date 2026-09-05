package hitlnotify_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/approvaltoken"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/dkim"
	"github.com/tokencanopy/e2a/internal/hitlnotify"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
)

const (
	notifySecret     = "hitl-notify-test-secret"
	notifyFromDomain = "notify.test"
	publicURL        = "https://app.example.test"
)

// newNotifier wires a notifier talking to a fake SMTP + a fresh test DB.
// Returns notifier, store, signer, and the smtpDone accessor.
func newNotifier(t *testing.T) (
	*hitlnotify.Notifier,
	*identity.Store,
	*approvaltoken.Signer,
	func() []testutil.SMTPMessage,
) {
	t.Helper()
	smtpAddr, smtpDone := testutil.FakeSMTPServer(t)
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{
		Host:       smtpAddr.Host,
		Port:       smtpAddr.Port,
		FromDomain: notifyFromDomain,
	})
	signer := approvaltoken.NewSigner(notifySecret)
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	notifierGates[store] = gatePool{gate: gate, pool: pool}
	n := hitlnotify.New(store, outbound.NewProviderSubmitter(relay, gate), signer, notifyFromDomain, "", "", publicURL)
	return n, store, signer, smtpDone
}

type gatePool struct {
	gate sendingpolicy.Gate
	pool *pgxpool.Pool
}

// notifierGates remembers the gate each test store was built with, so a test
// can mint the token its notification needs without threading it through
// every helper signature.
var notifierGates = map[*identity.Store]gatePool{}

// tokenFor prepares the notification operation for a held message and runs
// Reserve + ConsumeAttempt, returning the authorization the notifier redeems.
func tokenFor(t *testing.T, store *identity.Store, messageID string) sendingpolicy.ProviderAuthorization {
	t.Helper()
	gp, ok := notifierGates[store]
	if !ok {
		t.Fatal("no gate for this store")
	}
	ctx := context.Background()
	tx, err := gp.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := gp.gate.PrepareNotificationTx(ctx, tx, sendingpolicy.NewHITLNotificationRef(messageID))
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("prepare notification: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	early, attempt, err := gp.gate.Reserve(ctx, ref)
	if err != nil || !early.Allow {
		t.Fatalf("reserve: decision=%+v err=%v", early, err)
	}
	decision, auth, err := gp.gate.ConsumeAttempt(ctx, attempt)
	if err != nil || auth == nil {
		t.Fatalf("authorize: decision=%+v err=%v", decision, err)
	}
	return *auth
}

// setupPendingMessage creates a verified HITL-enabled agent with one
// pending outbound message. Returns (agent, message).
func setupPendingMessage(t *testing.T, store *identity.Store, slug string) (*identity.AgentIdentity, *identity.Message) {
	t.Helper()
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "owner-"+slug+"@reviewer.test", "Owner", "google-notify-"+slug)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, slug+".bot.test", user.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyDomain(ctx, slug+".bot.test", user.ID); err != nil {
		t.Fatal(err)
	}
	a, err := store.CreateAgent(ctx, "bot@"+slug+".bot.test", slug+".bot.test", "", "https://example.com/webhook", "", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateAgentHITL(ctx, a.ID, user.ID, identity.HITLDefaultTTLSeconds, identity.HITLExpirationReject); err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.GetAgentByID(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}

	msg, err := store.CreatePendingOutboundMessage(ctx, a.ID,
		[]string{"alice@example.com"}, []string{"carol@example.com"}, nil,
		"Important draft", "This is the body that will be reviewed.", "<p>html body</p>",
		nil, "send", "conv_1", "", "", 3600)
	if err != nil {
		t.Fatal(err)
	}
	return refreshed, msg
}

func TestNotifierSendsEmailToOwner(t *testing.T) {
	n, store, _, smtpDone := newNotifier(t)
	agent, msg := setupPendingMessage(t, store, "send-email")

	if err := n.NotifyPendingApproval(context.Background(), msg, agent, tokenFor(t, store, msg.ID)); err != nil {
		t.Fatalf("NotifyPendingApproval: %v", err)
	}

	msgs := smtpDone()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SMTP message, got %d", len(msgs))
	}
	sent := msgs[0]

	// From / To envelope
	if want := "approvals@" + notifyFromDomain; sent.From != want {
		t.Errorf("envelope from = %q, want %q", sent.From, want)
	}
	if sent.To != "owner-send-email@reviewer.test" {
		t.Errorf("envelope to = %q", sent.To)
	}

	// Body content: both plain-text and HTML parts should carry identifying
	// info but NOT the held message body. The body only appears on the
	// token-gated confirm page.
	data := sent.Data
	for _, needle := range []string{
		"bot@send-email.bot.test", // agent email
		"alice@example.com",       // recipient
		"carol@example.com",       // cc
		"Important draft",         // subject
		"/v1/approve?t=",          // secondary magic approve link
		"/v1/reject?t=",           // secondary magic reject link
		"/reviews?id=" + msg.ID,   // primary consolidated dashboard review
	} {
		if !strings.Contains(data, needle) {
			t.Errorf("email body missing %q", needle)
		}
	}
	if strings.Contains(data, "/dashboard/pending?id=") {
		t.Errorf("notification should link directly to the consolidated review page, got:\n%s", data)
	}
	reviewAt := strings.Index(data, "Review message")
	approveAt := strings.Index(data, "Quick approve")
	rejectAt := strings.Index(data, "Quick reject")
	if reviewAt < 0 || approveAt < 0 || rejectAt < 0 {
		t.Errorf("notification missing review-first action labels, got:\n%s", data)
	} else if reviewAt > approveAt || reviewAt > rejectAt {
		t.Errorf("Review message must be the primary action before quick actions, got:\n%s", data)
	}
	// Sensitive draft body must not travel in the email.
	if strings.Contains(data, "This is the body that will be reviewed.") {
		t.Errorf("notification leaked held message body into email:\n%s", data)
	}

	// Subject line should mention the agent + message subject
	if !strings.Contains(data, "Subject: ") {
		t.Error("missing Subject header")
	}
	// Reply-To points back at the platform, not the agent
	if !strings.Contains(data, "Reply-To: approvals@"+notifyFromDomain) {
		t.Errorf("Reply-To header should be platform sender, got:\n%s", data)
	}
}

func TestNotifierMagicLinksAreVerifiable(t *testing.T) {
	n, store, _, smtpDone := newNotifier(t)
	agent, msg := setupPendingMessage(t, store, "tok-verify")

	if err := n.NotifyPendingApproval(context.Background(), msg, agent, tokenFor(t, store, msg.ID)); err != nil {
		t.Fatal(err)
	}
	data := smtpDone()[0].Data

	approveTok := extractToken(t, data, "/v1/approve?t=")
	rejectTok := extractToken(t, data, "/v1/reject?t=")

	// Tokens are signed with the deployment HMAC secret (the only signer
	// — the notifier uses n.signer, built from notifySecret). Verify
	// against that secret.
	verifySecrets := []string{notifySecret}

	approveClaims, err := approvaltoken.Verify(verifySecrets, approveTok)
	if err != nil {
		t.Fatalf("approve token verify: %v", err)
	}
	if approveClaims.MessageID != msg.ID {
		t.Errorf("approve claims.MessageID = %q", approveClaims.MessageID)
	}
	if approveClaims.Action != approvaltoken.ActionApprove {
		t.Errorf("approve claims.Action = %q", approveClaims.Action)
	}

	rejectClaims, err := approvaltoken.Verify(verifySecrets, rejectTok)
	if err != nil {
		t.Fatalf("reject token verify: %v", err)
	}
	if rejectClaims.Action != approvaltoken.ActionReject {
		t.Errorf("reject claims.Action = %q", rejectClaims.Action)
	}

	// exp lives slightly past msg.ApprovalExpiresAt so a late click still works.
	if !approveClaims.ExpiresAt.After(*msg.ApprovalExpiresAt) {
		t.Errorf("approve token exp %v should be after msg.ApprovalExpiresAt %v",
			approveClaims.ExpiresAt, *msg.ApprovalExpiresAt)
	}
}

func TestNotifierBuildsAbsoluteURLs(t *testing.T) {
	n, store, _, smtpDone := newNotifier(t)
	agent, msg := setupPendingMessage(t, store, "abs-url")

	if err := n.NotifyPendingApproval(context.Background(), msg, agent, tokenFor(t, store, msg.ID)); err != nil {
		t.Fatal(err)
	}
	data := smtpDone()[0].Data
	if !strings.Contains(data, publicURL+"/v1/approve?t=") {
		t.Errorf("approve URL should be absolute under %q, got:\n%s", publicURL, data)
	}
	if !strings.Contains(data, publicURL+"/reviews?id="+msg.ID) {
		t.Errorf("review URL should be absolute under %q", publicURL)
	}
	if strings.Contains(data, publicURL+"/dashboard/pending?id=") {
		t.Errorf("notification should not use the legacy dashboard redirect, got:\n%s", data)
	}
}

func TestNotifierRejectsMessageWithNilApprovalExpiresAt(t *testing.T) {
	n, store, _, smtpDone := newNotifier(t)
	defer smtpDone()

	agent, msg := setupPendingMessage(t, store, "nil-exp")
	msg.ApprovalExpiresAt = nil

	err := n.NotifyPendingApproval(context.Background(), msg, agent, tokenFor(t, store, msg.ID))
	if err == nil {
		t.Fatal("expected error for nil ApprovalExpiresAt")
	}
	if !strings.Contains(err.Error(), "approval_expires_at") {
		t.Errorf("error should mention approval_expires_at, got: %v", err)
	}
}

// TestNotifierDeterministicMessageID: the approval-notification carries a
// deterministic Message-ID derived from the held message id, so a re-sent
// notification (crash-window / cutover re-drive) is byte-identical in that header
// and collapses at Message-ID-deduping recipients. Two sends of the same hold must
// carry the SAME Message-ID.
func TestNotifierDeterministicMessageID(t *testing.T) {
	n, store, _, smtpDone := newNotifier(t)
	agent, msg := setupPendingMessage(t, store, "msgid")

	if err := n.NotifyPendingApproval(context.Background(), msg, agent, tokenFor(t, store, msg.ID)); err != nil {
		t.Fatal(err)
	}
	if err := n.NotifyPendingApproval(context.Background(), msg, agent, tokenFor(t, store, msg.ID)); err != nil {
		t.Fatal(err)
	}

	msgs := smtpDone()
	if len(msgs) != 2 {
		t.Fatalf("got %d SMTP messages, want 2", len(msgs))
	}
	want := "Message-ID: <hitl-approve-" + msg.ID + "@" + notifyFromDomain + ">"
	for i, m := range msgs {
		if !strings.Contains(m.Data, want) {
			t.Errorf("message %d missing deterministic %q; data:\n%s", i, want, m.Data)
		}
		// Exactly one Message-ID (ours — compose omits its own), leading the block.
		if n := strings.Count(m.Data, "Message-ID:"); n != 1 {
			t.Errorf("message %d has %d Message-ID headers, want exactly 1", i, n)
		}
		// The provider submitter prepends its own attribution block (X-SES-* /
		// X-E2A-*) ahead of the composed message; Message-ID must lead what the
		// notifier composed, i.e. be the first non-provider header on the wire.
		if !strings.HasPrefix(stripProviderBlock(m.Data), "Message-ID: <hitl-approve-") {
			t.Errorf("message %d: Message-ID should lead the composed header block; got:\n%.200s", i, m.Data)
		}
	}
}

func TestNotifierDeliver(t *testing.T) {
	n, store, _, smtpDone := newNotifier(t)
	agent, msg := setupPendingMessage(t, store, "deliver")

	// Deliver is what the River NotifyWorker calls: it composes + sends once and
	// classifies the result. A healthy send returns a zero-value outcome.
	env, out := n.Compose(context.Background(), &identity.PendingNotify{Message: msg, Agent: agent})
	if out.Err != nil {
		t.Fatalf("Compose: unexpected err = %v", out.Err)
	}
	out = n.Submit(context.Background(), env, tokenFor(t, store, msg.ID))
	if out.Err != nil {
		t.Fatalf("Deliver: unexpected err = %v", out.Err)
	}
	if out.Permanent || out.Outage {
		t.Errorf("Deliver: healthy send classified Permanent=%v Outage=%v", out.Permanent, out.Outage)
	}
	msgs := smtpDone()
	if len(msgs) != 1 {
		t.Fatalf("Deliver: got %d messages, want 1", len(msgs))
	}
}

func TestNotifierNilSafe(t *testing.T) {
	var n *hitlnotify.Notifier
	// The sync compose+send tolerates a nil receiver so wiring can omit the
	// notifier in tests / partial deployments without guarding every call site.
	if err := n.NotifyPendingApproval(context.Background(), nil, nil, sendingpolicy.ProviderAuthorization{}); err != nil {
		t.Errorf("nil receiver sync: err = %v, want nil", err)
	}
}

// extractToken pulls the ?t=... token out of the first occurrence of the
// given URL prefix in the raw email data. Tolerates URL encoding since
// tokens contain only base64url-safe characters plus '.'.
func extractToken(t *testing.T, data, prefix string) string {
	t.Helper()
	i := strings.Index(data, prefix)
	if i < 0 {
		t.Fatalf("prefix %q not found in email data", prefix)
	}
	rest := data[i+len(prefix):]
	end := strings.IndexAny(rest, " \r\n\t\"<>)")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

// A configured notifications.reply_to must reach the wire, and the From must
// not advertise a noreply identity while doing so. Before this, Reply-To was
// hardcoded to the sender on the platform relay domain — which has no mailbox,
// so a reviewer who hit Reply was talking to the bounce endpoint. Approvals are
// time-boxed and a reviewer who cannot sign in has only the magic links, so
// that silent dead end was the worst place to have one.
func TestNotifierUsesConfiguredReplyTo(t *testing.T) {
	smtpAddr, smtpDone := testutil.FakeSMTPServer(t)
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{
		Host: smtpAddr.Host, Port: smtpAddr.Port, FromDomain: notifyFromDomain,
	})
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	notifierGates[store] = gatePool{gate: gate, pool: pool}
	signer := approvaltoken.NewSigner(notifySecret)
	n := hitlnotify.New(store, outbound.NewProviderSubmitter(relay, gate), signer, notifyFromDomain, "", "support@inbox.test", publicURL)

	agent, msg := setupPendingMessage(t, store, "replyto")
	if err := n.NotifyPendingApproval(context.Background(), msg, agent, tokenFor(t, store, msg.ID)); err != nil {
		t.Fatalf("NotifyPendingApproval: %v", err)
	}
	msgs := smtpDone()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SMTP message, got %d", len(msgs))
	}
	data := string(msgs[0].Data)

	if !strings.Contains(data, "Reply-To: support@inbox.test") {
		t.Errorf("configured reply_to missing from headers:\n%s", firstHeaders(data))
	}
	if strings.Contains(data, "Reply-To: approvals@"+notifyFromDomain) {
		t.Errorf("reply_to was configured but Reply-To still points at the sender")
	}
	// The From stays the sender identity; only Reply-To moves.
	if want := "From: e2a <approvals@" + notifyFromDomain + ">"; !strings.Contains(data, want) {
		t.Errorf("From changed unexpectedly, want %q in:\n%s", want, firstHeaders(data))
	}
	if strings.Contains(data, "noreply") {
		t.Errorf("a replyable notification must not carry a noreply identity:\n%s", firstHeaders(data))
	}
}

// firstHeaders trims a composed message to its header block for readable
// failure output.
func firstHeaders(data string) string {
	if i := strings.Index(data, "\r\n\r\n"); i > 0 {
		return data[:i]
	}
	if len(data) > 600 {
		return data[:600]
	}
	return data
}

// Setting notifications.from_address consolidates both notification senders
// into one identity. Verify it overrides the package default here too, and
// that the Message-ID domain follows the configured address rather than
// claiming the relay domain the From no longer uses.
func TestNotifierHonoursConfiguredFromAddress(t *testing.T) {
	smtpAddr, smtpDone := testutil.FakeSMTPServer(t)
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{
		Host: smtpAddr.Host, Port: smtpAddr.Port, FromDomain: notifyFromDomain,
	})
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	notifierGates[store] = gatePool{gate: gate, pool: pool}
	signer := approvaltoken.NewSigner(notifySecret)
	n := hitlnotify.New(store, outbound.NewProviderSubmitter(relay, gate), signer, notifyFromDomain, "alerts@mail.test", "", publicURL)

	agent, msg := setupPendingMessage(t, store, "fromaddr")
	if err := n.NotifyPendingApproval(context.Background(), msg, agent, tokenFor(t, store, msg.ID)); err != nil {
		t.Fatalf("NotifyPendingApproval: %v", err)
	}
	msgs := smtpDone()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SMTP message, got %d", len(msgs))
	}
	sent := msgs[0]
	data := string(sent.Data)

	if sent.From != "alerts@mail.test" {
		t.Errorf("envelope from = %q, want the configured address", sent.From)
	}
	if !strings.Contains(data, "From: e2a <alerts@mail.test>") {
		t.Errorf("From header ignored the configured address:\n%s", firstHeaders(data))
	}
	if strings.Contains(data, "Message-ID: <") && !strings.Contains(data, "@mail.test>") {
		t.Errorf("Message-ID domain should follow the configured From, got:\n%s", firstHeaders(data))
	}
}

// fakeDKIMLookup is a DKIMKeyLookup test double.
type fakeDKIMLookup struct {
	get func(ctx context.Context, domain string) (string, []byte, error)
}

func (f *fakeDKIMLookup) GetDKIMKeyInternal(ctx context.Context, domain string) (string, []byte, error) {
	return f.get(ctx, domain)
}

// Parity with webhooknotify. The relay never DKIM-signs on e2a's behalf for a
// custom notifications.from_address domain (BYODKIM: e2a holds the key), so
// the notifier must sign in-process. Without this, an operator who configured
// from_address would get signed webhook-health mail and UNSIGNED approval
// mail — leaving the most time-sensitive email the platform sends on an SPF
// leg alone, and so quarantined the moment anyone forwards it.
func TestNotifierSignsWithDKIMForConfiguredFromDomain(t *testing.T) {
	keypair, err := dkim.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	lookup := &fakeDKIMLookup{get: func(_ context.Context, domain string) (string, []byte, error) {
		if domain != "corp.example" {
			t.Fatalf("DKIM lookup domain = %q, want the From-header domain corp.example", domain)
		}
		return keypair.Selector, keypair.PrivateKeyDER, nil
	}}

	smtpAddr, smtpDone := testutil.FakeSMTPServer(t)
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{
		Host: smtpAddr.Host, Port: smtpAddr.Port, FromDomain: notifyFromDomain,
	})
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	notifierGates[store] = gatePool{gate: gate, pool: pool}
	signer := approvaltoken.NewSigner(notifySecret)
	n := hitlnotify.New(store, outbound.NewProviderSubmitter(relay, gate), signer, notifyFromDomain, "approvals@corp.example", "", publicURL).WithDKIM(lookup)

	agent, msg := setupPendingMessage(t, store, "dkim")
	if err := n.NotifyPendingApproval(context.Background(), msg, agent, tokenFor(t, store, msg.ID)); err != nil {
		t.Fatalf("NotifyPendingApproval: %v", err)
	}
	msgs := smtpDone()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SMTP message, got %d", len(msgs))
	}
	data := string(msgs[0].Data)

	if !strings.Contains(data, "DKIM-Signature:") {
		t.Fatalf("approval mail sent UNSIGNED from a custom from_address domain:\n%s", firstHeaders(data))
	}
	if !strings.Contains(data, "d=corp.example") {
		t.Errorf("DKIM-Signature must be for the From-header domain (d=corp.example):\n%s", firstHeaders(data))
	}
}

// The zero-config self-host path: no key, or no lookup wired at all, must
// send unsigned rather than fail.
func TestNotifierSendsUnsignedWithoutDKIMKey(t *testing.T) {
	lookup := &fakeDKIMLookup{get: func(_ context.Context, _ string) (string, []byte, error) {
		return "", nil, nil // no key stored
	}}
	smtpAddr, smtpDone := testutil.FakeSMTPServer(t)
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{
		Host: smtpAddr.Host, Port: smtpAddr.Port, FromDomain: notifyFromDomain,
	})
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	notifierGates[store] = gatePool{gate: gate, pool: pool}
	signer := approvaltoken.NewSigner(notifySecret)
	n := hitlnotify.New(store, outbound.NewProviderSubmitter(relay, gate), signer, notifyFromDomain, "", "", publicURL).WithDKIM(lookup)

	agent, msg := setupPendingMessage(t, store, "nodkim")
	if err := n.NotifyPendingApproval(context.Background(), msg, agent, tokenFor(t, store, msg.ID)); err != nil {
		t.Fatalf("must succeed unsigned: %v", err)
	}
	msgs := smtpDone()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 SMTP message, got %d", len(msgs))
	}
	if strings.Contains(string(msgs[0].Data), "DKIM-Signature:") {
		t.Errorf("unexpected DKIM-Signature without a stored key")
	}
}

// stripProviderBlock drops the leading provider-owned header lines the
// submitter prepends (X-SES-* and X-E2A-*), returning the composed message.
func stripProviderBlock(data string) string {
	for {
		if !strings.HasPrefix(data, "X-SES-") && !strings.HasPrefix(data, "X-E2A-") {
			return data
		}
		nl := strings.Index(data, "\n")
		if nl < 0 {
			return ""
		}
		data = data[nl+1:]
	}
}
