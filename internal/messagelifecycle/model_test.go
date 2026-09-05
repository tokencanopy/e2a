package messagelifecycle

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tokencanopy/e2a/internal/emailauth"
)

func TestCatalogIsExhaustive(t *testing.T) {
	tests := []struct {
		reason    ReasonCode
		stage     Stage
		outcome   Outcome
		retryable bool
	}{
		{ReasonAcceptanceInboundSMTP, StageAccepted, OutcomeAccepted, false},
		{ReasonAcceptanceOutboundAPI, StageAccepted, OutcomeAccepted, false},
		{ReasonAcceptanceLocalLoopback, StageAccepted, OutcomeAccepted, false},
		{ReasonAuthenticationDMARCPass, StageAuthentication, OutcomePassed, false},
		{ReasonAuthenticationDMARCFail, StageAuthentication, OutcomeFailed, false},
		{ReasonAuthenticationDMARCNone, StageAuthentication, OutcomeIndeterminate, false},
		{ReasonAuthenticationDMARCTemporaryError, StageAuthentication, OutcomeIndeterminate, true},
		{ReasonAuthenticationDMARCPermanentError, StageAuthentication, OutcomeIndeterminate, false},
		{ReasonReviewHoldCreated, StageReview, OutcomePending, false},
		{ReasonReviewApproved, StageReview, OutcomeApproved, false},
		{ReasonReviewRejected, StageReview, OutcomeRejected, false},
		{ReasonReviewExpiredApproved, StageReview, OutcomeApproved, false},
		{ReasonReviewExpiredRejected, StageReview, OutcomeRejected, false},
		{ReasonSuppressionRecipientBlocked, StageSuppression, OutcomeBlocked, false},
		{ReasonSuppressionHardBounceApplied, StageSuppression, OutcomeApplied, false},
		{ReasonSuppressionComplaintApplied, StageSuppression, OutcomeApplied, false},
		{ReasonQueueInboundProcessing, StageQueued, OutcomeEnqueued, false},
		{ReasonQueueOutboundSubmission, StageQueued, OutcomeEnqueued, false},
		{ReasonSubmissionUpstreamAccepted, StageSubmission, OutcomeAccepted, false},
		{ReasonSubmissionLocalLoopbackAccepted, StageSubmission, OutcomeAccepted, false},
		{ReasonSubmissionTemporaryFailure, StageSubmission, OutcomeDeferred, true},
		{ReasonSubmissionProviderRejected, StageSubmission, OutcomeFailed, false},
		{ReasonSubmissionLocalRetriesExhausted, StageSubmission, OutcomeFailed, true},
		{ReasonSubmissionCancelled, StageSubmission, OutcomeFailed, false},
		{ReasonSubmissionPolicyBudgetExpired, StageSubmission, OutcomeFailed, true},
		{ReasonSubmissionSendingSetupExpired, StageSubmission, OutcomeFailed, true},
		{ReasonDeliveryRecipientServerAccepted, StageDelivery, OutcomeDelivered, false},
		{ReasonDeliveryTemporaryDelay, StageDelivery, OutcomeDeferred, true},
		{ReasonDeliveryPermanentBounce, StageDelivery, OutcomeBounced, false},
		{ReasonDeliveryTransientBounce, StageDelivery, OutcomeBounced, true},
		{ReasonDeliveryUndeterminedBounce, StageDelivery, OutcomeBounced, false},
		{ReasonComplaintRecipientReported, StageComplaint, OutcomeReported, false},
	}

	catalog := Catalog()
	if got, want := len(catalog), 32; got != want {
		t.Fatalf("Catalog() length = %d, want %d", got, want)
	}
	seen := make(map[ReasonCode]bool, len(tests))
	for _, tt := range tests {
		if seen[tt.reason] {
			t.Fatalf("reason %q appears more than once in test catalog", tt.reason)
		}
		seen[tt.reason] = true
		got, ok := Lookup(tt.reason)
		if !ok {
			t.Errorf("Lookup(%q) was not found", tt.reason)
			continue
		}
		want := Definition{Stage: tt.stage, Outcome: tt.outcome, Retryable: tt.retryable}
		if got != want {
			t.Errorf("Lookup(%q) = %+v, want %+v", tt.reason, got, want)
		}
		if catalog[tt.reason] != want {
			t.Errorf("Catalog()[%q] = %+v, want %+v", tt.reason, catalog[tt.reason], want)
		}
	}
	for reason := range catalog {
		if !seen[reason] {
			t.Errorf("Catalog() contains unexpected reason %q", reason)
		}
	}
}

func TestCatalogRejectsUnknownAndCannotBeMutated(t *testing.T) {
	if _, ok := Lookup(ReasonCode("provider.free_form")); ok {
		t.Fatal("Lookup accepted an unknown provider reason")
	}

	copyOne := Catalog()
	copyOne[ReasonAcceptanceInboundSMTP] = Definition{Stage: StageComplaint, Outcome: OutcomeReported, Retryable: true}
	delete(copyOne, ReasonAcceptanceOutboundAPI)

	got, ok := Lookup(ReasonAcceptanceInboundSMTP)
	if !ok || got != (Definition{Stage: StageAccepted, Outcome: OutcomeAccepted}) {
		t.Fatalf("caller mutation changed canonical lookup: %+v, %v", got, ok)
	}
	if got := len(Catalog()); got != 32 {
		t.Fatalf("caller mutation changed canonical catalog length to %d", got)
	}
}

func TestCatalogProducerMappings(t *testing.T) {
	authTests := []struct {
		status string
		want   ReasonCode
	}{
		{"pass", ReasonAuthenticationDMARCPass},
		{"fail", ReasonAuthenticationDMARCFail},
		{"none", ReasonAuthenticationDMARCNone},
		{"temperror", ReasonAuthenticationDMARCTemporaryError},
		{"permerror", ReasonAuthenticationDMARCPermanentError},
	}
	for _, tt := range authTests {
		got, err := AuthenticationReason(tt.status)
		if err != nil || got != tt.want {
			t.Errorf("AuthenticationReason(%q) = %q, %v; want %q, nil", tt.status, got, err, tt.want)
		}
	}
	if _, err := AuthenticationReason("unknown"); err == nil {
		t.Fatal("AuthenticationReason accepted an unknown DMARC status")
	}

	bounceTests := []struct {
		bounceType string
		want       ReasonCode
	}{
		{"permanent", ReasonDeliveryPermanentBounce},
		{"transient", ReasonDeliveryTransientBounce},
		{"undetermined", ReasonDeliveryUndeterminedBounce},
		{"provider-new-value", ReasonDeliveryUndeterminedBounce},
	}
	for _, tt := range bounceTests {
		if got := BounceReason(tt.bounceType); got != tt.want {
			t.Errorf("BounceReason(%q) = %q, want %q", tt.bounceType, got, tt.want)
		}
	}
}

func TestNewTransitionDerivesCanonicalFieldsAndCopiesInput(t *testing.T) {
	when := time.Date(2026, 7, 21, 12, 30, 0, 123, time.FixedZone("test", -7*60*60))
	authentication := map[string]any{
		"spf":   map[string]any{"status": "pass", "domain": "example.com", "aligned": true, "detail": "aligned"},
		"dkim":  []any{map[string]any{"status": "pass", "domain": "example.com", "selector": "s1", "aligned": true, "detail": "valid"}},
		"dmarc": map[string]any{"status": "pass", "domain": "example.com", "policy": "reject", "aligned_by": []any{"spf", "dkim"}},
	}
	evidence := map[string]any{"authentication": authentication, "source": "smtp"}
	correlations := map[string]string{"event_id": "evt_123", "email_message_id": "<mail@example.com>"}

	got, err := NewTransition(AppendInput{
		MessageID:      "msg_123",
		DedupeKey:      "acceptance",
		Direction:      "inbound",
		ReasonCode:     ReasonAuthenticationDMARCPass,
		Evidence:       evidence,
		CorrelationIDs: correlations,
		OccurredAt:     when,
	})
	if err != nil {
		t.Fatalf("NewTransition() error = %v", err)
	}
	if got.ID != "" {
		t.Errorf("unpersisted transition ID = %q, want empty", got.ID)
	}
	if got.Stage != StageAuthentication || got.Outcome != OutcomePassed || got.Retryable {
		t.Errorf("derived tuple = %q/%q/%v", got.Stage, got.Outcome, got.Retryable)
	}
	if got.MessageID != "msg_123" || got.Direction != "inbound" || got.ReasonCode != ReasonAuthenticationDMARCPass {
		t.Errorf("transition identity fields = %+v", got)
	}
	if got.Reconstructed {
		t.Error("new transition was marked reconstructed")
	}
	if got.OccurredAt.Location() != time.UTC || !got.OccurredAt.Equal(when) {
		t.Errorf("occurred_at = %v (%v), want same instant in UTC", got.OccurredAt, got.OccurredAt.Location())
	}

	authentication["dmarc"].(map[string]any)["status"] = "fail"
	evidence["source"] = "mutated"
	correlations["event_id"] = "evt_mutated"
	gotAuth := got.Evidence["authentication"].(map[string]any)
	if gotAuth["dmarc"].(map[string]any)["status"] != "pass" || got.Evidence["source"] != "smtp" {
		t.Fatalf("evidence was not deep-copied: %#v", got.Evidence)
	}
	if got.CorrelationIDs["event_id"] != "evt_123" {
		t.Fatalf("correlation IDs were not copied: %#v", got.CorrelationIDs)
	}
}

func TestNewTransitionNormalizesNilMaps(t *testing.T) {
	got, err := NewTransition(validAppendInput())
	if err != nil {
		t.Fatalf("NewTransition() error = %v", err)
	}
	if got.Evidence == nil || got.CorrelationIDs == nil {
		t.Fatalf("nil maps were not normalized: evidence=%#v correlations=%#v", got.Evidence, got.CorrelationIDs)
	}
}

func TestNewTransitionRejectsInvalidRequiredFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AppendInput)
	}{
		{"missing message ID", func(in *AppendInput) { in.MessageID = "" }},
		{"blank message ID", func(in *AppendInput) { in.MessageID = "  " }},
		{"missing dedupe key", func(in *AppendInput) { in.DedupeKey = "" }},
		{"blank dedupe key", func(in *AppendInput) { in.DedupeKey = "\t" }},
		{"unknown direction", func(in *AppendInput) { in.Direction = "sideways" }},
		{"missing direction", func(in *AppendInput) { in.Direction = "" }},
		{"unknown reason", func(in *AppendInput) { in.ReasonCode = "provider.free_form" }},
		{"missing reason", func(in *AppendInput) { in.ReasonCode = "" }},
		{"zero timestamp", func(in *AppendInput) { in.OccurredAt = time.Time{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validAppendInput()
			tt.mutate(&in)
			if _, err := NewTransition(in); err == nil {
				t.Fatal("NewTransition() succeeded, want validation error")
			}
		})
	}
}

func TestNewTransitionEnforcesRecipientRules(t *testing.T) {
	required := map[ReasonCode]struct{}{
		ReasonSuppressionRecipientBlocked:     {},
		ReasonSuppressionHardBounceApplied:    {},
		ReasonSuppressionComplaintApplied:     {},
		ReasonDeliveryRecipientServerAccepted: {},
		ReasonDeliveryTemporaryDelay:          {},
		ReasonDeliveryPermanentBounce:         {},
		ReasonDeliveryTransientBounce:         {},
		ReasonDeliveryUndeterminedBounce:      {},
		ReasonComplaintRecipientReported:      {},
	}
	for reason := range required {
		t.Run(string(reason)+" requires recipient", func(t *testing.T) {
			in := validAppendInput()
			in.ReasonCode = reason
			if _, err := NewTransition(in); err == nil {
				t.Fatal("NewTransition() accepted missing recipient")
			}
			in.Recipient = "person@example.com"
			if _, err := NewTransition(in); err != nil {
				t.Fatalf("NewTransition() rejected recipient: %v", err)
			}
		})
	}

	for reason := range Catalog() {
		if _, recipientRequired := required[reason]; recipientRequired {
			continue
		}
		t.Run(string(reason)+" forbids recipient", func(t *testing.T) {
			in := validAppendInput()
			in.ReasonCode = reason
			in.Recipient = "person@example.com"
			if _, err := NewTransition(in); err == nil {
				t.Fatal("NewTransition() accepted forbidden recipient")
			}
		})
	}
}

func TestEvidenceRejectsForbiddenUnknownAndInvalidValues(t *testing.T) {
	for _, key := range []string{"body", "raw_mime", "headers", "credentials", "secret", "webhook_secret", "provider_response", "unknown"} {
		t.Run(key, func(t *testing.T) {
			in := validAppendInput()
			in.Evidence = map[string]any{key: "unsafe"}
			if _, err := NewTransition(in); err == nil {
				t.Fatal("NewTransition() accepted forbidden or unknown evidence key")
			}
		})
	}

	for name, evidence := range map[string]map[string]any{
		"non-string diagnostic":  {"smtp_detail": true},
		"unsupported JSON value": {"authentication": map[string]any{"bad": func() {}}},
	} {
		t.Run(name, func(t *testing.T) {
			in := validAppendInput()
			in.Evidence = evidence
			if _, err := NewTransition(in); err == nil {
				t.Fatal("NewTransition() accepted invalid evidence value")
			}
		})
	}
}

func TestEvidenceRejectsUnknownAuthenticationKeys(t *testing.T) {
	tests := map[string]map[string]any{
		"root credentials":  authenticationEvidence(map[string]any{"credentials": "secret"}, nil, nil, nil),
		"SPF raw MIME":      authenticationEvidence(nil, map[string]any{"raw_mime": "unsafe"}, nil, nil),
		"DKIM headers":      authenticationEvidence(nil, nil, map[string]any{"headers": "unsafe"}, nil),
		"DMARC credentials": authenticationEvidence(nil, nil, nil, map[string]any{"credentials": "secret"}),
	}
	for name, evidence := range tests {
		t.Run(name, func(t *testing.T) {
			in := validAppendInput()
			in.Evidence = evidence
			if _, err := NewTransition(in); err == nil {
				t.Fatal("NewTransition() accepted unknown authentication key")
			}
		})
	}
}

func TestEvidenceRejectsInvalidAuthenticationStructure(t *testing.T) {
	valid := validAuthenticationMap()
	tests := map[string]any{
		"authentication string": "not structured",
		"missing SPF":           map[string]any{"dkim": valid["dkim"], "dmarc": valid["dmarc"]},
		"missing DKIM":          map[string]any{"spf": valid["spf"], "dmarc": valid["dmarc"]},
		"missing DMARC":         map[string]any{"spf": valid["spf"], "dkim": valid["dkim"]},
		"SPF array":             map[string]any{"spf": []any{}, "dkim": valid["dkim"], "dmarc": valid["dmarc"]},
		"DKIM object":           map[string]any{"spf": valid["spf"], "dkim": map[string]any{}, "dmarc": valid["dmarc"]},
		"DKIM string item":      map[string]any{"spf": valid["spf"], "dkim": []any{"bad"}, "dmarc": valid["dmarc"]},
		"DMARC array":           map[string]any{"spf": valid["spf"], "dkim": valid["dkim"], "dmarc": []any{}},
		"wrong SPF field type":  map[string]any{"spf": map[string]any{"aligned": "yes"}, "dkim": valid["dkim"], "dmarc": valid["dmarc"]},
	}
	for name, authentication := range tests {
		t.Run(name, func(t *testing.T) {
			in := validAppendInput()
			in.Evidence = map[string]any{"authentication": authentication}
			if _, err := NewTransition(in); err == nil {
				t.Fatal("NewTransition() accepted invalid authentication structure")
			}
		})
	}
}

func TestEvidenceRejectsIncompleteOrInvalidAuthenticationValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing SPF status", func(auth map[string]any) { delete(auth["spf"].(map[string]any), "status") }},
		{"missing SPF domain", func(auth map[string]any) { delete(auth["spf"].(map[string]any), "domain") }},
		{"missing SPF aligned", func(auth map[string]any) { delete(auth["spf"].(map[string]any), "aligned") }},
		{"missing DKIM status", func(auth map[string]any) { delete(auth["dkim"].([]any)[0].(map[string]any), "status") }},
		{"missing DKIM domain", func(auth map[string]any) { delete(auth["dkim"].([]any)[0].(map[string]any), "domain") }},
		{"missing DKIM selector", func(auth map[string]any) { delete(auth["dkim"].([]any)[0].(map[string]any), "selector") }},
		{"missing DKIM aligned", func(auth map[string]any) { delete(auth["dkim"].([]any)[0].(map[string]any), "aligned") }},
		{"missing DMARC status", func(auth map[string]any) { delete(auth["dmarc"].(map[string]any), "status") }},
		{"missing DMARC domain", func(auth map[string]any) { delete(auth["dmarc"].(map[string]any), "domain") }},
		{"missing DMARC policy", func(auth map[string]any) { delete(auth["dmarc"].(map[string]any), "policy") }},
		{"missing DMARC aligned_by", func(auth map[string]any) { delete(auth["dmarc"].(map[string]any), "aligned_by") }},
		{"invalid SPF status", func(auth map[string]any) { auth["spf"].(map[string]any)["status"] = "policy" }},
		{"invalid DKIM status", func(auth map[string]any) { auth["dkim"].([]any)[0].(map[string]any)["status"] = "softfail" }},
		{"invalid DMARC status", func(auth map[string]any) { auth["dmarc"].(map[string]any)["status"] = "neutral" }},
		{"invalid DMARC policy", func(auth map[string]any) { auth["dmarc"].(map[string]any)["policy"] = "drop" }},
		{"null DKIM", func(auth map[string]any) { auth["dkim"] = nil }},
		{"null aligned_by", func(auth map[string]any) { auth["dmarc"].(map[string]any)["aligned_by"] = nil }},
		{"unknown aligned_by", func(auth map[string]any) { auth["dmarc"].(map[string]any)["aligned_by"] = []any{"arc"} }},
		{"duplicate aligned_by", func(auth map[string]any) { auth["dmarc"].(map[string]any)["aligned_by"] = []any{"spf", "spf"} }},
		{"null detail", func(auth map[string]any) { auth["spf"].(map[string]any)["detail"] = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authentication := validAuthenticationMap()
			tt.mutate(authentication)
			in := validAppendInput()
			in.Evidence = map[string]any{"authentication": authentication}
			if _, err := NewTransition(in); err == nil {
				t.Fatal("NewTransition() accepted incomplete or invalid authentication")
			}
		})
	}
}

func TestEvidenceEnforcesStringAndSerializedLimits(t *testing.T) {
	tooLong := strings.Repeat("x", 2*1024+1)
	for name, evidence := range map[string]map[string]any{
		"top-level string":             {"smtp_detail": tooLong},
		"nested authentication string": authenticationEvidence(nil, nil, nil, map[string]any{"detail": tooLong}),
	} {
		t.Run(name, func(t *testing.T) {
			in := validAppendInput()
			in.Evidence = evidence
			if _, err := NewTransition(in); err == nil {
				t.Fatal("NewTransition() accepted string over 2 KiB")
			}
		})
	}

	t.Run("exactly 2 KiB strings", func(t *testing.T) {
		limit := strings.Repeat("x", 2*1024)
		for name, evidence := range map[string]map[string]any{
			"top-level": {"smtp_detail": limit},
			"nested":    authenticationEvidence(nil, nil, nil, map[string]any{"detail": limit}),
		} {
			t.Run(name, func(t *testing.T) {
				in := validAppendInput()
				in.Evidence = evidence
				if _, err := NewTransition(in); err != nil {
					t.Fatalf("NewTransition() rejected string at 2 KiB: %v", err)
				}
			})
		}
	})

	t.Run("serialized evidence boundary", func(t *testing.T) {
		exact := evidenceWithSerializedSize(t, 16*1024)
		in := validAppendInput()
		in.Evidence = exact
		if _, err := NewTransition(in); err != nil {
			t.Fatalf("NewTransition() rejected evidence at 16 KiB: %v", err)
		}

		over := evidenceWithSerializedSize(t, 16*1024+1)
		in.Evidence = over
		if _, err := NewTransition(in); err == nil {
			t.Fatal("NewTransition() accepted serialized evidence at 16 KiB+1")
		}
	})
}

func TestEvidenceAcceptsConcreteAndMappedAuthentication(t *testing.T) {
	domain := "example.com"
	selector := "s1"
	aligned := true
	policy := emailauth.DMARCPolicyReject
	concrete := emailauth.Authentication{
		SPF:   emailauth.SPFResult{Status: emailauth.StatusPass, Domain: &domain, Aligned: &aligned, Detail: "aligned"},
		DKIM:  []emailauth.DKIMResult{{Status: emailauth.StatusPass, Domain: &domain, Selector: &selector, Aligned: &aligned, Detail: "valid"}},
		DMARC: emailauth.DMARCResult{Status: emailauth.StatusPass, Domain: &domain, Policy: &policy, AlignedBy: []emailauth.AlignmentMechanism{emailauth.AlignedBySPF, emailauth.AlignedByDKIM}, Detail: "passed"},
	}
	for name, authentication := range map[string]any{
		"struct":  concrete,
		"pointer": &concrete,
		"map":     validAuthenticationMap(),
		"nullable map": map[string]any{
			"spf":   map[string]any{"status": "none", "domain": nil, "aligned": nil},
			"dkim":  []any{map[string]any{"status": "none", "domain": nil, "selector": nil, "aligned": nil}},
			"dmarc": map[string]any{"status": "none", "domain": nil, "policy": nil, "aligned_by": []any{}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := validAppendInput()
			in.Evidence = map[string]any{"authentication": authentication, "smtp_detail": "250 2.0.0 accepted"}
			if _, err := NewTransition(in); err != nil {
				t.Fatalf("NewTransition() rejected valid full authentication: %v", err)
			}
		})
	}
}

func TestEvidenceValidatesCorrelationIDs(t *testing.T) {
	allowed := []string{"event_id", "job_id", "provider_message_id", "provider_event_id", "email_message_id"}
	for _, key := range allowed {
		in := validAppendInput()
		in.CorrelationIDs = map[string]string{key: "value"}
		if _, err := NewTransition(in); err != nil {
			t.Errorf("NewTransition() rejected correlation key %q: %v", key, err)
		}
	}

	in := validAppendInput()
	in.CorrelationIDs = map[string]string{"request_id": "value"}
	if _, err := NewTransition(in); err == nil {
		t.Fatal("NewTransition() accepted unknown correlation key")
	}
	in = validAppendInput()
	in.CorrelationIDs = map[string]string{"event_id": strings.Repeat("x", 2*1024+1)}
	if _, err := NewTransition(in); err == nil {
		t.Fatal("NewTransition() accepted correlation value over 2 KiB")
	}

	inputCorrelations := map[string]string{
		"event_id": "",
		"job_id":   strings.Repeat("x", 2*1024),
	}
	in = validAppendInput()
	in.CorrelationIDs = inputCorrelations
	got, err := NewTransition(in)
	if err != nil {
		t.Fatalf("NewTransition() rejected correlation boundary: %v", err)
	}
	if _, ok := got.CorrelationIDs["event_id"]; ok {
		t.Fatal("NewTransition() retained empty correlation value")
	}
	inputCorrelations["job_id"] = "mutated"
	if got.CorrelationIDs["job_id"] != strings.Repeat("x", 2*1024) {
		t.Fatal("mutating input changed copied correlation value")
	}
}

func TestNewTransitionSchemaEnumTags(t *testing.T) {
	typ := reflect.TypeOf(MessageLifecycleTransition{})
	assertTag := func(field, key, want string) {
		t.Helper()
		f, ok := typ.FieldByName(field)
		if !ok {
			t.Fatalf("field %s not found", field)
		}
		if got := f.Tag.Get(key); got != want {
			t.Errorf("%s %s tag = %q, want %q", field, key, got, want)
		}
	}
	assertTag("Direction", "enum", "inbound,outbound")
	assertTag("Stage", "enum", "accepted,authentication,review,suppression,queued,submission,delivery,complaint")
	assertTag("Outcome", "enum", "accepted,passed,failed,indeterminate,pending,approved,rejected,blocked,applied,enqueued,deferred,delivered,bounced,reported")
	assertTag("ReasonCode", "enum", "acceptance.inbound_smtp,acceptance.outbound_api,acceptance.local_loopback,authentication.dmarc_pass,authentication.dmarc_fail,authentication.dmarc_none,authentication.dmarc_temporary_error,authentication.dmarc_permanent_error,review.hold_created,review.approved,review.rejected,review.expired_approved,review.expired_rejected,suppression.recipient_blocked,suppression.hard_bounce_applied,suppression.complaint_applied,queue.inbound_processing,queue.outbound_submission,submission.upstream_accepted,submission.local_loopback_accepted,submission.temporary_failure,submission.provider_rejected,submission.local_retries_exhausted,submission.cancelled,submission.policy_budget_expired,submission.sending_setup_expired,delivery.recipient_server_accepted,delivery.temporary_delay,delivery.permanent_bounce,delivery.transient_bounce,delivery.undetermined_bounce,complaint.recipient_reported")
}

func validAppendInput() AppendInput {
	return AppendInput{
		MessageID:  "msg_123",
		DedupeKey:  "acceptance",
		Direction:  "outbound",
		ReasonCode: ReasonAcceptanceOutboundAPI,
		OccurredAt: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
	}
}

func validAuthenticationMap() map[string]any {
	return map[string]any{
		"spf": map[string]any{
			"status": "pass", "domain": "example.com", "aligned": true, "detail": "aligned",
		},
		"dkim": []any{map[string]any{
			"status": "pass", "domain": "example.com", "selector": "s1", "aligned": true, "detail": "valid",
		}},
		"dmarc": map[string]any{
			"status": "pass", "domain": "example.com", "policy": "reject", "aligned_by": []any{"spf", "dkim"}, "detail": "passed",
		},
	}
}

func authenticationEvidence(root, spf, dkim, dmarc map[string]any) map[string]any {
	authentication := validAuthenticationMap()
	for key, value := range root {
		authentication[key] = value
	}
	for key, value := range spf {
		authentication["spf"].(map[string]any)[key] = value
	}
	for key, value := range dkim {
		authentication["dkim"].([]any)[0].(map[string]any)[key] = value
	}
	for key, value := range dmarc {
		authentication["dmarc"].(map[string]any)[key] = value
	}
	return map[string]any{"authentication": authentication}
}

func evidenceWithSerializedSize(t *testing.T, target int) map[string]any {
	t.Helper()
	limit := strings.Repeat("x", 2*1024)
	evidence := map[string]any{
		"smtp_detail": limit, "bounce_type": limit, "bounce_sub_type": limit,
		"failure_reason": limit, "failure_code": limit, "review_resolution": limit,
		"suppression_scope": limit, "source": "",
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatalf("marshal boundary evidence: %v", err)
	}
	remainder := target - len(encoded)
	if remainder < 0 || remainder > 2*1024 {
		t.Fatalf("cannot construct %d-byte evidence: remainder %d", target, remainder)
	}
	evidence["source"] = strings.Repeat("y", remainder)
	encoded, err = json.Marshal(evidence)
	if err != nil {
		t.Fatalf("marshal sized evidence: %v", err)
	}
	if len(encoded) != target {
		t.Fatalf("serialized evidence size = %d, want %d", len(encoded), target)
	}
	return evidence
}

func TestSafeAuthenticationEvidencePreservesInBoundsAuthentication(t *testing.T) {
	authentication := validAuthenticationMap()
	got, err := SafeAuthenticationEvidence(authentication)
	if err != nil {
		t.Fatalf("SafeAuthenticationEvidence: %v", err)
	}
	want := map[string]any{"authentication": authentication}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("in-bounds authentication changed\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestSafeAuthenticationEvidenceBoundsRemoteDiagnosticsAndAggregate(t *testing.T) {
	tooLong := strings.Repeat("x", maxDiagnosticStringBytes+1)
	largeButIndividualSafe := strings.Repeat("d", maxDiagnosticStringBytes)
	dkim := []any{map[string]any{
		"status": "fail", "domain": tooLong, "selector": tooLong,
		"aligned": nil, "detail": tooLong,
	}}
	for i := 0; i < 32; i++ {
		dkim = append(dkim, map[string]any{
			"status": "fail", "domain": "example.com", "selector": "selector",
			"aligned": nil, "detail": largeButIndividualSafe,
		})
	}
	authentication := map[string]any{
		"spf": map[string]any{
			"status": "fail", "domain": tooLong, "aligned": nil, "detail": tooLong,
		},
		"dkim": dkim,
		"dmarc": map[string]any{
			"status": "temperror", "domain": tooLong, "policy": nil,
			"aligned_by": []any{}, "detail": tooLong,
		},
	}

	first, err := SafeAuthenticationEvidence(authentication)
	if err != nil {
		t.Fatalf("SafeAuthenticationEvidence: %v", err)
	}
	second, err := SafeAuthenticationEvidence(authentication)
	if err != nil {
		t.Fatalf("SafeAuthenticationEvidence repeat: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("sanitization is not deterministic\nfirst:  %#v\nsecond: %#v", first, second)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal sanitized evidence: %v", err)
	}
	if len(encoded) > maxEvidenceBytes {
		t.Fatalf("sanitized evidence size = %d, exceeds %d", len(encoded), maxEvidenceBytes)
	}
	if _, err := validatedEvidenceCopy(first); err != nil {
		t.Fatalf("sanitized evidence does not validate: %v", err)
	}
	sanitized := first["authentication"].(map[string]any)
	spf := sanitized["spf"].(map[string]any)
	if spf["domain"] != nil {
		t.Fatalf("oversized SPF domain retained: %#v", spf["domain"])
	}
	if _, ok := spf["detail"]; ok {
		t.Fatal("oversized SPF detail retained")
	}
	dmarc := sanitized["dmarc"].(map[string]any)
	if dmarc["status"] != "temperror" {
		t.Fatalf("DMARC verdict changed: %#v", dmarc["status"])
	}
	if dmarc["domain"] != nil {
		t.Fatalf("oversized DMARC domain retained: %#v", dmarc["domain"])
	}
	sanitizedDKIM := sanitized["dkim"].([]any)
	if len(sanitizedDKIM) >= len(dkim) {
		t.Fatalf("aggregate bound did not drop excess DKIM entries: got %d, original %d", len(sanitizedDKIM), len(dkim))
	}
	firstDKIM := sanitizedDKIM[0].(map[string]any)
	if firstDKIM["domain"] != nil || firstDKIM["selector"] != nil {
		t.Fatalf("oversized DKIM identifiers retained: %#v", firstDKIM)
	}
	if _, ok := firstDKIM["detail"]; ok {
		t.Fatal("oversized DKIM detail retained")
	}
}

func TestSafeCorrelationIDsOmitsUnsafeValuesIndependently(t *testing.T) {
	got := SafeCorrelationIDs(map[string]string{
		"job_id":           "42",
		"email_message_id": strings.Repeat("x", maxDiagnosticStringBytes+1),
		"unknown":          "drop-me",
	})
	want := map[string]string{"job_id": "42"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SafeCorrelationIDs() = %#v, want %#v", got, want)
	}
}

func TestSafeDiagnosticEvidencePreservesUTF8AndBoundsBytes(t *testing.T) {
	input := strings.Repeat("é", 2000)
	got := SafeDiagnostic(input)
	if !utf8.ValidString(got) {
		t.Fatal("SafeDiagnostic returned invalid UTF-8")
	}
	if len(got) > maxDiagnosticStringBytes {
		t.Fatalf("diagnostic bytes=%d", len(got))
	}
	if got == input {
		t.Fatal("oversized diagnostic was not bounded")
	}
}
