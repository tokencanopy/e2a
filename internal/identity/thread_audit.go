package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// ThreadIdentityAuditBatchSize bounds the number of message rows inspected
	// by one hourly maintenance pass. Message IDs carry random suffixes, so
	// walking the primary-key order gives a rotating sample without sorting or
	// scanning the production-sized table.
	ThreadIdentityAuditBatchSize = 1000
	// ThreadIdentityAuditMaxDepth bounds recursive parent traversal per sampled
	// row. Hitting the cap is surfaced as an invariant measurement but does not
	// mutate a possibly valid long chain.
	ThreadIdentityAuditMaxDepth = 64
)

// ThreadInvariantViolations counts invalid diagnostic parent edges observed in
// one bounded audit batch. Thread membership itself is never repaired.
type ThreadInvariantViolations struct {
	DanglingParent   int
	CrossAgentParent int
	ThreadMismatch   int
	Cycle            int
	CycleDepthLimit  int
}

// ThreadNullAgeBuckets counts recently created threadless rows in the rotating
// sample. Rows older than 24 hours are deliberately excluded: historical nulls
// are supported rollout state, while recent nulls indicate a live writer gap.
type ThreadNullAgeBuckets struct {
	LessThanOneHour      int
	OneToSixHours        int
	SixToTwentyFourHours int
}

// ThreadIdentityAuditResult is the low-cardinality output of one bounded
// integrity/measurement pass.
type ThreadIdentityAuditResult struct {
	Scanned                  int
	NextCursor               string
	Violations               ThreadInvariantViolations
	RepairedParentPointers   int
	NullThreadsByAge         ThreadNullAgeBuckets
	ThreadsSampled           int
	MultiConversationThreads int
	ConversationsSampled     int
	MultiThreadConversations int
}

type threadAuditRow struct {
	id             string
	agentID        string
	threadID       string
	threadParentID string
	conversationID string
	createdAt      time.Time
}

type threadAuditParent struct {
	agentID  string
	threadID string
}

type threadParentRepair struct {
	id       string
	parentID string
	kind     string
}

// AuditThreadIdentity runs one production-sized bounded pass.
func (s *Store) AuditThreadIdentity(ctx context.Context, afterID string) (ThreadIdentityAuditResult, error) {
	return s.AuditThreadIdentityBatch(
		ctx, afterID, ThreadIdentityAuditBatchSize, ThreadIdentityAuditMaxDepth,
	)
}

// AuditThreadIdentityBatch scans at most limit messages after afterID, detects
// invalid direct parent edges and bounded-depth cycles, and clears only those
// parent pointers. It never writes thread_id or rfc_message_id_key.
//
// The returned cursor advances in primary-key order. An empty cursor means the
// end of the table was reached; the next periodic starts a new rotating pass.
// maxDepth bounds the recursive work to roughly limit*maxDepth parent steps.
func (s *Store) AuditThreadIdentityBatch(ctx context.Context, afterID string, limit, maxDepth int) (ThreadIdentityAuditResult, error) {
	if limit <= 0 || limit > 5000 {
		return ThreadIdentityAuditResult{}, fmt.Errorf("thread audit limit must be between 1 and 5000")
	}
	if maxDepth <= 0 || maxDepth > 256 {
		return ThreadIdentityAuditResult{}, fmt.Errorf("thread audit max depth must be between 1 and 256")
	}

	var result ThreadIdentityAuditResult
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		batch, nextCursor, err := loadThreadAuditBatch(ctx, tx, afterID, limit)
		if err != nil {
			return err
		}
		result.Scanned = len(batch)
		result.NextCursor = nextCursor
		if len(batch) == 0 {
			return nil
		}

		measureThreadAuditBatch(batch, time.Now(), &result)

		parentIDs := make([]string, 0, len(batch))
		seenParents := make(map[string]struct{}, len(batch))
		seedIDs := make([]string, 0, len(batch))
		for _, row := range batch {
			seedIDs = append(seedIDs, row.id)
			if row.threadParentID == "" {
				continue
			}
			if _, exists := seenParents[row.threadParentID]; exists {
				continue
			}
			seenParents[row.threadParentID] = struct{}{}
			parentIDs = append(parentIDs, row.threadParentID)
		}
		parents, err := loadThreadAuditParents(ctx, tx, parentIDs)
		if err != nil {
			return err
		}

		repairs := make(map[string]threadParentRepair)
		for _, row := range batch {
			if row.threadParentID == "" {
				continue
			}
			parent, exists := parents[row.threadParentID]
			switch {
			case !exists:
				result.Violations.DanglingParent++
				repairs[row.id] = threadParentRepair{id: row.id, parentID: row.threadParentID, kind: "dangling_parent"}
			case parent.agentID != row.agentID:
				result.Violations.CrossAgentParent++
				repairs[row.id] = threadParentRepair{id: row.id, parentID: row.threadParentID, kind: "cross_agent_parent"}
			case row.threadID == "" || parent.threadID == "" ||
				!IsValidThreadID(row.threadID) || !IsValidThreadID(parent.threadID) ||
				row.threadID != parent.threadID:
				result.Violations.ThreadMismatch++
				repairs[row.id] = threadParentRepair{id: row.id, parentID: row.threadParentID, kind: "thread_mismatch"}
			}
		}

		cycleEdges, depthLimited, err := findThreadAuditCycles(ctx, tx, seedIDs, maxDepth)
		if err != nil {
			return err
		}
		result.Violations.Cycle = len(cycleEdges)
		result.Violations.CycleDepthLimit = depthLimited
		for _, edge := range cycleEdges {
			if _, alreadyInvalid := repairs[edge.id]; alreadyInvalid {
				continue
			}
			repairs[edge.id] = edge
		}

		repaired, err := clearInvalidThreadParents(ctx, tx, repairs)
		if err != nil {
			return err
		}
		result.RepairedParentPointers = repaired
		return nil
	})
	return result, err
}

func loadThreadAuditBatch(ctx context.Context, tx pgx.Tx, afterID string, limit int) ([]threadAuditRow, string, error) {
	rows, err := tx.Query(ctx,
		`SELECT id,
		        agent_id,
		        COALESCE(thread_id, ''),
		        COALESCE(thread_parent_id, ''),
		        COALESCE(conversation_id, ''),
		        created_at
		   FROM messages
		  WHERE id > $1
		  ORDER BY id
		  LIMIT $2`,
		afterID, limit+1,
	)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	batch := make([]threadAuditRow, 0, limit+1)
	for rows.Next() {
		var row threadAuditRow
		if err := rows.Scan(
			&row.id, &row.agentID, &row.threadID, &row.threadParentID,
			&row.conversationID, &row.createdAt,
		); err != nil {
			return nil, "", err
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(batch) <= limit {
		return batch, "", nil
	}
	batch = batch[:limit]
	return batch, batch[len(batch)-1].id, nil
}

func loadThreadAuditParents(ctx context.Context, tx pgx.Tx, parentIDs []string) (map[string]threadAuditParent, error) {
	out := make(map[string]threadAuditParent, len(parentIDs))
	if len(parentIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT id, agent_id, COALESCE(thread_id, '')
		   FROM messages
		  WHERE id = ANY($1)`,
		parentIDs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var parent threadAuditParent
		if err := rows.Scan(&id, &parent.agentID, &parent.threadID); err != nil {
			return nil, err
		}
		out[id] = parent
	}
	return out, rows.Err()
}

func measureThreadAuditBatch(batch []threadAuditRow, now time.Time, result *ThreadIdentityAuditResult) {
	type mailboxThread struct {
		agentID  string
		threadID string
	}
	type mailboxConversation struct {
		agentID        string
		conversationID string
	}

	threadConversations := make(map[mailboxThread]map[string]struct{})
	conversationThreads := make(map[mailboxConversation]map[string]struct{})
	for _, row := range batch {
		if row.threadID == "" {
			age := now.Sub(row.createdAt)
			switch {
			case age < time.Hour:
				result.NullThreadsByAge.LessThanOneHour++
			case age < 6*time.Hour:
				result.NullThreadsByAge.OneToSixHours++
			case age < 24*time.Hour:
				result.NullThreadsByAge.SixToTwentyFourHours++
			}
			continue
		}

		threadKey := mailboxThread{agentID: row.agentID, threadID: row.threadID}
		if _, exists := threadConversations[threadKey]; !exists {
			threadConversations[threadKey] = make(map[string]struct{})
		}
		if row.conversationID == "" {
			continue
		}
		threadConversations[threadKey][row.conversationID] = struct{}{}

		conversationKey := mailboxConversation{agentID: row.agentID, conversationID: row.conversationID}
		if _, exists := conversationThreads[conversationKey]; !exists {
			conversationThreads[conversationKey] = make(map[string]struct{})
		}
		conversationThreads[conversationKey][row.threadID] = struct{}{}
	}

	result.ThreadsSampled = len(threadConversations)
	for _, conversations := range threadConversations {
		if len(conversations) > 1 {
			result.MultiConversationThreads++
		}
	}
	result.ConversationsSampled = len(conversationThreads)
	for _, threads := range conversationThreads {
		if len(threads) > 1 {
			result.MultiThreadConversations++
		}
	}
}

func findThreadAuditCycles(ctx context.Context, tx pgx.Tx, seedIDs []string, maxDepth int) ([]threadParentRepair, int, error) {
	rows, err := tx.Query(ctx,
		`WITH RECURSIVE walk (
		     root_id, current_id, parent_id, path, depth, cycle_from, cycle_to
		 ) AS (
		     SELECT m.id,
		            m.id,
		            COALESCE(m.thread_parent_id, ''),
		            ARRAY[m.id]::text[],
		            0,
		            ''::text,
		            ''::text
		       FROM messages m
		      WHERE m.id = ANY($1)
		     UNION ALL
		     SELECT w.root_id,
		            p.id,
		            COALESCE(p.thread_parent_id, ''),
		            w.path || p.id,
		            w.depth + 1,
		            CASE WHEN p.id = ANY(w.path) THEN w.current_id ELSE '' END,
		            CASE WHEN p.id = ANY(w.path) THEN p.id ELSE '' END
		       FROM walk w
		       JOIN messages p ON p.id = w.parent_id
		      WHERE w.parent_id <> ''
		        AND w.cycle_from = ''
		        AND w.depth < $2
		 )
		 SELECT 'cycle', cycle_from, cycle_to
		   FROM walk
		  WHERE cycle_from <> ''
		  GROUP BY cycle_from, cycle_to
		 UNION ALL
		 SELECT 'depth_limit', root_id, ''
		   FROM walk
		  WHERE depth = $2
		    AND parent_id <> ''
		    AND cycle_from = ''
		  GROUP BY root_id`,
		seedIDs, maxDepth,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var cycleEdges []threadParentRepair
	depthLimited := 0
	for rows.Next() {
		var kind, id, parentID string
		if err := rows.Scan(&kind, &id, &parentID); err != nil {
			return nil, 0, err
		}
		if kind == "depth_limit" {
			depthLimited++
			continue
		}
		cycleEdges = append(cycleEdges, threadParentRepair{
			id: id, parentID: parentID, kind: "cycle",
		})
	}
	return cycleEdges, depthLimited, rows.Err()
}

func clearInvalidThreadParents(ctx context.Context, tx pgx.Tx, repairs map[string]threadParentRepair) (int, error) {
	if len(repairs) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(repairs))
	parentIDs := make([]string, 0, len(repairs))
	for _, repair := range repairs {
		ids = append(ids, repair.id)
		parentIDs = append(parentIDs, repair.parentID)
	}
	rows, err := tx.Query(ctx,
		`UPDATE messages AS m
		    SET thread_parent_id = NULL
		   FROM unnest($1::text[], $2::text[]) AS repair(id, parent_id)
		  WHERE m.id = repair.id
		    AND m.thread_parent_id = repair.parent_id
		 RETURNING m.id`,
		ids, parentIDs,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	repaired := 0
	for rows.Next() {
		repaired++
	}
	return repaired, rows.Err()
}
