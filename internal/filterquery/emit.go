package filterquery

import (
	"fmt"
	"strings"
)

// PostgresDialect uses $n placeholders.
type PostgresDialect struct{}

func (PostgresDialect) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }

// Emit renders a validated AST as a parenthesized boolean SQL fragment plus
// its bound args. Placeholders are numbered from start (1-based index of the
// next free parameter in the caller's query).
func (r *Registry) Emit(n Node, d Dialect, start int) (string, []any, error) {
	if r == nil {
		return "", nil, &Error{Kind: ErrValidate, Pos: -1, Msg: "filterquery: registry is required"}
	}
	if d == nil {
		return "", nil, &Error{Kind: ErrValidate, Pos: -1, Msg: "filterquery: dialect is required"}
	}
	if start < 1 {
		return "", nil, &Error{Kind: ErrValidate, Pos: -1, Msg: "filterquery: placeholder start must be at least 1"}
	}
	ctx := &EmitCtx{dialect: d, next: start}
	frag, err := r.emitNode(n, ctx)
	if err != nil {
		return "", nil, err
	}
	return frag, ctx.args, nil
}

func (r *Registry) emitNode(n Node, ctx *EmitCtx) (string, error) {
	switch t := n.(type) {
	case *And:
		parts := make([]string, len(t.Terms))
		for i, x := range t.Terms {
			s, err := r.emitNode(x, ctx)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		return "(" + strings.Join(parts, " AND ") + ")", nil
	case *Or:
		parts := make([]string, len(t.Terms))
		for i, x := range t.Terms {
			s, err := r.emitNode(x, ctx)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		return "(" + strings.Join(parts, " OR ") + ")", nil
	case *Not:
		s, err := r.emitNode(t.X, ctx)
		if err != nil {
			return "", err
		}
		return "(NOT " + s + ")", nil
	case *Comparison:
		if t.validatedBy != r {
			return "", &Error{Kind: ErrValidate, Pos: t.At, Msg: "filterquery: comparison was not validated by this registry"}
		}
		spec, ok := r.fields[t.Field]
		if !ok {
			return "", &Error{Kind: ErrValidate, Pos: t.At, Msg: "filterquery: validated field is missing from registry"}
		}
		leaf, err := spec.Emit(t, ctx)
		if err != nil {
			return "", err
		}
		return "(" + leaf + ")", nil
	default:
		return "", &Error{Kind: ErrValidate, Pos: -1, Msg: fmt.Sprintf("filterquery: cannot emit node type %T", n)}
	}
}

// Expr is a parsed + validated filter, ready to emit at any placeholder
// offset. Safe for concurrent Emit calls: the AST is immutable after Parse,
// and Emit constructs a fresh EmitCtx per call.
type Expr struct {
	root Node
	reg  *Registry
}

// Parse lexes, parses, and validates q. Empty/whitespace-only q yields a nil
// Expr and nil error (no constraint).
func Parse(q string, reg *Registry) (*Expr, error) {
	if strings.TrimSpace(q) == "" {
		return nil, nil
	}
	if reg == nil {
		return nil, &Error{Kind: ErrValidate, Pos: -1, Msg: "filterquery: registry is required"}
	}
	n, err := parse(q)
	if err != nil {
		return nil, err
	}
	if err := reg.Validate(n); err != nil {
		return nil, err
	}
	return &Expr{root: n, reg: reg}, nil
}

// Empty reports a nil (no-constraint) Expr.
func (e *Expr) Empty() bool { return e == nil }

// Emit renders the expression with placeholders numbered from start.
func (e *Expr) Emit(d Dialect, start int) (string, []any, error) {
	if e == nil {
		return "", nil, nil
	}
	return e.reg.Emit(e.root, d, start)
}

// Compile is the one-call pipeline for callers that know the placeholder
// offset up front. Handlers that discover the offset later should use Parse
// and defer Emit.
func Compile(q string, reg *Registry, d Dialect, start int) (string, []any, error) {
	expr, err := Parse(q, reg)
	if err != nil || expr == nil {
		return "", nil, err
	}
	return expr.Emit(d, start)
}
