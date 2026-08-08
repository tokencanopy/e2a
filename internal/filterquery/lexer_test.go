package filterquery

import "testing"

func TestLexBasics(t *testing.T) {
	toks, err := lex(`label:urgent OR (from:"alice@x.com" AND NOT subject:newsletter)`)
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
	for _, tc := range []struct {
		name  string
		query string
		pos   int
	}{
		{name: "unterminated quote", query: `subject:"unterminated`, pos: 8},
		{name: "invalid escape", query: `subject:"bad \q escape"`, pos: 13},
		{name: "trailing backslash", query: `subject:"trailing\`, pos: 17},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := lex(tc.query)
			fe, ok := err.(*Error)
			if !ok {
				t.Fatalf("lex(%q) error type %T, want *Error", tc.query, err)
			}
			if fe.Kind != ErrParse || fe.Pos != tc.pos {
				t.Errorf("lex(%q) error = %+v, want {Kind: ErrParse, Pos: %d}", tc.query, fe, tc.pos)
			}
		})
	}

	if _, err := lex(`label!urgent`); err == nil {
		t.Error("lex(label!urgent) = nil error, want error")
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
