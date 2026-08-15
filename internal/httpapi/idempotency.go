package httpapi

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/idempotency"
)

// IdemStore is the subset of *idempotency.Store the v1 layer needs. Declared
// as an interface so handlers are unit-testable without Postgres.
type IdemStore interface {
	Claim(ctx context.Context, userID, key, path, bodyHash string) (idempotency.ClaimResult, error)
	Complete(ctx context.Context, userID, key string, resp idempotency.CachedResponse) error
	// CompleteTx records the completion inside the caller's transaction — used by
	// the async accept path to commit the idempotency key atomically with the
	// message insert + send-job enqueue. See idempotency.Store.CompleteTx.
	CompleteTx(ctx context.Context, tx pgx.Tx, userID, key string, resp idempotency.CachedResponse) error
	Release(ctx context.Context, userID, key string) error
}

// Idempotency-Key namespaces. The store is keyed on a flat (user_id, key),
// so a server-minted automatic key (e.g. event redeliver) and a caller-
// supplied `Idempotency-Key` header must occupy DISJOINT key spaces. Without
// this separation a client could send with `Idempotency-Key: replay:evt_x:`
// and poison (422) a later genuine redelivery of evt_x that mints the same
// raw key internally. We prefix both so the two can never collide.
const (
	idemUserNS = "u:" // caller-supplied Idempotency-Key header (untrusted)
	idemAutoNS = "s:" // server-minted automatic key (never from client input)
)

// runIdempotent executes fn under a caller-supplied `Idempotency-Key` header
// and returns the (status, body) to emit. It is the one place the v1 write
// endpoints (send/reply/forward/approve/rotate-secret/create-api-key) get
// retry-safety, replacing the legacy capturingWriter (which doesn't fit
// Huma's return-value handler model).
//
// Semantics (api-v1-redesign §4 decision 8):
//   - No key, or no store wired → just run fn (idempotency is opt-in).
//   - Dedup key = (principal, route, body-hash). The body hash is over the
//     RAW request bytes (route + "\n" + body; see idempotency.HashRequest) —
//     NOT canonicalized JSON — and is load-bearing: the same key with a
//     *different* body is a 422, never a silent replay of the first response.
//     A retry must therefore resend byte-identical JSON or it 422s.
//   - Replay → the cached response, byte-faithful (unmarshaled back into T).
//   - In-flight → 409; mismatch → 422.
//   - Crash/panic safety: a panic between claim and completion releases the
//     key (below) so retries aren't 409-locked for the stale window. The generic
//     post-commit path is therefore at-least-once across a process crash. A
//     handler that needs a stronger boundary may call CompleteTx from fn's
//     side-effect transaction; domain DELETE does this to bind the key to the
//     deleted registration's incarnation atomically.
//
// fn's contract: return a non-nil error ONLY before any irreversible side
// effect (so the key is released and a retry can proceed); once the side
// effect commits, return success with the final response so it is cached and
// a retry replays it instead of re-doing the side effect. Transactional callers
// may have already completed the key; the final Complete below is then an
// idempotent no-op.
func runIdempotent[T any](s *Server, ctx context.Context, userID, key, route string, rawBody []byte, fn func() (int, T, error)) (int, T, error) {
	return runIdempotentNS(s, ctx, userID, idemUserNS, key, route, rawBody, fn)
}

// runIdempotentAuto is runIdempotent for SERVER-MINTED keys (never derived
// from client input), kept in a namespace disjoint from caller `Idempotency-
// Key` headers so the two can't collide in the flat (user_id, key) store.
func runIdempotentAuto[T any](s *Server, ctx context.Context, userID, key, route string, rawBody []byte, fn func() (int, T, error)) (int, T, error) {
	return runIdempotentNS(s, ctx, userID, idemAutoNS, key, route, rawBody, fn)
}

func runIdempotentNS[T any](s *Server, ctx context.Context, userID, ns, key, route string, rawBody []byte, fn func() (int, T, error)) (int, T, error) {
	var zero T
	if key != "" && (strings.TrimSpace(key) == "" || len(key) > idempotency.MaxKeyLength) {
		return 0, zero, NewError(400, "invalid_request", "Idempotency-Key must be non-blank and at most 255 bytes")
	}
	if key == "" || s.deps.Idempotency == nil {
		return fn()
	}
	nsKey := ns + key
	hash := idempotency.HashRequest(route, rawBody)
	claim, err := s.deps.Idempotency.Claim(ctx, userID, nsKey, route, hash)
	if err != nil {
		return 0, zero, NewError(500, "internal_error", "idempotency store error")
	}
	switch claim.Outcome {
	case idempotency.OutcomeReplay:
		var body T
		if e := json.Unmarshal(claim.Cached.Body, &body); e != nil {
			return 0, zero, NewError(500, "internal_error", "failed to decode cached response")
		}
		return claim.Cached.StatusCode, body, nil
	case idempotency.OutcomeInFlight:
		return 0, zero, NewError(409, "idempotency_in_flight", "a request with this Idempotency-Key is already in progress")
	case idempotency.OutcomeMismatch:
		return 0, zero, NewError(422, "idempotency_key_reuse", "Idempotency-Key was reused with a different request body")
	}
	// OutcomeAcquired: we own the key and must Complete (success) or
	// Release (pre-side-effect failure). Guard the window with a recover so a
	// panic in fn (or below) doesn't orphan the row as in_progress — which
	// would 409 every retry until the stale-claim window elapses. On panic we
	// release the key and re-raise (Huma still 500s). This is best-effort: a
	// panic strictly after a committed side effect degrades to at-least-once,
	// the same guarantee a mid-flight crash already gives.
	defer func() {
		if r := recover(); r != nil {
			_ = s.deps.Idempotency.Release(ctx, userID, nsKey)
			panic(r)
		}
	}()
	status, body, ferr := fn()
	if ferr != nil {
		_ = s.deps.Idempotency.Release(ctx, userID, nsKey)
		return 0, zero, ferr
	}
	// The side effect has committed. We MUST Complete the key — never leave
	// it Acquired (orphaned) — so a retry replays the cached response rather
	// than re-doing the side effect (e.g. re-sending email). Releasing here
	// would be the wrong move: it would let a retry re-execute. If the body
	// somehow fails to marshal, cache an empty body (status preserved) — a
	// replayed empty body is far better than a double-send.
	raw, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		raw = []byte("{}")
	}
	_ = s.deps.Idempotency.Complete(ctx, userID, nsKey, idempotency.CachedResponse{
		StatusCode: status, ContentType: "application/json", Body: raw,
	})
	return status, body, nil
}
