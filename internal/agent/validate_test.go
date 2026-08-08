package agent

import (
	"strings"
	"testing"
)

func TestValidateConversationID(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"empty", "", false},
		{"normal", "conv_abc123", false},
		{"with hyphens and dots", "task.2026-04-19.7f3a", false},
		{"contains LF — header injection attempt", "abc\nBcc: leak@evil.com", true},
		{"contains CRLF — header injection attempt", "abc\r\nBcc: leak@evil.com", true},
		{"lone CR", "abc\rdef", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConversationID(tc.id)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateConversationID(%q) err=%v, wantErr=%v", tc.id, err, tc.wantErr)
			}
		})
	}
}

func TestValidateRecipients(t *testing.T) {
	cases := []struct {
		name    string
		groups  [][]string
		wantErr bool
	}{
		{"empty groups", [][]string{}, false},
		{"empty slice", [][]string{{}}, false},
		{"single valid", [][]string{{"alice@example.com"}}, false},
		{"multiple groups, all valid", [][]string{{"a@x.com", "b@x.com"}, {"c@x.com"}, {"d@x.com"}}, false},
		{"display-name form", [][]string{{"Alice Smith <alice@example.com>"}}, false},
		{"IDN domain (Unicode)", [][]string{{"alice@пример.рф"}}, false},
		{"oversized UTF-8 addr-spec", [][]string{{strings.Repeat("😀", 250) + "@b.test"}}, true},
		{"single invalid — no @", [][]string{{"not an email"}}, true},
		{"valid then invalid", [][]string{{"alice@x.com", "garbage"}}, true},
		{"invalid in CC group", [][]string{{"alice@x.com"}, {"oops"}, {"bob@x.com"}}, true},
		{"empty string in slice", [][]string{{"alice@x.com", ""}}, true},
		{"whitespace garbage", [][]string{{"   "}}, true},
		// Typos that LOOK valid pass — SMTP layer handles those (this
		// is the layering we explicitly want; see validateRecipients
		// comment in api.go).
		{"typo but syntactically valid", [][]string{{"bob@gmial.com"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRecipients(tc.groups...)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateRecipients(%v) err=%v, wantErr=%v", tc.groups, err, tc.wantErr)
			}
		})
	}
}

func TestValidateDomain(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantErr   bool
		wantASCII string // expected normalized output on success; "" means don't check
	}{
		// Valid
		{"plain ASCII", "example.com", false, "example.com"},
		{"subdomain", "agents.example.com", false, "agents.example.com"},
		{"multi-level public suffix", "sub.example.co.uk", false, "sub.example.co.uk"},
		{"with hyphen", "my-domain.example.com", false, "my-domain.example.com"},
		{"IDN normalizes to Punycode", "пример.рф", false, "xn--e1afmkfd.xn--p1ai"},
		// Invalid — what today's prod was accepting before the fix
		{"empty", "", true, ""},
		{"whitespace + comma — the bug repro", "not a domain, just garbage", true, ""},
		{"single space", "exam ple.com", true, ""},
		{"no period — bare label", "localhost", true, ""},
		{"trailing newline", "example.com\n", true, ""},
		{"control char", "exa\x00mple.com", true, ""},
		// Length bounds — IDNA enforces 63-char label and 253-char total
		{"label exceeds 63 chars", strings.Repeat("a", 64) + ".com", true, ""},
		// IP literals are not registrable domains (B3): all-numeric
		// labels are valid IDNA, so without an explicit check a bare
		// IPv4 address sails through and registers with a nonsensical
		// wildcard-MX/DKIM record set, burning domain quota on a name
		// that can never verify.
		{"IPv4 literal — private", "10.0.0.5", true, ""},
		{"IPv4 literal — the bug repro", "192.168.1.1", true, ""},
		{"IPv4 literal — broadcast", "255.255.255.255", true, ""},
		{"IPv6 literal — loopback", "::1", true, ""},
		{"IPv6 literal — doc range", "2001:db8::1", true, ""},
		{"IPv6 literal — bracketed", "[2001:db8::1]", true, ""},
		{"IPv4-mapped IPv6 literal", "::ffff:192.168.1.1", true, ""},
		{"full-width-digit IPv4 — normalizes to an IP", "１９２.１６８.１.１", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateDomain(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateDomain(%q) err=%v, wantErr=%v", tc.input, err, tc.wantErr)
				return
			}
			if !tc.wantErr && tc.wantASCII != "" && got != tc.wantASCII {
				t.Errorf("validateDomain(%q) got=%q, want=%q (Punycode normalization)", tc.input, got, tc.wantASCII)
			}
		})
	}
}
