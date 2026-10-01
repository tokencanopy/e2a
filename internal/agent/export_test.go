package agent

// Test seams: re-export unexported helpers we want to unit-test from
// the external `agent_test` package without widening the public API.

import (
	"net/http"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// IsSelfSendForTest is the agent_test-package handle for isSelfSend.
// Pure function over (request, agent email); see selfsend.go for the
// behavioral contract. Renaming the public name is intentional — the
// "ForTest" suffix marks this as a test-only escape hatch so a future
// reader doesn't reach for it from production code.
func IsSelfSendForTest(req outbound.SendRequest, agentEmail string) bool {
	return isSelfSend(req, agentEmail)
}

// AuthenticateUserForTest exposes API.authenticateUser so external
// test helpers can construct synthetic guard-protected handlers
// without re-implementing the bearer-token plumbing.
func (a *API) AuthenticateUserForTest(r *http.Request) (userID string, err error) {
	u, err := a.authenticateUser(r)
	if err != nil {
		return "", err
	}
	return u.ID, nil
}

// IdempotencyGuardForTest exposes API.idempotencyGuard so tests in the
// external agent_test package can verify the side-effect-committed
// caching policy end-to-end (the standard handlers don't have a
// natural way to reach the "5xx after side effect" branch in a unit
// test).
func (a *API) IdempotencyGuardForTest(w http.ResponseWriter, r *http.Request, userID string, bodyBytes []byte) (replayed bool, out http.ResponseWriter, finalize func()) {
	return a.idempotencyGuard(w, r, userID, bodyBytes)
}

// MarkSideEffectCommittedForTest exposes markSideEffectCommitted so
// the external test harness can flip the cache-on-error flag from
// inside a synthetic handler.
func MarkSideEffectCommittedForTest(w http.ResponseWriter) {
	markSideEffectCommitted(w)
}

// WriteMagicMessageForTest exposes writeMagicMessage so the external
// agent_test package can verify the magic-link result page's HTML-escape
// contract directly (writeMagicMessage is the single place that escapes
// body; driving every handler path that can reach it would duplicate that
// coverage without testing anything writeMagicMessage itself doesn't
// already own).
func WriteMagicMessageForTest(w http.ResponseWriter, status int, title, body string) {
	writeMagicMessage(w, status, title, body)
}

// TruncatePreviewBytesForTest exposes truncatePreviewBytes for a direct
// unit test of the rune-boundary-safe byte cap on the confirm page's body
// preview.
func TruncatePreviewBytesForTest(s string, maxBytes int) string {
	return truncatePreviewBytes(s, maxBytes)
}

// MaxBodyPreviewBytesForTest is the confirm page's body-preview byte cap,
// exposed so tests assert against the real constant instead of a hardcoded
// copy that could drift from it.
const MaxBodyPreviewBytesForTest = maxBodyPreviewBytes

// BuildBlockedOutboundEventForTest exposes buildBlockedOutboundEvent (and the
// blockAuditID soft-ref it is keyed on) so the external agent_test package can
// drive the exact event the emit path constructs through a real outbox +
// production schema. Takes primitives so outboundVerdict stays unexported.
// Returns the event and the msgblk_ soft-ref.
func BuildBlockedOutboundEventForTest(agent *identity.AgentIdentity, req outbound.SendRequest, reviewReason, reason string) (webhookpub.Event, string) {
	softRef := blockAuditID(agent.ID, req)
	v := outboundVerdict{Applied: "block", ReviewReason: reviewReason, Reason: reason}
	// Receiver deliberately zero: the builder must stay stateless (pure over
	// its arguments) — if it ever reads API fields this seam nil-panics.
	return (&API{}).buildBlockedOutboundEvent(agent, softRef, req, v), softRef
}
