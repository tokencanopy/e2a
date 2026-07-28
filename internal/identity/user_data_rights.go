package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/eventpayload"
)

// UserExport is the structured dump returned by ExportUserData. It's
// designed to be a complete, machine-readable record of everything a
// single user owns in the system — the right-of-access counterpart to
// DeleteUserData below. Sensitive secrets are deliberately excluded:
//
//   - API key plaintexts are not stored anywhere (only hashes), so
//     they can't be exported even in principle.
//   - google_subject is an internal OAuth identifier with no value to
//     the user and is omitted on purpose.
//   - User session tokens are transient and excluded.
type UserExport struct {
	GeneratedAt      time.Time                    `json:"generated_at"`
	SchemaVersion    string                       `json:"schema_version" doc:"Version of the interior record shapes in this export. The export envelope (the top-level keys and schema_version) is stable; interior record shapes are versioned by schema_version and may evolve — branch on schema_version before interpreting interior records. The current server emits \"4\"; v4 messages expose canonical header_from, envelope_from, verified_domain, and authentication fields, and retain v3 suppression provenance through optional agent_email."`
	User             UserExportUser               `json:"user"`
	Domains          []Domain                     `json:"domains" nullable:"false"`
	Agents           []AgentIdentity              `json:"agents" nullable:"false"`
	APIKeys          []APIKeyExportEntry          `json:"api_keys" nullable:"false"`
	Messages         []Message                    `json:"messages" nullable:"false"`
	Suppressions     []SuppressionExportEntry     `json:"suppressions" nullable:"false"`
	ProtectionEvents []ProtectionEventExportEntry `json:"protection_events" nullable:"false"`
	UsageEvents      []UsageEventEntry            `json:"usage_events,omitempty" nullable:"false"`
	OAuthConnections []OAuthConnectionEntry       `json:"oauth_connections,omitempty" nullable:"false"`
} // @name UserExport

// SuppressionExportEntry is one suppressed recipient address the account owns.
// source_message_id is an internal correlation id, deliberately omitted.
type SuppressionExportEntry struct {
	AgentEmail string    `json:"agent_email,omitempty"`
	Address    string    `json:"address"`
	Reason     string    `json:"reason,omitempty"`
	Source     string    `json:"source"`
	CreatedAt  time.Time `json:"created_at"`
} // @name SuppressionExportEntry

// ProtectionEventExportEntry is one row of the protection_events audit log for
// the user's agents — a metadata projection. The provider-forensics `raw` blob
// and the `spans`/`categories` detector internals are omitted (internal, noisy,
// and not user-meaningful); the disposition (source/reason/action) and the
// counterparty address that tripped a gate are included.
type ProtectionEventExportEntry struct {
	ID          string    `json:"id"`
	MessageID   string    `json:"message_id"`
	AgentID     string    `json:"agent_email"`
	Direction   string    `json:"direction"`
	Source      string    `json:"source"`
	Reason      string    `json:"reason"`
	Action      string    `json:"action"`
	SubjectAddr string    `json:"peer_address,omitempty"`
	Detector    string    `json:"detector,omitempty"`
	Score       *float64  `json:"scan_score,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
} // @name ProtectionEventExportEntry

// OAuthConnectionEntry is one OAuth/MCP client connection. The
// underlying token signatures are intentionally excluded — they are
// credential-equivalent. The agent_email is the per-grant binding
// captured at consent time.
type OAuthConnectionEntry struct {
	ClientID   string     `json:"client_id"`
	ClientName string     `json:"client_name"`
	AgentEmail string     `json:"agent_email"`
	Scope      string     `json:"scope"`
	IssuedAt   time.Time  `json:"issued_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
} // @name OAuthConnectionEntry

// UserExportUser mirrors User but omits the google_subject internal
// identifier from the export payload.
type UserExportUser struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
} // @name UserExportUser

// APIKeyExportEntry is the API-key shape included in the export. We
// expose only metadata; the hash itself stays internal because (a) it's
// not useful to the user, (b) it's a credential equivalent for offline
// dictionary attacks if leaked.
type APIKeyExportEntry struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	KeyPrefix  string     `json:"key_prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
} // @name APIKeyExportEntry

// UsageEventEntry is one row of the usage_events table for the user.
type UsageEventEntry struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_email"`
	Domain    string    `json:"domain"`
	Direction string    `json:"direction"`
	EventType string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
} // @name UsageEventEntry

// ExportUserData gathers everything a user owns into a single struct
// for the right-of-access flow. Reads run inside a REPEATABLE READ
// transaction so the snapshot is internally consistent even if writes
// arrive while the export is being assembled.
func (s *Store) ExportUserData(ctx context.Context, userID string) (*UserExport, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("export: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Profile
	var u UserExportUser
	err = tx.QueryRow(ctx,
		`SELECT id, email, name, created_at FROM users WHERE id = $1`, userID,
	).Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("export: load user: %w", err)
	}

	// Domains
	domains, err := scanDomainsForUser(ctx, tx, userID)
	if err != nil {
		return nil, fmt.Errorf("export: load domains: %w", err)
	}

	// Agents
	agents, err := scanAgentsForUser(ctx, tx, userID)
	if err != nil {
		return nil, fmt.Errorf("export: load agents: %w", err)
	}

	// API keys (metadata only)
	keys, err := scanAPIKeysForUser(ctx, tx, userID)
	if err != nil {
		return nil, fmt.Errorf("export: load api keys: %w", err)
	}

	// Messages — across all the user's agents in one query so the order
	// is consistent and pagination doesn't matter for the export.
	messages, err := scanMessagesForUser(ctx, tx, userID)
	if err != nil {
		return nil, fmt.Errorf("export: load messages: %w", err)
	}

	// Suppressions (recipient addresses the account suppressed)
	suppressions, err := scanSuppressionsForUser(ctx, tx, userID)
	if err != nil {
		return nil, fmt.Errorf("export: load suppressions: %w", err)
	}

	// Protection events (the screening audit log across the user's agents)
	protectionEvents, err := scanProtectionEventsForUser(ctx, tx, userID)
	if err != nil {
		return nil, fmt.Errorf("export: load protection events: %w", err)
	}

	// Usage events (only present if E2A_USAGE_TRACKING is on)
	events, err := scanUsageEventsForUser(ctx, tx, userID)
	if err != nil {
		return nil, fmt.Errorf("export: load usage events: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("export: commit: %w", err)
	}

	return &UserExport{
		GeneratedAt:      time.Now().UTC(),
		SchemaVersion:    "4",
		User:             u,
		Domains:          domains,
		Agents:           agents,
		APIKeys:          keys,
		Messages:         messages,
		Suppressions:     suppressions,
		ProtectionEvents: protectionEvents,
		UsageEvents:      events,
	}, nil
}

// scanSuppressionsForUser loads the user's suppression list for the export.
func scanSuppressionsForUser(ctx context.Context, tx pgx.Tx, userID string) ([]SuppressionExportEntry, error) {
	rows, err := tx.Query(ctx,
		`SELECT agent_email, address, reason, source, created_at
		   FROM (
		         SELECT '' AS agent_email, address, COALESCE(reason, '') AS reason, source, created_at
		           FROM suppressions WHERE user_id = $1
		         UNION ALL
		         SELECT agent_id AS agent_email, address, reason, source, created_at
		           FROM agent_suppressions WHERE user_id = $1
		        ) all_suppressions
		  ORDER BY created_at DESC, address DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SuppressionExportEntry{}
	for rows.Next() {
		var e SuppressionExportEntry
		if err := rows.Scan(&e.AgentEmail, &e.Address, &e.Reason, &e.Source, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// scanProtectionEventsForUser loads the screening audit log across all the user's
// agents (metadata projection — see ProtectionEventExportEntry).
func scanProtectionEventsForUser(ctx context.Context, tx pgx.Tx, userID string) ([]ProtectionEventExportEntry, error) {
	rows, err := tx.Query(ctx,
		`SELECT pe.id, pe.message_id, pe.agent_id, pe.direction, pe.source, pe.reason,
		        pe.action, COALESCE(pe.subject_addr, ''), COALESCE(pe.detector, ''), pe.score, pe.created_at
		   FROM protection_events pe
		   JOIN agent_identities a ON a.id = pe.agent_id
		  WHERE a.user_id = $1
		  ORDER BY pe.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProtectionEventExportEntry{}
	for rows.Next() {
		var e ProtectionEventExportEntry
		if err := rows.Scan(&e.ID, &e.MessageID, &e.AgentID, &e.Direction, &e.Source, &e.Reason,
			&e.Action, &e.SubjectAddr, &e.Detector, &e.Score, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteUserDataResult breaks out per-table row counts for audit logs.
// Operators receiving a deletion request often have to attest to what
// was removed; returning structured counts beats parsing a log line.
//
// Deleted is the uniform delete-object marker (every /v1 delete returns 200 +
// {deleted:true, ...}); the /v1 handler sets it. It is always true on a
// response — a failed delete is an error envelope, never deleted:false.
type DeleteUserDataResult struct {
	Deleted                       bool  `json:"deleted" doc:"Always true — the account no longer exists. A failed delete is an error envelope, never deleted:false."`
	UsageEventsDeleted            int64 `json:"usage_events_deleted"`
	UsageSummariesDeleted         int64 `json:"usage_summaries_deleted"`
	MessagesDeleted               int64 `json:"messages_deleted"`
	AgentsDeleted                 int64 `json:"agents_deleted"`
	DomainsDeleted                int64 `json:"domains_deleted"`
	APIKeysDeleted                int64 `json:"api_keys_deleted"`
	SessionsDeleted               int64 `json:"sessions_deleted"`
	AgentSuppressionsDeleted      int64 `json:"agent_suppressions_deleted"`
	AgentUnsubscribeTokensDeleted int64 `json:"agent_unsubscribe_tokens_deleted"`
	OAuthAuthCodesDeleted         int64 `json:"oauth_auth_codes_deleted,omitempty"`
	OAuthAccessTokensDeleted      int64 `json:"oauth_access_tokens_deleted,omitempty"`
	OAuthRefreshTokensDeleted     int64 `json:"oauth_refresh_tokens_deleted,omitempty"`
	UserDeleted                   bool  `json:"user_deleted"`
} // @name DeleteUserDataResult

// DeleteUserData wipes everything tied to a user in a single transaction.
//
// Schema cascades cover most of it (user_sessions, domains, agent_identities,
// api_keys, usage_summaries all `ON DELETE CASCADE` from users; messages
// cascade through agent_identities; webhook_deliveries cascade through
// messages; agent_suppressions and agent_unsubscribe_tokens cascade directly
// from users). The one row that doesn't is usage_events: its FK is `ON
// DELETE SET NULL` so analytics survives, which we explicitly override
// here for full deletion.
//
// Per-table counts are returned to the caller so an operator can attest
// to what was removed in audit / compliance contexts.
func (s *Store) DeleteUserData(ctx context.Context, userID string) (*DeleteUserDataResult, error) {
	return s.DeleteUserDataTx(ctx, userID, nil)
}

// DeleteUserDataTx is DeleteUserData with an optional per-domain in-tx hook
// run for every domain the user owns, BEFORE the cascade deletes them. It is
// how sender-identity teardown is enqueued for an account delete (decision 4):
// each owned domain's SES deprovision job commits atomically with the user
// delete. A nil hook is a plain account delete (dev / no SES). The DB FK
// cascade still removes the domain rows; the hook only schedules the remote
// SES cleanup that the cascade cannot do.
func (s *Store) DeleteUserDataTx(ctx context.Context, userID string, perDomainInTx func(ctx context.Context, tx pgx.Tx, domain string) error) (*DeleteUserDataResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("delete: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	res := &DeleteUserDataResult{}

	// Count first so the result is informative even when CASCADE handles
	// the actual delete. We do a single SELECT per table — much cheaper
	// than counting after-the-fact.
	queries := []struct {
		dst   *int64
		query string
	}{
		{&res.SessionsDeleted, `SELECT count(*) FROM user_sessions WHERE user_id = $1`},
		{&res.DomainsDeleted, `SELECT count(*) FROM domains WHERE user_id = $1`},
		{&res.AgentsDeleted, `SELECT count(*) FROM agent_identities WHERE user_id = $1`},
		{&res.APIKeysDeleted, `SELECT count(*) FROM api_keys WHERE user_id = $1`},
		{&res.MessagesDeleted, `SELECT count(*) FROM messages m JOIN agent_identities a ON a.id = m.agent_id WHERE a.user_id = $1`},
		{&res.UsageEventsDeleted, `SELECT count(*) FROM usage_events WHERE user_id = $1`},
		{&res.UsageSummariesDeleted, `SELECT count(*) FROM usage_summaries WHERE user_id = $1`},
		{&res.AgentSuppressionsDeleted, `SELECT count(*) FROM agent_suppressions WHERE user_id = $1`},
		{&res.AgentUnsubscribeTokensDeleted, `SELECT count(*) FROM agent_unsubscribe_tokens WHERE user_id = $1`},
	}
	for _, q := range queries {
		if err := tx.QueryRow(ctx, q.query, userID).Scan(q.dst); err != nil {
			return nil, fmt.Errorf("delete: count: %w", err)
		}
	}

	// Lock agents before their messages to match the outbound worker's claim
	// order, then cancel every linked durable send in this same transaction.
	// A rollback restores both the jobs and the account data.
	agentRows, err := tx.Query(ctx,
		`SELECT id FROM agent_identities WHERE user_id = $1 ORDER BY id FOR UPDATE`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("delete: lock agents: %w", err)
	}
	for agentRows.Next() {
		var agentID string
		if err := agentRows.Scan(&agentID); err != nil {
			agentRows.Close()
			return nil, fmt.Errorf("delete: lock agents: %w", err)
		}
	}
	if err := agentRows.Err(); err != nil {
		agentRows.Close()
		return nil, fmt.Errorf("delete: lock agents: %w", err)
	}
	agentRows.Close()

	var sending bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1
			  FROM messages m
			  JOIN agent_identities a ON a.id = m.agent_id
			 WHERE a.user_id = $1
			   AND m.delivery_status = 'sending'
			   AND m.send_claimed_at > now() - make_interval(secs => $2)
		)`,
		userID, int64(OutboundSendClaimStaleWindow/time.Second),
	).Scan(&sending); err != nil {
		return nil, fmt.Errorf("delete: check active sends: %w", err)
	}
	if sending {
		return nil, ErrSendInProgress
	}

	jobRows, err := tx.Query(ctx,
		`SELECT m.send_job_id
		   FROM messages m
		   JOIN agent_identities a ON a.id = m.agent_id
		  WHERE a.user_id = $1
		    AND m.direction = 'outbound'
		    AND m.delivery_status IN ('accepted', 'sending')
		    AND m.send_job_id IS NOT NULL
		  FOR UPDATE OF m`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("delete: load outbound jobs: %w", err)
	}
	jobIDs, err := scanOutboundJobIDs(jobRows)
	if err != nil {
		return nil, fmt.Errorf("delete: load outbound jobs: %w", err)
	}
	if err := s.cancelOutboundJobIDsTx(ctx, tx, jobIDs); err != nil {
		return nil, fmt.Errorf("delete: cancel outbound jobs: %w", err)
	}

	// Override the SET NULL behavior on usage_events for a complete wipe.
	if _, err := tx.Exec(ctx, `DELETE FROM usage_events WHERE user_id = $1`, userID); err != nil {
		return nil, fmt.Errorf("delete: usage_events: %w", err)
	}

	// Sender-identity teardown: enqueue an SES deprovision job for every
	// owned domain in this same tx, before the cascade removes the domain
	// rows. The orphan reaper is the backstop, but doing it transactionally
	// here means a normal account delete never leaves a dangling SES identity.
	if perDomainInTx != nil {
		domains, derr := scanDomainsForUser(ctx, tx, userID)
		if derr != nil {
			return nil, fmt.Errorf("delete: load domains for teardown: %w", derr)
		}
		for _, d := range domains {
			if err := perDomainInTx(ctx, tx, d.Domain); err != nil {
				return nil, fmt.Errorf("delete: enqueue sender teardown for %s: %w", d.Domain, err)
			}
		}
	}

	// Cascade does the rest: user_sessions, domains, agent_identities
	// (and through them, messages → webhook_deliveries), api_keys,
	// usage_summaries, agent_suppressions, and agent_unsubscribe_tokens.
	tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("delete: users: %w", err)
	}
	res.UserDeleted = tag.RowsAffected() == 1

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("delete: commit: %w", err)
	}
	return res, nil
}

// --- internal scan helpers; private since they're only used by the
//     export path. Each takes a pgx.Tx so the caller controls
//     isolation/lifecycle. ---

func scanDomainsForUser(ctx context.Context, tx pgx.Tx, userID string) ([]Domain, error) {
	rows, err := tx.Query(ctx,
		`SELECT domain, user_id, verified, verification_token, created_at, verified_at
		   FROM domains WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Domain
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.Domain, &d.UserID, &d.Verified, &d.VerificationToken, &d.CreatedAt, &d.VerifiedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanAgentsForUser(ctx context.Context, tx pgx.Tx, userID string) ([]AgentIdentity, error) {
	// webhook_status is computed here (same derivation as ListAgentsByUser)
	// so the export never emits the un-computed zero value: an export that
	// said "unhealthy" for agents whose status was simply never calculated
	// is exactly the bug the enum replaced webhook_healthy to fix.
	rows, err := tx.Query(ctx,
		`SELECT a.id, a.registered_domain, a.name,
		        a.hitl_ttl_seconds, a.hitl_expiration_action,
		        a.suppress_notifications,
		        a.public, a.created_at, a.user_id,
		        `+webhookStatusSQL+` AS webhook_status
		   FROM agent_identities a WHERE a.user_id = $1 ORDER BY a.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentIdentity
	for rows.Next() {
		var a AgentIdentity
		if err := rows.Scan(&a.ID, &a.RegisteredDomain, &a.Name,
			&a.HITLTTLSeconds, &a.HITLExpirationAction,
			&a.SuppressNotifications,
			&a.Public, &a.CreatedAt, &a.UserID, &a.WebhookStatus); err != nil {
			return nil, err
		}
		// Domain verification flag is on the joined domain row; for
		// the export we don't need it, leave default false.
		a.populateEmail()
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanAPIKeysForUser(ctx context.Context, tx pgx.Tx, userID string) ([]APIKeyExportEntry, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, name, key_prefix, created_at, last_used_at, revoked_at
		   FROM api_keys WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKeyExportEntry
	for rows.Next() {
		var k APIKeyExportEntry
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func scanMessagesForUser(ctx context.Context, tx pgx.Tx, userID string) ([]Message, error) {
	// COALESCE the nullable string columns into '' so we can scan into
	// plain `string` rather than dragging in sql.NullString. Matches the
	// ListActivityByAgent pattern used elsewhere in this package.
	rows, err := tx.Query(ctx,
		`SELECT m.id, m.agent_id, m.direction, m.sender, m.recipient,
		        m.subject, m.email_message_id, m.provider_message_id,
		        COALESCE(m.method, ''), COALESCE(m.message_type, ''),
		        m.raw_message, m.auth_headers, COALESCE(m.header_from, ''), COALESCE(m.envelope_from, ''), m.authentication, m.auth_verdict, m.conversation_id,
		        COALESCE(m.inbox_status, ''),
		        m.created_at, m.expires_at,
		        m.to_recipients, m.cc, m.bcc, m.reply_to,
		        m.status, m.approval_expires_at, m.reviewed_at,
		        COALESCE(m.rejection_reason, ''),
		        m.edited, COALESCE(m.body_text, ''), COALESCE(m.body_html, ''),
		        m.attachments_json
		   FROM messages m
		   JOIN agent_identities a ON a.id = m.agent_id
		  WHERE a.user_id = $1
		  ORDER BY m.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var authHeadersJSON []byte
		var authentication, authVerdict []byte
		var attachmentsJSON []byte
		if err := rows.Scan(
			&m.ID, &m.AgentID, &m.Direction, &m.Sender, &m.Recipient,
			&m.Subject, &m.EmailMessageID, &m.ProviderMessageID, &m.Method, &m.Type,
			&m.RawMessage, &authHeadersJSON, &m.HeaderFrom, &m.EnvelopeFrom, &authentication, &authVerdict, &m.ConversationID, &m.DeliveryStatus,
			&m.CreatedAt, &m.ExpiresAt,
			&m.ToRecipients, &m.CC, &m.BCC, &m.ReplyTo,
			&m.Status, &m.ApprovalExpiresAt, &m.ReviewedAt, &m.RejectionReason,
			&m.Edited, &m.BodyText, &m.BodyHTML, &attachmentsJSON,
		); err != nil {
			return nil, err
		}
		if len(authHeadersJSON) > 0 {
			_ = json.Unmarshal(authHeadersJSON, &m.AuthHeaders)
		}
		if err := unmarshalAuthVerdict(authVerdict, &m); err != nil {
			return nil, err
		}
		if err := unmarshalAuthentication(authentication, authVerdict, &m); err != nil {
			return nil, err
		}
		if m.Direction == "outbound" && m.HeaderFrom == "" {
			m.HeaderFrom = m.Sender
		}
		// size_bytes: the RAW MIME length of the stored message (same value
		// the message list/detail views carry, and the dominant term of
		// storage-quota accounting). Cheap to compute here; a held draft has
		// no raw_message yet, so the field is omitted (0/omitempty) there.
		m.SizeBytes = len(m.RawMessage)
		// attachments: ONE shape everywhere — the export emits the same
		// AttachmentMetaView metadata {filename, content_type, size_bytes
		// (DECODED), index} the live API uses, mapped at export time. For
		// sent/inbound messages it is parsed from raw_message (whose exported
		// bytes still contain the full attachment content); for held drafts
		// (pending_review, no raw_message yet) it is mapped from the internal
		// attachments_json blob ([]outbound.Attachment with base64 data),
		// whose inline base64 bytes are deliberately NOT exported. The blob
		// remains retained through terminal transitions.
		m.Attachments = exportAttachmentMeta(m.RawMessage, attachmentsJSON)
		out = append(out, m)
	}
	return out, rows.Err()
}

// exportAttachmentMeta maps a message's attachments to the wire AttachmentMetaView
// shape for the export. Precedence: raw MIME when present (the authoritative
// copy, same extraction as the live API), else the held-draft blob. A blob
// that fails to parse yields nil rather than failing the whole export.
func exportAttachmentMeta(raw, draftJSON []byte) []eventpayload.AttachmentMetaView {
	if len(raw) > 0 {
		return eventpayload.AttachmentMetadata(raw)
	}
	if len(draftJSON) == 0 {
		return nil
	}
	// The stored draft shape is []outbound.Attachment; decode structurally
	// here to keep identity free of an outbound dependency.
	var drafts []struct {
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Data        string `json:"data"`
	}
	if err := json.Unmarshal(draftJSON, &drafts); err != nil {
		return nil
	}
	out := make([]eventpayload.AttachmentMetaView, 0, len(drafts))
	for i, d := range drafts {
		out = append(out, eventpayload.AttachmentMetaView{
			Filename:    d.Filename,
			ContentType: d.ContentType,
			SizeBytes:   decodedBase64Len(d.Data),
			Index:       i,
		})
	}
	return out
}

// decodedBase64Len returns the DECODED byte length of a base64 string,
// tolerating the line-wrapping whitespace mail tooling inserts (mirroring
// validateAttachments on the accept path). Invalid base64 — which the accept
// path rejects, so it shouldn't occur — reads as 0 rather than erroring.
func decodedBase64Len(data string) int {
	clean := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, data)
	decoded, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return 0
	}
	return len(decoded)
}

func scanUsageEventsForUser(ctx context.Context, tx pgx.Tx, userID string) ([]UsageEventEntry, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, agent_id, domain, direction, event_type, created_at
		   FROM usage_events WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageEventEntry
	for rows.Next() {
		var e UsageEventEntry
		if err := rows.Scan(&e.ID, &e.AgentID, &e.Domain, &e.Direction, &e.EventType, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
