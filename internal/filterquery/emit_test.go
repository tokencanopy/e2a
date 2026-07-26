package filterquery

import (
	"reflect"
	"testing"
)

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
