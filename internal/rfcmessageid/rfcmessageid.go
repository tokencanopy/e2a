// Package rfcmessageid parses and canonicalizes RFC 5322 message identifiers.
//
// Parsing is deliberately bounded. A header value may contain at most
// MaxInputBytes bytes and an individual bracketed identifier may contain at
// most MaxIdentifierBytes bytes. The parser is iterative (including for nested
// comments), and the number and total size of returned identifiers are
// therefore also bounded by MaxInputBytes.
package rfcmessageid

import (
	"errors"
	"fmt"
)

const (
	// MaxInputBytes is the largest Message-ID, In-Reply-To, or References
	// field value Parse will inspect. It is generous enough for long folded
	// References chains while placing a fixed bound on parser work and output.
	MaxInputBytes = 64 << 10

	// MaxIdentifierBytes is the largest individual msg-id, including its
	// angle brackets. A msg-id cannot fold internally, so RFC 5322's maximum
	// physical line length provides a conservative wire-format bound.
	MaxIdentifierBytes = 998

	// MaxTokens is the maximum number of nearest, distinct identifiers returned
	// from one field. The byte bound protects parsing; this independent count
	// bound protects downstream exact-anchor resolution from turning one long
	// References field into thousands of database probes.
	MaxTokens = 256
)

var (
	// ErrInvalidSyntax reports a value that does not contain a valid sequence
	// of bracketed RFC message identifiers (optionally surrounded by obsolete
	// reply-header phrase syntax).
	ErrInvalidSyntax = errors.New("invalid RFC message-id syntax")

	// ErrInputTooLarge reports a header value or individual identifier that
	// exceeds the documented parser bounds.
	ErrInputTooLarge = errors.New("RFC message-id input too large")
)

// Token contains both representations of one parsed msg-id. Original is the
// exact bracketed byte sequence from the field value. Canonical preserves the
// identifier-left bytes and lowercases ASCII bytes only in identifier-right.
type Token struct {
	Original  string
	Canonical string
}

// Parse extracts canonical msg-id tokens from an In-Reply-To or
// References-style field value, including obsolete phrase wrappers.
//
// Tokens are returned with angle brackets, in wire order. The identifier-left
// bytes are preserved exactly and ASCII uppercase bytes in the identifier-right
// are lowercased. Duplicate canonical tokens are removed by keeping their last
// occurrence, so iterating the result right-to-left retains RFC resolution
// precedence. Empty and CFWS-only values return nil, nil.
func Parse(value string) ([]string, error) {
	tokens, err := ParseTokens(value)
	if err != nil {
		return nil, err
	}
	if tokens == nil {
		return nil, nil
	}
	ids := make([]string, len(tokens))
	for offset := range tokens {
		ids[offset] = tokens[offset].Canonical
	}
	return ids, nil
}

// ParseTokens extracts msg-id tokens while retaining each exact bracketed
// token alongside its canonical lookup form. Duplicate identity is determined
// by Canonical, and the Token from the last occurrence is retained in wire
// order. At most MaxTokens nearest (rightmost) distinct tokens are returned.
func ParseTokens(value string) ([]Token, error) {
	tokens, err := scanTokens(value, true)
	if err != nil {
		return nil, err
	}
	tokens = keepLastDuplicates(tokens)
	if len(tokens) > MaxTokens {
		tokens = tokens[len(tokens)-MaxTokens:]
	}
	return tokens, nil
}

// Canonicalize validates a field value containing exactly one msg-id token,
// with optional surrounding comments or folding whitespace, and returns its
// canonical bracketed form. It is intended for callers such as outbound
// adapters that have one provider-qualified wire identifier rather than a
// References chain.
func Canonicalize(value string) (string, error) {
	tokens, err := scanTokens(value, false)
	if err != nil {
		return "", err
	}
	if len(tokens) != 1 {
		return "", ErrInvalidSyntax
	}
	return tokens[0].Canonical, nil
}

func scanTokens(value string, allowObsoletePhrases bool) ([]Token, error) {
	if len(value) > MaxInputBytes {
		return nil, ErrInputTooLarge
	}
	if err := validateControls(value); err != nil {
		return nil, err
	}

	var tokens []Token
	sawObsoletePhrase := false
	for offset := 0; ; {
		next, err := skipCFWS(value, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		if offset == len(value) {
			if len(tokens) == 0 && sawObsoletePhrase {
				return nil, ErrInvalidSyntax
			}
			return tokens, nil
		}
		if value[offset] != '<' {
			if !allowObsoletePhrases {
				return nil, invalidAt(offset)
			}
			next, err := skipObsoletePhrase(value, offset)
			if err != nil {
				return nil, err
			}
			sawObsoletePhrase = true
			offset = next
			continue
		}

		token, next, err := parseToken(value, offset)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
		offset = next
	}
}

func parseToken(value string, start int) (Token, int, error) {
	offset := start + 1
	leftStart := offset
	if offset < len(value) && value[offset] == '"' {
		var err error
		offset, err = consumeQuotedString(value, offset)
		if err != nil {
			return Token{}, 0, err
		}
	} else {
		offset = consumeDotAtomText(value, offset)
	}
	if offset == leftStart || offset >= len(value) || value[offset] != '@' {
		return Token{}, 0, invalidAt(offset)
	}
	left := value[leftStart:offset]
	offset++

	rightStart := offset
	if offset < len(value) && value[offset] == '[' {
		offset++
		for offset < len(value) && isDText(value[offset]) {
			offset++
		}
		if offset >= len(value) || value[offset] != ']' {
			return Token{}, 0, invalidAt(offset)
		}
		offset++
	} else {
		domainStart := offset
		offset = consumeDotAtomText(value, offset)
		if offset == domainStart {
			return Token{}, 0, invalidAt(offset)
		}
	}

	right := value[rightStart:offset]
	if offset >= len(value) || value[offset] != '>' {
		return Token{}, 0, invalidAt(offset)
	}
	offset++
	if offset-start > MaxIdentifierBytes {
		return Token{}, 0, ErrInputTooLarge
	}

	return Token{
		Original:  value[start:offset],
		Canonical: "<" + left + "@" + lowerASCII(right) + ">",
	}, offset, nil
}

func consumeQuotedString(value string, start int) (int, error) {
	for offset := start + 1; offset < len(value); {
		switch value[offset] {
		case '"':
			return offset + 1, nil
		case '\\':
			offset++
			if offset >= len(value) || !isQuotedPair(value[offset]) {
				return 0, invalidAt(offset)
			}
			offset++
		case '\r':
			// validateControls proved CRLF followed by WSP.
			offset += 2
		default:
			if !isQuotedStringText(value[offset]) {
				return 0, invalidAt(offset)
			}
			offset++
		}
	}
	return 0, invalidAt(len(value))
}

func isQuotedStringText(b byte) bool {
	return b == ' ' || b == '\t' ||
		(b >= 33 && b <= 126 && b != '"' && b != '\\')
}

func consumeDotAtomText(value string, offset int) int {
	if offset >= len(value) || !isAText(value[offset]) {
		return offset
	}

	for {
		for offset < len(value) && isAText(value[offset]) {
			offset++
		}
		if offset >= len(value) || value[offset] != '.' {
			return offset
		}
		if offset+1 >= len(value) || !isAText(value[offset+1]) {
			return offset
		}
		offset++
	}
}

func isAText(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z':
		return true
	case b >= 'a' && b <= 'z':
		return true
	case b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '/', '=',
		'?', '^', '_', '`', '{', '|', '}', '~':
		return true
	default:
		return false
	}
}

func isDText(b byte) bool {
	return b >= 33 && b <= 126 && b != '[' && b != ']' && b != '\\'
}

func skipCFWS(value string, offset int) (int, error) {
	for offset < len(value) {
		switch value[offset] {
		case ' ', '\t':
			offset++
		case '\r':
			// validateControls already proved this is CRLF followed by WSP.
			offset += 2
		case '(':
			next, err := skipComment(value, offset)
			if err != nil {
				return 0, err
			}
			offset = next
		default:
			return offset, nil
		}
	}
	return offset, nil
}

// skipObsoletePhrase accepts the RFC 5322 obs-in-reply-to/obs-references
// envelope around msg-id tokens. The topology-bearing identifiers themselves
// remain strictly parsed; printable phrase text is ignored, stray closing
// brackets are rejected, and quoted strings/comments are bounded by the same
// field-size and control validation as the rest of the parser.
func skipObsoletePhrase(value string, offset int) (int, error) {
	for offset < len(value) && value[offset] != '<' {
		switch value[offset] {
		case '>':
			return 0, invalidAt(offset)
		case '"':
			next, err := consumeQuotedString(value, offset)
			if err != nil {
				return 0, err
			}
			offset = next
		case '(':
			next, err := skipComment(value, offset)
			if err != nil {
				return 0, err
			}
			offset = next
		case '\r':
			offset += 2
		default:
			if value[offset] < 32 || value[offset] == 127 {
				return 0, invalidAt(offset)
			}
			offset++
		}
	}
	return offset, nil
}

func skipComment(value string, start int) (int, error) {
	depth := 1
	for offset := start + 1; offset < len(value); {
		switch value[offset] {
		case '(':
			depth++
			offset++
		case ')':
			depth--
			offset++
			if depth == 0 {
				return offset, nil
			}
		case '\\':
			offset++
			if offset >= len(value) || !isQuotedPair(value[offset]) {
				return 0, invalidAt(offset)
			}
			offset++
		case '\r':
			// validateControls already proved this is CRLF followed by WSP.
			offset += 2
		default:
			if !isCommentText(value[offset]) {
				return 0, invalidAt(offset)
			}
			offset++
		}
	}
	return 0, invalidAt(len(value))
}

func isCommentText(b byte) bool {
	return b == ' ' || b == '\t' ||
		(b >= 33 && b <= 126 && b != '(' && b != ')' && b != '\\')
}

func isQuotedPair(b byte) bool {
	return b == ' ' || b == '\t' || b >= 33 && b <= 126
}

func validateControls(value string) error {
	for offset := 0; offset < len(value); offset++ {
		switch b := value[offset]; {
		case b == '\r':
			if offset+2 >= len(value) || value[offset+1] != '\n' ||
				(value[offset+2] != ' ' && value[offset+2] != '\t') {
				return invalidAt(offset)
			}
		case b == '\n':
			if offset == 0 || value[offset-1] != '\r' {
				return invalidAt(offset)
			}
		case b == '\t':
			// HTAB is legal folding whitespace; its placement is checked by
			// the grammar parser.
		case b < 32 || b == 127:
			return invalidAt(offset)
		}
	}
	return nil
}

func lowerASCII(value string) string {
	var lowered []byte
	for offset := 0; offset < len(value); offset++ {
		if value[offset] < 'A' || value[offset] > 'Z' {
			continue
		}
		if lowered == nil {
			lowered = []byte(value)
		}
		lowered[offset] += 'a' - 'A'
	}
	if lowered == nil {
		return value
	}
	return string(lowered)
}

func keepLastDuplicates(tokens []Token) []Token {
	if len(tokens) < 2 {
		return tokens
	}

	seen := make(map[string]struct{}, len(tokens))
	result := make([]Token, 0, len(tokens))
	for offset := len(tokens) - 1; offset >= 0; offset-- {
		if _, duplicate := seen[tokens[offset].Canonical]; duplicate {
			continue
		}
		seen[tokens[offset].Canonical] = struct{}{}
		result = append(result, tokens[offset])
	}
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}

func invalidAt(offset int) error {
	return fmt.Errorf("%w at byte %d", ErrInvalidSyntax, offset)
}
