package identity

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Contact-layer sentinel errors. Handlers map these to their HTTP envelopes;
// callers must not string-match.
var (
	// ErrContactNotFound means no contact with that address exists for this
	// account. Deliberately the same error for "absent" and "owned by someone
	// else" so this surface cannot enumerate another tenant's contacts.
	ErrContactNotFound = errors.New("contact not found")
	// ErrContactExists is returned when creating an address the account
	// already holds. Callers that want upsert semantics use UpsertContact.
	ErrContactExists = errors.New("contact already exists")
)

// ContactSource records how a contact first entered the account. It is
// provenance, not lifecycle: it is set once at insert and never updated, so a
// contact imported from a list stays 'import' even after it replies.
const (
	ContactSourceImport  = "import"
	ContactSourceManual  = "manual"
	ContactSourceInbound = "inbound"
)

// Contact is one person this account corresponds with, identified by the
// canonical form of their address.
//
// Contacts are ACCOUNT-scoped on purpose. Per-agent outreach state (stage, next
// action, reply status) lives on contact_engagements instead, because the same
// person may be worked by several agents with independent consent and history —
// see docs/design/2026-07-24-contacts-and-outreach-state.md §3.1.
type Contact struct {
	ID            string         `json:"id"`
	Address       string         `json:"address"`
	DisplayName   string         `json:"display_name"`
	Metadata      map[string]any `json:"metadata"`
	Source        string         `json:"source"`
	ImportBatchID string         `json:"import_batch_id,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// ContactFilter narrows a contact listing. The zero value matches everything.
// Filters are ANDed. Time bounds are half-open: CreatedAfter is exclusive and
// CreatedBefore is exclusive, so paging by time cannot double-count a row.
type ContactFilter struct {
	Source        string
	ImportBatchID string
	CreatedAfter  time.Time
	CreatedBefore time.Time
}

// NewContactID mints a contact identifier.
func NewContactID() string { return "cnt_" + generateID() }

// contactColumns is the single select list every contact read uses, so a schema
// change cannot leave one query scanning a stale column set.
const contactColumns = `id, address, display_name, metadata, source,
	COALESCE(import_batch_id, ''), created_at, updated_at`

func scanContact(row pgx.Row) (Contact, error) {
	var c Contact
	var raw []byte
	if err := row.Scan(&c.ID, &c.Address, &c.DisplayName, &raw, &c.Source,
		&c.ImportBatchID, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Contact{}, err
	}
	// metadata is NOT NULL DEFAULT '{}', but decode defensively so a row
	// written by an older path can never panic a read.
	c.Metadata = map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c.Metadata); err != nil {
			return Contact{}, err
		}
	}
	return c, nil
}

// CreateContact inserts a new contact. address is canonicalized with
// NormalizeMailboxAddress — the same key suppression lookups use — so a
// display-name form ("Jane Doe <jane@fund.vc>") and the bare address collapse
// to one row. Returns ErrContactExists if the account already holds it.
func (s *Store) CreateContact(ctx context.Context, userID, address, displayName string, metadata map[string]any, source, importBatchID string) (Contact, error) {
	address = NormalizeMailboxAddress(address)
	if metadata == nil {
		metadata = map[string]any{}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return Contact{}, err
	}
	var batch *string
	if importBatchID != "" {
		batch = &importBatchID
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO contacts (id, user_id, address, display_name, metadata, source, import_batch_id)
		      VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (user_id, address) DO NOTHING
		   RETURNING `+contactColumns,
		NewContactID(), userID, address, displayName, encoded, source, batch)
	c, err := scanContact(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// DO NOTHING suppressed the insert: the row already exists.
		return Contact{}, ErrContactExists
	}
	if err != nil {
		return Contact{}, err
	}
	return c, nil
}

// GetContactByAddress fetches one contact scoped to the account.
func (s *Store) GetContactByAddress(ctx context.Context, userID, address string) (Contact, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+contactColumns+` FROM contacts WHERE user_id = $1 AND address = $2`,
		userID, NormalizeMailboxAddress(address))
	c, err := scanContact(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Contact{}, ErrContactNotFound
	}
	return c, err
}

// ListContacts returns one keyset page ordered (created_at DESC, id DESC).
// afterCreatedAt/afterID carry the previous page's last row; the zero value
// starts at the beginning. Callers request limit+1 to detect a further page.
func (s *Store) ListContacts(ctx context.Context, userID string, f ContactFilter, limit int, afterCreatedAt time.Time, afterID string) ([]Contact, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+contactColumns+`
		   FROM contacts
		  WHERE user_id = $1
		    AND ($2 = '' OR source = $2)
		    AND ($3 = '' OR import_batch_id = $3)
		    AND ($4::timestamptz IS NULL OR created_at > $4)
		    AND ($5::timestamptz IS NULL OR created_at < $5)
		    AND ($6::timestamptz IS NULL OR (created_at, id) < ($6, $7))
		  ORDER BY created_at DESC, id DESC
		  LIMIT $8`,
		userID, f.Source, f.ImportBatchID,
		nullableTime(f.CreatedAfter), nullableTime(f.CreatedBefore),
		nullableTime(afterCreatedAt), afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Contact
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateContact partially updates a contact. A nil pointer leaves that field
// untouched, which is what makes PATCH semantics safe: omitting metadata must
// not erase it.
//
// It deliberately cannot change address, source, or import_batch_id — address
// is the identity, and the other two are provenance.
func (s *Store) UpdateContact(ctx context.Context, userID, address string, displayName *string, metadata map[string]any) (Contact, error) {
	var encoded []byte
	if metadata != nil {
		var err error
		if encoded, err = json.Marshal(metadata); err != nil {
			return Contact{}, err
		}
	}
	row := s.pool.QueryRow(ctx,
		`UPDATE contacts
		    SET display_name = COALESCE($3, display_name),
		        metadata     = COALESCE($4::jsonb, metadata),
		        updated_at   = now()
		  WHERE user_id = $1 AND address = $2
		  RETURNING `+contactColumns,
		userID, NormalizeMailboxAddress(address), displayName, encoded)
	c, err := scanContact(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Contact{}, ErrContactNotFound
	}
	return c, err
}

// DeleteContact removes a contact, reporting whether a row was removed.
//
// It deliberately does NOT touch suppressions. Consent is keyed by address in
// suppressions/agent_suppressions and must outlive the contact record —
// deleting a contact must never make a previously-blocked address sendable
// again. Design §8.6 invariant 5.
func (s *Store) DeleteContact(ctx context.Context, userID, address string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM contacts WHERE user_id = $1 AND address = $2`,
		userID, NormalizeMailboxAddress(address))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CountContacts returns the account's contact total, for entitlement checks.
func (s *Store) CountContacts(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM contacts WHERE user_id = $1`, userID).Scan(&n)
	return n, err
}

// nullableTime renders a zero time.Time as SQL NULL so a filter left unset is
// skipped by the `IS NULL OR` guards above rather than matching year 1.
func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
