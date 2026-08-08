// Package filterquery parses, validates, and emits boolean filter
// expressions (AIP-160-derived grammar) as parameterized SQL. It is
// schema-agnostic: adopters supply a FieldRegistry; the package knows
// nothing about any caller's tables. See docs/superpowers/specs/
// 2026-07-25-filter-query-language-design.md.
//
// Grammar sharp edges, by design:
//   - One negation per term: NOT NOT x and -NOT x are parse errors.
//   - A leading "-" starts a negation, so unquoted negative values do not
//     parse; quote them (price="-5").
//   - A quoted field name ("name":x) parses, but validation still requires
//     an exact registered field and emission uses registry constants.
package filterquery

import "fmt"

// ErrKind classifies an *Error for the HTTP layer (all map to 400 today;
// the distinction exists for metrics and future client hints).
type ErrKind int

const (
	ErrParse ErrKind = iota
	ErrValidate
	ErrCap
)

// Error is a positioned filter-language error. Pos is a 0-based byte offset
// into the input; -1 when not position-specific. The displayed column is
// therefore byte-based: with multibyte input before the error position it
// overcounts runes. Inputs are short (capped) and the column is a hint, so
// this is accepted rather than paying for rune indexing.
type Error struct {
	Kind ErrKind
	Pos  int
	Msg  string
}

func (e *Error) Error() string {
	if e.Pos >= 0 {
		return fmt.Sprintf("%s (at column %d)", e.Msg, e.Pos+1)
	}
	return e.Msg
}
