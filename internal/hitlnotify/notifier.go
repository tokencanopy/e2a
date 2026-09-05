// Package hitlnotify sends the approval notification email that fires
// whenever a new outbound message enters pending_review.
//
// The notification is the reviewer's primary touchpoint with HITL — it
// arrives in the account owner's inbox with a preview of the held
// message and a primary link to the consolidated dashboard review,
// plus signed quick-action links for reviewers who cannot sign in.
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
	"net/url"
	"strings"
	"time"

	"github.com/tokencanopy/e2a/internal/approvaltoken"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// notifyLocalPart is the default local-part of the notification sender
// address, used when notifications.from_address is unset. Reusing a single
// from-address lets mail clients group all HITL notifications into a single
// conversation / filter — deliberately a DIFFERENT default from
// webhooknotify's, so an approval (time-boxed: miss it and the hold expires)
// stays filterable apart from routine webhook-health mail.
//
// Not "hitl-noreply": once notifications.reply_to points at a real inbox, a
// From that says noreply tells the reader the opposite of what we want, and
// the From is the most visible address in a mail client. "hitl" was also
// internal jargon on a customer-facing address.
const notifyLocalPart = "approvals"

// tokenGraceAfterTTL extends the magic-link token's exp slightly past the
// message's approval_expires_at so a click received just before TTL is
// still honored — the expiration worker is the authoritative TTL gate.
const tokenGraceAfterTTL = 10 * time.Minute

// Notifier sends approval notification emails. Construct with New, then
// call NotifyPendingApproval from the HITL gate right after the pending
// row is written. Errors are logged, never returned upstream.
type Notifier struct {
	store     *identity.Store
	submitter *outbound.ProviderSubmitter
	signer    *approvaltoken.Signer
	// fromAddress is the resolved sender: notifications.from_address when
	// set, else notifyLocalPart on fromDomain.
	fromAddress string
	// fromDomain is the domain used for Message-ID generation. It tracks
	// fromAddress's domain when that is configured, so a Message-ID never
	// claims a domain the From does not use.
	fromDomain string
	// replyTo is notifications.reply_to. Empty preserves the historical
	// behaviour of pointing Reply-To back at the sender address.
	replyTo   string
	publicURL string
	// dkim, when wired via WithDKIM, signs for the From-header domain.
	// nil (the zero-config self-host path) sends unsigned.
	dkim outbound.DKIMKeyLookup
}

// New returns a Notifier that sends mail through relay using the given
// public URL to build magic-link URLs. fromDomain is the platform relay's
// from-domain — e.g. "send.example.com" — combined with notifyLocalPart to
// produce the From address when fromAddress is empty.
//
// fromAddress and replyTo are notifications.from_address / .reply_to. Setting
// fromAddress to the same value here and in webhooknotify is what
// consolidates both senders into one identity; leaving it unset keeps them
// distinct and separately filterable. Resolution deliberately mirrors
// webhooknotify.New line for line; it is a copy rather than a shared helper,
// so changing one means changing the other.
func New(store *identity.Store, submitter *outbound.ProviderSubmitter, signer *approvaltoken.Signer, fromDomain, fromAddress, replyTo, publicURL string) *Notifier {
	addr := strings.TrimSpace(fromAddress)
	if addr == "" {
		addr = fmt.Sprintf("%s@%s", notifyLocalPart, fromDomain)
	}
	msgIDDomain := fromDomain
	if i := strings.LastIndex(addr, "@"); i >= 0 && i+1 < len(addr) {
		msgIDDomain = addr[i+1:]
	}
	return &Notifier{
		store:       store,
		submitter:   submitter,
		signer:      signer,
		fromAddress: addr,
		fromDomain:  msgIDDomain,
		replyTo:     strings.TrimSpace(replyTo),
		publicURL:   strings.TrimRight(publicURL, "/"),
	}
}

// WithDKIM wires per-domain DKIM signing, mirroring webhooknotify. The relay
// itself never signs and an upstream provider only signs identities it
// manages, so a custom notifications.from_address domain is signed here or
// not at all. nil-safe and fail-open via outbound.SignWithDKIM.
func (n *Notifier) WithDKIM(lookup outbound.DKIMKeyLookup) *Notifier {
	n.dkim = lookup
	return n
}

// NotifyPendingApproval composes and sends the notification email for a held
// message with an already-authorized attempt: Compose then Submit in one call,
// for callers that hold the token up front (tests, the reconciler drill). The
// worker calls the two phases itself so the token is consumed last.
func (n *Notifier) NotifyPendingApproval(ctx context.Context, msg *identity.Message, agent *identity.AgentIdentity, auth sendingpolicy.ProviderAuthorization) error {
	if n == nil {
		return nil
	}
	env, err := n.compose(ctx, msg, agent)
	if err != nil {
		return err
	}
	return n.submit(ctx, env, auth)
}

// compose builds the approval email: owner lookup, magic-link tokens, MIME,
// deterministic Message-ID and DKIM. It touches no provider.
func (n *Notifier) compose(ctx context.Context, msg *identity.Message, agent *identity.AgentIdentity) (outbound.Envelope, error) {
	if msg == nil || agent == nil {
		return outbound.Envelope{}, fmt.Errorf("notify: msg or agent is nil")
	}
	if msg.ApprovalExpiresAt == nil {
		return outbound.Envelope{}, fmt.Errorf("notify: approval_expires_at is nil on msg %s", msg.ID)
	}

	owner, err := n.store.GetUserByID(ctx, agent.UserID)
	if err != nil {
		return outbound.Envelope{}, fmt.Errorf("notify: lookup owner: %w", err)
	}
	if owner.Email == "" {
		return outbound.Envelope{}, fmt.Errorf("notify: owner %s has no email on record", owner.ID)
	}

	tokenExp := msg.ApprovalExpiresAt.Add(tokenGraceAfterTTL)

	// Magic-link tokens are signed with the deployment HMAC secret
	// (cfg.Signing.HMACSecret) via n.signer — the sole signer.
	signFn := func(action string) (string, error) {
		return n.signer.Sign(msg.ID, action, tokenExp)
	}

	approveTok, err := signFn(approvaltoken.ActionApprove)
	if err != nil {
		return outbound.Envelope{}, fmt.Errorf("notify: sign approve token: %w", err)
	}
	rejectTok, err := signFn(approvaltoken.ActionReject)
	if err != nil {
		return outbound.Envelope{}, fmt.Errorf("notify: sign reject token: %w", err)
	}

	subject := fmt.Sprintf("[e2a] approve outbound from %s: %s",
		agent.EmailAddress(), truncate(msg.Subject, 60))

	approveURL := n.magicURL("/v1/approve", approveTok)
	rejectURL := n.magicURL("/v1/reject", rejectTok)
	dashboardURL := n.dashboardURL(msg.ID)

	text := renderText(msg, agent, approveURL, rejectURL, dashboardURL)
	htmlBody := renderHTML(msg, agent, approveURL, rejectURL, dashboardURL)

	fromAddr := n.fromAddress
	fromHeader := fmt.Sprintf("e2a <%s>", fromAddr)

	// Reply-To: the configured inbox when there is one, else the sender
	// address as before. The historical value pointed replies back at the
	// platform rather than the agent, which was the right instinct — but the
	// platform relay domain has no mailbox, so a reviewer who hit Reply was
	// talking to the bounce endpoint. That matters most here: an approval is
	// time-boxed, and a reviewer who cannot sign in has only the magic links.
	replyTo := n.replyTo
	if replyTo == "" {
		replyTo = fromAddr
	}

	message, err := outbound.ComposeMultipartMessage(
		fromHeader, []string{owner.Email}, nil,
		subject, text, htmlBody,
		"",           // no reply-to-message-id (fresh notification)
		nil,          // no references chain (fresh notification)
		n.fromDomain, // from_domain (Message-ID generation)
		replyTo,      // reply_to — a real inbox when configured, never the agent
		"",           // no conversation_id
	)
	if err != nil {
		return outbound.Envelope{}, fmt.Errorf("notify: compose: %w", err)
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

	// DKIM-sign for the From-header domain AFTER the Message-ID prepend, so the
	// signature covers the final header set.
	//
	// Required for parity with webhooknotify once from_address is configurable:
	// the SMTP relay never signs, and an upstream provider only signs identities
	// IT manages, so a custom from_address domain (BYODKIM — e2a holds the key)
	// is signed here or not at all. Without this, an operator who set
	// notifications.from_address would get signed webhook-health mail and
	// UNSIGNED approval mail — leaving the most time-sensitive email the
	// platform sends on a single SPF leg, and so quarantined the moment anyone
	// forwards it. Fail-open via outbound.SignWithDKIM: no lookup, no stored key
	// for the domain, or a signing failure all send unsigned, exactly as before.
	if signed, ok := outbound.SignWithDKIM(n.dkim, message, n.fromDomain); ok {
		message = signed
	}

	return outbound.Envelope{From: fromAddr, Recipients: []string{owner.Email}, Message: message}, nil
}

// submit is the one authorized submission: the submitter redeems the token
// immediately before the socket opens and settles the provider's answer;
// River (not the relay's in-process loop) owns retries, each as a fresh
// attempt. The %w keeps the SMTP error classifiable via internal/outbound's
// IsPermanentSMTPError / IsConnectionError.
func (n *Notifier) submit(ctx context.Context, env outbound.Envelope, auth sendingpolicy.ProviderAuthorization) error {
	if _, err := n.submitter.SubmitOnce(ctx, auth, env); err != nil {
		return fmt.Errorf("notify: smtp send: %w", err)
	}
	return nil
}

// Compose implements Deliverer: the provider-free half, classified like a
// send so the worker treats a permanent compose failure the same way.
func (n *Notifier) Compose(ctx context.Context, pn *identity.PendingNotify) (outbound.Envelope, DeliverOutcome) {
	if pn == nil {
		return outbound.Envelope{}, DeliverOutcome{Err: fmt.Errorf("notify: nothing to compose"), Permanent: true}
	}
	env, err := n.compose(ctx, pn.Message, pn.Agent)
	if err != nil {
		return outbound.Envelope{}, classify(err)
	}
	return env, DeliverOutcome{}
}

// Submit implements Deliverer: one authorized submission, classified for the
// River NotifyWorker — a 5xx / validation reject is Permanent (no retry), an
// unreachable relay is an Outage (snooze), everything else retries.
func (n *Notifier) Submit(ctx context.Context, env outbound.Envelope, auth sendingpolicy.ProviderAuthorization) DeliverOutcome {
	if err := n.submit(ctx, env, auth); err != nil {
		return classify(err)
	}
	return DeliverOutcome{}
}

func classify(err error) DeliverOutcome {
	return DeliverOutcome{
		Err:       err,
		Permanent: outbound.IsPermanentSMTPError(err),
		Outage:    outbound.IsConnectionError(err),
	}
}

func (n *Notifier) magicURL(path, token string) string {
	if n.publicURL == "" {
		return path + "?t=" + url.QueryEscape(token)
	}
	return n.publicURL + path + "?t=" + url.QueryEscape(token)
}

func (n *Notifier) dashboardURL(messageID string) string {
	// The consolidated review page reads the held message id from ?id= and
	// expands the matching row. Keep this direct so notification recipients
	// do not traverse the legacy /dashboard/pending compatibility redirect.
	query := "?id=" + url.QueryEscape(messageID)
	if n.publicURL == "" {
		return "/reviews" + query
	}
	return n.publicURL + "/reviews" + query
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
// email. The body lives in the database and is shown in the authenticated
// dashboard or on a signed confirmation page; keeping it out of the
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
	b.WriteString("\nThe full body is not included in this email.\n\n")
	fmt.Fprintf(&b, "Review message:\n  %s\n\n", dashboardURL)
	b.WriteString("Can't sign in? These signed links still open a confirmation page before acting:\n\n")
	fmt.Fprintf(&b, "Quick approve:\n  %s\n\n", approveURL)
	fmt.Fprintf(&b, "Quick reject:\n  %s\n\n", rejectURL)
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
		primary   = "#2F4638" // --button-primary
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

	fmt.Fprintf(&b, `<p style="font-size:13px;color:%s">The message body is not included in this email. Review it in e2a before deciding.</p>`, muted)

	// The authenticated dashboard is the canonical review experience and
	// therefore the only primary button. The signed links remain available
	// as secondary no-login shortcuts; each opens a GET confirmation page,
	// and the actual side effect only fires on POST. That keeps mail-client
	// URL scanners from approving or rejecting on the reviewer's behalf.
	const btnStyle = `display:block;background:%s;color:%s;font-weight:500;padding:12px 18px;text-decoration:none;border-radius:6px;text-align:center;font-size:15px`
	fmt.Fprintf(&b,
		`<a href="%s" style="`+btnStyle+`;margin-top:16px">Review message</a>`,
		html.EscapeString(dashboardURL), primary, onAccent)
	fmt.Fprintf(&b,
		`<p style="margin-top:16px;font-size:13px;color:%s">Can&apos;t sign in? <a href="%s" style="color:%s">Quick approve</a> or <a href="%s" style="color:%s">Quick reject</a>. Each signed link asks for confirmation before acting.</p>`,
		muted, html.EscapeString(approveURL), link, html.EscapeString(rejectURL), link)

	fmt.Fprintf(&b, `<p style="margin-top:24px;font-size:12px;color:%s">If no action is taken before the expiration time, the message will be finalized according to the agent's configured auto-expiration policy.</p>`, subtle)
	b.WriteString(`</div></body></html>`)
	return b.String()
}
