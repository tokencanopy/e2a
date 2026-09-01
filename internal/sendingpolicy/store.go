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
	// ErrAttestationCommitUnchanged means Commit returned an error and the
	// mandatory reread proved the reviewed prior revision/hash are still
	// current. The same forward request is safe to retry if still intended.
	ErrAttestationCommitUnchanged = errors.New("sendingpolicy: attestation commit left state unchanged")
	// ErrAttestationCommitUnknown means Commit returned an error and the
	// mandatory reread could not prove either the exact requested next state or
	// the unchanged prior state. The caller must inspect and fence/restore.
	ErrAttestationCommitUnknown = errors.New("sendingpolicy: attestation commit outcome is unknown")
	// ErrPolicyHashMismatch means the reviewed hash does not describe the
	// policy actually being submitted.
	ErrPolicyHashMismatch = errors.New("sendingpolicy: policy hash mismatch")
	// ErrRegistryConflict means an operator-recipient version already exists
	// with a different key identity or commitment. Append-only history is
	// never rewritten, so this is always zero writes.
	ErrRegistryConflict = errors.New("sendingpolicy: operator recipient registry conflict")
	// ErrOperatorRecipientUnavailable means the policy-selected logical
	// version is absent from either this process's immutable secret map or the
	// permanent registry. Policy activation must fail before writing anything.
	ErrOperatorRecipientUnavailable = errors.New("sendingpolicy: selected operator recipient is unavailable")
	// ErrBillingContractTooLow means a budget-enforcement transition was
	// attempted without both active and rollback billing images attested at
	// sending-protection contract level 1 or higher.
	ErrBillingContractTooLow = errors.New("sendingpolicy: billing contract is too low for budget enforcement")
	// ErrInvalidBillingDigest means an attestation did not name a canonical
	// immutable sha256 image digest. Empty is reserved for contract level 0.
	ErrInvalidBillingDigest = errors.New("sendingpolicy: invalid billing image digest")
	// ErrAlreadyGrandfathered means the one-shot ramp-grandfathering marker
	// already exists. The retry is a documented no-op: it can never widen the
	// grandfathered set, so it performs zero writes.
	ErrAlreadyGrandfathered = errors.New("sendingpolicy: sending domains were already grandfathered")
)

const attestationCommitRecoveryTimeout = 5 * time.Second

// Module is the single concrete owner of sending-protection policy state. Later
// slices expose narrow role interfaces (Gate, FeedbackProcessor, Admin) backed
// by this same object; the Postgres store stays private because there is only
// ever one adapter.
type Module struct {
	pool              *pgxpool.Pool
	secrets           Secrets
	commitAttestation func(context.Context, pgx.Tx) error
}

// NewModule binds the module to a pool and the immutable trust roots parsed at
// startup. It performs no I/O: a server that never reads the policy (self-host
// on config source) must not pay for a query.
func NewModule(pool *pgxpool.Pool, secrets Secrets) *Module {
	return &Module{
		pool:    pool,
		secrets: secrets,
		commitAttestation: func(ctx context.Context, tx pgx.Tx) error {
			return tx.Commit(ctx)
		},
	}
}

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

	// GrandfatherCurrentSendingDomains performs the one-shot phase-3 snapshot
	// in the same transaction as the policy CAS: insert the singleton marker,
	// lock the domains table against concurrent sender transitions, and flip
	// every currently sending-verified, ramp-inactive domain to exempt. A
	// second attempt fails with ErrAlreadyGrandfathered and writes nothing.
	GrandfatherCurrentSendingDomains bool
}

// GrandfatherResult reports what the one-shot snapshot did.
type GrandfatherResult struct {
	DomainsExempted int64
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
	if err := m.requireSelectedOperatorRecipient(ctx, tx, req.Policy.OperatorNoticeRecipientVersion); err != nil {
		return PolicySnapshot{}, err
	}
	if current.Policy.BudgetMode != ModeEnforce && req.Policy.BudgetMode == ModeEnforce {
		attestation, err := scanAttestation(tx.QueryRow(ctx, attestationSelect+" FOR SHARE"))
		if err != nil {
			return PolicySnapshot{}, err
		}
		if attestation.ActiveBillingContract < 1 || attestation.RollbackBillingContract < 1 {
			return PolicySnapshot{}, fmt.Errorf("%w: active=%d rollback=%d",
				ErrBillingContractTooLow,
				attestation.ActiveBillingContract,
				attestation.RollbackBillingContract)
		}
	}

	next := current.Generation + 1
	if req.GrandfatherCurrentSendingDomains {
		if _, err := m.grandfatherLocked(ctx, tx, next, req.Actor); err != nil {
			return PolicySnapshot{}, err
		}
	}
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

// requireSelectedOperatorRecipient proves that the policy's non-secret
// logical version means exactly the same recipient in this process and in the
// append-only registry. The registry row is share-locked for the activation
// transaction even though ordinary application code cannot mutate it; this
// keeps the read/selection boundary explicit and composes with bootstrap or
// repair tooling that might be running concurrently.
func (m *Module) requireSelectedOperatorRecipient(ctx context.Context, tx pgx.Tx, version int) error {
	recipients := m.secrets.Recipients
	if recipients == nil {
		return fmt.Errorf("%w: local operator recipient map is not loaded", ErrOperatorRecipientUnavailable)
	}
	localCommitment, ok := recipients.Commitment(version)
	if !ok {
		return fmt.Errorf("%w: logical version %d is absent from the local map",
			ErrOperatorRecipientUnavailable, version)
	}

	var registeredKeyID, registeredCommitment string
	err := tx.QueryRow(ctx,
		`SELECT commitment_key_id, recipient_commitment
		   FROM sending_operator_recipient_versions
		  WHERE logical_version = $1 FOR SHARE`, version,
	).Scan(&registeredKeyID, &registeredCommitment)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: logical version %d is not registered",
			ErrOperatorRecipientUnavailable, version)
	}
	if err != nil {
		return fmt.Errorf("sendingpolicy: read selected operator recipient version %d: %w", version, err)
	}
	if registeredKeyID != recipients.KeyID() || registeredCommitment != localCommitment {
		return fmt.Errorf("%w: logical version %d does not match the local map",
			ErrRegistryConflict, version)
	}
	return nil
}

// grandfatherLocked runs the one-shot snapshot inside the caller's activation
// transaction, which already holds the policy row lock at the expected
// generation.
//
// Ordering matters and mirrors the design: the marker insert comes first, so
// a replay returns without taking a disruptive domains-table lock. The winning
// transaction then takes SHARE ROW EXCLUSIVE, which conflicts with the ROW
// EXCLUSIVE writes of a concurrent pending->verified sender transition. Such
// a transition linearizes either before this transaction (and is
// grandfathered) or after it (and meets the active ramp as inactive). Only the
// transaction that creates the marker may flip domains, so a retry that finds
// it existing is a no-op that can never widen the set. The check
// deliberately reads sending_status, not domains.verified_at -- verified_at
// records inbound ownership verification, not the later sender transition.
func (m *Module) grandfatherLocked(ctx context.Context, tx pgx.Tx, generation int64, actor string) (GrandfatherResult, error) {
	// The marker records the generation being activated WITH this snapshot and
	// the actor who requested it -- not the outgoing row's generation, whose
	// activated_by may still read 'migration'.
	tag, err := tx.Exec(ctx,
		`INSERT INTO sending_ramp_grandfathering (singleton, policy_generation, completed_at, completed_by)
		 VALUES (true, $1, now(), $2)
		 ON CONFLICT (singleton) DO NOTHING`, generation, actor)
	if err != nil {
		return GrandfatherResult{}, fmt.Errorf("sendingpolicy: insert grandfathering marker: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return GrandfatherResult{}, ErrAlreadyGrandfathered
	}

	if _, err := tx.Exec(ctx, `LOCK TABLE domains IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return GrandfatherResult{}, fmt.Errorf("sendingpolicy: lock domains for grandfathering: %w", err)
	}

	updated, err := tx.Exec(ctx,
		`UPDATE domains SET sending_ramp_status = 'exempt'
		  WHERE sending_status = 'verified' AND sending_ramp_status = 'inactive'`)
	if err != nil {
		return GrandfatherResult{}, fmt.Errorf("sendingpolicy: grandfather verified domains: %w", err)
	}
	return GrandfatherResult{DomainsExempted: updated.RowsAffected()}, nil
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
	if err := validateBillingDigest(a.ActiveBillingDigest, a.ActiveBillingContract); err != nil {
		return RuntimeAttestation{}, fmt.Errorf("sendingpolicy: stored active billing image: %w", err)
	}
	if err := validateBillingDigest(a.RollbackBillingDigest, a.RollbackBillingContract); err != nil {
		return RuntimeAttestation{}, fmt.Errorf("sendingpolicy: stored rollback billing image: %w", err)
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
	if err := validateBillingDigest(req.ActiveBillingDigest, req.ActiveBillingContract); err != nil {
		return RuntimeAttestation{}, fmt.Errorf("active billing image: %w", err)
	}
	if err := validateBillingDigest(req.RollbackBillingDigest, req.RollbackBillingContract); err != nil {
		return RuntimeAttestation{}, fmt.Errorf("rollback billing image: %w", err)
	}
	if !isLowerHexSHA256(req.ExpectedSHA256) {
		return RuntimeAttestation{}, errors.New("sendingpolicy: expected attestation SHA-256 must be 64 lowercase hexadecimal characters")
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
		return RuntimeAttestation{}, fmt.Errorf("%w: stored four-field hash no longer matches the reviewed value",
			ErrStaleAttestation)
	}

	next := current.Revision + 1
	nextHash, err := AttestationHash(RuntimeAttestation{
		ActiveBillingDigest:     req.ActiveBillingDigest,
		ActiveBillingContract:   req.ActiveBillingContract,
		RollbackBillingDigest:   req.RollbackBillingDigest,
		RollbackBillingContract: req.RollbackBillingContract,
	})
	if err != nil {
		return RuntimeAttestation{}, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO sending_protection_runtime_attestation_events
			(revision, prior_revision, prior_attestation_sha256, new_attestation_sha256, actor, reason)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		next, current.Revision, currentHash, nextHash, req.Actor, req.Reason,
	); err != nil {
		return RuntimeAttestation{}, fmt.Errorf("sendingpolicy: record attestation event: %w", err)
	}

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

	if err := m.commitAttestation(ctx, tx); err != nil {
		return m.reconcileAttestationCommit(ctx, req, err)
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

func validateBillingDigest(digest string, contract int) error {
	if digest == "" && contract == 0 {
		return nil
	}
	if len(digest) != len("sha256:")+64 || !strings.HasPrefix(digest, "sha256:") || !isLowerHexSHA256(digest[len("sha256:"):]) {
		return ErrInvalidBillingDigest
	}
	return nil
}

func (m *Module) reconcileAttestationCommit(ctx context.Context, req RuntimeAttestationRequest, commitErr error) (RuntimeAttestation, error) {
	// A commit can become ambiguous precisely because the request deadline was
	// exceeded or its caller disconnected. Recovery must therefore outlive that
	// cancellation, while remaining tightly bounded so this operator command
	// cannot hang indefinitely on a broken database connection.
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), attestationCommitRecoveryTimeout)
	defer cancel()

	observed, err := m.InspectAttestation(recoveryCtx)
	if err != nil {
		return RuntimeAttestation{}, fmt.Errorf("%w: commit returned an error and reread failed: %v", ErrAttestationCommitUnknown, err)
	}

	if observed.Revision == req.ExpectedRevision+1 &&
		observed.ActiveBillingDigest == req.ActiveBillingDigest &&
		observed.ActiveBillingContract == req.ActiveBillingContract &&
		observed.RollbackBillingDigest == req.RollbackBillingDigest &&
		observed.RollbackBillingContract == req.RollbackBillingContract {
		return observed, nil
	}

	observedHash, err := AttestationHash(observed)
	if err != nil {
		return RuntimeAttestation{}, fmt.Errorf("%w: commit returned an error and reread hashing failed: %v", ErrAttestationCommitUnknown, err)
	}
	if observed.Revision == req.ExpectedRevision && observedHash == req.ExpectedSHA256 {
		return RuntimeAttestation{}, fmt.Errorf("%w: commit returned: %v", ErrAttestationCommitUnchanged, commitErr)
	}
	return RuntimeAttestation{}, fmt.Errorf("%w: %w: reread found revision %d",
		ErrAttestationCommitUnknown, ErrStaleAttestation, observed.Revision)
}

func isLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// RegisterOperatorRecipients inserts registry rows for versions that are not
// yet recorded, and refuses any disagreement with history.
//
// The table's triggers already make UPDATE and DELETE impossible, so the only
// way to be wrong here is to insert a row that contradicts an existing one.
// That is checked explicitly and fails the whole transaction, because a partial
// registration would leave the operator unable to tell which versions are
// trustworthy.
func (m *Module) RegisterOperatorRecipients(ctx context.Context, actor, reason string) (inserted []int, err error) {
	if strings.TrimSpace(actor) == "" {
		return nil, errors.New("sendingpolicy: registration requires an actor")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, errors.New("sendingpolicy: registration requires a reason")
	}
	recipients := m.secrets.Recipients
	if recipients == nil {
		return nil, errors.New("sendingpolicy: registration requires the local operator recipient map")
	}

	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("sendingpolicy: begin registration: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	keyID := recipients.KeyID()
	for _, version := range recipients.Versions() {
		commitment, _ := recipients.Commitment(version)

		var insertedVersion int
		err := tx.QueryRow(ctx,
			`INSERT INTO sending_operator_recipient_versions
				(logical_version, commitment_key_id, recipient_commitment, created_by, reason)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (logical_version) DO NOTHING
			 RETURNING logical_version`,
			version, keyID, commitment, actor, reason,
		).Scan(&insertedVersion)
		if err == nil {
			inserted = append(inserted, version)
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("sendingpolicy: register version %d: %w", version, err)
		}

		// A serial or concurrent replay lost ON CONFLICT. Lock and verify the
		// winner rather than surfacing a unique violation or trusting that the
		// logical version means the same recipient.
		var existingKeyID, existingCommitment string
		if err := tx.QueryRow(ctx,
			`SELECT commitment_key_id, recipient_commitment
			   FROM sending_operator_recipient_versions
			  WHERE logical_version = $1 FOR SHARE`, version,
		).Scan(&existingKeyID, &existingCommitment); err != nil {
			return nil, fmt.Errorf("sendingpolicy: read registry version %d after conflict: %w", version, err)
		}
		if existingKeyID != keyID {
			return nil, fmt.Errorf("%w: version %d was registered under a different commitment key", ErrRegistryConflict, version)
		}
		if existingCommitment != commitment {
			return nil, fmt.Errorf("%w: version %d was registered with a different recipient commitment", ErrRegistryConflict, version)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("sendingpolicy: commit registration: %w", err)
	}
	return inserted, nil
}
