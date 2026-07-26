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
				return token{}, &Error{Kind: ErrParse, Pos: l.i, Msg: fmt.Sprintf(`invalid escape \%c — only \" and \\ are supported`, n)}
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
