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
	// AutoDisableReason is the short, customer-facing failure reason (e.g.
	// "HTTP 404") captured from the most recent terminal delivery error when
	// the auto-disable sweep tripped this webhook. Sourced from
	// webhook_subscriber_deliveries.last_error, which the delivery worker
	// restricts to sanitized customer-facing strings — never internal
	// hosts, IPs, or DB identifiers. Empty when never auto-disabled;
	// cleared on re-enable alongside AutoDisabledAt.
	AutoDisableReason string `json:"auto_disabled_reason,omitempty"`
	// WarnNotifiedAt is the early-warning dedupe marker: stamped by the
	// warn sweep in the same transaction as the notification enqueue,
	// cleared by a successful delivery so a recovered webhook re-arms.
	// Internal bookkeeping — not exposed on the API view.
	WarnNotifiedAt *time.Time `json:"-"`
	// ReenabledAt records the last PATCH that set enabled=true. The
	// breaker and warn sweeps only count delivery rows created after it,
	// so "fix the endpoint, then re-enable" actually resets the evidence
	// window instead of re-tripping on stale failures within one sweep.
	// Internal bookkeeping — not exposed on the API view.
	ReenabledAt     *time.Time `json:"-"`
	CreatedAt       time.Time  `json:"created_at"`
	LastDeliveredAt *time.Time `json:"last_delivered_at,omitempty"`
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

// webhookColumns is the single projection every full-webhook read uses. It is
// deliberately unqualified so the identical list also serves as an UPDATE …
// RETURNING list, where bare column names resolve to the row's POST-update
// values.
const webhookColumns = `id, user_id, url, description, events, filters,
	        signing_secret, COALESCE(signing_secret_prev, ''),
	        signing_secret_prev_expires_at,
	        enabled, auto_disabled_at, COALESCE(auto_disable_reason, ''),
	        warn_notified_at, reenabled_at, created_at, last_delivered_at`

// scanWebhook materializes one webhookColumns row. ErrNoRows is left for the
// caller to translate, since "no row" means not-found on a read and
// not-found-or-not-owned on a write.
func scanWebhook(row pgx.Row) (*Webhook, error) {
	w := &Webhook{}
	var filtersJSON []byte
	if err := row.Scan(
		&w.ID, &w.UserID, &w.URL, &w.Description, &w.Events, &filtersJSON,
		&w.SigningSecret, &w.SigningSecretPrev,
		&w.SigningSecretPrevExpiresAt,
		&w.Enabled, &w.AutoDisabledAt, &w.AutoDisableReason,
		&w.WarnNotifiedAt, &w.ReenabledAt, &w.CreatedAt, &w.LastDeliveredAt,
	); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(filtersJSON, &w.Filters); err != nil {
		return nil, fmt.Errorf("unmarshal filters: %w", err)
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
	w, err := scanWebhook(s.pool.QueryRow(ctx,
		`SELECT `+webhookColumns+` FROM webhooks WHERE id = $1 AND user_id = $2`,
		webhookID, userID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWebhookNotFound
	}
	return w, err
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
	w, err := scanWebhook(s.pool.QueryRow(ctx,
		`SELECT `+webhookColumns+` FROM webhooks WHERE id = $1`,
		webhookID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWebhookNotFound
	}
	return w, err
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
	q := `SELECT ` + webhookColumns + `
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
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
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
		`SELECT `+webhookColumns+`
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
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
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
		// burst can re-trip it cleanly — and the captured reason with
		// it, so a healthy webhook never shows a stale cause. It also
		// stamps reenabled_at: the explicit enable asserts "this
		// endpoint is fine now", and the breaker/warn sweeps only count
		// evidence created after it (otherwise the >=threshold failed
		// rows still in the window would re-disable within one sweep
		// and mail a false alert — the exact loop the feature's own
		// "fix, then re-enable" instruction would create).
		// The two evidence markers are stamped ONLY on a real disabled→enabled
		// transition, never on an enable-an-already-enabled no-op. In an UPDATE's
		// SET right-hand side a column reference yields the row's OLD value, so
		// `webhooks.enabled` here is the pre-update state.
		//
		// Unconditional stamping breaks in both directions:
		//
		//   reenabled_at — any client that reconciles desired state (an IaC loop,
		//   a health-check script, an MCP agent calling update_webhook(enabled:
		//   true) each run) would push the evidence window forward on every call.
		//   Both sweeps only count deliveries created after it, so calling more
		//   often than evidence accumulates leaves them permanently blind: the
		//   breaker never trips, no warning fires, and the customer is never told
		//   their endpoint is dead. It is also reachable on purpose — a 4-minute
		//   PATCH loop would keep e2a POSTing to an arbitrary URL forever with no
		//   breaker and no cooldown (the 5-minute cooldown keys on
		//   auto_disabled_at, which the first re-enable NULLs).
		//
		//   warn_notified_at — clearing it on every PATCH would RE-ARM the warning
		//   each time, so the same reconciler loop would mail the customer a fresh
		//   "your webhook is failing" email on every sweep. Transition-scoping it
		//   fixes the missed second episode without opening a mail loop.
		if *u.Enabled {
			args = append(args, nil)
			sets = append(sets, fmt.Sprintf("auto_disabled_at = $%d", len(args)))
			sets = append(sets, "auto_disable_reason = NULL")
			// Cleared on re-enable so a second failure episode warns again.
			// Its only other reset is a successful delivery — and after a
			// re-enable onto a still-broken endpoint no delivery ever succeeds,
			// so without this the customer gets no early warning for episode 2,
			// only the disable email 30h+ later (or never, on a webhook too
			// low-traffic to reach 10 terminal failures).
			sets = append(sets, "warn_notified_at = CASE WHEN webhooks.enabled THEN webhooks.warn_notified_at ELSE NULL END")
			sets = append(sets, "reenabled_at = CASE WHEN webhooks.enabled THEN webhooks.reenabled_at ELSE now() END")
		}
	}

	if len(sets) == 0 {
		// No-op PATCH. Return the current row.
		return s.GetWebhookByID(ctx, webhookID, userID)
	}

	// RETURNING the full row rather than RETURNING id + a separate
	// GetWebhookByID. The follow-up read ran on its own snapshot outside this
	// statement's transaction, so a concurrent PATCH could land in between and
	// the caller got the OTHER writer's config echoed back as the result of
	// their own update — and a concurrent DELETE turned a committed update into
	// a 404. RETURNING is evaluated on the row this statement wrote, so the
	// response always describes this write.
	query := fmt.Sprintf(
		`UPDATE webhooks SET %s WHERE id = $1 AND user_id = $2 RETURNING %s`,
		joinComma(sets), webhookColumns,
	)
	w, err := scanWebhook(s.pool.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWebhookNotFound
	}
	return w, err
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

// WarnThreshold / WarnWindow gate the early-warning notification: in the
// last WarnWindow, at least WarnThreshold delivery rows recorded a failed
// attempt (attempts >= 1 AND a non-empty last_error) and zero rows reached
// 'delivered'. Keying on ATTEMPT-level failures rather than terminal
// 'failed' rows is the point — a delivery only terminalizes after the
// 29h21m retry envelope, so a terminal-keyed warning could never beat the
// breaker meaningfully (docs/design/2026-08-08-webhook-health-notifications.md).
//
// TUNABLE: 5 / 24h are the design's proposed values, not yet frozen —
// low enough to fire within one sweep of a real hard-failure burst, high
// enough that a single transient blip mails nobody.
const (
	WarnThreshold = 5
	WarnWindow    = 24 * time.Hour
)

// LastErrorWebhookDisabled is the synthetic last_error the delivery
// worker's disabled-snooze cap writes on rows it terminalizes
// (webhookdelivery uses this constant). These rows are e2a bookkeeping,
// not endpoint failures, so the breaker, the warn pass, the captured
// reason, and the email failure stats all EXCLUDE them — otherwise a
// webhook disabled for >24h would accumulate synthetic "failures" that
// re-trip the breaker minutes after a re-enable, with the
// self-referential reason "webhook disabled" mailed to the customer.
const LastErrorWebhookDisabled = "webhook disabled"

// The remaining e2a-attributable terminal errors. Each is written when
// something on OUR side failed — in two of the three cases no HTTP request
// ever reached the customer's endpoint. They are declared here, beside the
// exclusion list that consumes them, and referenced by the packages that
// write them so the vocabulary cannot drift.
const (
	// LastErrorExpiredBeforeDelivery — the janitor found a row still pending
	// at its TTL, i.e. we never got it out (internal/webhook).
	LastErrorExpiredBeforeDelivery = "expired before delivery"
	// LastErrorInternalLoadDelivery — our DB read of the delivery row failed
	// on the final attempt (internal/webhookdelivery).
	LastErrorInternalLoadDelivery = "internal error loading delivery"
	// LastErrorInternalResolveWebhook — our DB read of the webhook row failed
	// on the final attempt (internal/webhookdelivery).
	LastErrorInternalResolveWebhook = "internal error resolving webhook"
)

// E2AAttributableLastErrors is the full set of terminal last_error values
// that describe an e2a-side failure rather than a customer endpoint failure.
// The breaker count, the warn count, the captured auto_disable_reason, and
// the email's failure stats all exclude every one of them.
//
// The original code excluded only LastErrorWebhookDisabled, on the correct
// reasoning that "the breaker must not eat its own output" — but applied it
// to a single string rather than to the class. The other three count toward
// the disable threshold AND are eligible to be quoted verbatim as the
// customer-facing reason, so a sustained e2a outage (DB degradation, queue
// backlog) would disable customers' webhooks and email them:
//
//	"e2a has disabled one of your webhooks after repeated delivery
//	 failures. Most recent error: internal error loading delivery."
//
// That is e2a destroying a customer's event flow during e2a's own outage and
// naming its own internal error as the customer's fault — and auto-disable is
// lossy forwards, so the events published while it stays disabled can never
// be replayed to that endpoint.
var E2AAttributableLastErrors = []string{
	LastErrorWebhookDisabled,
	LastErrorExpiredBeforeDelivery,
	LastErrorInternalLoadDelivery,
	LastErrorInternalResolveWebhook,
}

// DisableSweepMaxPerTick bounds how many webhooks one auto-disable pass may
// flip + enqueue, mirroring WarnSweepMaxPerTick. The disable pass needed this
// MORE than the warn pass did, not less: it sends the louder email and it
// destroys state (auto-disable stops fan-out, and events published while
// disabled are never queued for that webhook, so they cannot be replayed).
// Applied INSIDE the candidate subquery alongside the eligibility filter, so
// the cap bounds rows we might actually disable and drains across sweeps.
// TUNABLE.
const DisableSweepMaxPerTick = 100

// WarnSweepMaxPerTick bounds how many webhooks one warn pass may stamp +
// enqueue. A systemic failure on OUR side (egress outage) makes every
// active webhook satisfy the warn condition at once; an unbounded pass
// would mass-mail the entire customer base copy blaming THEIR endpoints,
// inside one lock-holding transaction. The cap drains legitimately over
// subsequent 5-minute sweeps. TUNABLE.
const WarnSweepMaxPerTick = 100

// WebhookNotifyTx enqueues one webhook health-notification job inside the
// sweep's transaction, so the state transition and its notification commit
// atomically (a row cannot be disabled/warn-stamped without its job, and
// cannot be returned twice). nil = no notification pipeline configured;
// the sweep then runs its state transition alone.
type WebhookNotifyTx func(ctx context.Context, tx pgx.Tx, webhookID string) error

// AutoDisableFailingWebhooks scans for webhooks whose recent delivery
// history exceeds the failure threshold and flips them to
// enabled=false with auto_disabled_at = now(), capturing the most recent
// terminal delivery error as the customer-facing auto_disable_reason.
// Returns the count of webhooks newly disabled. Designed to be called
// periodically (every 5 minutes) from the maintenance sweep.
//
// "Consecutive failed events" is interpreted as: in the last
// AutoDisableWindow, at least AutoDisableThreshold rows in
// webhook_subscriber_deliveries reached status='failed' AND zero
// rows reached status='delivered'. The zero-delivered guard prevents
// a noisy webhook that's still mostly working from being disabled.
//
// The AND enabled = true predicate makes the transition observable
// exactly once: a webhook can only be RETURNING'd on the flip, never on
// a later pass — that plus the same-tx enqueue is what guarantees
// exactly one disable notification per transition (SC2 in the design).
func (s *Store) AutoDisableFailingWebhooks(ctx context.Context, notifyTx WebhookNotifyTx) (int, error) {
	// Evidence rules shared with the warn pass: only rows created inside
	// the window AND after the last explicit re-enable count (reenabled_at
	// resets the window — see migration 097), and the snooze cap's
	// synthetic rows (last_error = LastErrorWebhookDisabled) never count —
	// the breaker must not eat its own output.
	return s.sweepWebhooksTx(ctx, notifyTx, "auto-disable",
		`UPDATE webhooks
		 SET enabled = false,
		     auto_disabled_at = now(),
		     auto_disable_reason = (
		         SELECT d.last_error
		         FROM webhook_subscriber_deliveries d
		         WHERE d.webhook_id = webhooks.id
		           AND d.status = 'failed'
		           AND d.created_at > now() - $2::interval
		           AND d.created_at > COALESCE(webhooks.reenabled_at, '-infinity'::timestamptz)
		           AND d.last_error IS NOT NULL AND d.last_error <> '' AND d.last_error <> ALL($3::text[])
		         ORDER BY d.last_attempt_at DESC NULLS LAST
		         LIMIT 1
		     )
		 WHERE id IN (
		     SELECT d.webhook_id
		     FROM webhook_subscriber_deliveries d
		     JOIN webhooks w2 ON w2.id = d.webhook_id
		          AND w2.enabled = true
		     WHERE d.created_at > now() - $2::interval
		       AND d.created_at > COALESCE(w2.reenabled_at, '-infinity'::timestamptz)
		     GROUP BY d.webhook_id
		     HAVING COUNT(*) FILTER (WHERE d.status = 'failed' AND (d.last_error IS NULL OR d.last_error <> ALL($3::text[]))) >= $1
		        AND COUNT(*) FILTER (WHERE d.status = 'delivered') = 0
		     LIMIT $4
		 )
		 AND enabled = true
		 RETURNING id`,
		AutoDisableThreshold, AutoDisableWindow, E2AAttributableLastErrors, DisableSweepMaxPerTick,
	)
}

// WarnFailingWebhooks is the early-warning pass of the same sweep: it
// stamps warn_notified_at (the dedupe marker) and enqueues one warning
// notification for every ENABLED webhook whose recent deliveries are
// failing at the attempt level — within seconds of an endpoint breaking,
// long before any delivery exhausts the retry envelope and the breaker
// can see it. warn_notified_at IS NULL is both the dedupe and the
// trigger, so the same exactly-once argument as the disable pass applies.
// A successful delivery clears the marker (webhook.SubscriberStore.
// MarkDeliveredIfPending), re-arming the warning for a later episode.
//
// Run AFTER the disable pass in a sweep: the enabled = true predicate
// then excludes rows that same sweep just disabled, so a burst that
// crosses both thresholds at once produces only the disable email.
func (s *Store) WarnFailingWebhooks(ctx context.Context, notifyTx WebhookNotifyTx) (int, error) {
	// Same evidence rules as the disable pass (window + reenabled_at reset
	// + synthetic-row exclusion), plus the per-tick cap: a systemic e2a-side
	// egress failure makes EVERY webhook satisfy this condition at once,
	// and an unbounded pass would mass-mail the customer base inside one
	// lock-holding transaction.
	//
	// Two properties this query has to get right, both of which it got wrong
	// before and neither of which any test covered:
	//
	//  1. Failures are counted only SINCE THE LAST SUCCESS
	//     (created_at > last_delivered_at), not across the whole window. The
	//     earlier form paired an unrestricted window with a
	//     COUNT(delivered) = 0 guard, which meant ANY success in the trailing
	//     24h suppressed the warning — so a webhook that delivered an hour ago
	//     and broke five minutes ago could not warn until its last success
	//     aged out, ~24h later. That is the exact "told a day late" failure
	//     this feature exists to remove, and it applied to every integration
	//     that was actually working. The zero-delivered guard is retained as a
	//     cheap invariant (no delivered row can survive the created_at filter,
	//     since last_delivered_at is stamped at delivery time) so a stale
	//     last_delivered_at still fails closed.
	//
	//  2. The eligibility filters sit INSIDE the subquery, so the LIMIT caps
	//     rows we might actually warn. Applied only on the outer UPDATE they
	//     ran after the cap, so the arbitrary (unordered) 100 was drawn from
	//     every failing webhook including already-warned ones — and those stay
	//     candidates for as long as they keep failing. Past ~100 simultaneous
	//     failures the pass warned nobody new, forever and silently (the sweep
	//     logs only when n > 0). Measured before the fix: 500 already-warned +
	//     20 never-warned → 3 warned on tick 1, then 0 on every tick after.
	//
	// The outer enabled/warn_notified_at predicates are KEPT: they are the
	// concurrency guard that gives exactly-once-per-transition under
	// concurrent sweeps (EvalPlanQual re-checks them against the updated
	// tuple), which the subquery copies cannot provide.
	return s.sweepWebhooksTx(ctx, notifyTx, "warn",
		`UPDATE webhooks
		 SET warn_notified_at = now()
		 WHERE id IN (
		     SELECT d.webhook_id
		     FROM webhook_subscriber_deliveries d
		     JOIN webhooks w2 ON w2.id = d.webhook_id
		          AND w2.enabled = true
		          AND w2.warn_notified_at IS NULL
		     WHERE d.created_at > now() - $2::interval
		       AND d.created_at > COALESCE(w2.reenabled_at, '-infinity'::timestamptz)
		       AND d.created_at > COALESCE(w2.last_delivered_at, '-infinity'::timestamptz)
		     GROUP BY d.webhook_id
		     HAVING COUNT(*) FILTER (WHERE d.attempts >= 1 AND d.last_error IS NOT NULL AND d.last_error <> '' AND d.last_error <> ALL($3::text[])) >= $1
		        AND COUNT(*) FILTER (WHERE d.status = 'delivered') = 0
		     LIMIT $4
		 )
		 AND enabled = true
		 AND warn_notified_at IS NULL
		 RETURNING id`,
		WarnThreshold, WarnWindow, E2AAttributableLastErrors, WarnSweepMaxPerTick,
	)
}

// sweepWebhooksTx runs one maintenance-pass UPDATE … RETURNING id inside a
// transaction and invokes notifyTx once per returned id in that same
// transaction. Any enqueue error aborts the whole pass — the state
// transition must never commit without its notification job (the next
// sweep retries the transition from scratch). Ids are drained before the
// enqueues run because pgx forbids issuing statements on a tx while a
// result set is open.
func (s *Store) sweepWebhooksTx(ctx context.Context, notifyTx WebhookNotifyTx, pass, query string, args ...interface{}) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("%s sweep: begin: %w", pass, err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("%s sweep: %w", pass, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("%s sweep: scan: %w", pass, err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("%s sweep: %w", pass, err)
	}

	if notifyTx != nil {
		for _, id := range ids {
			if err := notifyTx(ctx, tx, id); err != nil {
				return 0, fmt.Errorf("%s sweep: enqueue notify for %s: %w", pass, id, err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("%s sweep: commit: %w", pass, err)
	}
	return len(ids), nil
}

// WebhookFailureStats summarizes a webhook's recent delivery failures for
// the health-notification emails.
type WebhookFailureStats struct {
	// FailedAttempts counts delivery rows in the window that recorded a
	// failed attempt (attempts >= 1 AND a non-empty last_error) — the same
	// attempt-level signal the warn sweep keys on, so the email's number
	// matches the condition that triggered it.
	FailedAttempts int
	// LastError is the most recent non-empty last_error in the window —
	// already sanitized to customer-facing strings by the delivery worker
	// (never internal hosts, IPs, or DB identifiers).
	LastError string
}

// RecentWebhookFailureStats reports the failing-delivery summary quoted in
// the health-notification emails.
func (s *Store) RecentWebhookFailureStats(ctx context.Context, webhookID string, window time.Duration) (WebhookFailureStats, error) {
	var st WebhookFailureStats
	// Every e2a-attributable row is excluded from both the count and the
	// quoted error: they are e2a bookkeeping, not endpoint failures. Quoting
	// "webhook disabled" as the reason a webhook was disabled would be
	// self-referential nonsense, and quoting "internal error loading
	// delivery" would blame the customer for our outage.
	//
	// The reenabled_at cut is applied here too. Every other evidence query
	// has it; without it the email's "failed deliveries in the last N days"
	// counts failures the breaker itself was told to ignore, so the number
	// matches neither the evidence that tripped it nor the customer's own
	// dashboard — in the one message whose only asset is being credible.
	// The re-enable cut is read as a scalar subquery rather than a join: the
	// outer query aggregates, so a joined row cannot be referenced from the
	// correlated last_error subquery ("subquery uses ungrouped column").
	err := s.pool.QueryRow(ctx,
		`SELECT
		    COUNT(*) FILTER (WHERE d.attempts >= 1 AND d.last_error IS NOT NULL AND d.last_error <> '' AND d.last_error <> ALL($3::text[])),
		    COALESCE((
		        SELECT d2.last_error
		        FROM webhook_subscriber_deliveries d2
		        WHERE d2.webhook_id = $1
		          AND d2.created_at > now() - $2::interval
		          AND d2.created_at > COALESCE((SELECT w2.reenabled_at FROM webhooks w2 WHERE w2.id = $1), '-infinity'::timestamptz)
		          AND d2.last_error IS NOT NULL AND d2.last_error <> '' AND d2.last_error <> ALL($3::text[])
		        ORDER BY d2.last_attempt_at DESC NULLS LAST
		        LIMIT 1
		    ), '')
		 FROM webhook_subscriber_deliveries d
		 WHERE d.webhook_id = $1
		   AND d.created_at > now() - $2::interval
		   AND d.created_at > COALESCE((SELECT w.reenabled_at FROM webhooks w WHERE w.id = $1), '-infinity'::timestamptz)`,
		webhookID, window, E2AAttributableLastErrors,
	).Scan(&st.FailedAttempts, &st.LastError)
	if err != nil {
		return WebhookFailureStats{}, fmt.Errorf("webhook failure stats: %w", err)
	}
	return st, nil
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
