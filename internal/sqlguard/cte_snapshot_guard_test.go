// Package sqlguard holds repo-wide static guards over the SQL embedded in Go
// source. Test-only: there is no runtime code here, only tripwires in the
// spirit of internal/logredact's logguard.
package sqlguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestNoSelectFromCTEModifiedTable is a tripwire for one Postgres hazard: a
// data-modifying CTE (WITH x AS (UPDATE/INSERT/DELETE … RETURNING …)) whose
// outer statement SELECTs from the table the CTE modified, instead of from
// the CTE's RETURNING output.
//
// In Postgres, ALL parts of one statement run on the same snapshot, taken
// before the statement's own writes: the outer query of a data-modifying CTE
// therefore sees the table as it was BEFORE the CTE's UPDATE/INSERT/DELETE.
// `WITH updated AS (UPDATE t … RETURNING id) SELECT … FROM t WHERE t.id =
// (SELECT id FROM updated)` silently returns the PRE-update row. That is not
// theoretical: UpdateEngagementIfUnchanged (internal/identity/engagements.go)
// shipped exactly that shape — the conditional engagement update committed
// the new state but returned the old row, so the derived ETag never matched
// again and every guarded read-modify-write chain died with 412. It was
// caught only by the first live staging conformance run (e2a-ops
// release-pipeline run 30612956986); no offline gate looked at the shape.
// The fix (PR #775) splits the statement: UPDATE … RETURNING id, then a
// second read by primary key in the same transaction, which does see the
// first statement's writes. Reading FROM the CTE's own RETURNING output
// (`SELECT … FROM updated …`) is equally safe and stays a single statement —
// GetPrincipalByAPIKey and GetMessageWithContent (internal/identity) are the
// in-tree reference examples.
//
// What the guard does: for every tracked non-test .go file it resolves each
// string expression (including `+` concatenation chains, substituting
// same-package const strings — the original bug's outer FROM clause lived in
// the package const `engagementFrom`, so a literal-only scan would have
// missed it), finds WITH clauses, classifies each CTE body as data-modifying
// or read-only, and fails if a statement with a data-modifying CTE has an
// outer SELECT that references a modified table via FROM/JOIN.
//
// What it deliberately does NOT flag, so it stays conservative:
//   - Read-only CTEs (the webhook/webhookpub lease queries, thread_identity's
//     threadAnchorBatchQuery, detachThreadChildrenBatchTx): no CTE write, no
//     snapshot hazard of this class.
//   - Outer statements that are themselves UPDATE/INSERT/DELETE (the lease
//     queries' outer UPDATE … FROM candidates): a different statement shape
//     with its own rules; this guard pins the SELECT-read-back class only.
//   - SQL assembled through variables/function calls it cannot resolve —
//     unresolvable fragments become inert placeholders. A hazard smuggled
//     through dynamic SQL passes this guard; that stays a review concern.
//
// If this test failed on your change: either read back FROM the CTE's
// RETURNING output, or split the write and the read into two statements in
// one transaction (see UpdateEngagementIfUnchanged). If the outer SELECT's
// read of the modified table is GENUINELY intentional — you want the
// pre-write snapshot — add a `// cte-snapshot-safe: <reason>` comment on the
// line(s) directly above the query string and the guard will skip it (and
// will flag the waiver as stale if the query stops matching).
func TestNoSelectFromCTEModifiedTable(t *testing.T) {
	root := moduleRoot(t)

	byDir := map[string][]string{}
	for _, rel := range guardedGoFiles(t, root) {
		byDir[filepath.Dir(rel)] = append(byDir[filepath.Dir(rel)], rel)
	}

	filesChecked := 0
	withStatements := 0
	modifyingCTEStatements := 0
	var violations []string
	var staleWaivers []string

	for _, files := range byDir {
		fset := token.NewFileSet()
		parsed := make(map[string]*ast.File, len(files))
		for _, rel := range files {
			f, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(rel)), nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			parsed[rel] = f
			filesChecked++
		}
		consts := collectStringConsts(parsed)

		for _, rel := range files {
			scan := scanParsedFile(fset, rel, parsed[rel], consts)
			withStatements += scan.withStatements
			modifyingCTEStatements += scan.modifyingCTEStatements
			violations = append(violations, scan.violations...)
			staleWaivers = append(staleWaivers, scan.staleWaivers...)
		}
	}

	for _, v := range violations {
		t.Errorf("%s\n"+
			"In Postgres the outer query of a data-modifying CTE runs on the statement\n"+
			"snapshot, which predates the CTE's own writes — the SELECT returns the\n"+
			"PRE-write row (this killed UpdateEngagementIfUnchanged's ETag chain; found\n"+
			"live by the staging conformance gate, fixed in PR #775). Read FROM the CTE's\n"+
			"RETURNING output instead (see GetPrincipalByAPIKey / GetMessageWithContent in\n"+
			"internal/identity), or split write and read into two statements in one\n"+
			"transaction (see UpdateEngagementIfUnchanged), or — only if the pre-write\n"+
			"snapshot is genuinely intended — waive with `// cte-snapshot-safe: <reason>`\n"+
			"directly above the query string.", v)
	}
	for _, w := range staleWaivers {
		t.Errorf("%s: `cte-snapshot-safe` waiver matches no flagged query — remove the stale waiver", w)
	}

	// Non-vacuity: the guard must actually be seeing the tree's SQL. Today's
	// tree carries several WITH statements (webhook/webhookpub leases,
	// thread_identity) and at least two data-modifying CTEs that read from
	// their RETURNING output (GetPrincipalByAPIKey, GetMessageWithContent).
	// If parsing or resolution drifts so these are no longer recognised, fail
	// loudly rather than pass over an empty denominator.
	if filesChecked == 0 {
		t.Fatal("scanned no Go source files — file discovery is broken")
	}
	if withStatements < 4 {
		t.Fatalf("recognised only %d WITH statement(s) across the tree (expected >= 4) — the SQL extraction has drifted", withStatements)
	}
	if modifyingCTEStatements < 2 {
		t.Fatalf("recognised only %d statement(s) with a data-modifying CTE (expected >= 2) — the CTE classifier has drifted", modifyingCTEStatements)
	}
}

// ---------------------------------------------------------------------------
// Per-file scan (shared by the tree-wide guard and the self-tests)
// ---------------------------------------------------------------------------

type fileScan struct {
	violations             []string
	staleWaivers           []string
	withStatements         int
	modifyingCTEStatements int
}

func scanParsedFile(fset *token.FileSet, rel string, file *ast.File, consts map[string]string) fileScan {
	var scan fileScan
	waivers := waiverLines(fset, file)
	usedWaivers := map[int]bool{}

	forEachStringExpr(file, func(expr ast.Expr) {
		sql, ok := resolveString(expr, consts)
		if !ok {
			return
		}
		findings, hasWith, hasModifying := analyzeSQL(sql)
		if hasWith {
			scan.withStatements++
		}
		if hasModifying {
			scan.modifyingCTEStatements++
		}
		if len(findings) == 0 {
			return
		}
		start := fset.Position(expr.Pos()).Line
		end := fset.Position(expr.End()).Line
		if line, ok := waiverInRange(waivers, start-2, end); ok {
			usedWaivers[line] = true
			return
		}
		for _, f := range findings {
			scan.violations = append(scan.violations, fmt.Sprintf(
				"%s:%d: CTE %q modifies table %q but the outer SELECT reads FROM/JOIN %q",
				rel, start, f.cte, f.table, f.table))
		}
	})

	for _, line := range waivers {
		if !usedWaivers[line] {
			scan.staleWaivers = append(scan.staleWaivers, fmt.Sprintf("%s:%d", rel, line))
		}
	}
	return scan
}

// ---------------------------------------------------------------------------
// SQL analysis
// ---------------------------------------------------------------------------

type finding struct {
	cte   string // CTE name
	table string // table the CTE modifies and the outer SELECT reads
}

var (
	withStart    = regexp.MustCompile(`(?is)\bWITH\s+(RECURSIVE\s+)?`)
	cteHeader    = regexp.MustCompile(`(?is)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*(?:\([^)]*\)\s*)?AS\s+(?:NOT\s+MATERIALIZED\s+|MATERIALIZED\s+)?\(`)
	updateTarget = regexp.MustCompile(`(?is)^\s*UPDATE\s+(?:ONLY\s+)?(?:[A-Za-z_][A-Za-z0-9_]*\.)?([A-Za-z_][A-Za-z0-9_]*)`)
	insertTarget = regexp.MustCompile(`(?is)^\s*INSERT\s+INTO\s+(?:[A-Za-z_][A-Za-z0-9_]*\.)?([A-Za-z_][A-Za-z0-9_]*)`)
	deleteTarget = regexp.MustCompile(`(?is)^\s*DELETE\s+FROM\s+(?:ONLY\s+)?(?:[A-Za-z_][A-Za-z0-9_]*\.)?([A-Za-z_][A-Za-z0-9_]*)`)
	outerSelect  = regexp.MustCompile(`(?is)^\s*\(*\s*SELECT\b`)
)

// analyzeSQL scans one resolved SQL string. Returns the violations, whether a
// WITH clause (CTE list) was recognised, and whether any recognised CTE was
// data-modifying (both counters feed the guard's non-vacuity checks).
//
// Every `WITH` occurrence is tried until one parses as a CTE list, so
// non-CTE uses of the keyword (`timestamp WITH time zone`, `WITH
// ORDINALITY`) can neither shadow a real CTE nor trip the parser.
func analyzeSQL(sql string) (findings []finding, hasWith, hasModifying bool) {
	masked := maskLiteralsAndComments(sql)

	for _, loc := range withStart.FindAllStringIndex(masked, -1) {
		findings, ok, modifying := analyzeCTEListAt(masked, loc[1])
		if ok {
			return findings, true, modifying
		}
	}
	return nil, false, false
}

// analyzeCTEListAt parses a CTE list starting just past a WITH keyword at
// offset rest. ok is false when the text there is not a CTE list.
func analyzeCTEListAt(masked string, rest int) (findings []finding, ok, hasModifying bool) {
	type cte struct {
		name  string
		table string // non-empty when data-modifying
	}
	var ctes []cte

	for {
		m := cteHeader.FindStringSubmatchIndex(masked[rest:])
		if m == nil {
			return nil, false, false
		}
		name := masked[rest+m[2] : rest+m[3]]
		bodyStart := rest + m[1] // just past the opening paren
		bodyEnd := matchParen(masked, bodyStart)
		if bodyEnd < 0 {
			return nil, false, false
		}
		body := masked[bodyStart:bodyEnd]

		entry := cte{name: name}
		for _, target := range []*regexp.Regexp{updateTarget, insertTarget, deleteTarget} {
			if tm := target.FindStringSubmatch(body); tm != nil {
				entry.table = strings.ToLower(tm[1])
				break
			}
		}
		ctes = append(ctes, entry)

		rest = bodyEnd + 1
		// Skip whitespace; a comma continues the CTE list.
		for rest < len(masked) && (masked[rest] == ' ' || masked[rest] == '\t' || masked[rest] == '\n' || masked[rest] == '\r') {
			rest++
		}
		if rest < len(masked) && masked[rest] == ',' {
			rest++
			continue
		}
		break
	}

	for _, c := range ctes {
		if c.table != "" {
			hasModifying = true
		}
	}
	outer := masked[rest:]
	if !hasModifying || !outerSelect.MatchString(outer) {
		return nil, true, hasModifying
	}

	for _, c := range ctes {
		if c.table == "" {
			continue
		}
		// The outer SELECT referencing the modified table via FROM or JOIN —
		// anywhere, including subqueries — reads the pre-write snapshot.
		ref := regexp.MustCompile(`(?is)\b(?:FROM|JOIN)\s+(?:ONLY\s+)?(?:[A-Za-z_][A-Za-z0-9_]*\.)?` + regexp.QuoteMeta(c.table) + `\b`)
		if ref.MatchString(outer) {
			findings = append(findings, finding{cte: c.name, table: c.table})
		}
	}
	return findings, true, hasModifying
}

// maskLiteralsAndComments blanks single-quoted SQL literals and `--` line
// comments so their content can't confuse paren matching or keyword scans.
// Replacement preserves length and line structure.
func maskLiteralsAndComments(sql string) string {
	b := []byte(sql)
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '\'':
			for j := i + 1; j < len(b); j++ {
				if b[j] == '\'' {
					// Escaped '' quote: mask and continue the literal.
					if j+1 < len(b) && b[j+1] == '\'' {
						b[j], b[j+1] = ' ', ' '
						j++
						continue
					}
					for k := i + 1; k < j; k++ {
						if b[k] != '\n' {
							b[k] = ' '
						}
					}
					i = j
					break
				}
				if j == len(b)-1 {
					i = j // unterminated: mask to end
					for k := i; k < len(b); k++ {
						if b[k] != '\n' {
							b[k] = ' '
						}
					}
				}
			}
		case b[i] == '-' && i+1 < len(b) && b[i+1] == '-':
			for j := i; j < len(b) && b[j] != '\n'; j++ {
				b[j] = ' '
			}
		}
	}
	return string(b)
}

// matchParen returns the index of the ')' closing the paren just before
// start (i.e. the body opened at start), or -1.
func matchParen(s string, start int) int {
	depth := 1
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Go-source string resolution
// ---------------------------------------------------------------------------

const exprPlaceholder = " __goexpr__ "

// collectStringConsts maps every package-level string constant declared in
// the directory's files to its (recursively resolved) value. This is what
// lets the guard see through `"SELECT " + engagementColumns + engagementFrom`
// — the shape the original bug used.
func collectStringConsts(files map[string]*ast.File) map[string]string {
	// Two passes so a const referencing a const declared later still resolves.
	raw := map[string]ast.Expr{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						raw[name.Name] = vs.Values[i]
					}
				}
			}
		}
	}
	resolved := map[string]string{}
	var resolve func(name string, seen map[string]bool) (string, bool)
	resolve = func(name string, seen map[string]bool) (string, bool) {
		if v, ok := resolved[name]; ok {
			return v, true
		}
		if seen[name] {
			return "", false
		}
		seen[name] = true
		expr, ok := raw[name]
		if !ok {
			return "", false
		}
		v, ok := resolveExpr(expr, func(ident string) (string, bool) { return resolve(ident, seen) })
		if !ok {
			return "", false
		}
		resolved[name] = v
		return v, true
	}
	for name := range raw {
		resolve(name, map[string]bool{})
	}
	return resolved
}

// resolveExpr renders a string expression: literals verbatim, identifiers via
// lookup, anything else as an inert placeholder. The bool is false only when
// the expression contains no string literal at all.
func resolveExpr(expr ast.Expr, lookup func(string) (string, bool)) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return exprPlaceholder, false
		}
		v, err := strconv.Unquote(e.Value)
		if err != nil {
			return exprPlaceholder, false
		}
		return v, true
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return exprPlaceholder, false
		}
		l, lok := resolveExpr(e.X, lookup)
		r, rok := resolveExpr(e.Y, lookup)
		return l + r, lok || rok
	case *ast.ParenExpr:
		return resolveExpr(e.X, lookup)
	case *ast.Ident:
		if v, ok := lookup(e.Name); ok {
			return v, true
		}
		return exprPlaceholder, false
	default:
		return exprPlaceholder, false
	}
}

func resolveString(expr ast.Expr, consts map[string]string) (string, bool) {
	return resolveExpr(expr, func(name string) (string, bool) {
		v, ok := consts[name]
		return v, ok
	})
}

// forEachStringExpr visits every MAXIMAL string-ish expression in the file:
// a string literal or a `+` chain containing one, not nested inside a larger
// chain (so one concatenated query is analyzed exactly once, whole).
func forEachStringExpr(file *ast.File, visit func(ast.Expr)) {
	ast.Inspect(file, func(n ast.Node) bool {
		expr, ok := n.(ast.Expr)
		if !ok || !isStringish(expr) {
			return true
		}
		visit(expr)
		return false // don't descend into the chain's parts
	})
}

func isStringish(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.BinaryExpr:
		return e.Op == token.ADD && (isStringish(e.X) || isStringish(e.Y))
	case *ast.ParenExpr:
		return isStringish(e.X)
	}
	return false
}

// ---------------------------------------------------------------------------
// Waivers
// ---------------------------------------------------------------------------

const waiverMarker = "cte-snapshot-safe:"

// waiverLines returns the line numbers of every waiver comment in the file.
func waiverLines(fset *token.FileSet, file *ast.File) []int {
	var lines []int
	for _, group := range file.Comments {
		for _, c := range group.List {
			if strings.Contains(c.Text, waiverMarker) {
				lines = append(lines, fset.Position(c.Pos()).Line)
			}
		}
	}
	return lines
}

// waiverInRange reports whether a waiver comment sits within [lo, hi]
// (typically the query string's span plus the two lines above it).
func waiverInRange(waivers []int, lo, hi int) (int, bool) {
	for _, line := range waivers {
		if line >= lo && line <= hi {
			return line, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// File discovery (same approach as internal/logredact's logguard: tracked
// plus untracked-but-not-ignored files, so nested worktrees under gitignored
// paths are never attributed to this tree)
// ---------------------------------------------------------------------------

func guardedGoFiles(t *testing.T, root string) []string {
	t.Helper()

	if out, err := exec.Command("git", "-C", root, "ls-files", "-z",
		"--cached", "--others", "--exclude-standard", "*.go").Output(); err == nil {
		var files []string
		seen := map[string]bool{}
		for _, rel := range strings.Split(string(out), "\x00") {
			if rel == "" || strings.HasSuffix(rel, "_test.go") || seen[rel] {
				continue
			}
			seen[rel] = true
			files = append(files, rel)
		}
		return files
	}

	var files []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "web", "sdks", "docs":
				return filepath.SkipDir
			}
			if path != root {
				if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
					return filepath.SkipDir
				}
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
		files = append(files, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatalf("walk module source: %v", err)
	}
	return files
}

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
