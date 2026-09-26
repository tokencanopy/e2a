package agent

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"os"
	"strings"

	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// ExternalSendingNotEnabledCode is the stable 403 code for a send the account
// may not make to its recipients through its sending identity.
const ExternalSendingNotEnabledCode = "external_sending_not_enabled"

// ExternalSendingRecoveryPath is the authenticated dashboard page that
// explains the restriction and offers the recovery routes (verify a domain,
// request approval, and — on the hosted service — a paid base plan).
const ExternalSendingRecoveryPath = "/sending-access"

// Allowed-destination tokens carried in the 403 details. Open set.
const (
	AllowedVerifiedOwnerEmail = "verified_owner_email"
	AllowedSameAccountAgents  = "same_account_agents"
)

// SetExternalAccess wires the external-sending-access preflight and status
// role. Nil (the default) skips the preflight entirely; acceptance and final
// authorization inside the gate still enforce whatever the policy says.
func (a *API) SetExternalAccess(x sendingpolicy.ExternalAccess) { a.externalAccess = x }

// preflightExternalAccess judges the composed sender and the whole To/Cc/Bcc
// envelope before anything is persisted — including a customer-review hold,
// so a draft the account could never send is refused up front instead of
// waiting for a reviewer. It is guidance for a clean 403; the gate repeats
// the check inside the accept transaction and at every later stage.
func (a *API) preflightExternalAccess(ctx context.Context, userID, agentID string, req outbound.SendRequest) *OutboundError {
	if a.externalAccess == nil {
		return nil
	}
	recipients := make([]string, 0, len(req.To)+len(req.CC)+len(req.BCC))
	for _, list := range [][]string{req.To, req.CC, req.BCC} {
		for _, r := range list {
			recipients = append(recipients, bareRecipient(r))
		}
	}
	verdict, err := a.externalAccess.ExternalAccessPreflight(ctx, userID, agentID, recipients)
	if err != nil {
		// Fail closed: an unreadable decision is neither an allow nor a claim
		// that the account lacks access. Retryable 500, nothing persisted.
		log.Printf("[api] external access preflight failed: agent=%s err=%v", agentID, err)
		return &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "could not verify sending access; retry shortly"}
	}
	if verdict.Paused {
		// Pause wins: report it, never a restriction that approval or a
		// payment would appear to lift.
		return &OutboundError{Status: http.StatusForbidden, Code: "sending_paused", Msg: "sending is paused for this account"}
	}
	if verdict.Denied() {
		return a.externalSendingNotEnabledError(ctx, userID)
	}
	return nil
}

// bareRecipient reduces a request recipient to its addr-spec. The send API
// accepts display-name forms ("Name <a@b>"); the composer persists only the
// bare address, and the gate judges exactly that, so the preflight must too.
// An unparseable value is passed through unchanged and fails closed in the
// gate's own validation.
func bareRecipient(raw string) string {
	if addr, err := mail.ParseAddress(raw); err == nil {
		return addr.Address
	}
	return strings.TrimSpace(raw)
}

// externalSendingNotEnabledError builds the structured 403. The message names
// the allowed destinations and the recovery routes, and deliberately does not
// suggest retrying: the same request will be refused until access changes.
func (a *API) externalSendingNotEnabledError(ctx context.Context, userID string) *OutboundError {
	allowed := []string{AllowedSameAccountAgents}
	ownerVerified := false
	if a.externalAccess != nil {
		if st, err := a.externalAccess.ExternalAccessStatus(ctx, userID); err == nil && st.OwnerRecipientVerified {
			ownerVerified = true
		}
	}
	msg := "External sending is not enabled for this account. You can send to agent inboxes in this account"
	if ownerVerified {
		allowed = []string{AllowedVerifiedOwnerEmail, AllowedSameAccountAgents}
		msg += " and to your verified account email"
	}
	msg += ". To email other recipients, send from your own verified domain or request approval in the dashboard. Retrying this request will not change the result."
	details := map[string]any{"allowed_recipients": allowed}
	if base := strings.TrimRight(a.publicURL, "/"); base != "" {
		details["recovery_url"] = base + ExternalSendingRecoveryPath
	}
	return &OutboundError{Status: http.StatusForbidden, Code: "external_sending_not_enabled", Msg: msg, Details: details}
}

// NotifySendingAccessRequest emails the deployment's operator notification
// recipients (FEEDBACK_NOTIFY_TO / FEEDBACK_NOTIFY_CC — the same fixed,
// server-configured envelope as /api/feedback) that an account filed an
// external sending access request. It never files a public issue: request
// details are private support data. Best effort — the request is already
// durably queued; a failed email is only logged. No channel configured (the
// self-host default) means log-only.
func (a *API) NotifySendingAccessRequest(ctx context.Context, userID string, req sendingpolicy.AccessRequest) {
	to := splitFeedbackAddrs(os.Getenv("FEEDBACK_NOTIFY_TO"))
	cc := splitFeedbackAddrs(os.Getenv("FEEDBACK_NOTIFY_CC"))
	if len(to) == 0 && len(cc) == 0 {
		log.Printf("[api] sending access request %s filed (no operator notification channel configured)", req.ID)
		return
	}
	// Operator-authored content first, customer text last and fenced: the
	// free-text fields are customer-supplied and must never read as part of
	// the command block (an injected "run this instead" line).
	body := fmt.Sprintf("Account: %s\nRequest: %s\nExpected daily volume: %d\n\n"+
		"Review, then decide with the audited local command:\n"+
		"  e2a -inspect-external-sending -account-id %s\n"+
		"  e2a -approve-external-sending -account-id %s -expected-external-sending-revision <rev> -external-sending-request-id %s -reason \"...\"\n"+
		"  e2a -decline-external-sending-request -account-id %s -external-sending-request-id %s\n\n"+
		"--- customer-supplied (untrusted) ---\nWhat they are building:\n%s\n\nWho they will email:\n%s\n",
		userID, req.ID, req.ExpectedDailyVolume,
		userID, userID, req.ID, userID, req.ID,
		quoteUntrusted(req.UseCase), quoteUntrusted(req.Recipients))
	if err := a.sendFeedbackEmail(ctx, "External sending access request "+req.ID, "sending-access", body, "", "", to, cc); err != nil {
		log.Printf("[api] sending access request %s: operator notification failed: %v", req.ID, err)
	}
}

// quoteUntrusted prefixes every line of customer-supplied text with "> " so
// it is visibly fenced off from operator content in the notification.
func quoteUntrusted(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}
