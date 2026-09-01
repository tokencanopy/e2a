package sendingpolicy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// These tests operate on singleton rows in a shared test database, so none of
// them assume an absolute generation or revision. Each reads current state and
// expresses its compare-and-swap relative to that, which is also exactly how a
// real operator command behaves.

func newModule(t *testing.T) (*sendingpolicy.Module, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.TestDB(t)
	recipients, err := sendingpolicy.LoadOperatorRecipients(testOperatorV1)
	if err != nil {
		t.Fatalf("load default operator map: %v", err)
	}
	m := sendingpolicy.NewModule(pool, sendingpolicy.Secrets{Recipients: recipients})
	if _, err := m.RegisterOperatorRecipients(context.Background(), "integration-test-bootstrap"); err != nil {
		t.Fatalf("register default operator map: %v", err)
	}
	return m, pool
}

func mustInspect(t *testing.T, m *sendingpolicy.Module) sendingpolicy.PolicySnapshot {
	t.Helper()
	snapshot, err := m.InspectPolicy(context.Background())
	if err != nil {
		t.Fatalf("inspect policy: %v", err)
	}
	return snapshot
}

// TestInspectPolicyVerifiesStoredHash proves the read path re-derives the hash
// from the bytes it actually read rather than trusting the recorded column.
// Postgres normalizes jsonb, so this is the only thing that would catch a row
// whose content and hash drifted apart.
func TestInspectPolicyVerifiesStoredHash(t *testing.T) {
	m, _ := newModule(t)
	snapshot := mustInspect(t, m)

	recomputed, err := sendingpolicy.Hash(snapshot.Policy)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if recomputed != snapshot.PolicySHA256 {
		t.Errorf("stored hash %s does not describe the stored policy (%s)", snapshot.PolicySHA256, recomputed)
	}
	if snapshot.SchemaVersion != sendingpolicy.SchemaVersion {
		t.Errorf("schema version = %d, want %d", snapshot.SchemaVersion, sendingpolicy.SchemaVersion)
	}
}

func TestPolicyCASCorruptFailClosed(t *testing.T) {
	ctx := context.Background()

	for name, corrupt := range map[string]func(*testing.T, *pgxpool.Pool){
		"hash mismatch": func(t *testing.T, pool *pgxpool.Pool) {
			if _, err := pool.Exec(ctx,
				`UPDATE sending_protection_runtime_policy SET policy_sha256 = $1 WHERE singleton`,
				strings.Repeat("0", 64)); err != nil {
				t.Fatalf("corrupt policy hash: %v", err)
			}
		},
		"unsupported schema": func(t *testing.T, pool *pgxpool.Pool) {
			if _, err := pool.Exec(ctx,
				`UPDATE sending_protection_runtime_policy SET schema_version = $1 WHERE singleton`,
				sendingpolicy.SchemaVersion+1); err != nil {
				t.Fatalf("corrupt schema version: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, pool := newModule(t)
			corrupt(t, pool)
			if _, err := m.InspectPolicy(ctx); err == nil {
				t.Fatal("expected corrupt runtime policy to fail closed")
			}
		})
	}
}

// TestActivatePolicyAdvancesOneGeneration is the happy path, and asserts the
// audit event is written in the same transaction as the policy itself.
func TestPolicyCASAdvancesOneGeneration(t *testing.T) {
	ctx := context.Background()
	m, pool := newModule(t)
	before := mustInspect(t, m)

	next := before.Policy
	next.DetectorIntervalSeconds = before.Policy.DetectorIntervalSeconds + 1

	after, err := m.ActivatePolicy(ctx, sendingpolicy.ActivationRequest{
		ExpectedGeneration: before.Generation,
		Policy:             next,
		Actor:              "integration-test",
		Reason:             "advance one generation",
	})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if after.Generation != before.Generation+1 {
		t.Errorf("generation = %d, want %d", after.Generation, before.Generation+1)
	}
	if after.PolicySHA256 == before.PolicySHA256 {
		t.Error("a changed policy must produce a different hash")
	}

	var priorHash, newHash, actor, reason string
	if err := pool.QueryRow(ctx,
		`SELECT prior_policy_sha256, new_policy_sha256, actor, reason
		   FROM sending_protection_policy_events WHERE generation = $1`, after.Generation,
	).Scan(&priorHash, &newHash, &actor, &reason); err != nil {
		t.Fatalf("read audit event: %v", err)
	}
	if priorHash != before.PolicySHA256 || newHash != after.PolicySHA256 {
		t.Errorf("audit event does not chain %s -> %s (got %s -> %s)",
			before.PolicySHA256, after.PolicySHA256, priorHash, newHash)
	}
	if actor != "integration-test" || reason != "advance one generation" {
		t.Errorf("audit event lost actor/reason: %s / %s", actor, reason)
	}

	// The stored policy must read back as exactly what was submitted.
	reread := mustInspect(t, m)
	if reread.Policy.DetectorIntervalSeconds != next.DetectorIntervalSeconds {
		t.Errorf("stored detector interval = %d, want %d",
			reread.Policy.DetectorIntervalSeconds, next.DetectorIntervalSeconds)
	}
}

// TestActivatePolicyRejectsStaleGeneration is the core safety property: an
// operator who reviewed generation N cannot write over generation N+1 that
// someone else activated in the meantime.
func TestPolicyCASRejectsStaleGeneration(t *testing.T) {
	ctx := context.Background()
	m, pool := newModule(t)
	current := mustInspect(t, m)

	stale := current.Policy
	stale.DetectorWindowDays = current.Policy.DetectorWindowDays + 1

	var eventsBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sending_protection_policy_events`).Scan(&eventsBefore); err != nil {
		t.Fatalf("count events: %v", err)
	}

	_, err := m.ActivatePolicy(ctx, sendingpolicy.ActivationRequest{
		ExpectedGeneration: current.Generation - 1, // deliberately behind
		Policy:             stale,
		Actor:              "integration-test",
		Reason:             "should not apply",
	})
	if !errors.Is(err, sendingpolicy.ErrStaleGeneration) {
		t.Fatalf("expected ErrStaleGeneration, got %v", err)
	}

	// Zero writes: neither the policy nor the audit log moved.
	after := mustInspect(t, m)
	if after.Generation != current.Generation || after.PolicySHA256 != current.PolicySHA256 {
		t.Error("a stale activation must not change stored policy state")
	}
	var eventsAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sending_protection_policy_events`).Scan(&eventsAfter); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventsAfter != eventsBefore {
		t.Errorf("a stale activation wrote %d audit events", eventsAfter-eventsBefore)
	}
}

// TestActivatePolicyRequiresActorAndReason keeps the audit trail meaningful.
// The database CHECK would also reject these, but failing before the
// transaction opens gives the operator a usable message.
func TestPolicyCASRequiresActorAndReason(t *testing.T) {
	ctx := context.Background()
	m, _ := newModule(t)
	current := mustInspect(t, m)

	for name, req := range map[string]sendingpolicy.ActivationRequest{
		"blank actor":  {ExpectedGeneration: current.Generation, Policy: current.Policy, Actor: "  ", Reason: "r"},
		"blank reason": {ExpectedGeneration: current.Generation, Policy: current.Policy, Actor: "a", Reason: ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := m.ActivatePolicy(ctx, req); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

// TestActivatePolicyRejectsInvalidPolicy proves validation runs before any
// database work, so an illegal policy can never reach the row.
func TestPolicyCASRejectsInvalidPolicy(t *testing.T) {
	ctx := context.Background()
	m, _ := newModule(t)
	current := mustInspect(t, m)

	invalid := current.Policy
	invalid.BouncePauseBasisPoints = 0

	if _, err := m.ActivatePolicy(ctx, sendingpolicy.ActivationRequest{
		ExpectedGeneration: current.Generation,
		Policy:             invalid,
		Actor:              "integration-test",
		Reason:             "invalid",
	}); err == nil {
		t.Fatal("expected an invalid policy to be rejected")
	}

	if after := mustInspect(t, m); after.Generation != current.Generation {
		t.Error("a rejected policy must not advance the generation")
	}
}

// TestConcurrentActivationsSerialize proves the row lock makes exactly one of
// two racing activations win. Both start from the same generation, so the
// loser must be told it is stale rather than silently overwriting the winner.
func TestPolicyCASConcurrentActivationsSerialize(t *testing.T) {
	ctx := context.Background()
	m, _ := newModule(t)
	start := mustInspect(t, m)

	first := start.Policy
	first.DetectorIntervalSeconds = 601
	second := start.Policy
	second.DetectorIntervalSeconds = 602

	type result struct {
		err error
	}
	results := make(chan result, 2)
	for _, p := range []sendingpolicy.RuntimePolicy{first, second} {
		go func(policy sendingpolicy.RuntimePolicy) {
			_, err := m.ActivatePolicy(ctx, sendingpolicy.ActivationRequest{
				ExpectedGeneration: start.Generation,
				Policy:             policy,
				Actor:              "integration-test",
				Reason:             "concurrent activation",
			})
			results <- result{err: err}
		}(p)
	}

	var wins, stales int
	for i := 0; i < 2; i++ {
		r := <-results
		switch {
		case r.err == nil:
			wins++
		case errors.Is(r.err, sendingpolicy.ErrStaleGeneration):
			stales++
		default:
			t.Fatalf("unexpected error: %v", r.err)
		}
	}
	if wins != 1 || stales != 1 {
		t.Fatalf("expected exactly one winner and one stale loser, got %d/%d", wins, stales)
	}
	if after := mustInspect(t, m); after.Generation != start.Generation+1 {
		t.Errorf("generation advanced by more than one: %d -> %d", start.Generation, after.Generation)
	}
}

// TestActivatePolicyRejectsUnregisteredSelectedOperatorRecipient pins the
// registry precondition on policy CAS itself. A reviewed payload must not be
// selectable merely because its operator version is a positive integer: that
// version's non-secret commitment has to exist in the permanent registry
// before the policy generation can advance.
func TestPolicyCASRejectsUnregisteredSelectedOperatorRecipient(t *testing.T) {
	ctx := context.Background()
	m, pool := newModule(t)
	current := mustInspect(t, m)

	next := current.Policy
	next.OperatorNoticeRecipientVersion = 910101

	var eventsBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sending_protection_policy_events`).Scan(&eventsBefore); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if _, err := m.ActivatePolicy(ctx, sendingpolicy.ActivationRequest{
		ExpectedGeneration: current.Generation,
		Policy:             next,
		Actor:              "integration-test",
		Reason:             "unregistered recipient must not be selected",
	}); err == nil {
		t.Fatal("expected an unregistered operator recipient version to be rejected")
	}

	after := mustInspect(t, m)
	if after.Generation != current.Generation || after.PolicySHA256 != current.PolicySHA256 {
		t.Error("an unregistered recipient version advanced the policy")
	}
	var eventsAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sending_protection_policy_events`).Scan(&eventsAfter); err != nil {
		t.Fatalf("count events after rejection: %v", err)
	}
	if eventsAfter != eventsBefore {
		t.Errorf("an unregistered recipient version wrote %d policy events", eventsAfter-eventsBefore)
	}
}

func TestPolicyCASRejectsMismatchedSelectedOperatorRecipient(t *testing.T) {
	ctx := context.Background()
	_, pool := newModule(t) // registers version 1 under testOperatorV1

	changed, err := sendingpolicy.LoadOperatorRecipients(
		`{"commitment_key":"` + testCommitmentKey + `","recipients":{"1":"changed-operator@example.test"}}`,
	)
	if err != nil {
		t.Fatalf("load changed operator map: %v", err)
	}
	m := sendingpolicy.NewModule(pool, sendingpolicy.Secrets{Recipients: changed})
	current := mustInspect(t, m)

	if _, err := m.ActivatePolicy(ctx, sendingpolicy.ActivationRequest{
		ExpectedGeneration: current.Generation,
		Policy:             current.Policy,
		Actor:              "integration-test",
		Reason:             "mismatched commitment must not be selected",
	}); !errors.Is(err, sendingpolicy.ErrRegistryConflict) {
		t.Fatalf("expected ErrRegistryConflict, got %v", err)
	}
	if after := mustInspect(t, m); after.Generation != current.Generation {
		t.Error("a mismatched recipient commitment advanced the policy")
	}
}

// TestBudgetEnforcementRequiresCompatibleBillingAttestation is the dormant
// mechanism's most important activation interlock. Shadow remains deployable
// on the migration-owned contract-0 attestation, but the first transition to
// enforce must lock that row and reject either billing contract below 1.
func TestPolicyCASBudgetEnforcementRequiresCompatibleBillingAttestation(t *testing.T) {
	ctx := context.Background()
	m, _ := newModule(t)
	current := mustInspect(t, m)

	shadow := current.Policy
	shadow.BudgetMode = sendingpolicy.ModeShadow
	shadowSnapshot, err := m.ActivatePolicy(ctx, sendingpolicy.ActivationRequest{
		ExpectedGeneration: current.Generation,
		Policy:             shadow,
		Actor:              "integration-test",
		Reason:             "shadow is compatible with contract zero",
	})
	if err != nil {
		t.Fatalf("activate shadow policy: %v", err)
	}

	enforce := shadowSnapshot.Policy
	enforce.BudgetMode = sendingpolicy.ModeEnforce
	if _, err := m.ActivatePolicy(ctx, sendingpolicy.ActivationRequest{
		ExpectedGeneration: shadowSnapshot.Generation,
		Policy:             enforce,
		Actor:              "integration-test",
		Reason:             "contract zero must block enforcement",
	}); err == nil {
		t.Fatal("expected contract-0 billing attestation to block budget enforcement")
	}

	after := mustInspect(t, m)
	if after.Generation != shadowSnapshot.Generation || after.PolicySHA256 != shadowSnapshot.PolicySHA256 {
		t.Error("a rejected enforcement transition advanced the policy")
	}

	attestation, hash := mustAttestation(t, m)
	if _, err := m.AttestRuntime(ctx, sendingpolicy.RuntimeAttestationRequest{
		ExpectedRevision:        attestation.Revision,
		ExpectedSHA256:          hash,
		ActiveBillingDigest:     "sha256:active-compatible",
		ActiveBillingContract:   1,
		RollbackBillingDigest:   "sha256:rollback-compatible",
		RollbackBillingContract: 1,
		Actor:                   "integration-test",
		Reason:                  "attest compatible billing pair",
	}); err != nil {
		t.Fatalf("attest compatible billing pair: %v", err)
	}
	if _, err := m.ActivatePolicy(ctx, sendingpolicy.ActivationRequest{
		ExpectedGeneration: shadowSnapshot.Generation,
		Policy:             enforce,
		Actor:              "integration-test",
		Reason:             "contract one permits enforcement",
	}); err != nil {
		t.Fatalf("activate enforcement with compatible billing: %v", err)
	}
}

// --- Runtime attestation --------------------------------------------------

func mustAttestation(t *testing.T, m *sendingpolicy.Module) (sendingpolicy.RuntimeAttestation, string) {
	t.Helper()
	current, err := m.InspectAttestation(context.Background())
	if err != nil {
		t.Fatalf("inspect attestation: %v", err)
	}
	hash, err := sendingpolicy.AttestationHash(current)
	if err != nil {
		t.Fatalf("attestation hash: %v", err)
	}
	return current, hash
}

func TestRuntimeAttestationForwardCAS(t *testing.T) {
	ctx := context.Background()
	m, _ := newModule(t)
	current, hash := mustAttestation(t, m)

	next, err := m.AttestRuntime(ctx, sendingpolicy.RuntimeAttestationRequest{
		ExpectedRevision:        current.Revision,
		ExpectedSHA256:          hash,
		ActiveBillingDigest:     "sha256:aaa",
		ActiveBillingContract:   1,
		RollbackBillingDigest:   "sha256:bbb",
		RollbackBillingContract: 1,
		Actor:                   "integration-test",
		Reason:                  "forward",
	})
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	if next.Revision != current.Revision+1 {
		t.Errorf("revision = %d, want %d", next.Revision, current.Revision+1)
	}
	if next.ActiveBillingDigest != "sha256:aaa" || next.RollbackBillingContract != 1 {
		t.Errorf("attestation did not store the requested fields: %+v", next)
	}
}

func TestRuntimeAttestationRejectsStaleRevisionAndHash(t *testing.T) {
	ctx := context.Background()
	m, _ := newModule(t)
	current, hash := mustAttestation(t, m)

	t.Run("stale revision", func(t *testing.T) {
		_, err := m.AttestRuntime(ctx, sendingpolicy.RuntimeAttestationRequest{
			ExpectedRevision: current.Revision + 5,
			ExpectedSHA256:   hash,
			Actor:            "integration-test",
			Reason:           "stale revision",
		})
		if !errors.Is(err, sendingpolicy.ErrStaleAttestation) {
			t.Fatalf("expected ErrStaleAttestation, got %v", err)
		}
	})

	t.Run("wrong four-field hash", func(t *testing.T) {
		_, err := m.AttestRuntime(ctx, sendingpolicy.RuntimeAttestationRequest{
			ExpectedRevision: current.Revision,
			ExpectedSHA256:   "0000000000000000000000000000000000000000000000000000000000000000",
			Actor:            "integration-test",
			Reason:           "wrong hash",
		})
		if !errors.Is(err, sendingpolicy.ErrStaleAttestation) {
			t.Fatalf("expected ErrStaleAttestation, got %v", err)
		}
	})

	if after, _ := mustAttestation(t, m); after.Revision != current.Revision {
		t.Error("a rejected attestation must not advance the revision")
	}
}

// TestAttestRuntimeRejectsABARetry is the sequence the plan names explicitly:
// P@r1 -> F@r2 -> P@r3, then a delayed writer that still expects r1 with
// hash(P). Its four expected fields DO match the current state at r3, so a CAS
// on content alone would accept it and silently resurrect a superseded write.
// The revision comparison is what makes it stale.
func TestRuntimeAttestationRejectsABARetry(t *testing.T) {
	ctx := context.Background()
	m, _ := newModule(t)

	stateP := func(r sendingpolicy.RuntimeAttestation) sendingpolicy.RuntimeAttestationRequest {
		return sendingpolicy.RuntimeAttestationRequest{
			ActiveBillingDigest: "sha256:P", ActiveBillingContract: 1,
			RollbackBillingDigest: "sha256:P-rollback", RollbackBillingContract: 1,
			Actor: "integration-test", Reason: "state P",
		}
	}
	stateF := sendingpolicy.RuntimeAttestationRequest{
		ActiveBillingDigest: "sha256:F", ActiveBillingContract: 1,
		RollbackBillingDigest: "sha256:F-rollback", RollbackBillingContract: 1,
		Actor: "integration-test", Reason: "state F",
	}

	// Reach P at r1.
	current, hash := mustAttestation(t, m)
	reqP := stateP(current)
	reqP.ExpectedRevision, reqP.ExpectedSHA256 = current.Revision, hash
	atP1, err := m.AttestRuntime(ctx, reqP)
	if err != nil {
		t.Fatalf("write P: %v", err)
	}
	hashP, err := sendingpolicy.AttestationHash(atP1)
	if err != nil {
		t.Fatalf("hash P: %v", err)
	}

	// Move to F at r2.
	stateF.ExpectedRevision, stateF.ExpectedSHA256 = atP1.Revision, hashP
	atF2, err := m.AttestRuntime(ctx, stateF)
	if err != nil {
		t.Fatalf("write F: %v", err)
	}
	hashF, err := sendingpolicy.AttestationHash(atF2)
	if err != nil {
		t.Fatalf("hash F: %v", err)
	}

	// Restore P at r3 — same four fields as r1, higher revision.
	reqP2 := stateP(atF2)
	reqP2.ExpectedRevision, reqP2.ExpectedSHA256 = atF2.Revision, hashF
	atP3, err := m.AttestRuntime(ctx, reqP2)
	if err != nil {
		t.Fatalf("restore P: %v", err)
	}
	if atP3.Revision != atP1.Revision+2 {
		t.Fatalf("expected r1+2, got %d", atP3.Revision)
	}

	// The delayed writer from before F: its expected content matches the
	// CURRENT state exactly, and it must still be refused.
	currentHash, err := sendingpolicy.AttestationHash(atP3)
	if err != nil {
		t.Fatalf("hash current: %v", err)
	}
	if currentHash != hashP {
		t.Fatal("test precondition: r1 and r3 must carry identical four fields")
	}
	delayed := stateF
	delayed.ExpectedRevision, delayed.ExpectedSHA256 = atP1.Revision, hashP
	delayed.Reason = "delayed ABA retry"
	if _, err := m.AttestRuntime(ctx, delayed); !errors.Is(err, sendingpolicy.ErrStaleAttestation) {
		t.Fatalf("an ABA retry must be stale even when its four fields match, got %v", err)
	}

	if after, _ := mustAttestation(t, m); after.Revision != atP3.Revision {
		t.Error("the rejected ABA retry must not have advanced the revision")
	}
}

func TestRuntimeAttestationConcurrentWinnerAndLoser(t *testing.T) {
	ctx := context.Background()
	m, _ := newModule(t)
	current, hash := mustAttestation(t, m)

	requests := []sendingpolicy.RuntimeAttestationRequest{
		{
			ExpectedRevision: current.Revision, ExpectedSHA256: hash,
			ActiveBillingDigest: "sha256:concurrent-a", ActiveBillingContract: 1,
			RollbackBillingDigest: "sha256:concurrent-a-rollback", RollbackBillingContract: 1,
			Actor: "integration-test", Reason: "concurrent A",
		},
		{
			ExpectedRevision: current.Revision, ExpectedSHA256: hash,
			ActiveBillingDigest: "sha256:concurrent-b", ActiveBillingContract: 1,
			RollbackBillingDigest: "sha256:concurrent-b-rollback", RollbackBillingContract: 1,
			Actor: "integration-test", Reason: "concurrent B",
		},
	}

	results := make(chan error, 2)
	for _, req := range requests {
		go func(r sendingpolicy.RuntimeAttestationRequest) {
			_, err := m.AttestRuntime(ctx, r)
			results <- err
		}(req)
	}
	var wins, stales int
	for range requests {
		switch err := <-results; {
		case err == nil:
			wins++
		case errors.Is(err, sendingpolicy.ErrStaleAttestation):
			stales++
		default:
			t.Fatalf("unexpected attestation result: %v", err)
		}
	}
	if wins != 1 || stales != 1 {
		t.Fatalf("expected one winner and one stale loser, got wins=%d stales=%d", wins, stales)
	}
	if after, _ := mustAttestation(t, m); after.Revision != current.Revision+1 {
		t.Errorf("concurrent attestation advanced revision by more than one: %d -> %d", current.Revision, after.Revision)
	}
}

func TestRuntimeAttestationAbortFenceBothOrders(t *testing.T) {
	ctx := context.Background()

	t.Run("fence first makes delayed forward stale", func(t *testing.T) {
		m, _ := newModule(t)
		prior, priorHash := mustAttestation(t, m)
		fence := sendingpolicy.RuntimeAttestationRequest{
			ExpectedRevision: prior.Revision, ExpectedSHA256: priorHash,
			ActiveBillingDigest: prior.ActiveBillingDigest, ActiveBillingContract: prior.ActiveBillingContract,
			RollbackBillingDigest: prior.RollbackBillingDigest, RollbackBillingContract: prior.RollbackBillingContract,
			Actor: "integration-test", Reason: "ambiguous abort fence",
		}
		if _, err := m.AttestRuntime(ctx, fence); err != nil {
			t.Fatalf("write fence: %v", err)
		}
		delayedForward := sendingpolicy.RuntimeAttestationRequest{
			ExpectedRevision: prior.Revision, ExpectedSHA256: priorHash,
			ActiveBillingDigest: "sha256:delayed", ActiveBillingContract: 1,
			RollbackBillingDigest: "sha256:delayed-rollback", RollbackBillingContract: 1,
			Actor: "integration-test", Reason: "delayed forward",
		}
		if _, err := m.AttestRuntime(ctx, delayedForward); !errors.Is(err, sendingpolicy.ErrStaleAttestation) {
			t.Fatalf("delayed forward after fence = %v, want stale", err)
		}
	})

	t.Run("forward first makes fence stale and restores at next revision", func(t *testing.T) {
		m, _ := newModule(t)
		prior, priorHash := mustAttestation(t, m)
		forwardReq := sendingpolicy.RuntimeAttestationRequest{
			ExpectedRevision: prior.Revision, ExpectedSHA256: priorHash,
			ActiveBillingDigest: "sha256:forward", ActiveBillingContract: 1,
			RollbackBillingDigest: "sha256:forward-rollback", RollbackBillingContract: 1,
			Actor: "integration-test", Reason: "forward wins",
		}
		forward, err := m.AttestRuntime(ctx, forwardReq)
		if err != nil {
			t.Fatalf("write forward: %v", err)
		}
		fence := sendingpolicy.RuntimeAttestationRequest{
			ExpectedRevision: prior.Revision, ExpectedSHA256: priorHash,
			ActiveBillingDigest: prior.ActiveBillingDigest, ActiveBillingContract: prior.ActiveBillingContract,
			RollbackBillingDigest: prior.RollbackBillingDigest, RollbackBillingContract: prior.RollbackBillingContract,
			Actor: "integration-test", Reason: "losing fence",
		}
		if _, err := m.AttestRuntime(ctx, fence); !errors.Is(err, sendingpolicy.ErrStaleAttestation) {
			t.Fatalf("fence after forward = %v, want stale", err)
		}
		forwardHash, err := sendingpolicy.AttestationHash(forward)
		if err != nil {
			t.Fatalf("hash forward: %v", err)
		}
		restore := fence
		restore.ExpectedRevision = forward.Revision
		restore.ExpectedSHA256 = forwardHash
		restore.Reason = "restore after winning forward"
		restored, err := m.AttestRuntime(ctx, restore)
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		if restored.Revision != prior.Revision+2 {
			t.Errorf("restored revision = %d, want %d", restored.Revision, prior.Revision+2)
		}
	})
}

// --- Operator recipient registry -----------------------------------------

const (
	testCommitmentKey = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI"
	altCommitmentKey  = "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ"
	testOperatorV1    = `{"commitment_key":"` + testCommitmentKey + `","recipients":{"1":"default-operator@example.test"}}`
)

func TestRegisterOperatorRecipientsIsAppendOnlyAndIdempotent(t *testing.T) {
	ctx := context.Background()
	_, pool := newModule(t)

	// A version high enough that a shared test database cannot collide with
	// another test's registration.
	const version = 900001
	mapJSON := `{"commitment_key":"` + testCommitmentKey +
		`","recipients":{"900001":"registry-one@example.test"}}`

	recipients, err := sendingpolicy.LoadOperatorRecipients(mapJSON)
	if err != nil {
		t.Fatalf("load map: %v", err)
	}

	registryModule := sendingpolicy.NewModule(pool, sendingpolicy.Secrets{Recipients: recipients})
	inserted, err := registryModule.RegisterOperatorRecipients(ctx, "integration-test")
	if err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if len(inserted) != 1 || inserted[0] != version {
		t.Fatalf("expected version %d inserted, got %v", version, inserted)
	}

	// Replay is idempotent and writes nothing.
	replayed, err := registryModule.RegisterOperatorRecipients(ctx, "integration-test")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(replayed) != 0 {
		t.Errorf("replay must insert nothing, inserted %v", replayed)
	}

	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sending_operator_recipient_versions WHERE logical_version = $1`, version,
	).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("expected exactly one registry row, got %d", rows)
	}
}

func TestRegisterOperatorRecipientsRejectsChangedIdentity(t *testing.T) {
	ctx := context.Background()
	_, pool := newModule(t)

	const version = "900002"
	original := `{"commitment_key":"` + testCommitmentKey +
		`","recipients":{"` + version + `":"registry-two@example.test"}}`
	changedKey := `{"commitment_key":"` + altCommitmentKey +
		`","recipients":{"` + version + `":"registry-two@example.test"}}`
	changedMailbox := `{"commitment_key":"` + testCommitmentKey +
		`","recipients":{"` + version + `":"registry-two-moved@example.test"}}`

	first, err := sendingpolicy.LoadOperatorRecipients(original)
	if err != nil {
		t.Fatalf("load original: %v", err)
	}
	firstModule := sendingpolicy.NewModule(pool, sendingpolicy.Secrets{Recipients: first})
	if _, err := firstModule.RegisterOperatorRecipients(ctx, "integration-test"); err != nil {
		t.Fatalf("register original: %v", err)
	}

	for name, raw := range map[string]string{
		"different commitment key": changedKey,
		"different mailbox":        changedMailbox,
	} {
		t.Run(name, func(t *testing.T) {
			recipients, err := sendingpolicy.LoadOperatorRecipients(raw)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			changedModule := sendingpolicy.NewModule(pool, sendingpolicy.Secrets{Recipients: recipients})
			_, err = changedModule.RegisterOperatorRecipients(ctx, "integration-test")
			if !errors.Is(err, sendingpolicy.ErrRegistryConflict) {
				t.Fatalf("expected ErrRegistryConflict, got %v", err)
			}
		})
	}
}

func TestOperatorRecipientRegistryNeverReusesVersion(t *testing.T) {
	ctx := context.Background()
	_, pool := newModule(t) // version 1 is now permanently registered

	retiredMap, err := sendingpolicy.LoadOperatorRecipients(
		`{"commitment_key":"` + testCommitmentKey + `","recipients":{"2":"replacement-operator@example.test"}}`,
	)
	if err != nil {
		t.Fatalf("load post-retirement map: %v", err)
	}
	retiredModule := sendingpolicy.NewModule(pool, sendingpolicy.Secrets{Recipients: retiredMap})
	if _, err := retiredModule.RegisterOperatorRecipients(ctx, "integration-test"); err != nil {
		t.Fatalf("register replacement version: %v", err)
	}

	reusedV1, err := sendingpolicy.LoadOperatorRecipients(
		`{"commitment_key":"` + testCommitmentKey + `","recipients":{"1":"reused-version@example.test"}}`,
	)
	if err != nil {
		t.Fatalf("load attempted reuse: %v", err)
	}
	reuseModule := sendingpolicy.NewModule(pool, sendingpolicy.Secrets{Recipients: reusedV1})
	if _, err := reuseModule.RegisterOperatorRecipients(ctx, "integration-test"); !errors.Is(err, sendingpolicy.ErrRegistryConflict) {
		t.Fatalf("reusing retired logical version 1 = %v, want ErrRegistryConflict", err)
	}

	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sending_operator_recipient_versions WHERE logical_version = 1`,
	).Scan(&rows); err != nil {
		t.Fatalf("count permanent v1 row: %v", err)
	}
	if rows != 1 {
		t.Errorf("retired version 1 registry rows = %d, want 1 permanent row", rows)
	}
}

// --- One-shot ramp grandfathering ------------------------------------------

// TestGrandfatherFlipsOnlyVerifiedInactiveDomains covers the phase-3 snapshot:
// the policy CAS and the domain flip commit together, only currently
// sending-verified + ramp-inactive domains are exempted, and a second attempt
// is rejected by the marker with zero writes.
func TestPolicyCASGrandfatherFlipsOnlyVerifiedInactiveDomains(t *testing.T) {
	ctx := context.Background()
	m, pool := newModule(t)

	const userID = "gf-user-1"
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, google_subject) VALUES ($1, $2, $3)
		 ON CONFLICT (id) DO NOTHING`,
		userID, "gf-owner@example.test", "gf-subject-1"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	seed := func(domain, sendingStatus, rampStatus string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO domains (domain, user_id, verified, sending_status, sending_ramp_status)
			 VALUES ($1, $2, true, $3, $4)`,
			domain, userID, sendingStatus, rampStatus); err != nil {
			t.Fatalf("seed domain %s: %v", domain, err)
		}
	}
	seed("gf-flip.example.test", "verified", "inactive")     // the one that must flip
	seed("gf-pending.example.test", "pending", "inactive")   // not sender-verified: untouched
	seed("gf-ramping.example.test", "verified", "ramping")   // already ramping: untouched
	seed("gf-complete.example.test", "verified", "complete") // finished: untouched

	before := mustInspect(t, m)
	next := before.Policy
	next.RampEnabled = true

	after, err := m.ActivatePolicy(ctx, sendingpolicy.ActivationRequest{
		ExpectedGeneration:               before.Generation,
		Policy:                           next,
		Actor:                            "integration-test",
		Reason:                           "one-shot grandfather activation",
		GrandfatherCurrentSendingDomains: true,
	})
	if err != nil {
		t.Fatalf("grandfathering activation: %v", err)
	}

	wantStatus := map[string]string{
		"gf-flip.example.test":     "exempt",
		"gf-pending.example.test":  "inactive",
		"gf-ramping.example.test":  "ramping",
		"gf-complete.example.test": "complete",
	}
	for domain, want := range wantStatus {
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT sending_ramp_status FROM domains WHERE domain = $1`, domain).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", domain, err)
		}
		if got != want {
			t.Errorf("%s: ramp status = %s, want %s", domain, got, want)
		}
	}

	// The marker records the generation activated WITH the snapshot and the
	// requesting actor -- not the outgoing row's provenance.
	var markerGeneration int64
	var markerActor string
	if err := pool.QueryRow(ctx,
		`SELECT policy_generation, completed_by FROM sending_ramp_grandfathering WHERE singleton`,
	).Scan(&markerGeneration, &markerActor); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if markerGeneration != after.Generation {
		t.Errorf("marker generation = %d, want the activated generation %d", markerGeneration, after.Generation)
	}
	if markerActor != "integration-test" {
		t.Errorf("marker actor = %s", markerActor)
	}

	// Retry: a later domain must never be widened into the set, and the
	// rejected activation must not advance the policy either.
	seed("gf-late.example.test", "verified", "inactive")
	retry := after.Policy
	retry.DetectorIntervalSeconds++
	_, err = m.ActivatePolicy(ctx, sendingpolicy.ActivationRequest{
		ExpectedGeneration:               after.Generation,
		Policy:                           retry,
		Actor:                            "integration-test",
		Reason:                           "retry must be rejected",
		GrandfatherCurrentSendingDomains: true,
	})
	if !errors.Is(err, sendingpolicy.ErrAlreadyGrandfathered) {
		t.Fatalf("expected ErrAlreadyGrandfathered, got %v", err)
	}
	var lateStatus string
	if err := pool.QueryRow(ctx,
		`SELECT sending_ramp_status FROM domains WHERE domain = 'gf-late.example.test'`).Scan(&lateStatus); err != nil {
		t.Fatalf("read late domain: %v", err)
	}
	if lateStatus != "inactive" {
		t.Errorf("a rejected retry widened the grandfathered set: %s", lateStatus)
	}
	if current := mustInspect(t, m); current.Generation != after.Generation {
		t.Errorf("a rejected grandfather retry advanced the policy: %d -> %d", after.Generation, current.Generation)
	}
}
