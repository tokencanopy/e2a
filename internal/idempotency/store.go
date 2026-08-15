// Package idempotency implements the storage backing the
// Idempotency-Key header on the outbound send endpoints.
//
// Stripe-style semantics:
//
//   - Caller sends `Idempotency-Key: <string>` on a POST that has a
//     side-effect (creating an outbound email).
//   - Server scopes the key by (user_id, key) and remembers it for
//     [TTL].
//   - Same key + same request body hash → replay the cached response;
//     do NOT redo the side effect.
//   - Same key + different request body hash → 422 (mismatch).
//   - Same key, prior request still in-flight → 409.
//
// Why (user_id, key) and not (api_key_id, key): the request path's
// authenticator (internal/agent.API.authenticateUser) returns only the
// owning user, and threading the credential id through every handler
// is invasive. UUIDv4 collisions across one user's keys are
// mathematically negligible, and the body-hash check catches the
// pathological collision case explicitly with a 422. Stripe scopes
// at the account level for the same reason.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxKeyLength caps Idempotency-Key header values. Past this the
// header is rejected at the request boundary (400). Matches the
// upper-bound Stripe documents.
const MaxKeyLength = 255

// TTL is how long a completed row stays cached (and continues
// rejecting body-mismatched replays) before the sweep removes it.
const TTL = 24 * time.Hour

// StaleClaimWindow is the age past which an in_progress row is
// treated as the leftover of a crashed handler and may be taken over
// by the next caller. Bounded above by SMTPRelay's worst-case retry
// envelope (~6.5min) but kept tighter so legitimate stalls produce a
// loud 409 to the second caller rather than silently double-sending.
const StaleClaimWindow = 5 * time.Minute

// SweepInterval is the cadence the cmd/e2a hourly cleanup loop uses
// when invoking Sweep. Exposed as a constant so the loop and the
// docstring stay consistent.
const SweepInterval = 1 * time.Hour

// ClaimOutcome describes what happened when a caller tried to claim
// (user_id, key) for a fresh request.
type ClaimOutcome int

const (
	// OutcomeAcquired — caller owns the row and MUST follow up with
	// Complete (success path) or Release (caller-side abort that
	// did not produce a side-effect, e.g. early validation failure).
	OutcomeAcquired ClaimOutcome = iota
	// OutcomeReplay — a previous request with this key completed
	// successfully and the body hash matches. Serve the cached
	// response verbatim.
	OutcomeReplay
	// OutcomeMismatch — a previous completed request used this key
	// with a different body. Refuse with 422.
	OutcomeMismatch
	// OutcomeInFlight — a concurrent request with this key is still
	// being processed (and the row is not stale). Refuse with 409.
	OutcomeInFlight
)

// CachedResponse is what the server replays for an OutcomeReplay.
// Content-Type is captured separately so the replay is wire-faithful.
type CachedResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

// ClaimResult bundles the outcome with the cached response when the
// outcome is OutcomeReplay. Other outcomes leave Cached zero-valued.
type ClaimResult struct {
	Outcome ClaimOutcome
	Cached  CachedResponse
	// Token fences this ownership generation. A stale takeover receives a new
	// token; delayed prior handlers cannot complete or release the new claim.
	Token ClaimToken
}

// ClaimToken is the database-authored generation for one acquired claim.
// created_at changes on every stale takeover and needs no schema expansion.
type ClaimToken struct{ createdAt time.Time }

// ErrClaimLost means this handler no longer owns the in-progress generation.
// Transactional callers must propagate it so their side effect rolls back.
var ErrClaimLost = errors.New("idempotency: claim ownership lost")

// Store is the postgres-backed idempotency store.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a pgx pool. The pool must already have the schema
// from migrations/015_idempotency_and_send_attempts.sql applied.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// HashBody returns the sha256 hex of the raw request body. Exposed so
// callers (the HTTP layer) can compute it once from the bytes they
// already read, rather than re-reading or re-encoding.
func HashBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// HashRequest mixes the request path into the body hash so the same
// Idempotency-Key cannot accidentally replay a cached response across
// different routes (e.g. reusing a key set on a reply to message A
// while replying to message B). The path is included on its own line
// before the body so the hash is unambiguous about where path ends
// and body begins.
//
// Callers compute this once with the raw body bytes they already
// have and pass the result to Store.Claim.
func HashRequest(path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(path))
	h.Write([]byte{'\n'})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// Claim atomically reserves (userID, key) for the caller. The outcome
// determines what the HTTP layer should do next:
//
//   - OutcomeAcquired → run the side effect, then Complete on success
//     or Release on caller-side abort.
//   - OutcomeReplay   → write Cached to the response, do NOT re-run
//     the side effect.
//   - OutcomeMismatch → respond 422.
//   - OutcomeInFlight → respond 409.
//
// path (the request_path column) is stored for diagnostics ONLY — it is
// not part of the unique (user_id, key) constraint. Cross-route separation
// is therefore NOT enforced by the store; it comes from callers folding the
// route into bodyHash via HashRequest (so the same key on a different route
// yields a different hash → OutcomeMismatch/422). A caller that passes a raw
// HashBody instead of HashRequest would silently replay across routes — pass
// HashRequest. (Stripe likewise 422s key reuse with a different request,
// surfacing caller bugs faster than a silent allow.)
//
// Concurrency: the claim is decided in a single UPSERT with
// RETURNING. Two concurrent callers can never both observe
// OutcomeAcquired because only one of the racing statements has its
// row materialized in RETURNING — the other either hits the conflict
// path (DO UPDATE WHERE not stale → row skipped in RETURNING) or, if
// the row was stale, the takeover serializes on the unique-index
// lock and the loser sees a non-stale in_progress on its follow-up
// read.
func (s *Store) Claim(ctx context.Context, userID, key, path, bodyHash string) (ClaimResult, error) {
	if userID == "" {
		return ClaimResult{}, errors.New("idempotency: userID required")
	}
	if key == "" {
		return ClaimResult{}, errors.New("idempotency: key required")
	}

	staleSecs := int(StaleClaimWindow.Seconds())

	// Atomic claim path. Returns a row iff we own the slot — either as
	// a fresh INSERT, or as a stale-takeover where the DO UPDATE's
	// WHERE clause is true. Returns no rows when an existing row
	// blocks us (completed, or in_progress but not yet stale), in
	// which case we read the existing row to classify the outcome.
	var claimedAt time.Time
	err := s.pool.QueryRow(ctx,
		`INSERT INTO idempotency_keys (
		     user_id, key, request_path, request_body_hash,
		     response_status, response_content_type, response_body,
		     status, created_at, completed_at
		 )
		 VALUES ($1, $2, $3, $4, 0, '', ''::bytea, 'in_progress', now(), NULL)
		 ON CONFLICT (user_id, key) DO UPDATE
		    SET request_path          = EXCLUDED.request_path,
		        request_body_hash     = EXCLUDED.request_body_hash,
		        response_status       = 0,
		        response_content_type = '',
		        response_body         = ''::bytea,
		        status                = 'in_progress',
		        created_at            = clock_timestamp(),
		        completed_at          = NULL
		  WHERE idempotency_keys.status = 'in_progress'
		    AND idempotency_keys.request_body_hash = EXCLUDED.request_body_hash
		    AND idempotency_keys.created_at < now() - make_interval(secs => $5)
		 RETURNING created_at`,
		userID, key, path, bodyHash, staleSecs,
	).Scan(&claimedAt)
	if err == nil {
		return ClaimResult{Outcome: OutcomeAcquired, Token: ClaimToken{createdAt: claimedAt}}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ClaimResult{}, err
	}

	// Lost the race. Read the existing row to classify.
	var (
		gotStatus string
		gotHash   string
		gotCode   int
		gotCT     string
		gotBody   []byte
	)
	err = s.pool.QueryRow(ctx,
		`SELECT status, request_body_hash, response_status,
		        response_content_type, response_body
		   FROM idempotency_keys
		  WHERE user_id = $1 AND key = $2`,
		userID, key,
	).Scan(&gotStatus, &gotHash, &gotCode, &gotCT, &gotBody)
	if err != nil {
		// The row has to exist — we just lost a conflict against it.
		// If pgx.ErrNoRows fires here it means the row was swept
		// between the UPSERT and this SELECT (a 24-hour-old row
		// becoming sweep-eligible mid-call, vanishingly unlikely);
		// treat that as transient and let the caller retry.
		return ClaimResult{}, err
	}

	if gotHash != bodyHash {
		return ClaimResult{Outcome: OutcomeMismatch}, nil
	}
	if gotStatus == "in_progress" {
		return ClaimResult{Outcome: OutcomeInFlight}, nil
	}
	// gotStatus == "completed"
	return ClaimResult{
		Outcome: OutcomeReplay,
		Cached: CachedResponse{
			StatusCode:  gotCode,
			ContentType: gotCT,
			Body:        gotBody,
		},
	}, nil
}

// Complete records the response for an OutcomeAcquired claim. After
// this point any subsequent Claim with the same key either replays
// (body matches) or 422s (body differs), until the row is swept.
//
// Fenced against double-call and stale owners: only the currently acquired
// in-progress generation can complete. ErrClaimLost tells transactional
// callers to roll back their side effect.
func (s *Store) Complete(ctx context.Context, userID, key string, token ClaimToken, resp CachedResponse) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE idempotency_keys
		    SET status                = 'completed',
		        response_status       = $4,
		        response_content_type = $5,
		        response_body         = $6,
		        completed_at          = now()
		  WHERE user_id = $1 AND key = $2 AND created_at = $3 AND status = 'in_progress'`,
		userID, key, token.createdAt, resp.StatusCode, resp.ContentType, resp.Body,
	)
	return claimMutationResult(tag.RowsAffected(), err)
}

// CompleteTx is Complete on the caller's transaction — for the async accept path
// (async-message-pipeline.md, slice C), which commits the idempotency-key completion
// atomically with the message-row insert + send-job enqueue. Committing the
// completion IN the accept-tx closes the crash window that Complete (run AFTER the
// side effect) leaves open: if the process dies after the accept-tx commits but
// before a post-hoc Complete, the key stays in_progress and a retry past the stale
// window re-runs the send. With CompleteTx the key is 'completed' the instant the
// message is durable, so every retry replays. A later post-hoc Complete returns
// ErrClaimLost; the generic HTTP wrapper deliberately ignores that expected
// result because the transaction already persisted the response.
func (s *Store) CompleteTx(ctx context.Context, tx pgx.Tx, userID, key string, token ClaimToken, resp CachedResponse) error {
	tag, err := tx.Exec(ctx,
		`UPDATE idempotency_keys
		    SET status                = 'completed',
		        response_status       = $4,
		        response_content_type = $5,
		        response_body         = $6,
		        completed_at          = now()
		  WHERE user_id = $1 AND key = $2 AND created_at = $3 AND status = 'in_progress'`,
		userID, key, token.createdAt, resp.StatusCode, resp.ContentType, resp.Body,
	)
	return claimMutationResult(tag.RowsAffected(), err)
}

// Release drops an OutcomeAcquired claim without recording a response.
// Use when the caller decided not to perform the side effect (e.g.
// the request failed validation before any external work happened),
// so the next caller with the same key can try again with a fresh
// payload rather than getting OutcomeMismatch on the second attempt.
//
// Only the current in-progress generation can be deleted. A stale owner gets
// ErrClaimLost instead of deleting a replacement claim.
func (s *Store) Release(ctx context.Context, userID, key string, token ClaimToken) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM idempotency_keys
		  WHERE user_id = $1 AND key = $2 AND created_at = $3 AND status = 'in_progress'`,
		userID, key, token.createdAt,
	)
	return claimMutationResult(tag.RowsAffected(), err)
}

func claimMutationResult(rows int64, err error) error {
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrClaimLost
	}
	return nil
}

// Sweep removes completed rows older than TTL. Returns the count
// deleted. Wire this into the cmd/e2a hourly cleanup loop.
//
// Does NOT delete in_progress rows past StaleClaimWindow on its own —
// those are taken over by the next Claim via the UPSERT WHERE clause,
// which keeps the takeover path concentrated in one place (the
// concurrency model lives in Claim, not split across two functions).
// expiredDeleteBatch bounds one DELETE in the batched sweep — idempotency_keys scales
// with request volume, so the janitor prunes it in ctid-bounded chunks to keep each
// statement's lock/WAL small. Caller's ctx bounds total runtime; a partial sweep
// resumes next hour (idempotent).
const expiredDeleteBatch = 5000

func (s *Store) Sweep(ctx context.Context) (int64, error) {
	ttlSecs := int(TTL.Seconds())
	var total int64
	for {
		tag, err := s.pool.Exec(ctx,
			`DELETE FROM idempotency_keys WHERE ctid IN (
			   SELECT ctid FROM idempotency_keys
			    WHERE status = 'completed' AND created_at < now() - make_interval(secs => $2) LIMIT $1)`,
			expiredDeleteBatch, ttlSecs)
		if err != nil {
			return total, err
		}
		n := tag.RowsAffected()
		total += n
		if n < expiredDeleteBatch {
			return total, nil
		}
	}
}
