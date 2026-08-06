package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// reviewGatedAgent turns a normal agent into "require human review for every
// outbound email" — outbound_gate_policy=allowlist with an empty allowlist and
// action=review — and returns the refreshed identity so DeliverOutbound sees the
// updated protection.
func reviewGatedAgent(t *testing.T, store *identity.Store, ctx context.Context, user *identity.User, ag *identity.AgentIdentity) *identity.AgentIdentity {
	t.Helper()
	if _, err := store.UpdateAgentProtection(ctx, ag.ID, user.ID, identity.ProtectionConfig{
		InboundGatePolicy:       "open",
		InboundGateAction:       "flag",
		InboundScanSensitivity:  identity.SensitivityOff,
		OutboundGatePolicy:      "allowlist", // empty allowlist → every recipient held
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
	return refreshed
}

func readScheduledAt(t *testing.T, store *identity.Store, ctx context.Context, messageID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT scheduled_at FROM messages WHERE id=$1`, messageID).Scan(&at)
	}); err != nil {
		t.Fatalf("read scheduled_at: %v", err)
	}
	return at
}

// TestDeliverOutbound_HoldPreservesScheduledAt: a scheduled send caught by a
// review hold must NOT discard send_at (#815). The held result surfaces
// scheduled_at, and the pending_review row persists it so the approval path can
// re-arm the schedule.
func TestDeliverOutbound_HoldPreservesScheduledAt(t *testing.T) {
	api, store, _, _ := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "holdsched")
	ag = reviewGatedAgent(t, store, ctx, user, ag)
	at := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"alice@external.test"}, Subject: "later, if approved", Body: "b",
		ScheduledAt: &at,
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}
	if res == nil || !res.Held {
		t.Fatalf("result = %+v, want a held result", res)
	}
	if res.ScheduledAt == nil || !res.ScheduledAt.Equal(at) {
		t.Fatalf("held result ScheduledAt = %v, want %v (schedule must survive the hold)", res.ScheduledAt, at)
	}
	if got := readScheduledAt(t, store, ctx, res.PendingMessageID); got == nil || !got.Equal(at) {
		t.Fatalf("stored scheduled_at = %v, want %v persisted on the held row", got, at)
	}
}

// TestDeliverOutbound_HeldSelfSendRejectsScheduledAt: a self-send can never honor
// a future send_at (approval delivers via immediate loopback), so it is rejected
// up front — even when the agent's gate would otherwise hold it for review. The
// schedule is never silently persisted-then-dropped (#815).
func TestDeliverOutbound_HeldSelfSendRejectsScheduledAt(t *testing.T) {
	api, store, _, _ := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "heldselfsched")
	ag = reviewGatedAgent(t, store, ctx, user, ag)
	at := time.Now().Add(30 * time.Minute).UTC()

	_, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{ag.EmailAddress()}, Subject: "self later", Body: "b", ScheduledAt: &at,
	}, "send", "", nil, nil)
	if oerr == nil || oerr.Status != 400 || oerr.Code != "invalid_request" {
		t.Fatalf("held self-send with send_at: want 400 invalid_request, got %+v", oerr)
	}
}

// TestApprovePendingCore_ReArmsFutureSchedule: approving a held draft whose
// send_at is still in the future re-arms the schedule — the send is enqueued to
// run at scheduled_at (not immediately), and the approved row keeps scheduled_at
// (#815).
func TestApprovePendingCore_ReArmsFutureSchedule(t *testing.T) {
	api, store, _, enq := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "rearmfuture")
	ag = reviewGatedAgent(t, store, ctx, user, ag)
	at := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"alice@external.test"}, Subject: "scheduled + held", Body: "b",
		ScheduledAt: &at,
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}

	sent, oerr := api.ApprovePendingCore(ctx, user.ID, res.PendingMessageID, ag.Email, agent.ApproveOverrides{}, nil)
	if oerr != nil {
		t.Fatalf("ApprovePendingCore: status=%d code=%s msg=%s", oerr.Status, oerr.Code, oerr.Msg)
	}
	if sent.ScheduledAt == nil || !sent.ScheduledAt.Equal(at) {
		t.Fatalf("approved message ScheduledAt = %v, want %v (re-armed)", sent.ScheduledAt, at)
	}
	// The scheduled instant was threaded to the River enqueue, not the immediate arm.
	if !enq.scheduledAt.Equal(at) {
		t.Fatalf("enqueued ScheduledAt = %v, want %v (re-armed to scheduled, not immediate)", enq.scheduledAt, at)
	}
	if got := readScheduledAt(t, store, ctx, res.PendingMessageID); got == nil || !got.Equal(at) {
		t.Errorf("approved row scheduled_at = %v, want %v retained", got, at)
	}
}

// TestApprovePendingCore_PastScheduleSendsImmediately: when send_at has already
// passed by the time a human approves, "not before" is satisfied and approval was
// the last blocker — the send goes out immediately (the immediate enqueue arm),
// not on the scheduled arm (#815).
func TestApprovePendingCore_PastScheduleSendsImmediately(t *testing.T) {
	api, store, _, enq := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "pastsched")

	// Create a held draft and stamp a scheduled_at that is already in the past —
	// simulating a schedule that lapsed while the message sat in review. (The
	// accept path only ever persists a future instant; time then moves on.)
	msg, err := store.CreatePendingOutboundMessage(ctx, ag.ID,
		[]string{"alice@external.test"}, nil, nil, "was scheduled", "b", "", nil, "send", "", "", "", 3600)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return store.StampScheduledAtTx(ctx, tx, msg.ID, past)
	}); err != nil {
		t.Fatalf("StampScheduledAtTx: %v", err)
	}

	sent, oerr := api.ApprovePendingCore(ctx, user.ID, msg.ID, ag.Email, agent.ApproveOverrides{}, nil)
	if oerr != nil {
		t.Fatalf("ApprovePendingCore: status=%d code=%s msg=%s", oerr.Status, oerr.Code, oerr.Msg)
	}
	if sent.DeliveryStatus != "accepted" {
		t.Errorf("DeliveryStatus = %q, want accepted", sent.DeliveryStatus)
	}
	// A past schedule must NOT re-arm the scheduled River job — it submits now.
	if !enq.scheduledAt.IsZero() {
		t.Fatalf("enqueued ScheduledAt = %v, want the immediate arm (zero) for a lapsed schedule", enq.scheduledAt)
	}
}

// TestApprovePendingCore_EditedSelfSendFutureScheduleRejects: approving a held
// message that still carries a future send_at while EDITING the recipients to the
// agent's own address is rejected up front — self-delivery is an immediate
// loopback with no scheduled arm, so the approve would silently drop the schedule
// (#815). The hold stays pending_review, so a corrected approve (without the
// self-addressed edit) succeeds and re-arms the schedule.
func TestApprovePendingCore_EditedSelfSendFutureScheduleRejects(t *testing.T) {
	api, store, _, enq := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "editselfsched")
	ag = reviewGatedAgent(t, store, ctx, user, ag)
	at := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)

	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"alice@external.test"}, Subject: "held + scheduled", Body: "b",
		ScheduledAt: &at,
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}

	self := []string{ag.EmailAddress()}
	_, oerr = api.ApprovePendingCore(ctx, user.ID, res.PendingMessageID, ag.Email, agent.ApproveOverrides{To: &self}, nil)
	if oerr == nil || oerr.Status != 400 || oerr.Code != "invalid_request" {
		t.Fatalf("edited self-send on a still-scheduled hold: want 400 invalid_request, got %+v", oerr)
	}

	// The hold is untouched — a corrected approve (original recipients, no edit)
	// succeeds and re-arms the schedule.
	sent, oerr := api.ApprovePendingCore(ctx, user.ID, res.PendingMessageID, ag.Email, agent.ApproveOverrides{}, nil)
	if oerr != nil {
		t.Fatalf("corrected approve: status=%d code=%s msg=%s", oerr.Status, oerr.Code, oerr.Msg)
	}
	if sent.ScheduledAt == nil || !sent.ScheduledAt.Equal(at) {
		t.Fatalf("corrected approve ScheduledAt = %v, want %v (re-armed)", sent.ScheduledAt, at)
	}
	if !enq.scheduledAt.Equal(at) {
		t.Fatalf("corrected approve enqueued ScheduledAt = %v, want %v", enq.scheduledAt, at)
	}
}

// TestApprovePendingCore_EditedSelfSendLapsedScheduleDeliversNow: once the
// schedule has lapsed while the message sat in review, "not before" is satisfied —
// so a reviewer edit that re-targets the agent's own address is allowed and the
// approval delivers via the immediate loopback (no schedule to drop).
func TestApprovePendingCore_EditedSelfSendLapsedScheduleDeliversNow(t *testing.T) {
	api, store, _, _ := setupAsyncAPI(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "editselfschedlapsed")

	msg, err := store.CreatePendingOutboundMessage(ctx, ag.ID,
		[]string{"alice@external.test"}, nil, nil, "lapsed then re-targeted", "b", "", nil, "send", "", "", "", 3600)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return store.StampScheduledAtTx(ctx, tx, msg.ID, past)
	}); err != nil {
		t.Fatalf("StampScheduledAtTx: %v", err)
	}

	self := []string{ag.EmailAddress()}
	sent, oerr := api.ApprovePendingCore(ctx, user.ID, msg.ID, ag.Email, agent.ApproveOverrides{To: &self}, nil)
	if oerr != nil {
		t.Fatalf("approve of lapsed schedule with self-edit: status=%d code=%s msg=%s", oerr.Status, oerr.Code, oerr.Msg)
	}
	if sent.Method != "loopback" {
		t.Fatalf("Method = %q, want loopback (immediate self-delivery for a lapsed schedule)", sent.Method)
	}
}
