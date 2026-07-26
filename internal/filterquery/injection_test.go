package filterquery

import (
	"reflect"
	"strings"
	"testing"
)

// TestInjectionInvariant pins the complete SQL fragment independently of the
// attack text and proves the exact original value travels only in a bound
// argument. Substring checks are invalid here because attack strings such as
// "'", "\\", and "$1" also occur in the emitter's fixed SQL syntax.
func TestInjectionInvariant(t *testing.T) {
	attacks := []struct {
		name  string
		value string
	}{
		{"single quote", `'`},
		{"double quote", `"`},
		{"backslash", `\`},
		{"placeholder", `$1`},
		{"drop statement", `'; DROP TABLE products; --`},
		{"percent wildcard", `%`},
		{"underscore wildcard", `_`},
		{"asterisk wildcard", `*`},
		{"delete statement", "x'; DELETE FROM products WHERE 'a'='a"},
		{"boolean predicate", " OR 1=1 --"},
		{"nul bytes", "a\x00b"},
	}
	for _, attack := range attacks {
		t.Run(attack.name, func(t *testing.T) {
			// Quoted form is the adversarial path: attacker controls the bytes
			// between quotes. Escape per our lexer rules.
			q := `name:"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(attack.value) + `"`
			frag, args, err := Compile(q, toyRegistry(t), PostgresDialect{}, 1)
			if err != nil {
				t.Fatalf("Compile(%q): %v", q, err)
			}
			if want := `(p.name ILIKE $1 ESCAPE '\')`; frag != want {
				t.Errorf("attack value %q changed fragment:\ngot  %s\nwant %s", attack.value, frag, want)
			}
			if want := []any{"%" + attack.value + "%"}; !reflect.DeepEqual(args, want) {
				t.Errorf("attack value %q: args=%#v, want %#v", attack.value, args, want)
			}
		})
	}
}

// TestIdentifierSafety verifies field names can never inject identifiers:
// unregistered or malformed field names fail before emission.
func TestIdentifierSafety(t *testing.T) {
	for _, q := range []string{
		`name;DROP:x`, `name--:x`, `name$x:x`, `name":x`, `1=1--:x`,
	} {
		t.Run(q, func(t *testing.T) {
			if _, _, err := Compile(q, toyRegistry(t), PostgresDialect{}, 1); err == nil {
				t.Errorf("q=%q compiled; want rejection", q)
			}
		})
	}
}
