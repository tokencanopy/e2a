package filterquery

import (
	"strings"
	"testing"
)

func parseToString(t *testing.T, q string) string {
	t.Helper()
	n, err := parse(q)
	if err != nil {
		t.Fatalf("parse(%q): %v", q, err)
	}
	return sexpr(n)
}

func TestParsePrecedence(t *testing.T) {
	cases := map[string]string{
		// OR binds tighter than implicit AND (sequence); implicit AND binds
		// tighter than explicit AND. NOT binds tightest.
		`a:x b:y OR c:z`:           `(and (a : x) (or (b : y) (c : z)))`,
		`a:x OR b:y c:z`:           `(and (or (a : x) (b : y)) (c : z))`,
		`a:x b:y AND c:z`:          `(and (and (a : x) (b : y)) (c : z))`,
		`a:x AND b:y AND c:z`:      `(and (a : x) (b : y) (c : z))`,
		`NOT a:x`:                  `(not (a : x))`,
		`NOT a:x OR b:y`:           `(or (not (a : x)) (b : y))`,
		`-a:x`:                     `(not (a : x))`,
		`a:x AND (b:y OR c:z)`:     `(and (a : x) (or (b : y) (c : z)))`,
		`(a:x OR b:y) AND NOT c:z`: `(and (or (a : x) (b : y)) (not (c : z)))`,
		`label:urgent OR (from:alerts AND NOT subject:newsletter) created>=2026-07-01`: `(and (or (label : urgent) (and (from : alerts) (not (subject : newsletter)))) (created >= 2026-07-01))`,
	}
	for q, want := range cases {
		if got := parseToString(t, q); got != want {
			t.Errorf("parse(%q) = %s, want %s", q, got, want)
		}
	}
}

func TestParseWhitespaceAroundComparator(t *testing.T) {
	// Restrictions are whitespace-insensitive (AIP-160).
	if got := parseToString(t, `label : urgent`); got != `(label : urgent)` {
		t.Errorf("got %s", got)
	}
	if got := parseToString(t, `label: urgent`); got != `(label : urgent)` {
		t.Errorf("got %s", got)
	}
}

func TestParseBareTerm(t *testing.T) {
	if got := parseToString(t, `hello world`); got != `(and (bare hello) (bare world))` {
		t.Errorf("got %s", got)
	}
}

func TestParseDottedMember(t *testing.T) {
	if got := parseToString(t, `metrics.latency>100`); got != `(metrics.latency > 100)` {
		t.Errorf("got %s", got)
	}
}

func TestParsePunctuationInsideUnquotedValues(t *testing.T) {
	cases := map[string]string{
		`from:alice@x.com`:                 `(from : alice@x.com)`,
		`label:e2a:held`:                   `(label : e2a:held)`,
		`created<2026-07-25T10:30:00Z`:     `(created < 2026-07-25T10:30:00Z)`,
		`created<2026-07-25T10:30:00.123Z`: `(created < 2026-07-25T10:30:00.123Z)`,
	}
	for q, want := range cases {
		if got := parseToString(t, q); got != want {
			t.Errorf("parse(%q) = %s, want %s", q, got, want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		q    string
		pos  int
	}{
		{name: "unclosed paren", q: `(a:x`, pos: 4},
		{name: "stray close", q: `a:x)`, pos: 3},
		{name: "dangling AND", q: `a:x AND`, pos: 7},
		{name: "leading OR", q: `OR a:x`, pos: 0},
		{name: "missing field", q: `:x`, pos: 0},
		{name: "missing value", q: `label:`, pos: 6},
		{name: "malformed dotted member missing segment", q: `a.:y`, pos: 2},
		{name: "malformed dotted member duplicate dot", q: `a..b:y`, pos: 2},
		{name: "NOT missing whitespace", q: `NOT(a:x)`, pos: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.q)
			fe, ok := err.(*Error)
			if !ok {
				t.Fatalf("parse(%q) error type %T, want *Error", tc.q, err)
			}
			if fe.Kind != ErrParse || fe.Pos != tc.pos {
				t.Errorf("parse(%q) error = %+v, want {Kind: ErrParse, Pos: %d}", tc.q, fe, tc.pos)
			}
		})
	}
}

func TestParseCaps(t *testing.T) {
	deep := strings.Repeat("(", 65) + "a:x" + strings.Repeat(")", 65)
	if _, err := parse(deep); err == nil {
		t.Error("depth 65: want error")
	} else if fe, ok := err.(*Error); !ok || fe.Kind != ErrCap {
		t.Errorf("depth error = %v (%T), want ErrCap", err, err)
	}
	var b strings.Builder
	for i := 0; i < 600; i++ {
		b.WriteString("a:x ")
	}
	_, err := parse(b.String())
	fe, ok := err.(*Error)
	if !ok {
		t.Fatalf("600 nodes error type %T, want *Error", err)
	}
	if fe.Kind != ErrCap || fe.Pos != 2048 {
		t.Errorf("600 nodes error = %+v, want {Kind: ErrCap, Pos: 2048}", fe)
	}
}
