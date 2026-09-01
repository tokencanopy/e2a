package sendingpolicy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/testutil"
)

var errSyntheticCommitResponseLoss = errors.New("synthetic commit response loss")

func responseLossDigest(hexDigit string) string {
	return "sha256:" + strings.Repeat(hexDigit, 64)
}

func TestRuntimeAttestationCommitResponseLossRereadsExactSuccess(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	m := NewModule(pool, Secrets{})
	current, err := m.InspectAttestation(ctx)
	if err != nil {
		t.Fatalf("inspect prior attestation: %v", err)
	}
	priorHash, err := AttestationHash(current)
	if err != nil {
		t.Fatalf("hash prior attestation: %v", err)
	}

	m.commitAttestation = func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return errSyntheticCommitResponseLoss
	}

	next, err := m.AttestRuntime(ctx, RuntimeAttestationRequest{
		ExpectedRevision:        current.Revision,
		ExpectedSHA256:          priorHash,
		ActiveBillingDigest:     responseLossDigest("a"),
		ActiveBillingContract:   1,
		RollbackBillingDigest:   responseLossDigest("b"),
		RollbackBillingContract: 1,
		Actor:                   "response-loss-test",
		Reason:                  "committed response was lost",
	})
	if err != nil {
		t.Fatalf("exact requested state at expected revision + 1 must resolve as success: %v", err)
	}
	if next.Revision != current.Revision+1 || next.ActiveBillingDigest != responseLossDigest("a") {
		t.Fatalf("reconciled attestation = %+v", next)
	}
}

func TestRuntimeAttestationCommitFailureClassifiesUnchangedState(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	m := NewModule(pool, Secrets{})
	current, err := m.InspectAttestation(ctx)
	if err != nil {
		t.Fatalf("inspect prior attestation: %v", err)
	}
	priorHash, err := AttestationHash(current)
	if err != nil {
		t.Fatalf("hash prior attestation: %v", err)
	}

	m.commitAttestation = func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Rollback(ctx); err != nil {
			return err
		}
		return errSyntheticCommitResponseLoss
	}

	_, err = m.AttestRuntime(ctx, RuntimeAttestationRequest{
		ExpectedRevision:        current.Revision,
		ExpectedSHA256:          priorHash,
		ActiveBillingDigest:     responseLossDigest("c"),
		ActiveBillingContract:   1,
		RollbackBillingDigest:   responseLossDigest("d"),
		RollbackBillingContract: 1,
		Actor:                   "response-loss-test",
		Reason:                  "commit did not happen",
	})
	if !errors.Is(err, ErrAttestationCommitUnchanged) {
		t.Fatalf("unchanged commit result = %v, want ErrAttestationCommitUnchanged", err)
	}
	after, err := m.InspectAttestation(ctx)
	if err != nil {
		t.Fatalf("inspect unchanged attestation: %v", err)
	}
	if after.Revision != current.Revision {
		t.Errorf("unchanged commit advanced revision from %d to %d", current.Revision, after.Revision)
	}
}

func TestRuntimeAttestationCommitResponseLossClassifiesHigherRevisionStale(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	m := NewModule(pool, Secrets{})
	other := NewModule(pool, Secrets{})
	current, err := m.InspectAttestation(ctx)
	if err != nil {
		t.Fatalf("inspect prior attestation: %v", err)
	}
	priorHash, err := AttestationHash(current)
	if err != nil {
		t.Fatalf("hash prior attestation: %v", err)
	}

	m.commitAttestation = func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		winner, err := other.InspectAttestation(ctx)
		if err != nil {
			return err
		}
		winnerHash, err := AttestationHash(winner)
		if err != nil {
			return err
		}
		_, err = other.AttestRuntime(ctx, RuntimeAttestationRequest{
			ExpectedRevision:        winner.Revision,
			ExpectedSHA256:          winnerHash,
			ActiveBillingDigest:     responseLossDigest("7"),
			ActiveBillingContract:   1,
			RollbackBillingDigest:   responseLossDigest("8"),
			RollbackBillingContract: 1,
			Actor:                   "response-loss-test",
			Reason:                  "independent higher revision",
		})
		if err != nil {
			return err
		}
		return errSyntheticCommitResponseLoss
	}

	_, err = m.AttestRuntime(ctx, RuntimeAttestationRequest{
		ExpectedRevision:        current.Revision,
		ExpectedSHA256:          priorHash,
		ActiveBillingDigest:     responseLossDigest("5"),
		ActiveBillingContract:   1,
		RollbackBillingDigest:   responseLossDigest("6"),
		RollbackBillingContract: 1,
		Actor:                   "response-loss-test",
		Reason:                  "ambiguous transition overtaken by another writer",
	})
	if !errors.Is(err, ErrAttestationCommitUnknown) || !errors.Is(err, ErrStaleAttestation) {
		t.Fatalf("higher-revision response-loss result = %v, want unknown and stale classification", err)
	}
}

func TestRuntimeAttestationConcurrentAbortFenceBothLockOrders(t *testing.T) {
	ctx := context.Background()

	runRace := func(t *testing.T, firstIsFence bool) {
		t.Helper()
		pool := testutil.TestDB(t)
		first := NewModule(pool, Secrets{})
		second := NewModule(pool, Secrets{})
		prior, err := first.InspectAttestation(ctx)
		if err != nil {
			t.Fatalf("inspect prior: %v", err)
		}
		priorHash, err := AttestationHash(prior)
		if err != nil {
			t.Fatalf("hash prior: %v", err)
		}

		fence := RuntimeAttestationRequest{
			ExpectedRevision:        prior.Revision,
			ExpectedSHA256:          priorHash,
			ActiveBillingDigest:     prior.ActiveBillingDigest,
			ActiveBillingContract:   prior.ActiveBillingContract,
			RollbackBillingDigest:   prior.RollbackBillingDigest,
			RollbackBillingContract: prior.RollbackBillingContract,
			Actor:                   "fence-race-test",
			Reason:                  "abort fence",
		}
		forward := RuntimeAttestationRequest{
			ExpectedRevision:        prior.Revision,
			ExpectedSHA256:          priorHash,
			ActiveBillingDigest:     responseLossDigest("e"),
			ActiveBillingContract:   1,
			RollbackBillingDigest:   responseLossDigest("f"),
			RollbackBillingContract: 1,
			Actor:                   "fence-race-test",
			Reason:                  "forward transition",
		}
		firstReq, secondReq := forward, fence
		if firstIsFence {
			firstReq, secondReq = fence, forward
		}

		reachedCommit := make(chan struct{})
		releaseCommit := make(chan struct{})
		first.commitAttestation = func(ctx context.Context, tx pgx.Tx) error {
			close(reachedCommit)
			<-releaseCommit
			return tx.Commit(ctx)
		}

		firstResult := make(chan error, 1)
		go func() {
			_, err := first.AttestRuntime(ctx, firstReq)
			firstResult <- err
		}()
		<-reachedCommit // first writer now holds the attestation row lock

		secondResult := make(chan error, 1)
		go func() {
			_, err := second.AttestRuntime(ctx, secondReq)
			secondResult <- err
		}()
		select {
		case err := <-secondResult:
			close(releaseCommit)
			t.Fatalf("second writer returned before the first released its row lock: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		close(releaseCommit)

		if err := <-firstResult; err != nil {
			t.Fatalf("first writer: %v", err)
		}
		if err := <-secondResult; !errors.Is(err, ErrStaleAttestation) {
			t.Fatalf("second writer = %v, want ErrStaleAttestation", err)
		}

		observed, err := first.InspectAttestation(ctx)
		if err != nil {
			t.Fatalf("inspect winner: %v", err)
		}
		if firstIsFence {
			if observed.Revision != prior.Revision+1 || observed.ActiveBillingDigest != prior.ActiveBillingDigest {
				t.Fatalf("fence-first state = %+v", observed)
			}
			return
		}

		// Forward-first makes the fence stale, so the rollback protocol must
		// restore the prior four fields from the winning forward revision.
		observedHash, err := AttestationHash(observed)
		if err != nil {
			t.Fatalf("hash forward winner: %v", err)
		}
		restore := fence
		restore.ExpectedRevision = observed.Revision
		restore.ExpectedSHA256 = observedHash
		restore.Reason = "restore after forward won"
		restored, err := second.AttestRuntime(ctx, restore)
		if err != nil {
			t.Fatalf("restore prior state: %v", err)
		}
		if restored.Revision != prior.Revision+2 || restored.ActiveBillingDigest != prior.ActiveBillingDigest {
			t.Fatalf("restored state = %+v", restored)
		}
	}

	t.Run("fence first", func(t *testing.T) { runRace(t, true) })
	t.Run("forward first", func(t *testing.T) { runRace(t, false) })
}
