package httpapi

import (
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
)

// B2 (review correctness bug): the `status` field must mean the same thing on
// the list (MessageSummaryView) and detail (MessageView) for the SAME message.
// Today the summary sets status=inbox_status while the detail sets
// status=delivery_status, so an outbound message reports a different `status`
// depending on which endpoint you hit — a required field that changes value on
// re-fetch. This test pins them to agree.
func TestMessageStatusConsistentAcrossViews_Outbound(t *testing.T) {
	// An outbound message: inbox_status is never written for outbound rows (""),
	// the delivery rollup is "sent", and the HITL lifecycle is "sent".
	m := identity.Message{
		ID:             "msg_b2",
		Direction:      "outbound",
		InboxStatus:    "",
		DeliveryStatus: "sent",
		Status:         "sent",
		CreatedAt:      time.Unix(1700000000, 0).UTC(),
	}
	summary := messageSummaryFromIdentity(m)
	detail := messageViewFromIdentity(&m)
	if summary.Status != detail.Status {
		t.Errorf(
			"status differs across views for the same outbound message: summary=%q detail=%q; "+
				"`status` must be identical on list and detail",
			summary.Status, detail.Status,
		)
	}
}

// Inbound is already consistent (the store resolves DeliveryStatus=InboxStatus
// for inbound rows) — this is a guard that the B2 fix doesn't break it.
func TestMessageStatusConsistentAcrossViews_Inbound(t *testing.T) {
	m := identity.Message{
		ID:             "msg_b2_in",
		Direction:      "inbound",
		InboxStatus:    "unread",
		DeliveryStatus: "unread", // store sets DeliveryStatus = InboxStatus for inbound
		CreatedAt:      time.Unix(1700000000, 0).UTC(),
	}
	summary := messageSummaryFromIdentity(m)
	detail := messageViewFromIdentity(&m)
	if summary.Status != detail.Status {
		t.Errorf("inbound status differs: summary=%q detail=%q", summary.Status, detail.Status)
	}
}

// scheduled_at must appear on BOTH the list (MessageSummaryView) and detail
// (MessageView) for the same scheduled outbound row, so the dashboard can
// distinguish a scheduled send from an ordinary queued one straight from the
// inbox list — the whole point of exposing it on the list contract.
func TestScheduledAtConsistentAcrossViews_Outbound(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC().Add(24 * time.Hour)
	m := identity.Message{
		ID:             "msg_sched",
		Direction:      "outbound",
		DeliveryStatus: "accepted",
		Status:         "sent",
		ScheduledAt:    &at,
		CreatedAt:      time.Unix(1700000000, 0).UTC(),
	}
	summary := messageSummaryFromIdentity(m)
	detail := messageViewFromIdentity(&m)
	if summary.ScheduledAt == nil || detail.ScheduledAt == nil {
		t.Fatalf("scheduled_at must appear on both views: summary=%v detail=%v", summary.ScheduledAt, detail.ScheduledAt)
	}
	if !summary.ScheduledAt.Equal(at) || !summary.ScheduledAt.Equal(*detail.ScheduledAt) {
		t.Errorf("scheduled_at differs: summary=%v detail=%v want=%v", summary.ScheduledAt, detail.ScheduledAt, at)
	}
}

// An immediate send (nil scheduled_at) and inbound rows must omit scheduled_at
// on the list view: the pointer stays nil so omitempty drops it from the wire,
// and the field is set only inside the outbound branch of the constructor.
func TestScheduledAtOmittedWhenUnset(t *testing.T) {
	immediate := messageSummaryFromIdentity(identity.Message{
		ID: "msg_imm", Direction: "outbound", DeliveryStatus: "accepted",
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	})
	if immediate.ScheduledAt != nil {
		t.Errorf("immediate send must omit scheduled_at, got %v", immediate.ScheduledAt)
	}
	at := time.Unix(1700000000, 0).UTC().Add(24 * time.Hour)
	inbound := messageSummaryFromIdentity(identity.Message{
		ID: "msg_in", Direction: "inbound", ScheduledAt: &at,
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	})
	if inbound.ScheduledAt != nil {
		t.Errorf("inbound row must omit scheduled_at, got %v", inbound.ScheduledAt)
	}
}
