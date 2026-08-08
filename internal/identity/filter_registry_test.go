package identity

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/filterquery"
)

func compileFilter(t *testing.T, filter string, start int) (string, []any) {
	t.Helper()
	frag, args, err := filterquery.Compile(filter, MessagesFilterRegistry(), filterquery.PostgresDialect{}, start)
	if err != nil {
		t.Fatalf("Compile(%q): %v", filter, err)
	}
	return frag, args
}

func TestMessagesFilterRegistrySingleton(t *testing.T) {
	t.Parallel()

	const callers = 32
	registries := make(chan *filterquery.Registry, callers)
	for range callers {
		go func() { registries <- MessagesFilterRegistry() }()
	}

	first := <-registries
	for range callers - 1 {
		if got := <-registries; got != first {
			t.Fatal("MessagesFilterRegistry returned distinct registries")
		}
	}
}

func TestLabelField(t *testing.T) {
	t.Parallel()

	frag, args := compileFilter(t, `label:urgent`, 1)
	if frag != `(m.labels @> $1)` || !reflect.DeepEqual(args, []any{[]string{"urgent"}}) {
		t.Errorf("frag=%s args=%v", frag, args)
	}
	if _, _, err := filterquery.Compile(`label:UPPER`, MessagesFilterRegistry(), filterquery.PostgresDialect{}, 1); err == nil {
		t.Error("uppercase label: want rejection (charset)")
	}
	if _, _, err := filterquery.Compile(`label = "urgent"`, MessagesFilterRegistry(), filterquery.PostgresDialect{}, 1); err == nil {
		t.Error("label=: want operator rejection")
	}
	if _, _, err := filterquery.Compile(`label:e2a:held`, MessagesFilterRegistry(), filterquery.PostgresDialect{}, 1); err != nil {
		t.Errorf("system label filter should work: %v", err)
	}
}

func TestFromFieldMatchesFlatParam(t *testing.T) {
	t.Parallel()

	frag, args := compileFilter(t, `from:alice@x.com`, 1)
	if frag != `(m.sender ILIKE $1 ESCAPE '\')` {
		t.Errorf("frag = %s", frag)
	}
	if !reflect.DeepEqual(args, []any{"%alice@x.com%"}) {
		t.Errorf("args = %v", args)
	}

	frag, args = compileFilter(t, `from:"*@x_%.com\\tail"`, 1)
	if !reflect.DeepEqual(args, []any{`%%@x\_\%.com\\tail%`}) {
		t.Errorf("wildcard args = %v", args)
	}

	for _, op := range []string{"=", "!="} {
		frag, args = compileFilter(t, `from `+op+` "a*b%_"`, 1)
		wantFrag := `(LOWER(m.sender) ` + op + ` LOWER($1))`
		if frag != wantFrag || !reflect.DeepEqual(args, []any{"a*b%_"}) {
			t.Errorf("exact %s: frag=%s args=%v", op, frag, args)
		}
	}
}

func TestTextFieldLengthBounds(t *testing.T) {
	t.Parallel()

	for _, field := range []string{"from", "subject"} {
		field := field
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name  string
				value string
				valid bool
			}{
				{"200 ASCII bytes", strings.Repeat("a", 200), true},
				{"201 ASCII bytes", strings.Repeat("a", 201), false},
				{"200 UTF-8 bytes", strings.Repeat("é", 100), true},
				{"202 UTF-8 bytes", strings.Repeat("é", 101), false},
			} {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					_, _, err := filterquery.Compile(field+`:"`+tc.value+`"`, MessagesFilterRegistry(), filterquery.PostgresDialect{}, 1)
					if tc.valid && err != nil {
						t.Fatalf("%s %s value: %v", tc.name, field, err)
					}
					if !tc.valid && err == nil {
						t.Errorf("%s %s value: want rejection", tc.name, field)
					}
				})
			}
		})
	}
}

func TestSubjectField(t *testing.T) {
	t.Parallel()

	frag, args := compileFilter(t, `subject:quarterly`, 1)
	if frag != `(m.subject ILIKE $1 ESCAPE '\')` || !reflect.DeepEqual(args, []any{"%quarterly%"}) {
		t.Errorf("frag=%s args=%v", frag, args)
	}

	frag, args = compileFilter(t, `subject:"*@x_%.com\\tail"`, 1)
	if !reflect.DeepEqual(args, []any{`%%@x\_\%.com\\tail%`}) {
		t.Errorf("wildcard args = %v", args)
	}

	for _, op := range []string{"=", "!="} {
		frag, args = compileFilter(t, `subject `+op+` "a*b%_"`, 1)
		wantFrag := `(LOWER(m.subject) ` + op + ` LOWER($1))`
		if frag != wantFrag || !reflect.DeepEqual(args, []any{"a*b%_"}) {
			t.Errorf("exact %s: frag=%s args=%v", op, frag, args)
		}
	}
}

func TestHasAttachmentIsDeferred(t *testing.T) {
	t.Parallel()

	_, _, err := filterquery.Compile(`has:attachment`, MessagesFilterRegistry(), filterquery.PostgresDialect{}, 1)
	if err == nil {
		t.Fatal("has:attachment: want unknown-field rejection")
	}
	if got, want := err.Error(), `unknown field "has" — supported fields: created, from, label, subject`; !strings.Contains(got, want) {
		t.Errorf("error = %q, want substring %q", got, want)
	}
}

func TestCreatedField(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	for _, tc := range []struct {
		name string
		q    string
		frag string
		args []any
	}{
		{"equal", `created = "2026-07-01"`, `((m.created_at >= $1 AND m.created_at < $2))`, []any{start, end}},
		{"not equal", `created != "2026-07-01"`, `((m.created_at < $1 OR m.created_at >= $2))`, []any{start, end}},
		{"before", `created<2026-07-01`, `(m.created_at < $1)`, []any{start}},
		{"through day", `created<=2026-07-01`, `(m.created_at < $1)`, []any{end}},
		{"after day", `created>2026-07-01`, `(m.created_at >= $1)`, []any{end}},
		{"on or after", `created>=2026-07-01`, `(m.created_at >= $1)`, []any{start}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			frag, args := compileFilter(t, tc.q, 1)
			if frag != tc.frag || !reflect.DeepEqual(args, tc.args) {
				t.Errorf("frag=%s args=%v, want frag=%s args=%v", frag, args, tc.frag, tc.args)
			}
		})
	}

	ts := "2026-07-25T10:30:00Z"
	frag, args := compileFilter(t, `created<`+ts, 1)
	want, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatal(err)
	}
	if frag != `(m.created_at < $1)` || !reflect.DeepEqual(args, []any{want}) {
		t.Errorf("rfc3339 frag=%s args=%v", frag, args)
	}
	frag, args = compileFilter(t, `created=`+ts, 1)
	if frag != `(m.created_at = $1)` || !reflect.DeepEqual(args, []any{want}) {
		t.Errorf("rfc3339 equality frag=%s args=%v", frag, args)
	}
	if _, _, err := filterquery.Compile(`created>yesterday`, MessagesFilterRegistry(), filterquery.PostgresDialect{}, 1); err == nil {
		t.Error("bad date: want rejection")
	}
}
