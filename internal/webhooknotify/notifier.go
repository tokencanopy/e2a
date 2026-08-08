package webhooknotify

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// notifyLocalPart is the fallback local-part of the sender address, used
// when notifications.from_address is not configured: the email then sends
// from <notifyLocalPart>@<outbound_smtp.from_domain>, exactly the
// hitlnotify pattern, so self-host works with zero configuration. Hosted
// deployments set notifications.from_address to a replyable support
// address instead — the address is CONFIGURATION, never a constant,
// because a hardcoded operator address would make every self-host try to
// send as an identity it does not own.
const notifyLocalPart = "webhooks-noreply"

// maxReasonLen bounds the failure-reason string echoed into the email.
// The source (webhook_subscriber_deliveries.last_error) is already
// sanitized by the delivery worker, but it can be long; the email needs
// only the gist.
const maxReasonLen = 200

// errNoOwnerEmail marks the one compose-time failure that can never
// succeed on retry: the account has no email on record. Classified
// Permanent so the worker cancels instead of churning the retry tail.
var errNoOwnerEmail = errors.New("owner has no email on record")

// NotifierStore is the read surface the notifier needs beyond the webhook
// row itself (which the worker passes in). *identity.Store satisfies it.
type NotifierStore interface {
	GetUserByID(ctx context.Context, id string) (*identity.User, error)
	RecentWebhookFailureStats(ctx context.Context, webhookID string, window time.Duration) (identity.WebhookFailureStats, error)
}

// relay is the narrow send surface (*outbound.SMTPRelay satisfies it).
// SendOnce, not Send: this runs inside a River job, so River owns retries.
type relay interface {
	SendOnce(envelopeFrom string, recipients []string, message []byte) (string, error)
}

// Notifier composes and sends the two webhook health emails. Construct
// with New; the NotifyWorker drives Deliver.
type Notifier struct {
	store NotifierStore
	relay relay
	// fromAddress is the resolved sender address (config value, or the
	// notifyLocalPart fallback on fromDomain).
	fromAddress string
	// fromDomain is the domain part of fromAddress (Message-ID generation).
	fromDomain string
	publicURL  string
}

// New returns a Notifier. fromDomain is cfg.OutboundSMTP.FromDomain (must
// be non-empty — the caller gates on it); fromAddress is the optional
// notifications.from_address config value, empty = fall back to the fixed
// local part on fromDomain. publicURL builds the dashboard link; empty
// degrades to generic copy.
func New(store NotifierStore, r relay, fromDomain, fromAddress, publicURL string) *Notifier {
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
		relay:       r,
		fromAddress: addr,
		fromDomain:  msgIDDomain,
		publicURL:   strings.TrimRight(publicURL, "/"),
	}
}

// FromAddress exposes the resolved sender address (tests + startup log).
func (n *Notifier) FromAddress() string { return n.fromAddress }

// Deliver composes and sends one health email, classifying the result for
// the NotifyWorker. Implements Deliverer.
func (n *Notifier) Deliver(ctx context.Context, wh *identity.Webhook, kind string) DeliverOutcome {
	if err := n.send(ctx, wh, kind); err != nil {
		return DeliverOutcome{
			Err:       err,
			Permanent: outbound.IsPermanentSMTPError(err) || errors.Is(err, errNoOwnerEmail),
			Outage:    outbound.IsConnectionError(err),
		}
	}
	return DeliverOutcome{}
}

func (n *Notifier) send(ctx context.Context, wh *identity.Webhook, kind string) error {
	if n == nil {
		return nil
	}
	if wh == nil {
		return fmt.Errorf("webhook notify: webhook is nil")
	}

	owner, err := n.store.GetUserByID(ctx, wh.UserID)
	if err != nil {
		return fmt.Errorf("webhook notify: lookup owner: %w", err)
	}
	if owner.Email == "" {
		return fmt.Errorf("webhook notify: owner %s: %w", owner.ID, errNoOwnerEmail)
	}

	window := identity.WarnWindow
	if kind == KindDisabled {
		window = identity.AutoDisableWindow
	}
	stats, err := n.store.RecentWebhookFailureStats(ctx, wh.ID, window)
	if err != nil {
		return fmt.Errorf("webhook notify: failure stats: %w", err)
	}

	reason := stats.LastError
	if kind == KindDisabled && wh.AutoDisableReason != "" {
		reason = wh.AutoDisableReason
	}
	if reason == "" {
		reason = "repeated delivery failures"
	}
	reason = truncate(reason, maxReasonLen)

	subject := fmt.Sprintf("[e2a] webhook delivery failing: %s", endpointLabel(wh.URL))
	if kind == KindDisabled {
		subject = fmt.Sprintf("[e2a] webhook disabled: %s", endpointLabel(wh.URL))
	}

	dashURL := ""
	if n.publicURL != "" {
		dashURL = n.publicURL + "/webhooks/detail?id=" + url.QueryEscape(wh.ID)
	}

	text := renderText(wh, kind, reason, stats.FailedAttempts, window, dashURL)
	htmlBody := renderHTML(wh, kind, reason, stats.FailedAttempts, window, dashURL)

	fromHeader := fmt.Sprintf("e2a <%s>", n.fromAddress)
	// Reply-To is set EXPLICITLY (to the same resolved address) rather than
	// relying on From, so a future change of sending identity cannot
	// silently break the reply path.
	message, err := outbound.ComposeMultipartMessage(
		fromHeader, []string{owner.Email}, nil,
		subject, text, htmlBody,
		"",            // no reply-to-message-id (fresh notification)
		nil,           // no references chain
		n.fromDomain,  // from_domain (Message-ID generation)
		n.fromAddress, // reply_to
		"",            // no conversation_id
	)
	if err != nil {
		return fmt.Errorf("webhook notify: compose: %w", err)
	}

	// Deterministic Message-ID so a crash-after-send re-drive collapses at
	// Message-ID-deduping recipients instead of showing twice (same
	// technique + rationale as hitlnotify). Keyed per EPISODE — the warn
	// stamp / disable timestamp — not per webhook, because a webhook can
	// legitimately warn again after recovering, and a webhook-keyed id
	// would make Gmail swallow the second episode's email.
	episode := time.Now()
	switch {
	case kind == KindWarning && wh.WarnNotifiedAt != nil:
		episode = *wh.WarnNotifiedAt
	case kind == KindDisabled && wh.AutoDisabledAt != nil:
		episode = *wh.AutoDisabledAt
	}
	msgIDHeader := fmt.Sprintf("<webhook-health-%s-%s-%d@%s>", kind, wh.ID, episode.Unix(), n.fromDomain)
	if !strings.ContainsAny(msgIDHeader, "\r\n") {
		message = append([]byte("Message-ID: "+msgIDHeader+"\r\n"), message...)
	}

	if _, err := n.relay.SendOnce(n.fromAddress, []string{owner.Email}, message); err != nil {
		return fmt.Errorf("webhook notify: smtp send: %w", err)
	}

	log.Printf("[webhook-notify] sent %s email: webhook=%s owner=%s", kind, wh.ID, owner.ID)
	return nil
}

// endpointLabel condenses the webhook URL for the subject line: host when
// parseable, else the (truncated) raw URL.
func endpointLabel(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return truncate(raw, 60)
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

func windowLabel(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		days := int(d / (24 * time.Hour))
		if days == 1 {
			return "24 hours"
		}
		return fmt.Sprintf("%d days", days)
	}
	return fmt.Sprintf("%d hours", int(d/time.Hour))
}

// What the disabled email must NOT claim: "your events are safe, replay
// them later". Replay is gated on matched_webhook_ids, stored at fan-out
// time against ENABLED subscribers only — so auto-disable is recoverable
// BACKWARDS (events queued before the disable) and lossy FORWARDS (events
// after it were never queued for this endpoint). The copy below states
// exactly that.

func renderText(wh *identity.Webhook, kind, reason string, failures int, window time.Duration, dashURL string) string {
	var b strings.Builder
	if kind == KindDisabled {
		fmt.Fprintf(&b, "e2a has disabled one of your webhooks after repeated delivery failures.\n\n")
	} else {
		fmt.Fprintf(&b, "Deliveries to one of your webhooks are failing.\n\n")
	}
	fmt.Fprintf(&b, "Endpoint: %s\n", wh.URL)
	fmt.Fprintf(&b, "Most recent error: %s\n", reason)
	fmt.Fprintf(&b, "Failed deliveries in the last %s: %d\n\n", windowLabel(window), failures)

	if kind == KindDisabled {
		b.WriteString("Events are no longer being delivered to this endpoint.\n")
		b.WriteString("Events received before the webhook was disabled can be replayed\n")
		b.WriteString("through the API for 30 days; events received after it are not\n")
		b.WriteString("queued for this endpoint and cannot be replayed to it.\n\n")
		b.WriteString("To recover: fix the endpoint, then re-enable the webhook")
		if dashURL != "" {
			fmt.Fprintf(&b, " from the dashboard:\n  %s\n", dashURL)
		} else {
			b.WriteString(" from the e2a dashboard (Webhooks page) or via PATCH /v1/webhooks/{id}.\n")
		}
		b.WriteString("\nRe-enabling is blocked for a short cooldown right after the automatic disable.\n")
	} else {
		fmt.Fprintf(&b, "e2a is still retrying each delivery, but if the endpoint keeps failing\n")
		fmt.Fprintf(&b, "(%d failed deliveries with none succeeding over %s), it will be\n",
			identity.AutoDisableThreshold, windowLabel(identity.AutoDisableWindow))
		b.WriteString("disabled automatically and events will stop being queued for it.\n\n")
		b.WriteString("To keep your events flowing, fix the endpoint now")
		if dashURL != "" {
			fmt.Fprintf(&b, ". Delivery history:\n  %s\n", dashURL)
		} else {
			b.WriteString(" (delivery history is on the e2a dashboard's Webhooks page).\n")
		}
	}
	b.WriteString("\nReply to this email if you need help.\n")
	return b.String()
}

// renderHTML mirrors hitlnotify's email styling (the dashboard's "Loft"
// palette, table-free simple card) so the notification reads as the same
// product. All dynamic strings are escaped.
func renderHTML(wh *identity.Webhook, kind, reason string, failures int, window time.Duration, dashURL string) string {
	const (
		fontStack = `"Inter",-apple-system,BlinkMacSystemFont,"Segoe UI",system-ui,sans-serif`
		bg        = "#FAF7F2"
		panel     = "#FFFFFF"
		border    = "#E5DED3"
		fg        = "#1A1714"
		muted     = "#6E665B"
		subtle    = "#9A9082"
		primary   = "#2F4638"
		onAccent  = "#FFFFFF"
		danger    = "#8C2B23"
	)
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><html><body style="margin:0;padding:24px 16px;background:%s;font-family:%s;color:%s;line-height:1.5">`, bg, fontStack, fg)
	fmt.Fprintf(&b, `<div style="max-width:560px;margin:0 auto;background:%s;border:1px solid %s;border-radius:10px;padding:28px">`, panel, border)

	if kind == KindDisabled {
		fmt.Fprintf(&b, `<p style="margin:0 0 4px;font-size:15px;color:%s"><strong>e2a has disabled one of your webhooks</strong> after repeated delivery failures.</p>`, danger)
	} else {
		fmt.Fprintf(&b, `<p style="margin:0 0 4px;font-size:15px"><strong>Deliveries to one of your webhooks are failing.</strong></p>`)
	}

	fmt.Fprintf(&b, `<table style="font-size:14px;color:%s;border-collapse:collapse;margin:16px 0" cellpadding="4">`, fg)
	fmt.Fprintf(&b, `<tr><td style="color:%s">Endpoint</td><td style="word-break:break-all">%s</td></tr>`, subtle, html.EscapeString(wh.URL))
	fmt.Fprintf(&b, `<tr><td style="color:%s">Last error</td><td><strong>%s</strong></td></tr>`, subtle, html.EscapeString(reason))
	fmt.Fprintf(&b, `<tr><td style="color:%s">Failures</td><td>%d in the last %s</td></tr>`, subtle, failures, windowLabel(window))
	b.WriteString(`</table>`)

	if kind == KindDisabled {
		fmt.Fprintf(&b, `<p style="font-size:13px;color:%s">Events are no longer being delivered to this endpoint. Events received <em>before</em> the disable can be replayed through the API for 30 days; events received <em>after</em> it are not queued for this endpoint and cannot be replayed to it. Fix the endpoint, then re-enable the webhook (a short cooldown applies right after the automatic disable).</p>`, muted)
	} else {
		fmt.Fprintf(&b, `<p style="font-size:13px;color:%s">e2a is still retrying each delivery, but if the endpoint keeps failing (%d failed deliveries with none succeeding over %s) it will be disabled automatically and events will stop being queued for it.</p>`,
			muted, identity.AutoDisableThreshold, windowLabel(identity.AutoDisableWindow))
	}

	if dashURL != "" {
		label := "Review deliveries"
		if kind == KindDisabled {
			label = "Re-enable webhook"
		}
		fmt.Fprintf(&b,
			`<a href="%s" style="display:block;background:%s;color:%s;font-weight:500;padding:12px 18px;text-decoration:none;border-radius:6px;text-align:center;font-size:15px;margin-top:16px">%s</a>`,
			html.EscapeString(dashURL), primary, onAccent, label)
	}

	fmt.Fprintf(&b, `<p style="margin-top:24px;font-size:12px;color:%s">Reply to this email if you need help.</p>`, subtle)
	b.WriteString(`</div></body></html>`)
	return b.String()
}
