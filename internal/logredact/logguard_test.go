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
// CONTENT: the `subject<sep><verb>` family in the shapes that actually occur —
// either separator (`subject=%q`, `subject: %q`), any case (`Subject=%v`), and
// the quoted / escaped-quoted forms (`"subject": %s`, `subject=\"%s\"`).
// subject_len=%d and other derived, content-free fields do not match.
var subjectContentPattern = regexp.MustCompile(`(?i)subject(\\?")?\s*[:=]\s*(\\?")?%[qsv]`)

// logSitePattern narrows subjectContentPattern to lines that actually emit a
// log or error string. Without it the case-insensitive `[:=]` form above also
// flags legitimate `"Subject: %s\n"` MIME-header composition (the HITL
// notification email body) and prompt building (the piguard LLM prompt) —
// sites that handle the subject on purpose and never reach stdout. Keeping the
// two checks separate is what lets the content pattern stay broad without
// generating false positives that would get the whole guard allowlisted away.
var logSitePattern = regexp.MustCompile(`\b(log|logger|slog)\.|fmt\.Errorf|errors\.Errorf`)

// TestNoSubjectContentInLogFormats is a TRIPWIRE for one rule only: email
// subject CONTENT must not appear in a log or error format string. It scans
// every non-test Go source file in the module and fails on the `subject=%q`
// family — the exact pattern this package's introduction removed from the
// [mail:...] lines.
//
// What it does NOT do, so nobody mistakes a green run for compliance:
//   - It enforces the SUBJECT rule only. The rest of the logredact policy
//     (external addresses, IPs, free text, third-party error bodies) is not
//     machine-checked — that stays a code-review responsibility.
//   - It is a textual, line-at-a-time scan, not analysis. A subject reached
//     through a variable (`log.Printf("%s", subj)`), a differently named
//     field, a positional form like `log.Println("subject", s)`, a format
//     string built on a different line from its logging call, or a logger
//     reached through some other name all pass right through it.
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
			if subjectLogViolation(line) {
				t.Errorf("%s:%d logs subject CONTENT (%s).\n"+
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
		if readErr != nil || !anyLineViolates(string(raw)) {
			t.Errorf("subjectLogPatternAllowlist entry %q matched no subject-content pattern — remove the stale entry", rel)
		}
	}
}

// subjectLogViolation reports whether one source line both formats subject
// content AND is a log/error emission site.
func subjectLogViolation(line string) bool {
	return subjectContentPattern.MatchString(line) && logSitePattern.MatchString(line)
}

func anyLineViolates(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		if subjectLogViolation(line) {
			return true
		}
	}
	return false
}

// TestSubjectLogViolationDiscrimination pins the guard's own behaviour: the
// shapes it must catch (the reason it exists) and the shapes it must not flag
// (the reason it stays useful rather than being allowlisted into silence).
func TestSubjectLogViolationDiscrimination(t *testing.T) {
	violations := []string{
		`log.Printf("[mail:%s] subject=%q", id, subject)`,
		`log.Printf("subject: %q", subject)`,
		`log.Printf("Subject=%v", subject)`,
		`log.Printf("SUBJECT = %s", subject)`,
		`log.Printf("{\"subject\": %q}", subject)`,
		`log.Printf("subject=\"%s\"", subject)`,
		`return fmt.Errorf("bad subject: %q", subject)`,
		`slog.Info(fmt.Sprintf("subject=%s", subject))`,
	}
	for _, line := range violations {
		if !subjectLogViolation(line) {
			t.Errorf("guard missed a subject-content log line: %s", line)
		}
	}

	clean := []string{
		`log.Printf("[mail:%s] subject_len=%d", id, utf8.RuneCountInString(subject))`,
		`log.Printf("subject_len=%d", n)`,
		// MIME header composition into an email body, not a log.
		`fmt.Fprintf(&b, "Subject: %s\n", msg.Subject)`,
		// LLM prompt construction, not a log.
		`return fmt.Sprintf("Subject: %s\nFrom: %s\n\n%s", subject, req.Sender, body)`,
		`h.Set("Subject", subject)`,
		`var subject = "%s"`,
	}
	for _, line := range clean {
		if subjectLogViolation(line) {
			t.Errorf("guard false-positived on a non-log line: %s", line)
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
