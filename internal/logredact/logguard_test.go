package logredact

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// subjectLogPatternAllowlist is the complete set of non-test source files
// that are DELIBERATELY allowed to carry a subject-content log verb,
// keyed by module-relative path, with the reason.
//
// The governing rule (see the package comment on logredact): container
// stdout ships to centralized log storage with long retention and broad
// queryability, so message subject lines — customer content — must never
// be logged. Log `subject_len=%d` instead; the full subject stays on the
// message row in Postgres.
//
// Adding an entry here is a privacy decision, not a formality: be able to
// state why the value formatted at that site can never contain a real
// message subject.
var subjectLogPatternAllowlist = map[string]string{
	// (empty — no exceptions today)
}

// subjectContentPattern matches format verbs that would render subject
// CONTENT into a log or error string: subject=%q / subject=%s / subject=%v
// and the quoted form subject="%s". subject_len=%d and other derived,
// content-free fields do not match.
var subjectContentPattern = regexp.MustCompile(`subject=(%[qsv]|\\?"%[qsv])`)

// TestNoSubjectContentInLogFormats is the stance gate for the log-redaction
// rule on email subject lines. It scans every non-test Go source file in the
// module and fails when a format string renders subject content — the exact
// pattern this package's introduction removed (e.g. `subject=%q` in the
// [mail:...] lines).
//
// If this test failed because you added a subject to a log line: log
// `subject_len=%d` (utf8.RuneCountInString) instead — operators get a
// non-empty/anomaly signal, and the full subject is durably stored on the
// message row. If the site genuinely cannot log subject content (e.g. the
// value is a system constant), allowlist the file above WITH the reason.
func TestNoSubjectContentInLogFormats(t *testing.T) {
	root := moduleRoot(t)

	checked := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Only Go source can call log; skip trees that never hold it.
			switch d.Name() {
			case ".git", "node_modules", "vendor", "web", "sdks", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		checked++
		if !subjectContentPattern.Match(raw) {
			return nil
		}
		if _, ok := subjectLogPatternAllowlist[filepath.ToSlash(rel)]; ok {
			return nil
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if subjectContentPattern.MatchString(line) {
				t.Errorf("%s:%d formats subject CONTENT (%s).\n"+
					"Email subjects are customer content and logs are shipped to long-retention,\n"+
					"widely queryable storage: log subject_len=%%d instead (the full subject lives\n"+
					"on the message row in Postgres), or allowlist the file in\n"+
					"subjectLogPatternAllowlist with the reason it can never carry a real subject.",
					rel, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module source: %v", err)
	}
	if checked == 0 {
		t.Fatal("scanned no Go source files — the module-root discovery or the walk is wrong")
	}

	// Prune the allowlist when a file stops carrying the pattern, so it stays
	// the exact inventory of deliberate exceptions.
	for rel := range subjectLogPatternAllowlist {
		raw, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if readErr != nil || !subjectContentPattern.Match(raw) {
			t.Errorf("subjectLogPatternAllowlist entry %q matched no subject-content pattern — remove the stale entry", rel)
		}
	}
}

// moduleRoot walks up from the test's working directory to the directory
// holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
