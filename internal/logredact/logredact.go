// Package logredact keeps personal data out of process logs.
//
// Container stdout is shipped to centralized log storage with long retention
// and broad queryability, so log lines are NOT a private scratchpad: an email
// subject, an external human's address, or a free-text rejection reason in a
// log line is personal data replicated outside the database's access controls.
// The policy:
//
//   - Never log message subject lines — log subject_len instead.
//   - Never log an EXTERNAL human's full email address — log only its domain
//     (AddressDomain) or, for recipient lists, the count plus the distinct
//     domain set (AddressDomains). e2a AGENT addresses are our own namespace
//     and the primary tracing key, so they stay in full — but only once the
//     address has RESOLVED to an agent row. An unresolved recipient is just an
//     attacker-supplied string (SMTP RCPT runs before any authentication) and
//     is redacted like any external address.
//   - Truncate connecting IPs to a network prefix (IPNetwork): /24 for IPv4,
//     /48 for IPv6 — enough for abuse/rate-limit debugging, less identifying.
//   - Cap free-text human input and third-party error bodies (Truncate).
//   - Redact at the LOG SINK, not at the source. A value that is also stored,
//     served by the API, or emitted in a webhook (e.g. a HITL rejection
//     reason) must stay precise there; only its log copy is reduced.
//
// Only the subject rule is machine-checked, by the tripwire scan in
// logguard_test.go; the rest is a code-review responsibility.
//
// Every helper here runs on hostile external input on the SMTP hot path and
// must never panic; malformed input degrades to a safe placeholder.
package logredact

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"unicode"
)

// invalidPlaceholder is returned when the input cannot be interpreted at all.
// It deliberately carries no part of the original value.
const invalidPlaceholder = "invalid"

const (
	// maxDomainRunes bounds a single logged domain (RFC 1035 caps real domain
	// names at 253 octets; anything longer is hostile input).
	maxDomainRunes = 253
	// maxDomains bounds the distinct-domain set for recipient lists so a
	// hostile 1000-recipient submit cannot bloat a log line.
	maxDomains = 10
)

// AddressDomain reduces an email address to only its domain, e.g.
// "alice@example.com" -> "example.com". It tolerates display-name forms
// ("Alice <alice@example.com>"), angle brackets, and the RFC 5322 comment
// form ("alice@example.com (Alice Smith)"), lowercases the result, and
// returns "invalid" for anything whose domain part is not hostname-shaped.
// It never returns the local part, a display name, or a comment.
func AddressDomain(addr string) string {
	addr = strings.TrimSpace(addr)
	// Tolerate "Display Name <local@domain>" and bare "<local@domain>".
	if i := strings.LastIndexByte(addr, '<'); i >= 0 {
		addr = strings.TrimSpace(addr[i+1:])
		addr = strings.TrimSuffix(addr, ">")
	}
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return invalidPlaceholder
	}
	domain := strings.ToLower(strings.TrimSpace(addr[at+1:]))
	// Cut at the first character that cannot occur inside a hostname. Without
	// this, the RFC 5322 comment form leaks a PERSON'S NAME as the "domain"
	// ("alice@example.com (Alice Smith)" -> "example.com (alice smith)"), and
	// a stray unmatched bracket rides along ("example.com>").
	if i := strings.IndexFunc(domain, notHostnameRune); i >= 0 {
		domain = domain[:i]
	}
	if !hostnameShaped(domain) {
		return invalidPlaceholder
	}
	return Truncate(domain, maxDomainRunes)
}

// notHostnameRune reports whether r cannot appear inside a hostname, and so
// terminates the domain. Kept as a denylist of the structural/quoting runes
// RFC 5322 puts around an address, plus all whitespace.
func notHostnameRune(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("()<>[]{},;:\"'\\@", r)
}

// hostnameShaped reports whether s looks like a domain name: non-empty, no
// leading or trailing dot, and built only from letters, digits, dots, hyphens
// and underscores. Unicode letters count, so IDN labels ("bücher.example")
// pass through unchanged; anything else degrades to the invalid placeholder
// rather than being logged.
func hostnameShaped(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
		case r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// AddressDomains reduces a recipient list to its distinct, sorted domain set,
// bounded at maxDomains entries (a "+N more" marker replaces the overflow).
// Use together with the list's length, e.g.:
//
//	log.Printf("... to_count=%d to_domains=%v", len(to), logredact.AddressDomains(to))
func AddressDomains(addrs []string) []string {
	seen := make(map[string]struct{}, len(addrs))
	domains := make([]string, 0, len(addrs))
	for _, a := range addrs {
		d := AddressDomain(a)
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		domains = append(domains, d)
	}
	sort.Strings(domains)
	if len(domains) > maxDomains {
		overflow := len(domains) - maxDomains
		domains = append(domains[:maxDomains:maxDomains], fmt.Sprintf("+%d more", overflow))
	}
	return domains
}

// IPNetwork truncates an IP address to its network prefix: /24 for IPv4,
// /48 for IPv6. It accepts bare addresses, "host:port" forms, and bracketed
// IPv6 literals; anything unparsable (including a non-IP transport address)
// returns "invalid".
func IPNetwork(ip string) string {
	s := strings.TrimSpace(ip)
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	parsed := net.ParseIP(s)
	if parsed == nil {
		return invalidPlaceholder
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return parsed.Mask(net.CIDRMask(48, 128)).String() + "/48"
}

// Truncate caps s at max runes, appending "…" when anything was cut. It is
// rune-safe (never splits a UTF-8 sequence) and returns "" for max <= 0.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
