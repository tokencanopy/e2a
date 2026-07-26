package filterquery

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

type dollarDialect struct{}

func (dollarDialect) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }

// toyRegistry backs the package's conformance and unit tests: a fake
// "products" table with text/int/bool/text[] columns. It proves the package
// is schema-agnostic — no e2a concepts involved.
func toyRegistry(t *testing.T) *Registry {
	t.Helper()
	reg, err := NewRegistry(
		FieldSpec{
			Name: "name",
			Ops:  []string{":", "=", "!="},
			Coerce: func(raw string, quoted bool) (any, error) {
				if raw == "" {
					return nil, fmt.Errorf("empty name")
				}
				return raw, nil
			},
			Emit: func(c *Comparison, e *EmitCtx) (string, error) {
				v := c.Value.(string)
				switch c.Op {
				case ":":
					return `p.name ILIKE ` + e.PH("%"+v+"%") + ` ESCAPE '\'`, nil
				case "=":
					return "LOWER(p.name) = LOWER(" + e.PH(v) + ")", nil
				default:
					return "LOWER(p.name) != LOWER(" + e.PH(v) + ")", nil
				}
			},
		},
		FieldSpec{
			Name: "price",
			Ops:  []string{"=", "!=", "<", "<=", ">", ">="},
			Coerce: func(raw string, quoted bool) (any, error) {
				n, err := strconv.Atoi(raw)
				if err != nil {
					return nil, fmt.Errorf("expected integer, got %q", raw)
				}
				return n, nil
			},
			Emit: func(c *Comparison, e *EmitCtx) (string, error) {
				return "p.price " + c.Op + " " + e.PH(c.Value.(int)), nil
			},
		},
		FieldSpec{
			Name: "active",
			Ops:  []string{"=", "!="},
			Coerce: func(raw string, quoted bool) (any, error) {
				switch raw {
				case "true":
					return true, nil
				case "false":
					return false, nil
				}
				return nil, fmt.Errorf("expected true or false, got %q", raw)
			},
			Emit: func(c *Comparison, e *EmitCtx) (string, error) {
				return "p.active " + c.Op + " " + e.PH(c.Value.(bool)), nil
			},
		},
		FieldSpec{
			Name: "tags",
			Ops:  []string{":"},
			Coerce: func(raw string, quoted bool) (any, error) {
				if raw == "" {
					return nil, fmt.Errorf("empty tag")
				}
				return raw, nil
			},
			Emit: func(c *Comparison, e *EmitCtx) (string, error) {
				return "p.tags @> " + e.PH([]string{c.Value.(string)}), nil
			},
		},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

func validateToErr(t *testing.T, reg *Registry, q string) error {
	t.Helper()
	n, err := parse(q)
	if err != nil {
		t.Fatalf("parse(%q): %v", q, err)
	}
	return reg.Validate(n)
}

func TestValidateUnknownField(t *testing.T) {
	reg := toyRegistry(t)
	err := validateToErr(t, reg, `color:red`)
	fe, ok := err.(*Error)
	if !ok || fe.Kind != ErrValidate {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if !strings.Contains(fe.Msg, `unknown field "color"`) || !strings.Contains(fe.Msg, "active") {
		t.Errorf("msg should name the field and list supported fields: %q", fe.Msg)
	}
}

func TestValidateBareTermRejected(t *testing.T) {
	reg := toyRegistry(t)
	err := validateToErr(t, reg, `hello`)
	fe, ok := err.(*Error)
	if !ok || fe.Kind != ErrValidate || fe.Pos != 0 || !strings.Contains(fe.Msg, "bare term") {
		t.Errorf("err = %v, want bare-term rejection", err)
	}
}

func TestValidateOperatorNotAllowed(t *testing.T) {
	reg := toyRegistry(t)
	err := validateToErr(t, reg, `tags=new`)
	fe, ok := err.(*Error)
	if !ok || fe.Kind != ErrValidate || fe.Pos != 0 || !strings.Contains(fe.Msg, `operator "=" is not allowed on field "tags"`) {
		t.Errorf("err = %v", err)
	}
}

func TestValidateCoercion(t *testing.T) {
	reg := toyRegistry(t)
	if err := validateToErr(t, reg, `price:notanumber`); err == nil {
		t.Error("want coercion error — wait, ':' is not allowed on price either; still an error")
	}
	if err := validateToErr(t, reg, `price=abc`); err == nil {
		t.Error("want integer coercion error")
	} else if fe, ok := err.(*Error); !ok || fe.Kind != ErrValidate || fe.Pos != 0 {
		t.Errorf("err = %v, want validation error at 0", err)
	}
	if err := validateToErr(t, reg, `active=maybe`); err == nil {
		t.Error("want bool coercion error")
	} else if fe, ok := err.(*Error); !ok || fe.Kind != ErrValidate || fe.Pos != 0 {
		t.Errorf("err = %v, want validation error at 0", err)
	}
	n, err := parse(`price>=42`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := reg.Validate(n); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	cmp := n.(*Comparison)
	if cmp.Value != 42 {
		t.Errorf("coerced value = %v (%T), want 42 int", cmp.Value, cmp.Value)
	}
}

func TestValidateRecurses(t *testing.T) {
	reg := toyRegistry(t)
	if err := validateToErr(t, reg, `name:ok AND (price>1 OR bogus:field)`); err == nil {
		t.Error("want unknown-field error from inside the tree")
	} else if fe, ok := err.(*Error); !ok || fe.Kind != ErrValidate || fe.Pos != 24 {
		t.Errorf("err = %v, want validation error at 24", err)
	}
	if err := validateToErr(t, reg, `name:ok AND (price>1 OR NOT tags:sale)`); err != nil {
		t.Errorf("valid expression rejected: %v", err)
	}
}

func TestRegistryNamesSortedFresh(t *testing.T) {
	reg := toyRegistry(t)
	names := reg.Names()
	if got, want := strings.Join(names, ","), "active,name,price,tags"; got != want {
		t.Errorf("Names() = %q, want %q", got, want)
	}
	names[0] = "changed"
	if got, want := strings.Join(reg.Names(), ","), "active,name,price,tags"; got != want {
		t.Errorf("Names() returned aliased output: got %q, want %q", got, want)
	}
}

func TestNameEmitEscapesWithOneBackslash(t *testing.T) {
	reg := toyRegistry(t)
	n, err := parse(`name:x`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := reg.Validate(n); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	ctx := &EmitCtx{dialect: dollarDialect{}, next: 1}
	sql, err := reg.fields["name"].Emit(n.(*Comparison), ctx)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if want := `p.name ILIKE $1 ESCAPE '\'`; sql != want {
		t.Errorf("SQL = %q, want %q", sql, want)
	}
	if got, want := fmt.Sprint(ctx.args), "[%x%]"; got != want {
		t.Errorf("args = %s, want %s", got, want)
	}
}

func TestNewRegistryRejectsBadSpecs(t *testing.T) {
	if _, err := NewRegistry(FieldSpec{Name: "x"}); err == nil {
		t.Error("missing Coerce/Emit: want error")
	}
	if _, err := NewRegistry(FieldSpec{
		Name: "x", Coerce: func(s string, q bool) (any, error) { return s, nil },
		Emit: func(c *Comparison, e *EmitCtx) (string, error) { return "1=1", nil },
	}); err == nil {
		t.Error("missing Ops: want error")
	}
	mk := func() FieldSpec {
		return FieldSpec{Name: "x", Ops: []string{":"}, Coerce: func(s string, q bool) (any, error) { return s, nil }, Emit: func(c *Comparison, e *EmitCtx) (string, error) { return "1=1", nil }}
	}
	if _, err := NewRegistry(mk(), mk()); err == nil {
		t.Error("duplicate field: want error")
	}
}

func TestNewRegistryCopiesOps(t *testing.T) {
	ops := []string{":"}
	reg, err := NewRegistry(FieldSpec{
		Name: "x", Ops: ops,
		Coerce: func(s string, q bool) (any, error) { return s, nil },
		Emit:   func(c *Comparison, e *EmitCtx) (string, error) { return "1=1", nil },
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ops[0] = "="
	n, err := parse(`x:value`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := reg.Validate(n); err != nil {
		t.Errorf("caller mutation changed registry operators: %v", err)
	}
}
