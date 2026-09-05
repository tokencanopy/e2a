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
type Envelope struct {
	// MessageID is the e2a message id stamped as X-E2A-Message-ID, the stable
	// correlation marker delivery feedback keys on. Empty for mail that has no
	// message row.
	MessageID string
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
	provider, err := providerHeaderLines(headers, s.sesConfigSet, env.MessageID)
	if err != nil {
		return ProviderResult{}, err
	}
	// Refuse a misconfigured relay before the token is spent. Redeeming first
	// would invalidate the attempt for a failure that had nothing to do with
	// the message, and the caller would burn a fresh ordinal per retry.
	if !s.relay.Configured() {
		return ProviderResult{}, fmt.Errorf("outbound SMTP relay not configured")
	}
	wire := append(provider, stripProviderHeaders(env.Message)...)

	if err := s.gate.RedeemProviderCall(ctx, auth); err != nil {
		return ProviderResult{}, fmt.Errorf("provider authorization: %w", err)
	}

	providerID, sendErr := s.relay.SendOnceContext(ctx, env.From, env.Recipients, wire)
	if sendErr != nil {
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

// providerHeaderLines renders the provider-owned header block from the token's
// view and the deployment configuration — and from nothing else.
//
// Callers cannot pass a tenant name or correlation id; the only way to change
// what SES receives is to change what the gate authorized.
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
	if messageID != "" {
		b.WriteString(delivery.MessageIDHeader + ": " + messageID + "\r\n")
	}
	if h.AttemptCorrelationID != "" {
		b.WriteString(ProviderAttemptHeader + ": " + h.AttemptCorrelationID + "\r\n")
	}
	if h.TenantRequired {
		b.WriteString(SESTenantHeader + ": " + h.TenantName + "\r\n")
	}
	if sesConfigSet != "" {
		b.WriteString(SESConfigurationSetHeader + ": " + sesConfigSet + "\r\n")
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
func stripProviderHeaders(msg []byte) []byte {
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
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 {
			// End of the header section: emit the separator and the body
			// verbatim.
			out = append(out, line...)
			out = append(out, rest...)
			return out
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
	return out
}
