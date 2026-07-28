package contactdue

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// DueClaimer selects due schedules and advances their notification marker in a
// caller-owned transaction.
type DueClaimer interface {
	ClaimDueEngagementsTx(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]identity.DueEngagement, error)
}

// TxOutbox is the one outbox capability this module needs. The narrow seam
// makes the atomic rollback behavior directly testable while the production
// webhookpub.Outbox satisfies it without an adapter.
type TxOutbox interface {
	PublishTx(ctx context.Context, tx pgx.Tx, e webhookpub.Event) error
}

// OutboxPublisher owns the schedule-claim → durable-event invariant. Both
// writes use one PostgreSQL transaction, eliminating the loss window where a
// schedule was previously marked notified before a best-effort publisher tried
// to create the event.
type OutboxPublisher struct {
	pool    *pgxpool.Pool
	claimer DueClaimer
	outbox  TxOutbox
}

// NewOutboxPublisher builds the atomic batch publisher.
func NewOutboxPublisher(pool *pgxpool.Pool, claimer DueClaimer, outbox TxOutbox) *OutboxPublisher {
	return &OutboxPublisher{pool: pool, claimer: claimer, outbox: outbox}
}

// PublishDueBatch claims at most limit schedules and persists every wake-up in
// the same transaction. On error, attempted reports how many schedules had
// been selected before rollback so observability can count the affected batch.
func (p *OutboxPublisher) PublishDueBatch(ctx context.Context, now time.Time, limit int) (attempted int, err error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	due, err := p.claimer.ClaimDueEngagementsTx(ctx, tx, now, limit)
	if err != nil {
		return 0, err
	}
	for _, d := range due {
		if err := p.outbox.PublishTx(ctx, tx, eventForDue(d)); err != nil {
			return len(due), err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return len(due), err
	}
	return len(due), nil
}

// eventForDue creates the self-contained wake-up payload. It deliberately does
// not carry suggested content: e2a wakes; the agent decides and writes.
func eventForDue(d identity.DueEngagement) webhookpub.Event {
	contact := map[string]any{
		"address":      d.Address,
		"display_name": d.DisplayName,
		"metadata":     d.ContactMetadata,
	}
	data := map[string]any{
		"agent_email":    d.AgentEmail,
		"address":        d.Address,
		"stage":          d.Stage,
		"next_action_at": d.NextActionAt,
		"replied":        d.Replied,
		"outbound_count": d.OutboundCount,
		"contact":        contact,
	}
	if d.LastOutboundAt != nil {
		data["last_outbound_at"] = *d.LastOutboundAt
	}
	if d.LastConversationID != "" {
		data["last_conversation_id"] = d.LastConversationID
	}
	e := webhookpub.NewEvent(webhookpub.EventContactDue, d.UserID, data)
	e.AgentID = d.AgentEmail
	e.ConversationID = d.LastConversationID
	e.ID = webhookpub.DeterministicEventID(
		d.EngagementID+"\x00"+d.NextActionAt.UTC().Format("20060102150405.000000000"),
		webhookpub.EventContactDue)
	return e
}
