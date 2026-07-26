package identity

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrEngagementNotFound means this agent has no engagement with that address.
// Deliberately the same error for "absent", "another agent's", and "another
// account's", so the surface cannot be used to probe.
var ErrEngagementNotFound = errors.New("engagement not found")

// ContactEngagement is one agent's relationship with one contact: what stage
// the outreach is at, when to act next, and what has actually happened on the
// wire.
//
// The agent owns Stage, NextActionAt, and Metadata — e2a never interprets them.
// Everything else is derived by e2a from real message activity and is read-only
// to clients; a caller cannot fake a reply.
type ContactEngagement struct {
	ID         string `json:"id"`
	AgentEmail string `json:"agent_email"`
	Address    string `json:"address"`
	ContactID  string `json:"contact_id"`

	Stage        string         `json:"stage"`
	NextActionAt *time.Time     `json:"next_action_at"`
	Metadata     map[string]any `json:"metadata"`

	FirstOutboundAt    *time.Time `json:"first_outbound_at"`
	LastOutboundAt     *time.Time `json:"last_outbound_at"`
	LastInboundAt      *time.Time `json:"last_inbound_at"`
	OutboundCount      int        `json:"outbound_count"`
	InboundCount       int        `json:"inbound_count"`
	LastConversationID string     `json:"last_conversation_id"`

	// Suppressed and its reason are joined from the suppression tables rather
	// than stored here, so consent has exactly one home and this view can never
	// disagree with what the send path enforces.
	Suppressed        bool   `json:"suppressed"`
	SuppressionSource string `json:"suppression_source,omitempty"`
	SuppressionReason string `json:"suppression_reason,omitempty"`

	// Contact identity, embedded so an agent-scoped caller can compose without
	// being granted account-wide contact reads.
	DisplayName     string         `json:"display_name"`
	ContactMetadata map[string]any `json:"contact_metadata"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Replied reports whether the contact has answered since outreach began.
//
// Computed rather than stored so it cannot drift from the timestamps it derives
// from. The comparison against first_outbound_at (not merely "any inbound") is
// what makes it mean "they replied to us" rather than "they have ever written".
func (e ContactEngagement) Replied() bool {
	return e.LastInboundAt != nil && e.FirstOutboundAt != nil &&
		e.LastInboundAt.After(*e.FirstOutboundAt)
}

// EngagementFilter narrows an outreach listing. The zero value matches
// everything. Replied and Suppressed are tri-state: nil means "don't filter".
type EngagementFilter struct {
	Stage              string
	Replied            *bool
	Suppressed         *bool
	NextActionBefore   time.Time
	LastOutboundBefore time.Time
}

// NewEngagementID mints an engagement identifier.
func NewEngagementID() string { return "eng_" + generateID() }

// engagementColumns is the single select list every engagement read uses, so a
// schema change cannot leave one query scanning a stale column set. The
// suppression join is a LEFT JOIN over both scopes: an account-wide block and
// an agent-scoped one both mean "cannot send", and the agent-scoped row wins
// when reporting the reason because it is the more specific fact.
const engagementColumns = `
	ce.id, ce.agent_id, ce.address, ce.contact_id,
	ce.stage, ce.next_action_at, ce.metadata,
	ce.first_outbound_at, ce.last_outbound_at, ce.last_inbound_at,
	-- Counts are computed here rather than stored (design §10): they are
	-- projected, never filtered, so this runs over the page the query already
	-- narrowed to. Storing them would reintroduce the only non-idempotent
	-- writes in the feature, and with them the drift a reconciliation sweep
	-- existed to chase. Mirrors the conversation-summary query.
	(SELECT count(*) FROM messages m
	  WHERE m.agent_id = ce.agent_id
	    AND m.direction = 'outbound'
	    AND m.deleted_at IS NULL
	    AND EXISTS (SELECT 1 FROM unnest(m.to_recipients) AS r
	                 WHERE lower(r) = ce.address)) AS outbound_count,
	(SELECT count(*) FROM messages m
	  WHERE m.agent_id = ce.agent_id
	    AND m.direction = 'inbound'
	    AND m.deleted_at IS NULL
	    AND lower(m.sender) = ce.address) AS inbound_count,
	ce.last_conversation_id,
	(asup.address IS NOT NULL OR sup.address IS NOT NULL) AS suppressed,
	COALESCE(asup.source, CASE WHEN sup.address IS NOT NULL THEN sup.source ELSE '' END) AS suppression_source,
	COALESCE(asup.reason, CASE WHEN sup.address IS NOT NULL THEN sup.reason ELSE '' END) AS suppression_reason,
	c.display_name, c.metadata,
	ce.created_at, ce.updated_at`

const engagementFrom = `
	FROM contact_engagements ce
	JOIN contacts c ON c.id = ce.contact_id
	LEFT JOIN agent_suppressions asup
	       ON asup.user_id = ce.user_id AND asup.agent_id = ce.agent_id AND asup.address = ce.address
	LEFT JOIN suppressions sup
	       ON sup.user_id = ce.user_id AND sup.address = ce.address`

func scanEngagement(row pgx.Row) (ContactEngagement, error) {
	var e ContactEngagement
	var meta, contactMeta []byte
	if err := row.Scan(
		&e.ID, &e.AgentEmail, &e.Address, &e.ContactID,
		&e.Stage, &e.NextActionAt, &meta,
		&e.FirstOutboundAt, &e.LastOutboundAt, &e.LastInboundAt,
		&e.OutboundCount, &e.InboundCount, &e.LastConversationID,
		&e.Suppressed, &e.SuppressionSource, &e.SuppressionReason,
		&e.DisplayName, &contactMeta,
		&e.CreatedAt, &e.UpdatedAt,
	); err != nil {
		return ContactEngagement{}, err
	}
	e.Metadata = map[string]any{}
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &e.Metadata); err != nil {
			return ContactEngagement{}, err
		}
	}
	e.ContactMetadata = map[string]any{}
	if len(contactMeta) > 0 {
		if err := json.Unmarshal(contactMeta, &e.ContactMetadata); err != nil {
			return ContactEngagement{}, err
		}
	}
	return e, nil
}

// UpsertEngagement enrolls a contact with an agent, or updates the agent-owned
// fields of an existing enrollment.
//
// Nil pointers mean "leave unchanged", so advancing the stage does not disturb
// the next action and vice versa. The contact is created if absent (source
// 'manual'), because enrolling someone you have not imported yet is a normal
// first step rather than an error.
//
// It never touches the derived columns: those are e2a's account of what
// actually happened on the wire, and a client must not be able to fake a reply
// or a send.
func (s *Store) UpsertEngagement(ctx context.Context, userID, agentID, address string, stage *string, nextActionAt **time.Time, metadata map[string]any) (ContactEngagement, bool, error) {
	agentID = NormalizeEmail(agentID)
	address = NormalizeMailboxAddress(address)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ContactEngagement{}, false, err
	}
	defer tx.Rollback(ctx)

	var contactID string
	err = tx.QueryRow(ctx,
		`INSERT INTO contacts (id, user_id, address, display_name, metadata, source)
		      VALUES ($1, $2, $3, '', '{}'::jsonb, 'manual')
		 ON CONFLICT (user_id, address) DO UPDATE SET updated_at = contacts.updated_at
		  RETURNING id`,
		NewContactID(), userID, address).Scan(&contactID)
	if err != nil {
		return ContactEngagement{}, false, err
	}

	var encoded []byte
	if metadata != nil {
		if encoded, err = json.Marshal(metadata); err != nil {
			return ContactEngagement{}, false, err
		}
	}
	var stageVal *string = stage
	var nextVal *time.Time
	var nextSet bool
	if nextActionAt != nil {
		nextSet = true
		nextVal = *nextActionAt
	}

	var created bool
	err = tx.QueryRow(ctx,
		`INSERT INTO contact_engagements
		     (id, user_id, contact_id, agent_id, address, stage, next_action_at, metadata)
		  VALUES ($1, $2, $3, $4, $5, COALESCE($6, ''), $7, COALESCE($8::jsonb, '{}'::jsonb))
		  ON CONFLICT (user_id, agent_id, contact_id) DO UPDATE
		     SET stage          = COALESCE($6, contact_engagements.stage),
		         next_action_at = CASE WHEN $9 THEN $7 ELSE contact_engagements.next_action_at END,
		         metadata       = COALESCE($8::jsonb, contact_engagements.metadata),
		         updated_at     = now()
		  RETURNING (xmax = 0)`,
		NewEngagementID(), userID, contactID, agentID, address,
		stageVal, nextVal, encoded, nextSet).Scan(&created)
	if err != nil {
		return ContactEngagement{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ContactEngagement{}, false, err
	}

	e, err := s.GetEngagement(ctx, userID, agentID, address)
	return e, created, err
}

// GetEngagement loads one engagement scoped to (account, agent, address).
func (s *Store) GetEngagement(ctx context.Context, userID, agentID, address string) (ContactEngagement, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+engagementColumns+engagementFrom+`
		  WHERE ce.user_id = $1 AND ce.agent_id = $2 AND ce.address = $3`,
		userID, NormalizeEmail(agentID), NormalizeMailboxAddress(address))
	e, err := scanEngagement(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ContactEngagement{}, ErrEngagementNotFound
	}
	return e, err
}

// ListEngagements returns one keyset page of an agent's outreach, ordered
// (created_at DESC, id DESC).
//
// The filters exist to answer one question in a single round trip: "who has not
// replied, is due, and is still mailable?" Anything that cannot be expressed
// here forces the caller back into client-side aggregation over messages, which
// is the problem this resource was built to remove.
func (s *Store) ListEngagements(ctx context.Context, userID, agentID string, f EngagementFilter, limit int, afterCreatedAt time.Time, afterID string) ([]ContactEngagement, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+engagementColumns+engagementFrom+`
		  WHERE ce.user_id = $1
		    AND ce.agent_id = $2
		    AND ($3 = '' OR ce.stage = $3)
		    AND ($4::timestamptz IS NULL OR (ce.next_action_at IS NOT NULL AND ce.next_action_at <= $4))
		    AND ($5::timestamptz IS NULL OR (ce.last_outbound_at IS NOT NULL AND ce.last_outbound_at <= $5))
		    AND ($6::bool IS NULL OR
		         (ce.last_inbound_at IS NOT NULL
		          AND ce.first_outbound_at IS NOT NULL
		          AND ce.last_inbound_at > ce.first_outbound_at) = $6)
		    AND ($7::bool IS NULL OR
		         (asup.address IS NOT NULL OR sup.address IS NOT NULL) = $7)
		    AND ($8::timestamptz IS NULL OR (ce.created_at, ce.id) < ($8, $9))
		  ORDER BY ce.created_at DESC, ce.id DESC
		  LIMIT $10`,
		userID, NormalizeEmail(agentID), f.Stage,
		nullableTime(f.NextActionBefore), nullableTime(f.LastOutboundBefore),
		f.Replied, f.Suppressed,
		nullableTime(afterCreatedAt), afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactEngagement
	for rows.Next() {
		e, err := scanEngagement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteEngagement un-enrols a contact from an agent's outreach.
//
// It removes ONLY the engagement. The contact survives (other agents may still
// be working them, and identity is account-level), and suppressions survive
// unconditionally — un-enrolling is not consent, and must never make a blocked
// address mailable again.
func (s *Store) DeleteEngagement(ctx context.Context, userID, agentID, address string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM contact_engagements
		  WHERE user_id = $1 AND agent_id = $2 AND address = $3`,
		userID, NormalizeEmail(agentID), NormalizeMailboxAddress(address))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// PurgeEngagementsForAgent removes every engagement belonging to an agent.
//
// Called from the janitor's agent hard-delete sweep, NOT on trash: a trashed
// agent must get its outreach state back if it is restored. Suppressions are
// deliberately left alone — consent survives agent deletion and recreation,
// operational state does not. That asymmetry is the whole reason this function
// exists rather than a cascade.
func (s *Store) PurgeEngagementsForAgent(ctx context.Context, userID, agentID string) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM contact_engagements WHERE user_id = $1 AND agent_id = $2`,
		userID, NormalizeEmail(agentID))
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
