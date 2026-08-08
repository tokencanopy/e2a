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
