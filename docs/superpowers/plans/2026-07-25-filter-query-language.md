# Filter Query Language (`q`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a boolean filter query language (`q` param) to `list_messages`, backed by a new dependency-free, schema-agnostic Go package `internal/filterquery` (parse → validate → emit parameterized Postgres SQL).

**Architecture:** Spec: `docs/superpowers/specs/2026-07-25-filter-query-language-design.md`. Hand-rolled recursive-descent parser over the AIP-160 EBNF; validation against an adopter-supplied `FieldRegistry`; emission through a `Dialect` interface. e2a's messages schema lives only in `internal/identity`'s registry. Ships as PR 1 (package only) + PR 2 (API surface + MCP + SDKs + CLI + docs).

**Tech Stack:** Go (stdlib only for the package — zero new dependencies), pgx v5, Postgres 16, huma (HTTP), zod (MCP, TS), OpenAPI Generator v7.16.0 (SDK regen).

**Spec errata (apply with Task 1's commit):** the spec's worked example header says "precedence: implicit AND > OR" — the correct AIP-160 precedence (and what this plan implements) is **`NOT` > `OR` > implicit AND (sequence) > explicit AND**. Evidence: the official EBNF (`expression = sequence {AND sequence}`; `sequence = factor {WS factor}`; `factor = term {OR term}`) and LUCI's doc example: `New York Giants OR Yankees` ≡ `New York (Giants OR Yankees)`. The spec example's AST is unchanged (its parens settle the grouping either way). Fix the spec line in `docs/superpowers/specs/2026-07-25-filter-query-language-design.md` to: `AST (precedence: NOT > OR > implicit AND > explicit AND)`.

## Global Constraints

- Worktree: `~/Desktop/e2a-worktrees/filter-query-language`, branch `feat/filter-query-language`. Run all commands from the worktree root.
- Zero new Go module dependencies in PR 1 (`internal/filterquery` is stdlib-only).
- Grammar precedence is AIP-160's: `NOT` > `OR` > implicit AND > explicit AND. `AND`/`OR`/`NOT` are case-sensitive keywords.
- Comparator syntax follows AIP-160 directly: use `created>=2026-07-01`, not
  an invalid colon-prefixed comparator form.
- Date-only values denote a whole UTC calendar day: `<=` includes that entire
  day and `>` begins at the following midnight (Josh decision, 2026-07-26).
- Dotted unquoted values such as `from:alice@x.com` must parse as one value;
  the dot token is structural only while parsing a field member.
- Go imports use the module path `github.com/tokencanopy/e2a/...`.
- Hard caps: input ≤ 500 chars (handler), nesting depth ≤ 64, nodes ≤ 512. Violations return positioned errors, never panics.
- Placeholders are always 1-based `$n`, numbered from a caller-supplied start index; identifiers come only from registry constants; user values are always bound args.
- DSL semantics MUST match flat params: `from:` = `m.sender ILIKE … ESCAPE '\'` (same as flat `from`), `subject:` = `m.subject ILIKE … ESCAPE '\'` (same as flat `subject_contains`), both via the existing `escapeLikePattern` helper; `*` in values maps to `%` (after escaping literals).
- `label:` values must match `^[a-z0-9:_-]{1,64}$`; `e2a:` system labels are filterable (read-only on writes, unchanged).
- DB-backed tests need Postgres: `make docker-up` (pg at localhost:5433), then `go test ./internal/identity/ -run <Test>`.
- Commit style: `feat(filterquery): …` / `feat(api): …` with trailer `Co-Authored-By: Kimi <noreply@moonshot.ai>`. Commit at every task boundary.
- After PR-2 tasks regenerate OpenAPI/SDKs: `make spec-check generate-sdk-check openapi-compat-check` must pass.

---

## PR 1 — `internal/filterquery` package

### Task 1: Lexer + errors

**Files:**
- Create: `internal/filterquery/errors.go`
- Create: `internal/filterquery/lexer.go`
- Test: `internal/filterquery/lexer_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `Error{Kind ErrKind, Pos int, Msg string}` with kinds `ErrParse, ErrValidate, ErrCap`; `token{kind tokenKind, text string, pos int, quoted bool}`; `lex(src string) ([]token, error)`. Token kinds: `tEOF, tWS, tText, tString, tAnd, tOr, tNot, tLParen, tRParen, tColon, tEq, tNeq, tLt, tLe, tGt, tGe, tMinus, tDot`. Later tasks rely on these exact names.

- [ ] **Step 1: Write the failing tests**

```go
package filterquery

import "testing"

func TestLexBasics(t *testing.T) {
	toks, err := lex(`label:urgent OR (from:"alice@x.com" AND NOT has:attachment)`)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	want := []tokenKind{tText, tColon, tText, tWS, tOr, tWS, tLParen, tText, tColon, tString, tWS, tAnd, tWS, tNot, tWS, tText, tColon, tText, tRParen, tEOF}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens %v, want %d", len(toks), toks, len(want))
	}
	for i, w := range want {
		if toks[i].kind != w {
			t.Errorf("tok[%d].kind = %v, want %v (%q)", i, toks[i].kind, w, toks[i].text)
		}
	}
}

func TestLexKeywordsAreCaseSensitive(t *testing.T) {
	toks, err := lex(`and or not AND OR NOT And`)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	var kinds []tokenKind
	for _, tk := range toks {
		if tk.kind != tWS && tk.kind != tEOF {
			kinds = append(kinds, tk.kind)
		}
	}
	want := []tokenKind{tText, tText, tText, tAnd, tOr, tNot, tText}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("kinds[%d] = %v, want %v", i, kinds[i], want[i])
		}
	}
}

func TestLexDateStaysOneToken(t *testing.T) {
	toks, err := lex(`created>=2026-07-01`)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	// '-' inside a text run is part of the text, not a minus token.
	want := []tokenKind{tText, tGe, tText, tEOF}
	if len(toks) != len(want) {
		t.Fatalf("got %v, want %d tokens", toks, len(want))
	}
	if toks[2].text != "2026-07-01" {
		t.Errorf("date text = %q", toks[2].text)
	}
}

func TestLexStringEscapes(t *testing.T) {
	toks, err := lex(`subject:"a \"quoted\" \\ value"`)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	if toks[2].kind != tString || toks[2].text != `a "quoted" \ value` {
		t.Errorf("string token = %+v", toks[2])
	}
}

func TestLexErrors(t *testing.T) {
	for _, q := range []string{
		`subject:"unterminated`,
		`subject:"bad \q escape"`,
		`label!urgent`,
	} {
		if _, err := lex(q); err == nil {
			t.Errorf("lex(%q) = nil error, want error", q)
		}
	}
}

func TestLexPositions(t *testing.T) {
	_, err := lex(`label!urgent`)
	fe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err type %T, want *Error", err)
	}
	if fe.Kind != ErrParse || fe.Pos != 5 {
		t.Errorf("Error = %+v, want {Kind: ErrParse, Pos: 5}", fe)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/filterquery/ -v`
Expected: FAIL — `internal/filterquery` does not exist / undefined `lex`, `Error`.

- [ ] **Step 3: Implement errors.go and lexer.go**

`internal/filterquery/errors.go`:

```go
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
```

`internal/filterquery/lexer.go`:

```go
package filterquery

import (
	"fmt"
	"strings"
)

type tokenKind int

const (
	tEOF tokenKind = iota
	tWS
	tText
	tString
	tAnd
	tOr
	tNot
	tLParen
	tRParen
	tColon
	tEq
	tNeq
	tLt
	tLe
	tGt
	tGe
	tMinus
	tDot
)

type token struct {
	kind   tokenKind
	text   string // tText/tString literal; tString is unescaped contents
	pos    int    // 0-based byte offset of first char
	quoted bool   // true for tString
}

type lexer struct {
	src string
	i   int
}

func lex(src string) ([]token, error) {
	lx := &lexer{src: src}
	var toks []token
	for {
		t, err := lx.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
		if t.kind == tEOF {
			return toks, nil
		}
	}
}

func (l *lexer) next() (token, error) {
	if l.i >= len(l.src) {
		return token{kind: tEOF, pos: len(l.src)}, nil
	}
	start := l.i
	c := l.src[l.i]
	switch {
	case isSpace(c):
		for l.i < len(l.src) && isSpace(l.src[l.i]) {
			l.i++
		}
		return token{kind: tWS, pos: start}, nil
	case c == '(':
		l.i++
		return token{kind: tLParen, text: "(", pos: start}, nil
	case c == ')':
		l.i++
		return token{kind: tRParen, text: ")", pos: start}, nil
	case c == ':':
		l.i++
		return token{kind: tColon, text: ":", pos: start}, nil
	case c == '=':
		l.i++
		return token{kind: tEq, text: "=", pos: start}, nil
	case c == '!':
		if l.i+1 < len(l.src) && l.src[l.i+1] == '=' {
			l.i += 2
			return token{kind: tNeq, text: "!=", pos: start}, nil
		}
		return token{}, &Error{Kind: ErrParse, Pos: start, Msg: "unexpected '!' (did you mean '!=')"}
	case c == '<':
		l.i++
		if l.i < len(l.src) && l.src[l.i] == '=' {
			l.i++
			return token{kind: tLe, text: "<=", pos: start}, nil
		}
		return token{kind: tLt, text: "<", pos: start}, nil
	case c == '>':
		l.i++
		if l.i < len(l.src) && l.src[l.i] == '=' {
			l.i++
			return token{kind: tGe, text: ">=", pos: start}, nil
		}
		return token{kind: tGt, text: ">", pos: start}, nil
	case c == '.':
		l.i++
		return token{kind: tDot, text: ".", pos: start}, nil
	case c == '"':
		return l.lexString()
	case c == '-':
		// '-' at a token boundary is the negation operator. '-' inside a
		// text run (e.g. 2026-07-01) is consumed by lexText and never
		// reaches here.
		l.i++
		return token{kind: tMinus, text: "-", pos: start}, nil
	default:
		return l.lexText()
	}
}

func (l *lexer) lexString() (token, error) {
	start := l.i
	l.i++ // opening quote
	var b strings.Builder
	for l.i < len(l.src) {
		c := l.src[l.i]
		switch c {
		case '\\':
			if l.i+1 >= len(l.src) {
				return token{}, &Error{Kind: ErrParse, Pos: l.i, Msg: "unterminated escape in string"}
			}
			n := l.src[l.i+1]
			if n != '"' && n != '\\' {
				return token{}, &Error{Kind: ErrParse, Pos: l.i, Msg: fmt.Sprintf("invalid escape \\%c — only \\\" and \\\\ are supported", n)}
			}
			b.WriteByte(n)
			l.i += 2
		case '"':
			l.i++
			return token{kind: tString, text: b.String(), pos: start, quoted: true}, nil
		default:
			b.WriteByte(c)
			l.i++
		}
	}
	return token{}, &Error{Kind: ErrParse, Pos: start, Msg: "unterminated string"}
}

func (l *lexer) lexText() (token, error) {
	start := l.i
	for l.i < len(l.src) && !isDelim(l.src[l.i]) {
		l.i++
	}
	text := l.src[start:l.i]
	switch text {
	case "AND":
		return token{kind: tAnd, text: text, pos: start}, nil
	case "OR":
		return token{kind: tOr, text: text, pos: start}, nil
	case "NOT":
		return token{kind: tNot, text: text, pos: start}, nil
	}
	return token{kind: tText, text: text, pos: start}, nil
}

func isDelim(c byte) bool {
	return isSpace(c) || strings.IndexByte(`()":=!<>.`, c) >= 0
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/filterquery/ -v`
Expected: PASS (6 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/filterquery/ docs/superpowers/specs/2026-07-25-filter-query-language-design.md
git commit -m "feat(filterquery): lexer and error type for the q filter language

Also corrects the spec's precedence note (NOT > OR > implicit AND >
explicit AND, per the AIP-160 EBNF).

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

(The spec edit — one line: replace `(precedence: implicit AND > OR)` with `(precedence: NOT > OR > implicit AND > explicit AND)` — rides this commit.)

---

### Task 2: AST + parser

**Files:**
- Create: `internal/filterquery/ast.go`
- Create: `internal/filterquery/parser.go`
- Test: `internal/filterquery/parser_test.go`

**Interfaces:**
- Consumes: Task 1's `lex`, `token`, token-kind constants, `Error`/`ErrParse`/`ErrCap`.
- Produces: `Node` interface (`Pos() int`); node types `And{Terms []Node, At int}`, `Or{Terms []Node, At int}`, `Not{X Node, At int}`, `Comparison{Field, Op, Raw string, Quoted bool, Value any, At int}`, `Bare{Text string, At int}`; `parse(src string) (Node, error)`; caps `maxDepth = 64`, `maxNodes = 512`.

- [ ] **Step 1: Write the failing tests**

```go
package filterquery

import (
	"strings"
	"testing"
)

// sexpr renders an AST as an s-expression for compact assertions.
func sexpr(n Node) string {
	switch t := n.(type) {
	case *And:
		parts := make([]string, len(t.Terms))
		for i, x := range t.Terms {
			parts[i] = sexpr(x)
		}
		return "(and " + strings.Join(parts, " ") + ")"
	case *Or:
		parts := make([]string, len(t.Terms))
		for i, x := range t.Terms {
			parts[i] = sexpr(x)
		}
		return "(or " + strings.Join(parts, " ") + ")"
	case *Not:
		return "(not " + sexpr(t.X) + ")"
	case *Comparison:
		return "(" + t.Field + " " + t.Op + " " + t.Raw + ")"
	case *Bare:
		return "(bare " + t.Text + ")"
	default:
		return "<?>"
	}
}

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
		`a:x b:y OR c:z`:             `(and (a : x) (or (b : y) (c : z)))`,
		`a:x OR b:y c:z`:             `(and (or (a : x) (b : y)) (c : z))`,
		`a:x b:y AND c:z`:            `(and (and (a : x) (b : y)) (c : z))`,
		`a:x AND b:y AND c:z`:        `(and (a : x) (b : y) (c : z))`,
		`NOT a:x`:                    `(not (a : x))`,
		`NOT a:x OR b:y`:             `(or (not (a : x)) (b : y))`,
		`-a:x`:                       `(not (a : x))`,
		`a:x AND (b:y OR c:z)`:       `(and (a : x) (or (b : y) (c : z)))`,
		`(a:x OR b:y) AND NOT c:z`:   `(and (or (a : x) (b : y)) (not (c : z)))`,
		`label:urgent OR (from:alerts AND NOT has:attachment) created>=2026-07-01`: `(and (or (label : urgent) (and (from : alerts) (not (has : attachment)))) (created >= 2026-07-01))`,
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
		`from:alice@x.com`:                  `(from : alice@x.com)`,
		`label:e2a:held`:                    `(label : e2a:held)`,
		`created<2026-07-25T10:30:00Z`:      `(created < 2026-07-25T10:30:00Z)`,
		`created<2026-07-25T10:30:00.123Z`:  `(created < 2026-07-25T10:30:00.123Z)`,
	}
	for q, want := range cases {
		if got := parseToString(t, q); got != want {
			t.Errorf("parse(%q) = %s, want %s", q, got, want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, q := range []string{
		`(a:x`,        // unclosed paren
		`a:x)`,        // stray close
		`a:x AND`,     // dangling AND
		`OR a:x`,      // leading OR
		`:x`,          // missing field
		`label:`,      // missing value
		`a.x:y`,       // a.x is fine — but `a.:y` and `a..b:y` are not
		`NOT(a:x)`,    // NOT must be followed by whitespace
	} {
		if q == `a.x:y` {
			continue // valid dotted member, covered elsewhere
		}
		if _, err := parse(q); err == nil {
			t.Errorf("parse(%q) = nil error, want error", q)
		}
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
	if _, err := parse(b.String()); err == nil {
		t.Error("600 nodes: want error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/filterquery/ -run TestParse -v`
Expected: FAIL — undefined `parse`, `Node`, `And`, …

- [ ] **Step 3: Implement ast.go and parser.go**

`internal/filterquery/ast.go`:

```go
package filterquery

// Node is one AST node. Pos is the 0-based byte offset of the node's first
// character in the input, for error messages.
type Node interface {
	Pos() int
}

// And is a conjunction — explicit (expression level) or implicit (sequence
// level). The two levels share a node type; precedence is settled at parse.
type And struct {
	Terms []Node
	At    int
}

func (n *And) Pos() int { return n.At }

type Or struct {
	Terms []Node
	At    int
}

func (n *Or) Pos() int { return n.At }

type Not struct {
	X  Node
	At int
}

func (n *Not) Pos() int { return n.At }

// Comparison is `field op value`. Value holds the coerced value and is set
// by Registry.Validate; before validation only Raw is meaningful.
type Comparison struct {
	Field  string // dotted path, e.g. "label" or "metrics.latency"
	Op     string // one of ":", "=", "!=", "<", "<=", ">", ">="
	Raw    string // value text (unescaped contents for quoted strings)
	Quoted bool   // value was a quoted string
	Value  any    // coerced value, set by Validate
	At     int
}

func (n *Comparison) Pos() int { return n.At }

// Bare is an unqualified term (AIP-160 global restriction). The v1 registry
// rejects these at validation; the parser still produces them so the
// rejection error can name the term precisely.
type Bare struct {
	Text string
	At   int
}

func (n *Bare) Pos() int { return n.At }
```

`internal/filterquery/parser.go`:

```go
package filterquery

import "fmt"

const (
	maxDepth = 64
	maxNodes = 512
)

type parser struct {
	toks  []token
	i     int
	nodes int
}

func parse(src string) (Node, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	n, err := p.parseExpression(0)
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, &Error{Kind: ErrParse, Pos: t.pos, Msg: fmt.Sprintf("unexpected %q after complete expression", t.text)}
	}
	return n, nil
}

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) advance() token { t := p.toks[p.i]; p.i++; return t }

// skipWS collapses whitespace and reports whether any was seen.
func (p *parser) skipWS() bool {
	seen := false
	for p.peek().kind == tWS {
		p.advance()
		seen = true
	}
	return seen
}

func (p *parser) addNode() error {
	p.nodes++
	if p.nodes > maxNodes {
		return &Error{Kind: ErrCap, Pos: -1, Msg: fmt.Sprintf("expression too large (limit %d nodes)", maxNodes)}
	}
	return nil
}

// expression = sequence { WS AND WS sequence } — explicit AND is the LOOSEST
// level.
func (p *parser) parseExpression(depth int) (Node, error) {
	first, err := p.parseSequence(depth)
	if err != nil {
		return nil, err
	}
	terms := []Node{first}
	for {
		p.skipWS()
		if p.peek().kind != tAnd {
			break
		}
		p.advance()
		p.skipWS()
		n, err := p.parseSequence(depth)
		if err != nil {
			return nil, err
		}
		terms = append(terms, n)
	}
	if len(terms) == 1 {
		return first, nil
	}
	if err := p.addNode(); err != nil {
		return nil, err
	}
	return &And{Terms: terms, At: first.Pos()}, nil
}

// sequence = factor { WS factor } — whitespace adjacency is implicit AND and
// binds TIGHTER than explicit AND but LOOSER than OR (AIP-160).
func (p *parser) parseSequence(depth int) (Node, error) {
	first, err := p.parseFactor(depth)
	if err != nil {
		return nil, err
	}
	terms := []Node{first}
	for {
		if !p.skipWS() {
			break
		}
		if !startsFactor(p.peek()) {
			break
		}
		n, err := p.parseFactor(depth)
		if err != nil {
			return nil, err
		}
		terms = append(terms, n)
	}
	if len(terms) == 1 {
		return first, nil
	}
	if err := p.addNode(); err != nil {
		return nil, err
	}
	return &And{Terms: terms, At: first.Pos()}, nil
}

// factor = term { WS OR WS term }
func (p *parser) parseFactor(depth int) (Node, error) {
	first, err := p.parseTerm(depth)
	if err != nil {
		return nil, err
	}
	terms := []Node{first}
	for {
		// Preserve whitespace for parseSequence when the following token is
		// another factor rather than OR. Consuming it here would erase the
		// implicit-AND separator.
		save := p.i
		p.skipWS()
		if p.peek().kind != tOr {
			p.i = save
			break
		}
		p.advance()
		p.skipWS()
		n, err := p.parseTerm(depth)
		if err != nil {
			return nil, err
		}
		terms = append(terms, n)
	}
	if len(terms) == 1 {
		return first, nil
	}
	if err := p.addNode(); err != nil {
		return nil, err
	}
	return &Or{Terms: terms, At: first.Pos()}, nil
}

// term = [NOT WS | MINUS] simple
func (p *parser) parseTerm(depth int) (Node, error) {
	t := p.peek()
	negated := false
	if t.kind == tNot {
		p.advance()
		if p.peek().kind != tWS {
			return nil, &Error{Kind: ErrParse, Pos: t.pos, Msg: "NOT must be followed by whitespace"}
		}
		p.skipWS()
		negated = true
	} else if t.kind == tMinus {
		p.advance()
		negated = true
	}
	x, err := p.parseSimple(depth)
	if err != nil {
		return nil, err
	}
	if !negated {
		return x, nil
	}
	if err := p.addNode(); err != nil {
		return nil, err
	}
	return &Not{X: x, At: t.pos}, nil
}

// simple = restriction | composite
func (p *parser) parseSimple(depth int) (Node, error) {
	if depth > maxDepth {
		return nil, &Error{Kind: ErrCap, Pos: p.peek().pos, Msg: fmt.Sprintf("expression nested too deeply (limit %d)", maxDepth)}
	}
	if p.peek().kind == tLParen {
		p.advance()
		p.skipWS()
		n, err := p.parseExpression(depth + 1)
		if err != nil {
			return nil, err
		}
		p.skipWS()
		if p.peek().kind != tRParen {
			return nil, &Error{Kind: ErrParse, Pos: p.peek().pos, Msg: "expected ')'"}
		}
		p.advance()
		return n, nil
	}
	return p.parseRestriction()
}

// restriction = comparable [comparator arg]. Restrictions are
// whitespace-insensitive: `label : urgent` parses.
func (p *parser) parseRestriction() (Node, error) {
	field, pos, err := p.parseMember()
	if err != nil {
		return nil, err
	}
	// Look past optional whitespace for a comparator; if none follows, this
	// is a bare term and the whitespace must be handed back to the sequence
	// loop (it delimits the NEXT factor).
	save := p.i
	p.skipWS()
	t := p.peek()
	var op string
	switch t.kind {
	case tColon:
		op = ":"
	case tEq:
		op = "="
	case tNeq:
		op = "!="
	case tLt:
		op = "<"
	case tLe:
		op = "<="
	case tGt:
		op = ">"
	case tGe:
		op = ">="
	default:
		p.i = save
		if err := p.addNode(); err != nil {
			return nil, err
		}
		return &Bare{Text: field, At: pos}, nil
	}
	p.advance()
	p.skipWS()
	val, quoted, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	if err := p.addNode(); err != nil {
		return nil, err
	}
	return &Comparison{Field: field, Op: op, Raw: val, Quoted: quoted, At: pos}, nil
}

// comparable = member; member = TEXT {DOT TEXT}
func (p *parser) parseMember() (string, int, error) {
	t := p.peek()
	if t.kind != tText && t.kind != tString {
		return "", 0, &Error{Kind: ErrParse, Pos: t.pos, Msg: fmt.Sprintf("expected a field name or term, got %q", t.text)}
	}
	p.advance()
	name := t.text
	for p.peek().kind == tDot {
		p.advance()
		d := p.peek()
		if d.kind != tText {
			return "", 0, &Error{Kind: ErrParse, Pos: d.pos, Msg: "expected field name after '.'"}
		}
		p.advance()
		name += "." + d.text
	}
	return name, t.pos, nil
}

func (p *parser) parseValue() (string, bool, error) {
	t := p.peek()
	if t.kind != tText && t.kind != tString {
		return "", false, &Error{Kind: ErrParse, Pos: t.pos, Msg: "expected a value (text or quoted string)"}
	}
	p.advance()
	if t.kind == tString {
		return t.text, true, nil
	}
	// Dot and colon are structural while parsing a field/comparator, but are
	// ordinary characters inside an unquoted value. Reassemble contiguous
	// segments so email addresses, e2a: system labels, and RFC3339 timestamps
	// do not require quotes.
	value := t.text
	for p.peek().kind == tDot || p.peek().kind == tColon {
		sep := p.advance()
		next := p.peek()
		if next.kind != tText {
			return "", false, &Error{Kind: ErrParse, Pos: next.pos, Msg: "expected value text after " + sep.text}
		}
		p.advance()
		value += sep.text + next.text
	}
	return value, false, nil
}

func startsFactor(t token) bool {
	switch t.kind {
	case tText, tString, tLParen, tNot, tMinus:
		return true
	}
	return false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/filterquery/ -v`
Expected: PASS (all lexer + parser tests)

- [ ] **Step 5: Commit**

```bash
git add internal/filterquery/
git commit -m "feat(filterquery): AST and recursive-descent parser (AIP-160 precedence)

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 3: Registry + validation

**Files:**
- Create: `internal/filterquery/registry.go`
- Test: `internal/filterquery/registry_test.go` (also defines the toy registry reused by Tasks 4–6)

**Interfaces:**
- Consumes: `Node` tree (Task 2), `Error`/`ErrValidate` (Task 1).
- Produces: `FieldSpec{Name string, Ops []string, Coerce func(raw string, quoted bool) (any, error), Emit func(c *Comparison, e *EmitCtx) (string, error)}`; `Registry` with `NewRegistry(specs ...FieldSpec) (*Registry, error)`, `Names() []string`, `Validate(n Node) error`; the one-method `Dialect` interface (`Placeholder(n int) string`) required by `EmitCtx`; `EmitCtx` with `PH(arg any) string`; toy registry helper `toyRegistry(t *testing.T) *Registry` (fields: `name` text `:`/`=`/`!=`, `price` int all comparators, `active` bool `=`/`!=`, `tags` text[] `:`). Task 4 supplies `PostgresDialect`.

- [ ] **Step 1: Write the failing tests**

```go
package filterquery

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

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
	fe, _ := err.(*Error)
	if fe == nil || !strings.Contains(fe.Msg, "bare term") {
		t.Errorf("err = %v, want bare-term rejection", err)
	}
}

func TestValidateOperatorNotAllowed(t *testing.T) {
	reg := toyRegistry(t)
	err := validateToErr(t, reg, `tags=new`)
	fe, _ := err.(*Error)
	if fe == nil || !strings.Contains(fe.Msg, `operator "=" is not allowed on field "tags"`) {
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
	}
	if err := validateToErr(t, reg, `active=maybe`); err == nil {
		t.Error("want bool coercion error")
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
	}
	if err := validateToErr(t, reg, `name:ok AND (price>1 OR NOT tags:sale)`); err != nil {
		t.Errorf("valid expression rejected: %v", err)
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
		Emit: func(c *Comparison, e *EmitCtx) (string, error) { return "1=1", nil },
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
```

(The first assertion in `TestValidateCoercion` is intentionally a "still an error" case — `:` on `price` fails operator check before coercion. Keep it; it documents the ordering.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/filterquery/ -run 'TestValidate|TestNewRegistry' -v`
Expected: FAIL — undefined `NewRegistry`, `FieldSpec`, `EmitCtx`.

- [ ] **Step 3: Implement registry.go**

```go
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
// raw values coerce, and how a validated comparison emits SQL.
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
	switch t := n.(type) {
	case *And:
		for _, x := range t.Terms {
			if err := r.Validate(x); err != nil {
				return err
			}
		}
	case *Or:
		for _, x := range t.Terms {
			if err := r.Validate(x); err != nil {
				return err
			}
		}
	case *Not:
		return r.Validate(t.X)
	case *Bare:
		return &Error{Kind: ErrValidate, Pos: t.At, Msg: fmt.Sprintf("bare term %q is not supported — qualify it with a field (%s)", t.Text, strings.Join(r.Names(), ", "))}
	case *Comparison:
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
	default:
		return &Error{Kind: ErrValidate, Pos: -1, Msg: fmt.Sprintf("filterquery: unknown node type %T", n)}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/filterquery/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/filterquery/
git commit -m "feat(filterquery): field registry and validation

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 4: Postgres dialect + emitter + public API

**Files:**
- Create: `internal/filterquery/emit.go`
- Test: `internal/filterquery/emit_test.go`

**Interfaces:**
- Consumes: `Registry`, `FieldSpec.Emit`, `EmitCtx` (Task 3); `parse` (Task 2).
- Produces: `PostgresDialect` implementing Task 3's `Dialect`; `Expr` type with `Parse(q string, reg *Registry) (*Expr, error)`, `(e *Expr) Emit(d Dialect, start int) (string, []any, error)`, `(e *Expr) Empty() bool`; `Compile(q string, reg *Registry, d Dialect, start int) (string, []any, error)` convenience. The HTTP handler (Task 12) and store closure (Task 11) consume `Expr.Emit`.

- [ ] **Step 1: Write the failing tests**

```go
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
```

For the last tests to pass, `Registry.Emit` must reject a `Comparison` that
was never coerced or was validated by a different registry. Enforce with an
unexported `validatedBy *Registry` field on `Comparison`, set by `Validate`,
and checked before the field emitter runs. This preserves `Expr`'s
concurrent-Emit claim and prevents a mismatched registry/type assertion from
panicking.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/filterquery/ -run 'TestEmit|TestCompile|TestExpr' -v`
Expected: FAIL — undefined `Compile`, `Parse`, `Expr`, `PostgresDialect`.

- [ ] **Step 3: Implement emit.go (+ ast.go/registry.go touch-ups)**

Add to `Comparison` in `ast.go`:

```go
	Value       any       // coerced value, set by Validate
	validatedBy *Registry // set by Validate; Emit requires the same registry
```

(Adjust the existing `Value any` line and doc comment accordingly.)

In `registry.go` `Validate`, `*Comparison` case, after `t.Value = v` add:

```go
		t.validatedBy = r
```

`internal/filterquery/emit.go`:

```go
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
// offset. Safe for concurrent Emit calls (the AST is immutable after Parse —
// EmitCtx is constructed fresh per call).
type Expr struct {
	root Node
	reg  *Registry
}

// Parse lexes, parses, and validates q. Empty/whitespace-only q yields a
// nil Expr and nil error (no constraint).
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/filterquery/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/filterquery/
git commit -m "feat(filterquery): dialect, emitter, and public Parse/Emit/Compile API

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 5: Golden conformance vectors

**Files:**
- Create: `internal/filterquery/testdata/conformance.json`
- Test: `internal/filterquery/conformance_test.go`

**Interfaces:**
- Consumes: `sexpr` (parser_test.go, Task 2 — move it to a shared `helpers_test.go` in this task), `Compile`, `Parse`, `PostgresDialect` (Task 4), `toyRegistry` (Task 3).
- Produces: the vector format (`name, q, ast, error, sql, args`) — the future extraction seed. PR-2 differential tests (Task 10) reuse the runner shape.

- [ ] **Step 1: Write the conformance vectors**

`internal/filterquery/testdata/conformance.json`:

```json
[
  {"name": "single has", "q": "tags:sale", "ast": "(tags : sale)", "sql": "(p.tags @> $1)", "args": [["sale"]]},
  {"name": "substring", "q": "name:widget", "ast": "(name : widget)", "sql": "(p.name ILIKE $1 ESCAPE '\\')", "args": ["%widget%"]},
  {"name": "exact", "q": "name = \"Widget Pro\"", "ast": "(name = Widget Pro)", "sql": "(LOWER(p.name) = LOWER($1))", "args": ["Widget Pro"]},
  {"name": "int comparator", "q": "price>=42", "ast": "(price >= 42)", "sql": "(p.price >= $1)", "args": [42]},
  {"name": "bool", "q": "active=true", "ast": "(active = true)", "sql": "(p.active = $1)", "args": [true]},
  {"name": "implicit and", "q": "name:a price>1", "ast": "(and (name : a) (price > 1))", "sql": "((p.name ILIKE $1 ESCAPE '\\') AND (p.price > $2))", "args": ["%a%", 1]},
  {"name": "or binds tighter than implicit and", "q": "tags:a name:b OR name:c", "ast": "(and (tags : a) (or (name : b) (name : c)))", "sql": "((p.tags @> $1) AND ((p.name ILIKE $2 ESCAPE '\\') OR (p.name ILIKE $3 ESCAPE '\\')))", "args": [["a"], "%b%", "%c%"]},
  {"name": "explicit and loosest", "q": "name:a name:b AND name:c", "ast": "(and (and (name : a) (name : b)) (name : c))", "sql": "(((p.name ILIKE $1 ESCAPE '\\') AND (p.name ILIKE $2 ESCAPE '\\')) AND (p.name ILIKE $3 ESCAPE '\\'))", "args": ["%a%", "%b%", "%c%"]},
  {"name": "not", "q": "NOT tags:x", "ast": "(not (tags : x))", "sql": "(NOT (p.tags @> $1))", "args": [["x"]]},
  {"name": "minus negation", "q": "-tags:x", "ast": "(not (tags : x))", "sql": "(NOT (p.tags @> $1))", "args": [["x"]]},
  {"name": "not or", "q": "NOT a:x OR b:y", "ast": "(or (not (a : x)) (b : y))", "error": "validate: unknown field"},
  {"name": "parens regroup", "q": "(name:a OR name:b) AND NOT tags:x", "ast": "(and (or (name : a) (name : b)) (not (tags : x)))", "sql": "(((p.name ILIKE $1 ESCAPE '\\') OR (p.name ILIKE $2 ESCAPE '\\')) AND (NOT (p.tags @> $3)))", "args": ["%a%", "%b%", ["x"]]},
  {"name": "ws around comparator", "q": "name : widget", "ast": "(name : widget)", "sql": "(p.name ILIKE $1 ESCAPE '\\')", "args": ["%widget%"]},
  {"name": "wildcard per-field choice", "q": "name:\"a*b\"", "ast": "(name : a*b)", "sql": "(p.name ILIKE $1 ESCAPE '\\')", "args": ["%a*b%"]},
  {"name": "unknown field", "q": "color:red", "error": "validate: unknown field \"color\""},
  {"name": "bare term", "q": "hello", "error": "validate: bare term"},
  {"name": "op not allowed", "q": "tags=new", "error": "validate: operator \"=\" is not allowed"},
  {"name": "bad int", "q": "price=abc", "error": "validate: invalid value"},
  {"name": "bad bool", "q": "active=maybe", "error": "validate: invalid value"},
  {"name": "unclosed paren", "q": "(name:a", "error": "parse: expected ')'"},
  {"name": "stray paren", "q": "name:a)", "error": "parse: unexpected \")\""},
  {"name": "dangling and", "q": "name:a AND", "error": "parse:"},
  {"name": "missing value", "q": "name:", "error": "parse: expected a value"},
  {"name": "unterminated string", "q": "name:\"abc", "error": "parse: unterminated string"},
  {"name": "not without ws", "q": "NOT(name:a)", "error": "parse: NOT must be followed by whitespace"},
  {"name": "keyword value needs quotes", "q": "name:NOT", "error": "parse: expected a value"}
]
```

The wildcard vector deliberately proves wildcard interpretation belongs to
the field adapter, not the parser: quotes do not change the `:` comparator,
and the toy `name` field treats `*` literally inside its ILIKE pattern while
e2a's later `from:`/`subject:` adapters translate it.

- [ ] **Step 2: Write the failing runner**

First move `sexpr` from `parser_test.go` into a new `internal/filterquery/helpers_test.go` (same package; delete it from `parser_test.go`).

`internal/filterquery/conformance_test.go`:

```go
package filterquery

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

type vector struct {
	Name  string `json:"name"`
	Q     string `json:"q"`
	AST   string `json:"ast"`
	Error string `json:"error"`
	SQL   string `json:"sql"`
	Args  []any  `json:"args"`
}

func TestConformance(t *testing.T) {
	data, err := os.ReadFile("testdata/conformance.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vectors []vector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(vectors) < 20 {
		t.Fatalf("only %d vectors — the suite must stay comprehensive", len(vectors))
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			reg := toyRegistry(t)
			n, perr := parse(v.Q)
			kind, _ := splitVectorError(t, v.Error)
			if perr != nil {
				if v.Error == "" {
					t.Fatalf("q=%q: unexpected parse error: %v", v.Q, perr)
				}
				if kind != "parse" && kind != "cap" {
					t.Fatalf("q=%q: failed during parse, want %s-stage error: %v", v.Q, kind, perr)
				}
				requireErrorKind(t, perr, kind, v.Error)
				return
			}
			if v.AST != "" {
				if got := sexpr(n); got != v.AST {
					t.Fatalf("q=%q: AST = %s, want %s", v.Q, got, v.AST)
				}
			}
			verr := reg.Validate(n)
			if v.Error != "" {
				if kind != "validate" {
					t.Fatalf("q=%q: parsed successfully, want %s-stage error", v.Q, kind)
				}
				if verr == nil {
					t.Fatalf("q=%q: want error matching %q, got none", v.Q, v.Error)
				}
				requireErrorKind(t, verr, kind, v.Error)
				return
			}
			if v.AST == "" {
				t.Fatalf("vector %q: successful vectors must assert AST", v.Name)
			}
			if verr != nil {
				t.Fatalf("q=%q: validate: %v", v.Q, verr)
			}
			if v.SQL == "" {
				t.Fatalf("vector %q: successful vectors must assert SQL", v.Name)
			}
			frag, args, err := reg.Emit(n, PostgresDialect{}, 1)
			if err != nil {
				t.Fatalf("q=%q: emit: %v", v.Q, err)
			}
			if frag != v.SQL {
				t.Errorf("q=%q: SQL =\n%s\nwant\n%s", v.Q, frag, v.SQL)
			}
			wantArgs := normalizeJSONArgs(v.Args)
			if !reflect.DeepEqual(args, wantArgs) {
				t.Errorf("q=%q: args = %#v, want %#v", v.Q, args, wantArgs)
			}
		})
	}
}

func splitVectorError(t *testing.T, full string) (kind, msg string) {
	t.Helper()
	if full == "" {
		return "", ""
	}
	kind, msg, _ = strings.Cut(full, ":")
	kind = strings.TrimSpace(kind)
	msg = strings.TrimSpace(msg)
	switch kind {
	case "parse", "validate", "cap":
		return kind, msg
	default:
		t.Fatalf("unknown vector error kind %q in %q", kind, full)
		return "", ""
	}
}

// requireErrorKind asserts err is an *Error whose Kind matches the prefix
// ("parse"/"validate"/"cap") and whose message contains the optional text
// after ": ".
func requireErrorKind(t *testing.T, err error, kind, full string) {
	t.Helper()
	fe, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %v (%T), want *Error", err, err)
	}
	wantKind := vectorErrorKind(t, kind)
	if fe.Kind != wantKind {
		t.Fatalf("err kind = %v, want %v (%q)", fe.Kind, wantKind, full)
	}
	if _, msg := splitVectorError(t, full); msg != "" {
		if !strings.Contains(fe.Msg, msg) {
			t.Fatalf("err msg = %q, want substring %q", fe.Msg, msg)
		}
	}
}

func vectorErrorKind(t *testing.T, kind string) ErrKind {
	t.Helper()
	switch kind {
	case "parse":
		return ErrParse
	case "validate":
		return ErrValidate
	case "cap":
		return ErrCap
	default:
		t.Fatalf("unknown vector error kind %q", kind)
		return ErrParse
	}
}

// normalizeJSONArgs converts JSON-decoded args (float64 numbers, []any
// arrays) into the Go values emission produces (int, []string).
func normalizeJSONArgs(in []any) []any {
	out := make([]any, len(in))
	for i, a := range in {
		switch v := a.(type) {
		case float64:
			out[i] = int(v)
		case []any:
			ss := make([]string, len(v))
			for j, x := range v {
				ss[j] = x.(string)
			}
			out[i] = ss
		default:
			out[i] = a
		}
	}
	return out
}
```

- [ ] **Step 3: Run to verify pass (and fix any vector/AST drift the runner exposes)**

Run: `go test ./internal/filterquery/ -run TestConformance -v`
Expected: PASS. If a precedence vector disagrees, trust the AIP-160 EBNF and fix the vector, not the parser — then re-check against the spec.

- [ ] **Step 4: Commit**

```bash
git add internal/filterquery/
git commit -m "feat(filterquery): golden conformance vectors (extraction seed)

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 6: Precedence enumeration matrix

**Files:**
- Test: `internal/filterquery/enumeration_test.go`

**Interfaces:**
- Consumes: `parse`, `sexpr` (helpers_test.go), toy registry.

**Coverage correction:** The illustrative cases below are not a complete
enumeration by themselves. The implementation must generate the full 3×3
ordered matrix for `{explicit AND, implicit AND, OR}` across the two operator
positions in a three-leaf expression, including equal-operator associativity,
plus generated NOT and parenthesized left/right regrouping variants. Use named
subtests that identify the operator pair and variant.

- [ ] **Step 1: Write the test**

```go
package filterquery

import (
	"fmt"
	"testing"
)

// TestEnumerationPrecedence enumerates every binary composition of three
// comparisons with each operator pair, with and without NOT and parens, and
// pins the resulting AST shape. Precedence regressions cannot slip through
// a hand-picked example set.
func TestEnumerationPrecedence(t *testing.T) {
	leaf := func(f, v string) string { return fmt.Sprintf("%s:%s", f, v) }
	// expectations keyed by generated query; generated programmatically below
	type c struct{ q, ast string }
	var cases []c
	x, y, z := `(tags : a)`, `(name : b)`, `(price > 1)`
	qx, qy, qz := leaf("tags", "a"), leaf("name", "b"), "price>1"

	// explicit AND vs implicit AND
	cases = append(cases,
		c{qx + " " + qy, "(and " + x + " " + y + ")"},
		c{qx + " AND " + qy, "(and " + x + " " + y + ")"},
		c{qx + " " + qy + " AND " + qz, "(and (and " + x + " " + y + ") " + z + ")"},
		c{qx + " AND " + qy + " " + qz, "(and " + x + " (and " + y + " " + z + "))"},
		// OR tighter than implicit AND
		c{qx + " " + qy + " OR " + qz, "(and " + x + " (or " + y + " " + z + "))"},
		c{qx + " OR " + qy + " " + qz, "(and (or " + x + " " + y + ") " + z + ")"},
		c{qx + " OR " + qy + " OR " + qz, "(or " + x + " " + y + " " + z + ")"},
		// NOT tightest
		c{"NOT " + qx + " OR " + qy, "(or (not " + x + ") " + y + ")"},
		c{"NOT " + qx + " " + qy, "(and (not " + x + ") " + y + ")"},
		// parens regroup
		c{"(" + qx + " OR " + qy + ") " + qz, "(and (or " + x + " " + y + ") " + z + ")"},
		c{qx + " OR (" + qy + " " + qz + ")", "(or " + x + " (and " + y + " " + z + "))"},
		c{"NOT (" + qx + " OR " + qy + ")", "(not (or " + x + " " + y + "))"},
	)
	for _, tc := range cases {
		n, err := parse(tc.q)
		if err != nil {
			t.Errorf("parse(%q): %v", tc.q, err)
			continue
		}
		if got := sexpr(n); got != tc.ast {
			t.Errorf("parse(%q) = %s, want %s", tc.q, got, tc.ast)
		}
	}
}
```

- [ ] **Step 2: Run**

Run: `go test ./internal/filterquery/ -run TestEnumeration -v`
Expected: PASS on first run (it pins Task 2's behavior). If any case fails, the parser's precedence is wrong — fix the parser, never the expected tree, unless the EBNF itself was misread (re-derive from `expression/sequence/factor/term`).

- [ ] **Step 3: Commit**

```bash
git add internal/filterquery/
git commit -m "test(filterquery): precedence enumeration matrix

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 7: Fuzzing

**Files:**
- Test: `internal/filterquery/fuzz_test.go`

- [ ] **Step 1: Write the fuzz test**

```go
package filterquery

import "testing"

// FuzzParse must never panic or hang on arbitrary input. Any *Error result
// is fine; a crash, unbounded recursion, or invalid position is not.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"tags:sale", "name:a price>1", "NOT (a:x OR b:y)", `name:"quoted"`,
		"created>=2026-07-01", "label : urgent", "a b c OR d AND e",
		"(", ")", ":", "\"", "\\", "NOT", "-", "a..b:c", "日本語:値",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		if len(q) > 2000 {
			return // the handler caps at 500; fuzz parser robustness somewhat beyond
		}
		n, err := parse(q)
		if err != nil {
			fe, ok := err.(*Error)
			if !ok {
				t.Fatalf("error type %T, want *Error: %v", err, err)
			}
			// Positions are byte offsets and EOF (len(q)) is valid.
			if fe.Pos < 0 || fe.Pos > len(q) {
				t.Fatalf("error position %d out of range for %q", fe.Pos, q)
			}
			return
		}
		// Round-trip: every parsed tree re-validates-or-rejects cleanly; no
		// second panic surface.
		_ = toyRegistry(t).Validate(n)
	})
}
```

- [ ] **Step 2: Run short fuzz**

Run: `go test ./internal/filterquery/ -run FuzzParse -fuzz FuzzParse -fuzztime 30s`
Expected: PASS, no crashes. Any crasher found becomes a seed + fixed before continuing.

- [ ] **Step 3: Commit**

```bash
git add internal/filterquery/
git commit -m "test(filterquery): fuzz the parser

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 8: Injection invariant

**Files:**
- Test: `internal/filterquery/injection_test.go`

**Interfaces:**
- Consumes: `Compile`, toy registry.

- [ ] **Step 1: Write the test**

```go
package filterquery

import (
	"reflect"
	"strings"
	"testing"
)

// TestInjectionInvariant pins the complete SQL fragment independently of the
// attack text and proves the exact original value travels only in a bound
// argument. Substring checks are invalid here because attack strings such as
// "'", "\", and "$1" also occur in the emitter's fixed SQL syntax.
func TestInjectionInvariant(t *testing.T) {
	attacks := []string{
		`'`, `"`, `\`, `$1`, `'; DROP TABLE products; --`, `%`, `_`, `*`,
		"x'; DELETE FROM products WHERE 'a'='a", " OR 1=1 --", "a\x00b",
	}
	for _, a := range attacks {
		// quoted form is the adversarial path: attacker controls the bytes
		// between quotes. Escape per our lexer rules.
		q := `name:"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a) + `"`
		frag, args, err := Compile(q, toyRegistry(t), PostgresDialect{}, 1)
		if err != nil {
			t.Fatalf("Compile(%q): %v", q, err)
		}
		if want := `(p.name ILIKE $1 ESCAPE '\')`; frag != want {
			t.Errorf("attack value %q changed fragment:\ngot  %s\nwant %s", a, frag, want)
		}
		if want := []any{"%" + a + "%"}; !reflect.DeepEqual(args, want) {
			t.Errorf("attack value %q: args=%#v, want %#v", a, args, want)
		}
	}
}

// TestIdentifierSafety: field names can never inject identifiers — unknown
// fields fail validation before emission.
func TestIdentifierSafety(t *testing.T) {
	for _, q := range []string{
		`name;DROP:x`, `name--:x`, `name$x:x`, `name":x`, `1=1--:x`,
	} {
		if _, _, err := Compile(q, toyRegistry(t), PostgresDialect{}, 1); err == nil {
			t.Errorf("q=%q compiled; want rejection", q)
		}
	}
}
```

Note: the toy `name` Emit wraps values in `%…%` (substring), so attack text
appears in *args*, which is correct and asserted. The exact-fragment assertion
is the SQL invariant; a substring assertion would produce false positives for
attack values that happen to match fixed SQL syntax.

- [ ] **Step 2: Run**

Run: `go test ./internal/filterquery/ -run 'TestInjection|TestIdentifier' -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/filterquery/
git commit -m "test(filterquery): injection invariant and identifier safety

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

**PR 1 exit gate:** `go test ./internal/filterquery/... -v` all green, `go vet ./internal/filterquery/...` clean, package has no non-stdlib imports (`go list -deps ./internal/filterquery | grep -v '^internal/'` shows only stdlib — run it and check `e2a/` packages are absent). This gate is the genericity proof.

---

## PR 2 — API surface + clients

### Task 9: e2a field registry (`internal/identity`)

**Files:**
- Create: `internal/identity/filter_registry.go`
- Test: `internal/identity/filter_registry_test.go` (unit tests only; DB tests are Task 10)

**Interfaces:**
- Consumes: `filterquery` public API (Tasks 1–4); existing `escapeLikePattern` (identity/store.go).
- Produces: `MessagesQRegistry() *filterquery.Registry` (lazy singleton); unexported `messagesFieldRegistry()`; value type `createdValue{at time.Time, dayRange bool}`. Task 10's differential tests and Task 11's handler consume these.

- [ ] **Step 1: Write the failing tests**

```go
package identity

import (
	"reflect"
	"testing"
	"time"

	"e2a/internal/filterquery"
)

func compileQ(t *testing.T, q string, start int) (string, []any) {
	t.Helper()
	frag, args, err := filterquery.Compile(q, MessagesQRegistry(), filterquery.PostgresDialect{}, start)
	if err != nil {
		t.Fatalf("Compile(%q): %v", q, err)
	}
	return frag, args
}

func TestLabelField(t *testing.T) {
	frag, args := compileQ(t, `label:urgent`, 1)
	if frag != `(m.labels @> $1)` || !reflect.DeepEqual(args, []any{[]string{"urgent"}}) {
		t.Errorf("frag=%s args=%v", frag, args)
	}
	if _, _, err := filterquery.Compile(`label:UPPER`, MessagesQRegistry(), filterquery.PostgresDialect{}, 1); err == nil {
		t.Error("uppercase label: want rejection (charset)")
	}
	if _, _, err := filterquery.Compile(`label = "urgent"`, MessagesQRegistry(), filterquery.PostgresDialect{}, 1); err == nil {
		t.Error("label=: want operator rejection")
	}
	// system labels are filterable
	if _, _, err := filterquery.Compile(`label:e2a:held`, MessagesQRegistry(), filterquery.PostgresDialect{}, 1); err != nil {
		t.Errorf("system label filter should work: %v", err)
	}
}

func TestFromFieldMatchesFlatParam(t *testing.T) {
	// MUST be identical in shape to the flat `from` filter: m.sender ILIKE … ESCAPE '\'
	frag, args := compileQ(t, `from:alice@x.com`, 1)
	if frag != `(m.sender ILIKE $1 ESCAPE '\')` {
		t.Errorf("frag = %s", frag)
	}
	if !reflect.DeepEqual(args, []any{"%alice@x.com%"}) {
		t.Errorf("args = %v", args)
	}
	// wildcard translation: '*' → '%', literal %/_/\ escaped
	frag, args = compileQ(t, `from:*@x_%.com`, 1)
	if !reflect.DeepEqual(args, []any{`%@x\_\%.com%`}) {
		t.Errorf("wildcard args = %v", args)
	}
	// exact forms
	frag, _ = compileQ(t, `from = "alice@x.com"`, 1)
	if frag != `(LOWER(m.sender) = LOWER($1))` {
		t.Errorf("exact frag = %s", frag)
	}
	frag, _ = compileQ(t, `from != "alice@x.com"`, 1)
	if frag != `(LOWER(m.sender) != LOWER($1))` {
		t.Errorf("neq frag = %s", frag)
	}
}

func TestSubjectField(t *testing.T) {
	frag, args := compileQ(t, `subject:quarterly`, 1)
	if frag != `(m.subject ILIKE $1 ESCAPE '\')` || !reflect.DeepEqual(args, []any{"%quarterly%"}) {
		t.Errorf("frag=%s args=%v", frag, args)
	}
}

func TestHasAttachment(t *testing.T) {
	frag, args := compileQ(t, `has:attachment`, 1)
	if frag != `(COALESCE(jsonb_array_length(m.attachments_json), 0) > 0)` || len(args) != 0 {
		t.Errorf("frag=%s args=%v", frag, args)
	}
	if _, _, err := filterquery.Compile(`has:body`, MessagesQRegistry(), filterquery.PostgresDialect{}, 1); err == nil {
		t.Error("has:body: want rejection")
	}
}

func TestCreatedField(t *testing.T) {
	frag, args := compileQ(t, `created>=2026-07-01`, 1)
	if frag != `(m.created_at >= $1)` || !reflect.DeepEqual(args, []any{time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}) {
		t.Errorf("frag=%s args=%v", frag, args)
	}
	// date-only equality is a day range
	frag, args = compileQ(t, `created = "2026-07-01"`, 1)
	if frag != `((m.created_at >= $1 AND m.created_at < $2))` {
		t.Errorf("day-range frag = %s", frag)
	}
	if !reflect.DeepEqual(args, []any{time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)}) {
		t.Errorf("day-range args = %v", args)
	}
	// date-only <= means "on or before that day"
	frag, _ = compileQ(t, `created<=2026-07-01`, 1)
	if frag != `(m.created_at < $1)` {
		t.Errorf("<= frag = %s", frag)
	}
	// RFC3339 exact
	ts := "2026-07-25T10:30:00Z"
	frag, args = compileQ(t, `created<`+ts, 1)
	want, _ := time.Parse(time.RFC3339, ts)
	if frag != `(m.created_at < $1)` || !reflect.DeepEqual(args, []any{want}) {
		t.Errorf("rfc3339 frag=%s args=%v", frag, args)
	}
	if _, _, err := filterquery.Compile(`created>yesterday`, MessagesQRegistry(), filterquery.PostgresDialect{}, 1); err == nil {
		t.Error("bad date: want rejection")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/identity/ -run 'TestLabelField|TestFromField|TestSubjectField|TestHasAttachment|TestCreatedField' -v`
Expected: FAIL — undefined `MessagesQRegistry`.

- [ ] **Step 3: Implement filter_registry.go**

```go
package identity

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"e2a/internal/filterquery"
)

// q-language field registry for the messages table. Semantics MUST match the
// flat list params (spec D2): from:/subject: are ILIKE substring with the
// same escapeLikePattern handling as the flat params; label: is single-label
// @> containment; the boolean composition lives in the AST.
//
// Everything messages-specific lives in this file — internal/filterquery
// stays schema-agnostic.

var (
	qRegistryOnce sync.Once
	qRegistry     *filterquery.Registry
)

// MessagesQRegistry returns the shared field registry for list_messages `q`.
func MessagesQRegistry() *filterquery.Registry {
	qRegistryOnce.Do(func() {
		reg, err := filterquery.NewRegistry(
			labelQField(),
			fromQField(),
			subjectQField(),
			hasQField(),
			createdQField(),
		)
		if err != nil {
			panic("filterquery: static messages registry is invalid: " + err.Error())
		}
		qRegistry = reg
	})
	return qRegistry
}

var qLabelRe = regexp.MustCompile(`^[a-z0-9:_-]{1,64}$`)

func labelQField() filterquery.FieldSpec {
	return filterquery.FieldSpec{
		Name: "label",
		Ops:  []string{":"},
		Coerce: func(raw string, quoted bool) (any, error) {
			if !qLabelRe.MatchString(raw) {
				return nil, fmt.Errorf("labels must match [a-z0-9:_-]+ (max 64 chars), got %q", raw)
			}
			return raw, nil
		},
		Emit: func(c *filterquery.Comparison, e *filterquery.EmitCtx) (string, error) {
			return "m.labels @> " + e.PH([]string{c.Value.(string)}), nil
		},
	}
}

// likeSubstring builds the ILIKE pattern shared by from:/subject:: '*' maps
// to '%'; literal %, _, \ are escaped (escapeLikePattern, same helper the
// flat params use).
func likeSubstring(v string) string {
	return "%" + strings.ReplaceAll(escapeLikePattern(v), "*", "%") + "%"
}

func textQField(name, column string, maxLen int) filterquery.FieldSpec {
	return filterquery.FieldSpec{
		Name: name,
		Ops:  []string{":", "=", "!="},
		Coerce: func(raw string, quoted bool) (any, error) {
			if raw == "" {
				return nil, fmt.Errorf("empty %s value", name)
			}
			if len(raw) > maxLen {
				return nil, fmt.Errorf("%s filter too long (max %d chars)", name, maxLen)
			}
			return raw, nil
		},
		Emit: func(c *filterquery.Comparison, e *filterquery.EmitCtx) (string, error) {
			v := c.Value.(string)
			switch c.Op {
			case ":":
				return column + ` ILIKE ` + e.PH(likeSubstring(v)) + ` ESCAPE '\'`, nil
			case "=":
				return "LOWER(" + column + ") = LOWER(" + e.PH(v) + ")", nil
			default: // "!="
				return "LOWER(" + column + ") != LOWER(" + e.PH(v) + ")", nil
			}
		},
	}
}

func fromQField() filterquery.FieldSpec    { return textQField("from", "m.sender", 200) }
func subjectQField() filterquery.FieldSpec { return textQField("subject", "m.subject", 200) }

func hasQField() filterquery.FieldSpec {
	return filterquery.FieldSpec{
		Name: "has",
		Ops:  []string{":"},
		Coerce: func(raw string, quoted bool) (any, error) {
			if raw != "attachment" {
				return nil, fmt.Errorf("unsupported has: value %q — v1 supports has:attachment", raw)
			}
			return raw, nil
		},
		Emit: func(c *filterquery.Comparison, e *filterquery.EmitCtx) (string, error) {
			return "COALESCE(jsonb_array_length(m.attachments_json), 0) > 0", nil
		},
	}
}

// createdValue carries date-coercion semantics: a date-only input (dayRange)
// makes "=" mean "that UTC day", not "that exact midnight second".
type createdValue struct {
	at       time.Time
	dayRange bool
}

func createdQField() filterquery.FieldSpec {
	return filterquery.FieldSpec{
		Name: "created",
		Ops:  []string{"=", "!=", "<", "<=", ">", ">="},
		Coerce: func(raw string, quoted bool) (any, error) {
			if ts, err := time.Parse(time.RFC3339, raw); err == nil {
				return createdValue{at: ts}, nil
			}
			if d, err := time.Parse("2006-01-02", raw); err == nil {
				return createdValue{at: d, dayRange: true}, nil
			}
			return nil, fmt.Errorf("expected RFC3339 or YYYY-MM-DD, got %q", raw)
		},
		Emit: func(c *filterquery.Comparison, e *filterquery.EmitCtx) (string, error) {
			v := c.Value.(createdValue)
			if !v.dayRange {
				return "m.created_at " + c.Op + " " + e.PH(v.at), nil
			}
			end := v.at.AddDate(0, 0, 1)
			switch c.Op {
			case "=":
				return "(m.created_at >= " + e.PH(v.at) + " AND m.created_at < " + e.PH(end) + ")", nil
			case "!=":
				return "(m.created_at < " + e.PH(v.at) + " OR m.created_at >= " + e.PH(end) + ")", nil
			case "<":
				return "m.created_at < " + e.PH(v.at), nil
			case "<=":
				return "m.created_at < " + e.PH(end), nil
			case ">":
				return "m.created_at >= " + e.PH(end), nil
			default: // ">="
				return "m.created_at >= " + e.PH(v.at), nil
			}
		},
	}
}
```

Note: `from` emits on `m.sender`, the settled binding because it exactly matches the flat `from` filter (spec D2).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/identity/ -run 'TestLabelField|TestFromField|TestSubjectField|TestHasAttachment|TestCreatedField' -v`
Expected: PASS (unit tests need no DB)

- [ ] **Step 5: Commit**

```bash
git add internal/identity/filter_registry.go internal/identity/filter_registry_test.go
git commit -m "feat(identity): messages field registry for the q filter language

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 10: Store plumbing + differential DB tests

**Files:**
- Modify: `internal/identity/store.go:3640-3665` (`MessageListFilter`) and `internal/identity/store.go:3798-3806` (after the Labels block in `GetMessagesByAgent`)
- Test: `internal/identity/filter_differential_test.go`

**Interfaces:**
- Consumes: `MessagesQRegistry()` (Task 9), `filterquery.Expr`.
- Produces: `MessageListFilter.QEmit func(startIdx int) (fragment string, args []interface{})` — the handler (Task 11) sets it.

- [ ] **Step 1: Add the filter field and the store splice**

In `MessageListFilter` (store.go ~3661, after `Labels []string`):

```go
	// QEmit, when non-nil, emits the validated q-expression predicate with
	// placeholders numbered from the given 1-based start index. The store
	// invokes it after the built-in filters so $n numbering stays correct.
	// Built by the handler from internal/filterquery.Expr.
	QEmit func(startIdx int) (fragment string, args []interface{})
```

In `GetMessagesByAgent`, immediately after the `if len(f.Labels) > 0 { … }` block:

```go
	if f.QEmit != nil {
		frag, qargs := f.QEmit(len(args) + 1)
		if frag != "" {
			query += " AND " + frag
			args = append(args, qargs...)
		}
	}
```

- [ ] **Step 2: Write the differential test**

`internal/identity/filter_differential_test.go`:

```go
package identity

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"e2a/internal/filterquery"
	"e2a/internal/testutil"
)

// Differential testing: for each q expression, the rows returned by the
// emitted SQL in Postgres MUST equal the rows selected by an independent,
// naive in-Go evaluator over the same fixtures. This catches precedence,
// escaping, wildcard, NULL, and date-boundary bugs that unit-level SQL-string
// assertions cannot.

type fixtureMsg struct {
	id          string
	sender      string
	subject     string
	labels      []string // nil = NULL labels column
	created     time.Time
	attachments int
}

func seedQFixtures(t *testing.T, store *Store, agentID string) []fixtureMsg {
	t.Helper()
	ctx := context.Background()
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	fixtures := []fixtureMsg{
		{sender: "alice@corp.com", subject: "Quarterly report", labels: []string{"urgent", "q3"}, created: day.Add(1 * time.Hour)},
		{sender: "bob@alerts.io", subject: "CPU alert", labels: []string{"alerts"}, created: day.Add(2 * time.Hour), attachments: 2},
		{sender: "carol@news.net", subject: "Weekly digest", labels: []string{"newsletter"}, created: day.Add(3 * time.Hour)},
		{sender: "ALICE@corp.com", subject: "Follow-up", labels: []string{"follow-up"}, created: day.Add(4 * time.Hour)},
		{sender: "dave@x.com", subject: "", labels: nil, created: day.Add(5 * time.Hour)},
		{sender: "eve@percent.com", subject: "100% sure _now_", labels: []string{"urgent"}, created: day.Add(6 * time.Hour)},
		{sender: "frank@star.com", subject: "a*b literal", labels: []string{}, created: day.Add(7 * time.Hour)},
		{sender: "日本語@例.jp", subject: "こんにちは 世界", labels: []string{"urgent", "日本"}, created: day.Add(26 * time.Hour)}, // next day
	}
	for i, fx := range fixtures {
		m, err := store.CreateInboundMessage(ctx, "", agentID, fx.sender, "bot@qdiff.example.com",
			fmt.Sprintf("<qd-%d@x>", i), fx.subject, "", "unread", []byte("From: "+fx.sender+"\r\nSubject: "+fx.subject+"\r\n\r\nx"),
			nil, nil, false, "", nil, nil, nil, InboundScreening{})
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		fixtures[i].id = m.ID
		if fx.labels != nil {
			if _, err := store.pool.Exec(ctx, `UPDATE messages SET labels = $1 WHERE id = $2`, fx.labels, m.ID); err != nil {
				t.Fatalf("labels %d: %v", i, err)
			}
		}
		if fx.attachments > 0 {
			att := fmt.Sprintf(`[{"filename":"a.pdf","content_type":"application/pdf","index":0,"size_bytes":10},{"filename":"b.pdf","content_type":"application/pdf","index":1,"size_bytes":10}][:{}]`, fx.attachments)
			if _, err := store.pool.Exec(ctx, `UPDATE messages SET attachments_json = $1::jsonb WHERE id = $2`, att, m.ID); err != nil {
				t.Fatalf("attachments %d: %v", i, err)
			}
		}
		if _, err := store.pool.Exec(ctx, `UPDATE messages SET created_at = $1 WHERE id = $2`, fx.created, m.ID); err != nil {
			t.Fatalf("created %d: %v", i, err)
		}
	}
	return fixtures
}

// naiveEval is the reference implementation: plain Go semantics over
// fixtures. It shares the front-end (parse+validate) — what differs is
// evaluation: no SQL involved.
func naiveEval(t *testing.T, expr *filterquery.Expr, rows []fixtureMsg) []string {
	t.Helper()
	_ = expr // evaluated via evalNode on the parsed tree below
	return nil
}
```

Stop — the naive evaluator needs the AST, and `Expr` doesn't expose it. Add an exported walker to filterquery instead: `func (e *Expr) Root() Node` (one line in emit.go, plus a line in Task 4's emit.go — add it in this task with its own micro-test). Then the evaluator:

```go
func evalIDs(t *testing.T, root filterquery.Node, rows []fixtureMsg) []string {
	t.Helper()
	var ids []string
	for _, r := range rows {
		if evalNode(root, r) {
			ids = append(ids, r.id)
		}
	}
	sort.Strings(ids)
	return ids
}

func evalNode(n filterquery.Node, r fixtureMsg) bool {
	switch t := n.(type) {
	case *filterquery.And:
		for _, x := range t.Terms {
			if !evalNode(x, r) {
				return false
			}
		}
		return true
	case *filterquery.Or:
		for _, x := range t.Terms {
			if evalNode(x, r) {
				return true
			}
		}
		return false
	case *filterquery.Not:
		return !evalNode(t.X, r)
	case *filterquery.Comparison:
		return evalComparison(t, r)
	default:
		return false
	}
}

// likeMatch mirrors "col ILIKE '%pat%' ESCAPE '\'" where '*' in the value
// became '%': case-insensitive, segments in order.
func likeMatch(value, col string) bool {
	col = strings.ToLower(col)
	value = strings.ToLower(value)
	for _, seg := range strings.Split(value, "*") {
		i := strings.Index(col, seg)
		if i < 0 {
			return false
		}
		col = col[i+len(seg):]
	}
	return true
}

func evalComparison(c *filterquery.Comparison, r fixtureMsg) bool {
	switch c.Field {
	case "label":
		for _, l := range r.labels {
			if l == c.Value.(string) {
				return true
			}
		}
		return false
	case "from":
		v := c.Value.(string)
		switch c.Op {
		case ":":
			return likeMatch(v, r.sender)
		case "=":
			return strings.EqualFold(r.sender, v)
		default:
			return !strings.EqualFold(r.sender, v)
		}
	case "subject":
		v := c.Value.(string)
		switch c.Op {
		case ":":
			return likeMatch(v, r.subject)
		case "=":
			return strings.EqualFold(r.subject, v)
		default:
			return !strings.EqualFold(r.subject, v)
		}
	case "has":
		return r.attachments > 0
	case "created":
		cv := c.Value.(createdValue)
		at := cv.at
		end := at.AddDate(0, 0, 1)
		switch c.Op {
		case "=":
			if cv.dayRange {
				return !r.created.Before(at) && r.created.Before(end)
			}
			return r.created.Equal(at)
		case "!=":
			if cv.dayRange {
				return r.created.Before(at) || !r.created.Before(end)
			}
			return !r.created.Equal(at)
		case "<":
			return r.created.Before(at)
		case "<=":
			if cv.dayRange {
				return r.created.Before(end)
			}
			return !r.created.After(at)
		case ">":
			if cv.dayRange {
				return !r.created.Before(end)
			}
			return r.created.After(at)
		default: // ">="
			return !r.created.Before(at)
		}
	}
	return false
}

func TestQDifferential(t *testing.T) {
	pool := testutil.TestDB(t)
	store := NewStore(pool)
	ctx := context.Background()
	user, agent := seedLabelAgent(t, store, ctx, "qdiff.example.com") // existing helper in labels_test.go
	_ = user
	fixtures := seedQFixtures(t, store, agent.ID)

	queries := []string{
		`label:urgent`,
		`label:urgent OR label:alerts`,
		`label:urgent AND NOT label:newsletter`,
		`NOT label:urgent`,
		`(label:urgent OR label:follow-up) AND NOT has:attachment`,
		`from:alice`,
		`from:ALICE`,
		`from:*@corp.com`,
		`from:100%25`,                       // literal % must not act as wildcard — no match
		`from = "alice@corp.com"`,
		`from != "alice@corp.com"`,
		`subject:100%`,                      // literal percent row matches
		`subject:_now_`,                     // literal underscores
		`subject:"a*b literal"`,
		`subject:こんにちは`,
		`has:attachment`,
		`created = "2026-07-01"`,            // day range: all but the next-day row
		`created>=2026-07-02`,
		`created<2026-07-01T05:00:00Z`,
		`label:urgent OR (from:alerts AND NOT has:attachment) created>=2026-07-01`,
	}
	for _, q := range queries {
		expr, err := filterquery.Parse(q, MessagesQRegistry())
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		want := evalIDs(t, expr.Root(), fixtures)
		msgs, err := store.GetMessagesByAgent(ctx, MessageListFilter{
			AgentID:   agent.ID,
			Direction: "all",
			Status:    "all",
			Limit:     100,
			QEmit: func(start int) (string, []interface{}) {
				frag, args, err := expr.Emit(filterquery.PostgresDialect{}, start)
				if err != nil {
					t.Fatalf("Emit: %v", err)
				}
				return frag, args
			},
		})
		if err != nil {
			t.Fatalf("q=%q: %v", q, err)
		}
		var got []string
		for _, m := range msgs {
			got = append(got, m.ID)
		}
		sort.Strings(got)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("q=%q:\n got %v\nwant %v", q, got, want)
		}
	}
}
```

One semantics call the differential test enforces deliberately: `from:`/`subject:` emit ILIKE on possibly-empty columns — the `dave@x.com` empty-subject fixture matches `subject:""` only never (empty values are rejected at coercion), and `likeMatch` treats Go `""` the way Postgres treats `''` (both "no rows" for non-empty patterns). Fixture labels are set via direct SQL because `CreateInboundMessage` doesn't take labels.

Also note: `from:100%25` — the literal `%` is escaped by `escapeLikePattern`, so it matches only a sender containing `100%25`; no fixture has it, and the naive evaluator agrees (no `*` → plain substring).

- [ ] **Step 3: Add `Expr.Root()` to filterquery**

In `internal/filterquery/emit.go`:

```go
// Root exposes the validated AST for reference evaluators and tests.
// Callers must not mutate it.
func (e *Expr) Root() Node { return e.root }
```

Micro-test in `emit_test.go`:

```go
func TestExprRoot(t *testing.T) {
	expr, err := Parse(`name:a`, toyRegistry(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := expr.Root().(*Comparison); !ok {
		t.Errorf("Root() = %T", expr.Root())
	}
}
```

- [ ] **Step 4: Run**

Run: `make docker-up` (if Postgres isn't up), then `go test ./internal/identity/ -run 'TestQDifferential|TestExprRoot' -v` and `go test ./internal/filterquery/ -run TestExprRoot -v`
Expected: PASS. Any mismatch: debug against the fixtures — the naive evaluator is the specification; fix emission, not the fixtures.

- [ ] **Step 5: Commit**

```bash
git add internal/identity/ internal/filterquery/
git commit -m "feat(identity): q predicate store plumbing + differential SQL-vs-reference tests

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 11: HTTP handler — `q` param + cursor pinning

**Files:**
- Modify: `internal/httpapi/messages.go:357-374` (`ListMessagesInput`), `:383-397` (`messagesCursor`), `:636-790` (`handleListMessages`)
- Test: `internal/httpapi/messages_q_test.go`

**Interfaces:**
- Consumes: `identity.MessagesQRegistry()` (Task 9), `filterquery.Parse/Expr`, `MessageListFilter.QEmit` (Task 10).
- Produces: public `q` query param on `GET /v1/agents/{email}/messages`; 400 `invalid_filter` contract.

- [ ] **Step 1: Write the failing handler tests**

```go
package httpapi

// Follow the patterns in messages_status_consistency_test.go: build the
// test server with a stubbed deps.ListMessages capturing the filter.

func TestQParamInvalid(t *testing.T) {
	// q with an unknown field → 400 invalid_filter naming the field
	// q longer than 500 chars → 400 invalid_filter
	// q with a parse error → 400 with "(at column N)"
}

func TestQParamCursorPinning(t *testing.T) {
	// page 1 with q=label:urgent → cursor; page 2 with q=label:other → 400 cursor mismatch
	// page 2 with identical q → 200
}

func TestQParamReachesStore(t *testing.T) {
	// q=label:urgent → stubbed ListMessages receives MessageListFilter with
	// non-nil QEmit; invoking QEmit(1) yields "(m.labels @> $1)" and
	// []interface{}{[]string{"urgent"}}
}

func TestQComposesWithFlatParams(t *testing.T) {
	// q=label:urgent&from=alice → both From="alice" and QEmit set (AND
	// composition happens in the store, Task 10 covered the SQL)
}
```

Implement the tests following the file's existing stub-server helpers (`newTestServer`-style helpers in the package — reuse them; do not invent new scaffolding).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/httpapi/ -run TestQParam -v`
Expected: FAIL — `q` not a known query param / QEmit never set.

- [ ] **Step 3: Implement the handler wiring**

In `ListMessagesInput` (messages.go ~368, after `Labels`):

```go
	Q               string   `query:"q" doc:"Boolean filter expression (AIP-160-derived). v1 fields: label, from, subject, has, created. Operators: : = != < <= > >= with AND / OR / NOT and parentheses; whitespace is implicit AND and binds looser than OR (e.g. 'label:urgent OR (from:alerts AND NOT has:attachment) created>=2026-07-01'). Composes with (ANDs) the flat filters. Unknown fields/operators are rejected with a positioned invalid_filter error. Max 500 chars."`
```

In `messagesCursor` (~395):

```go
	Q               string    `json:"q,omitempty"`
```

In `handleListMessages`, after the existing filter validation (~line 686, near `normalizeLabelFilter`):

```go
	var qEmit func(int) (string, []interface{})
	if in.Q != "" {
		if len(in.Q) > 500 {
			return nil, NewError(http.StatusBadRequest, "invalid_filter", "q filter too long (max 500 chars)")
		}
		expr, err := filterquery.Parse(in.Q, identity.MessagesQRegistry())
		if err != nil {
			return nil, NewError(http.StatusBadRequest, "invalid_filter", err.Error())
		}
		// Preflight: prove the expression emits (defensive — Validate already
		// guarantees it) so the store closure can ignore the error.
		if _, _, err := expr.Emit(filterquery.PostgresDialect{}, 1); err != nil {
			return nil, NewError(http.StatusBadRequest, "invalid_filter", err.Error())
		}
		qEmit = func(start int) (string, []interface{}) {
			frag, args, _ := expr.Emit(filterquery.PostgresDialect{}, start)
			return frag, args
		}
	}
```

Pass `QEmit: qEmit` in the `identity.MessageListFilter{…}` literal (~737), include `Q: in.Q` when encoding the cursor, and add to the cursor-mismatch check (~723) alongside the existing comparisons:

```go
			cur.Q != in.Q ||
```

(find the existing `if cur.AgentID != … || … !stringSlicesEqual(cur.Labels, labelsFilter)` block and add the one line; also set `Q: in.Q` on the cursor struct literal that encodes the next cursor.)

Import `"e2a/internal/filterquery"` in messages.go.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/httpapi/ -run TestQParam -v` then `go test ./internal/httpapi/ -v` (no regressions)
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi/
git commit -m "feat(api): q filter param on list_messages with cursor pinning

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 12: OpenAPI regen + contract gates

**Files:**
- Modify (generated): `api/openapi.yaml`

- [ ] **Step 1: Regenerate and gate**

```bash
make spec          # regenerates api/openapi.yaml from the /v1 handlers
make spec-check    # contract-drift gate
make openapi-compat-check   # vs origin/main — new optional param is backward compatible
```

Expected: all pass; `api/openapi.yaml` shows `q` on the listMessages operation.

- [ ] **Step 2: Commit**

```bash
git add api/openapi.yaml
git commit -m "feat(api): regenerate OpenAPI with q param

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 13: MCP `list_messages` gains `q`

**Files:**
- Modify: `mcp/src/tools/messages.ts` (the `list_messages` registration, ~line 388-440)
- Test: `mcp/tests/` — add `list-messages-q.test.ts` next to the existing tool tests

**Interfaces:**
- Consumes: regenerated TS SDK `ListMessagesParams.q` (Task 14 runs `make generate-sdk`; if the MCP typecheck needs it first, run `make generate-sdk-ts` before this task's implementation step and commit it in Task 14).

- [ ] **Step 1: Add the schema field + passthrough + test**

In the `list_messages` `inputSchema` (after `labels`, ~line 435):

```ts
        q: z
          .string()
          .max(500)
          .optional()
          .describe(
            "Boolean filter expression (AIP-160-derived). v1 fields: label, from, subject, has, created. Operators : = != < <= > >= with AND/OR/NOT and parentheses; whitespace is implicit AND (binds looser than OR). Example: 'label:urgent OR (from:alerts AND NOT has:attachment) created>=2026-07-01'. Composes (AND) with the flat filters (labels, from_, subject_contains, since, until). Invalid expressions are rejected with a positioned invalid_filter error.",
          ),
```

In the same tool's handler, add `q` to the params object passed to the SDK (follow the existing conditional-spread pattern, e.g. how `subject_contains` maps to `subjectContains`):

```ts
            ...(args.q !== undefined ? { q: args.q } : {}),
```

Update the tool's `description` (~line 388): after "**Search filters** (`from_`, `subject_contains`, `conversation_id`, `since`, `until`)" add "`q` (boolean expression)" to the list.

Test (`mcp/tests/list-messages-q.test.ts`, following the existing tool-test harness):

```ts
// - schema accepts q ≤ 500 chars, rejects > 500
// - handler passes q through to the SDK params verbatim
// - q omitted → params contain no q key
```

- [ ] **Step 2: Run**

Run: `cd mcp && npm test -- list-messages-q` (or the repo's MCP test runner — see mcp/package.json scripts)
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add mcp/
git commit -m "feat(mcp): q filter on list_messages

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 14: SDK regen + CLI `--q`

**Files:**
- Modify (generated): `sdks/python/src/e2a/v1/generated/**`, `sdks/typescript/src/v1/generated/**`
- Modify: `cli/src/commands/messages.ts:29` (usage line) and its params building (~line 44-77)
- Test: `cli/src/__tests__/args.test.ts`

- [ ] **Step 1: Regenerate both SDKs**

```bash
make generate-sdk            # both bases via openapi-generator v7.16.0
make generate-sdk-check      # drift gate
```

Expected: `ListMessagesParams` (TS) and the messages API (Python) expose `q`; checks pass.

- [ ] **Step 2: CLI flag**

In `cli/src/commands/messages.ts` usage line, append ` [--q <expr>]`; in the params building for `messages list`, add `q` when the flag is present (follow how `--since` maps into `params`). In `cli/src/__tests__/args.test.ts` add a case asserting `messages list --q "label:urgent"` puts `q: "label:urgent"` into the SDK params.

- [ ] **Step 3: Run**

```bash
cd cli && npm test
cd ../sdks/python && python -m pytest tests/ -q -k "messages"  # if the SDK has tests touching list params
```

Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add sdks/ cli/
git commit -m "feat(sdks,cli): q filter param across python/ts SDKs and CLI

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

---

### Task 15: Docs + e2e + final gate

**Files:**
- Create: `docs/filtering.md` (grammar reference: EBNF summary, precedence, v1 field table, examples, error contract, cap)
- Modify: `docs/api.md` (link the new page from the list_messages section)
- Modify: `tests/e2e-prod/` — add one `q` round-trip case following the suite's existing throwaway-agent pattern

- [ ] **Step 1: Write docs/filtering.md**

Sections: syntax (fields, operators, precedence `NOT > OR > implicit AND > explicit AND`, quoting/escaping, wildcards), v1 field table (same content as the spec's Section 2), composition with flat params, errors (positioned `invalid_filter`, 400), caps (500 chars / depth 64 / 512 nodes), examples. Keep it under 150 lines; the spec link carries the design rationale.

- [ ] **Step 2: e2e case**

In the e2e-prod suite, extend the existing message-filter test (find the labels-filter case and add beside it): create throwaway agents, seed two messages with distinct labels, assert `q=label:a OR label:b` returns the union and `q=label:a AND NOT label:b` returns the difference, clean up agents.

- [ ] **Step 3: Full verification**

```bash
go build ./...
go test ./internal/filterquery/... ./internal/identity/... ./internal/httpapi/... -count=1
make spec-check generate-sdk-check openapi-compat-check
cd mcp && npm test && cd ..
cd cli && npm test && cd ..
```

Expected: all green.

- [ ] **Step 4: Commit + push instruction for the human**

```bash
git add docs/ tests/
git commit -m "docs,e2e: filtering reference and q e2e case

Co-Authored-By: Kimi <noreply@moonshot.ai>"
```

Then report: branch `feat/filter-query-language` is ready for two PRs (PR 1 = tasks 1–8 commits, PR 2 = tasks 9–15 commits) — per house convention, PRs are opened on your say-so and merge is user-gated.

---
