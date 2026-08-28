package identity

import (
	"net/mail"
	"strings"
)

// NormalizeEmail returns the canonical lookup form of an email address:
// lower-cased, with surrounding whitespace stripped. Every external-input
// email used as a lookup key (path vars, form fields, OAuth consent
// choices, WebSocket subscriptions) must funnel through this so case
// variants ("Alice@x.com" vs "alice@x.com") resolve to the same row.
//
// Per RFC 5321 §2.4 the local-part is technically case-sensitive — a
// small number of providers (most famously ProtonMail) preserve case.
// We collapse it anyway because consistency across HTTP path, JSON body,
// SMTP envelope, and dashboard URL is more important than spec purity
// for the agent-inbox use case. Document this if a user complains.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// UniqueRecipientCount returns the number of distinct recipients across the
// given address lists, normalized via NormalizeEmail (lower-case + trim) so
// "Alice@x.com" in To and "alice@x.com" in CC count once. Empty entries are
// skipped. This is THE canonical recipient-unit primitive: outbound metering
// (one recipient-delivery per unit), the accept-time cap pre-check, the
// fire-time quota gate, and the sendramp reservation must all count with this
// one function — two normalizers that ever disagree would desynchronize the
// metered units from the persisted message_recipients rows.
func UniqueRecipientCount(lists ...[]string) int {
	seen := make(map[string]struct{})
	for _, list := range lists {
		for _, recipient := range list {
			recipient = NormalizeEmail(recipient)
			if recipient != "" {
				seen[recipient] = struct{}{}
			}
		}
	}
	return len(seen)
}

// NormalizeMailboxAddress returns the canonical addr-spec from an RFC 5322
// mailbox value. Suppression checks receive both bare addresses and display-
// name forms from outbound request shapes, but storage keys contain only the
// addr-spec. Invalid values fall back to NormalizeEmail so callers that have
// their own validation keep the historical lookup behavior.
func NormalizeMailboxAddress(value string) string {
	parsed, err := mail.ParseAddress(strings.TrimSpace(value))
	if err == nil {
		return NormalizeEmail(parsed.Address)
	}
	return NormalizeEmail(value)
}
