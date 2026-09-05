package outbound

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// This file is the provider seam: the one place a customer-bound message
// becomes an SMTP transaction with SES.
//
// Everything above it — composition, DKIM, footers, recipient normalization —
// is deliberately token-free, because none of it exposes the shared SES
// reputation. Opening the socket does. So the socket is the thing that
// requires a sendingpolicy.ProviderAuthorization, and the adapter redeems that
// single-use token immediately before dialing, not when the job was picked up
// and not when the message was composed. A decision that went stale in between
// — a pause, a plan change, a policy rotation, a duplicate worker — invalidates
// the token instead of being raced.
//
// The adapter also owns three headers SES reads for provider-side isolation and
// attribution: X-SES-TENANT, X-E2A-Provider-Attempt, and
// X-SES-CONFIGURATION-SET (plus the stable X-E2A-Message-ID correlation marker
// beside them). Their values come only from the token and this deployment's
// configuration. Whatever the composed MIME already carried under those names
// is removed first, every occurrence, so neither a customer nor an upstream
// compose bug can smuggle or duplicate a tenant, attempt, or configuration-set
// selector.

// ProviderAttemptHeader carries the random attempt correlation id SES echoes
// back in delivery feedback. It is the fallback lookup when the worker died
// between SES accepting the message and the provider id being stored.
const ProviderAttemptHeader = "X-E2A-Provider-Attempt"

// SESTenantHeader names the SES tenant a submission is attributed to.
const SESTenantHeader = "X-SES-TENANT"

// SESConfigurationSetHeader selects the SES configuration set (delivery
// feedback destination) for a submission.
const SESConfigurationSetHeader = "X-SES-CONFIGURATION-SET"

// Sentinel errors for the provider seam. Every one of them is returned before
// any network I/O and before the token is redeemed.
var (
	// ErrAuthorizationRequired means SubmitOnce was called without a token.
	// There is no tokenless path to the provider by construction; a caller
	// hitting this has bypassed the gate.
	ErrAuthorizationRequired = errors.New("outbound: provider submission requires an authorization")
	// ErrTenantNameMissing means the token demands a tenant header but carries
	// no tenant name. The gate refuses to mint such a token; the adapter checks
	// again because the header is the provider-side isolation boundary and
	// must never be emitted empty.
	ErrTenantNameMissing = errors.New("outbound: authorization requires a tenant header but names no tenant")
	// ErrProviderHeaderValue means a provider-owned header value carries a
	// line break. The values come from the gate, so this is a defect, not
	// input — and a defect here is a header injection, so it fails closed
	// rather than being sanitized silently.
	ErrProviderHeaderValue = errors.New("outbound: provider header value contains a line break")
	// ErrMalformedHeaderSection means the composed MIME's header section holds
	// a bare carriage return. Receivers disagree on whether a lone CR ends a
	// line, so a header hidden behind one might survive stripping here and
	// still be honoured by the provider. The composer never emits one — every
	// header value is sanitized — so this only fires on a compose defect, and
	// it fails the send rather than guess.
	ErrMalformedHeaderSection = errors.New("outbound: message header section contains a bare carriage return")
)

// providerOwnedHeaders are removed from the composed MIME before submission,
// matched case-insensitively, folded continuations included.
var providerOwnedHeaders = map[string]struct{}{
	strings.ToLower(SESTenantHeader):           {},
	strings.ToLower(ProviderAttemptHeader):     {},
	strings.ToLower(SESConfigurationSetHeader): {},
	strings.ToLower(delivery.MessageIDHeader):  {},
}

// Envelope is what a caller hands the provider seam: the SMTP envelope and the
// composed wire bytes.
//
// There is deliberately no message id here. The stable X-E2A-Message-ID marker
// that delivery feedback keys on is derived from the token — a customer
// message's operation IS its message id — so a caller cannot stamp one
// message's id on another's send and misroute its bounces.
//
// The sender is the one envelope field the token does not bind: the
// authorization carries recipients and tenant, not MAIL FROM. SES enforces
// identity ownership of the sender domain on its side, and the caller here is
// the trusted worker that composed the message, so the seam only insists the
// sender is present. Binding it would need the gate to learn the composed
// sender at acceptance; that is a gate change, not an adapter one.
type Envelope struct {
	// From is the SMTP MAIL FROM address.
	From string
	// Recipients is the exact final envelope: one entry per distinct mailbox,
	// and exactly the set the authorization was minted for. Order is free.
	Recipients []string
	// Message is the composed MIME. Provider-owned headers in it are stripped.
	Message []byte
}

// ProviderResult reports one accepted provider submission.
type ProviderResult struct {
	// ProviderMessageID is the id SES assigned on acceptance.
	ProviderMessageID string
	// Attempt is the durable attempt that was redeemed for this call.
	Attempt sendingpolicy.AttemptRef
	// SettlementErr is set when SES accepted the message but the local
	// settlement did not commit. The send HAPPENED; the caller must retry
	// SettleProvider with the same attempt and provider id, and must never
	// resubmit. Delivery feedback carrying the attempt header is the fallback
	// if it never does.
	//
	// One value is not a retry: errors.Is(SettlementErr,
	// sendingpolicy.ErrProviderMessageIDConflict) means this attempt was
	// already settled with a DIFFERENT provider id — two physical sends for one
	// charge. Retrying settlement cannot fix that; it must be surfaced as an
	// invariant violation, not absorbed as a transient.
	SettlementErr error
}

// ProviderSubmitter is the token-requiring adapter over the SMTP relay.
type ProviderSubmitter struct {
	relay        *SMTPRelay
	gate         sendingpolicy.Gate
	sesConfigSet string
}

// NewProviderSubmitter binds the relay to the gate whose tokens it honors.
func NewProviderSubmitter(relay *SMTPRelay, gate sendingpolicy.Gate) *ProviderSubmitter {
	return &ProviderSubmitter{relay: relay, gate: gate}
}

// SetSESConfigurationSet names the configuration set every submission is
// tagged with. Empty means no header (dev/self-host without SES).
func (s *ProviderSubmitter) SetSESConfigurationSet(name string) { s.sesConfigSet = name }

// SESConfigurationSet reports the configured configuration set, for wiring
// tests that must prove delivery feedback stayed switched on.
func (s *ProviderSubmitter) SESConfigurationSet() string { return s.sesConfigSet }

// SubmitOnce makes exactly one provider call for one authorized attempt.
//
// The sequence is fixed and every early exit is I/O-free: prove the envelope is
// the authorized one, derive the provider headers from the token, rewrite the
// wire bytes, redeem the token, THEN dial. A relay-level retry is not offered
// here on purpose — each physical submission exposes SES once and must be
// charged once, so a retry is a new attempt with a new token, obtained by the
// caller through the gate.
//
// Outcomes: a definite permanent rejection is settled as such and returned; an
// acceptance is settled with the provider id and returned as a result. Anything
// ambiguous (4xx, connection loss, cancellation) is returned unsettled, because
// a message that might have been delivered must not release anything.
func (s *ProviderSubmitter) SubmitOnce(ctx context.Context, auth sendingpolicy.ProviderAuthorization, env Envelope) (ProviderResult, error) {
	if s == nil || s.relay == nil || s.gate == nil {
		return ProviderResult{}, errors.New("outbound: provider submitter is not wired")
	}
	if auth.IsZero() {
		return ProviderResult{}, ErrAuthorizationRequired
	}
	if strings.TrimSpace(env.From) == "" {
		return ProviderResult{}, errors.New("outbound: envelope sender is empty")
	}
	headers, err := auth.ValidateEnvelope(env.Recipients)
	if err != nil {
		return ProviderResult{}, err
	}
	provider, err := providerHeaderLines(headers, s.sesConfigSet, correlationMessageID(auth))
	if err != nil {
		return ProviderResult{}, err
	}
	stripped, err := stripProviderHeaders(env.Message)
	if err != nil {
		return ProviderResult{}, err
	}
	// Refuse a misconfigured relay before the token is spent. Redeeming first
	// would invalidate the attempt for a failure that had nothing to do with
	// the message, and the caller would burn a fresh ordinal per retry.
	if !s.relay.Configured() {
		return ProviderResult{}, fmt.Errorf("outbound SMTP relay not configured")
	}
	wire := append(provider, stripped...)

	if err := s.gate.RedeemProviderCall(ctx, auth); err != nil {
		return ProviderResult{}, fmt.Errorf("provider authorization: %w", err)
	}

	// RCPT TO is issued from the token's canonical envelope, not the caller's
	// spelling of it. ValidateEnvelope has just proved the two name the same
	// mailboxes; what goes on the wire is the normalized set the budget priced,
	// so a padded or upper-cased entry cannot turn into an SMTP grammar error
	// that downstream classifies as the message's own permanent failure.
	//
	// A failure after the body was fully written (ErrProviderAcceptanceUnknown)
	// is neither accepted nor rejected here: it is returned unsettled, because
	// the provider may hold the message and only its feedback can say.
	providerID, sendErr := s.relay.sendOnceContext(ctx, env.From, auth.AuthorizedRecipients(), wire)
	if sendErr != nil {
		// IsPermanentSMTPError is the worker's retry classifier: any 5xx,
		// including one raised before DATA (an AUTH 535, say). Settling such a
		// failure as a rejection is conservative in the only direction that
		// matters — the provider never took the message, so giving its
		// capacity back is correct — and it keeps this seam's verdict identical
		// to the one the worker already acts on.
		if IsPermanentSMTPError(sendErr) {
			if err := s.gate.SettleProvider(ctx, sendingpolicy.ProviderSettlement{
				Attempt: auth.Attempt(),
				Outcome: sendingpolicy.SettlementProviderPermanentlyRejected,
			}); err != nil {
				return ProviderResult{}, errors.Join(sendErr, fmt.Errorf("settle rejection: %w", err))
			}
		}
		return ProviderResult{}, sendErr
	}

	result := ProviderResult{ProviderMessageID: providerID, Attempt: auth.Attempt()}
	if err := s.gate.SettleProvider(ctx, sendingpolicy.ProviderSettlement{
		Attempt:           auth.Attempt(),
		Outcome:           sendingpolicy.SettlementProviderAccepted,
		ProviderMessageID: providerID,
	}); err != nil {
		result.SettlementErr = fmt.Errorf("settle acceptance: %w", err)
	}
	return result, nil
}

// correlationMessageID is the value of the stable X-E2A-Message-ID marker for
// a token: the message id for a customer message, nothing for every other
// purpose (operational mail has no message row for feedback to land on).
func correlationMessageID(auth sendingpolicy.ProviderAuthorization) string {
	if auth.Purpose() == sendingpolicy.PurposeCustomerMessage {
		return auth.Attempt().OperationID()
	}
	return ""
}

// providerHeaderLines renders the provider-owned header block from the token's
// view and the deployment configuration — and from nothing else.
//
// Callers cannot pass a tenant name, correlation id, or message id; the only
// way to change what SES receives is to change what the gate authorized. The
// order — configuration set, then message id — matches what Sender.SubmitOnce
// emits today, so swapping the worker onto this seam is byte-identical for
// the headers both paths share.
func providerHeaderLines(h sendingpolicy.ProviderHeaders, sesConfigSet, messageID string) ([]byte, error) {
	if h.TenantRequired && strings.TrimSpace(h.TenantName) == "" {
		return nil, ErrTenantNameMissing
	}
	for _, v := range []string{h.AttemptCorrelationID, h.TenantName, sesConfigSet, messageID} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, ErrProviderHeaderValue
		}
	}
	var b bytes.Buffer
	if sesConfigSet != "" {
		b.WriteString(SESConfigurationSetHeader + ": " + sesConfigSet + "\r\n")
	}
	if messageID != "" {
		b.WriteString(delivery.MessageIDHeader + ": " + messageID + "\r\n")
	}
	if h.AttemptCorrelationID != "" {
		b.WriteString(ProviderAttemptHeader + ": " + h.AttemptCorrelationID + "\r\n")
	}
	if h.TenantRequired {
		b.WriteString(SESTenantHeader + ": " + h.TenantName + "\r\n")
	}
	return b.Bytes(), nil
}

// stripProviderHeaders removes every provider-owned header field from the
// header section of a message, leaving every other byte — including the body
// and the original line endings — untouched.
//
// It walks the header section line by line rather than parsing it as a
// message: the bytes were composed by this package or arrived from a customer,
// and a parser that normalized them would change what DKIM signed. A field is
// its name line plus every folded continuation (a line starting with space or
// tab); dropping a field drops its continuations with it. Matching is on the
// lowercased name, so mixed-case spellings are not an evasion.
//
// Lines end in LF or CRLF. A carriage return anywhere else in the header
// section is refused (ErrMalformedHeaderSection): a receiver that treats a
// bare CR as a line break would see a header this walker did not, and the
// composer never produces one.
func stripProviderHeaders(msg []byte) ([]byte, error) {
	// A message whose first line is a folded continuation has nothing to
	// continue — except the provider header this adapter is about to prepend,
	// whose value it would silently extend. Refuse it.
	if len(msg) > 0 && (msg[0] == ' ' || msg[0] == '\t') {
		return nil, ErrMalformedHeaderSection
	}
	out := make([]byte, 0, len(msg))
	rest := msg
	dropping := false
	for len(rest) > 0 {
		nl := bytes.IndexByte(rest, '\n')
		var line []byte
		if nl < 0 {
			line, rest = rest, nil
		} else {
			line, rest = rest[:nl+1], rest[nl+1:]
		}
		if raw := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r")); bytes.IndexByte(raw, '\r') >= 0 {
			return nil, ErrMalformedHeaderSection
		}
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 {
			// End of the header section: emit the separator and the body
			// verbatim.
			out = append(out, line...)
			out = append(out, rest...)
			return out, nil
		}
		if trimmed[0] == ' ' || trimmed[0] == '\t' {
			if !dropping {
				out = append(out, line...)
			}
			continue
		}
		dropping = false
		if colon := bytes.IndexByte(trimmed, ':'); colon > 0 {
			name := strings.ToLower(strings.TrimSpace(string(trimmed[:colon])))
			if _, owned := providerOwnedHeaders[name]; owned {
				dropping = true
				continue
			}
		}
		out = append(out, line...)
	}
	return out, nil
}
