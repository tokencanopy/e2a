package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhookpub"
	wsnotify "github.com/tokencanopy/e2a/internal/ws"
)

type captureHub struct{ payload []byte }

func (h *captureHub) IsConnected(string) bool { return true }
func (h *captureHub) Send(_ string, payload []byte) bool {
	h.payload = append([]byte(nil), payload...)
	return true
}

// TestSelfSend_DetectionEdgeCases: case-insensitive, whitespace-
// trimmed, single-address requirement. Mixed/external recipients must
// fall through to SMTP (covered indirectly — TestSendEmailViaSMTP
// already exercises the non-loopback path).
func TestSelfSend_DetectionEdgeCases(t *testing.T) {
	cases := []struct {
		name   string
		to     []string
		cc     []string
		want   bool
		reason string
	}{
		{"exact match", []string{"bot@x.com"}, nil, true, ""},
		{"case-insensitive local", []string{"BOT@x.com"}, nil, true, "ASCII case-insensitive"},
		{"case-insensitive domain", []string{"bot@X.COM"}, nil, true, "domain comparison is case-insensitive"},
		{"whitespace trimmed", []string{"  bot@x.com  "}, nil, true, "trim should normalize"},
		{"different address", []string{"other@x.com"}, nil, false, "not self"},
		{"self plus external in To", []string{"bot@x.com", "other@x.com"}, nil, false, "external recipient → SMTP"},
		{"self plus cc", []string{"bot@x.com"}, []string{"cc@x.com"}, false, "cc → SMTP"},
		{"empty to", []string{}, nil, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := outbound.SendRequest{To: c.to, CC: c.cc}
			got := agent.IsSelfSendForTest(req, "bot@x.com")
			if got != c.want {
				t.Errorf("isSelfSend(%v, cc=%v) = %v, want %v (%s)", c.to, c.cc, got, c.want, c.reason)
			}
		})
	}
}

// setupCoreAPI builds an *agent.API wired to a real test DB so tests can drive
// the extracted outbound core (DeliverOutbound) directly. The legacy
// POST /api/v1/send route these self-send tests once rode through was removed
// in the v1 cutover; the loopback core it called lives on (and is what /v1's
// sendMessage now invokes), so it still needs DB-backed coverage here. The
// pure HTTP-shape assertions moved to internal/httpapi; the loopback delivery
// + MIME-persistence behavior below has no /v1 unit home (httpapi tests use
// fakes), so it stays at the core level.
func setupCoreAPI(t *testing.T) (*agent.API, *identity.Store, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	api := agent.NewAPI(store, sender, smtpRelay, nil, usage.NewNoopUsageTracker(),
		"e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.SetOutbox(webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true)))
	return api, store, pool
}

// selfAgent provisions a verified domain + agent owned by a fresh user and
// returns the user and the loaded agent identity ready for DeliverOutbound.
func selfAgent(t *testing.T, store *identity.Store, label string) (*identity.User, *identity.AgentIdentity) {
	t.Helper()
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "self-"+label+"@example.com", "Owner", "google-self-"+label)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain := "self" + label + ".example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.VerifyDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	if _, err := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "local", user.ID); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	ag, err := store.GetAgentByEmail(ctx, "bot@"+domain)
	if err != nil {
		t.Fatalf("GetAgentByEmail: %v", err)
	}
	return user, ag
}

// TestSelfSend_HappyPath: an agent sending to its own address short-circuits
// to the loopback path (no SMTP) and lands BOTH an outbound and an inbound row
// tagged to the agent, with the outbound row persisting method="loopback".
func TestSelfSend_HappyPath(t *testing.T) {
	api, store, pool := setupCoreAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "owner")
	hub := &captureHub{}
	api.SetWebSocketHub(hub)
	const replyTo = "Support <support@example.com>"

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{ag.EmailAddress()}, Subject: "note to self", Body: "remember to refill coffee", ReplyTo: replyTo,
		Attachments: []outbound.Attachment{{Filename: "note.txt", ContentType: "text/plain", Data: "aGVsbG8="}},
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: status=%d code=%s msg=%s", oerr.Status, oerr.Code, oerr.Msg)
	}
	if res.Method != "loopback" {
		t.Errorf("method=%q want loopback", res.Method)
	}
	if !strings.HasPrefix(res.MessageID, "msg_") {
		t.Errorf("message_id=%q should be the GET-able outbound resource id", res.MessageID)
	}
	if res.ProviderMessageID != "" {
		t.Errorf("provider_message_id=%q want empty for providerless loopback delivery", res.ProviderMessageID)
	}

	var outboundCount, inboundCount int
	pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE agent_id=$1 AND direction='outbound' AND subject='note to self'`,
		ag.ID).Scan(&outboundCount)
	pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE agent_id=$1 AND direction='inbound' AND subject='note to self'`,
		ag.ID).Scan(&inboundCount)
	if outboundCount != 1 {
		t.Errorf("outbound rows=%d want 1", outboundCount)
	}
	if inboundCount != 1 {
		t.Errorf("inbound rows=%d want 1", inboundCount)
	}

	var inboundID, sender, recipient string
	pool.QueryRow(ctx,
		`SELECT id, sender, recipient FROM messages WHERE agent_id=$1 AND direction='inbound' AND subject='note to self'`,
		ag.ID).Scan(&inboundID, &sender, &recipient)
	if sender != "support@example.com" || recipient != ag.EmailAddress() {
		t.Errorf("self-note row sender=%q recipient=%q; want Reply-To and agent address", sender, recipient)
	}

	var method, deliveryStatus, providerID string
	var outboundRaw []byte
	pool.QueryRow(ctx,
		`SELECT method, COALESCE(delivery_status,''), provider_message_id, raw_message
		   FROM messages WHERE id=$1`, res.MessageID).Scan(&method, &deliveryStatus, &providerID, &outboundRaw)
	if method != "loopback" {
		t.Errorf("outbound method=%q want loopback", method)
	}
	if deliveryStatus != "sent" {
		t.Errorf("outbound delivery_status=%q want sent", deliveryStatus)
	}
	if len(outboundRaw) == 0 || !strings.Contains(string(outboundRaw), "remember to refill coffee") {
		t.Errorf("outbound sent copy must retain readable MIME; raw=%q", outboundRaw)
	}

	var inboundProviderID string
	if err := pool.QueryRow(ctx,
		`SELECT email_message_id FROM messages
		  WHERE agent_id=$1 AND direction='inbound' AND subject='note to self'`, ag.ID,
	).Scan(&inboundProviderID); err != nil {
		t.Fatalf("fetch inbound Message-ID: %v", err)
	}
	if providerID == "" || inboundProviderID != providerID {
		t.Errorf("loopback Message-ID mismatch: outbound=%q inbound=%q", providerID, inboundProviderID)
	}
	if !strings.Contains(string(outboundRaw), "Message-ID: "+providerID) {
		t.Errorf("loopback MIME missing its synthetic Message-ID header: %q", outboundRaw)
	}

	rows, err := pool.Query(ctx,
		`SELECT type, message_id FROM webhook_events
		  WHERE message_id IN (
		    SELECT id FROM messages WHERE agent_id=$1 AND subject='note to self'
		  ) ORDER BY type`, ag.ID)
	if err != nil {
		t.Fatalf("list loopback events: %v", err)
	}
	defer rows.Close()
	var eventTypes []string
	for rows.Next() {
		var eventType, messageID string
		if err := rows.Scan(&eventType, &messageID); err != nil {
			t.Fatalf("scan loopback event: %v", err)
		}
		eventTypes = append(eventTypes, eventType)
	}
	if got, want := strings.Join(eventTypes, ","), "email.received,email.sent"; got != want {
		t.Errorf("loopback events=%q want %q", got, want)
	}
	var sentEventHasProviderID bool
	if err := pool.QueryRow(ctx,
		`SELECT envelope->'data' ? 'provider_message_id'
		   FROM webhook_events WHERE message_id=$1 AND type='email.sent'`, res.MessageID,
	).Scan(&sentEventHasProviderID); err != nil {
		t.Fatalf("read loopback email.sent payload: %v", err)
	}
	if sentEventHasProviderID {
		t.Error("providerless loopback email.sent must omit provider_message_id")
	}
	assertLoopbackLifecycleParity(t, pool, res.MessageID, inboundID)
	var durableEnvelope []byte
	if err := pool.QueryRow(ctx,
		`SELECT envelope FROM webhook_events WHERE message_id=$1 AND type='email.received'`, inboundID,
	).Scan(&durableEnvelope); err != nil {
		t.Fatalf("read durable received envelope: %v", err)
	}
	if !bytes.Equal(hub.payload, durableEnvelope) {
		t.Errorf("live WebSocket frame must reuse exact committed envelope bytes\nlive=%s\ndurable=%s", hub.payload, durableEnvelope)
	}
	var durable, live map[string]any
	if err := json.Unmarshal(durableEnvelope, &durable); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(hub.payload, &live); err != nil {
		t.Fatalf("live WebSocket payload: %v (%q)", err, hub.payload)
	}
	if !reflect.DeepEqual(live, durable) {
		t.Errorf("live WebSocket envelope drifted from durable webhook event\nlive=%v\ndurable=%v", live, durable)
	}
	listed, err := store.GetMessagesByAgent(ctx, identity.MessageListFilter{
		AgentID: ag.ID, Status: "all", Direction: "inbound", Limit: 10,
	})
	if err != nil || len(listed) != 1 {
		t.Fatalf("list reconnect message: len=%d err=%v", len(listed), err)
	}
	if listed[0].Method != "loopback" {
		t.Fatalf("reconnect row method = %q, want durable loopback marker", listed[0].Method)
	}
	var reconnect map[string]any
	if err := json.Unmarshal(wsnotify.NotificationForMessage(ctx, store, &listed[0]), &reconnect); err != nil {
		t.Fatal(err)
	}
	reconnectData := reconnect["data"].(map[string]any)
	if !reflect.DeepEqual(reconnect, durable) {
		t.Errorf("reconnect envelope drifted from durable event\nreconnect=%v\ndurable=%v", reconnect, durable)
	}
	if reconnect["id"] != live["id"] || reconnectData["header_from"] != ag.EmailAddress() || reconnectData["authentication"] != nil {
		t.Errorf("reconnect identity drift: %v", reconnect)
	}
	data := live["data"].(map[string]any)
	if data["header_from"] != ag.EmailAddress() || data["envelope_from"] != nil || data["authentication"] != nil {
		t.Errorf("received identities = header_from:%v envelope_from:%v authentication:%v", data["header_from"], data["envelope_from"], data["authentication"])
	}
	if attachments, ok := data["attachments"].([]any); !ok || len(attachments) != 1 {
		t.Errorf("live/durable/reconnect envelope lost attachment metadata: %v", data["attachments"])
	}
}

// The idempotency completion hook must share the same transaction as both
// loopback message rows and their lifecycle events. A completion failure is a
// failure to commit the local delivery, not a partial Sent/Inbox pair.
func TestSelfSend_IdempotencyCompletionFailureRollsBackDeliveryLifecycle(t *testing.T) {
	api, store, pool := setupCoreAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "idemrollback")
	hub := &captureHub{}
	api.SetWebSocketHub(hub)
	var lifecycleBaseline, eventBaseline int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions`).Scan(&lifecycleBaseline); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM webhook_events`).Scan(&eventBaseline); err != nil {
		t.Fatal(err)
	}

	_, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{ag.EmailAddress()}, Subject: "atomic rollback", Body: "must not persist",
	}, "send", "", nil, func(_ context.Context, tx pgx.Tx, result *agent.OutboundResult) error {
		if !strings.HasPrefix(result.MessageID, "msg_") {
			t.Errorf("idempotency completion message_id=%q want msg_ resource id", result.MessageID)
		}
		if result.Method != "loopback" || result.Status != "" {
			t.Errorf("idempotency completion result=%+v want terminal loopback", result)
		}
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM messages WHERE agent_id=$1 AND subject='atomic rollback'`, ag.ID,
		).Scan(&count); err != nil {
			t.Fatalf("count transaction-local loopback rows: %v", err)
		}
		if count != 2 {
			t.Errorf("transaction-local loopback rows=%d want 2", count)
		}
		return errors.New("inject idempotency completion failure")
	})
	if oerr == nil || oerr.Code != "internal_error" {
		t.Fatalf("DeliverOutbound error=%v want internal_error", oerr)
	}

	var messages, events, lifecycleAfter int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE agent_id=$1 AND subject='atomic rollback'`, ag.ID,
	).Scan(&messages); err != nil {
		t.Fatalf("count rolled-back messages: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM webhook_events`).Scan(&events); err != nil {
		t.Fatalf("count rolled-back events: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions`).Scan(&lifecycleAfter); err != nil {
		t.Fatal(err)
	}
	if messages != 0 || events != eventBaseline || lifecycleAfter != lifecycleBaseline {
		t.Fatalf("partial loopback commit: messages=%d events=%d lifecycle before=%d after=%d", messages, events, lifecycleBaseline, lifecycleAfter)
	}
	if len(hub.payload) != 0 {
		t.Fatalf("rolled-back loopback sent a live WebSocket frame: %s", hub.payload)
	}
}

func TestSelfSend_LiveWebSocketSkipsMissingStoredEnvelope(t *testing.T) {
	api, store, pool := setupCoreAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "missingliveenvelope")
	hub := &captureHub{}
	api.SetWebSocketHub(hub)
	api.SetOutbox(webhookpub.NewOutbox(pool, webhookpub.StaticFlag(false)))

	if _, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{ag.EmailAddress()}, Subject: "no durable live frame", Body: "body",
	}, "send", "", nil, nil); oerr != nil {
		t.Fatalf("DeliverOutbound: %v", oerr)
	}
	if len(hub.payload) != 0 {
		t.Fatalf("missing durable envelope sent reconstructed WebSocket frame: %s", hub.payload)
	}
}

func TestSelfSend_LifecycleFailureRollsBackDelivery(t *testing.T) {
	api, store, pool := setupCoreAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "lifecyclerollback")
	var lifecycleBaseline, eventBaseline int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions`).Scan(&lifecycleBaseline); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM webhook_events`).Scan(&eventBaseline); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `DROP TRIGGER IF EXISTS test_fail_loopback_lifecycle ON message_lifecycle_transitions; DROP FUNCTION IF EXISTS test_fail_loopback_lifecycle(); CREATE FUNCTION test_fail_loopback_lifecycle() RETURNS trigger AS $f$ BEGIN IF NEW.reason_code='submission.local_loopback_accepted' THEN RAISE EXCEPTION 'forced loopback lifecycle failure'; END IF; RETURN NEW; END; $f$ LANGUAGE plpgsql; CREATE TRIGGER test_fail_loopback_lifecycle BEFORE INSERT ON message_lifecycle_transitions FOR EACH ROW EXECUTE FUNCTION test_fail_loopback_lifecycle();`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_fail_loopback_lifecycle ON message_lifecycle_transitions; DROP FUNCTION IF EXISTS test_fail_loopback_lifecycle();`)
	})
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{To: []string{ag.EmailAddress()}, Subject: "loopback lifecycle rollback", Body: "body"}, "send", "", nil, nil)
	if res != nil || oerr == nil {
		t.Fatalf("result=%+v error=%+v want failure", res, oerr)
	}
	var messages, events, lifecycleAfter int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE agent_id=$1 AND subject='loopback lifecycle rollback'`, ag.ID).Scan(&messages)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM webhook_events`).Scan(&events)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM message_lifecycle_transitions`).Scan(&lifecycleAfter)
	if messages != 0 || events != eventBaseline || lifecycleAfter != lifecycleBaseline {
		t.Fatalf("partial loopback messages=%d events=%d lifecycle before=%d after=%d", messages, events, lifecycleBaseline, lifecycleAfter)
	}
}

func assertLoopbackLifecycleParity(t *testing.T, pool *pgxpool.Pool, outboundID, inboundID string) {
	assertLoopbackLifecycleParityWithReasons(t, pool, outboundID, inboundID, []messagelifecycle.ReasonCode{
		messagelifecycle.ReasonAcceptanceOutboundAPI,
		messagelifecycle.ReasonSubmissionLocalLoopbackAccepted,
	})
}

func assertLoopbackLifecycleParityWithReasons(t *testing.T, pool *pgxpool.Pool, outboundID, inboundID string, outboundReasons []messagelifecycle.ReasonCode) {
	t.Helper()
	ctx := context.Background()
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate River lifecycle read dependency: %v", err)
	}
	read := func(messageID string) []messagelifecycle.MessageLifecycleTransition {
		got, err := messagelifecycle.NewStore(pool).ListForMessage(ctx, messageID, messageAgentID(t, pool, messageID))
		if err != nil {
			t.Fatalf("public lifecycle read: %v", err)
		}
		return got
	}
	outboundTransitions, inboundTransitions := read(outboundID), read(inboundID)
	if len(outboundTransitions) != len(outboundReasons) {
		t.Fatalf("outbound loopback lifecycle=%+v", outboundTransitions)
	}
	wantReasons := make(map[messagelifecycle.ReasonCode]bool, len(outboundReasons))
	for _, reason := range outboundReasons {
		wantReasons[reason] = true
	}
	for _, transition := range outboundTransitions {
		if !wantReasons[transition.ReasonCode] {
			t.Fatalf("unexpected outbound lifecycle reason %s in %+v", transition.ReasonCode, outboundTransitions)
		}
		delete(wantReasons, transition.ReasonCode)
	}
	if len(wantReasons) != 0 {
		t.Fatalf("missing outbound lifecycle reasons %v", wantReasons)
	}
	if len(inboundTransitions) != 1 || inboundTransitions[0].ReasonCode != messagelifecycle.ReasonAcceptanceLocalLoopback {
		t.Fatalf("inbound loopback lifecycle=%+v", inboundTransitions)
	}
	for _, tc := range []struct {
		id, event string
		reason    messagelifecycle.ReasonCode
		all       []messagelifecycle.MessageLifecycleTransition
	}{{outboundID, "email.sent", messagelifecycle.ReasonSubmissionLocalLoopbackAccepted, outboundTransitions}, {inboundID, "email.received", messagelifecycle.ReasonAcceptanceLocalLoopback, inboundTransitions}} {
		var raw []byte
		if err := pool.QueryRow(ctx, `SELECT envelope->'data'->'lifecycle_transitions' FROM webhook_events WHERE message_id=$1 AND type=$2`, tc.id, tc.event).Scan(&raw); err != nil {
			t.Fatalf("%s lifecycle payload: %v", tc.event, err)
		}
		var got []messagelifecycle.MessageLifecycleTransition
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ReasonCode != tc.reason {
			t.Fatalf("%s lifecycle=%+v want only %s", tc.event, got, tc.reason)
		}
		var matching *messagelifecycle.MessageLifecycleTransition
		for i := range tc.all {
			if tc.all[i].ID == got[0].ID {
				matching = &tc.all[i]
				break
			}
		}
		if matching == nil || !reflect.DeepEqual(got[0], *matching) {
			t.Fatalf("%s transition differs from public lifecycle read\nevent=%+v\nread=%+v", tc.event, got, tc.all)
		}
	}
	var sentEnvelope, receivedEnvelope []byte
	if err := pool.QueryRow(ctx, `SELECT envelope FROM webhook_events WHERE message_id=$1 AND type='email.sent'`, outboundID).Scan(&sentEnvelope); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT envelope FROM webhook_events WHERE message_id=$1 AND type='email.received'`, inboundID).Scan(&receivedEnvelope); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range append(append([]messagelifecycle.MessageLifecycleTransition(nil), outboundTransitions...), inboundTransitions...) {
		dedupe := map[messagelifecycle.ReasonCode]string{
			messagelifecycle.ReasonAcceptanceOutboundAPI:           "acceptance",
			messagelifecycle.ReasonReviewHoldCreated:               "review:hold",
			messagelifecycle.ReasonReviewApproved:                  "review:resolution",
			messagelifecycle.ReasonReviewExpiredApproved:           "review:resolution",
			messagelifecycle.ReasonSubmissionLocalLoopbackAccepted: "submission:local-loopback",
			messagelifecycle.ReasonAcceptanceLocalLoopback:         "acceptance",
		}[transition.ReasonCode]
		if _, err := messagelifecycle.AppendTx(ctx, tx, messagelifecycle.AppendInput{MessageID: transition.MessageID, DedupeKey: dedupe, Direction: transition.Direction, Recipient: transition.Recipient, ReasonCode: transition.ReasonCode, Evidence: transition.Evidence, CorrelationIDs: transition.CorrelationIDs, OccurredAt: transition.OccurredAt}); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("redrive %s: %v", transition.ReasonCode, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	outboundAfter := read(outboundID)
	inboundAfter := read(inboundID)
	var sentAfter, receivedAfter []byte
	_ = pool.QueryRow(ctx, `SELECT envelope FROM webhook_events WHERE message_id=$1 AND type='email.sent'`, outboundID).Scan(&sentAfter)
	_ = pool.QueryRow(ctx, `SELECT envelope FROM webhook_events WHERE message_id=$1 AND type='email.received'`, inboundID).Scan(&receivedAfter)
	if !reflect.DeepEqual(outboundAfter, outboundTransitions) || !reflect.DeepEqual(inboundAfter, inboundTransitions) || !bytes.Equal(sentAfter, sentEnvelope) || !bytes.Equal(receivedAfter, receivedEnvelope) {
		t.Fatal("duplicate lifecycle append changed logical transitions or stored envelopes")
	}
}

func messageAgentID(t *testing.T, pool *pgxpool.Pool, messageID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT agent_id FROM messages WHERE id=$1`, messageID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertApprovedLoopbackLifecycleParity(t *testing.T, pool *pgxpool.Pool, outboundID, inboundID string) {
	t.Helper()
	assertLoopbackLifecycleParityWithReasons(t, pool, outboundID, inboundID, []messagelifecycle.ReasonCode{
		messagelifecycle.ReasonAcceptanceOutboundAPI,
		messagelifecycle.ReasonReviewHoldCreated,
		messagelifecycle.ReasonReviewApproved,
		messagelifecycle.ReasonSubmissionLocalLoopbackAccepted,
	})
}

// TestSelfSend_FutureSendAtRejected: a self-send (direct loopback to the
// agent's own address) is delivered immediately in-process — the scheduling
// path (River ScheduledAt) never runs for it — so a future send_at would be
// silently dropped. DeliverOutbound must refuse it with 400 invalid_request
// rather than deliver early against intent, and persist nothing. This is the
// behavioral pin for the contract the OpenAPI send_at docs describe
// (previously only spec-text pinned).
func TestSelfSend_FutureSendAtRejected(t *testing.T) {
	api, store, pool := setupCoreAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "schedreject")

	future := time.Now().Add(24 * time.Hour).UTC()
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{ag.EmailAddress()}, Subject: "scheduled self note", Body: "must not persist",
		ScheduledAt: &future,
	}, "send", "", nil, nil)
	if res != nil {
		t.Fatalf("scheduled self-send returned result %+v, want refusal", res)
	}
	if oerr == nil || oerr.Status != 400 || oerr.Code != "invalid_request" {
		t.Fatalf("scheduled self-send error = %+v, want 400 invalid_request", oerr)
	}

	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE agent_id=$1 AND subject='scheduled self note'`, ag.ID,
	).Scan(&rows); err != nil {
		t.Fatalf("count refused self-send rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("refused scheduled self-send persisted %d message rows, want 0", rows)
	}

	// Sanity: the same self-send WITHOUT send_at still loopbacks — the refusal
	// is specifically the scheduling combination, not self-send itself.
	res, oerr = api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{ag.EmailAddress()}, Subject: "plain self note", Body: "hi me",
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("plain self-send after refusal: status=%d code=%s msg=%s", oerr.Status, oerr.Code, oerr.Msg)
	}
	if res.Method != "loopback" {
		t.Errorf("plain self-send method = %q, want loopback", res.Method)
	}
}

// TestSelfSend_PreservesAttachmentsInMIME: a self-send with an attachment must
// persist the attachment in the inbound row's raw_message so the SDK's MIME
// parser finds it on read. Guards a past regression where the loopback path
// stored only req.Body and silently dropped req.Attachments. Also asserts the
// synthetic Received: trace header (RFC 5321 §4.4) is present.
func TestSelfSend_PreservesAttachmentsInMIME(t *testing.T) {
	api, store, pool := setupCoreAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "attach")

	// "aGVsbG8gZmlsZQ==" is base64 of "hello file".
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To:      []string{ag.EmailAddress()},
		Subject: "note with file",
		Body:    "see attached",
		Attachments: []outbound.Attachment{{
			Filename: "note.txt", ContentType: "text/plain", Data: "aGVsbG8gZmlsZQ==",
		}},
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: status=%d msg=%s", oerr.Status, oerr.Msg)
	}
	if res.Method != "loopback" {
		t.Errorf("method=%q want loopback", res.Method)
	}

	var rawBytes []byte
	if err := pool.QueryRow(ctx,
		`SELECT raw_message FROM messages WHERE agent_id=$1 AND direction='inbound' AND subject='note with file'`,
		ag.ID).Scan(&rawBytes); err != nil {
		t.Fatalf("fetch inbound row: %v", err)
	}
	raw := string(rawBytes)

	if !strings.HasPrefix(raw, "Received: by ") {
		t.Errorf("inbound raw_message should start with synthetic Received: header; got:\n%.200s", raw)
	}
	if !strings.Contains(raw, "with loopback id ") {
		t.Errorf("Received: header should carry 'with loopback id' keyword; got:\n%.300s", raw)
	}
	if !strings.Contains(raw, "Content-Type: multipart/") {
		t.Errorf("raw_message should be multipart MIME (attachments present); got:\n%.500s", raw)
	}
	if !strings.Contains(raw, `filename="note.txt"`) {
		t.Errorf("attachment filename header missing from MIME; got:\n%.800s", raw)
	}
	if !strings.Contains(raw, "aGVsbG8gZmlsZQ==") {
		t.Errorf("attachment base64 payload missing from MIME body; got:\n%.800s", raw)
	}
	if !strings.Contains(raw, "From: "+ag.EmailAddress()) {
		t.Errorf("From: header should be the agent's own address; got:\n%.300s", raw)
	}
	if !strings.Contains(raw, "To: "+ag.EmailAddress()) {
		t.Errorf("To: header should be the agent's own address; got:\n%.300s", raw)
	}
}

// TestSelfSend_NoAttachmentsUsesSinglePart: the attachment-less loopback path
// uses the simpler single-part composer (no multipart wrapper), keeping the
// stored MIME small for the dominant note-to-self case.
func TestSelfSend_NoAttachmentsUsesSinglePart(t *testing.T) {
	api, store, pool := setupCoreAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "plain")

	if _, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{ag.EmailAddress()}, Subject: "plain", Body: "hi me",
	}, "send", "", nil, nil); oerr != nil {
		t.Fatalf("DeliverOutbound: status=%d msg=%s", oerr.Status, oerr.Msg)
	}

	var rawBytes []byte
	pool.QueryRow(ctx,
		`SELECT raw_message FROM messages WHERE agent_id=$1 AND direction='inbound' AND subject='plain'`,
		ag.ID).Scan(&rawBytes)
	raw := string(rawBytes)

	if !strings.HasPrefix(raw, "Received: by ") {
		t.Errorf("Received: header missing on plain self-send; got:\n%.200s", raw)
	}
	if strings.Contains(raw, "Content-Type: multipart/") {
		t.Errorf("plain self-send should NOT use multipart MIME; got:\n%.400s", raw)
	}
	if !strings.Contains(raw, "hi me") {
		t.Errorf("body text missing from raw_message; got:\n%.400s", raw)
	}
}
