package agent_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/approvaltoken"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/unsubscribe"
	"github.com/tokencanopy/e2a/internal/usage"
)

const magicLinkSecret = "magic-link-test-secret"

// setupMagicLinkAPI mirrors setupAPIWithSMTP but also wires an
// approvaltoken.Signer onto the API. Returns the server, store, signer
// (for issuing tokens in tests), and the fake-SMTP accessor.
func setupMagicLinkAPI(t *testing.T) (
	*httptest.Server,
	*identity.Store,
	*approvaltoken.Signer,
	func() []testutil.SMTPMessage,
) {
	t.Helper()
	smtpAddr, smtpDone := testutil.FakeSMTPServer(t)
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: smtpAddr.Host, Port: smtpAddr.Port})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	noopUsage := usage.NewNoopUsageTracker()
	api := agent.NewAPI(store, sender, smtpRelay, nil, noopUsage, "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.SetOutboundEnqueuer(&fakeOutboundEnqueuer{jobID: 999})
	issuer, err := unsubscribe.NewIssuer(magicLinkSecret, "https://api.example.test", false, store)
	if err != nil {
		t.Fatal(err)
	}
	api.SetManagedUnsubscribeIssuer(issuer)
	signer := approvaltoken.NewSigner(magicLinkSecret)
	api.SetApprovalSigner(signer)
	router := mux.NewRouter()
	api.RegisterRoutes(router)
	// Magic-link routes are no longer part of RegisterRoutes — the chi root
	// owns them in production; mount them directly here.
	router.Handle("/v1/approve", api.ApproveMagicLinkHandler())
	router.Handle("/v1/reject", api.RejectMagicLinkHandler())
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, store, signer, smtpDone
}

func TestMagicApproveManagedUnsubscribeMintsOnApprovalAndPersistsMIME(t *testing.T) {
	server, store, signer, _ := setupMagicLinkAPI(t)
	ctx := context.Background()
	a, userID := prepareHITLAgent(t, store, "magic-managed")
	msg, err := store.CreatePendingOutboundMessageManaged(ctx, a.ID,
		[]string{"FINAL@Example.net"}, nil, nil,
		"Managed", "plain body", "<p>html body</p>", nil,
		"send", "", "", "", 3600, true)
	if err != nil {
		t.Fatal(err)
	}
	var before int
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM agent_unsubscribe_tokens WHERE user_id=$1`, userID).Scan(&before)
	}); err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatalf("tokens before approval=%d", before)
	}

	tok, err := signer.Sign(msg.ID, approvaltoken.ActionApprove, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	resp := postForm(t, server.URL+"/v1/approve", map[string]string{"t": tok})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	_ = resp.Body.Close()

	var recipient string
	var raw []byte
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT address FROM agent_unsubscribe_tokens WHERE user_id=$1 AND agent_id=$2`, userID, a.ID).Scan(&recipient); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT raw_message FROM messages WHERE id=$1`, msg.ID).Scan(&raw)
	}); err != nil {
		t.Fatal(err)
	}
	if recipient != "final@example.net" {
		t.Fatalf("bound recipient=%q", recipient)
	}
	text := string(raw)
	if strings.Count(text, "List-Unsubscribe: <https://api.example.test/u/") != 1 || strings.Count(text, "List-Unsubscribe-Post: List-Unsubscribe=One-Click") != 1 {
		t.Fatalf("unsubscribe headers missing/duplicated:\n%s", text)
	}
	if !strings.Contains(text, "Unsubscribe from emails sent by "+a.ID) || !strings.Contains(text, `href="https://api.example.test/u/`) {
		t.Fatalf("visible managed footer missing:\n%s", text)
	}
}

func TestMagicApproveManagedUnsubscribeRejectsWhenFooterCrossesCap(t *testing.T) {
	server, store, signer, _ := setupMagicLinkAPI(t)
	ctx := context.Background()
	a, _ := prepareHITLAgent(t, store, "magic-managed-cap")
	subject := "s"
	body := strings.Repeat("x", outbound.MaxComposedMessageBytes-len(subject))
	msg, err := store.CreatePendingOutboundMessageManaged(ctx, a.ID,
		[]string{"final@example.net"}, nil, nil, subject, body, "", nil,
		"send", "", "", "", 3600, true)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := signer.Sign(msg.ID, approvaltoken.ActionApprove, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	resp := postForm(t, server.URL+"/v1/approve", map[string]string{"t": tok})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var status, deliveryStatus string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, COALESCE(delivery_status, '') FROM messages WHERE id=$1`, msg.ID).Scan(&status, &deliveryStatus)
	}); err != nil {
		t.Fatal(err)
	}
	if status != identity.MessageStatusPendingReview || deliveryStatus == "accepted" {
		t.Fatalf("status=%q delivery_status=%q", status, deliveryStatus)
	}
}

// prepareHITLAgent creates a verified agent with HITL enabled. Returns
// agent + userID.
func prepareHITLAgent(t *testing.T, store *identity.Store, slug string) (*identity.AgentIdentity, string) {
	t.Helper()
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "owner-"+slug+"@example.com", "Owner", "google-magic-"+slug)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, slug+".example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain(%s): %v", slug+".example.com", err)
	}
	if err := store.VerifyDomain(ctx, slug+".example.com", user.ID); err != nil {
		t.Fatalf("VerifyDomain(%s): %v", slug+".example.com", err)
	}
	a, err := store.CreateAgent(ctx, "bot@"+slug+".example.com", slug+".example.com", "", "https://example.com/webhook", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := store.UpdateAgentHITL(ctx, a.ID, user.ID, identity.HITLDefaultTTLSeconds, identity.HITLExpirationReject); err != nil {
		t.Fatal(err)
	}
	return a, user.ID
}

// issuePending creates a pending_approval outbound message on the agent.
func issuePending(t *testing.T, store *identity.Store, agentID string) *identity.Message {
	t.Helper()
	msg, err := store.CreatePendingOutboundMessage(context.Background(), agentID,
		[]string{"alice@example.com"}, nil, nil,
		"Held", "plain body", "<p>html</p>", nil,
		"send", "客户 1% ready", "", "", 3600)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// postForm submits a form-encoded POST to the given URL and returns the
// response. Mirrors what a browser does when the confirmation page's
// form is submitted.
func postForm(t *testing.T, url string, values map[string]string) *http.Response {
	t.Helper()
	form := make(map[string][]string, len(values))
	for k, v := range values {
		form[k] = []string{v}
	}
	resp, err := http.PostForm(url, form)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// --- GET confirmation page behavior ---

// TestMagicLinkGETDoesNotExecute is the core security property of the
// split GET/POST design: an email-client URL scanner that previews the
// approve link must not trigger the send.
func TestMagicLinkGETDoesNotExecute(t *testing.T) {
	server, store, signer, smtpDone := setupMagicLinkAPI(t)
	a, userID := prepareHITLAgent(t, store, "get-no-execute")
	msg := issuePending(t, store, a.ID)

	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionApprove, time.Now().Add(1*time.Hour))
	resp, err := http.Get(server.URL + "/v1/approve?t=" + url.QueryEscape(tok))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET confirm page: status = %d", resp.StatusCode)
	}

	// No SMTP activity at all — we only rendered a confirmation page.
	if msgs := smtpDone(); len(msgs) != 0 {
		t.Errorf("GET should not have triggered a send; got %d SMTP messages", len(msgs))
	}

	// Row stays pending.
	got, _ := store.GetOutboundMessageForUser(context.Background(), msg.ID, userID)
	if got.Status != identity.MessageStatusPendingReview {
		t.Errorf("status after GET = %q, want still pending_approval", got.Status)
	}
}

// TestMagicApproveGETRendersConfirmForm verifies the confirmation page
// contains a POST form with the token carried in a hidden field, plus
// the body preview so the reviewer can see what they're about to send.
func TestMagicApproveGETRendersConfirmForm(t *testing.T) {
	server, store, signer, _ := setupMagicLinkAPI(t)
	a, _ := prepareHITLAgent(t, store, "get-renders-form")
	msg := issuePending(t, store, a.ID)

	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionApprove, time.Now().Add(1*time.Hour))
	resp, _ := http.Get(server.URL + "/v1/approve?t=" + url.QueryEscape(tok))
	body := readBody(t, resp)

	for _, needle := range []string{
		`method="POST"`,
		`action="/v1/approve"`,
		`name="t"`,
		tok,                 // token echoed into the hidden input
		"alice@example.com", // recipient shown
		"Held",              // subject shown
		"plain body",        // body preview is on the confirm page
		"Approve &amp; send",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("confirm page missing %q", needle)
		}
	}
	// Security headers: no indexing, no frame, no referrer.
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestMagicRejectGETRendersConfirmFormWithReasonField(t *testing.T) {
	server, store, signer, _ := setupMagicLinkAPI(t)
	a, _ := prepareHITLAgent(t, store, "get-reject-form")
	msg := issuePending(t, store, a.ID)

	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionReject, time.Now().Add(1*time.Hour))
	resp, _ := http.Get(server.URL + "/v1/reject?t=" + url.QueryEscape(tok))
	body := readBody(t, resp)

	for _, needle := range []string{
		`method="POST"`,
		`action="/v1/reject"`,
		`name="t"`,
		`name="reason"`, // optional rejection reason input
		"Reject",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("reject confirm page missing %q", needle)
		}
	}
}

// --- POST executor behavior ---

func TestMagicApprovePOSTQueues(t *testing.T) {
	server, store, signer, smtpDone := setupMagicLinkAPI(t)
	a, userID := prepareHITLAgent(t, store, "post-approve")
	msg := issuePending(t, store, a.ID)

	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionApprove, time.Now().Add(1*time.Hour))
	resp := postForm(t, server.URL+"/v1/approve", map[string]string{"t": tok})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST approve: status = %d, body: %s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Approved") {
		t.Errorf("expected 'Approved' in body, got: %s", body)
	}
	assertViewMessageCTA(t, body, a.EmailAddress(), msg.ConversationID, msg.ID)

	if msgs := smtpDone(); len(msgs) != 0 {
		t.Fatalf("approval submitted %d SMTP messages inline, want zero", len(msgs))
	}

	got, _ := store.GetOutboundMessageForUser(context.Background(), msg.ID, userID)
	if got.Status != identity.MessageStatusSent {
		t.Errorf("status = %q, want sent", got.Status)
	}
	if got.DeliveryStatus != "accepted" {
		t.Errorf("delivery_status = %q, want accepted", got.DeliveryStatus)
	}
	if got.BodyText != "plain body" {
		t.Errorf("body_text should be retained, got %q", got.BodyText)
	}
}

// TestMagicApprovePOSTSelfSendDeliversViaLoopback: approving a held
// self-send via the magic-link path must route through loopback —
// outbound.Sender.Send would strip the agent's own address (self-spam
// guard) and fail with "no valid recipients". Asserts no SMTP traffic,
// outbound row → sent+loopback, inbound row landed.
func TestMagicApprovePOSTSelfSendDeliversViaLoopback(t *testing.T) {
	server, store, signer, smtpDone := setupMagicLinkAPI(t)
	a, userID := prepareHITLAgent(t, store, "post-approve-self")

	// Hold a self-send (To = agent's own address).
	ctx := context.Background()
	held, err := store.CreatePendingOutboundMessage(ctx, a.ID,
		[]string{a.EmailAddress()}, nil, nil,
		"self-magic", "note to self via magic link", "", nil,
		"send", "", "", "", 3600)
	if err != nil {
		t.Fatal(err)
	}

	tok, _ := signer.Sign(held.ID, approvaltoken.ActionApprove, time.Now().Add(1*time.Hour))
	resp := postForm(t, server.URL+"/v1/approve", map[string]string{"t": tok})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST approve (self-send): status = %d, body: %s", resp.StatusCode, readBody(t, resp))
	}
	if body := readBody(t, resp); !strings.Contains(body, "Approved") {
		t.Errorf("expected 'Approved' on the result page, got: %s", body)
	} else {
		assertViewMessageCTA(t, body, a.EmailAddress(), held.ConversationID, held.ID)
	}

	if msgs := smtpDone(); len(msgs) != 0 {
		t.Fatalf("self-send approve must not hit SMTP, got %d messages", len(msgs))
	}

	got, _ := store.GetOutboundMessageForUser(ctx, held.ID, userID)
	if got.Status != identity.MessageStatusSent {
		t.Errorf("outbound status = %q, want sent", got.Status)
	}
	if got.Method != "loopback" {
		t.Errorf("method = %q, want loopback", got.Method)
	}

	// Inbound row reaches the agent's mailbox. GetMessagesByAgent's
	// "all" status returns inbound rows regardless of read state.
	inboxes, err := store.GetMessagesByAgent(ctx, identity.MessageListFilter{
		AgentID:   a.ID,
		Status:    "all",
		Direction: "inbound",
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("GetMessagesByAgent: %v", err)
	}
	if len(inboxes) != 1 || inboxes[0].Subject != "self-magic" {
		t.Errorf("inbound rows = %d, subjects = %v; want one row 'self-magic'", len(inboxes), subjectsOf(inboxes))
	}
}

// assertViewMessageCTA pins the approve result page's call to action to the
// canonical inbox thread. Messages with a conversation select that thread;
// legacy/orphan messages fall back to their synthetic single-message thread.
func assertViewMessageCTA(t *testing.T, body, agentEmail, conversationID, messageID string) {
	t.Helper()
	prefix, value := "conv:", conversationID
	if conversationID == "" {
		prefix, value = "orphan:", messageID
	}
	want := "/inboxes/messages?email=" + url.QueryEscape(agentEmail) + "#" +
		prefix + url.PathEscape(value)
	if !strings.Contains(body, `href="`+want+`"`) {
		t.Errorf("result page missing view-message href %q, got: %s", want, body)
	}
	if strings.Contains(body, "/inboxes/messages/view") {
		t.Errorf("result page should not link to the retired focus view, got: %s", body)
	}
	if !strings.Contains(body, ">View message</a>") {
		t.Errorf("result page missing 'View message' button label, got: %s", body)
	}
	if strings.Contains(body, "Open the dashboard") {
		t.Errorf("approve result page should not fall back to the dashboard CTA, got: %s", body)
	}
}

// subjectsOf is a small helper to keep error messages readable when an
// inbox-shape assertion fails.
func subjectsOf(msgs []identity.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Subject
	}
	return out
}

func TestMagicRejectPOSTWithReason(t *testing.T) {
	server, store, signer, smtpDone := setupMagicLinkAPI(t)
	a, userID := prepareHITLAgent(t, store, "post-reject")
	msg := issuePending(t, store, a.ID)

	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionReject, time.Now().Add(1*time.Hour))
	resp := postForm(t, server.URL+"/v1/reject", map[string]string{
		"t":      tok,
		"reason": "not the right tone",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST reject: status = %d", resp.StatusCode)
	}
	// Only approve deep-links the message; a rejected draft is discarded, so
	// reject keeps the generic dashboard CTA.
	if body := readBody(t, resp); !strings.Contains(body, "Open the dashboard") {
		t.Errorf("reject result page should keep the dashboard CTA, got: %s", body)
	}

	if msgs := smtpDone(); len(msgs) != 0 {
		t.Errorf("reject should not call SMTP, got %d", len(msgs))
	}

	got, _ := store.GetOutboundMessageForUser(context.Background(), msg.ID, userID)
	if got.Status != identity.MessageStatusReviewRejected {
		t.Errorf("status = %q, want rejected", got.Status)
	}
	if got.RejectionReason != "not the right tone" {
		t.Errorf("rejection_reason = %q, want 'not the right tone'", got.RejectionReason)
	}
}

// The magic-link form is a human-facing entry path around the /v1 schema, so
// an over-long reason is CLAMPED to the shared identity.MaxRejectReasonLen
// (2000 Unicode code points — runes, not bytes: the fixture uses 3-byte CJK
// characters) rather than failing the rejection. The stored reason is exactly
// the first 2000 code points.
func TestMagicRejectPOSTClampsOverlongReason(t *testing.T) {
	server, store, signer, _ := setupMagicLinkAPI(t)
	a, userID := prepareHITLAgent(t, store, "post-reject-clamp")
	msg := issuePending(t, store, a.ID)

	long := strings.Repeat("日", identity.MaxRejectReasonLen+100)
	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionReject, time.Now().Add(1*time.Hour))
	resp := postForm(t, server.URL+"/v1/reject", map[string]string{
		"t":      tok,
		"reason": long,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST reject with over-long reason: status = %d (must clamp, not fail)", resp.StatusCode)
	}

	got, _ := store.GetOutboundMessageForUser(context.Background(), msg.ID, userID)
	if got.Status != identity.MessageStatusReviewRejected {
		t.Fatalf("status = %q, want rejected", got.Status)
	}
	want := strings.Repeat("日", identity.MaxRejectReasonLen)
	if got.RejectionReason != want {
		t.Errorf("rejection_reason = %d code points (%d bytes), want clamped to exactly %d code points",
			len([]rune(got.RejectionReason)), len(got.RejectionReason), identity.MaxRejectReasonLen)
	}
}

func TestMagicRejectPOSTWithoutReasonUsesDefault(t *testing.T) {
	server, store, signer, _ := setupMagicLinkAPI(t)
	a, userID := prepareHITLAgent(t, store, "post-reject-default")
	msg := issuePending(t, store, a.ID)

	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionReject, time.Now().Add(1*time.Hour))
	resp := postForm(t, server.URL+"/v1/reject", map[string]string{"t": tok})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got, _ := store.GetOutboundMessageForUser(context.Background(), msg.ID, userID)
	if got.RejectionReason != "magic-link rejection" {
		t.Errorf("default reason = %q", got.RejectionReason)
	}
}

// TestMagicRejectPOSTRefusesIllFormedReason is the /v1/reject arm of the
// request-content rules. /v1/approve and /v1/reject are registered on the chi
// root OUTSIDE Huma, so neither the raw-body format guard nor the registerOp
// walk sees them — and `reason` is persisted verbatim into
// messages.rejection_reason. Before the handler check, a raw 0xFF byte reached
// the UPDATE, Postgres refused it (SQLSTATE 22021) and the reviewer got a
// 500 "Rejection failed": a permanent client error dressed as an outage, with
// the review hold still open. Both rules are asserted against the real
// database, which is the only thing that actually produces 22021.
func TestMagicRejectPOSTRefusesIllFormedReason(t *testing.T) {
	cases := []struct {
		name   string
		reason string
	}{
		{"invalid utf-8", "bad\xffbytes"},
		{"NUL", "bad\x00bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, store, signer, _ := setupMagicLinkAPI(t)
			a, userID := prepareHITLAgent(t, store, "post-reject-illformed")
			msg := issuePending(t, store, a.ID)

			tok, _ := signer.Sign(msg.ID, approvaltoken.ActionReject, time.Now().Add(1*time.Hour))
			resp := postForm(t, server.URL+"/v1/reject", map[string]string{"t": tok, "reason": tc.reason})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 — ill-formed text is a permanent client error, never a 5xx", resp.StatusCode)
			}

			// The message must be untouched: refusing the note must not
			// half-resolve the hold, and must never store a laundered
			// (U+FFFD-substituted) reason.
			got, err := store.GetOutboundMessageForUser(context.Background(), msg.ID, userID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != identity.MessageStatusPendingReview {
				t.Errorf("status = %q, want the message still pending_review", got.Status)
			}
			if got.RejectionReason != "" {
				t.Errorf("rejection_reason = %q, want empty — nothing may be persisted", got.RejectionReason)
			}
		})
	}
}

// TestMagicRejectPOSTAcceptsValidMultiByteReason is the negative control for
// the check above: refusing ill-formed bytes must not refuse a single byte of
// legitimate international text. A properly ENCODED U+FFFD is legal input —
// only raw malformed sequences are refused — and the reason round-trips into
// the column byte-for-byte.
func TestMagicRejectPOSTAcceptsValidMultiByteReason(t *testing.T) {
	const reason = "日本語 ✉ 😀 — a�b"
	server, store, signer, _ := setupMagicLinkAPI(t)
	a, userID := prepareHITLAgent(t, store, "post-reject-intl")
	msg := issuePending(t, store, a.ID)

	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionReject, time.Now().Add(1*time.Hour))
	resp := postForm(t, server.URL+"/v1/reject", map[string]string{"t": tok, "reason": reason})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for valid multi-byte UTF-8", resp.StatusCode)
	}

	got, err := store.GetOutboundMessageForUser(context.Background(), msg.ID, userID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != identity.MessageStatusReviewRejected {
		t.Fatalf("status = %q, want rejected", got.Status)
	}
	if got.RejectionReason != reason {
		t.Errorf("rejection_reason = %q, want %q byte-for-byte", got.RejectionReason, reason)
	}
}

// TestMagicApproveHasNoUnguardedCallerText pins the other half of the
// non-Huma-route audit: /v1/approve persists nothing the caller authored. Its
// only form input is the HMAC magic-link token, which is self-validating — an
// ill-formed token cannot verify, so it is refused at the signature check and
// never reaches the store. Any future free-text field added to this form would
// need the same isWellFormedText guard /v1/reject's `reason` has.
func TestMagicApproveHasNoUnguardedCallerText(t *testing.T) {
	server, store, signer, smtpDone := setupMagicLinkAPI(t)
	defer smtpDone()
	a, _ := prepareHITLAgent(t, store, "approve-no-caller-text")
	msg := issuePending(t, store, a.ID)

	for _, bad := range []string{"bad\xfftoken", "bad\x00token"} {
		resp := postForm(t, server.URL+"/v1/approve", map[string]string{"t": bad})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST /v1/approve with an ill-formed token: status = %d, want 400", resp.StatusCode)
		}
		resp.Body.Close()

		r2, err := http.Get(server.URL + "/v1/reject?t=" + url.QueryEscape(bad))
		if err != nil {
			t.Fatal(err)
		}
		if r2.StatusCode != http.StatusBadRequest {
			t.Errorf("GET /v1/reject with an ill-formed token: status = %d, want 400", r2.StatusCode)
		}
		r2.Body.Close()
	}

	// A valid token plus an ill-formed EXTRA field still approves: the approve
	// form has no free-text field, so an unknown one is simply not read.
	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionApprove, time.Now().Add(1*time.Hour))
	resp := postForm(t, server.URL+"/v1/approve", map[string]string{"t": tok, "reason": "bad\xffbytes"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve with a stray ill-formed field: status = %d, want 200", resp.StatusCode)
	}
}

// --- Error paths (GET rejects + POST rejects consistently) ---

func TestMagicLinkGETMissingToken(t *testing.T) {
	server, _, _, smtpDone := setupMagicLinkAPI(t)
	defer smtpDone()

	resp, _ := http.Get(server.URL + "/v1/approve")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMagicLinkPOSTMissingToken(t *testing.T) {
	server, _, _, smtpDone := setupMagicLinkAPI(t)
	defer smtpDone()

	resp := postForm(t, server.URL+"/v1/approve", map[string]string{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMagicLinkGETInvalidToken(t *testing.T) {
	server, _, _, smtpDone := setupMagicLinkAPI(t)
	defer smtpDone()

	resp, _ := http.Get(server.URL + "/v1/approve?t=gibberish")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMagicLinkPOSTInvalidToken(t *testing.T) {
	server, _, _, smtpDone := setupMagicLinkAPI(t)
	defer smtpDone()

	resp := postForm(t, server.URL+"/v1/approve", map[string]string{"t": "gibberish"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMagicLinkExpiredToken(t *testing.T) {
	server, store, signer, smtpDone := setupMagicLinkAPI(t)
	defer smtpDone()
	a, _ := prepareHITLAgent(t, store, "magic-expired")
	msg := issuePending(t, store, a.ID)

	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionApprove, time.Now().Add(-1*time.Second))

	// GET and POST both reject expired tokens with 410.
	getResp, _ := http.Get(server.URL + "/v1/approve?t=" + url.QueryEscape(tok))
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusGone {
		t.Errorf("GET expired: status = %d, want 410", getResp.StatusCode)
	}
	postResp := postForm(t, server.URL+"/v1/approve", map[string]string{"t": tok})
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusGone {
		t.Errorf("POST expired: status = %d, want 410", postResp.StatusCode)
	}
}

// TestMagicApproveTokenRejectedAtRejectEndpoint confirms a token issued
// for approve cannot be redeemed at /reject. Tested on both GET and
// POST since either is a potential attack surface.
func TestMagicApproveTokenRejectedAtRejectEndpoint(t *testing.T) {
	server, store, signer, smtpDone := setupMagicLinkAPI(t)
	defer smtpDone()
	a, _ := prepareHITLAgent(t, store, "magic-wrong-action")
	msg := issuePending(t, store, a.ID)

	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionApprove, time.Now().Add(1*time.Hour))
	getResp, _ := http.Get(server.URL + "/v1/reject?t=" + url.QueryEscape(tok))
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET wrong action: status = %d, want 400", getResp.StatusCode)
	}
	postResp := postForm(t, server.URL+"/v1/reject", map[string]string{"t": tok})
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST wrong action: status = %d, want 400", postResp.StatusCode)
	}
}

func TestMagicRejectTokenRejectedAtApproveEndpoint(t *testing.T) {
	server, store, signer, smtpDone := setupMagicLinkAPI(t)
	defer smtpDone()
	a, _ := prepareHITLAgent(t, store, "magic-cross-action")
	msg := issuePending(t, store, a.ID)

	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionReject, time.Now().Add(1*time.Hour))
	postResp := postForm(t, server.URL+"/v1/approve", map[string]string{"t": tok})
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", postResp.StatusCode)
	}
}

func TestMagicLinkSecondPOSTReturns409(t *testing.T) {
	server, store, signer, _ := setupMagicLinkAPI(t)
	a, _ := prepareHITLAgent(t, store, "magic-second-post")
	msg := issuePending(t, store, a.ID)

	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionApprove, time.Now().Add(1*time.Hour))
	resp1 := postForm(t, server.URL+"/v1/approve", map[string]string{"t": tok})
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first POST: status = %d", resp1.StatusCode)
	}

	resp2 := postForm(t, server.URL+"/v1/approve", map[string]string{"t": tok})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("second POST: status = %d, want 409", resp2.StatusCode)
	}
	body := readBody(t, resp2)
	if !strings.Contains(body, "Already resolved") {
		t.Errorf("expected 'Already resolved' in body, got: %s", body)
	}
}

// TestMagicLinkGETRendersConflictForNonPending covers the UX hole where
// the reviewer opens an approve link for a message that has already
// been resolved (by dashboard, CLI, or the worker). The GET confirm
// page should surface this before any form submission.
func TestMagicLinkGETRendersConflictForNonPending(t *testing.T) {
	server, store, signer, _ := setupMagicLinkAPI(t)
	a, userID := prepareHITLAgent(t, store, "get-conflict")
	msg := issuePending(t, store, a.ID)

	// Resolve via the user-scoped API so the row is no longer pending.
	if _, err := store.RejectPending(context.Background(), msg.ID, userID, "already handled"); err != nil {
		t.Fatal(err)
	}

	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionApprove, time.Now().Add(1*time.Hour))
	resp, _ := http.Get(server.URL + "/v1/approve?t=" + url.QueryEscape(tok))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestMagicLinkNotFoundForBogusMessageID(t *testing.T) {
	server, _, signer, smtpDone := setupMagicLinkAPI(t)
	defer smtpDone()

	tok, _ := signer.Sign("msg_doesnotexist", approvaltoken.ActionApprove, time.Now().Add(1*time.Hour))
	resp, _ := http.Get(server.URL + "/v1/approve?t=" + url.QueryEscape(tok))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestMagicLinkDisabledWhenSignerMissing(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	api := agent.NewAPI(store, sender, smtpRelay, nil, usage.NewNoopUsageTracker(), "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	router := mux.NewRouter()
	api.RegisterRoutes(router)
	// Magic-link routes are no longer part of RegisterRoutes — mount directly.
	router.Handle("/v1/approve", api.ApproveMagicLinkHandler())
	router.Handle("/v1/reject", api.RejectMagicLinkHandler())
	server := httptest.NewServer(router)
	defer server.Close()

	// Both GET and POST should 404 when the signer is absent.
	getResp, _ := http.Get(server.URL + "/v1/approve?t=anything")
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Errorf("GET no signer: status = %d, want 404", getResp.StatusCode)
	}
	postResp := postForm(t, server.URL+"/v1/approve", map[string]string{"t": "anything"})
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusNotFound {
		t.Errorf("POST no signer: status = %d, want 404", postResp.StatusCode)
	}
}

func TestMagicLinkNoCacheAndSecurityHeaders(t *testing.T) {
	server, store, signer, _ := setupMagicLinkAPI(t)
	a, _ := prepareHITLAgent(t, store, "magic-headers")
	msg := issuePending(t, store, a.ID)

	tok, _ := signer.Sign(msg.ID, approvaltoken.ActionApprove, time.Now().Add(1*time.Hour))

	for _, path := range []string{
		server.URL + "/v1/approve?t=" + url.QueryEscape(tok),
	} {
		resp, _ := http.Get(path)
		resp.Body.Close()
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q", path, resp.Header.Get("Cache-Control"))
		}
		if resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q", path, resp.Header.Get("X-Frame-Options"))
		}
		if resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: Referrer-Policy = %q", path, resp.Header.Get("Referrer-Policy"))
		}
		if got := resp.Header.Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
			t.Errorf("%s: X-Robots-Tag = %q, want containing noindex", path, got)
		}
	}
}

// --- Magic-link verify path (deployment HMAC secret is the sole signer) ---

// Tokens signed with the deployment signer (cfg.Signing.HMACSecret) must
// verify. This is the only signer for magic-link tokens.
func TestMagicApprove_VerifiesWithDeploymentSigner(t *testing.T) {
	server, store, signer, _ := setupMagicLinkAPI(t)
	a, _ := prepareHITLAgent(t, store, "deployment-verify")
	msg := issuePending(t, store, a.ID)

	tok, err := signer.Sign(msg.ID, approvaltoken.ActionApprove, time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	resp := postForm(t, server.URL+"/v1/approve", map[string]string{"t": tok})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve via deployment-signed token: status %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
}

// A token signed with a secret other than the deployment secret must be
// rejected. Guards against accepting any HMAC-shaped blob just because
// the message_id resolves.
func TestMagicApprove_RejectsForeignSecret(t *testing.T) {
	server, store, _, _ := setupMagicLinkAPI(t)
	a, _ := prepareHITLAgent(t, store, "foreign-secret-reject")
	msg := issuePending(t, store, a.ID)

	tok, err := approvaltoken.Sign("attacker-controlled-secret", msg.ID, approvaltoken.ActionApprove, time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	resp := postForm(t, server.URL+"/v1/approve", map[string]string{"t": tok})
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("foreign-secret token MUST be rejected, got %d", resp.StatusCode)
	}
}
