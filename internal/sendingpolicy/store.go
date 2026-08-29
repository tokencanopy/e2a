package sendingpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors. Callers distinguish "you lost a race, re-read and decide"
// from "your request was malformed", because the correct operator response to
// each is different: one is a retry, the other is a fix.
var (
	// ErrStaleGeneration means the stored policy generation moved between the
	// operator reading it and this write. Zero rows were written.
	ErrStaleGeneration = errors.New("sendingpolicy: stale policy generation")
	// ErrStaleAttestation means the attestation revision or its four-field
	// hash did not match what the caller expected. Zero rows were written.
	ErrStaleAttestation = errors.New("sendingpolicy: stale runtime attestation")
	// ErrPolicyHashMismatch means the reviewed hash does not describe the
	// policy actually being submitted.
	ErrPolicyHashMismatch = errors.New("sendingpolicy: policy hash mismatch")
	// ErrRegistryConflict means an operator-recipient version already exists
	// with a different key identity or commitment. Append-only history is
	// never rewritten, so this is always zero writes.
	ErrRegistryConflict = errors.New("sendingpolicy: operator recipient registry conflict")
)

// Module is the single concrete owner of sending-protection policy state. Later
// slices expose narrow role interfaces (Gate, FeedbackProcessor, Admin) backed
// by this same object; the Postgres store stays private because there is only
// ever one adapter.
type Module struct {
	pool *pgxpool.Pool
}

// NewModule binds the module to a pool. It performs no I/O: a server that never
// reads the policy (self-host on config source) must not pay for a query.
func NewModule(pool *pgxpool.Pool) *Module { return &Module{pool: pool} }

// PolicySnapshot is the stored policy plus the metadata an operator needs to
// express the next compare-and-swap.
type PolicySnapshot struct {
	Generation    int64
	SchemaVersion int
	Policy        RuntimePolicy
	PolicySHA256  string
	ActivatedAt   time.Time
	ActivatedBy   string
}

// ActivationRequest is one reviewed policy change.
type ActivationRequest struct {
	ExpectedGeneration int64
	Policy             RuntimePolicy
	Actor              string
	Reason             string
}

// RuntimeAttestation records which billing images the deployment has verified,
// so a policy activation cannot outrun the artifacts that support it.
type RuntimeAttestation struct {
	Revision                int64
	ActiveBillingDigest     string
	ActiveBillingContract   int
	RollbackBillingDigest   string
	RollbackBillingContract int
	UpdatedAt               time.Time
	UpdatedBy               string
}

// RuntimeAttestationRequest is a CAS over both the revision and the canonical
// hash of the four fields being replaced.
type RuntimeAttestationRequest struct {
	ExpectedRevision int64
	ExpectedSHA256   string

	ActiveBillingDigest     string
	ActiveBillingContract   int
	RollbackBillingDigest   string
	RollbackBillingContract int

	Actor  string
	Reason string
}

// attestationFields is the canonical hash preimage.
//
// The spec pins that the CAS hashes "the canonical four digest/contract
// fields" without fixing their byte layout, so this struct IS that definition:
// RFC 8785 over exactly these four keys, hashed with SHA-256. It deliberately
// reuses the policy canonicalizer rather than inventing a second construction,
// because the OSS operator command and the ops-side deploy gate must compute
// identical values from identical state.
type attestationFields struct {
	ActiveBillingContract   int    `json:"active_billing_contract"`
	ActiveBillingDigest     string `json:"active_billing_digest"`
	RollbackBillingContract int    `json:"rollback_billing_contract"`
	RollbackBillingDigest   string `json:"rollback_billing_digest"`
}

// AttestationHash is the lowercase SHA-256 an operator passes back as
// -expected-attestation-sha256. It covers only the four state fields, never the
// revision: the revision is compared separately and explicitly.
func AttestationHash(a RuntimeAttestation) (string, error) {
	raw, err := json.Marshal(attestationFields{
		ActiveBillingContract:   a.ActiveBillingContract,
		ActiveBillingDigest:     a.ActiveBillingDigest,
		RollbackBillingContract: a.RollbackBillingContract,
		RollbackBillingDigest:   a.RollbackBillingDigest,
	})
	if err != nil {
		return "", fmt.Errorf("sendingpolicy: marshal attestation: %w", err)
	}
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

const policySelect = `SELECT generation, schema_version, policy, policy_sha256, activated_at, activated_by
	FROM sending_protection_runtime_policy WHERE singleton`

// InspectPolicy reads the current policy without locking. It is the readback
// half of every operator command and the dry-run of an activation.
func (m *Module) InspectPolicy(ctx context.Context) (PolicySnapshot, error) {
	return scanPolicy(m.pool.QueryRow(ctx, policySelect))
}

// currentPolicyForShare reads the policy inside a caller's transaction under a
// share lock. Provider authorization uses this so a concurrent activation
// cannot change the policy midway through a decision, while still permitting
// other readers.
func (m *Module) currentPolicyForShare(ctx context.Context, tx pgx.Tx) (RuntimePolicy, error) {
	snapshot, err := scanPolicy(tx.QueryRow(ctx, policySelect+" FOR SHARE"))
	if err != nil {
		return RuntimePolicy{}, err
	}
	return snapshot.Policy, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPolicy(row rowScanner) (PolicySnapshot, error) {
	var (
		s   PolicySnapshot
		raw []byte
	)
	if err := row.Scan(&s.Generation, &s.SchemaVersion, &raw, &s.PolicySHA256, &s.ActivatedAt, &s.ActivatedBy); err != nil {
		return PolicySnapshot{}, fmt.Errorf("sendingpolicy: read runtime policy: %w", err)
	}
	if s.SchemaVersion != SchemaVersion {
		return PolicySnapshot{}, fmt.Errorf("sendingpolicy: stored policy schema version %d is not supported by this binary (%d)",
			s.SchemaVersion, SchemaVersion)
	}

	// The stored bytes are re-canonicalized and re-hashed rather than trusted.
	// Postgres normalizes jsonb, so the column does not necessarily return the
	// exact bytes that were written; deriving the hash from the canonical form
	// of what we actually read is what makes a drifted row detectable.
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return PolicySnapshot{}, err
	}
	if got := HashBytes(canonical); got != s.PolicySHA256 {
		return PolicySnapshot{}, fmt.Errorf("sendingpolicy: stored policy hash %s does not match its content", s.PolicySHA256)
	}

	policy, err := ParsePolicy(canonical)
	if err != nil {
		return PolicySnapshot{}, err
	}
	s.Policy = policy
	return s, nil
}

// ActivatePolicy advances the policy by exactly one generation, under a
// compare-and-swap on the generation the operator reviewed.
//
// Everything happens in one transaction holding the singleton's row lock, so a
// concurrent activation either waits and then loses on generation, or wins and
// makes this one lose. There is no path that writes the policy without also
// writing its audit event.
func (m *Module) ActivatePolicy(ctx context.Context, req ActivationRequest) (PolicySnapshot, error) {
	if strings.TrimSpace(req.Actor) == "" {
		return PolicySnapshot{}, errors.New("sendingpolicy: activation requires an actor")
	}
	if strings.TrimSpace(req.Reason) == "" {
		return PolicySnapshot{}, errors.New("sendingpolicy: activation requires a reason")
	}
	if err := req.Policy.Validate(); err != nil {
		return PolicySnapshot{}, err
	}

	canonical, err := canonicalPolicyBytes(req.Policy)
	if err != nil {
		return PolicySnapshot{}, err
	}
	newHash := HashBytes(canonical)

	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return PolicySnapshot{}, fmt.Errorf("sendingpolicy: begin activation: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	current, err := scanPolicy(tx.QueryRow(ctx, policySelect+" FOR UPDATE"))
	if err != nil {
		return PolicySnapshot{}, err
	}
	if current.Generation != req.ExpectedGeneration {
		return PolicySnapshot{}, fmt.Errorf("%w: stored generation is %d, expected %d",
			ErrStaleGeneration, current.Generation, req.ExpectedGeneration)
	}

	next := current.Generation + 1
	if _, err := tx.Exec(ctx,
		`INSERT INTO sending_protection_policy_events
			(generation, prior_generation, prior_policy_sha256, new_policy_sha256, actor, reason)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		next, current.Generation, current.PolicySHA256, newHash, req.Actor, req.Reason,
	); err != nil {
		return PolicySnapshot{}, fmt.Errorf("sendingpolicy: record policy event: %w", err)
	}

	var activatedAt time.Time
	if err := tx.QueryRow(ctx,
		`UPDATE sending_protection_runtime_policy
		    SET generation = $1, schema_version = $2, policy = $3, policy_sha256 = $4,
		        activated_at = now(), activated_by = $5
		  WHERE singleton
		  RETURNING activated_at`,
		next, SchemaVersion, canonical, newHash, req.Actor,
	).Scan(&activatedAt); err != nil {
		return PolicySnapshot{}, fmt.Errorf("sendingpolicy: write runtime policy: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return PolicySnapshot{}, fmt.Errorf("sendingpolicy: commit activation: %w", err)
	}

	return PolicySnapshot{
		Generation:    next,
		SchemaVersion: SchemaVersion,
		Policy:        req.Policy.normalized(),
		PolicySHA256:  newHash,
		ActivatedAt:   activatedAt,
		ActivatedBy:   req.Actor,
	}, nil
}

const attestationSelect = `SELECT revision, active_billing_digest, active_billing_contract,
	rollback_billing_digest, rollback_billing_contract, updated_at, updated_by
	FROM sending_protection_runtime_attestation WHERE singleton`

// InspectAttestation reads the current runtime attestation.
func (m *Module) InspectAttestation(ctx context.Context) (RuntimeAttestation, error) {
	return scanAttestation(m.pool.QueryRow(ctx, attestationSelect))
}

func scanAttestation(row rowScanner) (RuntimeAttestation, error) {
	var a RuntimeAttestation
	if err := row.Scan(&a.Revision, &a.ActiveBillingDigest, &a.ActiveBillingContract,
		&a.RollbackBillingDigest, &a.RollbackBillingContract, &a.UpdatedAt, &a.UpdatedBy); err != nil {
		return RuntimeAttestation{}, fmt.Errorf("sendingpolicy: read runtime attestation: %w", err)
	}
	return a, nil
}

// AttestRuntime compare-and-swaps the billing attestation on both the expected
// revision and the canonical hash of the four fields it replaces.
//
// Requiring both is what defeats an ABA retry: a delayed writer whose four
// expected fields happen to match a later state is still rejected, because the
// revision advanced past it. A higher revision is always stale, never a
// candidate for replacement.
func (m *Module) AttestRuntime(ctx context.Context, req RuntimeAttestationRequest) (RuntimeAttestation, error) {
	if strings.TrimSpace(req.Actor) == "" {
		return RuntimeAttestation{}, errors.New("sendingpolicy: attestation requires an actor")
	}
	if strings.TrimSpace(req.Reason) == "" {
		return RuntimeAttestation{}, errors.New("sendingpolicy: attestation requires a reason")
	}
	if req.ActiveBillingContract < 0 || req.RollbackBillingContract < 0 {
		return RuntimeAttestation{}, errors.New("sendingpolicy: billing contract levels must be non-negative")
	}

	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RuntimeAttestation{}, fmt.Errorf("sendingpolicy: begin attestation: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	current, err := scanAttestation(tx.QueryRow(ctx, attestationSelect+" FOR UPDATE"))
	if err != nil {
		return RuntimeAttestation{}, err
	}
	if current.Revision != req.ExpectedRevision {
		return RuntimeAttestation{}, fmt.Errorf("%w: stored revision is %d, expected %d",
			ErrStaleAttestation, current.Revision, req.ExpectedRevision)
	}
	currentHash, err := AttestationHash(current)
	if err != nil {
		return RuntimeAttestation{}, err
	}
	if currentHash != req.ExpectedSHA256 {
		return RuntimeAttestation{}, fmt.Errorf("%w: stored four-field hash is %s, expected %s",
			ErrStaleAttestation, currentHash, req.ExpectedSHA256)
	}

	next := current.Revision + 1
	var updatedAt time.Time
	if err := tx.QueryRow(ctx,
		`UPDATE sending_protection_runtime_attestation
		    SET revision = $1, active_billing_digest = $2, active_billing_contract = $3,
		        rollback_billing_digest = $4, rollback_billing_contract = $5,
		        updated_at = now(), updated_by = $6
		  WHERE singleton
		  RETURNING updated_at`,
		next, req.ActiveBillingDigest, req.ActiveBillingContract,
		req.RollbackBillingDigest, req.RollbackBillingContract, req.Actor,
	).Scan(&updatedAt); err != nil {
		return RuntimeAttestation{}, fmt.Errorf("sendingpolicy: write runtime attestation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return RuntimeAttestation{}, fmt.Errorf("sendingpolicy: commit attestation: %w", err)
	}

	return RuntimeAttestation{
		Revision:                next,
		ActiveBillingDigest:     req.ActiveBillingDigest,
		ActiveBillingContract:   req.ActiveBillingContract,
		RollbackBillingDigest:   req.RollbackBillingDigest,
		RollbackBillingContract: req.RollbackBillingContract,
		UpdatedAt:               updatedAt,
		UpdatedBy:               req.Actor,
	}, nil
}

// RegisterOperatorRecipients inserts registry rows for versions that are not
// yet recorded, and refuses any disagreement with history.
//
// The table's triggers already make UPDATE and DELETE impossible, so the only
// way to be wrong here is to insert a row that contradicts an existing one.
// That is checked explicitly and fails the whole transaction, because a partial
// registration would leave the operator unable to tell which versions are
// trustworthy.
func (m *Module) RegisterOperatorRecipients(ctx context.Context, recipients *OperatorRecipients, actor string) (inserted []int, err error) {
	if strings.TrimSpace(actor) == "" {
		return nil, errors.New("sendingpolicy: registration requires an actor")
	}

	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("sendingpolicy: begin registration: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	keyID := recipients.KeyID()
	for _, version := range recipients.Versions() {
		commitment, _ := recipients.Commitment(version)

		var existingKeyID, existingCommitment string
		err := tx.QueryRow(ctx,
			`SELECT commitment_key_id, recipient_commitment
			   FROM sending_operator_recipient_versions
			  WHERE logical_version = $1 FOR SHARE`, version,
		).Scan(&existingKeyID, &existingCommitment)

		switch {
		case err == nil:
			// Already registered: it must agree exactly, or the local map and
			// recorded history describe different recipients.
			if existingKeyID != keyID {
				return nil, fmt.Errorf("%w: version %d was registered under a different commitment key", ErrRegistryConflict, version)
			}
			if existingCommitment != commitment {
				return nil, fmt.Errorf("%w: version %d was registered with a different recipient commitment", ErrRegistryConflict, version)
			}
			continue
		case errors.Is(err, pgx.ErrNoRows):
			if _, err := tx.Exec(ctx,
				`INSERT INTO sending_operator_recipient_versions
					(logical_version, commitment_key_id, recipient_commitment, created_by)
				 VALUES ($1, $2, $3, $4)`,
				version, keyID, commitment, actor,
			); err != nil {
				return nil, fmt.Errorf("sendingpolicy: register version %d: %w", version, err)
			}
			inserted = append(inserted, version)
		default:
			return nil, fmt.Errorf("sendingpolicy: read registry version %d: %w", version, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("sendingpolicy: commit registration: %w", err)
	}
	return inserted, nil
}
