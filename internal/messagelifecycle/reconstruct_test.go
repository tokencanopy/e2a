package messagelifecycle

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

var reconstructBaseTime = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

func TestReconstructAcceptanceByDirectionAndMethod(t *testing.T) {
	tests := []struct {
		name, direction, method string
		want                    ReasonCode
	}{
		{"outbound API", "outbound", "smtp", ReasonAcceptanceOutboundAPI},
		{"inbound SMTP", "inbound", "smtp", ReasonAcceptanceInboundSMTP},
		{"inbound loopback", "inbound", "loopback", ReasonAcceptanceLocalLoopback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Reconstruct(baseSnapshot(tt.direction, tt.method))
			assertReasons(t, got, tt.want)
			if !got[0].OccurredAt.Equal(reconstructBaseTime) {
				t.Fatalf("acceptance occurred_at = %v", got[0].OccurredAt)
			}
		})
	}
}

func TestReconstructAuthenticationMappingsAndMalformedOmission(t *testing.T) {
	for status, want := range map[string]ReasonCode{
		"pass": ReasonAuthenticationDMARCPass, "fail": ReasonAuthenticationDMARCFail,
		"none": ReasonAuthenticationDMARCNone, "temperror": ReasonAuthenticationDMARCTemporaryError,
		"permerror": ReasonAuthenticationDMARCPermanentError,
	} {
		t.Run(status, func(t *testing.T) {
			snapshot := baseSnapshot("inbound", "smtp")
			snapshot.Authentication = authenticationJSON(status)
			got := Reconstruct(snapshot)
			assertReasons(t, got, ReasonAcceptanceInboundSMTP, want)
			if findReason(got, want).Evidence["authentication"] == nil {
				t.Fatal("authentication evidence missing")
			}
		})
	}
	for name, raw := range map[string]json.RawMessage{
		"malformed": []byte(`{"dmarc":`),
		"unknown":   authenticationJSON("future"),
		"outbound":  authenticationJSON("pass"),
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := baseSnapshot("inbound", "smtp")
			if name == "outbound" {
				snapshot.Direction = "outbound"
			}
			snapshot.Authentication = raw
			got := Reconstruct(snapshot)
			if hasStage(got, StageAuthentication) {
				t.Fatalf("unexpected authentication: %#v", got)
			}
		})
	}
}

func TestReconstructReviewRules(t *testing.T) {
	reviewed := reconstructBaseTime.Add(time.Minute)
	tests := []struct {
		status   string
		reviewed *time.Time
		want     []ReasonCode
	}{
		{"pending_review", nil, []ReasonCode{ReasonAcceptanceOutboundAPI, ReasonReviewHoldCreated}},
		{"sent", &reviewed, []ReasonCode{ReasonAcceptanceOutboundAPI, ReasonReviewApproved}},
		{"review_approved", &reviewed, []ReasonCode{ReasonAcceptanceOutboundAPI, ReasonReviewApproved}},
		{"review_rejected", &reviewed, []ReasonCode{ReasonAcceptanceOutboundAPI, ReasonReviewRejected}},
		{"review_expired_approved", &reviewed, []ReasonCode{ReasonAcceptanceOutboundAPI, ReasonReviewExpiredApproved}},
		{"review_expired_rejected", &reviewed, []ReasonCode{ReasonAcceptanceOutboundAPI, ReasonReviewExpiredRejected}},
		{"review_rejected", nil, []ReasonCode{ReasonAcceptanceOutboundAPI}},
	}
	for _, tt := range tests {
		t.Run(tt.status+boolName(tt.reviewed != nil), func(t *testing.T) {
			snapshot := baseSnapshot("outbound", "smtp")
			snapshot.Status, snapshot.ReviewedAt = tt.status, tt.reviewed
			assertReasons(t, Reconstruct(snapshot), tt.want...)
		})
	}
}

func TestReconstructQueueTimestampsAndDirection(t *testing.T) {
	jobID := int64(42)
	jobAt := reconstructBaseTime.Add(3 * time.Minute)
	reviewed := reconstructBaseTime.Add(2 * time.Minute)
	tests := []struct {
		name      string
		direction string
		status    string
		reviewed  *time.Time
		jobAt     *time.Time
		wantAt    *time.Time
	}{
		{"river timestamp", "outbound", "sent", &reviewed, &jobAt, &jobAt},
		{"approved fallback", "outbound", "sent", &reviewed, nil, &reviewed},
		{"created fallback", "outbound", "accepted", nil, nil, &reconstructBaseTime},
		{"no inbound inference", "inbound", "", nil, &jobAt, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseSnapshot(tt.direction, "smtp")
			s.Status, s.ReviewedAt, s.SendJobID, s.JobCreatedAt = tt.status, tt.reviewed, &jobID, tt.jobAt
			got := Reconstruct(s)
			queue := findReason(got, ReasonQueueOutboundSubmission)
			if tt.wantAt == nil {
				if queue != nil {
					t.Fatalf("invented queue: %#v", *queue)
				}
				return
			}
			if queue == nil || !queue.OccurredAt.Equal(*tt.wantAt) || queue.CorrelationIDs["job_id"] != "42" {
				t.Fatalf("queue = %#v, want at %v with job 42", queue, *tt.wantAt)
			}
		})
	}
}

func TestReconstructSubmissionEvidence(t *testing.T) {
	providerAt := reconstructBaseTime.Add(time.Minute)
	s := baseSnapshot("outbound", "smtp")
	s.ProviderAcceptedAt, s.ProviderMessageID = &providerAt, "provider-1"
	got := Reconstruct(s)
	transition := findReason(got, ReasonSubmissionUpstreamAccepted)
	if transition == nil || transition.CorrelationIDs["provider_message_id"] != "provider-1" {
		t.Fatalf("provider submission = %#v", transition)
	}

	loopback := baseSnapshot("outbound", "loopback")
	loopback.DeliveryStatus = "sent"
	assertReasons(t, Reconstruct(loopback), ReasonAcceptanceOutboundAPI, ReasonSubmissionLocalLoopbackAccepted)

	reviewed := reconstructBaseTime.Add(time.Minute)
	loopback.ReviewedAt = &reviewed
	if got := findReason(Reconstruct(loopback), ReasonSubmissionLocalLoopbackAccepted); got == nil || !got.OccurredAt.Equal(reviewed) {
		t.Fatalf("reviewed loopback = %#v", got)
	}

	sentOnly := baseSnapshot("outbound", "smtp")
	sentOnly.DeliveryStatus = "sent"
	if hasStage(Reconstruct(sentOnly), StageSubmission) {
		t.Fatal("sent-only SMTP state fabricated submission")
	}
}

func TestReconstructRecipientMappingsAndIgnoredStatuses(t *testing.T) {
	wants := map[string]ReasonCode{
		"delivered":  ReasonDeliveryRecipientServerAccepted,
		"deferred":   ReasonDeliveryTemporaryDelay,
		"bounced":    ReasonDeliveryUndeterminedBounce,
		"complained": ReasonComplaintRecipientReported,
	}
	for status, want := range wants {
		t.Run(status, func(t *testing.T) {
			s := baseSnapshot("outbound", "smtp")
			s.Recipients = []RecipientSnapshot{{ID: "rcp_1", Address: "a@example.com", Status: status, Detail: "detail", UpdatedAt: reconstructBaseTime.Add(time.Minute)}}
			got := findReason(Reconstruct(s), want)
			if got == nil || got.Recipient != "a@example.com" || got.Evidence["smtp_detail"] != "detail" {
				t.Fatalf("recipient transition = %#v", got)
			}
		})
	}
	for _, status := range []string{"queued", "accepted", "sending", "sent", "failed"} {
		t.Run("ignore "+status, func(t *testing.T) {
			s := baseSnapshot("outbound", "smtp")
			s.Recipients = []RecipientSnapshot{{ID: "rcp_1", Address: "a@example.com", Status: status, UpdatedAt: reconstructBaseTime}}
			if len(Reconstruct(s)) != 1 {
				t.Fatalf("status %q proved an outcome", status)
			}
		})
	}
}

func TestReconstructCausalSuppressionsOnly(t *testing.T) {
	s := baseSnapshot("outbound", "smtp")
	s.Suppressions = []SuppressionSnapshot{
		{ID: "sup_1", Address: "bounce@example.com", Source: "bounce", SourceMessageID: s.MessageID, CreatedAt: reconstructBaseTime.Add(time.Minute)},
		{ID: "sup_2", Address: "complaint@example.com", Source: "complaint", SourceMessageID: s.MessageID, CreatedAt: reconstructBaseTime.Add(2 * time.Minute)},
		{ID: "sup_3", Address: "manual@example.com", Source: "manual", SourceMessageID: s.MessageID, CreatedAt: reconstructBaseTime},
		{ID: "sup_4", Address: "foreign@example.com", Source: "bounce", SourceMessageID: "msg_other", CreatedAt: reconstructBaseTime},
	}
	got := Reconstruct(s)
	if findReason(got, ReasonSuppressionHardBounceApplied) == nil || findReason(got, ReasonSuppressionComplaintApplied) == nil || len(got) != 3 {
		t.Fatalf("suppressions = %#v", got)
	}
}

func TestReconstructMappedRetainedEvents(t *testing.T) {
	tests := []struct {
		typeName string
		data     map[string]any
		want     ReasonCode
	}{
		{"email.received", map[string]any{"message_id": "msg_1"}, ReasonAcceptanceInboundSMTP},
		{"email.sent", map[string]any{"message_id": "msg_1", "method": "smtp", "provider_message_id": "p1"}, ReasonSubmissionUpstreamAccepted},
		{"email.delivered", map[string]any{"message_id": "msg_1", "delivered_to": "a@example.com", "smtp_detail": "250 ok"}, ReasonDeliveryRecipientServerAccepted},
		{"email.bounced", map[string]any{"message_id": "msg_1", "delivered_to": "a@example.com", "bounce_type": "transient", "bounce_sub_type": "MailboxFull"}, ReasonDeliveryTransientBounce},
		{"email.complained", map[string]any{"message_id": "msg_1", "delivered_to": "a@example.com"}, ReasonComplaintRecipientReported},
		{"email.review_requested", map[string]any{"message_id": "msg_1"}, ReasonReviewHoldCreated},
		{"email.review_approved", map[string]any{"message_id": "msg_1", "reason": "human"}, ReasonReviewApproved},
		{"email.review_rejected", map[string]any{"message_id": "msg_1", "reason": "human"}, ReasonReviewRejected},
		{"domain.suppression_added", map[string]any{"message_id": "msg_1", "address": "a@example.com", "source": "bounce"}, ReasonSuppressionHardBounceApplied},
	}
	for _, tt := range tests {
		t.Run(tt.typeName, func(t *testing.T) {
			direction := "outbound"
			if tt.typeName == "email.received" {
				direction = "inbound"
			}
			s := baseSnapshot(direction, "smtp")
			s.Events = []EventSnapshot{eventSnapshot("evt_1", tt.typeName, reconstructBaseTime.Add(time.Minute), tt.data)}
			got := findReason(Reconstruct(s), tt.want)
			if got == nil || got.CorrelationIDs["event_id"] != "evt_1" || got.Evidence["source"] != "webhook_events.envelope" {
				t.Fatalf("event transition = %#v", got)
			}
		})
	}
}

func TestReconstructExcludesScreeningUnknownAndAmbiguousFailureEvents(t *testing.T) {
	for _, eventType := range []string{"email.flagged", "email.blocked", "future.event", "email.failed"} {
		t.Run(eventType, func(t *testing.T) {
			s := baseSnapshot("outbound", "smtp")
			s.Events = []EventSnapshot{eventSnapshot("evt_1", eventType, reconstructBaseTime.Add(time.Minute), map[string]any{"message_id": s.MessageID, "reason": "failed"})}
			if got := Reconstruct(s); len(got) != 1 {
				t.Fatalf("excluded event reconstructed: %#v", got)
			}
		})
	}
	for source, want := range map[string]ReasonCode{"provider": ReasonSubmissionProviderRejected, "local": ReasonSubmissionLocalRetriesExhausted} {
		t.Run(source, func(t *testing.T) {
			s := baseSnapshot("outbound", "smtp")
			s.DeliveryFailureSource = source
			s.Events = []EventSnapshot{eventSnapshot("evt_1", "email.failed", reconstructBaseTime.Add(time.Minute), map[string]any{"message_id": s.MessageID, "reason": "no route", "reason_code": "smtp_rejected"})}
			got := findReason(Reconstruct(s), want)
			if got == nil || got.Evidence["failure_reason"] != "no route" || got.Evidence["failure_code"] != "smtp_rejected" {
				t.Fatalf("failure = %#v", got)
			}
		})
	}
}

func TestReconstructEventPreferredOverState(t *testing.T) {
	s := baseSnapshot("outbound", "smtp")
	s.Recipients = []RecipientSnapshot{{ID: "rcp_1", Address: "a@example.com", Status: "delivered", UpdatedAt: reconstructBaseTime.Add(3 * time.Minute)}}
	eventAt := reconstructBaseTime.Add(time.Minute)
	s.Events = []EventSnapshot{eventSnapshot("evt_1", "email.delivered", eventAt, map[string]any{"message_id": s.MessageID, "delivered_to": "a@example.com", "smtp_detail": "event detail"})}
	got := findReason(Reconstruct(s), ReasonDeliveryRecipientServerAccepted)
	if got == nil || !got.OccurredAt.Equal(eventAt) || got.Evidence["smtp_detail"] != "event detail" {
		t.Fatalf("event did not replace state: %#v", got)
	}
}

func TestReconstructRetainsDistinctLegacyEventObservationsAndDedupesExactSource(t *testing.T) {
	s := baseSnapshot("outbound", "smtp")
	oldAt := reconstructBaseTime.Add(time.Minute)
	newAt := reconstructBaseTime.Add(2 * time.Minute)
	old := eventSnapshot("evt_old", "email.delivered", oldAt, map[string]any{
		"message_id": s.MessageID, "delivered_to": "a@example.com", "provider_event_id": "provider_old",
	})
	newer := eventSnapshot("evt_new", "email.delivered", newAt, map[string]any{
		"message_id": s.MessageID, "delivered_to": "a@example.com", "provider_event_id": "provider_new",
	})
	s.Events = []EventSnapshot{newer, old, old}

	var delivered []MessageLifecycleTransition
	for _, item := range Reconstruct(s) {
		if item.ReasonCode == ReasonDeliveryRecipientServerAccepted {
			delivered = append(delivered, item)
		}
	}
	if len(delivered) != 2 {
		t.Fatalf("delivered observations = %#v, want distinct old/new and one exact old source", delivered)
	}
	if delivered[0].CorrelationIDs["event_id"] != "evt_old" || delivered[1].CorrelationIDs["event_id"] != "evt_new" {
		t.Fatalf("delivery ordering/source identity = %#v", delivered)
	}
}

func TestMergeTransitionsSuppressesOnlyMatchingReconstructedSource(t *testing.T) {
	s := baseSnapshot("outbound", "smtp")
	oldAt := reconstructBaseTime.Add(time.Minute)
	newAt := reconstructBaseTime.Add(2 * time.Minute)
	s.Events = []EventSnapshot{
		eventSnapshot("evt_old", "email.delivered", oldAt, map[string]any{"message_id": s.MessageID, "delivered_to": "a@example.com"}),
		eventSnapshot("evt_new", "email.delivered", newAt, map[string]any{"message_id": s.MessageID, "delivered_to": "a@example.com"}),
	}
	reconstructed := Reconstruct(s)
	persisted := persistedTransition("mlt_persisted_new", ReasonDeliveryRecipientServerAccepted, "a@example.com", newAt)
	persisted.CorrelationIDs["event_id"] = "evt_new"

	got := MergeTransitions([]MessageLifecycleTransition{persisted}, reconstructed)
	var delivered []MessageLifecycleTransition
	for _, item := range got {
		if item.ReasonCode == ReasonDeliveryRecipientServerAccepted {
			delivered = append(delivered, item)
		}
	}
	if len(delivered) != 2 || delivered[0].CorrelationIDs["event_id"] != "evt_old" || delivered[1].ID != persisted.ID {
		t.Fatalf("source-aware merge = %#v", got)
	}
	for i := 1; i < len(got); i++ {
		if transitionLess(got[i], got[i-1]) {
			t.Fatalf("merge ordering changed: %#v", got)
		}
	}
}

func TestMergeTransitionsUsesEmbeddedCanonicalTransitionIdentity(t *testing.T) {
	s := baseSnapshot("outbound", "smtp")
	at := reconstructBaseTime.Add(time.Minute)
	canonical := persistedTransition("mlt_canonical_delivery", ReasonDeliveryRecipientServerAccepted, "a@example.com", at)
	s.Events = []EventSnapshot{eventSnapshot("evt_delivery", "email.delivered", at.Add(time.Second), map[string]any{
		"message_id": s.MessageID, "direction": "outbound", "delivered_to": "a@example.com",
		"lifecycle_transitions": []MessageLifecycleTransition{canonical},
	})}
	reconstructed := Reconstruct(s)
	delivery := findReason(reconstructed, ReasonDeliveryRecipientServerAccepted)
	if delivery == nil || delivery.SourceTransitionID != canonical.ID {
		t.Fatalf("embedded canonical identity = %#v", reconstructed)
	}
	got := MergeTransitions([]MessageLifecycleTransition{canonical}, reconstructed)
	var deliveries int
	for _, item := range got {
		if item.ReasonCode == ReasonDeliveryRecipientServerAccepted {
			deliveries++
		}
	}
	if deliveries != 1 {
		t.Fatalf("embedded canonical transition duplicated: %#v", got)
	}
}

func TestMergeTransitionsMatchesPersistedProviderDedupeIdentity(t *testing.T) {
	s := baseSnapshot("outbound", "smtp")
	oldAt := reconstructBaseTime.Add(time.Minute)
	newAt := reconstructBaseTime.Add(2 * time.Minute)
	s.Events = []EventSnapshot{
		eventSnapshot("evt_old", "email.delivered", oldAt, map[string]any{"message_id": s.MessageID, "delivered_to": "a@example.com", "provider_event_id": "provider_old"}),
		eventSnapshot("evt_new", "email.delivered", newAt, map[string]any{"message_id": s.MessageID, "delivered_to": "a@example.com", "provider_event_id": "provider_new"}),
	}
	persisted := persistedTransition("mlt_persisted_new", ReasonDeliveryRecipientServerAccepted, "a@example.com", newAt)
	persisted.DedupeKey = "provider-feedback:provider_new:a@example.com:delivered"
	persisted.CorrelationIDs["event_id"] = "evt_canonical_publication"

	got := MergeTransitions([]MessageLifecycleTransition{persisted}, Reconstruct(s))
	var delivered []MessageLifecycleTransition
	for _, item := range got {
		if item.ReasonCode == ReasonDeliveryRecipientServerAccepted {
			delivered = append(delivered, item)
		}
	}
	if len(delivered) != 2 || delivered[0].CorrelationIDs["provider_event_id"] != "provider_old" || delivered[1].ID != persisted.ID {
		t.Fatalf("provider-source merge = %#v", got)
	}
}

func TestMergeTransitionsMatchesSubmissionMessageIdentityAliases(t *testing.T) {
	occurredAt := reconstructBaseTime.Add(time.Minute)
	reconstructed := persistedTransition("mlt_recon", ReasonSubmissionLocalLoopbackAccepted, "", occurredAt.Add(time.Second))
	reconstructed.Stage = StageSubmission
	reconstructed.Reconstructed = true
	reconstructed.CorrelationIDs["event_id"] = "evt_submission"
	reconstructed.CorrelationIDs["provider_message_id"] = "<message@example.com>"
	persisted := persistedTransition("mlt_persisted", ReasonSubmissionLocalLoopbackAccepted, "", occurredAt)
	persisted.Stage = StageSubmission
	persisted.CorrelationIDs["email_message_id"] = "<message@example.com>"

	got := MergeTransitions([]MessageLifecycleTransition{persisted}, []MessageLifecycleTransition{reconstructed})
	if len(got) != 1 || got[0].ID != persisted.ID {
		t.Fatalf("submission identity alias merge = %#v", got)
	}
}

func TestMergeTransitionsMatchesAcceptanceMessageIdentity(t *testing.T) {
	occurredAt := reconstructBaseTime.Add(time.Minute)
	reconstructed := persistedTransition("mlt_recon", ReasonAcceptanceLocalLoopback, "", occurredAt.Add(time.Second))
	reconstructed.Stage = StageAccepted
	reconstructed.Reconstructed = true
	reconstructed.CorrelationIDs["event_id"] = "evt_acceptance"
	reconstructed.CorrelationIDs["email_message_id"] = "<message@example.com>"
	persisted := persistedTransition("mlt_persisted", ReasonAcceptanceLocalLoopback, "", occurredAt)
	persisted.Stage = StageAccepted
	persisted.CorrelationIDs["email_message_id"] = "<message@example.com>"

	got := MergeTransitions([]MessageLifecycleTransition{persisted}, []MessageLifecycleTransition{reconstructed})
	if len(got) != 1 || got[0].ID != persisted.ID {
		t.Fatalf("acceptance identity merge = %#v", got)
	}
}

func TestMergeTransitionsMatchesMessageLocalReviewResolution(t *testing.T) {
	persisted := persistedTransition("mlt_persisted", ReasonReviewApproved, "", reconstructBaseTime)
	persisted.Stage = StageReview
	reconstructed := persistedTransition("mlt_recon", ReasonReviewApproved, "", reconstructBaseTime.Add(time.Second))
	reconstructed.Stage = StageReview
	reconstructed.Reconstructed = true
	reconstructed.CorrelationIDs["event_id"] = "evt_review"

	got := MergeTransitions([]MessageLifecycleTransition{persisted}, []MessageLifecycleTransition{reconstructed})
	if len(got) != 1 || got[0].ID != persisted.ID {
		t.Fatalf("message-local review merge = %#v", got)
	}
}

func TestMergeTransitionsFallsBackToTimestampWhenPersistedHasNoSourceIdentity(t *testing.T) {
	occurredAt := reconstructBaseTime.Add(time.Minute)
	reconstructed := persistedTransition("mlt_recon", ReasonAcceptanceOutboundAPI, "", occurredAt)
	reconstructed.Stage = StageAccepted
	reconstructed.Reconstructed = true
	reconstructed.CorrelationIDs["provider_message_id"] = "<message@example.com>"
	persisted := persistedTransition("mlt_persisted", ReasonAcceptanceOutboundAPI, "", occurredAt)
	persisted.Stage = StageAccepted

	got := MergeTransitions([]MessageLifecycleTransition{persisted}, []MessageLifecycleTransition{reconstructed})
	if len(got) != 1 || got[0].ID != persisted.ID {
		t.Fatalf("source-less persisted timestamp merge = %#v", got)
	}
}

func TestMergeTransitionsUsesTimestampFallbackForLegacyObservations(t *testing.T) {
	oldAt := reconstructBaseTime.Add(time.Minute)
	newAt := reconstructBaseTime.Add(2 * time.Minute)
	old := persistedTransition("mlt_recon_old", ReasonDeliveryRecipientServerAccepted, "a@example.com", oldAt)
	old.Reconstructed = true
	newer := persistedTransition("mlt_recon_new", ReasonDeliveryRecipientServerAccepted, "a@example.com", newAt)
	newer.Reconstructed = true
	persisted := persistedTransition("mlt_persisted_new", ReasonDeliveryRecipientServerAccepted, "a@example.com", newAt)

	got := MergeTransitions([]MessageLifecycleTransition{persisted}, []MessageLifecycleTransition{newer, old})
	if len(got) != 2 || got[0].ID != old.ID || got[1].ID != persisted.ID {
		t.Fatalf("timestamp-fallback merge = %#v", got)
	}
}

func TestReconstructRetainsDistinctTerminalFailureEventsForSameProviderMessage(t *testing.T) {
	s := baseSnapshot("outbound", "smtp")
	s.DeliveryFailureSource = "provider"
	s.ProviderMessageID = "provider-message-1"
	oldAt := reconstructBaseTime.Add(time.Minute)
	newAt := reconstructBaseTime.Add(2 * time.Minute)
	s.Events = []EventSnapshot{
		eventSnapshot("evt_failed_old", "email.failed", oldAt, map[string]any{"message_id": s.MessageID, "direction": "outbound", "reason": "old failure"}),
		eventSnapshot("evt_failed_new", "email.failed", newAt, map[string]any{"message_id": s.MessageID, "direction": "outbound", "reason": "new failure"}),
	}
	reconstructed := Reconstruct(s)
	var failures []MessageLifecycleTransition
	for _, item := range reconstructed {
		if item.ReasonCode == ReasonSubmissionProviderRejected {
			failures = append(failures, item)
		}
	}
	if len(failures) != 2 || failures[0].CorrelationIDs["event_id"] != "evt_failed_old" || failures[1].CorrelationIDs["event_id"] != "evt_failed_new" {
		t.Fatalf("terminal failures = %#v", failures)
	}
	persisted := persistedTransition("mlt_failed_new", ReasonSubmissionProviderRejected, "", newAt)
	persisted.CorrelationIDs["event_id"] = "evt_failed_new"
	got := MergeTransitions([]MessageLifecycleTransition{persisted}, reconstructed)
	failures = failures[:0]
	for _, item := range got {
		if item.ReasonCode == ReasonSubmissionProviderRejected {
			failures = append(failures, item)
		}
	}
	if len(failures) != 2 || failures[0].CorrelationIDs["event_id"] != "evt_failed_old" || failures[1].ID != persisted.ID {
		t.Fatalf("terminal failure merge = %#v", got)
	}
}

func TestReconstructExpiredReviewEventUsesDurableResolutionAndEventTimestamp(t *testing.T) {
	reviewedAt := reconstructBaseTime.Add(3 * time.Minute)
	eventAt := reconstructBaseTime.Add(time.Minute)
	s := baseSnapshot("outbound", "smtp")
	s.Status = "review_expired_rejected"
	s.ReviewedAt = &reviewedAt
	s.Events = []EventSnapshot{eventSnapshot("evt_expired", "email.review_rejected", eventAt, map[string]any{
		"message_id": s.MessageID, "reason": "ttl_expired",
	})}
	got := Reconstruct(s)
	transition := findReason(got, ReasonReviewExpiredRejected)
	if transition == nil || !transition.OccurredAt.Equal(eventAt) || transition.CorrelationIDs["event_id"] != "evt_expired" {
		t.Fatalf("expired review did not use event observation: %#v", got)
	}
	if findReason(got, ReasonReviewRejected) != nil {
		t.Fatalf("generic rejection duplicated expired rejection: %#v", got)
	}
}

func TestReconstructRejectsRetainedEventWithMismatchedDirection(t *testing.T) {
	s := baseSnapshot("inbound", "smtp")
	s.Events = []EventSnapshot{eventSnapshot("evt_wrong", "email.sent", reconstructBaseTime.Add(time.Minute), map[string]any{
		"message_id": s.MessageID, "direction": "outbound", "method": "smtp",
	})}
	if got := Reconstruct(s); len(got) != 1 || hasStage(got, StageSubmission) {
		t.Fatalf("mismatched event reconstructed: %#v", got)
	}
}

func TestReconstructDeterministicIDsDeepCopiesNonNilMapsAndOrdersEqualTimes(t *testing.T) {
	s := baseSnapshot("outbound", "smtp")
	s.ProviderAcceptedAt = ptrTime(reconstructBaseTime)
	s.ProviderMessageID = "provider-1"
	first := Reconstruct(s)
	second := Reconstruct(s)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeat reconstruction differs\n%#v\n%#v", first, second)
	}
	for _, item := range first {
		if !strings.HasPrefix(item.ID, "mlt_recon_") || len(item.ID) != len("mlt_recon_")+32 || item.Evidence == nil || item.CorrelationIDs == nil || !item.Reconstructed {
			t.Fatalf("invalid reconstructed item: %#v", item)
		}
	}
	first[0].Evidence["source"] = "mutated"
	first[0].CorrelationIDs["event_id"] = "mutated"
	if reflect.DeepEqual(first, second) {
		t.Fatal("result maps alias across calls")
	}
	ids := []string{second[0].ID, second[1].ID}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("equal timestamp IDs not ordered: %v", ids)
	}
}

func TestMergeTransitionsPersistedWinsAndRetainsRepeatedPersistedObservations(t *testing.T) {
	s := baseSnapshot("outbound", "smtp")
	s.Recipients = []RecipientSnapshot{{ID: "rcp", Address: "a@example.com", Status: "delivered", UpdatedAt: reconstructBaseTime.Add(time.Minute)}}
	reconstructed := Reconstruct(s)
	persisted := []MessageLifecycleTransition{
		persistedTransition("mlt_a", ReasonDeliveryRecipientServerAccepted, "a@example.com", reconstructBaseTime.Add(2*time.Minute)),
		persistedTransition("mlt_b", ReasonDeliveryRecipientServerAccepted, "a@example.com", reconstructBaseTime.Add(3*time.Minute)),
	}
	got := MergeTransitions(persisted, reconstructed)
	if len(got) != 3 || got[0].ReasonCode != ReasonAcceptanceOutboundAPI || got[1].ID != "mlt_a" || got[2].ID != "mlt_b" {
		t.Fatalf("merged = %#v", got)
	}
}

func TestReconstructDoesNotInventIntermediateStages(t *testing.T) {
	s := baseSnapshot("outbound", "smtp")
	s.Recipients = []RecipientSnapshot{{ID: "rcp", Address: "a@example.com", Status: "delivered", UpdatedAt: reconstructBaseTime.Add(time.Minute)}}
	got := Reconstruct(s)
	assertReasons(t, got, ReasonAcceptanceOutboundAPI, ReasonDeliveryRecipientServerAccepted)
	if hasStage(got, StageQueued) || hasStage(got, StageSubmission) {
		t.Fatalf("invented intermediate transition: %#v", got)
	}
}

func TestReconstructInboundDoesNotInventOutboundFacts(t *testing.T) {
	providerAt := reconstructBaseTime.Add(time.Minute)
	reviewedAt := reconstructBaseTime.Add(2 * time.Minute)
	s := baseSnapshot("inbound", "loopback")
	s.ProviderAcceptedAt = &providerAt
	s.ProviderMessageID = "provider-should-be-ignored"
	s.DeliveryStatus = "sent"
	s.ReviewedAt = &reviewedAt
	s.Recipients = []RecipientSnapshot{
		{ID: "rcp_delivered", Address: "delivered@example.com", Status: "delivered", UpdatedAt: reconstructBaseTime.Add(3 * time.Minute)},
		{ID: "rcp_bounced", Address: "bounced@example.com", Status: "bounced", UpdatedAt: reconstructBaseTime.Add(4 * time.Minute)},
		{ID: "rcp_complained", Address: "complained@example.com", Status: "complained", UpdatedAt: reconstructBaseTime.Add(5 * time.Minute)},
	}
	s.Suppressions = []SuppressionSnapshot{
		{ID: "sup_bounce", Address: "bounced@example.com", Source: "bounce", SourceMessageID: s.MessageID, CreatedAt: reconstructBaseTime.Add(6 * time.Minute)},
		{ID: "sup_complaint", Address: "complained@example.com", Source: "complaint", SourceMessageID: s.MessageID, CreatedAt: reconstructBaseTime.Add(7 * time.Minute)},
	}
	got := Reconstruct(s)
	for _, stage := range []Stage{StageSubmission, StageDelivery, StageComplaint, StageSuppression} {
		if hasStage(got, stage) {
			t.Fatalf("inbound message fabricated outbound %s fact: %#v", stage, got)
		}
	}
}

func TestReconstructAuthenticationEvidenceExactAndAbsent(t *testing.T) {
	raw := authenticationJSON("pass")
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	s := baseSnapshot("inbound", "smtp")
	s.Authentication = raw
	auth := findReason(Reconstruct(s), ReasonAuthenticationDMARCPass)
	if auth == nil || !reflect.DeepEqual(auth.Evidence["authentication"], want) {
		t.Fatalf("authentication evidence = %#v, want exact %#v", auth, want)
	}

	for name, absent := range map[string]json.RawMessage{"nil": nil, "null": json.RawMessage("null")} {
		t.Run(name, func(t *testing.T) {
			without := baseSnapshot("inbound", "smtp")
			without.Authentication = absent
			got := Reconstruct(without)
			if hasStage(got, StageAuthentication) || got[0].Evidence["authentication"] != nil {
				t.Fatalf("absent authentication reconstructed evidence: %#v", got)
			}
		})
	}
}

func TestReconstructEventCarriedAuthenticationExact(t *testing.T) {
	raw := authenticationJSON("temperror")
	var authentication map[string]any
	if err := json.Unmarshal(raw, &authentication); err != nil {
		t.Fatal(err)
	}
	s := baseSnapshot("inbound", "smtp")
	s.Events = []EventSnapshot{eventSnapshot("evt_auth", "email.received", reconstructBaseTime.Add(time.Minute), map[string]any{
		"message_id": s.MessageID, "direction": "inbound", "authentication": authentication,
	})}
	got := findReason(Reconstruct(s), ReasonAuthenticationDMARCTemporaryError)
	if got == nil || !reflect.DeepEqual(got.Evidence["authentication"], authentication) || got.CorrelationIDs["event_id"] != "evt_auth" {
		t.Fatalf("event authentication = %#v", got)
	}
}

func TestReconstructAllOutboundRetainedEventVariants(t *testing.T) {
	tests := []struct {
		name, eventType, method, failureSource string
		data                                   map[string]any
		want                                   ReasonCode
	}{
		{"sent smtp", "email.sent", "smtp", "", map[string]any{"method": "smtp"}, ReasonSubmissionUpstreamAccepted},
		{"sent loopback", "email.sent", "loopback", "", map[string]any{"method": "loopback"}, ReasonSubmissionLocalLoopbackAccepted},
		{"failed provider", "email.failed", "smtp", "provider", map[string]any{"reason": "rejected", "reason_code": "smtp_rejected"}, ReasonSubmissionProviderRejected},
		{"failed local", "email.failed", "smtp", "local", map[string]any{"reason": "exhausted", "reason_code": "retries_exhausted"}, ReasonSubmissionLocalRetriesExhausted},
		{"failed external access", "email.failed", "smtp", "local", map[string]any{"reason": "external_sending_not_enabled", "reason_code": "submission.external_sending_not_enabled"}, ReasonSubmissionExternalSendingNotEnabled},
		{"delivered", "email.delivered", "smtp", "", map[string]any{"delivered_to": "a@example.com"}, ReasonDeliveryRecipientServerAccepted},
		{"bounce permanent", "email.bounced", "smtp", "", map[string]any{"delivered_to": "a@example.com", "bounce_type": "permanent"}, ReasonDeliveryPermanentBounce},
		{"bounce transient", "email.bounced", "smtp", "", map[string]any{"delivered_to": "a@example.com", "bounce_type": "transient"}, ReasonDeliveryTransientBounce},
		{"bounce undetermined", "email.bounced", "smtp", "", map[string]any{"delivered_to": "a@example.com", "bounce_type": "undetermined"}, ReasonDeliveryUndeterminedBounce},
		{"complained", "email.complained", "smtp", "", map[string]any{"delivered_to": "a@example.com"}, ReasonComplaintRecipientReported},
		{"suppression bounce", "domain.suppression_added", "smtp", "", map[string]any{"address": "a@example.com", "source": "bounce"}, ReasonSuppressionHardBounceApplied},
		{"suppression complaint", "domain.suppression_added", "smtp", "", map[string]any{"address": "a@example.com", "source": "complaint"}, ReasonSuppressionComplaintApplied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseSnapshot("outbound", tt.method)
			s.DeliveryFailureSource = tt.failureSource
			data := map[string]any{"message_id": s.MessageID, "direction": "outbound"}
			for key, value := range tt.data {
				data[key] = value
			}
			s.Events = []EventSnapshot{eventSnapshot("evt_variant", tt.eventType, reconstructBaseTime.Add(time.Minute), data)}
			if got := findReason(Reconstruct(s), tt.want); got == nil {
				t.Fatalf("missing %s from %#v", tt.want, Reconstruct(s))
			}
		})
	}
}

func TestReconstructMalformedOrUncorrelatedEventsAreOmitted(t *testing.T) {
	validData := map[string]any{"message_id": "msg_1", "direction": "outbound", "delivered_to": "a@example.com"}
	tests := []struct {
		name  string
		event EventSnapshot
	}{
		{"malformed JSON", EventSnapshot{ID: "evt_bad", Type: "email.delivered", Envelope: json.RawMessage(`{"type":`), CreatedAt: reconstructBaseTime}},
		{"wrong envelope type", eventSnapshot("evt_bad", "email.bounced", reconstructBaseTime, validData)},
		{"mismatched envelope ID", eventSnapshot("evt_other", "email.delivered", reconstructBaseTime, validData)},
		{"invalid envelope ID", eventSnapshot("not-an-event-id", "email.delivered", reconstructBaseTime, validData)},
		{"mismatched message ID", eventSnapshot("evt_bad", "email.delivered", reconstructBaseTime, map[string]any{"message_id": "msg_other", "delivered_to": "a@example.com"})},
		{"delivered missing recipient", eventSnapshot("evt_bad", "email.delivered", reconstructBaseTime, map[string]any{"message_id": "msg_1"})},
		{"bounce missing recipient", eventSnapshot("evt_bad", "email.bounced", reconstructBaseTime, map[string]any{"message_id": "msg_1", "bounce_type": "permanent"})},
		{"complaint missing recipient", eventSnapshot("evt_bad", "email.complained", reconstructBaseTime, map[string]any{"message_id": "msg_1"})},
	}
	tests[1].event.Type = "email.delivered"
	tests[2].event.ID = "evt_bad"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseSnapshot("outbound", "smtp")
			s.Events = []EventSnapshot{tt.event}
			if got := Reconstruct(s); len(got) != 1 {
				t.Fatalf("invalid event reconstructed: %#v", got)
			}
		})
	}
}

func TestReconstructInboundSuppressionEventsAreOmitted(t *testing.T) {
	for _, direction := range []any{"inbound", nil} {
		name := "omitted direction"
		data := map[string]any{"message_id": "msg_1", "address": "victim@example.com", "source": "bounce"}
		if direction != nil {
			name = "explicit inbound direction"
			data["direction"] = direction
		}
		t.Run(name, func(t *testing.T) {
			s := baseSnapshot("inbound", "smtp")
			s.Events = []EventSnapshot{eventSnapshot("evt_inbound_suppression", "domain.suppression_added", reconstructBaseTime.Add(time.Minute), data)}
			if got := Reconstruct(s); hasStage(got, StageSuppression) {
				t.Fatalf("inbound event fabricated suppression: %#v", got)
			}
		})
	}
}

func TestReconstructOversizedStateCorrelationsAreOmittedWithoutDroppingFacts(t *testing.T) {
	oversized := strings.Repeat("x", maxDiagnosticStringBytes+1)
	inbound := baseSnapshot("inbound", "smtp")
	inbound.EmailMessageID = oversized
	inbound.Authentication = authenticationJSON("pass")
	got := Reconstruct(inbound)
	assertReasons(t, got, ReasonAcceptanceInboundSMTP, ReasonAuthenticationDMARCPass)
	for _, item := range got {
		if _, exists := item.CorrelationIDs["email_message_id"]; exists {
			t.Fatalf("oversized email_message_id survived: %#v", item)
		}
	}

	providerAt := reconstructBaseTime.Add(time.Minute)
	outbound := baseSnapshot("outbound", "smtp")
	outbound.ProviderAcceptedAt = &providerAt
	outbound.ProviderMessageID = oversized
	got = Reconstruct(outbound)
	assertReasons(t, got, ReasonAcceptanceOutboundAPI, ReasonSubmissionUpstreamAccepted)
	for _, item := range got {
		if _, exists := item.CorrelationIDs["provider_message_id"]; exists {
			t.Fatalf("oversized provider_message_id survived: %#v", item)
		}
	}
}

func TestReconstructOversizedEventCorrelationIsOmittedWithoutDroppingFact(t *testing.T) {
	oversized := strings.Repeat("x", maxDiagnosticStringBytes+1)
	s := baseSnapshot("outbound", "smtp")
	s.Events = []EventSnapshot{eventSnapshot("evt_oversized", "email.sent", reconstructBaseTime.Add(time.Minute), map[string]any{
		"message_id": s.MessageID, "direction": "outbound", "method": "smtp", "provider_message_id": oversized,
	})}
	got := findReason(Reconstruct(s), ReasonSubmissionUpstreamAccepted)
	if got == nil {
		t.Fatalf("proven submission disappeared: %#v", Reconstruct(s))
	}
	if _, exists := got.CorrelationIDs["provider_message_id"]; exists {
		t.Fatalf("oversized event correlation survived: %#v", got)
	}
	if got.CorrelationIDs["event_id"] != "evt_oversized" {
		t.Fatalf("valid event correlation disappeared: %#v", got)
	}
}

func TestFilteredCorrelationCopyOmitsInvalidEmptyAndOversizedEntries(t *testing.T) {
	got := filteredCorrelationCopy(map[string]string{
		"event_id":            "evt_valid",
		"job_id":              "",
		"provider_message_id": strings.Repeat("x", maxDiagnosticStringBytes+1),
		"unknown":             "value",
	})
	want := map[string]string{"event_id": "evt_valid"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered correlations = %#v, want %#v", got, want)
	}
}

func baseSnapshot(direction, method string) Snapshot {
	return Snapshot{MessageID: "msg_1", AgentID: "agt_1", Direction: direction, Method: method, CreatedAt: reconstructBaseTime}
}

func authenticationJSON(status string) json.RawMessage {
	return json.RawMessage(`{"spf":{"status":"none","domain":null,"aligned":null},"dkim":[],"dmarc":{"status":"` + status + `","domain":null,"policy":null,"aligned_by":[]}}`)
}

func eventSnapshot(id, eventType string, at time.Time, data map[string]any) EventSnapshot {
	envelope, _ := json.Marshal(map[string]any{"type": eventType, "id": id, "created_at": at, "data": data})
	return EventSnapshot{ID: id, Type: eventType, Envelope: envelope, CreatedAt: at.Add(time.Hour)}
}

func persistedTransition(id string, reason ReasonCode, recipient string, at time.Time) MessageLifecycleTransition {
	definition, _ := Lookup(reason)
	return MessageLifecycleTransition{ID: id, MessageID: "msg_1", Direction: "outbound", Recipient: recipient, Stage: definition.Stage, Outcome: definition.Outcome, ReasonCode: reason, Retryable: definition.Retryable, Evidence: map[string]any{}, CorrelationIDs: map[string]string{}, OccurredAt: at}
}

func assertReasons(t *testing.T, got []MessageLifecycleTransition, want ...ReasonCode) {
	t.Helper()
	reasons := make([]ReasonCode, len(got))
	for i := range got {
		reasons[i] = got[i].ReasonCode
	}
	sort.Slice(reasons, func(i, j int) bool { return reasons[i] < reasons[j] })
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if !reflect.DeepEqual(reasons, want) {
		t.Fatalf("reasons = %v, want %v", reasons, want)
	}
}

func findReason(items []MessageLifecycleTransition, reason ReasonCode) *MessageLifecycleTransition {
	for i := range items {
		if items[i].ReasonCode == reason {
			return &items[i]
		}
	}
	return nil
}

func hasStage(items []MessageLifecycleTransition, stage Stage) bool {
	for _, item := range items {
		if item.Stage == stage {
			return true
		}
	}
	return false
}

func ptrTime(value time.Time) *time.Time { return &value }
func boolName(value bool) string {
	if value {
		return " with timestamp"
	}
	return " without timestamp"
}
