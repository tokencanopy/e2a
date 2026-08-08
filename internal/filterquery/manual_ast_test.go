package filterquery

import "testing"

func requireFilterError(t *testing.T, err error) *Error {
	t.Helper()
	if err == nil {
		t.Fatal("want error")
	}
	fe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	return fe
}

func manualComparison(pos int) *Comparison {
	return &Comparison{Field: "name", Op: ":", Raw: "value", At: pos}
}

func TestValidateRejectsMalformedManualAST(t *testing.T) {
	reg := toyRegistry(t)
	var nilRegistry *Registry
	requireFilterError(t, nilRegistry.Validate(manualComparison(0)))

	var nilAnd *And
	var nilOr *Or
	var nilNot *Not
	var nilBare *Bare
	var nilComparison *Comparison
	cases := []struct {
		name string
		node Node
	}{
		{"typed nil and", nilAnd},
		{"typed nil or", nilOr},
		{"typed nil not", nilNot},
		{"typed nil bare", nilBare},
		{"typed nil comparison", nilComparison},
		{"empty and", &And{At: 1}},
		{"empty or", &Or{At: 1}},
		{"nil and child", &And{At: 1, Terms: []Node{nil}}},
		{"nil not child", &Not{At: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireFilterError(t, reg.Validate(tc.node))
		})
	}
}

func TestManualASTTraversalGuards(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		reg := toyRegistry(t)
		cycle := &And{At: 7}
		cycle.Terms = []Node{cycle}
		requireFilterError(t, reg.Validate(cycle))
		requireFilterError(t, emitManual(reg, cycle))
	})

	t.Run("depth", func(t *testing.T) {
		reg := toyRegistry(t)
		var node Node = manualComparison(65)
		for i := 0; i < 65; i++ {
			node = &Not{At: i, X: node}
		}
		if err := reg.Validate(node); requireFilterError(t, err).Kind != ErrCap {
			t.Errorf("Validate depth error kind = %v, want ErrCap", err.(*Error).Kind)
		}
		if err := emitManual(reg, node); requireFilterError(t, err).Kind != ErrCap {
			t.Errorf("Emit depth error kind = %v, want ErrCap", err.(*Error).Kind)
		}
	})

	t.Run("nodes", func(t *testing.T) {
		reg := toyRegistry(t)
		terms := make([]Node, maxNodes)
		for i := range terms {
			terms[i] = manualComparison(i)
		}
		node := &And{At: 0, Terms: terms}
		if err := reg.Validate(node); requireFilterError(t, err).Kind != ErrCap {
			t.Errorf("Validate node error kind = %v, want ErrCap", err.(*Error).Kind)
		}
		if err := emitManual(reg, node); requireFilterError(t, err).Kind != ErrCap {
			t.Errorf("Emit node error kind = %v, want ErrCap", err.(*Error).Kind)
		}
	})
}

func TestEmitManualBareReportsPosition(t *testing.T) {
	err := emitManual(toyRegistry(t), &Bare{Text: "unqualified", At: 12})
	if got := requireFilterError(t, err).Pos; got != 12 {
		t.Errorf("Emit bare error position = %d, want 12", got)
	}
}

func emitManual(reg *Registry, node Node) error {
	_, _, err := reg.Emit(node, PostgresDialect{}, 1)
	return err
}
