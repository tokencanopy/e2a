package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/identity"
)

// Contacts are account-level identity for the people this account corresponds
// with. Per-agent outreach state is a separate resource; nothing here is
// agent-bound. See docs/design/2026-07-24-contacts-and-outreach-state.md.
const contactBetaDescription = "Beta: the contacts surface may change before it is declared stable."

// Metadata bounds (design §7 Q2). metadata is opaque to e2a but it is still
// unbounded client input landing in JSONB, so it is bounded on every axis a
// caller controls. Deliberately generous — raising a cap later is additive,
// lowering a shipped one is breaking.
const (
	maxContactMetadataBytes     = 16 * 1024
	maxContactMetadataKeys      = 50
	maxContactMetadataKeyBytes  = 128
	maxContactMetadataValueSize = 4 * 1024
)

// ContactView is one person this account corresponds with.
type ContactView struct {
	Address     string         `json:"address" doc:"Canonical (normalized) email address. This is the resource key: a display-name form such as \"A. Partner <partner@fund.vc>\" and the bare address resolve to the same contact."`
	DisplayName string         `json:"display_name" doc:"Human-readable name. May be empty."`
	Metadata    map[string]any `json:"metadata" doc:"Caller-owned key/value data stored on the contact, returned verbatim. e2a never interprets it. An empty object when none is set. Flat objects only; the write-side bounds are published on CreateContactRequest.metadata."`
	Source      string         `json:"source" doc:"How this contact first entered the account — provenance, not lifecycle; it never changes after creation. Open set; tolerate unknown values. Known values: import, manual, inbound."`
	ImportBatch string         `json:"import_batch_id,omitempty" doc:"The import that created this contact, when source is import. Absent otherwise."`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

func contactView(c identity.Contact) ContactView {
	metadata := c.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	return ContactView{
		Address:     c.Address,
		DisplayName: c.DisplayName,
		Metadata:    metadata,
		Source:      c.Source,
		ImportBatch: c.ImportBatchID,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
}

// contactETag derives a strong validator from the fields a PATCH can change
// plus the update timestamp. Any accepted write moves updated_at, so a stale
// validator cannot match — which is what makes If-Match a real lost-update
// guard rather than decoration.
func contactETag(c identity.Contact) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%d", c.Address, c.DisplayName, c.UpdatedAt.UnixNano())
	encoded, _ := json.Marshal(c.Metadata)
	h.Write(encoded)
	return `"` + hex.EncodeToString(h.Sum(nil)[:16]) + `"`
}

// contactsCursor pins the filter set alongside the keyset position. A
// continuation whose filters changed would silently interleave rows from two
// different queries, so the cursor carries a fingerprint and the handler
// rejects a mismatch.
type contactsCursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
	Filters   string    `json:"f"`
}

func contactFilterFingerprint(f identity.ContactFilter) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d", f.Source, f.ImportBatchID,
		f.CreatedAfter.UnixNano(), f.CreatedBefore.UnixNano())
	return hex.EncodeToString(h.Sum(nil)[:8])
}

type listContactsInput struct {
	Source        string    `query:"source" doc:"Filter by provenance. Known values: import, manual, inbound."`
	ImportBatchID string    `query:"import_batch_id" doc:"Filter to the contacts created by one import."`
	CreatedAfter  time.Time `query:"created_after" doc:"Only contacts created strictly after this instant (RFC 3339)."`
	CreatedBefore time.Time `query:"created_before" doc:"Only contacts created strictly before this instant (RFC 3339)."`
	PageParams
}

type listContactsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         Page[ContactView]
}

// CreateContactRequest is the create body. Unknown fields are rejected (Huma
// registers request structs strict), so a camelCase typo is a loud 422 rather
// than a silently dropped field.
type CreateContactRequest struct {
	Address     string         `json:"address" required:"true" maxLength:"320" doc:"Email address. Accepts a bare address or an RFC 5322 mailbox (\"A. Partner <partner@fund.vc>\"); it is stored canonicalized. At most 320 Unicode code points."`
	DisplayName string         `json:"display_name,omitempty" maxLength:"320" doc:"Optional human-readable name."`
	Metadata    map[string]any `json:"metadata,omitempty" doc:"Optional flat key/value data owned by the caller and stored verbatim; e2a never interprets it."`
}

type createContactInput struct {
	IdempotencyKey string `header:"Idempotency-Key" doc:"Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first response instead of creating a second contact. Within the dedup window: same key + different body → 422 idempotency_key_reuse; same key while the first request is still executing → 409 idempotency_in_flight."`
	Body           CreateContactRequest
	RawBody        []byte
}

type createContactOutput struct {
	Location string `header:"Location"`
	Body     ContactView
}

type getContactInput struct {
	Address string `path:"address" doc:"The contact's email address, URL-encoded."`
}

type getContactOutput struct {
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         ContactView
}

// UpdateContactRequest is the PATCH body. Pointer/nil-able fields distinguish
// "omitted" from "set to empty": omitting metadata must leave it alone, which
// is what stops a display-name edit from erasing what an import wrote.
//
// address, source, and import_batch_id are absent by design — address is the
// identity and the other two are provenance. Because the struct is strict,
// sending them is a 422 rather than a silent no-op.
type UpdateContactRequest struct {
	DisplayName *string        `json:"display_name,omitempty" maxLength:"320" doc:"Replace the display name. Omit to leave unchanged."`
	Metadata    map[string]any `json:"metadata,omitempty" doc:"Replace the metadata object wholesale. Omit to leave unchanged; send an empty object to clear it."`
}

type updateContactInput struct {
	Address string `path:"address"`
	IfMatch string `header:"If-Match" doc:"Optional ETag from a prior read. When present it must still match at the instant of the write or the update is rejected with 412. Send the ETag exactly as returned; a W/-prefixed weak form of the same validator is also accepted, because a transforming CDN may weaken it in transit. * matches any existing representation. Sending the header with an empty value is a 400 invalid_request, not an unconditional write — omit the header entirely to write unconditionally."`
	Body    UpdateContactRequest
}

type updateContactOutput struct {
	ETag string `header:"ETag"`
	Body ContactView
}

type deleteContactInput struct {
	Address string `path:"address"`
	DeleteConfirm
}

// DeleteContactResult is the deletion receipt.
type DeleteContactResult struct {
	Deleted bool   `json:"deleted" doc:"Always true; the operation is a no-op-safe confirmation."`
	Address string `json:"address" doc:"The canonical address that was removed."`
}

type deleteContactOutput struct {
	Body DeleteContactResult
}

func (s *Server) registerContacts() {
	registerOp(s.API, huma.Operation{
		OperationID: "listContacts", Method: http.MethodGet, Path: "/v1/contacts",
		Summary: "List contacts (beta)", Tags: []string{"contacts"},
		Description: "Lists the people this account corresponds with, newest first. Account-scoped credentials only. " + contactBetaDescription,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleListContacts)

	registerOp(s.API, huma.Operation{
		OperationID: "createContact", Method: http.MethodPost, Path: "/v1/contacts",
		Summary: "Create a contact (beta)", Tags: []string{"contacts"},
		Description:   "Creates one contact. The address is canonicalized before storage, so a display-name form and the bare address are the same contact — a second create returns 409. Account-scoped credentials only. " + contactBetaDescription,
		Security:      []map[string][]string{{"bearer": {}}},
		DefaultStatus: http.StatusCreated,
		Extensions:    beta(),
	}, s.handleCreateContact)

	registerOp(s.API, huma.Operation{
		OperationID: "getContact", Method: http.MethodGet, Path: "/v1/contacts/{address}",
		Summary: "Get a contact (beta)", Tags: []string{"contacts"},
		Description: "Fetches one contact by address. Returns an ETag for use with If-Match on a subsequent update. Account-scoped credentials only. " + contactBetaDescription,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleGetContact)

	registerOp(s.API, huma.Operation{
		OperationID: "updateContact", Method: http.MethodPatch, Path: "/v1/contacts/{address}",
		Summary: "Update a contact (beta)", Tags: []string{"contacts"},
		Description: "Partially updates a contact. Omitted fields are left unchanged. Address and provenance are immutable. Account-scoped credentials only. " + contactBetaDescription,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleUpdateContact)

	registerOp(s.API, huma.Operation{
		OperationID: "deleteContact", Method: http.MethodDelete, Path: "/v1/contacts/{address}",
		Summary: "Delete a contact (beta)", Tags: []string{"contacts"},
		Description: "Removes a contact. Requires ?confirm=DELETE. Suppressions are NOT affected — consent outlives the contact record, so deleting a contact never makes a previously-blocked address sendable. Account-scoped credentials only. " + contactBetaDescription,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleDeleteContact)
}

// requireContactStore resolves the account principal and confirms the contacts
// capability is wired. Account scope is the ceiling for every contact
// operation: an agent-scoped credential reading account-wide contact identity
// would expose every person any sibling agent corresponds with.
func (s *Server) requireContactStore(ctx context.Context) (*identity.User, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.GetContact == nil || s.deps.ListContacts == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "contacts are not available on this deployment")
	}
	return user, nil
}

// validateContactAddress enforces that the value is a real, single mailbox and
// returns its canonical form — the same key the suppression lookup uses.
func validateContactAddress(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", NewError(http.StatusBadRequest, "invalid_recipient", "address is required")
	}
	parsed, err := mail.ParseAddress(trimmed)
	if err != nil || parsed.Address == "" {
		return "", NewError(http.StatusBadRequest, "invalid_recipient", "address must be a valid email address")
	}
	if !strings.Contains(parsed.Address, "@") {
		return "", NewError(http.StatusBadRequest, "invalid_recipient", "address must be a valid email address")
	}
	local, domain, _ := strings.Cut(parsed.Address, "@")
	if local == "" || domain == "" || !strings.Contains(domain, ".") {
		return "", NewError(http.StatusBadRequest, "invalid_recipient", "address must be a valid email address")
	}
	return identity.NormalizeMailboxAddress(trimmed), nil
}

// validateContactMetadata enforces the §7 Q2 bounds. Nested values are
// rejected rather than truncated: allowing nesting invites querying into it,
// which is the first step toward modeling a CRM inside an opaque blob.
func validateContactMetadata(metadata map[string]any) error {
	if metadata == nil {
		return nil
	}
	if len(metadata) > maxContactMetadataKeys {
		return NewError(http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("metadata has %d keys; at most %d are allowed", len(metadata), maxContactMetadataKeys))
	}
	for key, value := range metadata {
		if len(key) > maxContactMetadataKeyBytes {
			return NewError(http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("metadata key %q exceeds %d bytes", truncateForError(key), maxContactMetadataKeyBytes))
		}
		switch v := value.(type) {
		case nil, bool, float64, json.Number:
			// scalars are fine
		case string:
			if len(v) > maxContactMetadataValueSize {
				return NewError(http.StatusBadRequest, "invalid_request",
					fmt.Sprintf("metadata value for %q exceeds %d bytes", truncateForError(key), maxContactMetadataValueSize))
			}
		default:
			return NewError(http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("metadata value for %q must be a string, number, boolean, or null — nested objects and arrays are not supported", truncateForError(key)))
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return NewError(http.StatusBadRequest, "invalid_request", "metadata is not serializable")
	}
	if len(encoded) > maxContactMetadataBytes {
		return NewError(http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("metadata is %d bytes; at most %d are allowed", len(encoded), maxContactMetadataBytes))
	}
	return nil
}

func truncateForError(s string) string {
	if len(s) <= 32 {
		return s
	}
	return s[:32] + "…"
}

func (s *Server) handleListContacts(ctx context.Context, in *listContactsInput) (*listContactsOutput, error) {
	user, err := s.requireContactStore(ctx)
	if err != nil {
		return nil, err
	}
	filter := identity.ContactFilter{
		Source:        in.Source,
		ImportBatchID: in.ImportBatchID,
		CreatedAfter:  in.CreatedAfter,
		CreatedBefore: in.CreatedBefore,
	}
	if filter.Source != "" && filter.Source != identity.ContactSourceImport &&
		filter.Source != identity.ContactSourceManual && filter.Source != identity.ContactSourceInbound {
		return nil, NewError(http.StatusBadRequest, "invalid_filter",
			"source must be one of: import, manual, inbound")
	}

	fingerprint := contactFilterFingerprint(filter)
	var afterCreatedAt time.Time
	var afterID string
	if in.Cursor != "" {
		var cur contactsCursor
		if err := s.decodeCursor(user.ID, cursorContacts, in.Cursor, &cur); err != nil {
			return nil, err
		}
		if cur.Filters != fingerprint {
			// A cursor that decodes but was minted under different filters is
			// rejected too: honoring it would interleave two distinct queries.
			return nil, NewError(http.StatusBadRequest, "invalid_cursor", "invalid pagination cursor")
		}
		afterCreatedAt, afterID = cur.CreatedAt, cur.ID
	}

	limit := effectiveLimit(in.Limit)
	rows, err := s.deps.ListContacts(ctx, user.ID, filter, limit+1, afterCreatedAt, afterID)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to list contacts")
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	var nextCursor string
	if hasMore {
		last := rows[len(rows)-1]
		nextCursor, err = EncodeCursor(s.deps.CursorSecret, user.ID, cursorContacts, contactsCursor{
			CreatedAt: last.CreatedAt, ID: last.ID, Filters: fingerprint,
		})
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to build pagination cursor")
		}
	}
	items := make([]ContactView, len(rows))
	for i := range rows {
		items[i] = contactView(rows[i])
	}
	return &listContactsOutput{
		CacheControl: "no-store",
		Body:         NewPage(items, nextCursor),
	}, nil
}

func (s *Server) handleCreateContact(ctx context.Context, in *createContactInput) (*createContactOutput, error) {
	user, err := s.requireContactStore(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.CreateContact == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "contacts are not available on this deployment")
	}
	address, err := validateContactAddress(in.Body.Address)
	if err != nil {
		return nil, err
	}
	if err := validateContactMetadata(in.Body.Metadata); err != nil {
		return nil, err
	}

	// Keyed-idempotency guard, same machinery as api-keys and send: a
	// network-retried create with the same key and byte-identical body replays
	// the first response instead of racing the unique constraint and surfacing
	// a spurious 409 for a request the caller believes never landed.
	_, view, err := runIdempotent(s, ctx, user.ID, in.IdempotencyKey,
		"/v1/contacts", in.RawBody,
		func() (int, ContactView, error) {
			c, cerr := s.deps.CreateContact(ctx, user.ID, address, in.Body.DisplayName,
				in.Body.Metadata, identity.ContactSourceManual, "")
			if errors.Is(cerr, identity.ErrContactExists) {
				return 0, ContactView{}, NewError(http.StatusConflict, "conflict", "a contact with this address already exists")
			}
			if errors.Is(cerr, identity.ErrContactLimitReached) {
				return 0, ContactView{}, NewError(http.StatusBadRequest, "contact_limit_reached",
					"the account is at its contact limit")
			}
			if cerr != nil {
				return 0, ContactView{}, NewError(http.StatusInternalServerError, "internal_error", "failed to create contact")
			}
			return http.StatusCreated, contactView(c), nil
		})
	if err != nil {
		return nil, err
	}
	return &createContactOutput{
		Location: "/v1/contacts/" + url.PathEscape(view.Address),
		Body:     view,
	}, nil
}

func (s *Server) handleGetContact(ctx context.Context, in *getContactInput) (*getContactOutput, error) {
	user, err := s.requireContactStore(ctx)
	if err != nil {
		return nil, err
	}
	c, err := s.loadContact(ctx, user.ID, in.Address)
	if err != nil {
		return nil, err
	}
	return &getContactOutput{
		ETag:         contactETag(c),
		CacheControl: "no-store",
		Body:         contactView(c),
	}, nil
}

// loadContact resolves one contact for the calling account, conflating
// "absent" and "belongs to another account" into the same 404 so this surface
// cannot be used to probe another tenant's contact list.
func (s *Server) loadContact(ctx context.Context, userID, rawAddress string) (identity.Contact, error) {
	address, err := validateContactAddress(rawAddress)
	if err != nil {
		// An unparseable address in the path is simply not found, not a
		// validation error — it cannot name an existing resource.
		return identity.Contact{}, NewError(http.StatusNotFound, "contact_not_found", "contact not found")
	}
	c, err := s.deps.GetContact(ctx, userID, address)
	if errors.Is(err, identity.ErrContactNotFound) {
		return identity.Contact{}, NewError(http.StatusNotFound, "contact_not_found", "contact not found")
	}
	if err != nil {
		return identity.Contact{}, NewError(http.StatusInternalServerError, "internal_error", "failed to load contact")
	}
	return c, nil
}

func (s *Server) handleUpdateContact(ctx context.Context, in *updateContactInput) (*updateContactOutput, error) {
	user, err := s.requireContactStore(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.UpdateContact == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "contacts are not available on this deployment")
	}
	if env := emptyIfMatchError(ctx, in.IfMatch); env != nil {
		return nil, env
	}
	if err := validateContactMetadata(in.Body.Metadata); err != nil {
		return nil, err
	}

	current, err := s.loadContact(ctx, user.ID, in.Address)
	if err != nil {
		return nil, err
	}
	// Reject an already-stale read early for a useful response, then carry the
	// same version into the UPDATE predicate below. The second check is the
	// load-bearing one: it closes a write that lands between this read and the
	// database update.
	if in.IfMatch != "" && !etagMatches(in.IfMatch, contactETag(current)) {
		return nil, NewError(http.StatusPreconditionFailed, "precondition_failed",
			"the contact was modified by another request; re-read it and retry")
	}

	var updated identity.Contact
	if in.IfMatch != "" {
		if s.deps.UpdateContactIfUnchanged == nil {
			return nil, NewError(http.StatusNotImplemented, "not_implemented", "conditional contact updates are not available on this deployment")
		}
		updated, err = s.deps.UpdateContactIfUnchanged(ctx, user.ID, current.Address,
			in.Body.DisplayName, in.Body.Metadata, current.UpdatedAt)
	} else {
		updated, err = s.deps.UpdateContact(ctx, user.ID, current.Address,
			in.Body.DisplayName, in.Body.Metadata)
	}
	if errors.Is(err, identity.ErrContactNotFound) {
		return nil, NewError(http.StatusNotFound, "contact_not_found", "contact not found")
	}
	if errors.Is(err, identity.ErrContactPreconditionFailed) {
		return nil, NewError(http.StatusPreconditionFailed, "precondition_failed",
			"the contact was modified by another request; re-read it and retry")
	}
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to update contact")
	}
	return &updateContactOutput{ETag: contactETag(updated), Body: contactView(updated)}, nil
}

// emptyIfMatchError rejects an If-Match header that was sent with no value.
//
// RFC 9110 §13.1.1 defines the field value as `"*" / #entity-tag` with at least
// one member, so an empty field value is a malformed request rather than a
// precondition that failed — which is why this is 400 and not 412. It matters
// because the alternative reading is the worst one available: a header present
// but empty would otherwise silently degrade a write the caller believed was
// guarded into an unconditional one. A client that builds the header by
// interpolating a possibly-empty variable then overwrites a concurrent edit and
// receives a 200 telling it the guard held. (The quoted-empty form `""` is a
// syntactically valid validator and keeps its existing 412 behaviour.)
//
// Detecting this needs the raw request: Go collapses a present-but-empty header
// to the same "" an absent header produces, so presence is checked directly.
// When the raw request is unavailable the check does not fire — it can only ever
// add a rejection, never remove one.
func emptyIfMatchError(ctx context.Context, ifMatch string) *ErrorEnvelope {
	if ifMatch != "" {
		return nil
	}
	r := RequestFromContext(ctx)
	if r == nil {
		return nil
	}
	if _, present := r.Header[textproto.CanonicalMIMEHeaderKey("If-Match")]; !present {
		return nil
	}
	return NewError(http.StatusBadRequest, "invalid_request",
		"If-Match was sent with no value; supply the ETag from a prior read, or omit the header for an unconditional write").
		WithDetails(ValidationErrorDetails{Fields: []FieldError{{
			Location: "header.If-Match",
			Message:  "must not be empty",
		}}})
}

// etagMatches implements the subset of RFC 9110 §13.1.1 this surface needs:
// the wildcard, and a comma-separated list of candidate validators.
//
// THE W/ PREFIX IS TOLERATED ON INPUT ON PURPOSE. DO NOT "FIX" THIS TO A
// STRICT STRONG COMPARISON. §13.1.1 does specify strong comparison for
// If-Match, and reading only the RFC makes this look like a bug — it is not,
// and the deviation is load-bearing in production:
//
//   - api.e2a.dev sits behind a Cloudflare proxy, and that edge actively
//     transforms responses (br compression is confirmed live; our origin sets
//     no `encode` directive in either Caddyfile, so the compression is the
//     edge's own).
//   - Cloudflare downgrades a strong ETag to a weak one whenever it transforms
//     a response. "Respect Strong ETags" is an Enterprise-only setting and
//     e2a.dev is on the Free plan, so we cannot turn that off.
//   - So a client can legitimately GET `ETag: W/"abc"` for a row whose origin
//     validator is `"abc"`, echo exactly what it was given as If-Match, and —
//     under a strict comparison — receive a PERMANENT 412 that no retry ever
//     clears. Every conditional write on this surface would break, in prod
//     only: the staging host is DNS-only (unproxied), so the conformance gate
//     structurally cannot observe it and a green gate proves nothing here.
//
// Tolerating the prefix is what makes optimistic concurrency work through a
// transforming intermediary. The guard still holds: the compared body is the
// full strong validator, which changes on every accepted write, so a stale
// validator cannot match whether or not it arrived wearing a W/.
//
// The wildcard is deliberate and correct: `If-Match: *` means "if any current
// representation exists". Both call sites load the resource before consulting
// this function and answer 404/412 when it is absent, so `*` can only reach
// here for a resource that does exist — it never creates one.
func etagMatches(ifMatch, current string) bool {
	ifMatch = strings.TrimSpace(ifMatch)
	// RFC 9110 grammar is `"*" / 1#entity-tag` — the wildcard stands alone and
	// is not a list member, so it is matched here rather than inside the loop.
	if ifMatch == "*" {
		return true
	}
	for _, candidate := range strings.Split(ifMatch, ",") {
		// The strip happens per candidate, inside the loop, so a single tag and
		// a comma list behave identically — an intermediary that weakens
		// validators weakens them the same way however many the client sent.
		// TrimPrefix is case-sensitive by design: RFC 9110 defines the weak
		// prefix as exactly "W/", so a lowercase "w/" is not a weak validator
		// and correctly fails to match.
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == current {
			return true
		}
	}
	return false
}

func (s *Server) handleDeleteContact(ctx context.Context, in *deleteContactInput) (*deleteContactOutput, error) {
	user, err := s.requireContactStore(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.DeleteContact == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "contacts are not available on this deployment")
	}
	// Resolve first so a delete of someone else's contact 404s identically to
	// a delete of an absent one.
	current, err := s.loadContact(ctx, user.ID, in.Address)
	if err != nil {
		return nil, err
	}
	removed, err := s.deps.DeleteContact(ctx, user.ID, current.Address)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to delete contact")
	}
	if !removed {
		return nil, NewError(http.StatusNotFound, "contact_not_found", "contact not found")
	}
	return &deleteContactOutput{Body: DeleteContactResult{Deleted: true, Address: current.Address}}, nil
}
