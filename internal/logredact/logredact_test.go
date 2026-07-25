package logredact

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestAddressDomain(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain address", "alice@example.com", "example.com"},
		{"uppercase is normalized", "Alice@EXAMPLE.COM", "example.com"},
		{"display-name form", "Alice Example <alice@example.com>", "example.com"},
		{"bare angle brackets", "<alice@example.com>", "example.com"},
		{"surrounding whitespace", "  alice@example.com  ", "example.com"},
		{"plus tag stripped with local part", "alice+tag@example.com", "example.com"},
		{"quoted local part with @", `"a@b"@example.com`, "example.com"},
		{"subdomain", "bob@mail.corp.example.co.uk", "mail.corp.example.co.uk"},
		{"unicode domain", "user@bücher.example", "bücher.example"},
		{"no at sign", "not-an-address", "invalid"},
		{"empty string", "", "invalid"},
		{"only at sign", "@", "invalid"},
		{"trailing at sign", "alice@", "invalid"},
		{"whitespace only", "   ", "invalid"},
		{"null bytes", "a\x00b", "invalid"},
		// RFC 5322 comment form: the parenthesised display name is a PERSON'S
		// NAME and must never come back as part of the "domain".
		{"comment form", "alice@example.com (Alice Smith)", "example.com"},
		{"comment form no space", "alice@example.com(Alice Smith)", "example.com"},
		{"display name plus comment", "Alice Smith <alice@example.com> (work)", "example.com"},
		{"stray closing bracket", "alice@example.com>", "example.com"},
		{"stray opening bracket", "alice@<example.com", "invalid"},
		{"trailing semicolon", "alice@example.com;", "example.com"},
		{"trailing comma in a header list", "alice@example.com, bob@other.test", "other.test"},
		{"address literal", "alice@[192.168.0.1]", "invalid"},
		{"leading dot", "alice@.example.com", "invalid"},
		{"trailing dot", "alice@example.com.", "invalid"},
		{"domain is only punctuation", "alice@---", "---"},
		{"control characters in domain", "alice@exa\x01mple.com", "invalid"},
		{"quoted domain", `alice@"example.com"`, "invalid"},
		{"tab separated comment", "alice@example.com\t(Alice)", "example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AddressDomain(tt.in); got != tt.want {
				t.Errorf("AddressDomain(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestAddressDomainNeverReturnsLocalPart is the property the whole package
// exists for: whatever the input shape, the local part must not leak.
func TestAddressDomainNeverReturnsLocalPart(t *testing.T) {
	for _, in := range []string{
		"secret-local@example.com",
		"Secret Local <secret-local@example.com>",
		"<secret-local@example.com>",
		"secret-local@",
		"secret-local",
	} {
		if got := AddressDomain(in); strings.Contains(got, "secret-local") {
			t.Errorf("AddressDomain(%q) = %q leaks the local part", in, got)
		}
	}
}

// TestAddressDomainNeverReturnsDisplayName covers the other half of the
// property: a human's NAME is personal data too, and RFC 5322 lets it sit on
// either side of the address (angle-bracket form before it, comment form
// after it). Neither may survive into a log line.
func TestAddressDomainNeverReturnsDisplayName(t *testing.T) {
	for _, in := range []string{
		"alice@example.com (Secret Name)",
		"alice@example.com(Secret Name)",
		"Secret Name <alice@example.com>",
		"Secret Name <alice@example.com> (Secret Name)",
		"alice@example.com (Secret Name) <alice@example.com>",
		"alice@example.com \"Secret Name\"",
	} {
		if got := AddressDomain(in); strings.Contains(strings.ToLower(got), "secret") {
			t.Errorf("AddressDomain(%q) = %q leaks the display name", in, got)
		}
	}
}

func TestAddressDomainBoundsHostileLength(t *testing.T) {
	hostile := "a@" + strings.Repeat("x", 100_000)
	got := AddressDomain(hostile)
	if n := len([]rune(got)); n > maxDomainRunes+1 { // +1 for the ellipsis
		t.Errorf("AddressDomain on hostile input returned %d runes, want <= %d", n, maxDomainRunes+1)
	}
}

func TestAddressDomains(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil list", nil, []string{}},
		{"empty list", []string{}, []string{}},
		{"distinct and sorted", []string{"z@zeta.com", "a@acme.com", "b@zeta.com"}, []string{"acme.com", "zeta.com"}},
		{"malformed entries collapse to invalid", []string{"nope", "also-nope", "ok@acme.com"}, []string{"acme.com", "invalid"}},
		{"case-insensitive dedupe", []string{"a@Acme.COM", "b@acme.com"}, []string{"acme.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AddressDomains(tt.in)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("AddressDomains(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestAddressDomainsBoundsListLength(t *testing.T) {
	var addrs []string
	for i := 0; i < 100; i++ {
		addrs = append(addrs, fmt.Sprintf("user@domain-%03d.example", i))
	}
	got := AddressDomains(addrs)
	if len(got) != maxDomains+1 {
		t.Fatalf("len(AddressDomains(100 distinct)) = %d, want %d (cap + overflow marker)", len(got), maxDomains+1)
	}
	if got[maxDomains] != "+90 more" {
		t.Errorf("overflow marker = %q, want %q", got[maxDomains], "+90 more")
	}
}

func TestIPNetwork(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"ipv4", "203.0.113.99", "203.0.113.0/24"},
		{"ipv4 with port", "203.0.113.99:52731", "203.0.113.0/24"},
		{"ipv6", "2001:db8:85a3:8d3:1319:8a2e:370:7348", "2001:db8:85a3::/48"},
		{"ipv6 bracketed with port", "[2001:db8:85a3:8d3::1]:25", "2001:db8:85a3::/48"},
		{"ipv4-mapped ipv6", "::ffff:203.0.113.99", "203.0.113.0/24"},
		{"loopback", "127.0.0.1", "127.0.0.0/24"},
		{"nil net.IP String()", "<nil>", "invalid"},
		{"empty", "", "invalid"},
		{"hostname", "mail.example.com", "invalid"},
		{"unix socket path", "/var/run/proxy.sock", "invalid"},
		{"garbage", "999.999.999.999", "invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IPNetwork(tt.in); got != tt.want {
				t.Errorf("IPNetwork(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter than max", "hello", 10, "hello"},
		{"exactly max", "hello", 5, "hello"},
		{"truncated", "hello world", 5, "hello…"},
		{"zero max", "hello", 0, ""},
		{"negative max", "hello", -1, ""},
		{"empty input", "", 5, ""},
		{"multibyte runes not split", "héllo wörld", 6, "héllo …"},
		{"emoji not split", "🙂🙂🙂🙂", 2, "🙂🙂…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Truncate(tt.in, tt.max); got != tt.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
		})
	}
}

// TestHelpersNeverPanic fuzzes the helpers with adversarial shapes by hand:
// they all run on hostile external input inside the SMTP receive path, where
// a panic would take down mail acceptance.
func TestHelpersNeverPanic(t *testing.T) {
	inputs := []string{
		"", " ", "@", "@@", "<", ">", "<>", "<@>", "a@", "@b",
		"\x00", "\xff\xfe", strings.Repeat("@", 10_000),
		strings.Repeat("<", 10_000), "a@b@c@d", "[::1]", "[",
		"a@b (", "a@b )", "a@(((", strings.Repeat("a@b (c) ", 1_000),
		"名前 <ユーザー@例え.テスト>", string(rune(0xD800)), // lone surrogate
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic on input %q: %v", in, r)
				}
			}()
			_ = AddressDomain(in)
			_ = AddressDomains([]string{in, in})
			_ = IPNetwork(in)
			_ = Truncate(in, 3)
			_ = Truncate(in, len(in)+1)
		}()
	}
}
