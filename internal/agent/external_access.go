package agent

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"time"

	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// ExternalSendingNotEnabledCode is the stable 403 code for a send the account
// may not make to its recipients through its sending identity.
const ExternalSendingNotEnabledCode = "external_sending_not_enabled"

// ExternalSendingRecoveryPath is the authenticated dashboard page that
// explains the restriction and offers the recovery routes the deployment's
// unlock set allows (always a request for approval; a verified domain and a
// paid base plan only where configured).
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
	// Unknown unlock set (status unreadable) falls back to naming only the
	// route that is always available: approval. Never a route the
	// deployment may not honor.
	domainUnlock := false
	if a.externalAccess != nil {
		if st, err := a.externalAccess.ExternalAccessStatus(ctx, userID); err == nil {
			ownerVerified = st.OwnerRecipientVerified
			for _, u := range st.AvailableUnlocks {
				if u == sendingpolicy.UnlockVerifiedDomain {
					domainUnlock = true
				}
			}
		}
	}
	msg := "External sending is not enabled for this account. You can send to agent inboxes in this account"
	if ownerVerified {
		allowed = []string{AllowedVerifiedOwnerEmail, AllowedSameAccountAgents}
		msg += " and to your verified account email"
	}
	if domainUnlock {
		msg += ". To email other recipients, send from your own verified domain or request approval in the dashboard."
	} else {
		msg += ". To email other recipients, request approval in the dashboard (an operator reviews each request)."
	}
	msg += " Retrying this request will not change the result."
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
	// the command block (an injected "run this instead" line). The account
	// facts block is server-owned data read fresh from the database, so it
	// sits in the operator section too — above the fence — sparing the
	// reviewer an -inspect-external-sending round trip just to learn who
	// filed the request. A failed read only drops this block; it never
	// blocks the base notice (the request is already durably queued).
	body := fmt.Sprintf("Account: %s\nRequest: %s\nExpected daily volume: %d\n\n%s"+
		"Review, then decide with the audited local command:\n"+
		"  e2a -inspect-external-sending -account-id %s\n"+
		"  e2a -approve-external-sending -account-id %s -expected-external-sending-revision <rev> -external-sending-request-id %s -reason \"...\"\n"+
		"  e2a -decline-external-sending-request -account-id %s -external-sending-request-id %s\n\n"+
		"--- customer-supplied (untrusted) ---\nWhat they are building:\n%s\n\nWho they will email:\n%s\n",
		userID, req.ID, req.ExpectedDailyVolume, a.accountFactsBlock(ctx, userID, req.ID),
		userID, userID, req.ID, userID, req.ID,
		quoteUntrusted(req.UseCase), quoteUntrusted(req.Recipients))
	if err := a.sendFeedbackEmail(ctx, "External sending access request "+req.ID, "sending-access", body, "", "", to, cc); err != nil {
		log.Printf("[api] sending access request %s: operator notification failed: %v", req.ID, err)
	}
}

// operatorNoticeNameMaxRunes bounds the display name shown in the operator
// notice's account-facts block: a user-chosen value, printed above the
// untrusted fence.
const operatorNoticeNameMaxRunes = 80

// accountFactsBlock renders the server-owned account facts shown above the
// untrusted fence, terminated by a blank line ("" when unavailable, so the
// caller's format string degrades to exactly the pre-existing layout). Every
// fact is read fresh from the database for this one notification; a read
// failure anywhere logs a warning and the whole block is omitted rather than
// failing the request (it is already durably queued regardless).
func (a *API) accountFactsBlock(ctx context.Context, userID, requestID string) string {
	lines, err := a.loadAccountFactsLines(ctx, userID, requestID)
	if err != nil {
		log.Printf("[api] sending access request %s: account facts unavailable, sending base notice: %v", requestID, err)
		return ""
	}
	return strings.Join(lines, "\n") + "\n\n"
}

func (a *API) loadAccountFactsLines(ctx context.Context, userID, requestID string) ([]string, error) {
	if a.store == nil {
		return nil, fmt.Errorf("no identity store wired")
	}
	if a.externalAccess == nil {
		return nil, fmt.Errorf("no external-access reader wired")
	}
	user, err := a.store.GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("read user: %w", err)
	}
	counts, err := a.store.SendingAccessNoticeCounts(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("read account counts: %w", err)
	}
	status, err := a.externalAccess.ExternalAccessStatus(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("read external access status: %w", err)
	}
	priorDecided, lastOutcome, err := a.externalAccess.PriorDecidedAccessRequests(ctx, userID, requestID)
	if err != nil {
		return nil, fmt.Errorf("read prior access requests: %w", err)
	}

	ageDays := int(time.Since(user.CreatedAt).Hours() / 24)
	planCode := counts.PlanCode
	if planCode == "" {
		planCode = "free"
	}
	ownerLine := "Owner: " + user.Email
	if name := sanitizeOperatorLine(user.Name, operatorNoticeNameMaxRunes); name != "" {
		ownerLine += " (" + name + ")"
	}
	lines := []string{
		ownerLine,
		fmt.Sprintf("Signed up: %s (%d days ago)", user.CreatedAt.UTC().Format(time.RFC3339), ageDays),
	}
	if user.AccountClass != "" && user.AccountClass != "standard" {
		lines = append(lines, "Account class: "+user.AccountClass)
	}
	lines = append(lines,
		"Plan: "+planCode,
		"Owner mailbox verified: "+yesNo(status.OwnerRecipientVerified),
		fmt.Sprintf("Agents: %d live", counts.LiveAgents),
		fmt.Sprintf("Domains: %d verified", counts.VerifiedDomains),
	)
	if counts.SendingState == "paused" {
		pauseClass := counts.PauseClass
		if pauseClass == "" {
			pauseClass = "unknown"
		}
		lines = append(lines, fmt.Sprintf("Sending: paused (%s)", pauseClass))
	} else {
		lines = append(lines, "Sending: "+counts.SendingState)
	}
	if priorDecided == 0 {
		lines = append(lines, "Prior requests: none")
	} else {
		lines = append(lines, fmt.Sprintf("Prior requests: %d decided (last: %s)", priorDecided, lastOutcome))
	}
	return lines, nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// sanitizeOperatorLine neutralizes a user-chosen value before it is printed
// as part of one line of the operator-authored section, ABOVE the untrusted
// fence: every line break (the same Unicode set quoteUntrusted's fence
// splits on) becomes a space, and every other control character — including
// the bidi embedding/override/isolate controls that could visually reorder
// the line, e.g. to make later operator text read as part of the name — is
// dropped outright. Consecutive whitespace left behind collapses to single
// spaces, and the result is capped to maxRunes. Unlike quoteUntrusted, this
// never introduces a newline: the value must stay on the one line it is
// printed on, never fabricate a line that could pass as operator content.
func sanitizeOperatorLine(s string, maxRunes int) string {
	s = untrustedLineBreaks.Replace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(' ')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// C0/C1 control characters.
			continue
		case r >= 0x202a && r <= 0x202e:
			// Bidi embedding/override controls (LRE/RLE/PDF/LRO/RLO).
			continue
		case r >= 0x2066 && r <= 0x2069:
			// Bidi isolate controls (LRI/RLI/FSI/PDI).
			continue
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if runes := []rune(out); len(runes) > maxRunes {
		out = string(runes[:maxRunes])
	}
	return out
}

// untrustedLineBreaks are every line break a mail client may render: CR,
// VT, FF, NEL and the Unicode line/paragraph separators, besides LF.
var untrustedLineBreaks = strings.NewReplacer(
	"\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n",
	"\u0085", "\n", "\u2028", "\n", "\u2029", "\n",
)

// quoteUntrusted prefixes every line of customer-supplied text with "> " so
// it is visibly fenced off from operator content in the notification. It
// splits on every Unicode line break, not only LF, so no line can escape
// the fence (intake also rejects these characters; this is the second wall).
func quoteUntrusted(text string) string {
	lines := strings.Split(untrustedLineBreaks.Replace(text), "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}
