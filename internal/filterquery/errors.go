// Package filterquery parses, validates, and emits boolean filter
// expressions (AIP-160-derived grammar) as parameterized SQL. It is
// schema-agnostic: adopters supply a FieldRegistry; the package knows
// nothing about any caller's tables. See docs/superpowers/specs/
// 2026-07-25-filter-query-language-design.md.
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
// into the input; -1 when not position-specific.
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
