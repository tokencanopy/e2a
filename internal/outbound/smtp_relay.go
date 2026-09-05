package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"strings"
	"time"

	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/logredact"
)

var smtpRetryBackoffs = []time.Duration{1 * time.Second, 5 * time.Second, 15 * time.Second}

// ErrProviderAcceptanceUnknown marks a failure that happened AFTER the whole
// message body was handed to the provider: the terminating dot was written and
// the 250 never arrived. The provider may have accepted the message. No
// classifier can call this permanent or transient, and a caller that retries
// it as if nothing was sent will deliver the message twice. The sending
// protection seam leaves such an attempt unsettled; delivery feedback carrying
// the attempt header is the only authoritative answer.
var ErrProviderAcceptanceUnknown = errors.New("outbound smtp: message body delivered, provider acceptance unknown")

type SMTPRelay struct {
	cfg *config.OutboundSMTPConfig
}

func NewSMTPRelay(cfg *config.OutboundSMTPConfig) *SMTPRelay {
	return &SMTPRelay{cfg: cfg}
}

func (r *SMTPRelay) Configured() bool {
	return r.cfg.Host != ""
}

// Send sends an email to one or more recipients and returns the Message-ID assigned by the remote server (e.g. SES).
func (r *SMTPRelay) Send(from string, recipients []string, message []byte) (string, error) {
	return r.SendWithContext(context.Background(), from, recipients, message)
}

// SendWithContext sends an email while honoring ctx during SMTP I/O and retry
// backoff. It is intended for request-bound callers that cannot allow the
// relay's normal retry envelope to outlive the request budget.
func (r *SMTPRelay) SendWithContext(ctx context.Context, from string, recipients []string, message []byte) (string, error) {
	return r.SendWithEnvelopeContext(ctx, from, recipients, message)
}

// SendWithEnvelope sends an email using envelopeFrom for SMTP MAIL FROM.
// Issues RCPT TO for each recipient. If any RCPT TO is rejected, the transaction is aborted.
// Returns the Message-ID assigned by the remote SMTP server from the DATA response.
// Retries transient SMTP errors (4xx) up to 3 times with backoff.
func (r *SMTPRelay) SendWithEnvelope(envelopeFrom string, recipients []string, message []byte) (string, error) {
	return r.SendWithEnvelopeContext(context.Background(), envelopeFrom, recipients, message)
}

// SendWithEnvelopeContext is SendWithEnvelope with caller-controlled
// cancellation and deadline propagation.
func (r *SMTPRelay) SendWithEnvelopeContext(ctx context.Context, envelopeFrom string, recipients []string, message []byte) (string, error) {
	if !r.Configured() {
		return "", fmt.Errorf("outbound SMTP relay not configured")
	}

	var lastErr error
	for attempt := 0; attempt <= len(smtpRetryBackoffs); attempt++ {
		msgID, err := r.sendOnceContext(ctx, envelopeFrom, recipients, message)
		if err == nil {
			return msgID, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !isTransientSMTPError(lastErr) {
			return "", lastErr
		}
		if attempt < len(smtpRetryBackoffs) {
			// lastErr is an upstream MTA response and cannot be perfectly
			// sanitized: rejections routinely quote the recipient back at us
			// ("550 5.1.1 <bob@example.com>: user unknown"), which would
			// otherwise defeat the recipient redaction on this same line. Cap
			// it so at most a bounded slice of provider text is retained; the
			// full error still reaches the caller and the message row.
			log.Printf("[smtp-relay] transient error sending to recipient_count=%d recipient_domains=%v (attempt %d/%d), retrying in %s: %s",
				len(recipients), logredact.AddressDomains(recipients), attempt+1, len(smtpRetryBackoffs)+1, smtpRetryBackoffs[attempt], logredact.Truncate(lastErr.Error(), 200))
			select {
			case <-time.After(smtpRetryBackoffs[attempt]):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	return "", lastErr
}

// SendOnce performs a SINGLE SMTP submit — no internal retry loop — and returns
// the provider Message-ID. This is the entry point for the River outbound worker
// (internal/outboundsend), which owns the retry envelope: River reschedules the
// next attempt per the worker's NextRetry, so the relay must NOT loop (a loop here
// would hide the envelope from river_job and make each Work() run up to ~6.5 min).
// Classify the returned error with IsTransientSMTPError — transient (4xx/throttle)
// → let River retry; permanent (5xx/validation) → fail the message terminally.
func (r *SMTPRelay) SendOnce(envelopeFrom string, recipients []string, message []byte) (string, error) {
	return r.SendOnceContext(context.Background(), envelopeFrom, recipients, message)
}

// SendOnceContext is SendOnce with caller cancellation propagated into the
// SMTP dial/command path. River workers use it so remotely cancelling a running
// job can stop provider I/O promptly.
func (r *SMTPRelay) SendOnceContext(ctx context.Context, envelopeFrom string, recipients []string, message []byte) (string, error) {
	if !r.Configured() {
		return "", fmt.Errorf("outbound SMTP relay not configured")
	}
	return r.sendOnceContext(ctx, envelopeFrom, recipients, message)
}

// IsTransientSMTPError reports whether err is a retryable SMTP failure (4xx /
// throttle) vs a permanent one. Exported so the River worker's deliverer can set
// DeliverOutcome.Permanent. Nil is not transient.
func IsTransientSMTPError(err error) bool { return isTransientSMTPError(err) }

// IsPermanentSMTPError reports whether err is a DEFINITELY-permanent SMTP failure —
// a 5xx response (recipient rejected, message refused) that must not be retried.
// The River worker's deliverer uses this to set DeliverOutcome.Permanent.
//
// It is deliberately conservative: ONLY a 5xx is permanent. Connection errors (dial
// timeout, connection refused), 4xx, and any unclassified error are NOT permanent —
// they retry. This matters for at-least-once: mis-classifying a transient network
// error as permanent would terminal-fail a send that should retry until the
// provider accepts. (Provider-outage snooze — deferring an outage instead of
// spending retries — is slice D.)
func IsPermanentSMTPError(err error) bool {
	code, ok := smtpCode(err)
	return ok && code >= 500 && code < 600
}

// IsConnectionError reports whether err is a provider-CONNECTION failure (the relay
// is unreachable / misconfigured) rather than a per-message SMTP rejection. The
// River worker snoozes these (design §8 circuit breaker): a regional SES/SNS outage
// should DEFER the whole queue without spending each job's retry budget and
// mass-firing false email.failed, whereas a per-message 4xx/5xx uses the
// bounded-retry / terminal path.
//
// CRITICAL: an error carrying an SMTP code is a per-message verdict, NEVER an
// outage — even if its text contains "tls"/"timeout"/"eof" (e.g. "550 5.7.1 TLS
// required"). We check the code first and bail; only codeless network-level
// failures (dial/timeout/reset/refused, or our own not-configured relay) are
// outages. (Earlier substring matching mis-classified a permanent 5xx-with-"tls"
// as a 72h outage snooze — adversarial review of #388.)
func IsConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := smtpCode(err); ok {
		return false // a real SMTP reply is a per-message verdict, not an outage
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true // dial/read/write timeout, connection refused, etc.
	}
	m := strings.ToLower(err.Error())
	for _, s := range []string{
		"not configured", "connection refused", "connection reset", "no such host",
		"no route to host", "broken pipe", "network is unreachable", "i/o timeout",
	} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// sendOnce performs a single SMTP send using smtp.Client for the handshake,
// then drives the DATA command manually via c.Text to capture the response.
// Issues RCPT TO for each recipient; aborts if any is rejected.
// SES returns the assigned Message-ID in the 250 response after DATA, e.g.:
//
//	250 Ok <010f019d2bd82cd5-49c4925c-...@us-east-2.amazonses.com>
func (r *SMTPRelay) sendOnceContext(ctx context.Context, envelopeFrom string, recipients []string, message []byte) (msgID string, err error) {
	defer func() {
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			err = ctx.Err()
			return
		}
		// The conn deadline is set to the ctx deadline below, so the net
		// poller's timer races the context's own timer to the same instant.
		// When the poller wins, the I/O error surfaces while ctx.Err() is
		// still nil — map it to the deadline error the caller contracted for.
		if d, ok := ctx.Deadline(); ok && !time.Now().Before(d) && errors.Is(err, os.ErrDeadlineExceeded) {
			err = context.DeadlineExceeded
		}
	}()

	addr := net.JoinHostPort(r.cfg.Host, fmt.Sprintf("%d", r.cfg.Port))

	dialer := net.Dialer{Timeout: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	// Set an overall deadline so a hanging server can't block forever
	deadline := time.Now().Add(2 * time.Minute)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("set smtp deadline: %w", err)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stopCancel()

	c, err := smtp.NewClient(conn, r.cfg.Host)
	if err != nil {
		conn.Close()
		return "", fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()

	// STARTTLS. Negotiate whenever the server advertises it; track
	// whether the connection actually became encrypted so we can fail
	// closed below. A network attacker can strip the STARTTLS capability
	// from the EHLO response to force a cleartext relay — RequireTLS
	// turns that into a hard error instead of a silent downgrade.
	tlsActive := false
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: r.cfg.Host}); err != nil {
			return "", fmt.Errorf("starttls: %w", err)
		}
		tlsActive = true
	}
	if r.cfg.RequireTLS != nil && *r.cfg.RequireTLS && !tlsActive {
		return "", fmt.Errorf("outbound smtp: server did not offer STARTTLS and require_tls is set; refusing to relay in cleartext")
	}

	// Auth. Never send PLAIN credentials over an unencrypted connection,
	// regardless of RequireTLS — that would leak the relay username and
	// password to anyone on the path. This is a hard floor below the
	// RequireTLS policy.
	if r.cfg.Username != "" {
		if !tlsActive {
			return "", fmt.Errorf("outbound smtp: refusing to send PLAIN auth over an unencrypted connection")
		}
		auth := smtp.PlainAuth("", r.cfg.Username, r.cfg.Password, r.cfg.Host)
		if err := c.Auth(auth); err != nil {
			return "", fmt.Errorf("auth: %w", err)
		}
	}

	// MAIL FROM
	if err := c.Mail(envelopeFrom); err != nil {
		return "", fmt.Errorf("mail from: %w", err)
	}

	// RCPT TO — one per recipient; abort if any is rejected
	for _, rcpt := range recipients {
		if err := c.Rcpt(rcpt); err != nil {
			return "", fmt.Errorf("rcpt to %s: %w", rcpt, err)
		}
	}

	// DATA — drive manually via c.Text to capture the 250 response text.
	// smtp.Client.Data() would consume and discard the response message.
	text := c.Text

	// Send DATA command
	id, err := text.Cmd("DATA")
	if err != nil {
		return "", fmt.Errorf("data cmd: %w", err)
	}
	text.StartResponse(id)
	_, _, err = text.ReadResponse(354)
	text.EndResponse(id)
	if err != nil {
		return "", fmt.Errorf("data response: %w", err)
	}

	// Write message body using dot-encoding writer
	w := text.DotWriter()
	if _, err := w.Write(message); err != nil {
		w.Close()
		return "", fmt.Errorf("data write: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("data close: %w", err)
	}

	// After DotWriter.Close() sends the terminating ".\r\n", the server's
	// 250 response is waiting in the buffer. Read it directly.
	_, msg, err := text.ReadResponse(250)
	if err != nil {
		return "", fmt.Errorf("data final: %w", errors.Join(ErrProviderAcceptanceUnknown, err))
	}

	// Parse Message-ID from response like "Ok <xxx@us-east-2.amazonses.com>"
	// and qualify it with the provider domain when the response carried the id
	// bare — the captured value must equal the on-wire Message-ID verbatim.
	sesMessageID := qualifyMessageIDDomain(parseMessageIDFromResponse(msg), r.cfg)

	c.Quit()
	return sesMessageID, nil
}

// qualifyMessageIDDomain appends the provider's Message-ID domain to an id
// that lacks one. SES's SMTP 250 response returns the assigned id BARE
// (010f...-000000, no domain), but the Message-ID it stamps on the message is
// <010f...-000000@<region>.amazonses.com>. The stored provider_message_id is
// what replies anchor In-Reply-To/References on, so it must equal the on-wire
// header verbatim — the domainless form matches nothing and every RFC 5322
// client forks the thread. Already-qualified ids pass through untouched; when
// no domain can be determined the id is left as-is (never fabricate one).
func qualifyMessageIDDomain(id string, cfg *config.OutboundSMTPConfig) string {
	if id == "" || strings.Contains(id, "@") {
		return id
	}
	domain := cfg.MessageIDDomain
	if domain == "" {
		if region := sesRegionFromHost(cfg.Host); region != "" {
			domain = region + ".amazonses.com"
		}
	}
	if domain == "" {
		return id
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(id, "<"), ">")
	return "<" + inner + "@" + domain + ">"
}

// sesRegionFromHost extracts the region from a standard SES SMTP endpoint
// (email-smtp.<region>.amazonaws.com). Returns "" for anything else — a
// non-SES relay or non-standard endpoint needs message_id_domain set instead.
func sesRegionFromHost(host string) string {
	rest, ok := strings.CutPrefix(host, "email-smtp.")
	if !ok {
		return ""
	}
	region, ok := strings.CutSuffix(rest, ".amazonaws.com")
	if !ok || region == "" || strings.Contains(region, ".") {
		return ""
	}
	return region
}

// parseMessageIDFromResponse extracts a Message-ID from an SMTP response string.
// SES format: "Ok <010f019d...@us-east-2.amazonses.com>"
// Production SES SMTP returns bare IDs without angle brackets or domain:
// "Ok 010f019d...-000000" — qualifyMessageIDDomain adds the domain afterward.
// Returns the full angle-bracket ID including <>, or the bare ID wrapped in <>.
func parseMessageIDFromResponse(resp string) string {
	if i := strings.Index(resp, "<"); i >= 0 {
		if j := strings.Index(resp[i:], ">"); j >= 0 {
			return resp[i : i+j+1]
		}
	}
	// Fallback: strip "Ok " prefix and wrap in angle brackets
	trimmed := strings.TrimSpace(resp)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimPrefix(trimmed, "Ok ")
	return "<" + trimmed + ">"
}

// smtpCode extracts the SMTP response code if err wraps a *textproto.Error (which
// net/smtp returns for any non-2xx/3xx reply, preserved through sendOnce's %w
// wrapping). Classifying on the real code — not first-char / substring of the
// wrapped string — is essential: production errors are wrapped ("rcpt to x: 550
// 5.1.1 User unknown"), so string heuristics on the whole message mis-fire.
func smtpCode(err error) (int, bool) {
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code, true
	}
	return 0, false
}

// isTransientSMTPError returns true for SMTP 4xx replies that are worth retrying.
func isTransientSMTPError(err error) bool {
	if err == nil {
		return false
	}
	if code, ok := smtpCode(err); ok {
		return code >= 400 && code < 500
	}
	// Some providers phrase throttling without a clean 4xx code.
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "throttl") || strings.Contains(lower, "rate limit")
}
