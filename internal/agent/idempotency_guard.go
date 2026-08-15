package agent

import (
	"bytes"
	"log"
	"net/http"
	"strings"

	"github.com/tokencanopy/e2a/internal/idempotency"
)

// idempotencyGuard runs the Idempotency-Key handshake at the start of a
// side-effectful POST handler. The transport contract is Stripe-style:
//
//   - No header → transparent no-op. The handler runs unchanged.
//   - Header set + cached completed response (same body) → server writes
//     the cached response verbatim and the caller MUST return from the
//     handler. Detected via the returned `replayed=true`.
//   - Header set + cached completed response (different body) → 422.
//     replayed=true; caller returns.
//   - Header set + another request currently in-flight → 409.
//     replayed=true; caller returns.
//   - Header set + claim acquired → handler proceeds. `out` is a
//     capturing wrapper around w that records the response so the
//     deferred `finalize()` can persist it. `finalize` MUST be deferred
//     by the caller — it decides between Complete and Release based on
//     the observed status code AND whether the handler signalled that
//     a non-recoverable side effect committed (see markSideEffectCommitted).
//
// On any internal error from the store (e.g. a transient DB blip),
// the guard fails open: the request proceeds without idempotency,
// the operator gets a log line, and no claim is recorded. The
// alternative — failing the request closed — would conflate
// idempotency-store outages with /send outages and make the system
// more brittle than the no-header baseline.
//
// Status-code policy for finalize:
//   - 2xx → Complete (cache for TTL). 202 (HITL held) included; a
//     retry must not create a second pending_review row.
//   - 4xx or 5xx, side effect NOT committed → Release. Client errors
//     mean the caller can fix and retry; an early server error before
//     the side effect happened means the caller can safely retry too.
//   - 4xx or 5xx, side effect committed → Complete (cache the error
//     response). Once the upstream send (or the loopback row writes)
//     accepted, we're locked in; a late server error must not free
//     the key for a retry that would double-send. Handlers signal
//     this via markSideEffectCommitted(w) after the irreversible
//     step succeeded.
//
// The signal is in-band on the capturingWriter (rather than a return
// from idempotencyGuard) so that no-header handlers and replay paths
// can ignore it entirely — the helper is a no-op when the writer
// isn't a capturing wrapper.
func (a *API) idempotencyGuard(w http.ResponseWriter, r *http.Request, userID string, bodyBytes []byte) (replayed bool, out http.ResponseWriter, finalize func()) {
	noop := func() {}

	if a.idempotency == nil {
		return false, w, noop
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		return false, w, noop
	}
	if len(key) > idempotency.MaxKeyLength {
		http.Error(w, "Idempotency-Key exceeds max length", http.StatusBadRequest)
		return true, nil, nil
	}

	res, err := a.idempotency.Claim(r.Context(), userID, key, r.URL.Path, idempotency.HashRequest(r.URL.Path, bodyBytes))
	if err != nil {
		log.Printf("[idempotency] claim error: %v (failing open)", err)
		return false, w, noop
	}

	switch res.Outcome {
	case idempotency.OutcomeReplay:
		if res.Cached.ContentType != "" {
			w.Header().Set("Content-Type", res.Cached.ContentType)
		}
		w.Header().Set("Idempotent-Replayed", "true")
		w.WriteHeader(res.Cached.StatusCode)
		_, _ = w.Write(res.Cached.Body)
		return true, nil, nil

	case idempotency.OutcomeMismatch:
		http.Error(w, "Idempotency-Key reused with a different request body", http.StatusUnprocessableEntity)
		return true, nil, nil

	case idempotency.OutcomeInFlight:
		http.Error(w, "another request with this Idempotency-Key is in progress", http.StatusConflict)
		return true, nil, nil

	case idempotency.OutcomeAcquired:
		cap := &capturingWriter{ResponseWriter: w}
		return false, cap, func() {
			// WriteHeader may not have been called explicitly (Go's
			// http package implicitly writes 200 on first Write).
			code := cap.statusCode
			if code == 0 {
				code = http.StatusOK
			}
			if shouldCacheResponse(code, cap.sideEffectCommitted) {
				if err := a.idempotency.Complete(r.Context(), userID, key, res.Token, idempotency.CachedResponse{
					StatusCode:  code,
					ContentType: cap.Header().Get("Content-Type"),
					Body:        cap.body.Bytes(),
				}); err != nil {
					log.Printf("[idempotency] complete error: %v", err)
				}
				return
			}
			if err := a.idempotency.Release(r.Context(), userID, key, res.Token); err != nil {
				log.Printf("[idempotency] release error: %v", err)
			}
		}
	}

	// Unreachable in practice; satisfy the compiler with a no-op pass-through.
	return false, w, noop
}

// shouldCacheResponse decides whether finalize should Complete (cache
// the response) or Release the claim. Pulled out as a pure function
// so the policy can be unit-tested without standing up the full
// HTTP + DB stack.
//
// Rule: cache on 2xx/3xx always; cache on 4xx/5xx ONLY if the handler
// signalled that an irreversible side effect committed (i.e. the
// upstream send accepted, or the loopback DB rows landed). Otherwise
// Release so the caller can retry with the same key.
func shouldCacheResponse(statusCode int, sideEffectCommitted bool) bool {
	if statusCode < 400 {
		return true
	}
	return sideEffectCommitted
}

// markSideEffectCommitted is called by handlers immediately after an
// irreversible action succeeds (an upstream SMTP/SES accept, a
// loopback row write) and BEFORE the response is written. It tells
// finalize to cache the eventual response no matter what status
// code the handler ends up writing — late panics, write errors, or
// follow-up DB writes that fail post-send must NOT release the
// key, because doing so would let a retry double-send.
//
// No-op when w isn't a capturingWriter (no-header path / replay
// path), so handlers can call it unconditionally without branching
// on whether idempotency is active for this request.
func markSideEffectCommitted(w http.ResponseWriter) {
	if cw, ok := w.(*capturingWriter); ok {
		cw.sideEffectCommitted = true
	}
}

// capturingWriter is a minimal http.ResponseWriter wrapper that
// records the outgoing status code and body so the idempotency layer
// can cache the response for replay. It still forwards everything to
// the underlying writer, so the live client receives the response
// unchanged.
//
// Not safe for concurrent use; handlers write their response from a
// single goroutine, matching the http server contract.
type capturingWriter struct {
	http.ResponseWriter
	statusCode          int
	body                bytes.Buffer
	sideEffectCommitted bool
}

func (c *capturingWriter) WriteHeader(code int) {
	if c.statusCode == 0 {
		c.statusCode = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *capturingWriter) Write(b []byte) (int, error) {
	if c.statusCode == 0 {
		c.statusCode = http.StatusOK
	}
	c.body.Write(b)
	return c.ResponseWriter.Write(b)
}
