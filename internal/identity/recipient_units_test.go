package identity

import "testing"

// TestUniqueRecipientCount pins the canonical recipient-unit primitive:
// deduplication across lists, NormalizeEmail folding (case + whitespace),
// and empty-entry skipping. Expected counts are hand-derived from the
// distinct normalized addresses in each case.
func TestUniqueRecipientCount(t *testing.T) {
	cases := []struct {
		name string
		to   []string
		cc   []string
		bcc  []string
		want int
	}{
		{"single", []string{"a@x.test"}, nil, nil, 1},
		{"three_distinct", []string{"a@x.test"}, []string{"b@x.test"}, []string{"c@x.test"}, 3},
		{"case_dupe_across_lists", []string{"Alice@x.test"}, []string{"alice@x.test"}, nil, 1},
		{"whitespace_dupe", []string{" a@x.test "}, nil, []string{"a@x.test"}, 1},
		{"empty_entries_skipped", []string{"a@x.test", ""}, []string{""}, nil, 1},
		{"all_empty", nil, nil, nil, 0},
		{"dupe_within_list", []string{"a@x.test", "a@x.test", "b@x.test"}, nil, nil, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UniqueRecipientCount(tc.to, tc.cc, tc.bcc); got != tc.want {
				t.Errorf("UniqueRecipientCount = %d, want %d", got, tc.want)
			}
		})
	}
}
