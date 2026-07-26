// Package hitlnotify sends the approval notification email that fires
// whenever a new outbound message enters pending_review.
//
// The notification is the reviewer's primary touchpoint with HITL — it
// arrives in the account owner's inbox with a preview of the held
// message and one-click approve / reject magic links, plus a link back
// to the dashboard for edit-before-approve.
//
// Delivery is durable, on River: the hold accept-tx enqueues a hitl_notify
// job (QueueNotify) in the same transaction as the pending_review row, and
// the NotifyWorker (worker.go) recomposes and submits the email ONCE off the
// request path — River owns the retry envelope (docs/design/hitl-notify-river.md).
// This replaced the earlier detached, best-effort goroutine, which lost the
// notification on a crash or SMTP outage between the 202 response and the send.
package hitlnotify

import (
	"context"
	"fmt"
	"html"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/tokencanopy/e2a/internal/approvaltoken"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// notifyLocalPart is the fixed local-part of the notification sender
// address. Reusing a single from-address lets mail clients group all HITL
// notifications into a single conversation / filter.
const notifyLocalPart = "hitl-noreply"

// tokenGraceAfterTTL extends the magic-link token's exp slightly past the
// message's approval_expires_at so a click received just before TTL is
// still honored — the expiration worker is the authoritative TTL gate.
const tokenGraceAfterTTL = 10 * time.Minute

// Notifier sends approval notification emails. Construct with New, then
// call NotifyPendingApproval from the HITL gate right after the pending
// row is written. Errors are logged, never returned upstream.
type Notifier struct {
	store      *identity.Store
	relay      *outbound.SMTPRelay
	signer     *approvaltoken.Signer
	fromDomain string
	publicURL  string
}

// New returns a Notifier that sends mail through relay using the given
// public URL to build magic-link URLs. fromDomain is the platform
// relay's from-domain — e.g. "send.example.com" — which is combined with
// the fixed local-part to produce the From address.
func New(store *identity.Store, relay *outbound.SMTPRelay, signer *approvaltoken.Signer, fromDomain, publicURL string) *Notifier {
	return &Notifier{
		store:      store,
		relay:      relay,
		signer:     signer,
		fromDomain: fromDomain,
		publicURL:  strings.TrimRight(publicURL, "/"),
	}
}

// NotifyPendingApproval composes and sends the notification email for a held
// message, submitting once (SendOnce). It is the compose+send core the River
// NotifyWorker drives via Deliver; the returned error is classified there into
// retry/permanent/outage.
func (n *Notifier) NotifyPendingApproval(ctx context.Context, msg *identity.Message, agent *identity.AgentIdentity) error {
	if n == nil {
		return nil
	}
	if msg == nil || agent == nil {
		return fmt.Errorf("notify: msg or agent is nil")
	}
	if msg.ApprovalExpiresAt == nil {
		return fmt.Errorf("notify: approval_expires_at is nil on msg %s", msg.ID)
	}

	owner, err := n.store.GetUserByID(ctx, agent.UserID)
	if err != nil {
		return fmt.Errorf("notify: lookup owner: %w", err)
	}
	if owner.Email == "" {
		return fmt.Errorf("notify: owner %s has no email on record", owner.ID)
	}

	tokenExp := msg.ApprovalExpiresAt.Add(tokenGraceAfterTTL)

	// Magic-link tokens are signed with the deployment HMAC secret
	// (cfg.Signing.HMACSecret) via n.signer — the sole signer.
	signFn := func(action string) (string, error) {
		return n.signer.Sign(msg.ID, action, tokenExp)
	}

	approveTok, err := signFn(approvaltoken.ActionApprove)
	if err != nil {
		return fmt.Errorf("notify: sign approve token: %w", err)
	}
	rejectTok, err := signFn(approvaltoken.ActionReject)
	if err != nil {
		return fmt.Errorf("notify: sign reject token: %w", err)
	}

	subject := fmt.Sprintf("[e2a] approve outbound from %s: %s",
		agent.EmailAddress(), truncate(msg.Subject, 60))

	approveURL := n.magicURL("/v1/approve", approveTok)
	rejectURL := n.magicURL("/v1/reject", rejectTok)
	dashboardURL := n.dashboardURL(msg.ID)

	text := renderText(msg, agent, approveURL, rejectURL, dashboardURL)
	htmlBody := renderHTML(msg, agent, approveURL, rejectURL, dashboardURL)

	fromAddr := fmt.Sprintf("%s@%s", notifyLocalPart, n.fromDomain)
	fromHeader := fmt.Sprintf("e2a <%s>", fromAddr)

	message, err := outbound.ComposeMultipartMessage(
		fromHeader, []string{owner.Email}, nil,
		subject, text, htmlBody,
		"",           // no reply-to-message-id (fresh notification)
		nil,          // no references chain (fresh notification)
		n.fromDomain, // from_domain (Message-ID generation)
		fromAddr,     // reply_to — point replies back at the platform, not the agent
		"",           // no conversation_id
	)
	if err != nil {
		return fmt.Errorf("notify: compose: %w", err)
	}

	// Prepend a DETERMINISTIC Message-ID so a re-sent notification collapses at
	// Message-ID-deduping recipients (Gmail/Workspace and most major clients) instead
	// of showing twice. The at-least-once notification pipeline can legitimately
	// re-send the same reviewer alert (a crash between SendOnce and MarkMessageNotified,
	// or a cutover reconciler re-drive); this makes those re-sends carry the SAME
	// Message-ID (stable per held message, unique across holds), which the recipient
	// then dedups on. Best-effort + recipient-side only — SES has no send-side dedup.
	//
	// compose deliberately omits Message-ID (SES assigns one for TRACKED sends to
	// avoid an id mismatch); a notification isn't tracked for delivery events, so a
	// caller-set id is safe here. Prepending a header line is valid RFC 5322 (the
	// Message-ID may lead the header block); msg.ID (msg_<rand>) + n.fromDomain carry
	// no CR/LF, so there's no header-injection risk. SES/SMTP preserves a supplied
	// Message-ID rather than overwriting it.
	msgIDHeader := fmt.Sprintf("<hitl-approve-%s@%s>", msg.ID, n.fromDomain)
	// Defense-in-depth: never let a Message-ID value inject extra headers. msg.ID
	// (msg_<hex>) and fromDomain (deployment config) are trusted and CRLF-free, so
	// this guard only trips on a future regression — falling back to SES's own
	// assigned id (no dedup, but no injection either).
	if !strings.ContainsAny(msgIDHeader, "\r\n") {
		message = append([]byte("Message-ID: "+msgIDHeader+"\r\n"), message...)
	}

	// SendOnce, not Send: this runs inside a River job, so River (not the relay's
	// in-process loop) owns retries. The %w keeps the SMTP error classifiable by
	// Deliver via internal/outbound's IsPermanentSMTPError / IsConnectionError.
	if _, err := n.relay.SendOnce(fromAddr, []string{owner.Email}, message); err != nil {
		return fmt.Errorf("notify: smtp send: %w", err)
	}

	log.Printf("[hitl-notify] sent approval email: msg=%s owner=%s agent=%s",
		msg.ID, owner.ID, agent.ID)
	return nil
}

// Deliver composes and sends the approval email for one held message, classifying
// the result for the River NotifyWorker: a 5xx / validation reject is Permanent
// (no retry), an unreachable relay is an Outage (snooze), everything else retries.
// Implements hitlnotify.Deliverer. The classifiers key on the SMTP code / net
// error preserved through NotifyPendingApproval's %w wrapping.
func (n *Notifier) Deliver(ctx context.Context, pn *identity.PendingNotify) DeliverOutcome {
	if err := n.NotifyPendingApproval(ctx, pn.Message, pn.Agent); err != nil {
		return DeliverOutcome{
			Err:       err,
			Permanent: outbound.IsPermanentSMTPError(err),
			Outage:    outbound.IsConnectionError(err),
		}
	}
	return DeliverOutcome{}
}

func (n *Notifier) magicURL(path, token string) string {
	if n.publicURL == "" {
		return path + "?t=" + url.QueryEscape(token)
	}
	return n.publicURL + path + "?t=" + url.QueryEscape(token)
}

func (n *Notifier) dashboardURL(messageID string) string {
	// The dashboard's pending page is a static route that reads the message
	// id from the ?id= query param (it redirects to /reviews?id=…). A
	// path-style /dashboard/pending/{id} URL 404s — the static export has no
	// per-message route.
	query := "?id=" + url.QueryEscape(messageID)
	if n.publicURL == "" {
		return "/dashboard/pending" + query
	}
	return n.publicURL + "/dashboard/pending" + query
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// The notification deliberately omits the held message's body from the
// email. The body lives in the database and is shown on the token-gated
// confirmation page the magic link leads to; keeping it out of the
// email avoids leaking sensitive draft content through the reviewer's
// mail infrastructure (spam filters, corporate archives, mobile sync,
// etc.). Reviewers see recipients and subject here — enough to know
// which message is waiting — and the full body only after they click.

func renderText(msg *identity.Message, agent *identity.AgentIdentity, approveURL, rejectURL, dashboardURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your agent %s wants to send a message.\n\n", agent.EmailAddress())
	if len(msg.ToRecipients) > 0 {
		fmt.Fprintf(&b, "To: %s\n", strings.Join(msg.ToRecipients, ", "))
	}
	if len(msg.CC) > 0 {
		fmt.Fprintf(&b, "Cc: %s\n", strings.Join(msg.CC, ", "))
	}
	if len(msg.BCC) > 0 {
		fmt.Fprintf(&b, "Bcc: %s\n", strings.Join(msg.BCC, ", "))
	}
	fmt.Fprintf(&b, "Subject: %s\n", msg.Subject)
	if msg.ApprovalExpiresAt != nil {
		fmt.Fprintf(&b, "Expires: %s\n", msg.ApprovalExpiresAt.UTC().Format(time.RFC1123))
	}
	b.WriteString("\nThe full body is not included in this email. Open a link\n")
	b.WriteString("below to review the message before approving or rejecting.\n\n")

	fmt.Fprintf(&b, "Review and approve:\n  %s\n\n", approveURL)
	fmt.Fprintf(&b, "Review and reject:\n  %s\n\n", rejectURL)
	fmt.Fprintf(&b, "Edit before approving (dashboard):\n  %s\n\n", dashboardURL)
	fmt.Fprintf(&b, "If no action is taken by the expiration time above, the\n")
	fmt.Fprintf(&b, "message will be finalized according to the agent's\n")
	fmt.Fprintf(&b, "configured auto-expiration policy.\n")
	return b.String()
}

// renderHTML builds the approval email in the dashboard's "Loft" palette so the
// notification reads as the same product as the web app: a warm cream shell, a
// white card, the Inter UI type stack, gold spark links, and the web app's semantic
// success/danger button shades. Colors are hardcoded (no CSS vars or @media) and
// the layout is table-based so it survives mail clients. Token values mirror
// web/src/app/globals.css; keep them in sync if the brand palette moves.
func renderHTML(msg *identity.Message, agent *identity.AgentIdentity, approveURL, rejectURL, dashboardURL string) string {
	const (
		fontStack = `"Inter",-apple-system,BlinkMacSystemFont,"Segoe UI",system-ui,sans-serif`
		bg        = "#FAF7F2" // --bg (cream shell)
		panel     = "#FFFFFF" // --bg-panel (card)
		border    = "#E5DED3" // --border
		fg        = "#1A1714" // --fg-strong
		muted     = "#6E665B" // --fg-muted
		subtle    = "#9A9082" // --fg-subtle
		link      = "#8A5214" // --accent-strong (gold spark)
		success   = "#0F7A4D" // --success (approve)
		danger    = "#CC2E2E" // --danger (reject)
		onAccent  = "#FFFFFF" // --accent-fg
	)
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><html><body style="margin:0;padding:24px 16px;background:%s;font-family:%s;color:%s;line-height:1.5">`, bg, fontStack, fg)
	fmt.Fprintf(&b, `<div style="max-width:560px;margin:0 auto;background:%s;border:1px solid %s;border-radius:10px;padding:28px">`, panel, border)

	fmt.Fprintf(&b, `<p style="margin:0 0 4px;font-size:15px">Your agent <strong>%s</strong> wants to send a message.</p>`,
		html.EscapeString(agent.EmailAddress()))

	fmt.Fprintf(&b, `<table style="font-size:14px;color:%s;border-collapse:collapse;margin:16px 0" cellpadding="4">`, fg)
	if len(msg.ToRecipients) > 0 {
		fmt.Fprintf(&b, `<tr><td style="color:%s">To</td><td>%s</td></tr>`,
			subtle, html.EscapeString(strings.Join(msg.ToRecipients, ", ")))
	}
	if len(msg.CC) > 0 {
		fmt.Fprintf(&b, `<tr><td style="color:%s">Cc</td><td>%s</td></tr>`,
			subtle, html.EscapeString(strings.Join(msg.CC, ", ")))
	}
	if len(msg.BCC) > 0 {
		fmt.Fprintf(&b, `<tr><td style="color:%s">Bcc</td><td>%s</td></tr>`,
			subtle, html.EscapeString(strings.Join(msg.BCC, ", ")))
	}
	fmt.Fprintf(&b, `<tr><td style="color:%s">Subject</td><td><strong>%s</strong></td></tr>`,
		subtle, html.EscapeString(msg.Subject))
	if msg.ApprovalExpiresAt != nil {
		fmt.Fprintf(&b, `<tr><td style="color:%s">Expires</td><td>%s</td></tr>`,
			subtle, html.EscapeString(msg.ApprovalExpiresAt.UTC().Format(time.RFC1123)))
	}
	b.WriteString(`</table>`)

	fmt.Fprintf(&b, `<p style="font-size:13px;color:%s">The message body is not included in this email. Click a button below to review it before deciding.</p>`, muted)

	// Action buttons point at the token-gated confirm pages (GET). The
	// actual approve/reject side effect only fires when the reviewer
	// submits the form on that page — this is what keeps mail-client URL
	// scanners from approving on the reviewer's behalf.
	//
	// Each button is a block-level anchor so the two stack vertically and
	// fill the available width. Inline buttons sitting side-by-side
	// overflowed and overlapped on narrow mobile viewports (the padded
	// inline anchors didn't grow line height when they wrapped); stacking
	// is robust across every width without needing @media queries, which
	// many mail clients strip.
	const btnStyle = `display:block;background:%s;color:%s;font-weight:500;padding:12px 18px;text-decoration:none;border-radius:6px;text-align:center;font-size:15px`
	fmt.Fprintf(&b, `<div style="margin-top:16px">`)
	fmt.Fprintf(&b,
		`<a href="%s" style="`+btnStyle+`;margin-bottom:10px">Review &amp; approve</a>`,
		html.EscapeString(approveURL), success, onAccent)
	fmt.Fprintf(&b,
		`<a href="%s" style="`+btnStyle+`">Review &amp; reject</a>`,
		html.EscapeString(rejectURL), danger, onAccent)
	fmt.Fprintf(&b, `</div>`)

	fmt.Fprintf(&b,
		`<p style="margin-top:16px;font-size:13px;color:%s">Need to edit before approving? <a href="%s" style="color:%s">Review in the dashboard</a>.</p>`,
		muted, html.EscapeString(dashboardURL), link)

	fmt.Fprintf(&b, `<p style="margin-top:24px;font-size:12px;color:%s">If no action is taken before the expiration time, the message will be finalized according to the agent's configured auto-expiration policy.</p>`, subtle)
	b.WriteString(`</div></body></html>`)
	return b.String()
}
