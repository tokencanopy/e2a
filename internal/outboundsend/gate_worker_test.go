package outboundsend_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// These tests pin the fixed worker order over the sending-protection gate:
// Reserve → rate → suppression → ConsumeAttempt → authorized submit, with
// every hold snoozing without provider I/O, every deferral/cancellation
// returning the right ledger, and every finite hold persisting a class whose
// derived deadline decides expiry and its lifecycle reason.

func isSnooze(err error) bool {
	var snooze *river.JobSnoozeError
	return errors.As(err, &snooze)
}

func isCancel(err error) bool {
	var cancel *river.JobCancelError
	return errors.As(err, &cancel)
}

func TestGatedWorker_AllowedPathAuthorizesThenSubmits(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_1")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-1", SentAs: "relay"}}
	g := allowAll()
	if err := outboundsend.NewSendWorker(st, dl).WithGate(g).Work(context.Background(), gatedJob("msg_1", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if g.reserves != 1 || g.consumes != 1 || dl.calls != 1 || len(st.sent) != 1 {
		t.Fatalf("reserves=%d consumes=%d delivers=%d sent=%d, want 1/1/1/1", g.reserves, g.consumes, dl.calls, len(st.sent))
	}
	if len(g.deferred)+len(g.cancelled) != 0 {
		t.Fatalf("deferred=%v cancelled=%v on an allowed path", g.deferred, g.cancelled)
	}
}

func TestGatedWorker_EarlyHoldSnoozesWithoutProviderIOAndPersistsClass(t *testing.T) {
	for reason, want := range map[string]outboundsend.HoldClass{
		sendingpolicy.ReasonAccountDailyBudget:        outboundsend.HoldPolicyBudget,
		sendingpolicy.ReasonGlobalProbation:           outboundsend.HoldPolicyBudget,
		sendingpolicy.ReasonTenantNotReady:            outboundsend.HoldTenantSetup,
		sendingpolicy.ReasonTenantUnnamed:             outboundsend.HoldTenantSetup,
		sendingpolicy.ReasonRampCapacity:              outboundsend.HoldRateRampOrProvider,
		sendingpolicy.ReasonSendingIdentityUnverified: outboundsend.HoldRateRampOrProvider,
	} {
		j := acceptedJob("msg_hold")
		j.AcceptedAt = time.Now().Add(-time.Hour)
		st := &fakeStore{job: j}
		dl := &fakeDeliverer{}
		g := &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: reason, RetryAt: time.Now().Add(2 * time.Hour)}}
		err := outboundsend.NewSendWorker(st, dl).WithGate(g).Work(context.Background(), gatedJob("msg_hold", 1))
		if !isSnooze(err) {
			t.Fatalf("%s: err = %v, want snooze", reason, err)
		}
		if dl.calls != 0 || len(st.failed) != 0 || g.consumes != 0 {
			t.Fatalf("%s: delivers=%d failed=%d consumes=%d, want no I/O and no terminal", reason, dl.calls, len(st.failed), g.consumes)
		}
		if len(st.holds) != 1 || st.holds[0].class != want {
			t.Fatalf("%s: holds = %+v, want one %s hold", reason, st.holds, want)
		}
		// A first-observed tenant-setup hold starts its clock at the
		// observation; every other class starts at the latest of the
		// message's own timestamps.
		if want == outboundsend.HoldTenantSetup {
			if st.holds[0].anchor.Before(j.AcceptedAt.Add(time.Hour - time.Minute)) {
				t.Fatalf("%s: anchor = %v, want the observation time, not accept", reason, st.holds[0].anchor)
			}
		} else if !st.holds[0].anchor.Equal(j.AcceptedAt) {
			t.Fatalf("%s: anchor = %v, want accept %v", reason, st.holds[0].anchor, j.AcceptedAt)
		}
		if len(st.released) != 1 {
			t.Fatalf("%s: claim releases = %v, want one", reason, st.released)
		}
	}
}

func TestGatedWorker_PauseHoldIsIndefiniteAndPersistsNothing(t *testing.T) {
	j := acceptedJob("msg_paused")
	j.AcceptedAt = time.Now().Add(-30 * 24 * time.Hour) // far past every finite horizon
	st := &fakeStore{job: j}
	g := &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonAccountPaused}}
	err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(g).Work(context.Background(), gatedJob("msg_paused", 1))
	if !isSnooze(err) {
		t.Fatalf("err = %v, want snooze — a pause waits for an operator", err)
	}
	if len(st.holds) != 0 || len(st.failed) != 0 {
		t.Fatalf("holds=%+v failed=%+v, want neither for a pause", st.holds, st.failed)
	}
}

func TestGatedWorker_PauseNeverEvaluatesADeadlineButNeverExtendsIt(t *testing.T) {
	// Paused with a budget deadline already eight days gone: the paused job
	// only waits. Nothing is failed, nothing rewritten.
	j := acceptedJob("msg_paused_budget")
	j.LocalHoldClass, j.LocalHoldAnchor = outboundsend.HoldPolicyBudget, time.Now().Add(-8*24*time.Hour)
	st := &fakeStore{job: j}
	g := &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonAccountPaused}}
	if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(g).Work(context.Background(), gatedJob("msg_paused_budget", 1)); !isSnooze(err) {
		t.Fatalf("paused err = %v, want snooze — a paused job evaluates no deadline", err)
	}
	if len(st.failed) != 0 || len(st.holds) != 0 {
		t.Fatalf("failed=%+v holds=%+v, want nothing touched while paused", st.failed, st.holds)
	}
	// After resume the first hold it meets applies the unextended deadline.
	g = &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonGlobalAllBudget, RetryAt: time.Now().Add(time.Hour)}}
	err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(g).Work(context.Background(), gatedJob("msg_paused_budget", 2))
	if !isCancel(err) || len(st.failed) != 1 || st.failed[0].reason != messagelifecycle.ReasonSubmissionPolicyBudgetExpired {
		t.Fatalf("after resume err=%v failed=%+v, want the budget deadline to fire with its own reason", err, st.failed)
	}
}

func TestGatedWorker_BudgetHoldPromotesAnyClassAndKeepsTheAnchor(t *testing.T) {
	anchor := time.Now().Add(-2 * time.Hour)
	for _, existing := range []outboundsend.HoldClass{outboundsend.HoldRateRampOrProvider, outboundsend.HoldTenantSetup} {
		j := acceptedJob("msg_promote")
		j.LocalHoldClass, j.LocalHoldAnchor = existing, anchor
		st := &fakeStore{job: j}
		g := &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonGlobalAllBudget, RetryAt: time.Now().Add(time.Hour)}}
		if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(g).Work(context.Background(), gatedJob("msg_promote", 1)); !isSnooze(err) {
			t.Fatalf("%s: err = %v, want snooze", existing, err)
		}
		if len(st.holds) != 1 || st.holds[0].class != outboundsend.HoldPolicyBudget || !st.holds[0].anchor.Equal(anchor) {
			t.Fatalf("%s: holds = %+v, want promotion to policy_budget with the anchor kept", existing, st.holds)
		}
	}
	// And policy_budget never changes again, even under a later setup hold.
	j := acceptedJob("msg_sticky")
	j.LocalHoldClass, j.LocalHoldAnchor = outboundsend.HoldPolicyBudget, anchor
	st := &fakeStore{job: j}
	g := &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonTenantNotReady}}
	if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(g).Work(context.Background(), gatedJob("msg_sticky", 1)); !isSnooze(err) {
		t.Fatalf("err = %v, want snooze", err)
	}
	if len(st.holds) != 0 {
		t.Fatalf("holds = %+v, want no rewrite of a policy_budget hold", st.holds)
	}
}

func TestGatedWorker_ExpiryReasonFollowsTheClass(t *testing.T) {
	for _, tc := range []struct {
		class  outboundsend.HoldClass
		age    time.Duration
		reason messagelifecycle.ReasonCode
		hold   string
	}{
		{outboundsend.HoldPolicyBudget, 7*24*time.Hour + time.Minute, messagelifecycle.ReasonSubmissionPolicyBudgetExpired, sendingpolicy.ReasonGlobalAllBudget},
		{outboundsend.HoldTenantSetup, 72*time.Hour + time.Minute, messagelifecycle.ReasonSubmissionSendingSetupExpired, sendingpolicy.ReasonTenantNotReady},
		{outboundsend.HoldRateRampOrProvider, 72*time.Hour + time.Minute, messagelifecycle.ReasonSubmissionLocalRetriesExhausted, sendingpolicy.ReasonRampCapacity},
	} {
		j := acceptedJob("msg_expire")
		j.LocalHoldClass, j.LocalHoldAnchor = tc.class, time.Now().Add(-tc.age)
		st := &fakeStore{job: j}
		g := &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: tc.hold, RetryAt: time.Now().Add(time.Hour)}}
		err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(g).Work(context.Background(), gatedJob("msg_expire", 1))
		if !isCancel(err) {
			t.Fatalf("%s: err = %v, want cancel", tc.class, err)
		}
		if len(st.failed) != 1 || st.failed[0].reason != tc.reason || st.failed[0].source != delivery.FailureSourceLocal {
			t.Fatalf("%s: failed = %+v, want one local failure with reason %s", tc.class, st.failed, tc.reason)
		}
		if len(g.cancelled) != 1 {
			t.Fatalf("%s: cancelled = %v, want the attempt given back", tc.class, g.cancelled)
		}
	}
	// One minute short of the deadline still snoozes.
	j := acceptedJob("msg_almost")
	j.LocalHoldClass, j.LocalHoldAnchor = outboundsend.HoldPolicyBudget, time.Now().Add(-7*24*time.Hour+time.Minute)
	st := &fakeStore{job: j}
	g := &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonGlobalAllBudget, RetryAt: time.Now().Add(time.Hour)}}
	if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(g).Work(context.Background(), gatedJob("msg_almost", 1)); !isSnooze(err) {
		t.Fatalf("err = %v, want snooze one minute before the deadline", err)
	}
}

func TestGatedWorker_TerminalHoldCancelsNow(t *testing.T) {
	for _, reason := range []string{sendingpolicy.ReasonAccountDeleted, sendingpolicy.ReasonClassChanged, sendingpolicy.ReasonRampUnavailable} {
		st := &fakeStore{job: acceptedJob("msg_terminal")}
		g := &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: reason, Terminal: true}}
		err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(g).Work(context.Background(), gatedJob("msg_terminal", 1))
		if !isCancel(err) {
			t.Fatalf("%s: err = %v, want cancel", reason, err)
		}
		if len(st.failed) != 1 || st.failed[0].reason != messagelifecycle.ReasonSubmissionCancelled {
			t.Fatalf("%s: failed = %+v, want one local cancellation", reason, st.failed)
		}
	}
}

func TestGatedWorker_RateDeferralDefersTheAttempt(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_rate")}
	g := allowAll()
	gate := &fakeRateGate{decision: outboundsend.RateDecision{Allowed: false, RetryAt: time.Now().Add(30 * time.Second)}, window: time.Minute}
	err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(g).WithRateGate(gate).Work(context.Background(), gatedJob("msg_rate", 1))
	if !isSnooze(err) {
		t.Fatalf("err = %v, want snooze", err)
	}
	if len(g.deferred) != 1 || g.consumes != 0 {
		t.Fatalf("deferred=%v consumes=%d, want the attempt deferred before final authorization", g.deferred, g.consumes)
	}
	if len(st.holds) != 1 || st.holds[0].class != outboundsend.HoldRateRampOrProvider {
		t.Fatalf("holds = %+v, want a rate/ramp/provider hold", st.holds)
	}
}

func TestGatedWorker_SuppressionCancelsTheAttempt(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_sup"), suppressed: []string{"b@y.com"}}
	g := allowAll()
	dl := &fakeDeliverer{}
	err := outboundsend.NewSendWorker(st, dl).WithGate(g).Work(context.Background(), gatedJob("msg_sup", 1))
	if !isCancel(err) || dl.calls != 0 {
		t.Fatalf("err=%v delivers=%d, want cancel with no I/O", err, dl.calls)
	}
	if len(g.cancelled) != 1 || g.consumes != 0 {
		t.Fatalf("cancelled=%v consumes=%d, want the attempt cancelled before final authorization", g.cancelled, g.consumes)
	}
}

func TestGatedWorker_FinalAuthorizationHoldSnoozesWithoutProviderIO(t *testing.T) {
	j := acceptedJob("msg_late_hold")
	j.AcceptedAt = time.Now().Add(-time.Hour)
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{}
	g := &fakeGate{reserve: sendingpolicy.Decision{Allow: true}, consume: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonAccountSharedBudget, RetryAt: time.Now().Add(time.Hour)}}
	err := outboundsend.NewSendWorker(st, dl).WithGate(g).Work(context.Background(), gatedJob("msg_late_hold", 1))
	if !isSnooze(err) || dl.calls != 0 {
		t.Fatalf("err=%v delivers=%d, want snooze with no I/O", err, dl.calls)
	}
	if len(st.holds) != 1 || st.holds[0].class != outboundsend.HoldPolicyBudget {
		t.Fatalf("holds = %+v, want a policy_budget hold from the late gate", st.holds)
	}
}

func TestGatedWorker_GateOutageSnoozesWithoutBurningAnAttempt(t *testing.T) {
	for name, g := range map[string]*fakeGate{
		"reserve":   {reserveErr: errors.New("policy db down")},
		"authorize": {reserve: sendingpolicy.Decision{Allow: true}, consumeErr: errors.New("policy db down")},
	} {
		st := &fakeStore{job: acceptedJob("msg_gate_down")}
		dl := &fakeDeliverer{}
		err := outboundsend.NewSendWorker(st, dl).WithGate(g).Work(context.Background(), gatedJob("msg_gate_down", 1))
		if !isSnooze(err) || dl.calls != 0 || len(st.failed) != 0 {
			t.Fatalf("%s: err=%v delivers=%d failed=%d, want snooze, no I/O, no terminal", name, err, dl.calls, len(st.failed))
		}
		if len(st.released) != 1 {
			t.Fatalf("%s: claim releases = %v, want one", name, st.released)
		}
	}
}

func TestGatedWorker_ProviderEvidenceSettlesTheOperation(t *testing.T) {
	j := acceptedJob("msg_evidence")
	j.ProviderAccepted, j.ProviderMessageID = true, "ses-evidence"
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{}
	g := allowAll()
	if err := outboundsend.NewSendWorker(st, dl).WithGate(g).Work(context.Background(), gatedJob("msg_evidence", 2)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if dl.calls != 0 || len(st.sent) != 1 || g.reserves != 0 {
		t.Fatalf("delivers=%d sent=%d reserves=%d, want settle without resubmit or a new reservation", dl.calls, len(st.sent), g.reserves)
	}
	if g.lookupCalls != 1 || len(g.settled) != 1 || g.settled[0] != sendingpolicy.SettlementProviderAccepted {
		t.Fatalf("lookups=%d settled=%v, want the operation settled as accepted", g.lookupCalls, g.settled)
	}
}

func TestGatedWorker_LegacyJobResolvesThroughTheAcceptPath(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_legacy")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-legacy"}}
	g := allowAll()
	resolved := 0
	w := outboundsend.NewSendWorker(st, dl).WithGate(g).WithOperationResolver(func(_ context.Context, id string) (sendingpolicy.AcceptanceDecision, sendingpolicy.OperationRef, error) {
		resolved++
		return sendingpolicy.AcceptanceAccept, refFor(id), nil
	})
	if err := w.Work(context.Background(), job("msg_legacy", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if resolved != 1 || g.reserves != 1 || dl.calls != 1 {
		t.Fatalf("resolved=%d reserves=%d delivers=%d, want the legacy job authorized like a new one", resolved, g.reserves, dl.calls)
	}

	// A paused account at resolution holds; an orphan source cancels; no
	// resolver at all fails closed.
	st = &fakeStore{job: acceptedJob("msg_legacy_paused")}
	w = outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(allowAll()).WithOperationResolver(func(context.Context, string) (sendingpolicy.AcceptanceDecision, sendingpolicy.OperationRef, error) {
		return sendingpolicy.AcceptanceSendingPaused, sendingpolicy.OperationRef{}, nil
	})
	if err := w.Work(context.Background(), job("msg_legacy_paused", 1)); !isSnooze(err) {
		t.Fatalf("paused legacy: err = %v, want snooze", err)
	}
	st = &fakeStore{job: acceptedJob("msg_legacy_orphan")}
	w = outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(allowAll()).WithOperationResolver(func(context.Context, string) (sendingpolicy.AcceptanceDecision, sendingpolicy.OperationRef, error) {
		return "", sendingpolicy.OperationRef{}, sendingpolicy.ErrSourceUnavailable
	})
	if err := w.Work(context.Background(), job("msg_legacy_orphan", 1)); !isCancel(err) || len(st.failed) != 1 {
		t.Fatalf("orphan legacy: err=%v failed=%d, want cancel with one local failure", err, len(st.failed))
	}
	st = &fakeStore{job: acceptedJob("msg_legacy_unwired")}
	dl = &fakeDeliverer{}
	if err := outboundsend.NewSendWorker(st, dl).WithGate(allowAll()).Work(context.Background(), job("msg_legacy_unwired", 1)); !isCancel(err) || dl.calls != 0 {
		t.Fatalf("unwired resolver: err=%v delivers=%d, want cancel with no I/O", err, dl.calls)
	}
}

func TestGatedWorker_TenantReadinessMovesSetupHoldToRateClassOnce(t *testing.T) {
	anchor := time.Now().Add(-70 * time.Hour)
	ready := anchor.Add(60 * time.Hour) // inside the 72h setup deadline
	j := acceptedJob("msg_ready")
	j.LocalHoldClass, j.LocalHoldAnchor, j.TenantReadyAt = outboundsend.HoldTenantSetup, anchor, ready
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "ses-ready"}}
	if err := outboundsend.NewSendWorker(st, dl).WithGate(allowAll()).Work(context.Background(), gatedJob("msg_ready", 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(st.holds) != 1 || st.holds[0].class != outboundsend.HoldRateRampOrProvider || !st.holds[0].anchor.Equal(ready) {
		t.Fatalf("holds = %+v, want the one-way move to rate_ramp_or_provider anchored at readiness", st.holds)
	}

	// Readiness that landed AFTER the setup deadline does not rescue the
	// message: it expires as setup on its next hold.
	late := acceptedJob("msg_late_ready")
	late.LocalHoldClass, late.LocalHoldAnchor, late.TenantReadyAt = outboundsend.HoldTenantSetup, time.Now().Add(-80*time.Hour), time.Now().Add(-time.Hour)
	st = &fakeStore{job: late}
	g := &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonRampCapacity, RetryAt: time.Now().Add(time.Hour)}}
	err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(g).Work(context.Background(), gatedJob("msg_late_ready", 1))
	if !isCancel(err) || len(st.failed) != 1 || st.failed[0].reason != messagelifecycle.ReasonSubmissionSendingSetupExpired {
		t.Fatalf("late readiness: err=%v failed=%+v, want setup expiry", err, st.failed)
	}
}

func TestGatedWorker_FirstHoldAnchorsAtTheLatestOfAcceptScheduleReviewResume(t *testing.T) {
	base := time.Now().Add(-10 * 24 * time.Hour)
	j := acceptedJob("msg_anchor")
	j.AcceptedAt = base
	j.ScheduledAt = base.Add(24 * time.Hour)
	j.ReviewedAt = base.Add(48 * time.Hour)
	j.LastResumedAt = base.Add(9*24*time.Hour + 23*time.Hour) // an hour ago: the latest
	st := &fakeStore{job: j}
	g := &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonRampCapacity, RetryAt: time.Now().Add(time.Hour)}}
	if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(g).Work(context.Background(), gatedJob("msg_anchor", 1)); !isSnooze(err) {
		t.Fatalf("err = %v, want snooze — a ten-day-old accept is not the clock, the resume an hour ago is", err)
	}
	if len(st.holds) != 1 || !st.holds[0].anchor.Equal(j.LastResumedAt) {
		t.Fatalf("holds = %+v, want anchored at the last resume", st.holds)
	}
}

func TestGatedWorker_AcceptanceUnknownIsRetriedAsANewOrdinalNotSettled(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_unknown")}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("data final: acceptance unknown"), AcceptanceUnknown: true}}
	g := allowAll()
	err := outboundsend.NewSendWorker(st, dl).WithGate(g).Work(context.Background(), gatedJob("msg_unknown", 1))
	if err == nil || isSnooze(err) || isCancel(err) {
		t.Fatalf("err = %v, want a plain retryable error (River's next attempt returns to Reserve)", err)
	}
	if len(st.temporary) != 1 || len(st.failed) != 0 || len(g.settled) != 0 {
		t.Fatalf("temporary=%d failed=%d settled=%v, want a temporary record and nothing settled", len(st.temporary), len(st.failed), g.settled)
	}
}

func TestGatedWorker_HoldConstantsMatchThePolicyDefault(t *testing.T) {
	if got := time.Duration(sendingpolicy.DisabledPolicy().BudgetHoldMaxDays) * 24 * time.Hour; got != outboundsend.PolicyBudgetHoldHorizon {
		t.Fatalf("PolicyBudgetHoldHorizon = %s, policy budget_hold_max_days default = %s", outboundsend.PolicyBudgetHoldHorizon, got)
	}
}

func TestGatedWorker_GateOutageIsBoundedByTheHoldDeadline(t *testing.T) {
	j := acceptedJob("msg_gate_down_long")
	j.AcceptedAt = time.Now().Add(-73 * time.Hour)
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{}
	g := &fakeGate{reserveErr: errors.New("policy db down")}
	err := outboundsend.NewSendWorker(st, dl).WithGate(g).Work(context.Background(), gatedJob("msg_gate_down_long", 1))
	if !isCancel(err) || dl.calls != 0 {
		t.Fatalf("err=%v delivers=%d, want the 72-hour expiry with no I/O", err, dl.calls)
	}
	if len(st.failed) != 1 || st.failed[0].reason != messagelifecycle.ReasonSubmissionLocalRetriesExhausted {
		t.Fatalf("failed = %+v, want local_retries_exhausted", st.failed)
	}
	// Inside the horizon it holds as rate/ramp/provider and snoozes.
	j = acceptedJob("msg_gate_down_short")
	j.AcceptedAt = time.Now().Add(-time.Hour)
	st = &fakeStore{job: j}
	if err := outboundsend.NewSendWorker(st, dl).WithGate(g).Work(context.Background(), gatedJob("msg_gate_down_short", 1)); !isSnooze(err) {
		t.Fatalf("err = %v, want snooze", err)
	}
	if len(st.holds) != 1 || st.holds[0].class != outboundsend.HoldRateRampOrProvider {
		t.Fatalf("holds = %+v, want a rate/ramp/provider hold", st.holds)
	}
}

func TestGatedWorker_ReadinessLossDoesNotReplaceARateClass(t *testing.T) {
	anchor := time.Now().Add(-time.Hour)
	j := acceptedJob("msg_keep_rate")
	j.LocalHoldClass, j.LocalHoldAnchor = outboundsend.HoldRateRampOrProvider, anchor
	st := &fakeStore{job: j}
	g := &fakeGate{reserve: sendingpolicy.Decision{Allow: false, Reason: sendingpolicy.ReasonTenantNotReady}}
	if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(g).Work(context.Background(), gatedJob("msg_keep_rate", 1)); !isSnooze(err) {
		t.Fatalf("err = %v, want snooze", err)
	}
	if len(st.holds) != 0 {
		t.Fatalf("holds = %+v, want the persisted rate class left alone", st.holds)
	}
}

func TestGatedWorker_ProviderOutagePersistsTheHoldAndHonorsTheBudgetClock(t *testing.T) {
	// First outage: enters the rate/ramp/provider class anchored at accept.
	j := acceptedJob("msg_outage")
	j.AcceptedAt = time.Now().Add(-time.Hour)
	st := &fakeStore{job: j}
	dl := &fakeDeliverer{out: outboundsend.DeliverOutcome{Err: errors.New("connection refused"), Outage: true}}
	if err := outboundsend.NewSendWorker(st, dl).WithGate(allowAll()).Work(context.Background(), gatedJob("msg_outage", 1)); !isSnooze(err) {
		t.Fatalf("err = %v, want snooze", err)
	}
	if len(st.holds) != 1 || st.holds[0].class != outboundsend.HoldRateRampOrProvider || !st.holds[0].anchor.Equal(j.AcceptedAt) {
		t.Fatalf("holds = %+v, want rate/ramp/provider anchored at accept", st.holds)
	}
	// Under a policy_budget hold four days old, an outage keeps waiting on
	// the seven-day clock instead of the 72-hour one.
	j = acceptedJob("msg_outage_budget")
	j.AcceptedAt = time.Now().Add(-5 * 24 * time.Hour)
	j.LocalHoldClass, j.LocalHoldAnchor = outboundsend.HoldPolicyBudget, time.Now().Add(-4*24*time.Hour)
	st = &fakeStore{job: j}
	if err := outboundsend.NewSendWorker(st, dl).WithGate(allowAll()).Work(context.Background(), gatedJob("msg_outage_budget", 1)); !isSnooze(err) {
		t.Fatalf("budget-held outage err = %v, want snooze on the seven-day clock", err)
	}
	if len(st.holds) != 0 || len(st.failed) != 0 {
		t.Fatalf("holds=%+v failed=%+v, want the budget class untouched", st.holds, st.failed)
	}
	// An outage that expires a tenant_setup class never emits the setup
	// reason: setup was not what blocked the send at the end.
	j = acceptedJob("msg_outage_setup")
	j.LocalHoldClass, j.LocalHoldAnchor = outboundsend.HoldTenantSetup, time.Now().Add(-73*time.Hour)
	st = &fakeStore{job: j}
	err := outboundsend.NewSendWorker(st, dl).WithGate(allowAll()).Work(context.Background(), gatedJob("msg_outage_setup", 1))
	if err == nil || len(st.failed) != 1 || st.failed[0].reason != messagelifecycle.ReasonSubmissionLocalRetriesExhausted {
		t.Fatalf("err=%v failed=%+v, want local_retries_exhausted, never sending_setup_expired", err, st.failed)
	}
}

func TestGatedWorker_EvidenceSettleUnderATerminalWriteSettlesTheOperation(t *testing.T) {
	// A suppression arrives for a message whose earlier attempt dialed and
	// whose provider evidence has since landed: the guarded terminal write
	// settles the row as SENT, and the dialed attempt must be settled too.
	st := &fakeStore{job: acceptedJob("msg_late_evidence"), suppressed: []string{"b@y.com"}, settleStatus: delivery.StatusSent}
	g := allowAll()
	if err := outboundsend.NewSendWorker(st, &fakeDeliverer{}).WithGate(g).Work(context.Background(), gatedJob("msg_late_evidence", 2)); !isCancel(err) {
		t.Fatalf("err = %v, want cancel", err)
	}
	if g.lookupCalls != 1 || len(g.settled) != 1 || g.settled[0] != sendingpolicy.SettlementProviderAccepted {
		t.Fatalf("lookups=%d settled=%v, want the operation settled as accepted from the evidence", g.lookupCalls, g.settled)
	}
}

func TestHoldClassForNamesEveryReasonExplicitly(t *testing.T) {
	// Every hold reason the gate can emit decides a horizon; the mapping is
	// by name, and an unknown name takes the shorter clock.
	cases := map[string]outboundsend.HoldClass{
		sendingpolicy.ReasonAccountPaused:             "",
		sendingpolicy.ReasonAccountDailyBudget:        outboundsend.HoldPolicyBudget,
		sendingpolicy.ReasonAccountSharedBudget:       outboundsend.HoldPolicyBudget,
		sendingpolicy.ReasonGlobalAllBudget:           outboundsend.HoldPolicyBudget,
		sendingpolicy.ReasonGlobalProbation:           outboundsend.HoldPolicyBudget,
		sendingpolicy.ReasonGlobalCritical:            outboundsend.HoldPolicyBudget,
		sendingpolicy.ReasonGlobalViolation:           outboundsend.HoldPolicyBudget,
		sendingpolicy.ReasonTenantNotReady:            outboundsend.HoldTenantSetup,
		sendingpolicy.ReasonTenantUnnamed:             outboundsend.HoldTenantSetup,
		sendingpolicy.ReasonRampCapacity:              outboundsend.HoldRateRampOrProvider,
		sendingpolicy.ReasonSendingIdentityUnverified: outboundsend.HoldRateRampOrProvider,
		"some_future_budget_exhausted":                outboundsend.HoldRateRampOrProvider,
	}
	for reason, want := range cases {
		if got := outboundsend.HoldClassFor(reason); got != want {
			t.Errorf("HoldClassFor(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestGatedWorker_OperationReferenceMustNameThisMessage(t *testing.T) {
	st := &fakeStore{job: acceptedJob("msg_a")}
	dl := &fakeDeliverer{}
	g := allowAll()
	rj := job("msg_a", 1)
	other := refFor("msg_b")
	rj.Args.OperationRef = &other
	err := outboundsend.NewSendWorker(st, dl).WithGate(g).Work(context.Background(), rj)
	if !isCancel(err) || dl.calls != 0 || g.reserves != 0 {
		t.Fatalf("err=%v delivers=%d reserves=%d, want cancel before any ledger call", err, dl.calls, g.reserves)
	}
	if len(st.failed) != 1 || st.failed[0].reason != messagelifecycle.ReasonSubmissionCancelled {
		t.Fatalf("failed = %+v, want one local cancellation", st.failed)
	}
}
