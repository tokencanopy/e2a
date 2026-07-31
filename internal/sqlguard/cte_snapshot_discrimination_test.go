package sqlguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestAnalyzeSQLDiscrimination pins the matcher's own behaviour: the shapes
// it must catch (the reason it exists) and the shapes it must not flag (the
// reason it can hold a zero-waiver tree). The clean cases are distilled from
// the five real in-tree CTE sites audited in PR #775.
func TestAnalyzeSQLDiscrimination(t *testing.T) {
	hazardous := map[string]string{
		"outer SELECT re-reads the UPDATEd table (the UpdateEngagementIfUnchanged bug)": `
			WITH updated AS (
				UPDATE contact_engagements
				   SET stage = COALESCE($4, stage), updated_at = now()
				 WHERE user_id = $1 AND agent_id = $2 AND address = $3
				 RETURNING id
			)
			SELECT ce.id, ce.stage FROM contact_engagements ce
			 WHERE ce.id = (SELECT id FROM updated)`,
		"outer SELECT JOINs the INSERTed table": `
			WITH ins AS (
				INSERT INTO messages (id, subject) VALUES ($1, $2) RETURNING id
			)
			SELECT m.id, m.subject FROM ins JOIN messages m ON m.id = ins.id`,
		"outer SELECT re-reads a DELETEd table": `
			WITH gone AS (
				DELETE FROM api_keys WHERE expires_at < now() RETURNING user_id
			)
			SELECT count(*) FROM api_keys WHERE user_id IN (SELECT user_id FROM gone)`,
		"second CTE of a list modifies, outer re-reads it": `
			WITH scope AS (
				SELECT id FROM users WHERE account_class = 'internal'
			), upd AS (
				UPDATE agent_identities SET deleted_at = now()
				 WHERE user_id IN (SELECT id FROM scope) RETURNING id
			)
			SELECT a.id FROM agent_identities a WHERE a.deleted_at IS NOT NULL`,
		"schema-qualified reference to the modified table": `
			WITH updated AS (
				UPDATE messages SET inbox_status = 'read' WHERE id = $1 RETURNING id
			)
			SELECT m.id FROM public.messages m WHERE m.id = (SELECT id FROM updated)`,
	}
	for label, sql := range hazardous {
		findings, hasWith, hasModifying := analyzeSQL(sql)
		if len(findings) == 0 {
			t.Errorf("matcher missed a hazardous shape (%s); hasWith=%v hasModifying=%v", label, hasWith, hasModifying)
		}
	}

	clean := map[string]string{
		"outer SELECT reads the CTE's RETURNING output (GetPrincipalByAPIKey shape)": `
			WITH touched AS (
				UPDATE api_keys SET last_used_at = now()
				WHERE key_hash = $1 RETURNING user_id, scope, agent_id
			)
			SELECT u.id, u.email, t.scope FROM touched t JOIN users u ON u.id = t.user_id`,
		"outer SELECT LEFT JOINs a DIFFERENT table (GetMessageWithContent shape)": `
			WITH upd AS (
				UPDATE messages SET inbox_status = 'read' WHERE id = $1 RETURNING id, subject
			)
			SELECT upd.id, upd.subject, COALESCE(wd.status, '')
			FROM upd LEFT JOIN webhook_deliveries wd ON wd.message_id = upd.id`,
		"read-only CTE feeding an outer UPDATE (webhook lease shape)": `
			WITH candidates AS (
				SELECT id FROM webhook_events
				WHERE status = 'pending' ORDER BY created_at LIMIT $1
				FOR UPDATE SKIP LOCKED
			)
			UPDATE webhook_events e SET next_poll_at = now()
			FROM candidates c WHERE e.id = c.id
			RETURNING e.id, e.envelope`,
		"read-only locking CTE feeding an outer UPDATE (detachThreadChildrenBatchTx shape)": `
			WITH children AS (
				SELECT id FROM messages WHERE thread_parent_id = ANY($1)
				ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED
			)
			UPDATE messages AS m SET thread_parent_id = NULL
			FROM children WHERE m.id = children.id`,
		"read-only CTE with an outer SELECT over the same table (threadAnchorBatchQuery shape)": `
			WITH requested AS (
				SELECT * FROM unnest($2::integer[], $3::text[]) AS input(ordinal, original)
			)
			SELECT requested.ordinal, m.id FROM requested
			CROSS JOIN LATERAL (
				SELECT id FROM messages WHERE rfc_message_id_key = requested.original LIMIT $5
			) m`,
		"modifying CTE, outer SELECT reads only other tables": `
			WITH upd AS (
				UPDATE messages SET flagged = true WHERE id = $1 RETURNING agent_id
			)
			SELECT a.email FROM upd JOIN agent_identities a ON a.id = upd.agent_id`,
		"WITH TIME ZONE is not a CTE": `
			SELECT id, created_at::timestamp WITH TIME ZONE FROM messages WHERE id = $1`,
		"modified table name inside a masked string literal is not a reference": `
			WITH upd AS (
				UPDATE messages SET status = 'read' WHERE id = $1 RETURNING id
			)
			SELECT x.note FROM upd JOIN audit_log x ON x.ref = upd.id AND x.kind = 'from messages'`,
	}
	for label, sql := range clean {
		findings, _, _ := analyzeSQL(sql)
		if len(findings) != 0 {
			t.Errorf("matcher false-positived on a clean shape (%s): %v", label, findings)
		}
	}

	// A prefix-collision guard: modifying table "messages" must not be
	// "found" in a reference to "messages_archive".
	findings, _, _ := analyzeSQL(`
		WITH upd AS (
			UPDATE messages SET status = 'read' WHERE id = $1 RETURNING id
		)
		SELECT a.id FROM upd JOIN messages_archive a ON a.src = upd.id`)
	if len(findings) != 0 {
		t.Errorf("matcher confused messages_archive with messages: %v", findings)
	}
}

// TestScanParsedFileEndToEnd drives the same per-file pipeline the tree-wide
// guard uses over synthetic Go source, pinning the parts a raw analyzeSQL
// call can't reach: const-concatenation resolution (the ORIGINAL bug's outer
// FROM clause lived in the package const engagementFrom — a literal-only scan
// misses it), waiver comments, and stale-waiver detection.
func TestScanParsedFileEndToEnd(t *testing.T) {
	parse := func(t *testing.T, src string) (*token.FileSet, *ast.File, map[string]string) {
		t.Helper()
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "synthetic.go", src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse synthetic source: %v", err)
		}
		return fset, file, collectStringConsts(map[string]*ast.File{"synthetic.go": file})
	}

	t.Run("const concatenation reproducing the original bug is flagged", func(t *testing.T) {
		fset, file, consts := parse(t, `package identity

const engagementColumns = "ce.id, ce.stage, ce.updated_at"
const engagementFrom = " FROM contact_engagements ce JOIN contacts c ON c.id = ce.contact_id"

func q() string {
	return "WITH updated AS (" +
		"UPDATE contact_engagements SET stage = $4, updated_at = now() " +
		"WHERE user_id = $1 RETURNING id" +
		") SELECT " + engagementColumns + engagementFrom +
		" WHERE ce.id = (SELECT id FROM updated)"
}
`)
		scan := scanParsedFile(fset, "synthetic.go", file, consts)
		if len(scan.violations) != 1 || !strings.Contains(scan.violations[0], "contact_engagements") {
			t.Fatalf("expected exactly one contact_engagements violation, got %v", scan.violations)
		}
	})

	t.Run("waiver comment suppresses the finding and registers as used", func(t *testing.T) {
		fset, file, consts := parse(t, `package identity

func q() string {
	// cte-snapshot-safe: this read WANTS the pre-update row (audit trail diff).
	return "WITH upd AS (UPDATE messages SET status = 'read' WHERE id = $1 RETURNING id) " +
		"SELECT m.status FROM messages m WHERE m.id = (SELECT id FROM upd)"
}
`)
		scan := scanParsedFile(fset, "synthetic.go", file, consts)
		if len(scan.violations) != 0 {
			t.Fatalf("waived query still flagged: %v", scan.violations)
		}
		if len(scan.staleWaivers) != 0 {
			t.Fatalf("used waiver reported stale: %v", scan.staleWaivers)
		}
	})

	t.Run("waiver that suppresses nothing is reported stale", func(t *testing.T) {
		fset, file, consts := parse(t, `package identity

func q() string {
	// cte-snapshot-safe: left behind after the query was fixed.
	return "WITH upd AS (UPDATE messages SET status = 'read' WHERE id = $1 RETURNING id) " +
		"SELECT upd.id FROM upd"
}
`)
		scan := scanParsedFile(fset, "synthetic.go", file, consts)
		if len(scan.violations) != 0 {
			t.Fatalf("clean query flagged: %v", scan.violations)
		}
		if len(scan.staleWaivers) != 1 {
			t.Fatalf("expected one stale waiver, got %v", scan.staleWaivers)
		}
	})

	t.Run("unresolvable dynamic fragments stay inert", func(t *testing.T) {
		fset, file, consts := parse(t, `package identity

func q(lockClause string) string {
	return "WITH children AS (SELECT id FROM messages WHERE thread_parent_id = ANY($1) " +
		lockClause +
		") UPDATE messages AS m SET thread_parent_id = NULL FROM children WHERE m.id = children.id"
}
`)
		scan := scanParsedFile(fset, "synthetic.go", file, consts)
		if len(scan.violations) != 0 {
			t.Fatalf("dynamic-fragment query flagged: %v", scan.violations)
		}
		if scan.withStatements != 1 {
			t.Fatalf("expected the WITH statement to be recognised despite the placeholder, got %d", scan.withStatements)
		}
	})
}
