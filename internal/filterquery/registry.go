package filterquery

import (
	"fmt"
	"sort"
	"strings"
)

// Dialect abstracts SQL placeholder style. Task 4 adds the built-in
// Postgres adapter; defining the interface here keeps EmitCtx compilable.
type Dialect interface {
	Placeholder(n int) string
}

// FieldSpec describes one filterable field: which operators it accepts, how
// raw values coerce, and how a validated comparison emits SQL. Coerce and
// Emit must be deterministic and safe for concurrent calls: emission
// re-coerces a private comparison copy on every call.
type FieldSpec struct {
	Name string
	Ops  []string // allowed operators, e.g. []string{":"}
	// Coerce converts the raw value text into the typed value stored on
	// Comparison.Value. quoted reports that the input was a quoted string.
	Coerce func(raw string, quoted bool) (any, error)
	// Emit renders one boolean predicate. Placeholders and args are bound
	// through EmitCtx.PH. Fields binding no value (boolean flags) simply
	// don't call PH.
	Emit func(c *Comparison, e *EmitCtx) (string, error)
}

// EmitCtx hands FieldSpec.Emit sequential placeholders and collects args.
type EmitCtx struct {
	dialect Dialect
	next    int
	args    []any
}

// PH records arg and returns its placeholder (e.g. "$4").
func (e *EmitCtx) PH(arg any) string {
	ph := e.dialect.Placeholder(e.next)
	e.next++
	e.args = append(e.args, arg)
	return ph
}

// Registry is the adopter-supplied field set. Construct once at startup;
// safe for concurrent use after construction.
type Registry struct {
	fields map[string]FieldSpec
}

func NewRegistry(specs ...FieldSpec) (*Registry, error) {
	r := &Registry{fields: make(map[string]FieldSpec, len(specs))}
	for _, s := range specs {
		if s.Name == "" || len(s.Ops) == 0 || s.Coerce == nil || s.Emit == nil {
			return nil, fmt.Errorf("filterquery: FieldSpec %q needs Name, Ops, Coerce and Emit", s.Name)
		}
		if _, dup := r.fields[s.Name]; dup {
			return nil, fmt.Errorf("filterquery: duplicate field %q", s.Name)
		}
		s.Ops = append([]string(nil), s.Ops...)
		r.fields[s.Name] = s
	}
	return r, nil
}

// Names returns sorted field names, for error messages and docs.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.fields))
	for n := range r.fields {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Validate checks the AST against the registry and coerces every value in
// place (Comparison.Value). Bare terms are rejected here — the grammar
// accepts them, the v1 vocabulary does not.
func (r *Registry) Validate(n Node) error {
	if r == nil {
		return &Error{Kind: ErrValidate, Pos: -1, Msg: "filterquery: registry is required"}
	}
	return r.validateNode(n, newTraversalState())
}

func (r *Registry) validateNode(n Node, state *traversalState) error {
	leave, err := state.enter(n)
	if err != nil {
		return err
	}
	defer leave()

	switch t := n.(type) {
	case *And:
		if len(t.Terms) == 0 {
			return &Error{Kind: ErrValidate, Pos: t.At, Msg: "filterquery: cannot validate empty AND"}
		}
		for _, x := range t.Terms {
			if err := r.validateNode(x, state); err != nil {
				return err
			}
		}
	case *Or:
		if len(t.Terms) == 0 {
			return &Error{Kind: ErrValidate, Pos: t.At, Msg: "filterquery: cannot validate empty OR"}
		}
		for _, x := range t.Terms {
			if err := r.validateNode(x, state); err != nil {
				return err
			}
		}
	case *Not:
		return r.validateNode(t.X, state)
	case *Bare:
		return &Error{Kind: ErrValidate, Pos: t.At, Msg: fmt.Sprintf("bare term %q is not supported — qualify it with a field (%s)", t.Text, strings.Join(r.Names(), ", "))}
	case *Comparison:
		return r.validateComparison(t)
	default:
		return &Error{Kind: ErrValidate, Pos: -1, Msg: fmt.Sprintf("filterquery: unknown node type %T", n)}
	}
	return nil
}

func (r *Registry) validateComparison(t *Comparison) error {
	spec, ok := r.fields[t.Field]
	if !ok {
		return &Error{Kind: ErrValidate, Pos: t.At, Msg: fmt.Sprintf("unknown field %q — supported fields: %s", t.Field, strings.Join(r.Names(), ", "))}
	}
	allowed := false
	for _, op := range spec.Ops {
		if op == t.Op {
			allowed = true
			break
		}
	}
	if !allowed {
		return &Error{Kind: ErrValidate, Pos: t.At, Msg: fmt.Sprintf("operator %q is not allowed on field %q (allowed: %s)", t.Op, t.Field, strings.Join(spec.Ops, " "))}
	}
	v, err := spec.Coerce(t.Raw, t.Quoted)
	if err != nil {
		return &Error{Kind: ErrValidate, Pos: t.At, Msg: fmt.Sprintf("invalid value for %q: %s", t.Field, err)}
	}
	t.Value = v
	t.validatedBy = r
	return nil
}
