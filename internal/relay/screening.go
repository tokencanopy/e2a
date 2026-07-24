package relay

import (
	"context"
	"log"

	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/inboundpolicy"
	"github.com/tokencanopy/e2a/internal/inboundscreen"
)

// inboundScreenResult is the outcome of content-screening one inbound message.
// The evaluation core was extracted verbatim to internal/inboundscreen so the
// loopback self-send paths share it; the alias keeps this package's call sites
// and tests unchanged.
type inboundScreenResult = inboundscreen.Result

// screenInbound runs the agent's content scan (when inbound_scan='on'), combines it
// with the ingestion-gate decision into one applied action, and decides whether the
// message is HELD (review/block) or delivered (flag/allow). Thin delegation to
// inboundscreen.Evaluate — see that package for the full semantics.
func (s *Server) screenInbound(ctx context.Context, agent *identity.AgentIdentity, messageID, senderEmail string, body []byte, auth *emailauth.Authentication, gate inboundpolicy.Decision) inboundScreenResult {
	return inboundscreen.Evaluate(ctx, s.screen, agent, messageID, senderEmail, body, auth, gate)
}

// writeProtectionEvents appends the audit rows best-effort. Deterministic ids +
// ON CONFLICT DO NOTHING make an MTA-retried re-screen idempotent, so writing
// outside the message transaction is safe.
func (s *Server) writeProtectionEvents(ctx context.Context, messageID string, events []identity.ProtectionEvent) {
	for _, ev := range events {
		if err := s.store.CreateProtectionEvent(ctx, ev); err != nil {
			log.Printf("[mail:%s] screening_event write failed (%s/%s): %v", messageID, ev.Source, ev.Reason, err)
		}
	}
}
