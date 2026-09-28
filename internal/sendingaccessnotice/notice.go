// Package sendingaccessnotice emails an account owner the operator's decision
// on an external sending access request.
//
// The local operator commands (-approve-external-sending with
// -external-sending-request-id, and -decline-external-sending-request) decide
// the request in their own committed transaction, then call NotifyDecision.
// The notice is platform mail about the account, so it leaves through the
// same authorized seam as every other notification: a customer_notification
// operation prepared from the durable request row, Reserve, ConsumeAttempt,
// and one ProviderSubmitter call per charged attempt. The envelope is the
// gate's (the account owner's current address), never a caller's.
//
// Content is operator-authored only. The customer's free-text request fields
// are never echoed back: the notice states the decision and where to go next.
package sendingaccessnotice

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// notifyLocalPart is the fallback sender local part when
// notifications.from_address is unset, on outbound_smtp.from_domain — the
// same zero-config pattern the HITL and webhook-health notices use.
const notifyLocalPart = "notifications"

// SendingAccessPath is the dashboard page a declined notice links to.
const SendingAccessPath = "/sending-access"

// Decision states the notice reports, mirroring the request row.
const (
	DecisionApproved = "approved"
	DecisionDeclined = "declined"
)

// sendAttempts bounds the physical submissions one notice may make; each is a
// distinct charged ordinal on the one operation.
const sendAttempts = 3

var retryBackoff = []time.Duration{time.Second, 2 * time.Second}

// TxBeginner is the transaction surface the notice needs (*pgxpool.Pool).
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Submitter is the one provider seam (*outbound.ProviderSubmitter).
type Submitter interface {
	SubmitOnce(ctx context.Context, auth sendingpolicy.ProviderAuthorization, env outbound.Envelope) (outbound.ProviderResult, error)
}

// Notifier composes and sends decision notices.
type Notifier struct {
	pool        TxBeginner
	gate        sendingpolicy.Gate
	submitter   Submitter
	dkim        outbound.DKIMKeyLookup
	fromAddress string
	fromDomain  string
	replyTo     string
	publicURL   string
	replyable   bool
}

// New returns a Notifier. fromDomain is outbound_smtp.from_domain;
// fromAddress and replyTo are the optional notifications.from_address and
// notifications.reply_to values (the deployment's notification identity);
// publicURL builds the dashboard links (empty degrades to generic copy).
func New(pool TxBeginner, gate sendingpolicy.Gate, s Submitter, fromDomain, fromAddress, replyTo, publicURL string) *Notifier {
	addr := strings.TrimSpace(fromAddress)
	if addr == "" && strings.TrimSpace(fromDomain) != "" {
		addr = fmt.Sprintf("%s@%s", notifyLocalPart, strings.TrimSpace(fromDomain))
	}
	msgIDDomain := strings.TrimSpace(fromDomain)
	if i := strings.LastIndex(addr, "@"); i >= 0 && i+1 < len(addr) {
		msgIDDomain = addr[i+1:]
	}
	return &Notifier{
		pool:        pool,
		gate:        gate,
		submitter:   s,
		fromAddress: addr,
		fromDomain:  msgIDDomain,
		replyTo:     strings.TrimSpace(replyTo),
		publicURL:   strings.TrimRight(strings.TrimSpace(publicURL), "/"),
		replyable:   strings.TrimSpace(fromAddress) != "" || strings.TrimSpace(replyTo) != "",
	}
}

// WithDKIM wires per-domain DKIM signing for the From domain (fail-open, as
// for the other notices).
func (n *Notifier) WithDKIM(lookup outbound.DKIMKeyLookup) *Notifier {
	n.dkim = lookup
	return n
}

// NotifyDecision emails the owner of the request's account the decision the
// request row records. It returns an error when the notice was not sent; the
// caller (an operator command whose decision has already committed) reports
// it as a warning and does not fail.
func (n *Notifier) NotifyDecision(ctx context.Context, requestID string) error {
	if n == nil || n.pool == nil || n.gate == nil || n.submitter == nil {
		return errors.New("sending access notice: notifier is not wired")
	}
	if n.fromAddress == "" {
		return errors.New("sending access notice: no sender identity (outbound_smtp.from_domain or notifications.from_address) is configured")
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return errors.New("sending access notice: request id is empty")
	}

	ref, decision, err := n.prepare(ctx, requestID)
	if err != nil {
		return err
	}

	var last error
	for attempt := 0; attempt < sendAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return errors.Join(ctx.Err(), last)
			case <-time.After(retryBackoff[attempt-1]):
			}
		}
		early, attemptRef, err := n.gate.Reserve(ctx, ref)
		if err != nil {
			return fmt.Errorf("sending access notice: reserve: %w", err)
		}
		if !early.Allow {
			return fmt.Errorf("sending access notice: held by sending policy: %s", early.Reason)
		}
		d, auth, err := n.gate.ConsumeAttempt(ctx, attemptRef)
		if err != nil {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_ = n.gate.CancelAttempt(releaseCtx, attemptRef)
			cancel()
			return fmt.Errorf("sending access notice: authorize: %w", err)
		}
		if !d.Allow || auth == nil {
			return fmt.Errorf("sending access notice: held by sending policy: %s", d.Reason)
		}
		// Composed from the gate's resolved envelope, so the To header and
		// RCPT TO name exactly the mailbox that was authorized.
		recipients := auth.AuthorizedRecipients()
		message, err := n.compose(requestID, decision, recipients)
		if err != nil {
			return err
		}
		_, err = n.submitter.SubmitOnce(ctx, *auth, outbound.Envelope{From: n.fromAddress, Recipients: recipients, Message: message})
		if err == nil {
			return nil
		}
		last = err
		if outbound.IsPermanentSMTPError(err) || errors.Is(err, outbound.ErrProviderAcceptanceUnknown) {
			// A definite rejection resends nothing; an unknown acceptance
			// may already be in the owner's inbox.
			break
		}
	}
	return fmt.Errorf("sending access notice: smtp send: %w", last)
}

// prepare derives the notice operation from the decided request row and reads
// the decision it reports, in one committed transaction.
func (n *Notifier) prepare(ctx context.Context, requestID string) (sendingpolicy.OperationRef, string, error) {
	tx, err := n.pool.Begin(ctx)
	if err != nil {
		return sendingpolicy.OperationRef{}, "", fmt.Errorf("sending access notice: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ref, err := n.gate.PrepareNotificationTx(ctx, tx, sendingpolicy.NewSendingAccessDecisionNotificationRef(requestID))
	if err != nil {
		return sendingpolicy.OperationRef{}, "", fmt.Errorf("sending access notice: prepare: %w", err)
	}
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM external_sending_access_requests WHERE id = $1`, requestID).Scan(&state); err != nil {
		return sendingpolicy.OperationRef{}, "", fmt.Errorf("sending access notice: read decision: %w", err)
	}
	if state != DecisionApproved && state != DecisionDeclined {
		return sendingpolicy.OperationRef{}, "", fmt.Errorf("sending access notice: request is %q, not decided", state)
	}
	if err := tx.Commit(ctx); err != nil {
		return sendingpolicy.OperationRef{}, "", fmt.Errorf("sending access notice: commit: %w", err)
	}
	return ref, state, nil
}

func (n *Notifier) compose(requestID, decision string, recipients []string) ([]byte, error) {
	subject, text, htmlBody := Render(decision, requestID, n.publicURL, n.replyable)
	message, err := outbound.ComposeMultipartMessage(
		fmt.Sprintf("e2a <%s>", n.fromAddress), recipients, nil,
		subject, text, htmlBody,
		"", nil, n.fromDomain, n.replyTo, "",
	)
	if err != nil {
		return nil, fmt.Errorf("sending access notice: compose: %w", err)
	}
	// Deterministic per request: a request is decided once, and a re-drive
	// after an ambiguous send collapses at Message-ID-deduping clients.
	msgID := fmt.Sprintf("<sending-access-%s-%s@%s>", decision, requestID, n.fromDomain)
	if !strings.ContainsAny(msgID, "\r\n") {
		message = append([]byte("Message-ID: "+msgID+"\r\n"), message...)
	}
	if signed, ok := outbound.SignWithDKIM(n.dkim, message, n.fromDomain); ok {
		message = signed
	}
	return message, nil
}

// Render returns the subject, plain-text and HTML bodies of a decision
// notice. Everything in it is operator-authored copy plus the request id and
// the deployment's dashboard URL; no customer-supplied text is included.
func Render(decision, requestID, publicURL string, replyable bool) (subject, text, htmlBody string) {
	base := strings.TrimRight(publicURL, "/")
	var link, linkLabel string
	var lines []string
	switch decision {
	case DecisionApproved:
		subject = "[e2a] External sending is enabled for your account"
		lines = []string{
			fmt.Sprintf("Your external sending access request (%s) was reviewed and approved.", requestID),
			"Your account can now send email to external recipients. Sending limits, pause controls and content checks still apply.",
		}
		if base != "" {
			link, linkLabel = base+"/", "Open the dashboard"
		}
	default:
		subject = "[e2a] Your external sending access request was declined"
		lines = []string{
			fmt.Sprintf("Your external sending access request (%s) was reviewed and declined.", requestID),
			"Your account can still send to agent inboxes in the account and to your verified account email.",
			"You may file a new request with more detail about your use case; each account can file up to 3 requests per 30 days.",
		}
		if base != "" {
			link, linkLabel = base+SendingAccessPath, "Review sending access"
		}
	}

	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n\n")
	}
	if link != "" {
		fmt.Fprintf(&b, "%s:\n  %s\n", linkLabel, link)
	} else if decision != DecisionApproved {
		b.WriteString("You can file a new request from the Sending access page of the e2a dashboard.\n")
	}
	if replyable {
		b.WriteString("\nReply to this email if you have questions.\n")
	}
	text = b.String()

	var h strings.Builder
	h.WriteString(`<!doctype html><html><body style="margin:0;padding:24px 16px;background:#FAF7F2;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;color:#1A1714;line-height:1.5">`)
	h.WriteString(`<div style="max-width:560px;margin:0 auto;background:#FFFFFF;border:1px solid #E5DED3;border-radius:10px;padding:28px">`)
	for i, l := range lines {
		style := "margin:0 0 12px;font-size:14px"
		if i == 0 {
			style = "margin:0 0 12px;font-size:15px;font-weight:600"
		}
		fmt.Fprintf(&h, `<p style="%s">%s</p>`, style, html.EscapeString(l))
	}
	if link != "" {
		fmt.Fprintf(&h, `<a href="%s" style="display:block;background:#2F4638;color:#FFFFFF;font-weight:500;padding:12px 18px;text-decoration:none;border-radius:6px;text-align:center;font-size:15px;margin-top:16px">%s</a>`,
			html.EscapeString(link), html.EscapeString(linkLabel))
	}
	if replyable {
		h.WriteString(`<p style="margin-top:24px;font-size:12px;color:#9A9082">Reply to this email if you have questions.</p>`)
	}
	h.WriteString(`</div></body></html>`)
	return subject, text, h.String()
}
