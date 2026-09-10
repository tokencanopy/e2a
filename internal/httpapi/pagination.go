package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Page is the one list-response shape across every v1 collection
// (api-v1-redesign §4 decision 7): an items array plus an opaque
// continuation cursor that is null on the last page.
//
//	{ "items": [...], "next_cursor": "…" | null }
//
// It is generic over the item view type so each resource reuses the same
// envelope without redefining it.
type Page[T any] struct {
	Items      []T     `json:"items" nullable:"false"`
	NextCursor *string `json:"next_cursor"`
}

// NewPage builds a Page. A nil/empty nextCursor renders as JSON null,
// signalling "no more pages"; items is normalized to a non-nil empty slice
// so the field is always `[]`, never `null`.
func NewPage[T any](items []T, nextCursor string) Page[T] {
	if items == nil {
		items = []T{}
	}
	p := Page[T]{Items: items}
	if nextCursor != "" {
		p.NextCursor = &nextCursor
	}
	return p
}

// PageParams is the embeddable Huma input fragment for cursor pagination.
// Every list operation embeds it so `cursor` + `limit` are declared, typed,
// and validated identically across the surface.
type PageParams struct {
	Cursor string `query:"cursor" doc:"Opaque pagination cursor from a previous response's next_cursor. Continuation requests must not change the other filters."`
	Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"100" doc:"Maximum number of items to return (1-100)."`
}

// ErrInvalidCursor is returned when a cursor fails to decode. Handlers map
// it to a 400 with code "invalid_cursor".
var ErrInvalidCursor = errors.New("invalid pagination cursor")

// ErrCursorResourceMismatch is returned when a cursor verifies and decodes
// but was minted by a DIFFERENT collection than the one now replaying it.
// It wraps ErrInvalidCursor so `errors.Is(err, ErrInvalidCursor)` still
// holds for callers that only care that the cursor is unusable, while
// callers that want to explain *why* can branch on this.
var ErrCursorResourceMismatch = fmt.Errorf("%w: minted by a different collection", ErrInvalidCursor)

// CursorResource names the collection that minted a cursor. It is signed
// into the cursor and re-checked on decode, so a cursor issued by one list
// endpoint cannot be used as a keyset anchor by another.
//
// This exists because every account-level list shares the same
// (created_at, id) cursor shape: a validly-signed cursor from, say,
// /v1/contacts used to decode cleanly on /v1/events and be applied as a
// real keyset anchor. Nothing leaked — the queries stayed account-scoped —
// but the anchor was meaningless, so the endpoint returned an arbitrarily
// truncated page (commonly `{"items":[]}`) and the client concluded "no
// events" when there were plenty. A silent wrong answer is worse than a
// 400. The agent-scoped lists (messages, conversations, engagements, agent
// suppressions) already pinned their cursors to a filter fingerprint and
// so already rejected foreign cursors; this generalizes that guarantee to
// every collection, including the ones with no filters to fingerprint.
//
// The value is part of the signed payload, not just a client-visible tag:
// a client cannot re-target a cursor without breaking the HMAC.
type CursorResource string

const (
	cursorAgents              CursorResource = "agents"
	cursorAPIKeys             CursorResource = "api_keys"
	cursorAccountSuppressions CursorResource = "account_suppressions"
	cursorAgentSuppressions   CursorResource = "agent_suppressions"
	cursorContacts            CursorResource = "contacts"
	cursorConversations       CursorResource = "conversations"
	cursorDomains             CursorResource = "domains"
	cursorEngagements         CursorResource = "engagements"
	cursorEvents              CursorResource = "events"
	cursorMessageLifecycle    CursorResource = "message_lifecycle"
	cursorMessages            CursorResource = "messages"
	cursorReviews             CursorResource = "reviews"
	cursorScheduled           CursorResource = "scheduled"
	cursorStarterTemplates    CursorResource = "starter_templates"
	cursorTemplates           CursorResource = "templates"
	cursorWebhookDeliveries   CursorResource = "webhook_deliveries"
	cursorWebhooks            CursorResource = "webhooks"
)

// cursorEnvelope is the on-the-wire cursor payload: the minting
// collection plus that collection's own private position/filter struct.
// Wrapping (rather than adding an `r` field to each of the sixteen cursor
// structs) is what makes the binding impossible to forget — the resource
// is a required argument of EncodeCursor/DecodeCursor, so a new list
// endpoint cannot accidentally mint an unbound cursor. The same mechanism
// covers the second binding axis, the minting ACCOUNT: the account ID is
// likewise a required argument (threaded from the handler's authenticated
// principal, never pulled implicitly from a context), so a missed site is
// a compile error, not a silently unbound cursor. The account does NOT
// appear as an envelope field — see cursorKey for why it lives in the MAC
// key instead.
type cursorEnvelope struct {
	Resource string          `json:"r"`
	Payload  json.RawMessage `json:"p"`
}

// keysetCursor is the standard opaque continuation for a collection
// keyset-paginated on (created_at, id) with no cursor-pinned filters — the
// common case across the account-scoped lists (agents, domains, webhooks,
// webhook deliveries, templates, api keys, reviews). The compact json keys keep
// the encoded cursor short. Resources that must pin extra filters across a
// continuation (messages, events) define their own richer cursor instead.
//
// `id` holds whatever unique tiebreak the resource's ORDER BY uses — the row id
// for most, the domain string for the domains list (its unique key).
type keysetCursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
	// Deleted binds a cursor to the trash view (?deleted=true) on collections
	// that have one (agents), so a continuation can't silently flip between
	// the live list and the trash. omitempty keeps pre-existing live-list
	// cursors decodable (absent = live view).
	Deleted bool `json:"dl,omitempty"`
}

// decodeCursor is the one place list handlers turn a client-supplied cursor
// into their own cursor struct. It binds the cursor to `resource` and to the
// calling account (`accountID` — the authenticated Principal.User.ID) and
// converts every failure into the right 400 invalid_cursor envelope, so no
// handler has to remember any of those steps.
//
// An empty cursor is the first page: dst is left untouched and nil returned.
func (s *Server) decodeCursor(accountID string, resource CursorResource, cursor string, dst any) error {
	err := DecodeCursor([]string{s.deps.CursorSecret}, accountID, resource, cursor, dst)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrCursorResourceMismatch):
		return NewError(http.StatusBadRequest, "invalid_cursor",
			"cursor was created by a different endpoint — start a new query without a cursor")
	default:
		return NewError(http.StatusBadRequest, "invalid_cursor", "invalid pagination cursor")
	}
}

// decodeKeyset resolves a (created_at, id) continuation cursor into its keyset
// position. An empty cursor is the first page (zero time, empty id). A malformed,
// tampered, or foreign-collection cursor yields a 400 invalid_cursor envelope.
//
// This is the NO-trash-view variant, so a cursor carrying Deleted=true is
// rejected. Today that is unreachable — agents is the only collection with a
// trash view and it uses decodeKeysetView. It is asserted anyway because the
// resource argument is compile-enforced while the view argument is not: a future
// endpoint that grows a ?deleted= filter but keeps calling decodeKeyset would
// silently lose its view binding with no compile error. Fail loudly instead.
func (s *Server) decodeKeyset(accountID string, resource CursorResource, cursor string) (time.Time, string, error) {
	if cursor == "" {
		return time.Time{}, "", nil
	}
	var cur keysetCursor
	if err := s.decodeCursor(accountID, resource, cursor, &cur); err != nil {
		return time.Time{}, "", err
	}
	if cur.Deleted {
		return time.Time{}, "", NewError(http.StatusBadRequest, "invalid_cursor",
			"cursor was created with a different view — start a new query without a cursor")
	}
	return cur.CreatedAt, cur.ID, nil
}

// decodeKeysetView is decodeKeyset for collections with a trash view: it also
// verifies the cursor was minted for the SAME view (live vs deleted), so a
// live-list cursor replayed with ?deleted=true (or vice versa) is a 400
// instead of a silently truncated other-view listing.
func (s *Server) decodeKeysetView(accountID string, resource CursorResource, cursor string, deleted bool) (time.Time, string, error) {
	if cursor == "" {
		return time.Time{}, "", nil
	}
	var cur keysetCursor
	if err := s.decodeCursor(accountID, resource, cursor, &cur); err != nil {
		return time.Time{}, "", err
	}
	if cur.Deleted != deleted {
		return time.Time{}, "", NewError(http.StatusBadRequest, "invalid_cursor",
			"cursor was created with a different view — start a new query without a cursor")
	}
	return cur.CreatedAt, cur.ID, nil
}

// encodeKeyset mints the next-page cursor from the last row's (created_at, id).
// A marshal failure maps to a 500 envelope (matches the other list handlers).
func (s *Server) encodeKeyset(accountID string, resource CursorResource, createdAt time.Time, id string) (string, error) {
	return s.encodeKeysetView(accountID, resource, createdAt, id, false)
}

// encodeKeysetView is encodeKeyset carrying the trash-view flag.
func (s *Server) encodeKeysetView(accountID string, resource CursorResource, createdAt time.Time, id string, deleted bool) (string, error) {
	c, err := EncodeCursor(s.deps.CursorSecret, accountID, resource, keysetCursor{CreatedAt: createdAt, ID: id, Deleted: deleted})
	if err != nil {
		return "", NewError(http.StatusInternalServerError, "internal_error", "failed to build pagination cursor")
	}
	return c, nil
}

// effectiveLimit normalizes a request limit to the default when unset (<=0).
// Mirrors the inline `if limit <= 0 { limit = 100 }` the list handlers share.
func effectiveLimit(limit int) int {
	if limit <= 0 {
		return defaultPageLimit
	}
	return limit
}

// defaultPageLimit is the page size when a list request omits limit — the same
// default PageParams declares (100).
const defaultPageLimit = 100

// EncodeCursor serializes an arbitrary cursor payload (the position +
// filter snapshot a resource needs to resume) into the opaque, URL-safe,
// tamper-evident string clients echo back.
//
// The cursor is HMAC-signed (issue #144, finding M2): a client can no
// longer decode the base64, edit a field, and re-encode it — any such edit
// breaks the signature and DecodeCursor rejects it. The cursor remains
// opaque to clients; the payload shape stays private to each resource.
//
// resource is the minting collection; it is embedded in the signed payload
// so DecodeCursor can reject a cursor replayed on a different endpoint.
//
// accountID is the minting account (the authenticated Principal.User.ID —
// the level every list query is already scoped at, deliberately NOT the API
// key ID, whose rotation would break in-flight pagination, and NOT the
// scope/agent, so an account-scoped key can resume a cursor an agent-scoped
// key minted over the same rows). It binds the cursor via the MAC key — see
// cursorKey — so account A's cursor fails verification for account B on the
// same endpoint instead of anchoring B's query at a meaningless position.
//
// Format:
//
//	envelope     = {"r": resource, "p": json_payload}
//	base64url(envelope) + "." + base64url(hmac_sha256(cursorKey(secret, accountID), base64url(envelope)))
//
// The MAC is computed over the LITERAL emitted base64url(json) segment (not
// a re-marshaled struct) so encode and verify are byte-canonical and cannot
// drift. secret is the deployment HMAC secret (config.Signing.HMACSecret) —
// the same deployment key used by approval tokens,
// so there is no new key to manage.
func EncodeCursor(secret, accountID string, resource CursorResource, payload any) (string, error) {
	inner, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(cursorEnvelope{Resource: string(resource), Payload: inner})
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	sig := cursorMAC(cursorKey(secret, accountID), []byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// DecodeCursor verifies a cursor's HMAC and reverses it into dst. A
// malformed, tampered, or wrong-secret cursor yields ErrInvalidCursor
// rather than a generic error so callers branch on a stable sentinel. An
// empty cursor is treated as "start from the beginning" — dst is left
// untouched and nil is returned.
//
// secrets is tried in order and the signature is compared in constant time
// (hmac.Equal, mirroring approvaltoken.Verify). Accepting a slice supports
// HMAC-secret rotation: a cursor signed under an old secret keeps verifying
// until that secret is retired. Today callers pass a single-element slice.
//
// resource is the collection doing the decoding. A cursor whose signed
// envelope names a different collection is rejected with
// ErrCursorResourceMismatch — the fix for foreign cursors being silently
// accepted as keyset anchors across account-level lists.
//
// accountID is the account doing the decoding. Because the account binds
// via the derived MAC key (cursorKey), a cursor minted by a different
// account simply fails signature verification and falls out as the plain
// ErrInvalidCursor — deliberately indistinguishable from a forged or
// tampered cursor, so the error surface gains no new code and a caller
// learns nothing about whether a stolen cursor was "well-formed".
//
// Old unsigned cursors (plain base64url(json) with no "." signature segment,
// as emitted before issue #144 M2) no longer verify and are hard-rejected
// with ErrInvalidCursor. Pre-envelope signed cursors are likewise rejected:
// they carry no resource, so there is nothing to bind them to. Cursors are
// ephemeral, so a client mid-pagination simply restarts the query — the
// same trade the M2 signing change already made.
func DecodeCursor(secrets []string, accountID string, resource CursorResource, cursor string, dst any) error {
	if cursor == "" {
		return nil
	}
	parts := strings.SplitN(cursor, ".", 2)
	if len(parts) != 2 {
		return ErrInvalidCursor
	}
	providedSig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ErrInvalidCursor
	}
	matched := false
	for _, secret := range secrets {
		if hmac.Equal(providedSig, cursorMAC(cursorKey(secret, accountID), []byte(parts[0]))) {
			matched = true
			break
		}
	}
	if !matched {
		return ErrInvalidCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ErrInvalidCursor
	}
	var env cursorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return ErrInvalidCursor
	}
	if env.Resource == "" || len(env.Payload) == 0 {
		// Pre-envelope cursor (or a hand-rolled payload). Unbindable.
		return ErrInvalidCursor
	}
	if env.Resource != string(resource) {
		return ErrCursorResourceMismatch
	}
	if err := json.Unmarshal(env.Payload, dst); err != nil {
		return ErrInvalidCursor
	}
	return nil
}

// cursorMAC computes HMAC-SHA256 of payload under secret. Mirrors
// approvaltoken.signMAC so the two signing paths stay convention-identical.
func cursorMAC(secret, payload []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return mac.Sum(nil)
}

// cursorKeyDomain domain-separates the per-account cursor key derivation
// from any other HMAC use of the deployment secret (approval tokens sign
// with the raw secret; cursors sign with a derived key).
const cursorKeyDomain = "e2a/cursor/v1|"

// cursorKey derives the per-account cursor MAC key:
//
//	key = HMAC-SHA256(secret, "e2a/cursor/v1|" + accountID)
//
// Binding the account into the KEY rather than into the envelope is
// deliberate: the envelope is signed but NOT encrypted (plain base64url),
// and cursors travel in query strings — a plaintext account ID would leak
// into access logs, referrer headers, and anything else that records URLs.
// Key derivation keeps the envelope schema untouched and makes a foreign
// account's cursor fail plain MAC verification, indistinguishable from a
// forgery.
func cursorKey(secret, accountID string) []byte {
	return cursorMAC([]byte(secret), []byte(cursorKeyDomain+accountID))
}
