package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// captureDeliverer records the SendJobs the SendWorker hands to the provider,
// so tests can assert exactly what the wire would carry (envelope sender,
// RCPT set, raw bytes) without a network.
type captureDeliverer struct {
	jobs []*outboundsend.SendJob
	out  outboundsend.DeliverOutcome
}

func (c *captureDeliverer) Deliver(_ context.Context, j *outboundsend.SendJob) outboundsend.DeliverOutcome {
	c.jobs = append(c.jobs, j)
	return c.out
}

func countAgentMessages(t *testing.T, store *identity.Store, agentID, direction string) int {
	t.Helper()
	var n int
	if err := store.WithTx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM messages WHERE agent_id=$1 AND direction=$2`, agentID, direction,
		).Scan(&n)
	}); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

// TestSendTestCore_AsyncAcceptPlatformOriginated is the endpoint's new core
// contract: POST .../test durably accepts a PLATFORM-originated message —
// msg_ resource + outbound_send job committed before any provider I/O — and
// returns status=accepted with the e2a message id (never a provider id). The
// real SendWorker then submits it over the external SMTP path with the
// platform/noreply envelope sender and the agent's own address as recipient,
// records provider_message_id, and emits email.sent. Local self-send loopback
// is never used (no locally-fabricated inbound row).
func TestSendTestCore_AsyncAcceptPlatformOriginated(t *testing.T) {
	api, store, outbox, enq := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "testaccept")

	res, oerr := api.SendTestCore(ctx, ag)
	if oerr != nil {
		t.Fatalf("SendTestCore: status=%d code=%s msg=%s", oerr.Status, oerr.Code, oerr.Msg)
	}
	if res.Status != "accepted" {
		t.Fatalf("Status = %q, want accepted", res.Status)
	}
	if !strings.HasPrefix(res.MessageID, "msg_") {
		t.Fatalf("MessageID = %q, want a durable e2a msg_ id (never a provider id)", res.MessageID)
	}
	if res.ProviderMessageID != "" {
		t.Fatalf("ProviderMessageID = %q, want empty before worker submission", res.ProviderMessageID)
	}
	if res.Method != "smtp" {
		t.Fatalf("Method = %q, want smtp (loopback must not be used)", res.Method)
	}

	// The msg_ resource is GET-able through the owner-scoped store read that
	// backs GET /v1/messages/{id}.
	got, err := store.GetOutboundMessageForUser(ctx, res.MessageID, user.ID)
	if err != nil {
		t.Fatalf("accepted test message is not GET-able: %v", err)
	}
	if got.Type != "test" {
		t.Errorf("message_type = %q, want test", got.Type)
	}
	if got.ConversationID == "" {
		t.Errorf("conversation_id empty; a fresh thread anchor must be minted like other sends")
	}

	// Row invariants: accepted, job stamped, platform envelope persisted, no
	// provider id yet, raw bytes carry the platform From identity.
	var (
		deliveryStatus, providerID, envelopeFrom, method string
		sendJobID                                        *int64
		raw                                              []byte
		toRecipients                                     []string
	)
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT delivery_status, COALESCE(provider_message_id,''), COALESCE(envelope_from,''), method, send_job_id, raw_message, to_recipients FROM messages WHERE id=$1`,
			res.MessageID,
		).Scan(&deliveryStatus, &providerID, &envelopeFrom, &method, &sendJobID, &raw, &toRecipients)
	}); err != nil {
		t.Fatalf("read accepted row: %v", err)
	}
	if deliveryStatus != "accepted" {
		t.Errorf("delivery_status = %q, want accepted", deliveryStatus)
	}
	if providerID != "" {
		t.Errorf("provider_message_id = %q, want empty at accept", providerID)
	}
	if envelopeFrom != "noreply@test.e2a.dev" {
		t.Errorf("envelope_from = %q, want noreply@test.e2a.dev (platform-originated)", envelopeFrom)
	}
	if method != "smtp" {
		t.Errorf("method = %q, want smtp", method)
	}
	if sendJobID == nil || *sendJobID != enq.jobID {
		t.Errorf("send_job_id = %v, want %d", sendJobID, enq.jobID)
	}
	if len(toRecipients) != 1 || toRecipients[0] != ag.EmailAddress() {
		t.Errorf("to_recipients = %v, want [%s]", toRecipients, ag.EmailAddress())
	}
	if !strings.Contains(string(raw), `From: "e2a" <noreply@test.e2a.dev>`) {
		t.Errorf("raw From header is not the platform identity:\n%s", raw)
	}
	// Loopback not used: no locally-fabricated inbound copy (the real inbound
	// arrives via external SMTP, outside this process).
	if n := countAgentMessages(t, store, ag.ID, "inbound"); n != 0 {
		t.Errorf("inbound rows at accept = %d, want 0 (self-send loopback must not run)", n)
	}
	// Terminal outcome not reached: no email.sent yet.
	if n := countEvents(t, store, ag.UserID, webhookpub.EventEmailSent); n != 0 {
		t.Errorf("email.sent events at accept = %d, want 0", n)
	}

	// Run the real SendWorker over the production adapter + a capturing submit.
	adapter := agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker())
	deliverer := &captureDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "<ses-test-1@amazonses.com>", SentAs: "relay"}}
	worker := outboundsend.NewSendWorker(adapter, deliverer)
	if err := worker.Work(ctx, workerJob(res.MessageID, 1)); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	// The provider saw the platform/noreply sender and the agent's own
	// address as recipient — the real external route, not loopback.
	if len(deliverer.jobs) != 1 {
		t.Fatalf("provider submits = %d, want 1", len(deliverer.jobs))
	}
	j := deliverer.jobs[0]
	if j.EnvelopeFrom != "noreply@test.e2a.dev" {
		t.Errorf("provider envelope sender = %q, want noreply@test.e2a.dev", j.EnvelopeFrom)
	}
	if len(j.Recipients) != 1 || j.Recipients[0] != ag.EmailAddress() {
		t.Errorf("provider recipients = %v, want [%s]", j.Recipients, ag.EmailAddress())
	}

	// Terminal state: provider id stored, email.sent emitted once.
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT delivery_status, COALESCE(provider_message_id,'') FROM messages WHERE id=$1`,
			res.MessageID,
		).Scan(&deliveryStatus, &providerID)
	}); err != nil {
		t.Fatalf("read sent row: %v", err)
	}
	if deliveryStatus != "sent" {
		t.Errorf("post-worker delivery_status = %q, want sent", deliveryStatus)
	}
	if providerID != "<ses-test-1@amazonses.com>" {
		t.Errorf("provider_message_id = %q, want the provider id from the worker", providerID)
	}
	if n := countEvents(t, store, ag.UserID, webhookpub.EventEmailSent); n != 1 {
		t.Errorf("email.sent events after worker = %d, want 1", n)
	}
	if n := countAgentMessages(t, store, ag.ID, "inbound"); n != 0 {
		t.Errorf("inbound rows after worker = %d, want 0 (delivery is external)", n)
	}
}

// TestSendTestCore_WorkerFailureEmitsEmailFailed: a terminal provider failure
// on a queued test send surfaces through GET state (delivery_status=failed)
// and the email.failed event — never a silent drop.
func TestSendTestCore_WorkerFailureEmitsEmailFailed(t *testing.T) {
	api, store, outbox, _ := setupAsyncAPI(t)
	ctx := context.Background()
	_, ag := selfAgent(t, store, "testfail")

	res, oerr := api.SendTestCore(ctx, ag)
	if oerr != nil {
		t.Fatalf("SendTestCore: %+v", oerr)
	}

	adapter := agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker())
	worker := outboundsend.NewSendWorker(adapter, &captureDeliverer{
		out: outboundsend.DeliverOutcome{Err: errors.New("550 mailbox unavailable"), Permanent: true},
	})
	// A permanent failure surfaces as a job-cancel error from Work; the row +
	// event are what the contract pins.
	_ = worker.Work(ctx, workerJob(res.MessageID, 1))

	var deliveryStatus string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT delivery_status FROM messages WHERE id=$1`, res.MessageID).Scan(&deliveryStatus)
	}); err != nil {
		t.Fatalf("read failed row: %v", err)
	}
	if deliveryStatus != "failed" {
		t.Errorf("delivery_status = %q, want failed", deliveryStatus)
	}
	if n := countEvents(t, store, ag.UserID, webhookpub.EventEmailFailed); n != 1 {
		t.Errorf("email.failed events = %d, want 1", n)
	}
}

// TestSendTestCore_SuppressedTargetNotAccepted: the test send honors the same
// suppression gate as every other send — a suppressed agent address returns
// the structured 422 and nothing is persisted or enqueued.
func TestSendTestCore_SuppressedTargetNotAccepted(t *testing.T) {
	api, store, _, _ := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "testsuppr")

	if _, _, err := store.AddAgentSuppression(ctx, user.ID, ag.ID, ag.EmailAddress(), "opted out", "unsubscribe", nil); err != nil {
		t.Fatalf("AddAgentSuppression: %v", err)
	}

	res, oerr := api.SendTestCore(ctx, ag)
	if res != nil {
		t.Fatalf("result = %+v, want nil for a suppressed recipient", res)
	}
	if oerr == nil || oerr.Status != 422 || oerr.Code != "recipient_suppressed" {
		t.Fatalf("error = %+v, want 422 recipient_suppressed", oerr)
	}
	if n := countAgentMessages(t, store, ag.ID, "outbound"); n != 0 {
		t.Errorf("outbound rows = %d, want 0 (suppressed target must not be accepted)", n)
	}
}

// TestSendTestCore_MissingQueueFailsClosed: like DeliverOutbound, a miswired
// process (no outbound enqueuer) fails closed before provider I/O — no inline
// SMTP submit, no orphan row.
func TestSendTestCore_MissingQueueFailsClosed(t *testing.T) {
	smtpAddr, smtpDone := testutil.FakeSMTPServer(t)
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: smtpAddr.Host, Port: smtpAddr.Port})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	api := agent.NewAPI(store, sender, smtpRelay, nil, usage.NewNoopUsageTracker(),
		"e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	_, ag := selfAgent(t, store, "testnoq")

	res, oerr := api.SendTestCore(context.Background(), ag)
	if res != nil {
		t.Fatalf("result = %+v, want nil when outbound queue is unavailable", res)
	}
	if oerr == nil || oerr.Status != 500 || oerr.Code != "internal_error" {
		t.Fatalf("error = %+v, want 500 internal_error", oerr)
	}
	if got := smtpDone(); len(got) != 0 {
		t.Fatalf("missing queue submitted %d SMTP messages; want zero (fail closed)", len(got))
	}
	if n := countAgentMessages(t, store, ag.ID, "outbound"); n != 0 {
		t.Errorf("outbound rows = %d, want 0", n)
	}
}

// TestSendTestCore_ScreeningReviewHolds: outbound screening still governs the
// test send — a review posture holds it as a durable pending_review msg_ row
// (type=test), exactly as before the async cutover.
func TestSendTestCore_ScreeningReviewHolds(t *testing.T) {
	api, store, _, _ := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "testreview")

	if _, err := store.UpdateAgentProtection(ctx, ag.ID, user.ID, identity.ProtectionConfig{
		InboundGatePolicy:       "open",
		InboundGateAction:       "review",
		InboundScanSensitivity:  identity.SensitivityOff,
		OutboundGatePolicy:      "allowlist", // empty allowlist → every recipient flagged
		OutboundGateAction:      "review",
		OutboundScanSensitivity: identity.SensitivityOff,
		HITLTTLSeconds:          3600,
		HITLExpirationAction:    "approve",
	}); err != nil {
		t.Fatalf("UpdateAgentProtection: %v", err)
	}
	refreshed, err := store.GetAgentByID(ctx, ag.ID)
	if err != nil {
		t.Fatalf("GetAgentByID: %v", err)
	}

	res, oerr := api.SendTestCore(ctx, refreshed)
	if oerr != nil {
		t.Fatalf("SendTestCore: %+v", oerr)
	}
	if !res.Held || !strings.HasPrefix(res.PendingMessageID, "msg_") {
		t.Fatalf("res = %+v, want held with a msg_ pending id", res)
	}
	var status, msgType string
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, message_type FROM messages WHERE id=$1`, res.PendingMessageID).Scan(&status, &msgType)
	}); err != nil {
		t.Fatalf("read held row: %v", err)
	}
	if status != identity.MessageStatusPendingReview || msgType != "test" {
		t.Errorf("held row = %s/%s, want pending_review/test", status, msgType)
	}
}

// TestApprovePendingCore_TestHoldStaysPlatformSMTP: human approval of a held
// platform test must keep the platform-originated external SMTP semantics —
// queued onto the outbound pipeline with the noreply envelope — and must NOT
// silently become local self-send loopback just because the recipient is the
// agent's own address.
func TestApprovePendingCore_TestHoldStaysPlatformSMTP(t *testing.T) {
	api, store, _, enq := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "testapprove")

	msg, err := store.CreatePendingOutboundMessage(ctx, ag.ID,
		[]string{ag.EmailAddress()}, nil, nil,
		"Test email from e2a", "test body", "", nil, "test", "", "", "", 3600)
	if err != nil {
		t.Fatal(err)
	}

	sent, oerr := api.ApprovePendingCore(ctx, user.ID, msg.ID, ag.Email, agent.ApproveOverrides{}, nil)
	if oerr != nil {
		t.Fatalf("ApprovePendingCore: status=%d code=%s msg=%s", oerr.Status, oerr.Code, oerr.Msg)
	}
	if sent.DeliveryStatus != "accepted" {
		t.Errorf("DeliveryStatus = %q, want accepted (queued, not loopback-delivered)", sent.DeliveryStatus)
	}
	if sent.Method != "smtp" {
		t.Errorf("Method = %q, want smtp", sent.Method)
	}

	var envelopeFrom, method string
	var sendJobID *int64
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(envelope_from,''), method, send_job_id FROM messages WHERE id=$1`, msg.ID,
		).Scan(&envelopeFrom, &method, &sendJobID)
	}); err != nil {
		t.Fatalf("read approved row: %v", err)
	}
	if envelopeFrom != "noreply@test.e2a.dev" {
		t.Errorf("envelope_from = %q, want noreply@test.e2a.dev (platform-originated)", envelopeFrom)
	}
	if method != "smtp" {
		t.Errorf("method = %q, want smtp (not loopback)", method)
	}
	if sendJobID == nil || *sendJobID != enq.jobID {
		t.Errorf("send_job_id = %v, want %d (queued onto QueueOutbound)", sendJobID, enq.jobID)
	}
	// No locally-fabricated inbound twin — the real inbound arrives via SMTP.
	if n := countAgentMessages(t, store, ag.ID, "inbound"); n != 0 {
		t.Errorf("inbound rows = %d, want 0 (approval must not loopback a test)", n)
	}
}
