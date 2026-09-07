package identity

import (
	"context"
	"fmt"
	"time"
)

// ScheduledListItem is one row of the scheduled-send queue (GET /v1/scheduled-messages):
// non-secret summary of an outbound message that has been accepted and is
// waiting for a future send_at to fire (#815). Bodies are fetched per-item via
// the shared message-detail read path.
//
// A scheduled message is NOT a review: it is delivery_status='accepted' with a
// future scheduled_at (migration 084 introduces no new delivery_status — the
// only discriminator is the non-null future scheduled_at). Held drafts
// (pending_review) never reach delivery_status='accepted', so they belong to
// the review queue, not here; the two sets are disjoint by construction.
type ScheduledListItem struct {
	ID             string
	AgentID        string
	Direction      string // always outbound for scheduled sends
	Sender         string
	HeaderFrom     string
	To             []string
	Subject        string
	ConversationID string
	DeliveryStatus string
	CreatedAt      time.Time
	// ScheduledAt is the instant the queued send was scheduled to be submitted.
	// Always non-nil for rows this query returns, but NOT always in the future:
	// a send whose fire time has passed can still be waiting (e.g. deferred at
	// fire time by the daily send cap, which leaves the row 'accepted' and stamps
	// no new scheduled_at). Such overdue-but-pending rows are included here so a
	// deferred send stays visible instead of vanishing until it fires.
	ScheduledAt *time.Time
}

// ListScheduled returns one page of outbound messages accepted and awaiting a
// scheduled send across all of userID's agents, SOONEST-FIRST, keyset-paginated
// on (scheduled_at, id). The caller passes limit (fetch limit+1 to detect a
// further page; limit<=0 returns every row unpaginated) and the after-key from
// the previous page's last row (zero afterScheduledAt = first page).
//
// The set is delivery_status='accepted' with a non-null scheduled_at (migration
// 084's framing of a scheduled send). Once the River job fires, delivery_status
// moves off 'accepted' (sent/failed) and the row drops out. We deliberately do
// NOT bound scheduled_at to the future: a send can be deferred at fire time
// (e.g. the daily send cap in outbound_async.go releases the claim, snoozes the
// job to the next UTC reset, and leaves scheduled_at untouched) and would then
// sit 'accepted' with a now-past scheduled_at for hours — neither sent nor held.
// Excluding past rows made such a deferred send vanish from every dashboard tab
// until it fired; including them keeps it visible (soonest-first puts the most
// overdue at the top). A genuinely stuck send is still terminated by the send
// worker's retry horizon (MarkFailed → not 'accepted' → drops out here).
//
// Held drafts (pending_review) are excluded structurally — they are never
// delivery_status='accepted' — so this queue and the review queue (ListReviews)
// do not overlap.
//
// SECURITY: account-scoped operator flow only; the user join is the
// tenant-isolation guard. Mirrors ListReviews' scoping.
func (s *Store) ListScheduled(ctx context.Context, userID string, limit int, afterScheduledAt time.Time, afterID string) ([]ScheduledListItem, error) {
	q := `SELECT m.id, m.agent_id, m.direction, m.sender,
		        COALESCE(m.header_from, ''), m.to_recipients,
		        COALESCE(m.subject, ''), COALESCE(m.conversation_id, ''),
		        COALESCE(m.delivery_status, ''), m.created_at, m.scheduled_at
		   FROM messages m
		   JOIN agent_identities a ON a.id = m.agent_id
		  WHERE a.user_id = $1 AND a.deleted_at IS NULL
		    AND m.deleted_at IS NULL
		    AND m.direction = 'outbound'
		    AND m.delivery_status = 'accepted'
		    AND m.scheduled_at IS NOT NULL`
	args := []interface{}{userID}
	if !afterScheduledAt.IsZero() {
		i := len(args) + 1
		q += fmt.Sprintf(` AND (m.scheduled_at > $%d OR (m.scheduled_at = $%d AND m.id > $%d))`, i, i, i+1)
		args = append(args, afterScheduledAt, afterID)
	}
	q += ` ORDER BY m.scheduled_at ASC, m.id ASC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT $%d`, len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledListItem
	for rows.Next() {
		var it ScheduledListItem
		if err := rows.Scan(&it.ID, &it.AgentID, &it.Direction, &it.Sender,
			&it.HeaderFrom, &it.To, &it.Subject, &it.ConversationID,
			&it.DeliveryStatus, &it.CreatedAt, &it.ScheduledAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
