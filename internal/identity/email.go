package identity

import (
	"net/mail"
	"strings"

	"golang.org/x/net/idna"
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
// given address lists, normalized via NormalizeMailboxAddress (addr-spec
// extraction + lower-case + trim) so "Bob <b@x.com>" in To and "b@x.com" in
// CC count once, as do case variants. Empty entries are skipped. This is THE
// canonical recipient-unit primitive: outbound metering (one
// recipient-delivery per unit), the accept-time cap pre-check, the approve
// probe, and the fire-time quota gate must all count with this one function —
// counting raw request strings would over-charge display-name duplicates at
// accept relative to the compose-normalized set the terminal meter and the
// SMTP envelope actually see. For already-normalized bare addresses (the
// terminal meter's stored to/cc/bcc, the claim's envelope recipients) this is
// byte-identical to NormalizeEmail.
func UniqueRecipientCount(lists ...[]string) int {
	seen := make(map[string]struct{})
	for _, list := range lists {
		for _, recipient := range list {
			recipient = NormalizeMailboxAddress(recipient)
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

// SuppressionLookupForms returns every spelling under which a suppression of
// the normalized address may be stored: the address itself plus, for an
// internationalized domain, its A-label (punycode) and Unicode spellings.
// Provider feedback reports an IDN recipient in A-label form while a
// customer may type it in Unicode (or the reverse), and both kinds of row
// land in the same table — so a send-time lookup that compared only the
// typed spelling would let a bounced or complained Unicode address through.
// A domain the IDNA lookup profile refuses contributes only its raw form.
func SuppressionLookupForms(normalized string) []string {
	forms := []string{normalized}
	at := strings.LastIndexByte(normalized, '@')
	if at <= 0 || at == len(normalized)-1 {
		return forms
	}
	local, domain := normalized[:at+1], normalized[at+1:]
	for _, conv := range []func(string) (string, error){idna.Lookup.ToASCII, idna.Lookup.ToUnicode} {
		if d, err := conv(domain); err == nil && d != "" {
			if f := local + strings.ToLower(d); f != forms[0] && (len(forms) < 2 || f != forms[1]) {
				forms = append(forms, f)
			}
		}
	}
	return forms
}

// suppressionLookupSet expands normalized addresses into every stored
// spelling SuppressionLookupForms names, deduplicated.
func suppressionLookupSet(normalized []string) []string {
	seen := make(map[string]struct{}, len(normalized))
	out := make([]string, 0, len(normalized))
	for _, n := range normalized {
		for _, f := range SuppressionLookupForms(n) {
			if _, ok := seen[f]; !ok {
				seen[f] = struct{}{}
				out = append(out, f)
			}
		}
	}
	return out
}
