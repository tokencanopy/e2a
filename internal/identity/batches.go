package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Batch is the parent header row for a batch-send request. One batches row
// is inserted per successful POST /v1/agents/{email}/batches accept-tx; each
// resulting messages row carries the batch_id back to this parent. Items
// dropped by the suppression filter get no messages row — the drop is
// captured in SuppressedJSON. See docs/design/batch-send.md §3, §7.1.
type Batch struct {
	BatchID        string    `json:"batch_id"`
	UserID         string    `json:"user_id"`
	AgentID        string    `json:"agent_id"`
	Requested      int       `json:"requested"`
	Accepted       int       `json:"accepted"`
	SuppressedJSON []byte    `json:"-"` // opaque JSONB; decode via DecodeSuppressed
	RequestID      string    `json:"request_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// BatchSuppressedItem is one entry inside batches.suppressed_json — the
// record of an item that was dropped by the suppression filter during the
// accept-tx (docs/design/batch-send.md §1.3, §2.2). Field names match the
// wire shape returned by GET /v1/batches/{id}.
type BatchSuppressedItem struct {
	ItemIndex int    `json:"item_index"`
	Address   string `json:"address"`
	Reason    string `json:"reason"`
}

// DecodeSuppressed unmarshals the opaque SuppressedJSON blob into a typed
// slice. Returns an empty (non-nil) slice when the blob is empty or is the
// canonical '[]' default — callers can iterate the result without a
// nil-check.
func (b *Batch) DecodeSuppressed() ([]BatchSuppressedItem, error) {
	if len(b.SuppressedJSON) == 0 {
		return []BatchSuppressedItem{}, nil
	}
	var items []BatchSuppressedItem
	if err := json.Unmarshal(b.SuppressedJSON, &items); err != nil {
		return nil, fmt.Errorf("decode suppressed_json: %w", err)
	}
	if items == nil {
		return []BatchSuppressedItem{}, nil
	}
	return items, nil
}

// BatchStatusRollup is the per-status count of the batch's child messages
// rows, computed by BatchStatusRollupByID via one grouped query. Statuses
// with no matching rows read as zero — no distinction between "field absent"
// and "count is zero". Field set mirrors internal/delivery/status.go's
// terminal + in-flight vocabulary (accepted → sending → sent →
// {delivered, deferred, bounced, complained, failed}).
type BatchStatusRollup struct {
	Accepted   int `json:"accepted"`
	Sending    int `json:"sending"`
	Sent       int `json:"sent"`
	Delivered  int `json:"delivered"`
	Deferred   int `json:"deferred"`
	Bounced    int `json:"bounced"`
	Complained int `json:"complained"`
	Failed     int `json:"failed"`
}

// OutboundMessageInput bundles the arguments CreateOutboundMessagesTx accepts
// for one message. Mirrors CreateOutboundMessageTx's positional args as a
// struct so a batch of N doesn't need a 14-arg loop body. When BatchID is
// non-empty the resulting messages row is linked back to that batch header
// via the FK added in migration 067. See docs/design/batch-send.md §9
// (accept-tx step 10.b).
type OutboundMessageInput struct {
	AgentID           string
	ToRecipients      []string
	CC                []string
	BCC               []string
	Subject           string
	MsgType           string
	Method            string
	ProviderMessageID string
	ConversationID    string
	RawMessage        []byte
	DeliveryStatus    string
	EnvelopeFrom      string
	SentAs            string
	BatchID           string // when part of a batch; empty for single-send (nulled in SQL)
}

// NewBatchID mints a durable batch id in the project's `<prefix>_<gen>` form
// (see NewMessageID). Callers that mint many ids in one accept-tx should
// call this once per batch, not per message.
func NewBatchID() string {
	return "bat_" + generateID()
}

// CreateBatchTx inserts a batches row inside the caller's transaction. The
// caller is expected to have populated every field on the *Batch — Batch.
// SuppressedJSON is passed through opaquely, so the caller controls the
// exact byte-level shape (marshaling of []BatchSuppressedItem happens at the
// accept-tx site where the drops were computed). Empty SuppressedJSON is
// stored as the '[]'::jsonb default.
func (s *Store) CreateBatchTx(ctx context.Context, tx pgx.Tx, b *Batch) error {
	if b == nil {
		return errors.New("batch: nil input")
	}
	if b.BatchID == "" {
		return errors.New("batch: empty batch_id")
	}
	if b.UserID == "" {
		return errors.New("batch: empty user_id")
	}
	if b.AgentID == "" {
		return errors.New("batch: empty agent_id")
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	suppressed := b.SuppressedJSON
	if len(suppressed) == 0 {
		suppressed = []byte("[]")
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO batches (batch_id, user_id, agent_id, requested, accepted, suppressed_json, request_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)`,
		b.BatchID, b.UserID, b.AgentID, b.Requested, b.Accepted, suppressed, b.RequestID, b.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert batches: %w", err)
	}
	return nil
}

// GetBatch fetches a single batch row by id. Returns nil (with no error)
// when the batch does not exist — mirrors the not-found convention of
// GetMessage and friends.
func (s *Store) GetBatch(ctx context.Context, batchID string) (*Batch, error) {
	if batchID == "" {
		return nil, errors.New("batch: empty batch_id")
	}
	var b Batch
	err := s.pool.QueryRow(ctx,
		`SELECT batch_id, user_id, agent_id, requested, accepted, suppressed_json, request_id, created_at
		 FROM batches WHERE batch_id = $1`,
		batchID,
	).Scan(&b.BatchID, &b.UserID, &b.AgentID, &b.Requested, &b.Accepted, &b.SuppressedJSON, &b.RequestID, &b.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("select batches: %w", err)
	}
	return &b, nil
}

// BatchStatusRollupByID computes the delivery-status histogram over the
// batch's child messages rows in one grouped query. Not cached — batch
// observation is a rare, poll-after-send operation and at ≤100 rows per
// batch the query is cheap (the partial index in migration 067 makes the
// WHERE batch_id = $1 scan a bounded lookup). See docs/design/batch-send.md
// §7.1.
//
// Returns an empty rollup (all zeros) if the batch has no children rows —
// this is the correct outcome for an all-suppressed batch (§14 Q9), and
// distinguishable from a bad batch_id via a prior GetBatch check.
func (s *Store) BatchStatusRollupByID(ctx context.Context, batchID string) (*BatchStatusRollup, error) {
	if batchID == "" {
		return nil, errors.New("batch: empty batch_id")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT COALESCE(delivery_status, ''), count(*)
		 FROM messages
		 WHERE batch_id = $1
		 GROUP BY delivery_status`,
		batchID,
	)
	if err != nil {
		return nil, fmt.Errorf("rollup batches: %w", err)
	}
	defer rows.Close()

	rollup := &BatchStatusRollup{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan rollup row: %w", err)
		}
		switch status {
		case "accepted", "":
			// Empty string tolerates any legacy pre-async-pipeline row that
			// might exist without a delivery_status; it should not happen for
			// batch children (which are always inserted with an explicit
			// status) but is defensively grouped with `accepted`.
			rollup.Accepted += n
		case "sending":
			rollup.Sending += n
		case "sent":
			rollup.Sent += n
		case "delivered":
			rollup.Delivered += n
		case "deferred":
			rollup.Deferred += n
		case "bounced":
			rollup.Bounced += n
		case "complained":
			rollup.Complained += n
		case "failed":
			rollup.Failed += n
		}
		// Unknown statuses are silently dropped — the message model is an
		// open set (async-send-contract §3.1) and the rollup is not the
		// place to enforce a closed vocabulary. If a new status is added,
		// this switch is where to extend the rollup type.
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter rollup rows: %w", err)
	}
	return rollup, nil
}

// CreateOutboundMessagesTx inserts multiple outbound messages inside the
// caller's transaction, matching the single-row CreateOutboundMessageTx
// semantics per input. All inputs share the same tx, so either every row
// commits or none do (matching the batch accept-tx atomicity in
// docs/design/batch-send.md §9 step 10.d).
//
// Returns the resulting Messages in the same order as the inputs (positional,
// no re-sort). If any single input fails, the tx is left dirty for the
// caller to Rollback — this function does NOT rollback on the caller's
// behalf, matching WithTx's outer-scope discipline.
//
// This is a straight loop of tx.Exec calls — the repo has no established
// pgx.CopyFrom or pgx.Batch prior art, and a 100-row loop against a warm
// pool is well within the ≤100ms accept-tx budget (docs/design/batch-send.md
// §12 load-smoke target).
func (s *Store) CreateOutboundMessagesTx(ctx context.Context, tx pgx.Tx, inputs []OutboundMessageInput) ([]*Message, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make([]*Message, 0, len(inputs))
	now := time.Now()
	for i := range inputs {
		in := &inputs[i]
		if in.AgentID == "" {
			return nil, fmt.Errorf("batch item %d: empty agent_id", i)
		}
		id := "msg_" + generateID()
		var recipient string
		if len(in.ToRecipients) > 0 {
			recipient = in.ToRecipients[0]
		}
		m := &Message{
			ID:                id,
			AgentID:           in.AgentID,
			Direction:         "outbound",
			Recipient:         recipient,
			Subject:           in.Subject,
			Type:              in.MsgType,
			Method:            in.Method,
			ProviderMessageID: in.ProviderMessageID,
			ConversationID:    in.ConversationID,
			CreatedAt:         now,
			ExpiresAt:         now.Add(MessageTTL),
			ToRecipients:      in.ToRecipients,
			CC:                in.CC,
			BCC:               in.BCC,
			RawMessage:        in.RawMessage,
			Sender:            in.AgentID,
			DeliveryStatus:    in.DeliveryStatus,
			SentAs:            in.SentAs,
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO messages (id, agent_id, direction, recipient, subject, message_type, method, provider_message_id, conversation_id, created_at, expires_at, to_recipients, cc, bcc, status, sender, raw_message, delivery_status, sent_as, envelope_from, batch_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)`,
			m.ID, m.AgentID, m.Direction, m.Recipient, m.Subject, m.Type, m.Method, m.ProviderMessageID, m.ConversationID, m.CreatedAt, m.ExpiresAt, m.ToRecipients, m.CC, m.BCC, MessageStatusSent, m.Sender, nullIfEmptyBytes(m.RawMessage), nullIfEmpty(in.DeliveryStatus), nullIfEmpty(in.SentAs), nullIfEmpty(in.EnvelopeFrom), nullIfEmpty(in.BatchID),
		)
		if err != nil {
			return nil, fmt.Errorf("batch item %d insert: %w", i, err)
		}
		m.Status = MessageStatusSent
		out = append(out, m)
	}
	return out, nil
}
