package senderidentity

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

// reclaimNow is the fixed "current time" every table case is judged against,
// so a case's created/expires stamps read as explicit offsets from it rather
// than as wall-clock-relative values that drift as the suite runs.
var reclaimNow = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

// baseReclaimConfig is an ARMED, fully-configured policy. Every refusal case
// below mutates exactly one thing away from a qualifying candidate, so a test
// that starts passing for the wrong reason (a guard being dropped) fails here
// rather than silently widening what the reaper may delete.
func baseReclaimConfig() ReclaimConfig {
	return ReclaimConfig{
		Enabled:     true,
		Deployment:  DeploymentStaging,
		Zones:       []string{"trymnexa.com", "agents-staging.e2a.dev"},
		MinAge:      7 * 24 * time.Hour,
		MaxPerSweep: 5,
	}
}

// qualifyingAudit is an orphan that satisfies EVERY tag-side guard: managed by
// e2a, stamped for this deployment, a fixture, expired, old enough, and not
// verified for sending.
func qualifyingAudit() IdentityAudit {
	return IdentityAudit{
		Domain: "fixture-abc.trymnexa.com",
		Tags: map[string]string{
			managedIdentityTagKey: managedIdentityTagValue,
			envTagKey:             DeploymentStaging,
			purposeTagKey:         purposeFixture,
			createdTagKey:         reclaimNow.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
			expiresTagKey:         reclaimNow.Add(-29 * 24 * time.Hour).Format(time.RFC3339),
		},
		VerifiedForSending: false,
	}
}

func withTag(a IdentityAudit, key, value string) IdentityAudit {
	tags := make(map[string]string, len(a.Tags))
	for k, v := range a.Tags {
		tags[k] = v
	}
	if value == "" {
		delete(tags, key)
	} else {
		tags[key] = value
	}
	a.Tags = tags
	return a
}

func TestOrphanReclaimable(t *testing.T) {
	cases := []struct {
		name       string
		audit      IdentityAudit
		mutate     func(ReclaimConfig) ReclaimConfig
		wantOK     bool
		wantReason string // substring; empty means "any reason" for a refusal
	}{
		{
			name:   "every guard satisfied",
			audit:  qualifyingAudit(),
			wantOK: true,
		},
		{
			name:       "empty zone list reclaims nothing",
			audit:      qualifyingAudit(),
			mutate:     func(c ReclaimConfig) ReclaimConfig { c.Zones = nil; return c },
			wantReason: "no reclaim zone",
		},
		{
			name:       "unnamed deployment reclaims nothing",
			audit:      qualifyingAudit(),
			mutate:     func(c ReclaimConfig) ReclaimConfig { c.Deployment = ""; return c },
			wantReason: "deployment is unnamed",
		},
		{
			name:       "no minimum age configured",
			audit:      qualifyingAudit(),
			mutate:     func(c ReclaimConfig) ReclaimConfig { c.MinAge = 0; return c },
			wantReason: "minimum age",
		},
		{
			name:       "identity is verified for sending",
			audit:      func() IdentityAudit { a := qualifyingAudit(); a.VerifiedForSending = true; return a }(),
			wantReason: "verified for sending",
		},
		{
			name:       "ownership tag missing (untagged legacy identity)",
			audit:      withTag(qualifyingAudit(), managedIdentityTagKey, ""),
			wantReason: "not tagged as managed by e2a",
		},
		{
			name:       "ownership tag carries a foreign value",
			audit:      withTag(qualifyingAudit(), managedIdentityTagKey, "someone-elses-v1"),
			wantReason: "not tagged as managed by e2a",
		},
		{
			name:       "env tag names a different deployment",
			audit:      withTag(qualifyingAudit(), envTagKey, DeploymentProd),
			wantReason: "belongs to deployment",
		},
		{
			name:       "env tag missing",
			audit:      withTag(qualifyingAudit(), envTagKey, ""),
			wantReason: "belongs to deployment",
		},
		{
			name:       "purpose is customer",
			audit:      withTag(qualifyingAudit(), purposeTagKey, purposeCustomer),
			wantReason: "not a fixture",
		},
		{
			name:       "purpose tag missing",
			audit:      withTag(qualifyingAudit(), purposeTagKey, ""),
			wantReason: "not a fixture",
		},
		{
			name:       "purpose tag carries an unrecognized value",
			audit:      withTag(qualifyingAudit(), purposeTagKey, "probe"),
			wantReason: "not a fixture",
		},
		{
			name:       "expires tag missing",
			audit:      withTag(qualifyingAudit(), expiresTagKey, ""),
			wantReason: "no " + expiresTagKey,
		},
		{
			name:       "expires tag unparseable",
			audit:      withTag(qualifyingAudit(), expiresTagKey, "next tuesday"),
			wantReason: "not RFC3339",
		},
		{
			name:       "expiry is in the future",
			audit:      withTag(qualifyingAudit(), expiresTagKey, reclaimNow.Add(time.Hour).Format(time.RFC3339)),
			wantReason: "has not expired",
		},
		{
			name:       "created tag missing",
			audit:      withTag(qualifyingAudit(), createdTagKey, ""),
			wantReason: "no " + createdTagKey,
		},
		{
			name:       "created tag unparseable",
			audit:      withTag(qualifyingAudit(), createdTagKey, "2026/08/01"),
			wantReason: "not RFC3339",
		},
		{
			name: "younger than the minimum age despite an expired TTL",
			// Guards 5 and 6 are independent on purpose: a bad TTL (or clock
			// skew at provision time) can stamp an already-past expiry onto a
			// brand-new identity, and the age floor is what stops that from
			// making it instantly reclaimable.
			audit: withTag(
				withTag(qualifyingAudit(), createdTagKey, reclaimNow.Add(-time.Hour).Format(time.RFC3339)),
				expiresTagKey, reclaimNow.Add(-time.Minute).Format(time.RFC3339),
			),
			wantReason: "younger than the",
		},
		{
			name:       "created stamp is in the future",
			audit:      withTag(qualifyingAudit(), createdTagKey, reclaimNow.Add(24*time.Hour).Format(time.RFC3339)),
			wantReason: "younger than the",
		},
		{
			name:       "name outside every configured zone",
			audit:      func() IdentityAudit { a := qualifyingAudit(); a.Domain = "customer.example"; return a }(),
			wantReason: "outside every configured reclaim zone",
		},
		{
			name: "suffix near-miss must not match a zone",
			// The whole point of matching on a label boundary: an attacker (or
			// a typo) registering eviltrymnexa.com must not inherit
			// trymnexa.com's reclaim authority.
			audit:      func() IdentityAudit { a := qualifyingAudit(); a.Domain = "eviltrymnexa.com"; return a }(),
			wantReason: "outside every configured reclaim zone",
		},
		{
			name:   "the zone apex itself matches",
			audit:  func() IdentityAudit { a := qualifyingAudit(); a.Domain = "trymnexa.com"; return a }(),
			wantOK: true,
		},
		{
			name:   "a deeper subdomain of a zone matches",
			audit:  func() IdentityAudit { a := qualifyingAudit(); a.Domain = "a.b.trymnexa.com"; return a }(),
			wantOK: true,
		},
		{
			name:       "empty identity name matches nothing",
			audit:      func() IdentityAudit { a := qualifyingAudit(); a.Domain = ""; return a }(),
			wantReason: "outside every configured reclaim zone",
		},
		{
			name:       "an empty configured zone must not match everything",
			audit:      func() IdentityAudit { a := qualifyingAudit(); a.Domain = "customer.example"; return a }(),
			mutate:     func(c ReclaimConfig) ReclaimConfig { c.Zones = []string{""}; return c },
			wantReason: "no reclaim zone",
		},
		{
			name:       "no tags at all",
			audit:      IdentityAudit{Domain: "fixture-abc.trymnexa.com"},
			wantReason: "not tagged as managed by e2a",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseReclaimConfig()
			if tc.mutate != nil {
				cfg = tc.mutate(cfg)
			}
			ok, reason := orphanReclaimable(tc.audit, cfg, reclaimNow)
			if ok != tc.wantOK {
				t.Fatalf("orphanReclaimable = (%v, %q), want ok=%v", ok, reason, tc.wantOK)
			}
			if ok {
				if reason == "" {
					t.Fatal("an accepted candidate must still report WHY it qualified, for the deletion log")
				}
				return
			}
			if reason == "" {
				t.Fatal("a refusal must always carry a reason")
			}
			if tc.wantReason != "" && !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("refusal reason = %q, want it to mention %q", reason, tc.wantReason)
			}
		})
	}
}

// TestOrphanReclaimableIgnoresEnabled proves the decision function is purely
// about whether the identity QUALIFIES. Arming is the reaper's separate
// question, which is what lets observe-only mode log the real decision.
func TestOrphanReclaimableIgnoresEnabled(t *testing.T) {
	cfg := baseReclaimConfig()
	cfg.Enabled = false
	ok, reason := orphanReclaimable(qualifyingAudit(), cfg, reclaimNow)
	if !ok {
		t.Fatalf("disarming must not change the decision, got refusal %q", reason)
	}
}

// TestOrphanReclaimableCaseInsensitiveZone covers DNS names being
// case-insensitive: SES echoes back whatever case the identity was created
// with, and a mixed-case fixture must not silently escape its zone.
func TestOrphanReclaimableCaseInsensitiveZone(t *testing.T) {
	audit := qualifyingAudit()
	audit.Domain = "Fixture-ABC.TryMnexa.com"
	if ok, reason := orphanReclaimable(audit, baseReclaimConfig(), reclaimNow); !ok {
		t.Fatalf("mixed-case identity refused: %q", reason)
	}
}

// reclaimZone is the fictional zone the reaper-level tests declare as e2a's
// own fixture space.
const reclaimZone = "fixtures.example.test"

// armedReaperReclaim is the policy the reaper-level tests wire into a
// ReapWorker. It is judged against time.Now() (the reaper's own clock), so the
// fixture audits below are stamped relative to that rather than to reclaimNow.
func armedReaperReclaim(maxPerSweep int) ReclaimConfig {
	return ReclaimConfig{
		Enabled:     true,
		Deployment:  DeploymentStaging,
		Zones:       []string{reclaimZone},
		MinAge:      7 * 24 * time.Hour,
		MaxPerSweep: maxPerSweep,
	}
}

// liveFixtureAudit is a qualifying audit stamped against the wall clock, for
// the reaper-level tests that cannot inject a time source.
func liveFixtureAudit() IdentityAudit {
	now := time.Now().UTC()
	return IdentityAudit{
		Tags: map[string]string{
			managedIdentityTagKey: managedIdentityTagValue,
			envTagKey:             DeploymentStaging,
			purposeTagKey:         purposeFixture,
			createdTagKey:         now.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
			expiresTagKey:         now.Add(-29 * 24 * time.Hour).Format(time.RFC3339),
		},
	}
}

// captureLog redirects the standard logger for the duration of a test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func TestReapWorkerOrphanReclaim(t *testing.T) {
	t.Run("armed policy deletes a qualifying orphan", func(t *testing.T) {
		domain := "run-1." + reclaimZone
		store := newFakeStore()
		prov := NewFakeProvider()
		prov.SeedIdentity(domain)
		prov.SetIdentityAudit(domain, liveFixtureAudit())
		logs := captureLog(t)
		w := &ReapWorker{store: store, provider: prov, reclaim: armedReaperReclaim(5)}

		if err := runReapWorkerChain(context.Background(), w, ReapV2Args{}); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.DeprovisionCalls) != 1 || prov.DeprovisionCalls[0] != domain {
			t.Fatalf("qualifying orphan was not deleted via Deprovision: %v", prov.DeprovisionCalls)
		}
		identities, _ := prov.List(context.Background())
		if len(identities) != 0 {
			t.Fatalf("identity survived deletion: %v", identities)
		}
		if !strings.Contains(logs.String(), "DELETED orphan sending identity "+domain) {
			t.Fatalf("deletion must be logged loudly with the domain and why it qualified, got %q", logs.String())
		}
	})

	t.Run("disarmed policy logs the decision without deleting", func(t *testing.T) {
		domain := "run-2." + reclaimZone
		store := newFakeStore()
		prov := NewFakeProvider()
		prov.SeedIdentity(domain)
		prov.SetIdentityAudit(domain, liveFixtureAudit())
		logs := captureLog(t)
		cfg := armedReaperReclaim(5)
		cfg.Enabled = false
		w := &ReapWorker{store: store, provider: prov, reclaim: cfg}

		if err := runReapWorkerChain(context.Background(), w, ReapV2Args{}); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.DeprovisionCalls) != 0 {
			t.Fatalf("observe-only mode made a provider mutation: %v", prov.DeprovisionCalls)
		}
		if !strings.Contains(logs.String(), "WOULD DELETE orphan sending identity "+domain) {
			t.Fatalf("observe-only mode must log the decision it would have acted on, got %q", logs.String())
		}
	})

	t.Run("the per-job cap skips later candidates", func(t *testing.T) {
		store := newFakeStore()
		prov := NewFakeProvider()
		// The fake pages identities in sorted order, so the two lowest names
		// consume the budget and the third must be refused by the cap.
		for _, name := range []string{"a", "b", "c"} {
			domain := name + "." + reclaimZone
			prov.SeedIdentity(domain)
			prov.SetIdentityAudit(domain, liveFixtureAudit())
		}
		logs := captureLog(t)
		w := &ReapWorker{store: store, provider: prov, reclaim: armedReaperReclaim(2)}

		if err := runReapWorkerChain(context.Background(), w, ReapV2Args{}); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.DeprovisionCalls) != 2 {
			t.Fatalf("cap of 2 not honored, deleted: %v", prov.DeprovisionCalls)
		}
		identities, _ := prov.List(context.Background())
		if len(identities) != 1 || identities[0] != "c."+reclaimZone {
			t.Fatalf("wrong survivor after the cap: %v", identities)
		}
		if !strings.Contains(logs.String(), "per-sweep reclaim cap of 2 already reached") {
			t.Fatalf("the capped candidate must say so, got %q", logs.String())
		}
	})

	t.Run("an untagged legacy orphan is never deleted", func(t *testing.T) {
		// The fake reports a seeded-but-unaudited identity as untagged, which
		// is the shape of every identity created before tags.go shipped.
		domain := "legacy." + reclaimZone
		store := newFakeStore()
		prov := NewFakeProvider()
		prov.SeedIdentity(domain)
		logs := captureLog(t)
		w := &ReapWorker{store: store, provider: prov, reclaim: armedReaperReclaim(5)}

		if err := runReapWorkerChain(context.Background(), w, ReapV2Args{}); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.DeprovisionCalls) != 0 {
			t.Fatalf("untagged legacy identity was deleted: %v", prov.DeprovisionCalls)
		}
		logged := logs.String()
		if !strings.Contains(logged, "ALERT orphan sending identity") || !strings.Contains(logged, "not tagged as managed by e2a") {
			t.Fatalf("expected the standing ALERT extended with the refusal reason, got %q", logged)
		}
	})

	t.Run("an identity still verified for sending is never deleted", func(t *testing.T) {
		domain := "sending." + reclaimZone
		store := newFakeStore()
		prov := NewFakeProvider()
		prov.SeedIdentity(domain)
		audit := liveFixtureAudit()
		audit.VerifiedForSending = true
		prov.SetIdentityAudit(domain, audit)
		logs := captureLog(t)
		w := &ReapWorker{store: store, provider: prov, reclaim: armedReaperReclaim(5)}

		if err := runReapWorkerChain(context.Background(), w, ReapV2Args{}); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.DeprovisionCalls) != 0 {
			t.Fatalf("a sendable identity was deleted: %v", prov.DeprovisionCalls)
		}
		if !strings.Contains(logs.String(), "verified for sending") {
			t.Fatalf("expected the verified-for-sending refusal, got %q", logs.String())
		}
	})

	t.Run("a candidate outside the reclaim zone is never deleted", func(t *testing.T) {
		// The near-miss the zone rule exists for, exercised end to end: this
		// name would pass every tag guard.
		domain := "evil" + reclaimZone
		store := newFakeStore()
		prov := NewFakeProvider()
		prov.SeedIdentity(domain)
		prov.SetIdentityAudit(domain, liveFixtureAudit())
		w := &ReapWorker{store: store, provider: prov, reclaim: armedReaperReclaim(5)}

		if err := runReapWorkerChain(context.Background(), w, ReapV2Args{}); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.DeprovisionCalls) != 0 {
			t.Fatalf("a near-miss of the reclaim zone was deleted: %v", prov.DeprovisionCalls)
		}
	})

	t.Run("the zero-value policy reclaims nothing", func(t *testing.T) {
		// The self-host / unconfigured default, and what every ReapWorker
		// built by struct literal in this package's other tests gets.
		domain := "default." + reclaimZone
		store := newFakeStore()
		prov := NewFakeProvider()
		prov.SeedIdentity(domain)
		prov.SetIdentityAudit(domain, liveFixtureAudit())
		w := &ReapWorker{store: store, provider: prov}

		if err := runReapWorkerChain(context.Background(), w, ReapV2Args{}); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.DeprovisionCalls) != 0 {
			t.Fatalf("an unconfigured deployment deleted an identity: %v", prov.DeprovisionCalls)
		}
		// And it costs no provider round trip: an unconfigured policy is
		// refused before the candidate is ever inspected, so the alert-only
		// audit stays as cheap as it was before reclaim existed.
		if len(prov.InspectCalls) != 0 {
			t.Fatalf("an unconfigured deployment still inspected the identity: %v", prov.InspectCalls)
		}
	})

	t.Run("a provider inspect failure refuses rather than deletes", func(t *testing.T) {
		domain := "unreadable." + reclaimZone
		store := newFakeStore()
		prov := NewFakeProvider()
		prov.SeedIdentity(domain)
		prov.SetInspectErr(domain, errors.New("throttled"))
		logs := captureLog(t)
		w := &ReapWorker{store: store, provider: prov, reclaim: armedReaperReclaim(5)}

		if err := runReapWorkerChain(context.Background(), w, ReapV2Args{}); err != nil {
			t.Fatalf("an unreadable orphan must not red the sweep: %v", err)
		}
		if len(prov.DeprovisionCalls) != 0 {
			t.Fatalf("deleted an identity it could not read: %v", prov.DeprovisionCalls)
		}
		if !strings.Contains(logs.String(), "provider inspect failed") {
			t.Fatalf("expected the inspect-failure refusal, got %q", logs.String())
		}
	})

	t.Run("Deprovision refusing ownership keeps the sweep green and the identity alive", func(t *testing.T) {
		// Defense in depth: even with every tag guard satisfied, the provider
		// re-checks ownership and may still say no.
		domain := "disputed." + reclaimZone
		store := newFakeStore()
		prov := NewFakeProvider()
		prov.SeedIdentity(domain)
		prov.SetIdentityAudit(domain, liveFixtureAudit())
		prov.SetDeprovisionErr(ErrIdentityNotOwned)
		logs := captureLog(t)
		w := &ReapWorker{store: store, provider: prov, reclaim: armedReaperReclaim(5)}

		if err := runReapWorkerChain(context.Background(), w, ReapV2Args{}); err != nil {
			t.Fatalf("a refused reclaim must not red the sweep: %v", err)
		}
		identities, _ := prov.List(context.Background())
		if len(identities) != 1 {
			t.Fatalf("identity was removed despite the provider refusing: %v", identities)
		}
		if !strings.Contains(logs.String(), "delete failed") {
			t.Fatalf("expected the delete-failure refusal, got %q", logs.String())
		}
	})

	t.Run("a live domain outside the ledger is never inspected or deleted", func(t *testing.T) {
		// Guard 1, the caller's precondition: a domain row exists, so this is
		// not an orphan at all and the reclaim decision must never run.
		domain := "live." + reclaimZone
		store := newFakeStore()
		store.setStatus(domain, StatusVerified)
		prov := NewFakeProvider()
		prov.SeedIdentity(domain)
		prov.SetIdentityAudit(domain, liveFixtureAudit())
		w := &ReapWorker{store: store, provider: prov, reclaim: armedReaperReclaim(5)}

		if err := runReapWorkerChain(context.Background(), w, ReapV2Args{}); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		if len(prov.InspectCalls) != 0 || len(prov.DeprovisionCalls) != 0 {
			t.Fatalf("a domain-backed identity was evaluated for reclaim: inspect=%v deprovision=%v", prov.InspectCalls, prov.DeprovisionCalls)
		}
	})

	t.Run("the managed-ledger phase is unchanged by an armed reclaim policy", func(t *testing.T) {
		// Regression: reclaim touches only the orphan phase. A LEDGERED
		// tombstone still converges through the ordinary teardown path, which
		// consults neither the reclaim zones nor the cap.
		domain := "ledgered.customer.example"
		store := newFakeStore()
		store.managed[domain] = "old-incarnation"
		prov := NewFakeProvider()
		prov.SeedIdentity(domain)
		w := &ReapWorker{store: store, provider: prov, reclaim: armedReaperReclaim(5)}

		if err := runReapWorkerChain(context.Background(), w, ReapV2Args{}); err != nil {
			t.Fatalf("Work returned error: %v", err)
		}
		identities, _ := prov.List(context.Background())
		if len(identities) != 0 || len(store.managed) != 0 {
			t.Fatalf("managed orphan did not converge: provider=%v ledger=%v", identities, store.managed)
		}
		if len(prov.InspectCalls) != 0 {
			t.Fatalf("the ledger phase must not run the reclaim decision: %v", prov.InspectCalls)
		}
	})
}
