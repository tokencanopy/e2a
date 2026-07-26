package filterquery

import (
	"fmt"
	"testing"
)

// TestEnumerationPrecedence derives the expected trees from AIP-160's levels:
// expression (explicit AND), sequence (implicit AND), factor (OR), and term
// (NOT). It enumerates every ordered pair of binary separators rather than
// relying on a hand-picked selection of precedence examples.
func TestEnumerationPrecedence(t *testing.T) {
	type operator struct {
		name      string
		separator string
		node      string
	}

	operators := []operator{
		{name: "explicit-and", separator: " AND ", node: "and"},
		{name: "implicit-and", separator: " ", node: "and"},
		{name: "or", separator: " OR ", node: "or"},
	}

	x, y, z := "(tags : a)", "(name : b)", "(price > 1)"
	queries := []string{"tags:a", "name:b", "price>1"}

	// These shapes are an explicit transcription of the AIP-160 grammar, not
	// parser precedence logic. Rows are the first operator; columns are the
	// second. Equal-level operators are n-ary without parentheses.
	naturalShapes := map[string]map[string]string{
		"explicit-and": {
			"explicit-and": "(and %s %s %s)",
			"implicit-and": "(and %s (and %s %s))",
			"or":           "(and %s (or %s %s))",
		},
		"implicit-and": {
			"explicit-and": "(and (and %s %s) %s)",
			"implicit-and": "(and %s %s %s)",
			"or":           "(and %s (or %s %s))",
		},
		"or": {
			"explicit-and": "(and (or %s %s) %s)",
			"implicit-and": "(and (or %s %s) %s)",
			"or":           "(or %s %s %s)",
		},
	}

	combine := func(op, left, right string) string {
		return fmt.Sprintf("(%s %s %s)", op, left, right)
	}
	check := func(t *testing.T, query, want string) {
		t.Helper()
		n, err := parse(query)
		if err != nil {
			t.Fatalf("parse(%q): %v", query, err)
		}
		if got := sexpr(n); got != want {
			t.Errorf("parse(%q) = %s, want %s", query, got, want)
		}
	}

	for _, first := range operators {
		for _, second := range operators {
			pair := fmt.Sprintf("%s+%s", first.name, second.name)
			natural := func(leaves []string) string {
				return fmt.Sprintf(naturalShapes[first.name][second.name], leaves[0], leaves[1], leaves[2])
			}
			query := func(leaves []string) string {
				return leaves[0] + first.separator + leaves[1] + second.separator + leaves[2]
			}

			t.Run(pair+"/natural", func(t *testing.T) {
				check(t, query(queries), natural([]string{x, y, z}))
			})

			for i, position := range []string{"left", "middle", "right"} {
				negatedQueries := append([]string(nil), queries...)
				negatedQueries[i] = "NOT " + negatedQueries[i]
				negatedAST := []string{x, y, z}
				negatedAST[i] = "(not " + negatedAST[i] + ")"
				t.Run(pair+"/not-"+position, func(t *testing.T) {
					check(t, query(negatedQueries), natural(negatedAST))
				})
			}

			t.Run(pair+"/parenthesized-left", func(t *testing.T) {
				q := "(" + queries[0] + first.separator + queries[1] + ")" + second.separator + queries[2]
				want := combine(second.node, combine(first.node, x, y), z)
				check(t, q, want)
			})
			t.Run(pair+"/parenthesized-right", func(t *testing.T) {
				q := queries[0] + first.separator + "(" + queries[1] + second.separator + queries[2] + ")"
				want := combine(first.node, x, combine(second.node, y, z))
				check(t, q, want)
			})
		}
	}
}
