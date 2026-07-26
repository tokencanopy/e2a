package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- Webhook resource (top-level webhooks-as-a-resource feature) ---
//
// A Webhook is one subscriber row in the new /v1/webhooks resource.
// It is owned by a user (cross-user reads return ErrWebhookNotFound to
// avoid leaking existence), can subscribe to one or more event types,
// and applies scope filters (agent_ids, conversation_ids, labels) to
// further narrow which events fire to it.
//
// This subscriber resource is the sole push path: the legacy per-agent
// agent_identities.webhook_url + agent_mode columns were dropped in
// migration 029 (slice 3). See the final design at
// tmp/e2a_webhooks_design.md for the full feature scope.

// WebhookFilters is the structured form of webhooks.filters JSONB.
// Empty / nil slices mean "no constraint of that type" — a webhook
// with all-empty filters is a cross-cutting subscriber that matches
// every event of the right type for the owning user.
type WebhookFilters struct {
	AgentIDs        []string `json:"agent_ids,omitempty"`
	ConversationIDs []string `json:"conversation_ids,omitempty"`
	Labels          []string `json:"labels,omitempty"`
}

// Webhook is one row in the webhooks table.
//
// SigningSecret carries the plaintext secret. It's populated on
// CreateWebhook responses (the caller's one chance to see the secret)
// and read by the delivery worker when signing the X-E2A-Signature
// header. Public API GET endpoints in slice 2 will scrub this field
// before responding so a stolen API key cannot exfiltrate webhook
// secrets via list/get.
//
// SigningSecretPrev + SigningSecretPrevExpiresAt hold the previous
// secret during the 24h rotation grace window; slice 4 dual-signs
// using both during that window so receivers can roll forward.
type Webhook struct {
	ID                         string         `json:"id"`
	UserID                     string         `json:"user_id"`
	URL                        string         `json:"url"`
	Description                string         `json:"description"`
	Events                     []string       `json:"events"`
	Filters                    WebhookFilters `json:"filters"`
	SigningSecret              string         `json:"signing_secret,omitempty"`
	SigningSecretPrev          string         `json:"-"`
	SigningSecretPrevExpiresAt *time.Time     `json:"-"`
	Enabled                    bool           `json:"enabled"`
	AutoDisabledAt             *time.Time     `json:"auto_disabled_at,omitempty"`
	CreatedAt                  time.Time      `json:"created_at"`
	LastDeliveredAt            *time.Time     `json:"last_delivered_at,omitempty"`
}

// Sentinel errors so API handlers can map error → HTTP status with
// errors.Is rather than string-matching.
var (
	ErrWebhookNotFound   = errors.New("webhook not found")
	ErrWebhookCapReached = errors.New("webhook count limit reached for this user")
)

// generateWebhookID and generateWebhookSecret produce the prefixed IDs
// and secrets used by the webhooks API. Both use crypto/rand and
// panic on OS RNG failure — same pattern as generateID +
// generateAPIKey in store.go (an all-zero secret is catastrophic).
func generateWebhookID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("identity: crypto/rand failed: %v", err))
	}
	return "wh_" + hex.EncodeToString(b)
}

// generateWebhookSecret returns a prefixed secret of the form whsec_<64-hex>.
// The whsec_ prefix matches Stripe's convention so secret-scanning tools
// (GitGuardian, GitHub secret scanning, etc.) can recognize the format.
// 32 bytes of entropy is plenty for HMAC-SHA256 keying.
func generateWebhookSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("identity: crypto/rand failed: %v", err))
	}
	return "whsec_" + hex.EncodeToString(b)
}

// WebhookIdemCompleter completes a keyed create's idempotency row inside the
// webhook-insert transaction, so the new subscription and its cached replay
// response commit atomically — a crash after the commit replays the first
// response (same webhook id + one-time secret) instead of re-creating. Same
// shape as agent.AcceptIdemCompleter on the async accept path. nil = no
// idempotency (unkeyed create).
type WebhookIdemCompleter func(ctx context.Context, tx pgx.Tx, wh *Webhook) error

// CreateWebhook inserts a new row and returns it with the plaintext
// signing secret populated. The plaintext is only available on this
// response — subsequent GET/list calls scrub it.
//
// Filters validation (charset, count caps, agent ownership) is the
// handler's job in slice 2; the storage layer only verifies the
// per-user count cap from account_limits.max_webhooks.
func (s *Store) CreateWebhook(ctx context.Context, userID, url, description string, events []string, filters WebhookFilters) (*Webhook, error) {
	return s.CreateWebhookIdem(ctx, userID, url, description, events, filters, nil)
}

// CreateWebhookIdem is CreateWebhook with an optional idempotency completer.
// When idemCompleteTx is non-nil the insert runs in a transaction and the
// completer is invoked inside it (mirroring the send/approve same-tx pattern —
// idempotency.Store.CompleteTx): the webhook row and the completed idempotency
// key commit together, so there is no window in which a webhook exists without
// a replayable cached response. A completer error aborts the create entirely.
// With a nil completer the insert stays the original single statement.
func (s *Store) CreateWebhookIdem(ctx context.Context, userID, url, description string, events []string, filters WebhookFilters, idemCompleteTx WebhookIdemCompleter) (*Webhook, error) {
	// Enforce the per-user cap before generating any state. Race
	// across concurrent creates is bounded by the cap + 1 in the
	// worst case; an exact race-free check would need SELECT FOR
	// UPDATE on a sentinel row, which is not worth it for a cap of
	// 50. The race-window cost is "one user briefly has 51 webhooks"
	// — acceptable.
	max, err := s.MaxWebhooksForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	count, err := s.CountWebhooksByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if count >= max {
		return nil, ErrWebhookCapReached
	}

	filtersJSON, err := json.Marshal(filters)
	if err != nil {
		return nil, fmt.Errorf("marshal filters: %w", err)
	}

	w := &Webhook{
		ID:            generateWebhookID(),
		UserID:        userID,
		URL:           url,
		Description:   description,
		Events:        events,
		Filters:       filters,
		SigningSecret: generateWebhookSecret(),
		Enabled:       true,
		CreatedAt:     time.Now(),
	}
	const insertSQL = `INSERT INTO webhooks (id, user_id, url, description, events, filters, signing_secret, enabled, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	if idemCompleteTx == nil {
		if _, err := s.pool.Exec(ctx, insertSQL,
			w.ID, w.UserID, w.URL, w.Description, w.Events, filtersJSON, w.SigningSecret, w.Enabled, w.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("insert webhook: %w", err)
		}
		return w, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin webhook create tx: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, insertSQL,
		w.ID, w.UserID, w.URL, w.Description, w.Events, filtersJSON, w.SigningSecret, w.Enabled, w.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("insert webhook: %w", err)
	}
	if err := idemCompleteTx(ctx, tx, w); err != nil {
		return nil, fmt.Errorf("complete idempotency in webhook create tx: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit webhook create tx: %w", err)
	}
	return w, nil
}

// GetWebhookByID returns the webhook iff it's owned by userID. Cross-
// user reads (or missing rows) return ErrWebhookNotFound — same
// not-found-on-cross-user convention used elsewhere in the codebase
// (conversation reads, message reads).
//
// The returned Webhook has SigningSecret populated for the delivery
// worker's benefit; the public API layer scrubs this field before
// responding to GETs.
func (s *Store) GetWebhookByID(ctx context.Context, webhookID, userID string) (*Webhook, error) {
	w := &Webhook{}
	var filtersJSON []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, url, description, events, filters,
		        signing_secret, COALESCE(signing_secret_prev, ''),
		        signing_secret_prev_expires_at,
		        enabled, auto_disabled_at, created_at, last_delivered_at
		 FROM webhooks WHERE id = $1 AND user_id = $2`,
		webhookID, userID,
	).Scan(
		&w.ID, &w.UserID, &w.URL, &w.Description, &w.Events, &filtersJSON,
		&w.SigningSecret, &w.SigningSecretPrev,
		&w.SigningSecretPrevExpiresAt,
		&w.Enabled, &w.AutoDisabledAt, &w.CreatedAt, &w.LastDeliveredAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrWebhookNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal(filtersJSON, &w.Filters); err != nil {
		return nil, fmt.Errorf("unmarshal filters: %w", err)
	}
	return w, nil
}

// GetWebhookByIDInternal returns the webhook by ID with no ownership
// check. INTERNAL USE ONLY — handler code MUST use GetWebhookByID
// which scopes by user_id. The retry worker uses this to look up the
// URL + signing secret for a delivery row whose ownership was already
// established when the publisher inserted it.
//
// The suffix Internal mirrors the convention in dkim.GetDKIMKeyInternal:
// a method name that calls out "skipping the standard authorization
// check" so a reviewer doesn't have to read the body to know why.
func (s *Store) GetWebhookByIDInternal(ctx context.Context, webhookID string) (*Webhook, error) {
	w := &Webhook{}
	var filtersJSON []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, url, description, events, filters,
		        signing_secret, COALESCE(signing_secret_prev, ''),
		        signing_secret_prev_expires_at,
		        enabled, auto_disabled_at, created_at, last_delivered_at
		 FROM webhooks WHERE id = $1`,
		webhookID,
	).Scan(
		&w.ID, &w.UserID, &w.URL, &w.Description, &w.Events, &filtersJSON,
		&w.SigningSecret, &w.SigningSecretPrev,
		&w.SigningSecretPrevExpiresAt,
		&w.Enabled, &w.AutoDisabledAt, &w.CreatedAt, &w.LastDeliveredAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrWebhookNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal(filtersJSON, &w.Filters); err != nil {
		return nil, fmt.Errorf("unmarshal filters: %w", err)
	}
	return w, nil
}

// Storage layer surfaces enabled and disabled rows alike; filter at the handler
// if needed.
//
// ListWebhooksByUser returns one page of the user's webhooks, newest-first,
// keyset-paginated on (created_at, id). limit<=0 returns every webhook
// unpaginated (the all-consumers: prober seed); a positive limit fetches that
// many (pass limit+1 to detect a further page) starting after the
// (afterCreatedAt, afterID) key from the previous page's last row (zero
// afterCreatedAt = first page).
func (s *Store) ListWebhooksByUser(ctx context.Context, userID string, limit int, afterCreatedAt time.Time, afterID string) ([]Webhook, error) {
	q := `SELECT id, user_id, url, description, events, filters,
	        signing_secret, COALESCE(signing_secret_prev, ''),
	        signing_secret_prev_expires_at,
	        enabled, auto_disabled_at, created_at, last_delivered_at
	 FROM webhooks WHERE user_id = $1`
	args := []interface{}{userID}
	if !afterCreatedAt.IsZero() {
		i := len(args) + 1
		q += fmt.Sprintf(` AND (created_at < $%d OR (created_at = $%d AND id < $%d))`, i, i, i+1)
		args = append(args, afterCreatedAt, afterID)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT $%d`, len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Webhook
	for rows.Next() {
		var w Webhook
		var filtersJSON []byte
		if err := rows.Scan(
			&w.ID, &w.UserID, &w.URL, &w.Description, &w.Events, &filtersJSON,
			&w.SigningSecret, &w.SigningSecretPrev,
			&w.SigningSecretPrevExpiresAt,
			&w.Enabled, &w.AutoDisabledAt, &w.CreatedAt, &w.LastDeliveredAt,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(filtersJSON, &w.Filters); err != nil {
			return nil, fmt.Errorf("unmarshal filters: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListEnabledWebhooksForRouting is the hot-path query used by the
// event publisher. Returns enabled webhooks for the user that
// subscribe to the given event type. The in-process publisher then
// applies filter matching in Go (cheaper than encoding the
// AND-across-types + OR-within-type rule in SQL at slice-1 scale).
//
// The partial index idx_webhooks_user_enabled WHERE enabled = true
// keeps this O(log n) on the common case (a user has a small number
// of enabled webhooks).
func (s *Store) ListEnabledWebhooksForRouting(ctx context.Context, userID, eventType string) ([]Webhook, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, url, description, events, filters,
		        signing_secret, COALESCE(signing_secret_prev, ''),
		        signing_secret_prev_expires_at,
		        enabled, auto_disabled_at, created_at, last_delivered_at
		 FROM webhooks
		 WHERE user_id = $1 AND enabled = true AND $2 = ANY(events)`,
		userID, eventType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Webhook
	for rows.Next() {
		var w Webhook
		var filtersJSON []byte
		if err := rows.Scan(
			&w.ID, &w.UserID, &w.URL, &w.Description, &w.Events, &filtersJSON,
			&w.SigningSecret, &w.SigningSecretPrev,
			&w.SigningSecretPrevExpiresAt,
			&w.Enabled, &w.AutoDisabledAt, &w.CreatedAt, &w.LastDeliveredAt,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(filtersJSON, &w.Filters); err != nil {
			return nil, fmt.Errorf("unmarshal filters: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// CountWebhooksByUser returns the total number of webhooks (enabled +
// disabled) the user owns. Used by CreateWebhook to enforce the
// per-user cap from account_limits.max_webhooks.
func (s *Store) CountWebhooksByUser(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM webhooks WHERE user_id = $1`, userID,
	).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// MaxWebhooksForUser returns the per-user cap from account_limits.
// Users without an account_limits row default to 50 — the column
// DEFAULT, mirrored here as a fallback so the cap works on dev
// installs that haven't seeded an account_limits row.
const DefaultMaxWebhooks = 50

func (s *Store) MaxWebhooksForUser(ctx context.Context, userID string) (int, error) {
	var n *int
	err := s.pool.QueryRow(ctx,
		`SELECT max_webhooks FROM account_limits WHERE user_id = $1`, userID,
	).Scan(&n)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DefaultMaxWebhooks, nil
		}
		return 0, err
	}
	if n == nil {
		return DefaultMaxWebhooks, nil
	}
	return *n, nil
}

// WebhookUpdate carries the fields a PATCH can change. All fields are
// pointers (or "set-or-leave" flags) so handlers can distinguish
// "field present, set to X" from "field not present, leave unchanged".
//
// Per the design, url / events / filters are full-replace fields (the
// sent value is canonical when present). Enabled is a toggle. Re-enable
// has a 5-minute cooldown — UpdateWebhook returns ErrWebhookCooldown
// when the caller tries to flip Enabled true within 5 minutes of
// auto_disabled_at.
type WebhookUpdate struct {
	URL         *string
	Description *string
	Events      *[]string
	Filters     *WebhookFilters
	Enabled     *bool
}

// ErrWebhookCooldown is returned when a PATCH would re-enable a
// webhook that was auto-disabled within the last 5 minutes. Slice 4
// adds the auto-disable worker; this error type lands now so the
// handler doesn't need to map magic strings later.
var ErrWebhookCooldown = errors.New("webhook was auto-disabled within the last 5 minutes; wait before re-enabling")

// reEnableCooldown is the minimum delay between auto_disabled_at and
// a PATCH that flips enabled back to true. Decision #10.
const reEnableCooldown = 5 * time.Minute

// UpdateWebhook applies a partial update to a webhook. Only fields
// with a non-nil pointer in WebhookUpdate are touched. Returns the
// updated row.
//
// Validation (charset, count caps, agent ownership) is the handler's
// job; the storage layer enforces only the re-enable cooldown and
// the per-row CHECK constraints (events non-empty, url non-empty).
func (s *Store) UpdateWebhook(ctx context.Context, webhookID, userID string, u WebhookUpdate) (*Webhook, error) {
	// Re-enable cooldown — read the current state once before
	// running the UPDATE so we can return a typed error.
	if u.Enabled != nil && *u.Enabled {
		var autoDisabledAt *time.Time
		err := s.pool.QueryRow(ctx,
			`SELECT auto_disabled_at FROM webhooks WHERE id = $1 AND user_id = $2`,
			webhookID, userID,
		).Scan(&autoDisabledAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrWebhookNotFound
			}
			return nil, err
		}
		if autoDisabledAt != nil && time.Since(*autoDisabledAt) < reEnableCooldown {
			return nil, ErrWebhookCooldown
		}
	}

	// Build a dynamic UPDATE based on which fields are present. Using
	// COALESCE keeps the query simple at the cost of always touching
	// every column; at slice-1 webhook counts this isn't a concern.
	args := []interface{}{webhookID, userID}
	sets := []string{}
	add := func(col string, val interface{}) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if u.URL != nil {
		add("url", *u.URL)
	}
	if u.Description != nil {
		add("description", *u.Description)
	}
	if u.Events != nil {
		add("events", *u.Events)
	}
	if u.Filters != nil {
		filtersJSON, err := json.Marshal(*u.Filters)
		if err != nil {
			return nil, fmt.Errorf("marshal filters: %w", err)
		}
		add("filters", filtersJSON)
	}
	if u.Enabled != nil {
		add("enabled", *u.Enabled)
		// Re-enabling clears auto_disabled_at so a subsequent fail
		// burst can re-trip it cleanly.
		if *u.Enabled {
			args = append(args, nil)
			sets = append(sets, fmt.Sprintf("auto_disabled_at = $%d", len(args)))
		}
	}

	if len(sets) == 0 {
		// No-op PATCH. Return the current row.
		return s.GetWebhookByID(ctx, webhookID, userID)
	}

	query := fmt.Sprintf(
		`UPDATE webhooks SET %s WHERE id = $1 AND user_id = $2 RETURNING id`,
		joinComma(sets),
	)
	var returnedID string
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&returnedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrWebhookNotFound
		}
		return nil, err
	}
	return s.GetWebhookByID(ctx, webhookID, userID)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// RotateSecret generates a new signing secret, moves the current
// secret into signing_secret_prev with a 24h expiry, and returns
// the new plaintext (shown once). During the 24h grace window the
// delivery worker dual-signs each request so receivers can verify
// with either secret while they update their handler.
func (s *Store) RotateSecret(ctx context.Context, webhookID, userID string) (newPlaintext string, prevExpiresAt time.Time, err error) {
	newPlaintext = generateWebhookSecret()
	prevExpiresAt = time.Now().Add(24 * time.Hour)

	tag, err := s.pool.Exec(ctx,
		`UPDATE webhooks
		 SET signing_secret_prev = signing_secret,
		     signing_secret_prev_expires_at = $3,
		     signing_secret = $4
		 WHERE id = $1 AND user_id = $2`,
		webhookID, userID, prevExpiresAt, newPlaintext,
	)
	if err != nil {
		return "", time.Time{}, err
	}
	if tag.RowsAffected() == 0 {
		return "", time.Time{}, ErrWebhookNotFound
	}
	return newPlaintext, prevExpiresAt, nil
}

// AutoDisableThreshold is the consecutive-failed-events count over
// AutoDisableWindow that trips a webhook into the auto-disabled
// state. Tuned per design decision #12 (10 / 72h). The reviewer
// can re-enable via PATCH after the 5-min cooldown.
const (
	AutoDisableThreshold = 10
	AutoDisableWindow    = 72 * time.Hour
)

// AutoDisableFailingWebhooks scans for webhooks whose recent delivery
// history exceeds the failure threshold and flips them to
// enabled=false with auto_disabled_at = now(). Returns the count of
// webhooks newly disabled. Designed to be called periodically (e.g.
// every 5 minutes) from a janitor goroutine.
//
// "Consecutive failed events" is interpreted as: in the last
// AutoDisableWindow, at least AutoDisableThreshold rows in
// webhook_subscriber_deliveries reached status='failed' AND zero
// rows reached status='delivered'. The zero-delivered guard prevents
// a noisy webhook that's still mostly working from being disabled.
func (s *Store) AutoDisableFailingWebhooks(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE webhooks
		 SET enabled = false,
		     auto_disabled_at = now()
		 WHERE id IN (
		     SELECT webhook_id
		     FROM webhook_subscriber_deliveries
		     WHERE created_at > now() - $2::interval
		     GROUP BY webhook_id
		     HAVING COUNT(*) FILTER (WHERE status = 'failed') >= $1
		        AND COUNT(*) FILTER (WHERE status = 'delivered') = 0
		 )
		 AND enabled = true
		 RETURNING id`,
		AutoDisableThreshold, AutoDisableWindow,
	)
	if err != nil {
		return 0, fmt.Errorf("auto-disable scan: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return count, err
		}
		count++
	}
	return count, rows.Err()
}

// ClearExpiredPrevSecrets nulls signing_secret_prev /
// signing_secret_prev_expires_at on rows past their grace window.
// Idempotent. The worker already ignores expired prev secrets at
// signing time; this janitor is a hygiene pass so GET responses
// don't carry a meaningless prev_expires_at.
func (s *Store) ClearExpiredPrevSecrets(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE webhooks
		 SET signing_secret_prev = NULL,
		     signing_secret_prev_expires_at = NULL
		 WHERE signing_secret_prev_expires_at IS NOT NULL
		   AND signing_secret_prev_expires_at < now()`,
	)
	if err != nil {
		return 0, fmt.Errorf("clear expired prev secrets: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteWebhook removes a webhook owned by the user. Idempotent:
// deleting a non-existent or cross-user webhook returns
// ErrWebhookNotFound, never silently succeeds. The ON DELETE CASCADE
// on webhook_subscriber_deliveries.webhook_id drops pending delivery
// rows automatically — no separate cleanup needed.
//
// Slice 1 includes this method (rather than deferring to slice 2's
// handler work) because tests need it for setup teardown and the
// implementation is trivial.
func (s *Store) DeleteWebhook(ctx context.Context, webhookID, userID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM webhooks WHERE id = $1 AND user_id = $2`,
		webhookID, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrWebhookNotFound
	}
	return nil
}
