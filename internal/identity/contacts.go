package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// ErrImportBatchNotFound means no contacts remain that were created by
	// that batch — absent, already reversed, or another account's.
	ErrImportBatchNotFound = errors.New("import batch not found")
	// ErrContactLimitReached means the account is at its contact cap.
	ErrContactLimitReached = errors.New("contact limit reached")
)

// DefaultMaxContacts is the per-account contact cap when account_limits has no
// row or no explicit value. See migration 081 for why this number.
const DefaultMaxContacts = 10000

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
	// The cap is enforced INSIDE the insert rather than as a count-then-insert:
	// two concurrent creates at the boundary would both pass a prior check and
	// both land. The subquery counts within the same statement's snapshot.
	max, err := s.MaxContactsForUser(ctx, userID)
	if err != nil {
		return Contact{}, err
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO contacts (id, user_id, address, display_name, metadata, source, import_batch_id)
		 SELECT $1, $2, $3, $4, $5, $6, $7
		  WHERE (SELECT count(*) FROM contacts WHERE user_id = $2) < $8
		 ON CONFLICT (user_id, address) DO NOTHING
		   RETURNING `+contactColumns,
		NewContactID(), userID, address, displayName, encoded, source, batch, max)
	c, err := scanContact(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// No row can mean either "already exists" (ON CONFLICT DO NOTHING) or
		// "at the cap" (the WHERE filtered the insert out). They are different
		// errors to the caller, so disambiguate rather than guess.
		var exists bool
		if qerr := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM contacts WHERE user_id = $1 AND address = $2)`,
			userID, address).Scan(&exists); qerr != nil {
			return Contact{}, qerr
		}
		if exists {
			return Contact{}, ErrContactExists
		}
		return Contact{}, ErrContactLimitReached
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

// MaxContactsForUser resolves the account's contact cap, falling back to the
// default when the account has no limits row or no explicit value.
func (s *Store) MaxContactsForUser(ctx context.Context, userID string) (int, error) {
	var n *int
	err := s.pool.QueryRow(ctx,
		`SELECT max_contacts FROM account_limits WHERE user_id = $1`, userID,
	).Scan(&n)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DefaultMaxContacts, nil
		}
		return 0, err
	}
	if n == nil {
		return DefaultMaxContacts, nil
	}
	return *n, nil
}

// nullableTime renders a zero time.Time as SQL NULL so a filter left unset is
// skipped by the `IS NULL OR` guards above rather than matching year 1.
func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ContactImportRow is one inbound row of a bulk import, before validation.
type ContactImportRow struct {
	Address string
	// Nil means the upload did not carry a name column for this row, so an
	// existing name is left alone. A non-nil empty string explicitly clears it.
	// Same rule as Metadata below, and as PATCH on the contact resource.
	DisplayName *string
	Metadata    map[string]any
}

// ContactImportOutcome is the per-row result of a bulk import. Every submitted
// row gets exactly one outcome, so a caller can reconcile its spreadsheet
// line-by-line rather than diffing counts.
type ContactImportOutcome struct {
	Index   int
	Address string
	Status  string // created | updated | skipped | failed
	Code    string
	Message string
}

// Import outcome statuses.
const (
	ImportStatusCreated = "created"
	ImportStatusUpdated = "updated"
	ImportStatusSkipped = "skipped"
	ImportStatusFailed  = "failed"
)

// NewImportBatchID mints an import batch identifier.
func NewImportBatchID() string { return "imp_" + generateID() }

// ImportContacts applies one batch in a single transaction.
//
// Rows are expected to be pre-validated by the caller (address canonicalized,
// metadata bounded); rows the caller already rejected are passed with a
// non-empty Code and are recorded as failures without touching the database.
//
// merge=true refreshes display_name and metadata on an existing contact but
// deliberately leaves source and import_batch_id alone: re-importing a
// corrected spreadsheet must not rewrite where a contact originally came from,
// and must not disturb any state hanging off it. merge=false skips existing
// rows entirely.
func (s *Store) ImportContacts(ctx context.Context, userID, batchID string, rows []ContactImportRow, merge bool) ([]ContactImportOutcome, error) {
	outcomes := make([]ContactImportOutcome, len(rows))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Cap headroom for this batch. Import stays partial-success: a 1000-row
	// upload against 400 free slots fills those 400 and fails the rest
	// individually, rather than rejecting everything. Rejecting the batch would
	// contradict the per-row model the endpoint commits to, and would leave the
	// caller unable to see which lines they could keep.
	max, err := s.MaxContactsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	var existing int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM contacts WHERE user_id = $1`, userID).Scan(&existing); err != nil {
		return nil, err
	}
	headroom := max - existing
	if headroom < 0 {
		headroom = 0
	}

	// Collapse duplicates WITHIN the batch before touching the database, so a
	// spreadsheet listing the same person twice reports the second occurrence
	// explicitly instead of racing itself inside one transaction.
	seen := map[string]int{}

	for i, row := range rows {
		address := NormalizeMailboxAddress(row.Address)
		if first, dup := seen[address]; dup {
			outcomes[i] = ContactImportOutcome{
				Index: i, Address: address, Status: ImportStatusSkipped,
				Code:    "duplicate_in_batch",
				Message: fmt.Sprintf("duplicate of row %d in this batch", first),
			}
			continue
		}
		seen[address] = i

		// A row that OMITS metadata leaves the existing value alone; a row that
		// supplies one (even an empty object) replaces it. This mirrors PATCH
		// on the same resource, and matters because a corrected spreadsheet is
		// often narrower than the original — re-uploading it must not silently
		// erase the columns it no longer carries.
		var encoded []byte
		if row.Metadata != nil {
			var err error
			if encoded, err = json.Marshal(row.Metadata); err != nil {
				outcomes[i] = ContactImportOutcome{
					Index: i, Address: address, Status: ImportStatusFailed,
					Code: "invalid_request", Message: "metadata is not serializable",
				}
				continue
			}
		}
		// On INSERT an absent metadata still needs a concrete value.
		insertMetadata := encoded
		if insertMetadata == nil {
			insertMetadata = []byte("{}")
		}
		insertName := ""
		if row.DisplayName != nil {
			insertName = *row.DisplayName
		}

		// An UPDATE to an existing contact consumes no headroom; only a new row
		// does. Checking existence first keeps a re-import of an already-full
		// account working, which is the common corrective case.
		if headroom <= 0 {
			var already bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM contacts WHERE user_id = $1 AND address = $2)`,
				userID, address).Scan(&already); err != nil {
				return nil, err
			}
			if !already {
				outcomes[i] = ContactImportOutcome{
					Index: i, Address: address, Status: ImportStatusFailed,
					Code:    "contact_limit_reached",
					Message: fmt.Sprintf("account is at its contact limit of %d", max),
				}
				continue
			}
		}

		var stored string
		var inserted bool
		if merge {
			err = tx.QueryRow(ctx,
				`INSERT INTO contacts (id, user_id, address, display_name, metadata, source, import_batch_id)
				      VALUES ($1, $2, $3, $4, $5, 'import', $6)
				 ON CONFLICT (user_id, address) DO UPDATE
				    SET display_name = COALESCE($8, contacts.display_name),
				        metadata     = COALESCE($7::jsonb, contacts.metadata),
				        updated_at   = now()
				  RETURNING address, (xmax = 0)`,
				NewContactID(), userID, address, insertName, insertMetadata, batchID, encoded, row.DisplayName).
				Scan(&stored, &inserted)
		} else {
			err = tx.QueryRow(ctx,
				`INSERT INTO contacts (id, user_id, address, display_name, metadata, source, import_batch_id)
				      VALUES ($1, $2, $3, $4, $5, 'import', $6)
				 ON CONFLICT (user_id, address) DO NOTHING
				  RETURNING address, true`,
				NewContactID(), userID, address, insertName, insertMetadata, batchID).
				Scan(&stored, &inserted)
			if errors.Is(err, pgx.ErrNoRows) {
				outcomes[i] = ContactImportOutcome{
					Index: i, Address: address, Status: ImportStatusSkipped,
					Code: "already_exists", Message: "contact already exists and on_conflict is skip",
				}
				continue
			}
		}
		if err != nil {
			return nil, err
		}

		status := ImportStatusUpdated
		if inserted {
			status = ImportStatusCreated
			headroom--
		}
		outcomes[i] = ContactImportOutcome{Index: i, Address: stored, Status: status}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return outcomes, nil
}

// DeleteImportBatch reverses an import, returning how many contacts it removed
// and how many it deliberately kept.
//
// It removes only contacts that batch CREATED (import_batch_id still points at
// it). A contact whose provenance moved, or that a later merge re-pointed, is
// retained — reversing an upload must not delete a record the user has since
// built history on.
func (s *Store) DeleteImportBatch(ctx context.Context, userID, batchID string) (deleted int, retained int, err error) {
	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM contacts WHERE user_id = $1 AND import_batch_id = $2`,
		userID, batchID).Scan(&total); err != nil {
		return 0, 0, err
	}
	if total == 0 {
		return 0, 0, ErrImportBatchNotFound
	}
	// Contacts this account has actually corresponded with are RETAINED, not
	// deleted. Reversing an upload is an undo for a mistaken import, not a
	// licence to destroy a record someone has since built history on — and
	// because the reversal is addressed by batch id, the caller cannot tell
	// which rows those are. The counts in the receipt are how they find out.
	//
	// History is checked against messages rather than any engagement table so
	// this holds regardless of whether outreach state exists: a contact is
	// "corresponded with" if the account has sent to that address or received
	// from it.
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM contacts c
		  WHERE c.user_id = $1
		    AND c.import_batch_id = $2
		    AND NOT EXISTS (
		          SELECT 1
		            FROM messages m
		            JOIN agent_identities a ON a.id = m.agent_id AND a.user_id = c.user_id
		           WHERE lower(m.sender) = c.address
		              OR EXISTS (
		                   SELECT 1 FROM unnest(m.to_recipients) AS r
		                    WHERE lower(r) = c.address)
		    )`,
		userID, batchID)
	if err != nil {
		return 0, 0, err
	}
	deleted = int(tag.RowsAffected())
	return deleted, total - deleted, nil
}
