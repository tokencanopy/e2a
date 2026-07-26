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

func (p *parser) advance() token {
	t := p.toks[p.i]
	p.i++
	return t
}

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
