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
