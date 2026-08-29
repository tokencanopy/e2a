package sendingpolicy_test

import (
	"context"
	"errors"
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
	return sendingpolicy.NewModule(pool), pool
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

// TestActivatePolicyAdvancesOneGeneration is the happy path, and asserts the
// audit event is written in the same transaction as the policy itself.
func TestActivatePolicyAdvancesOneGeneration(t *testing.T) {
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
func TestActivatePolicyRejectsStaleGeneration(t *testing.T) {
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
func TestActivatePolicyRequiresActorAndReason(t *testing.T) {
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
func TestActivatePolicyRejectsInvalidPolicy(t *testing.T) {
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
func TestConcurrentActivationsSerialize(t *testing.T) {
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

func TestAttestRuntimeForwardCAS(t *testing.T) {
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

func TestAttestRuntimeRejectsStaleRevisionAndHash(t *testing.T) {
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
func TestAttestRuntimeRejectsABARetry(t *testing.T) {
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

// --- Operator recipient registry -----------------------------------------

const (
	testCommitmentKey = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI"
	altCommitmentKey  = "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ"
)

func TestRegisterOperatorRecipientsIsAppendOnlyAndIdempotent(t *testing.T) {
	ctx := context.Background()
	m, pool := newModule(t)

	// A version high enough that a shared test database cannot collide with
	// another test's registration.
	const version = 900001
	mapJSON := `{"commitment_key":"` + testCommitmentKey +
		`","recipients":{"900001":"registry-one@example.test"}}`

	recipients, err := sendingpolicy.LoadOperatorRecipients(mapJSON)
	if err != nil {
		t.Fatalf("load map: %v", err)
	}

	inserted, err := m.RegisterOperatorRecipients(ctx, recipients, "integration-test")
	if err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if len(inserted) != 1 || inserted[0] != version {
		t.Fatalf("expected version %d inserted, got %v", version, inserted)
	}

	// Replay is idempotent and writes nothing.
	replayed, err := m.RegisterOperatorRecipients(ctx, recipients, "integration-test")
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
	m, _ := newModule(t)

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
	if _, err := m.RegisterOperatorRecipients(ctx, first, "integration-test"); err != nil {
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
			_, err = m.RegisterOperatorRecipients(ctx, recipients, "integration-test")
			if !errors.Is(err, sendingpolicy.ErrRegistryConflict) {
				t.Fatalf("expected ErrRegistryConflict, got %v", err)
			}
		})
	}
}
