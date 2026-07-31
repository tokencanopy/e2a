package rfcmessageid

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseCanonicalizesDomainOnly(t *testing.T) {
	got, err := Parse("<CaseSensitive.Left@MAIL.Example.COM>")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"<CaseSensitive.Left@mail.example.com>"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %v, want %v", got, want)
	}
}

func TestParseAcceptsCFWSAroundTokens(t *testing.T) {
	value := " (first <ignored> (nested\\) comment))\r\n\t<one@EXAMPLE.COM> (between)\t<two@[IPv6:ABCD::1]> "

	got, err := Parse(value)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"<one@example.com>", "<two@[ipv6:abcd::1]>"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %v, want %v", got, want)
	}
}

func TestParseAcceptsObsoleteReplyPhrasesAroundTokens(t *testing.T) {
	got, err := ParseTokens(`answer to "the earlier note" <one@EXAMPLE.COM> and then <two@example.com>`)
	if err != nil {
		t.Fatalf("ParseTokens: %v", err)
	}
	want := []Token{
		{Original: "<one@EXAMPLE.COM>", Canonical: "<one@example.com>"},
		{Original: "<two@example.com>", Canonical: "<two@example.com>"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseTokens = %#v, want %#v", got, want)
	}
}

func TestParseAcceptsObsoletePhraseFoldingWhitespace(t *testing.T) {
	for _, value := range []string{
		"reply-to\t<one@EXAMPLE.COM>",
		"reply-to\r\n\t<one@EXAMPLE.COM>",
		"reply-to\r\n <one@EXAMPLE.COM>",
	} {
		got, err := ParseTokens(value)
		if err != nil {
			t.Fatalf("ParseTokens(%q): %v", value, err)
		}
		want := []Token{{
			Original:  "<one@EXAMPLE.COM>",
			Canonical: "<one@example.com>",
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ParseTokens(%q) = %#v, want %#v", value, got, want)
		}
	}
}

func TestParseAcceptsObsoleteQuotedIdentifierLeft(t *testing.T) {
	got, err := Parse(`<"Case Sensitive"@MAIL.EXAMPLE.COM>`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{`<"Case Sensitive"@mail.example.com>`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %#v, want %#v", got, want)
	}
}

func TestParseKeepsLastDuplicateInWireOrder(t *testing.T) {
	got, err := Parse("<first@EXAMPLE.COM> <second@example.com> <first@example.com> <third@example.com> <second@EXAMPLE.COM>")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"<first@example.com>", "<third@example.com>", "<second@example.com>"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %v, want %v", got, want)
	}
}

func TestParseTokensReturnsLastOriginalAndCanonicalForms(t *testing.T) {
	got, err := ParseTokens("<First@EXAMPLE.COM> <second@EXAMPLE.COM> <First@example.com>")
	if err != nil {
		t.Fatalf("ParseTokens: %v", err)
	}
	want := []Token{
		{Original: "<second@EXAMPLE.COM>", Canonical: "<second@example.com>"},
		{Original: "<First@example.com>", Canonical: "<First@example.com>"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseTokens = %#v, want %#v", got, want)
	}
}

func TestParseTreatsIdentifierLeftAsCaseSensitive(t *testing.T) {
	got, err := Parse("<Left@example.com> <left@EXAMPLE.COM>")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"<Left@example.com>", "<left@example.com>"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %v, want %v", got, want)
	}
}

func TestParseEmptyOrCFWSOnlyReturnsNoTokens(t *testing.T) {
	for _, value := range []string{"", " \t", "(nothing here)", "\r\n\t(comment)"} {
		got, err := Parse(value)
		if err != nil {
			t.Fatalf("Parse(%q): %v", value, err)
		}
		if got != nil {
			t.Fatalf("Parse(%q) = %#v, want nil", value, got)
		}
	}
}

func TestParseRejectsMalformedValues(t *testing.T) {
	tests := []string{
		"bare@example.com",
		"<missing-at.example.com>",
		"<@example.com>",
		"<left@>",
		"<left..dot@example.com>",
		"<.left@example.com>",
		"<left.@example.com>",
		"<left@example..com>",
		"<left@.example.com>",
		"<left@example.com.>",
		"<left@@example.com>",
		"<left @example.com>",
		"<left@(comment)example.com>",
		"<left@example.com",
		"left@example.com>",
		"<<left@example.com>>",
		"<left@[IPv6:ABCD::1>",
		"<left@[bad]literal>",
		"<left@[bad\\literal]>",
		"<left@éxample.com>",
		"(unterminated <left@example.com>",
		"(bad\\)",
	}

	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			if got, err := Parse(value); !errors.Is(err, ErrInvalidSyntax) {
				t.Fatalf("Parse(%q) = %v, %v; want ErrInvalidSyntax", value, got, err)
			}
		})
	}
}

func TestParseRejectsControlCharactersAndInvalidFolding(t *testing.T) {
	tests := []string{
		"<left@\x00example.com>",
		"<left@example.com>\x7f",
		"<left@example.com>\n <next@example.com>",
		"<left@example.com>\r <next@example.com>",
		"<left@example.com>\r\n<next@example.com>",
		"(comment\x01)<left@example.com>",
	}

	for _, value := range tests {
		if got, err := Parse(value); !errors.Is(err, ErrInvalidSyntax) {
			t.Fatalf("Parse(%q) = %v, %v; want ErrInvalidSyntax", value, got, err)
		}
	}
}

func TestParseRejectsOversizedInput(t *testing.T) {
	value := strings.Repeat(" ", MaxInputBytes+1)

	if got, err := Parse(value); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("Parse(oversized) = %v, %v; want ErrInputTooLarge", got, err)
	}
}

func TestParseRejectsOversizedIdentifier(t *testing.T) {
	value := "<" + strings.Repeat("a", MaxIdentifierBytes-len("<@example.com>")+1) + "@example.com>"

	if got, err := Parse(value); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("Parse(oversized identifier) = %v, %v; want ErrInputTooLarge", got, err)
	}
}

func TestParseAcceptsIdentifierAtSizeLimit(t *testing.T) {
	left := strings.Repeat("a", MaxIdentifierBytes-len("<@example.com>"))
	value := "<" + left + "@example.com>"

	got, err := Parse(value)
	if err != nil {
		t.Fatalf("Parse(identifier at limit): %v", err)
	}
	if len(got) != 1 || got[0] != value {
		t.Fatalf("Parse(identifier at limit) returned %d unexpected tokens", len(got))
	}
}

func TestParseTokensKeepsOnlyNearestIdentifiersAtTokenLimit(t *testing.T) {
	var value strings.Builder
	for i := 0; i < MaxTokens+5; i++ {
		fmt.Fprintf(&value, "<id-%03d@example.com>", i)
	}

	got, err := ParseTokens(value.String())
	if err != nil {
		t.Fatalf("ParseTokens: %v", err)
	}
	if len(got) != MaxTokens {
		t.Fatalf("ParseTokens returned %d tokens, want hard cap %d", len(got), MaxTokens)
	}
	if got[0].Canonical != "<id-005@example.com>" {
		t.Fatalf("first retained token = %q, want oldest token inside nearest-ID window", got[0].Canonical)
	}
	if got[len(got)-1].Canonical != fmt.Sprintf("<id-%03d@example.com>", MaxTokens+4) {
		t.Fatalf("last retained token = %q, want rightmost wire token", got[len(got)-1].Canonical)
	}
}

func TestCanonicalizeRequiresExactlyOneToken(t *testing.T) {
	got, err := Canonicalize(" (provider) <Wire.Left@MAIL.EXAMPLE.COM> ")
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if want := "<Wire.Left@mail.example.com>"; got != want {
		t.Fatalf("Canonicalize = %q, want %q", got, want)
	}

	for _, value := range []string{
		"",
		"(comment only)",
		"<one@example.com> <two@example.com>",
		"<same@example.com> <same@EXAMPLE.COM>",
	} {
		if got, err := Canonicalize(value); !errors.Is(err, ErrInvalidSyntax) {
			t.Fatalf("Canonicalize(%q) = %q, %v; want ErrInvalidSyntax", value, got, err)
		}
	}
}
