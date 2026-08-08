package filterquery

import (
	"reflect"
	"strconv"
	"sync"
	"testing"
)

type nilReceiverDialect struct {
	prefix string
}

func (d *nilReceiverDialect) Placeholder(int) string { return d.prefix }

func compileToy(t *testing.T, q string, start int) (string, []any) {
	t.Helper()
	frag, args, err := Compile(q, toyRegistry(t), PostgresDialect{}, start)
	if err != nil {
		t.Fatalf("Compile(%q): %v", q, err)
	}
	return frag, args
}

func TestEmitLeaves(t *testing.T) {
	frag, args := compileToy(t, `name:widget`, 1)
	if frag != `(p.name ILIKE $1 ESCAPE '\')` {
		t.Errorf("frag = %s", frag)
	}
	if !reflect.DeepEqual(args, []any{"%widget%"}) {
		t.Errorf("args = %v", args)
	}

	frag, args = compileToy(t, `price>=42`, 3)
	if frag != `(p.price >= $3)` || !reflect.DeepEqual(args, []any{42}) {
		t.Errorf("frag=%s args=%v", frag, args)
	}

	frag, args = compileToy(t, `tags:sale`, 1)
	if frag != `(p.tags @> $1)` || !reflect.DeepEqual(args, []any{[]string{"sale"}}) {
		t.Errorf("frag=%s args=%v", frag, args)
	}
}

func TestEmitBooleanTree(t *testing.T) {
	frag, args := compileToy(t, `name:a OR (price>1 AND NOT tags:x)`, 1)
	want := `((p.name ILIKE $1 ESCAPE '\') OR ((p.price > $2) AND (NOT (p.tags @> $3))))`
	if frag != want {
		t.Errorf("frag =\n%s\nwant\n%s", frag, want)
	}
	if !reflect.DeepEqual(args, []any{"%a%", 1, []string{"x"}}) {
		t.Errorf("args = %v", args)
	}
}

func TestEmitStartOffset(t *testing.T) {
	_, args1 := compileToy(t, `name:a price>1`, 1)
	frag2, args2 := compileToy(t, `name:a price>1`, 4)
	if frag2 != `((p.name ILIKE $4 ESCAPE '\') AND (p.price > $5))` {
		t.Errorf("frag2 = %s", frag2)
	}
	if !reflect.DeepEqual(args1, args2) {
		t.Errorf("args differ with offset: %v vs %v", args1, args2)
	}
}

func TestCompileEmpty(t *testing.T) {
	for _, q := range []string{"", "   ", "\t\n"} {
		frag, args, err := Compile(q, toyRegistry(t), PostgresDialect{}, 1)
		if err != nil || frag != "" || args != nil {
			t.Errorf("Compile(%q) = (%q, %v, %v), want empty", q, frag, args, err)
		}
	}
}

func TestExprReuseAtDifferentOffsets(t *testing.T) {
	expr, err := Parse(`name:a`, toyRegistry(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if expr.Empty() {
		t.Error("Empty() = true for non-empty q")
	}
	f1, a1, err := expr.Emit(PostgresDialect{}, 1)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	f9, a9, err := expr.Emit(PostgresDialect{}, 9)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if f1 != `(p.name ILIKE $1 ESCAPE '\')` || f9 != `(p.name ILIKE $9 ESCAPE '\')` {
		t.Errorf("f1=%s f9=%s", f1, f9)
	}
	if !reflect.DeepEqual(a1, a9) {
		t.Errorf("args differ: %v vs %v", a1, a9)
	}
}

func TestEmitWithoutValidateFails(t *testing.T) {
	// Guard: emitting a raw (unvalidated) AST errors instead of panicking.
	n, err := parse(`name:a`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	reg := toyRegistry(t)
	_, _, err = reg.Emit(n, PostgresDialect{}, 1) // Comparison.Value is nil
	if err == nil {
		t.Error("want error emitting unvalidated comparison")
	}
}

func TestEmitRejectsInvalidContextAndRegistryMismatch(t *testing.T) {
	reg := toyRegistry(t)
	expr, err := Parse(`name:a`, reg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, _, err := expr.Emit(nil, 1); err == nil {
		t.Error("nil dialect: want error")
	}
	if _, _, err := expr.Emit(PostgresDialect{}, 0); err == nil {
		t.Error("start=0: want error")
	}
	other := toyRegistry(t)
	if _, _, err := other.Emit(expr.root, PostgresDialect{}, 1); err == nil {
		t.Error("different registry: want validation-provenance error")
	}
	if _, err := Parse(`name:a`, nil); err == nil {
		t.Error("nil registry: want error")
	}
}

func TestEmitRejectsTypedNilDialect(t *testing.T) {
	expr, err := Parse(`name:a`, toyRegistry(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var dialect *nilReceiverDialect
	if _, _, err := expr.Emit(dialect, 1); err == nil {
		t.Error("typed-nil dialect: want error")
	} else if _, ok := err.(*Error); !ok {
		t.Errorf("err = %T, want *Error", err)
	}
}

func TestEmitRejectsMalformedNodes(t *testing.T) {
	var nilAnd *And
	var nilOr *Or
	var nilNot *Not
	var nilComparison *Comparison
	cases := []struct {
		name string
		node Node
	}{
		{"typed nil and", nilAnd},
		{"typed nil or", nilOr},
		{"typed nil not", nilNot},
		{"typed nil comparison", nilComparison},
		{"empty and", &And{}},
		{"empty or", &Or{}},
		{"nil child", &And{Terms: []Node{nil}}},
		{"typed nil child", &And{Terms: []Node{nilComparison}}},
	}
	reg := toyRegistry(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := reg.Emit(tc.node, PostgresDialect{}, 1); err == nil {
				t.Error("Emit: want error")
			} else if _, ok := err.(*Error); !ok {
				t.Errorf("err = %T, want *Error", err)
			}
		})
	}
}

func TestEmitRevalidatesComparisonCopy(t *testing.T) {
	t.Run("value mutation is recoerced", func(t *testing.T) {
		expr, err := Parse(`name:a`, toyRegistry(t))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		expr.root.(*Comparison).Value = 99
		frag, args, err := expr.Emit(PostgresDialect{}, 1)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if frag != `(p.name ILIKE $1 ESCAPE '\')` || !reflect.DeepEqual(args, []any{"%a%"}) {
			t.Errorf("Emit = (%q, %v), want original raw value", frag, args)
		}
	})

	for _, mutation := range []struct {
		name  string
		apply func(*Comparison)
	}{
		{"field", func(c *Comparison) { c.Field = "price" }},
		{"operator", func(c *Comparison) { c.Op = ">" }},
	} {
		t.Run("invalid "+mutation.name, func(t *testing.T) {
			expr, err := Parse(`name:a`, toyRegistry(t))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			mutation.apply(expr.root.(*Comparison))
			if _, _, err := expr.Emit(PostgresDialect{}, 1); err == nil {
				t.Error("Emit: want validation error")
			} else if _, ok := err.(*Error); !ok {
				t.Errorf("err = %T, want *Error", err)
			}
		})
	}
}

func TestEmitCallbackCannotMutateExpr(t *testing.T) {
	reg, err := NewRegistry(FieldSpec{
		Name:   "value",
		Ops:    []string{":"},
		Coerce: func(raw string, quoted bool) (any, error) { return raw, nil },
		Emit: func(c *Comparison, e *EmitCtx) (string, error) {
			c.Value = "callback mutation"
			return "p.value = " + e.PH(c.Value), nil
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	expr, err := Parse(`value:original`, reg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, _, err := expr.Emit(PostgresDialect{}, 1); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := expr.root.(*Comparison).Value; got != "original" {
		t.Errorf("shared comparison Value = %v, want original", got)
	}
}

func TestExprConcurrentEmit(t *testing.T) {
	expr, err := Parse(`name:a price>1`, toyRegistry(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	const workers = 32
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()
			frag, args, err := expr.Emit(PostgresDialect{}, start)
			if err != nil {
				errs <- err
				return
			}
			want := `((p.name ILIKE $` + strconv.Itoa(start) + ` ESCAPE '\') AND (p.price > $` + strconv.Itoa(start+1) + `))`
			if frag != want || !reflect.DeepEqual(args, []any{"%a%", 1}) {
				errs <- &Error{Kind: ErrValidate, Pos: -1, Msg: "unexpected concurrent emission"}
			}
		}(i + 1)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
