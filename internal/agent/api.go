package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/go-github/v72/github"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ory/fosite"
	"github.com/tokencanopy/e2a/internal/agentauth"
	"github.com/tokencanopy/e2a/internal/approvaltoken"
	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/dkim"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/inboundscreen"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/logredact"
	"github.com/tokencanopy/e2a/internal/oauth"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/piguard"
	"github.com/tokencanopy/e2a/internal/ratelimit"
	"github.com/tokencanopy/e2a/internal/telemetry"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhook"
	"github.com/tokencanopy/e2a/internal/webhookpub"
	"golang.org/x/net/idna"
)

// Managed one-click requests often originate from centralized mailbox-provider
// scanners and proxies, so many unrelated recipients can legitimately share one
// source IP. Keep this budget separate from attachment downloads and high enough
// for provider fan-in while still bounding anonymous token lookup work.
const unsubscribeRequestsPerMinute = 600

var feedbackEmailTimeout = 10 * time.Second

// writeJSON encodes payload as the response body. Logs encoding errors
// rather than swallowing them — an Encode failure usually means the
// client closed the connection mid-response or the payload contains a
// non-marshalable value, both of which are useful to surface in logs
// when debugging truncated responses.
func writeJSON(w http.ResponseWriter, payload any) {
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("[api] json encode failed: %v", err)
	}
}

// readJSON wraps the request body in a MaxBytesReader and decodes into
// dst. Use this for every JSON-decoding handler to bound memory and
// reject obviously oversized payloads early. We deliberately do not
// DisallowUnknownFields — adding it would break existing clients that
// send forward-compatible extra fields, and the SDKs publish typed
// requests so unknown-fields strictness adds little defense.
func readJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	return json.NewDecoder(r.Body).Decode(dst)
}

// normalizeEmail is the agent-package-local alias for identity.NormalizeEmail.
// Defined here as a one-line forwarder so the existing call sites in this
// file stay readable; the canonical implementation lives in identity so
// ws/, auth/, and oauth_handlers all reach the same canonicalization.
func normalizeEmail(email string) string {
	return identity.NormalizeEmail(email)
}

// writeTooManyRequests sends a 429 response with a Retry-After header
// (delay-seconds form, RFC 7231 §7.1.3). Callers should pass the
// duration returned from Limiter.AllowWithRetryAfter so the value
// reflects when the next slot actually opens up. Callers must return
// after invoking this — it writes the full response.
func writeTooManyRequests(w http.ResponseWriter, retryAfter time.Duration, msg string) {
	secs := int(retryAfter.Round(time.Second).Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	http.Error(w, msg, http.StatusTooManyRequests)
}

// Standard request body limits. Most agent/domain payloads are tiny;
// /send carries a full email body which can include large HTML +
// attachments base64-inlined, so it gets the largest cap. Anything over
// these limits is almost certainly malicious or a bug.
const (
	maxRequestBytesSmall = 64 * 1024        // 64 KB — domain/agent CRUD, HITL approve
	maxRequestBytesSend  = 25 * 1024 * 1024 // 25 MB — outbound /send + reply (matches typical SMTP attachment limits)
)

// ValidateWebhookImageURL — see ValidateWebhookURL.
//
// ValidateWebhookURL checks that a webhook URL is safe to call (SSRF protection).
func ValidateWebhookURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "https" {
		return fmt.Errorf("webhook URL must use HTTPS")
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("webhook URL must have a host")
	}

	// Reject IP addresses directly — require domain names
	if ip := net.ParseIP(host); ip != nil {
		return fmt.Errorf("webhook URL must use a domain name, not an IP address")
	}

	// Resolve and reject private/loopback IPs
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("cannot resolve webhook host %q: %w", host, err)
	}
	for _, ip := range ips {
		if webhook.IsDisallowedWebhookIP(ip) {
			return fmt.Errorf("webhook URL must not resolve to a private/loopback/link-local address")
		}
	}

	return nil
}

var (
	slugPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`)
	reservedSlugs = map[string]bool{
		"admin": true, "postmaster": true, "abuse": true, "noreply": true,
		"no-reply": true, "mailer-daemon": true, "info": true, "help": true,
		"demo": true, "test": true, "www": true, "mail": true, "agent": true,
		"api": true, "system": true, "root": true,
	}
)

func validateSlug(slug string) error {
	if len(slug) < 2 || len(slug) > 40 {
		return fmt.Errorf("slug must be 2–40 characters")
	}
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf("slug must be lowercase alphanumeric with hyphens, no leading/trailing hyphens")
	}
	if reservedSlugs[slug] {
		return fmt.Errorf("slug %q is reserved", slug)
	}
	return nil
}

type API struct {
	store             *identity.Store
	sender            *outbound.Sender
	unsubscribeIssuer ManagedUnsubscribeIssuer
	// screen runs outbound content screening (Slice 5). Stateless heuristics
	// engine; mirrors the relay's inbound piguard engine.
	screen *piguard.Engine
	// inboundScreen runs inbound content screening for the loopback self-send
	// paths — the same engine construction (heuristics + optional Gemini) the
	// relay uses for SMTP inbound, so a self-send's inbox leg is judged
	// identically to a wire roundtrip of the same message.
	inboundScreen *piguard.Engine
	smtpRelay     *outbound.SMTPRelay
	userAuth      *auth.UserAuth
	// oidcAuth wires optional, generic OpenID Connect browser login. Nil means
	// both OIDC routes are absent; it is independent of legacy Google login.
	oidcAuth   *auth.OIDCAuth
	usage      usage.UsageTracker
	smtpDomain string
	fromDomain string
	// sharedDomain enables slug-based agent registration when non-empty.
	// See config.Config.SharedDomain for the rationale.
	sharedDomain string
	// publicURL is the externally visible base URL of the API. Surfaced
	// via GET /v1/info so CLI/SDK clients can populate absolute
	// links without each user configuring it. Empty when the operator
	// hasn't set http.public_url.
	publicURL string
	// apiURL is the externally visible base URL of the programmatic API —
	// the OAuth issuer identity and the base for the token/registration/
	// revocation/jwks endpoints. Defaults to publicURL; SetAPIURL overrides
	// it when the deployment serves the API on a different host than the web
	// app (e.g. api.e2a.dev vs e2a.dev). The OAuth authorization_endpoint
	// and login/consent pages stay on publicURL (the browser-facing web app).
	apiURL              string
	production          bool
	sendLimit           *ratelimit.Limiter
	regLimit            *ratelimit.Limiter
	pollLimit           *ratelimit.Limiter
	feedbackLimit       *ratelimit.Limiter
	dcrLimit            *ratelimit.Limiter    // OAuth Dynamic Client Registration — anonymous endpoint, per-IP
	downloadLimit       *ratelimit.Limiter    // attachment byte-download — capability-token route (no bearer), per-IP
	unsubscribeLimit    *ratelimit.Limiter    // managed unsubscribe — separate capability-token budget, per-IP
	approvalSigner      *approvaltoken.Signer // optional; if nil, magic-link endpoints return 404
	notifyEnq           NotifyEnqueuer        // optional; if nil, holdForApproval persists the hold but sends no notification
	oauthProvider       fosite.OAuth2Provider // optional; if nil, /oauth2/* endpoints return 404
	oauthStorage        *oauth.Storage        // optional; consent handler needs Pool() for cross-package tx
	signer              *agentauth.Signer     // optional; nil ⇒ JWKS serves an empty set (agent-auth disabled)
	idempotency         *idempotency.Store    // optional; when nil, Idempotency-Key header is ignored
	enforcer            limits.Enforcer       // optional; when nil, all limit checks are skipped (effectively unlimited)
	usageStore          *usage.Store          // optional; needed by handleGetMyLimits to surface current counts
	internalAPISecret   string                // optional; when empty, /api/internal/* endpoints return 503
	provisioningEnabled bool                  // false by default; keeps /api/internal/users/provision disabled even if a secret is present
	provisioningSecret  string                // required with provisioningEnabled; signs /api/internal/users/provision
	billingHookURL      string                // optional; when set, handleDeleteUserData POSTs an HMAC-signed user-deleted notice here (sidecar's /api/internal/billing/cancel)
	// subscriberStore powers the slice-2 webhooks-as-a-resource
	// /webhooks/{id}/test and /webhooks/{id}/deliveries endpoints.
	// Optional — when nil, those endpoints return 404 (the rest of
	// the /webhooks CRUD still works, just without test + history).
	subscriberStore *webhook.SubscriberStore

	// domainTeardownHook (decision 4 / Slice 4) is run, within the account-
	// delete transaction, for every domain the user owns — it enqueues the
	// SES sending-identity deprovision job. Optional; nil when SES is not
	// configured (the orphan reaper is the backstop either way).
	domainTeardownHook func(ctx context.Context, tx pgx.Tx, domain string) error

	// outbox is the transactional event log (webhook_events). Outbound events
	// (email.sent / pending_review / review_approved / review_rejected) fire via
	// PublishTx / PublishBestEffortTx in the trigger's tx; the outbox drain fans
	// them out to subscribers and enqueues River delivery jobs.
	outbox webhookpub.Outbox
	// eventsPool is the raw pgxpool used by the slice-6 events API.
	// Optional — when nil, GET/POST /v1/events return 404.
	eventsPool *pgxpool.Pool
	// metrics is the slice 10 observability surface. Defaulted to
	// NoOp; production wires telemetry.Log or a real backend.
	metrics telemetry.Metrics

	// outboundEnq is the accept-tx's mandatory handle on the shared River client.
	// DeliverOutbound persists the message + enqueues the outbound_send job in one
	// transaction and returns accepted before provider submission.
	outboundEnq OutboundEnqueuer
	wsHub       WebSocketHub
}

// ManagedUnsubscribeIssuer is deliberately narrow: it receives only the final
// canonical recipient and returns a stored, absolute public capability URL.
type ManagedUnsubscribeIssuer interface {
	Ready() error
	Issue(ctx context.Context, userID, agentID, recipient string) (string, error)
}

const managedUnsubscribeRecipientMessage = "managed unsubscribe requires exactly one recipient"

func (a *API) SetManagedUnsubscribeIssuer(i ManagedUnsubscribeIssuer) { a.unsubscribeIssuer = i }

func prepareManagedUnsubscribe(ctx context.Context, issuer ManagedUnsubscribeIssuer, fromDomain, userID string, agent *identity.AgentIdentity, req *outbound.SendRequest, mint bool) *OutboundError {
	if req.Unsubscribe == nil {
		return nil
	}
	_, _, _, recipients, err := outbound.NormalizeRecipients(agent, fromDomain, *req)
	if err != nil {
		var validationErr *outbound.ValidationError
		if errors.As(err, &validationErr) && validationErr.Error() == "no valid recipients" {
			return &OutboundError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: managedUnsubscribeRecipientMessage}
		}
		return &OutboundError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: err.Error()}
	}
	if len(recipients) != 1 {
		return &OutboundError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: managedUnsubscribeRecipientMessage}
	}
	if issuer == nil {
		return &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "managed unsubscribe unavailable"}
	}
	if err := issuer.Ready(); err != nil {
		log.Printf("[api] managed unsubscribe issuer unavailable: agent=%s error=%v", agent.ID, err)
		return &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "managed unsubscribe unavailable"}
	}
	if !mint {
		return nil
	}
	link, err := issuer.Issue(ctx, userID, agent.ID, recipients[0])
	if err != nil {
		log.Printf("[api] managed unsubscribe issue failed: agent=%s error=%v", agent.ID, err)
		return &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "managed unsubscribe unavailable"}
	}
	req.Unsubscribe.URL = link
	return nil
}

// WebSocketHub is the narrow live-delivery seam shared with internal/ws.
type WebSocketHub interface {
	IsConnected(agentID string) bool
	Send(agentID string, msg []byte) bool
}

func (a *API) SetWebSocketHub(h WebSocketHub) { a.wsHub = h }

// SetOutboundEnqueuer wires the mandatory queue-first outbound pipeline. Tests may
// leave it unset to verify that a miswired process fails closed before provider I/O.
func (a *API) SetOutboundEnqueuer(e OutboundEnqueuer) { a.outboundEnq = e }

// SetSubscriberStore wires the subscriber-store dependency after
// NewAPI. Same optional-setter convention as SetEnforcer / etc.
func (a *API) SetSubscriberStore(s *webhook.SubscriberStore) {
	a.subscriberStore = s
}

// SetOutbox wires the slice-4 transactional outbox. Used by the
// outbound /send handler for the email.sent event (post-side-effect:
// PublishBestEffortTx). Other trigger sites (HITL) continue to use
// the legacy publisher; they will migrate in a future slice if/when
// their handlers gain transactional plumbing.
func (a *API) SetOutbox(o webhookpub.Outbox) { a.outbox = o }

// SetMetrics wires the slice 10 observability backend. Default is
// telemetry.NoOp; production passes telemetry.NewLog() or a real
// counter backend.
func (a *API) SetMetrics(m telemetry.Metrics) { a.metrics = m }

// emit returns the wired metrics backend, defaulting to NoOp so
// handler-level instrumentation doesn't need nil guards everywhere.
func (a *API) emit() telemetry.Metrics {
	if a.metrics == nil {
		return telemetry.NoOp{}
	}
	return a.metrics
}

// publishSent fires email.sent to the durable outbox (webhook_events). Uses
// PublishBestEffortTx because SES has already accepted the send — the outbox
// write must never roll back the already-sent email (post-side-effect).
func (a *API) publishSent(ctx context.Context, e webhookpub.Event, outMsg *identity.Message) {
	if a.outbox != nil && outMsg != nil {
		// Deterministic id dedupes a same-key resend through ON CONFLICT DO NOTHING.
		e.ID = webhookpub.DeterministicEventID(outMsg.ID, e.Type)
		// The messages row is already committed (SES accepted the send); this tx
		// only writes the outbox row — best-effort by contract (post-side-effect).
		if err := a.store.WithTx(ctx, func(tx pgx.Tx) error {
			a.outbox.PublishBestEffortTx(ctx, tx, e)
			return nil
		}); err != nil {
			log.Printf("[api] outbox tx for email.sent err: %v", err)
		}
	}
	a.emit().OutboxEventsPublished(e.Type)
}

// publishPendingApproval fires email.review_requested (direction=outbound) via the
// outbox (PublishTx — pre-side-effect: the pending row hasn't been sent to SES
// yet). pendingMsgID seeds the deterministic event id so /send retries with the
// same idempotency key don't fire duplicate events.
func (a *API) publishPendingApproval(ctx context.Context, e webhookpub.Event, pendingMsgID string) {
	if a.outbox != nil && pendingMsgID != "" {
		e.ID = webhookpub.DeterministicEventID(pendingMsgID, e.Type)
		if err := a.store.WithTx(ctx, func(tx pgx.Tx) error {
			return a.outbox.PublishTx(ctx, tx, e)
		}); err != nil {
			log.Printf("[api] outbox tx for email.review_requested err: %v", err)
		}
	}
	a.emit().OutboxEventsPublished(e.Type)
}

// publishApproved fires email.review_approved via the outbox (PublishBestEffortTx
// — post-side-effect: SES has already accepted the approved send).
func (a *API) publishApproved(ctx context.Context, e webhookpub.Event, sentMsg *identity.Message) {
	if a.outbox != nil && sentMsg != nil {
		e.ID = webhookpub.DeterministicEventID(sentMsg.ID, e.Type)
		if err := a.store.WithTx(ctx, func(tx pgx.Tx) error {
			a.outbox.PublishBestEffortTx(ctx, tx, e)
			return nil
		}); err != nil {
			log.Printf("[api] outbox tx for %s err: %v", e.Type, err)
		}
	}
	a.emit().OutboxEventsPublished(e.Type)
}

// publishRejected fires email.review_rejected via the outbox (PublishTx —
// pre-side-effect: rejection is a row update, no SES involvement).
func (a *API) publishRejected(ctx context.Context, e webhookpub.Event, rejectedMsgID string) {
	if a.outbox != nil && rejectedMsgID != "" {
		e.ID = webhookpub.DeterministicEventID(rejectedMsgID, e.Type)
		if err := a.store.WithTx(ctx, func(tx pgx.Tx) error {
			return a.outbox.PublishTx(ctx, tx, e)
		}); err != nil {
			log.Printf("[api] outbox tx for %s err: %v", e.Type, err)
		}
	}
	a.emit().OutboxEventsPublished(e.Type)
}

// SetApprovalSigner wires in the magic-link signer after construction so
// callers (and tests) that don't need HITL magic-link endpoints don't
// have to know about it. When nil, handleApproveMagicLink /
// handleRejectMagicLink respond with 404.
func (a *API) SetApprovalSigner(s *approvaltoken.Signer) { a.approvalSigner = s }

// NotifyEnqueuer inserts the HITL approval-notification job (QueueNotify) in the
// hold accept-tx, so the reviewer's email is enqueued atomically with the
// pending_review row (docs/design/hitl-notify-river.md). Satisfied by
// *hitlnotify.Jobs. When nil (notifier unconfigured), HoldForApprovalCore persists
// the hold on the plain path and sends no notification.
type NotifyEnqueuer interface {
	EnqueueNotifyTx(ctx context.Context, tx pgx.Tx, messageID string) (int64, error)
}

// SetNotifyEnqueuer wires the HITL-notification accept-tx enqueuer. When nil,
// holdForApproval still persists the pending message but enqueues no notification —
// useful for tests that don't want the SMTP traffic, and for deployments without a
// configured relay / public URL.
func (a *API) SetNotifyEnqueuer(e NotifyEnqueuer) { a.notifyEnq = e }

// SetOAuthProvider wires in the fosite-backed OAuth provider. When
// nil, /oauth2/* endpoints return 404 (matches the
// SetApprovalSigner / SetNotifyEnqueuer pattern of "optional capability,
// silently absent when not wired"). Operators who don't want OAuth
// enabled simply don't call this.
func (a *API) SetOAuthProvider(p fosite.OAuth2Provider) { a.oauthProvider = p }

// SetSigner wires the RS256 agent-token signer (Slice 5b). Optional — when nil
// or disabled, /.well-known/jwks.json serves an empty key set and the
// agent-identity token paths (later sub-slices) report "not enabled".
func (a *API) SetSigner(s *agentauth.Signer) { a.signer = s }

// SetOIDCAuth wires optional OIDC browser login. NewOIDCAuth returns nil when
// disabled, so callers may invoke this unconditionally and leave the routes
// genuinely absent from deployments that do not opt in.
func (a *API) SetOIDCAuth(oa *auth.OIDCAuth) { a.oidcAuth = oa }

// SetOAuthStorage wires in the OAuth storage handle separately from
// the provider. The consent handler needs Pool() to begin a pgx tx
// that spans the agent-create (identity pkg) and the auth-code insert
// (fosite → oauth pkg). Provider-only callers (e.g. /token) don't need
// it, but it's required for /consent to work; setting one without the
// other is a misconfiguration the consent handler surfaces as a 503.
func (a *API) SetOAuthStorage(s *oauth.Storage) { a.oauthStorage = s }

// SetIdempotencyStore enables Idempotency-Key processing on the
// outbound /send and /reply endpoints. When nil (the default) the
// header is silently ignored — keeps the agent package usable in
// environments that don't have postgres wired or want to disable
// the feature. The cmd/e2a runtime always sets it.
func (a *API) SetIdempotencyStore(s *idempotency.Store) { a.idempotency = s }

// SetEnforcer wires in the resource-limits enforcer. When nil (the
// default) every check passes — handlers behave as if every user has
// unlimited capacity. The cmd/e2a runtime always sets it; tests that
// don't care about limits omit it and continue to work as before.
func (a *API) SetEnforcer(e limits.Enforcer) { a.enforcer = e }

// SendLimitAllow exposes the per-agent outbound rate limiter so the v1 httpapi
// layer shares the *same* token bucket as the legacy handlers (a caller hitting
// the limit via either path is counted once). key = agent id.
func (a *API) SendLimitAllow(key string) (bool, time.Duration) {
	return a.sendLimit.AllowWithRetryAfter(key)
}

// PollLimitAllow exposes the per-user read limiter (key = user id) and
// RegLimitAllow the per-IP registration limiter (key = client ip), each
// returning the IETF RateLimit snapshot so the v1 httpapi middleware can
// stamp RateLimit-Limit/Remaining/Reset. Both share the SAME buckets as the
// legacy handlers, so a caller is counted once across either surface.
func (a *API) PollLimitAllow(key string) (bool, time.Duration, int, int, int) {
	return a.pollLimit.AllowSnapshot(key)
}

func (a *API) RegLimitAllow(key string) (bool, time.Duration, int, int, int) {
	return a.regLimit.AllowSnapshot(key)
}

// DownloadLimitAllow exposes the per-IP attachment-download limiter (key = client
// ip). The download route is a raw capability-token endpoint outside the Huma
// rate-limit middleware, so it calls this directly. Returns the IETF snapshot.
func (a *API) DownloadLimitAllow(key string) (bool, time.Duration, int, int, int) {
	return a.downloadLimit.AllowSnapshot(key)
}

// UnsubscribeLimitAllow exposes the distinct per-IP managed-unsubscribe
// limiter. Mailbox providers centralize link scanning and one-click requests,
// so this has a provider-friendly budget and never consumes attachment quota.
func (a *API) UnsubscribeLimitAllow(key string) (bool, time.Duration, int, int, int) {
	return a.unsubscribeLimit.AllowSnapshot(key)
}

// SetUsageStore wires in the usage store used by handleGetMyLimits to
// surface the user's current counts (agents, domains, messages this
// month, storage bytes) alongside the resolved caps. Separate from the
// usage.UsageTracker (which is for recording events) so the dashboard
// read path can stay alive even when usage-tracking is otherwise off.
func (a *API) SetUsageStore(s *usage.Store) { a.usageStore = s }

// SetInternalAPISecret wires in the shared HMAC secret used to
// authenticate the /api/internal/limits/invalidate endpoint. When
// empty (default), that endpoint returns 503 — self-host operators
// who don't run a billing provisioner never need to configure it.
func (a *API) SetInternalAPISecret(s string) { a.internalAPISecret = s }

// ConfigureProvisioning wires the explicit feature gate and shared HMAC
// secret used to authenticate /api/internal/users/provision. The gate and
// secret are deliberately separate: merely injecting a secret must not turn
// account creation on. Disabled or missing-secret configurations return 503.
func (a *API) ConfigureProvisioning(enabled bool, secret string) {
	a.provisioningEnabled = enabled
	a.provisioningSecret = secret
}

// SetBillingHookURL wires in the URL of an external billing service's
// user-event endpoint. When the user deletes their account, the API
// HMAC-signs a JSON payload and POSTs it there so the billing service
// can cancel the corresponding Stripe subscription. When empty (the
// self-host default), no hook fires — appropriate for deployments
// without a billing service. The same internal_api_secret is reused
// for the signature.
func (a *API) SetBillingHookURL(s string) { a.billingHookURL = s }

// SetDomainTeardownHook wires the per-domain SES deprovision enqueue run in the
// account-delete transaction (decision 4 / Slice 4). Optional.
func (a *API) SetDomainTeardownHook(h func(ctx context.Context, tx pgx.Tx, domain string) error) {
	a.domainTeardownHook = h
}

func NewAPI(store *identity.Store, sender *outbound.Sender, smtpRelay *outbound.SMTPRelay, userAuth *auth.UserAuth, usage usage.UsageTracker, smtpDomain, fromDomain, sharedDomain, publicURL string, production bool) *API {
	return &API{
		store:         store,
		sender:        sender,
		screen:        buildAgentScreenEngine(),
		inboundScreen: inboundscreen.BuildEngine(),
		smtpRelay:     smtpRelay,
		userAuth:      userAuth,
		usage:         usage,
		smtpDomain:    smtpDomain,
		fromDomain:    fromDomain,
		sharedDomain:  sharedDomain,
		publicURL:     publicURL,
		// Default the API/issuer URL to the web URL; SetAPIURL overrides it
		// for split web/API-host deployments.
		apiURL:     publicURL,
		production: production,
		sendLimit:  ratelimit.New(1*time.Minute, 60), // 60 sends per agent per minute
		regLimit:   ratelimit.New(1*time.Hour, 200),  // 200 registrations per IP per hour
		// The poll bucket is keyed per USER and shared by every reader the
		// account runs — each agent's polling loop plus the dashboard, whose
		// thread view fetches message bodies individually. 60/min starved
		// multi-agent accounts the moment a human opened a long thread.
		// Operators tune this via rate_limits.poll_per_minute (SetPollRateLimit).
		pollLimit:        ratelimit.New(1*time.Minute, 240),                          // 240 poll requests per user per minute
		feedbackLimit:    ratelimit.New(1*time.Hour, 10),                             // 10 feedback submissions per IP per hour
		dcrLimit:         ratelimit.New(1*time.Hour, 10),                             // 10 OAuth client registrations per IP per hour
		downloadLimit:    ratelimit.New(1*time.Minute, 120),                          // 120 attachment downloads per IP per minute
		unsubscribeLimit: ratelimit.New(1*time.Minute, unsubscribeRequestsPerMinute), // provider-friendly one-click budget
	}
}

// SetPollRateLimit sets the per-user poll budget to perMinute requests per
// minute (config: rate_limits.poll_per_minute). Values <= 0 are ignored and
// keep the built-in default.
func (a *API) SetPollRateLimit(perMinute int) {
	if perMinute <= 0 {
		return
	}
	a.pollLimit.SetMax(perMinute)
}

// buildAgentScreenEngine constructs the piguard screening engine for outbound agent
// mail (see screenOutbound in screening.go, the engine's only caller — it always
// passes Direction: piguard.DirectionOutput). Heuristics-only: GeminiDetector is
// deliberately NOT wired in here. Its prompt (geminiSystemPrompt in
// piguard/gemini.go) classifies only inbound injection/phishing aimed at the AI
// agent and ignores Request.Direction entirely, so on outbound mail it would
// produce false-positive holds (an agent quoting injection-like content scores as
// injection) while missing the actual outbound concern (egress/exfiltration),
// which only the heuristics detector currently checks for Direction ==
// DirectionOutput. Revisit once Gemini has an outbound/exfiltration-aware prompt.
func buildAgentScreenEngine() *piguard.Engine {
	return piguard.NewEngine(piguard.EngineConfig{}, piguard.NewHeuristicsDetector())
}

// SetAPIURL overrides the API/issuer base URL (default: publicURL). Set it
// when the programmatic API + MCP are served on a different host than the web
// app — that host becomes the OAuth issuer and the base for the token/
// registration/revocation/jwks endpoints, while authorization_endpoint and
// the login/consent pages stay on publicURL. A no-op for the empty string so
// callers can pass an unset config value safely.
func (a *API) SetAPIURL(u string) {
	if u != "" {
		a.apiURL = u
	}
}

func (a *API) RegisterRoutes(r *mux.Router) {
	// Catch-all 404/405 handlers so every error response leaves the
	// server as `text/plain; charset=utf-8` with a single-line body.
	// gorilla/mux's defaults are bare status codes with no body and no
	// Content-Type, which breaks client error handling and surfaced
	// during the e2e contract sweep — see tests/e2e-prod 07-error-contract.
	r.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	r.MethodNotAllowedHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})

	// Internal machine-to-machine endpoint: the external limits
	// provisioner (hosted billing sidecar) calls this to bust the
	// in-process limits cache for a given user immediately after it
	// writes account_limits. Authenticated by shared HMAC over the
	// request body; deliberately not advertised in OpenAPI.
	r.HandleFunc("/api/internal/limits/invalidate", a.handleInvalidateLimits).Methods("POST")

	// Internal machine-to-machine endpoint: an external control plane
	// calls this to provision an e2a user idempotently (keyed by the
	// caller's external_ref) ahead of that user's first sign-in.
	// Authenticated by a dedicated shared HMAC over the request body;
	// deliberately not advertised in OpenAPI. Off by default — 503s
	// unless provisioning.enabled + the provisioning secret are set.
	r.HandleFunc("/api/internal/users/provision", a.handleProvisionUser).Methods("POST")

	// HITL magic-link pages (/v1/approve, /v1/reject) are NOT registered on
	// this mux: the chi root (internal/httpapi) owns every /v1/* path and
	// serves them directly via API.ApproveMagicLinkHandler /
	// API.RejectMagicLinkHandler. Registering them here would be dead code —
	// the chi root's not-found handler never falls back to this mux for
	// /v1/* paths.

	// --- Non-versioned operational endpoints ---
	r.HandleFunc("/api/health", a.handleHealth).Methods("GET", "HEAD")
	r.HandleFunc("/api/feedback", a.handleFeedback).Methods("POST")

	// OAuth 2.1 / RFC 6749 endpoints, root + unversioned (Slice 5b: renamed
	// from /oauth2/* to /oauth2/* to conform to the auth.md spec — no
	// back-compat alias). Handlers 404 when SetOAuthProvider wasn't called, so
	// registering unconditionally is safe.
	r.HandleFunc("/oauth2/authorize", a.handleOAuthAuthorize).Methods("GET")
	r.HandleFunc("/oauth2/consent", a.handleOAuthConsent).Methods("POST")
	r.HandleFunc("/oauth2/token", a.handleOAuthToken).Methods("POST")
	r.HandleFunc("/oauth2/revoke", a.handleOAuthRevoke).Methods("POST")
	r.HandleFunc("/oauth2/register", a.handleOAuthRegister).Methods("POST")
	r.HandleFunc("/oauth2/clients/{client_id}", a.handleOAuthGetClient).Methods("GET")
	r.HandleFunc("/.well-known/oauth-authorization-server", a.handleOAuthDiscovery).Methods("GET")
	// Public JWKS for verifying e2a-minted agent JWTs (Slice 5b). Always
	// registered; serves {"keys":[]} when no signing key is configured.
	r.HandleFunc("/.well-known/jwks.json", a.handleJWKS).Methods("GET")
	// auth.md agent-identity bootstrap (Slice 5b-2): present an agent-scoped
	// credential, receive an identity_assertion to exchange at /oauth2/token
	// (grant_type=jwt-bearer). 501 when no signing key is configured.
	r.HandleFunc("/agent/identity", a.handleAgentIdentity).Methods("POST")

	// User auth (Google OAuth for agent developers)
	if a.userAuth != nil {
		r.HandleFunc("/api/auth/login", a.userAuth.HandleLogin).Methods("GET")
		r.HandleFunc("/api/auth/callback", a.userAuth.HandleCallback).Methods("GET")
		r.HandleFunc("/api/auth/logout", a.userAuth.HandleLogout).Methods("POST")
		r.HandleFunc("/api/auth/me", a.userAuth.HandleMe).Methods("GET")
		r.HandleFunc("/api/auth/me", a.userAuth.HandleUpdateMe).Methods("PATCH")

		// Dashboard
		r.HandleFunc("/api/dashboard/stats", a.userAuth.HandleDashboardStats).Methods("GET")
		r.HandleFunc("/api/dashboard/agents", a.userAuth.HandleDashboardAgents).Methods("GET")
		r.HandleFunc("/api/dashboard/agents/{email}", a.userAuth.HandleUpdateAgent).Methods("PUT")
		r.HandleFunc("/api/dashboard/agents/{email}", a.userAuth.HandleDeleteAgent).Methods("DELETE")
		r.HandleFunc("/api/dashboard/agents/{email}/activity", a.userAuth.HandleAgentActivity).Methods("GET")

		// API Keys
		r.HandleFunc("/api/keys", a.userAuth.HandleCreateAPIKey).Methods("POST")
		r.HandleFunc("/api/keys", a.userAuth.HandleListAPIKeys).Methods("GET")
		r.HandleFunc("/api/keys/{id}", a.userAuth.HandleDeleteAPIKey).Methods("DELETE")
	}

	// Generic OIDC login is independently optional and does not require the
	// legacy Google UserAuth integration to be configured.
	if a.oidcAuth != nil {
		r.HandleFunc("/api/auth/oidc/login", a.oidcAuth.HandleLogin).Methods("GET").Name("oidc-login")
		r.HandleFunc("/api/auth/oidc/callback", a.oidcAuth.HandleCallback).Methods("GET").Name("oidc-callback")
	}
}

// errOAuthBearerInvalid is returned by authenticateUser when an
// ate2a_-prefixed bearer fails validation (revoked, expired, unknown,
// or provider not wired). writeAuthError checks errors.Is on this to
// decide whether the WWW-Authenticate challenge should include the
// OAuth-specific error params per RFC 6750 §3.1.
var errOAuthBearerInvalid = errors.New("oauth bearer invalid")

// stripBearerScheme removes the "Bearer " prefix from an Authorization
// header value. RFC 6750 §2.1 specifies the scheme name as case-
// INSENSITIVE; a literal `TrimPrefix(h, "Bearer ")` would silently fail
// on `bearer ate2a_…` or `BEARER ate2a_…`. Returns the raw header value
// when no Bearer scheme was used (lets the legacy unprefixed API-key
// path continue to work).
func stripBearerScheme(h string) string {
	parts := strings.SplitN(h, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return h
}

// authenticateUser extracts and validates the bearer credential from
// the request, returning the owning user.
//
// Dispatch is by token prefix:
//   - ate2a_  → OAuth access token (fosite-validated via the configured
//     provider). Rejected if missing, revoked, expired, or the
//     provider isn't wired.
//   - anything else (typically e2a_, but we accept legacy unprefixed
//     keys too) → API key (api_keys table).
//
// If no Authorization header is present, falls back to the session
// cookie used by the web dashboard.
// AuthenticateUser is the exported seam over authenticateUser so the v1
// httpapi layer (internal/httpapi) reuses the exact same credential-
// resolution path (API key, OAuth bearer, session cookie) instead of
// forking a second one. There is one place credentials are checked.
func (a *API) AuthenticateUser(r *http.Request) (*identity.User, error) {
	p, err := a.authenticatePrincipal(r)
	if err != nil {
		return nil, err
	}
	return p.User, nil
}

// AuthenticatePrincipal is the scope-aware seam (Slice 5a): same credential
// resolution as AuthenticateUser, but returns the full principal (user + scope
// + bound agent) so the v1 layer can enforce the hard scope ceiling. There is
// still exactly one place credentials are checked.
func (a *API) AuthenticatePrincipal(r *http.Request) (*identity.Principal, error) {
	return a.authenticatePrincipal(r)
}

// authenticateUser is the user-only convenience over authenticatePrincipal,
// retained for the legacy mux handlers (OAuth / session auth) that do not enforce the v1 scope ceiling.
func (a *API) authenticateUser(r *http.Request) (*identity.User, error) {
	p, err := a.authenticatePrincipal(r)
	if err != nil {
		return nil, err
	}
	return p.User, nil
}

func (a *API) authenticatePrincipal(r *http.Request) (*identity.Principal, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		bearer := stripBearerScheme(authHeader)
		// auth.md agent access token (Slice 5b-2): an RS256 JWT we minted,
		// resolving to an agent-scoped principal. Checked before the API-key
		// path since a JWT is never a valid API key. A bearer that looks like
		// our JWT but fails verification is rejected here (not fall-through).
		if p, ours, err := a.resolveAgentAccessToken(r, bearer); ours {
			if err != nil {
				return nil, err
			}
			return p, nil
		}
		if strings.HasPrefix(bearer, oauth.AccessTokenPrefix) {
			return a.principalFromOAuthToken(r, bearer)
		}
		return a.store.GetPrincipalByAPIKey(r.Context(), bearer)
	}
	// Fall back to session cookie auth — the web dashboard owner, account scope.
	if a.userAuth != nil {
		if user := a.userAuth.AuthenticateRequest(r); user != nil {
			return &identity.Principal{User: user, Scope: identity.ScopeAccount}, nil
		}
	}
	return nil, fmt.Errorf("authorization required")
}

// principalFromOAuthToken validates an ate2a_-prefixed bearer via fosite's
// IntrospectToken (which derives the signature using the same strategy that
// issued the token, looks up the row via our Storage, and checks
// revoked/expired) and resolves it to a SCOPED principal.
//
// The granted OAuth scope is authoritative — it is read, never assumed.
// (CRITICAL-1 fix: this path previously hardcoded every OAuth token to
// ScopeAccount, which defeated the public-DCR scope ceiling
// (oauth_handlers.go) and silently handed agent-tier MCP tokens full
// account-wide admin.) Resolution:
//   - granted `account` → account-wide admin. Reachable only when the user
//     explicitly picks account on the consent screen for an account-eligible
//     client (loopback or https — see accountEligibleRedirect); never
//     autonomously by a client.
//   - granted `agent`   → confined to the consent-bound agent
//     (session.AgentEmail), with ownership re-checked, mirroring the REST
//     resolveOwnedAgent pin and the JWT resolveAgentAccessToken path.
//   - neither           → fail closed (reject), never a silent elevation.
//
// Every failure wraps errOAuthBearerInvalid so the response layer reliably
// classifies these as OAuth-bearer rejections and emits the OAuth challenge.
func (a *API) principalFromOAuthToken(r *http.Request, bearer string) (*identity.Principal, error) {
	if a.oauthProvider == nil {
		// OAuth not enabled on this deployment. Fail closed rather
		// than fall through to the API-key path (which would compare
		// the ate2a_ token against the api_keys hash and miss — a
		// slower path to the same 401, with a less actionable log).
		return nil, fmt.Errorf("%w: provider not configured", errOAuthBearerInvalid)
	}
	session := &oauth.Session{}
	tu, ar, err := a.oauthProvider.IntrospectToken(r.Context(), bearer, fosite.AccessToken, session)
	if err != nil {
		// Preserve fosite's typed error via %w so writeAuthError can
		// errors.Is(...) against fosite.ErrTokenExpired below.
		return nil, fmt.Errorf("%w: %w", errOAuthBearerInvalid, err)
	}
	// Defense in depth: fosite v0.49's IntrospectToken with
	// tokenUse=AccessToken doesn't HARD-reject a refresh-token row —
	// it falls back to refresh-token validation if access fails. We
	// rely on table separation in storage to keep them disjoint, but
	// an explicit check at the seam means a future fosite/storage
	// refactor can't silently break the type guard.
	if tu != fosite.AccessToken {
		return nil, fmt.Errorf("%w: token is not an access token (got %q)", errOAuthBearerInvalid, tu)
	}
	// Trust the session, not the request — the session was loaded
	// from the DB row fosite hydrated.
	sess, ok := ar.GetSession().(*oauth.Session)
	if !ok || sess.UserID == "" {
		return nil, fmt.Errorf("%w: session missing user_id", errOAuthBearerInvalid)
	}
	u, err := a.store.GetUserByID(r.Context(), sess.UserID)
	if err != nil {
		// Wrap so writeAuthError emits the OAuth challenge instead of
		// a bare 401 (the bearer that got us here was valid; the user
		// row vanished out from under it).
		return nil, fmt.Errorf("%w: user lookup: %v", errOAuthBearerInvalid, err)
	}

	granted := ar.GetGrantedScopes()
	switch {
	case granted.Has(identity.ScopeAccount):
		return &identity.Principal{User: u, Scope: identity.ScopeAccount}, nil
	case granted.Has(identity.ScopeAgent):
		// Confine to the consent-bound agent. An agent-scoped token with no
		// bound agent is malformed — reject rather than fall back to anything
		// broader.
		if sess.AgentEmail == "" {
			return nil, fmt.Errorf("%w: agent-scoped token has no bound agent", errOAuthBearerInvalid)
		}
		ag, err := a.store.GetAgentByEmail(r.Context(), identity.NormalizeEmail(sess.AgentEmail))
		if err != nil || ag == nil {
			return nil, fmt.Errorf("%w: bound agent not found", errOAuthBearerInvalid)
		}
		// Ownership re-check: the bound agent must still belong to the token's
		// user (defends against an agent reassigned/deleted-and-recreated under
		// a different owner after consent).
		if ag.UserID != u.ID {
			return nil, fmt.Errorf("%w: bound agent not owned by token user", errOAuthBearerInvalid)
		}
		return &identity.Principal{User: u, Scope: identity.ScopeAgent, AgentID: ag.ID}, nil
	default:
		// Fail closed: a token that carries no recognized scope gets no access.
		return nil, fmt.Errorf("%w: token carries no recognized scope (%v)", errOAuthBearerInvalid, []string(granted))
	}
}

// writeAuthError writes a 401 response with an RFC 6750 §3
// WWW-Authenticate challenge.
//
// Every 401 on an endpoint that accepts Bearer auth MUST advertise
// the Bearer scheme so clients know how to retry (§3, first paragraph).
// When the failing credential WAS an OAuth bearer (sentinel err wrap
// or `ate2a_` prefix observed on the request), we additionally emit
// the §3.1 error params so MCP clients can trigger the OAuth re-flow.
//
// We deliberately do NOT distinguish "revoked" from "unknown token" in
// error_description: that distinction would be a token-existence
// oracle (an attacker with a candidate ate2a_ string could probe
// whether it once existed by reading the description). fosite's
// `ErrTokenExpired` is the one signal we expose because it fires
// from the strategy's expiry check, never from the storage layer, so
// "expired" doesn't leak existence.
//
// TODO(slice: RFC 9728 resource metadata): when the protected-resource
// metadata document lands, add `resource_metadata="<url>"` here so MCP
// clients can auto-discover the authorization server.
func (a *API) writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("WWW-Authenticate", a.authChallenge(r, err))
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

// authChallenge builds the RFC 6750 §3 WWW-Authenticate value for a 401 on a
// Bearer-accepting endpoint, given the request and the auth error that caused
// the rejection. Every 401 advertises the bare `Bearer realm="e2a"`; OAuth-
// bearer failures additionally carry the §3.1 error params (see writeAuthError
// docstring for the existence-oracle rationale). Shared by the legacy mux
// (writeAuthError) and the v1 surface (WWWAuthenticateChallenge) so both
// surfaces emit byte-identical challenges from one definition.
func (a *API) authChallenge(r *http.Request, err error) string {
	bearer := stripBearerScheme(r.Header.Get("Authorization"))
	isOAuthFailure := errors.Is(err, errOAuthBearerInvalid) || strings.HasPrefix(bearer, oauth.AccessTokenPrefix)
	if !isOAuthFailure {
		return `Bearer realm="e2a"`
	}
	desc := "the access token is invalid"
	if errors.Is(err, fosite.ErrTokenExpired) {
		desc = "the access token has expired"
	}
	return `Bearer realm="e2a", error="invalid_token", error_description="` + desc + `"`
}

// WWWAuthenticateChallenge is the v1-surface seam (internal/httpapi) for the
// RFC 6750 challenge. The legacy mux had the auth error in hand at the 401 site
// (writeAuthError); the v1 layer rejects via the canonical envelope and reaches
// this from a response wrapper that only knows the status was 401, so we re-run
// the same credential resolution to recover the typed error and classify the
// challenge identically (expired vs invalid). 401s are rare, so the extra
// resolution is cheap, and routing it through the one auth path keeps the
// surfaces from diverging.
func (a *API) WWWAuthenticateChallenge(r *http.Request) string {
	_, err := a.authenticatePrincipal(r)
	return a.authChallenge(r, err)
}

// resolveAgentForUser loads an agent by email address and verifies the user owns it.
func (a *API) resolveAgentForUser(r *http.Request, email string, user *identity.User) (*identity.AgentIdentity, error) {
	agent, err := a.store.GetAgentByEmail(r.Context(), email)
	if err != nil {
		return nil, fmt.Errorf("agent not found")
	}
	if agent.UserID != user.ID {
		return nil, fmt.Errorf("forbidden")
	}
	return agent, nil
}

// --- Domain Management ---

// dnsRecordCheck holds the per-record probe results for the verify
// endpoint. Values are "found" / "missing" — DKIM additionally supports
// "deferred" (per-domain DKIM not provisioned yet, legacy rows
// pre-migration 014) and "mismatch" (a DKIM record is published at the
// selector but doesn't match the issued key — almost always a truncated
// or mis-copied TXT).
type dnsRecordCheck struct {
	TXTFound bool
	MX       string
	SPF      string
	DKIM     string
}

// checkDomainRecords runs the three per-record probes plus the TXT
// ownership check. In dev mode, all checks short-circuit to "found" /
// true so domain verification flows can be exercised without real DNS.
//
// Probe semantics:
//   - TXT: any TXT record contains the verification token (ownership proof)
//   - MX: any MX record points at smtpDomain (mail routing)
//   - SPF: any apex TXT record begins with v=spf1 (case-insensitive) and
//     contains smtpDomain as a substring (case-insensitive). Advisory only —
//     SPF does not gate domain verification.
//   - DKIM: when dkimSelector + dkimPublicKey are present, looks up
//     "{selector}._domainkey.{domain}" and matches the stored public
//     key — "found" on a match, "mismatch" when a key is published but
//     doesn't match (see classifyDKIM), "missing" when none is present.
//     Domains without a stored keypair report "deferred" — these are
//     pre-migration rows that the next claim would key.
//
// DNSRecordCheck is the exported diagnostic from CheckDomainRecords.
type DNSRecordCheck struct {
	TXTFound bool
	MX       string
	SPF      string
	DKIM     string
}

// CheckDomainRecords is the exported seam over checkDomainRecords so the v1
// httpapi layer reuses the exact DNS-probe logic for domain verification.
func CheckDomainRecords(domain, smtpDomain, verificationToken, dkimSelector, dkimPublicKey string, production bool) DNSRecordCheck {
	c := checkDomainRecords(domain, smtpDomain, verificationToken, dkimSelector, dkimPublicKey, production)
	return DNSRecordCheck{TXTFound: c.TXTFound, MX: c.MX, SPF: c.SPF, DKIM: c.DKIM}
}

func checkDomainRecords(domain, smtpDomain, verificationToken, dkimSelector, dkimPublicKey string, production bool) dnsRecordCheck {
	if !production {
		dkimState := "deferred"
		if dkimSelector != "" && dkimPublicKey != "" {
			// Dev short-circuit treats a stored keypair as "found" so
			// the Get-started flow can show the DKIM row populated.
			dkimState = "found"
		}
		return dnsRecordCheck{
			TXTFound: true,
			MX:       "found",
			SPF:      "found",
			DKIM:     dkimState,
		}
	}
	check := dnsRecordCheck{DKIM: "deferred", MX: "missing", SPF: "missing"}

	// TXT ownership + SPF live in the same record set
	if txts, err := net.LookupTXT(domain); err == nil {
		for _, txt := range txts {
			if strings.Contains(txt, verificationToken) {
				check.TXTFound = true
			}
			if strings.HasPrefix(strings.ToLower(txt), "v=spf1") &&
				strings.Contains(strings.ToLower(txt), strings.ToLower(smtpDomain)) {
				check.SPF = "found"
			}
		}
	}

	if mxs, err := net.LookupMX(domain); err == nil {
		for _, mx := range mxs {
			if strings.EqualFold(strings.TrimSuffix(mx.Host, "."), smtpDomain) {
				check.MX = "found"
				break
			}
		}
	}

	// DKIM: only probe if we have a stored keypair for the domain. The
	// expected DNS name is "{selector}._domainkey.{domain}" with a
	// "v=DKIM1; k=rsa; p=<base64>" value. classifyDKIM matches the stored
	// public key, tolerating extra tags (s=, t=, etc.) operators paste.
	if dkimSelector != "" && dkimPublicKey != "" {
		check.DKIM = "missing"
		dkimName := fmt.Sprintf("%s._domainkey.%s", dkimSelector, domain)
		if txts, err := net.LookupTXT(dkimName); err == nil {
			check.DKIM = classifyDKIM(txts, dkimPublicKey)
		}
	}

	return check
}

// classifyDKIM maps the TXT records found at a domain's DKIM selector name
// to a probe state, given the public key we issued:
//
//   - "found"    — some record's p= payload equals the issued key.
//   - "mismatch" — a p= is published at the selector but none match. This is
//     almost always a TRUNCATED or mis-copied record: the DKIM TXT is ~400
//     chars and is frequently clipped on publish (split wrong, pasted short,
//     or transcribed from a UI). Reporting it distinctly is what lets verify
//     tell the user to re-publish the FULL value, instead of the misleading
//     "missing → pending forever" that a silent truncation otherwise looks
//     like.
//   - "missing"  — no p= payload present at the selector name at all.
//
// A match wins over a mismatch (a domain may briefly serve both an old and a
// new key during rotation).
func classifyDKIM(txts []string, expectedPublicKey string) string {
	state := "missing"
	for _, txt := range txts {
		got := dkim.ExtractPublicKeyFromTXT(txt)
		if got == "" {
			continue
		}
		if got == expectedPublicKey {
			return "found"
		}
		state = "mismatch"
	}
	return state
}

// holdForApproval persists a fully composed outbound SendRequest as a
// pending_review message and writes a 202 response. It is the shared
// branch taken by handleSendEmail, handleReplyToMessage, and
// handleSendTestEmail when outbound screening holds the message for review.
//
// replyToEmailMessageID is the inbound Message-ID being replied to, or "".
// msgType is one of "send", "reply", "test", or "forward".
// errHoldAttachments is returned by HoldForApprovalCore when the attachments
// fail to serialize, so callers can map it to the same 500 the legacy handler
// produced ("failed to serialize attachments") vs the create-failure 500.
var errHoldAttachments = errors.New("failed to serialize attachments")

// HoldForApprovalCore is the HTTP-free core of the HITL hold: it persists the
// pending outbound message, fires the async reviewer notification, and
// publishes the pending-approval event, returning the held message. Both the
// legacy handler and the v1 httpapi layer call it so there is exactly one
// hold-and-notify path (api-v1-redesign — outbound extraction).
func (a *API) HoldForApprovalCore(ctx context.Context, agent *identity.AgentIdentity, req outbound.SendRequest, msgType, replyToEmailMessageID string) (*identity.Message, error) {
	var attachmentsJSON []byte
	if len(req.Attachments) > 0 {
		b, err := json.Marshal(req.Attachments)
		if err != nil {
			log.Printf("[api] hitl: marshal attachments: %v", err)
			return nil, errHoldAttachments
		}
		attachmentsJSON = b
	}

	var msg *identity.Message
	if a.notifyEnq != nil && !agent.SuppressNotifications {
		// Durable notification path: persist the pending_review row AND enqueue its
		// approval-notification job (QueueNotify) in ONE transaction, then stamp the
		// job id, so the reviewer's email is never lost on a crash between the 202
		// and the send (docs/design/hitl-notify-river.md). Mirrors the outbound
		// accept-tx. A same-tx enqueue failure fails the whole hold (500) — the same
		// DB fault would have failed the message insert anyway.
		if txErr := a.store.WithTx(ctx, func(tx pgx.Tx) error {
			m, err := a.store.CreatePendingOutboundMessageManagedTx(
				ctx, tx, agent.ID,
				req.To, req.CC, req.BCC,
				req.Subject, req.Body, req.HTMLBody,
				attachmentsJSON,
				msgType, req.ConversationID, replyToEmailMessageID, req.ReplyTo,
				agent.HITLTTLSeconds, req.Unsubscribe != nil,
			)
			if err != nil {
				return err
			}
			jobID, err := a.notifyEnq.EnqueueNotifyTx(ctx, tx, m.ID)
			if err != nil {
				return err
			}
			if err := a.store.StampNotifyJobIDTx(ctx, tx, m.ID, jobID); err != nil {
				return err
			}
			msg = m
			return nil
		}); txErr != nil {
			log.Printf("[api] hitl: create+enqueue pending message: agent=%s err=%v", agent.ID, txErr)
			return nil, txErr
		}
	} else {
		// No notifier configured: plain hold, no notification (behavior unchanged).
		m, err := a.store.CreatePendingOutboundMessageManaged(
			ctx, agent.ID,
			req.To, req.CC, req.BCC,
			req.Subject, req.Body, req.HTMLBody,
			attachmentsJSON,
			msgType, req.ConversationID, replyToEmailMessageID, req.ReplyTo,
			agent.HITLTTLSeconds, req.Unsubscribe != nil,
		)
		if err != nil {
			log.Printf("[api] hitl: create pending message: agent=%s err=%v", agent.ID, err)
			return nil, err
		}
		msg = m
	}

	slug, _, _ := strings.Cut(agent.EmailAddress(), "@")
	log.Printf("[mail:%s] dir=outbound type=%s status=%s from=%s to_count=%d to_domains=%v slug=%s conv_id=%s subject_len=%d approval_expires_at=%s",
		msg.ID, msgType, msg.Status, agent.EmailAddress(), len(req.To), logredact.AddressDomains(req.To), slug, req.ConversationID, utf8.RuneCountInString(req.Subject), msg.ApprovalExpiresAt.Format(time.RFC3339))

	a.publishPendingApproval(ctx, a.buildPendingApprovalEvent(agent, msg, req, msgType), msg.ID)
	return msg, nil
}

// OutboundResult is the HTTP-free outcome of DeliverOutbound.
type OutboundResult struct {
	Held              bool
	PendingMessageID  string
	ApprovalExpiresAt *time.Time
	MessageID         string // the e2a msg_ id (GET-able) for every send method
	ProviderMessageID string // provider/SES id when sent via smtp
	SentAs            string // "own_address" | "relay" (decision 4)
	Method            string // "smtp" | "loopback"
	// Status is the send progression the wire maps to `status`. Empty on the
	// synchronous path (the caller renders "sent"); "accepted" on the async path
	// (durably persisted + queued; the terminal outcome arrives via email.sent /
	// email.failed); "scheduled" when the send was deferred to a future instant
	// (ScheduledAt). See async-message-pipeline.md, slice C.
	Status string
	// ScheduledAt is set only when Status=="scheduled": the future instant the
	// queued send will be submitted. It must be carried on BOTH the live return
	// and the in-transaction idempotency completion so a keyed replay renders the
	// identical scheduled view (never a bare "accepted").
	ScheduledAt *time.Time
}

// OutboundError carries an HTTP status + message so both the legacy handler
// (http.Error) and the v1 httpapi layer (error envelope) render it
// consistently. Code is the machine-branchable code the v1 layer uses.
type OutboundError struct {
	Status int
	Code   string
	Msg    string
	// Details carries optional code-specific structured context across the
	// agent/httpapi boundary. It stays transport-neutral to avoid a package cycle.
	Details map[string]any
}

func (e *OutboundError) Error() string { return e.Msg }

// suppressionLister is the narrow store surface the suppression checks read —
// an interface so the check core is unit-testable without Postgres.
type suppressionLister interface {
	EffectiveSuppressions(ctx context.Context, userID, agentID string, addrs []string) ([]string, error)
}

// checkSuppressionCore rejects a send when any recipient of the request's
// FULL To/CC/BCC set is in the effective union of the owning account's global
// list and the exact sending agent's list. The store normalizes both sides, so
// case/whitespace differences still match. Returns recipient_suppressed 422.
//
// failClosed selects the store-error posture:
//   - false (accept-time direct send): fail OPEN — a suppression-DB hiccup
//     must not block legitimate mail at intake; the async pipeline's
//     pre-provider guard (internal/outboundsend) re-checks before any SES
//     I/O and backstops the published refusal promise.
//   - true (approval paths): fail CLOSED with a retryable internal_error —
//     approving is a human-driven, freely retryable action, and silently
//     sending on a store error would break the public "addresses e2a will
//     refuse to send to" contract (GET /v1/account/suppressions). The hold
//     stays pending_review.
func checkSuppressionCore(ctx context.Context, store suppressionLister, userID, agentID string, req outbound.SendRequest, failClosed bool) *OutboundError {
	addrs := make([]string, 0, len(req.To)+len(req.CC)+len(req.BCC))
	addrs = append(addrs, req.To...)
	addrs = append(addrs, req.CC...)
	addrs = append(addrs, req.BCC...)
	if len(addrs) == 0 {
		return nil
	}
	suppressed, err := store.EffectiveSuppressions(ctx, userID, agentID, addrs)
	if err != nil {
		if failClosed {
			log.Printf("[api] suppression check failed (refusing approval): %v", err)
			return &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "suppression check unavailable — the message was left pending; retry"}
		}
		log.Printf("[api] suppression check failed (allowing send): %v", err)
		return nil
	}
	if len(suppressed) > 0 {
		return &OutboundError{Status: http.StatusUnprocessableEntity, Code: "recipient_suppressed", Msg: "recipient(s) on the suppression list: " + strings.Join(suppressed, ", ") + outbound.SuppressionRemediation(agentID)}
	}
	return nil
}

// checkSuppression is the accept-time (direct send) check: fail-open on a
// store error (see checkSuppressionCore for the rationale + backstop).
func (a *API) checkSuppression(ctx context.Context, userID, agentID string, req outbound.SendRequest) *OutboundError {
	return checkSuppressionCore(ctx, a.store, userID, agentID, req, false)
}

// checkSuppressionStrict is the approval-time check (human approve + magic
// link): fail-closed on a store error, leaving the hold pending_review.
func (a *API) checkSuppressionStrict(ctx context.Context, userID, agentID string, req outbound.SendRequest) *OutboundError {
	return checkSuppressionCore(ctx, a.store, userID, agentID, req, true)
}

// DeliverOutbound is the shared send/reply/forward delivery tail, HTTP-free:
// HITL hold (HoldForApprovalCore), else self-send loopback, else SES send +
// record outbound + publish sent event. The caller has already authed,
// resolved + ownership-checked the agent, rate-limited, domain-verified, run
// the message-send cap, and built the SendRequest with its final Subject /
// ConversationID. Both the legacy handlers and the v1 layer call this so the
// live delivery path exists exactly once (api-v1-redesign — outbound
// extraction). On a nil-error return the side effect has committed, so the
// idempotency key must be Completed (cached), never Released.
// resolveOutboundConversationID picks the thread id for an outbound message, in
// precedence order (#328):
//  1. an explicit caller-supplied id wins;
//  2. a reply inherits the conversation of the message it answers (referenced),
//     so a multi-turn thread stays grouped — a forward does NOT inherit, since a
//     forward starts a new thread;
//  3. otherwise a fresh anchor is minted so this message becomes the root the
//     relay's In-Reply-To lookup threads later inbound replies onto. Without an
//     anchor an external reply recovers an empty id and the thread fragments.
func resolveOutboundConversationID(explicit, msgType string, referenced *identity.Message) string {
	if explicit != "" {
		return explicit
	}
	if msgType == "reply" && referenced != nil && referenced.ConversationID != "" {
		return referenced.ConversationID
	}
	return identity.NewConversationID()
}

func (a *API) DeliverOutbound(ctx context.Context, user *identity.User, agent *identity.AgentIdentity, req outbound.SendRequest, msgType, replyToEmailMessageID string, referenced *identity.Message, idemCompleteTx AcceptIdemCompleter) (*OutboundResult, *OutboundError) {
	// Validate the canonical envelope before screening can durably hold the
	// draft, but deliberately do not mint while it is pending human review.
	if uerr := prepareManagedUnsubscribe(ctx, a.unsubscribeIssuer, a.fromDomain, user.ID, agent, &req, false); uerr != nil {
		return nil, uerr
	}
	// Suppression enforcement (decision 9 / Slice 4b): fail fast if any
	// recipient is on this tenant's suppression list. Enforced fresh on every
	// attempt and NOT cached under the idempotency key (it's a clearable state,
	// released like every other error).
	if supErr := a.checkSuppression(ctx, user.ID, agent.ID, req); supErr != nil {
		return nil, supErr
	}

	// Conversation threading (#328): resolve the thread id once, here, so every
	// downstream use — the X-E2A-Conversation-Id header (compose), the
	// held-for-review row, the stored outbound row, and the email.sent event — sees
	// the same value.
	req.ConversationID = resolveOutboundConversationID(req.ConversationID, msgType, referenced)

	// Outbound screening (Slice 5): the recipient gate (outbound_policy) + content
	// scan (outbound_scan) combine into one applied action. block ⇒ refuse;
	// review ⇒ hold; flag ⇒ send + annotate; allow ⇒ send.
	verdict := a.screenOutbound(ctx, agent, req)
	if verdict.Block() {
		// Egress block: refuse to the caller. No message row is persisted; the
		// audit lives in protection_events keyed to a STABLE soft-ref id so a
		// retried block doesn't write duplicate audit rows / events.
		a.auditRowless(ctx, agent, blockAuditID(agent.ID, req), req, verdict)
		a.emitBlockedOutbound(agent, blockAuditID(agent.ID, req), req, verdict)
		return nil, &OutboundError{Status: http.StatusForbidden, Code: "blocked_by_policy", Msg: "message blocked by outbound policy"}
	}

	// Hold when outbound screening says review. The outbound recipient gate
	// (outbound_policy: allowlist+review is the trust-ramp) + content scan now
	// fully own the hold decision — hitl_enabled/hitl_mode were retired in
	// Slice 5b (their behavior is mapped forward by migration 042).
	if verdict.Review() {
		msg, err := a.HoldForApprovalCore(ctx, agent, req, msgType, replyToEmailMessageID)
		if err != nil {
			if errors.Is(err, errHoldAttachments) {
				return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "failed to serialize attachments"}
			}
			return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "failed to hold message for approval"}
		}
		// Tag the held row + audit only when screening drove the hold (a pure
		// legacy-HITL hold carries no screening verdict).
		if verdict.Annotate() {
			a.annotateAndAudit(ctx, agent, msg.ID, req, verdict)
		}
		return &OutboundResult{Held: true, PendingMessageID: msg.ID, ApprovalExpiresAt: msg.ApprovalExpiresAt}, nil
	}
	if uerr := prepareManagedUnsubscribe(ctx, a.unsubscribeIssuer, a.fromDomain, user.ID, agent, &req, true); uerr != nil {
		return nil, uerr
	}

	// Usage is metered AFTER the side effect + persist, never before (slice 1
	// §7.2 / async-send-contract.md): billing must not run ahead of a durable
	// message row, or a crash between meter and persist bills an invisible send.
	if isSelfSend(req, agent.EmailAddress()) {
		// Self-send is an immediate in-process loopback; the scheduling path
		// (River ScheduledAt) never runs for it, so a future send_at would be
		// silently dropped. Reject it rather than deliver early against intent.
		if req.ScheduledAt != nil {
			return nil, &OutboundError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: "scheduled send (send_at) is not supported when delivering to the agent's own address"}
		}
		outMsg, err := a.performSelfSend(ctx, agent, req, msgType, idemCompleteTx)
		if err != nil {
			log.Printf("[api] self-send failed: agent=%s error=%v", agent.EmailAddress(), err)
			return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "self-send failed"}
		}
		// Meter both durable copies after the local delivery commits. Never block
		// on metering; the pre-check remains the quota gate.
		a.recordLoopbackUsage(ctx, user.ID, agent)
		slug, _, _ := strings.Cut(agent.EmailAddress(), "@")
		log.Printf("[mail:%s] dir=outbound type=%s method=loopback status=sent from=%s to=%s slug=%s conv_id=%s subject_len=%d", outMsg.ID, msgType, agent.EmailAddress(), agent.EmailAddress(), slug, req.ConversationID, utf8.RuneCountInString(req.Subject))
		return &OutboundResult{MessageID: outMsg.ID, SentAs: "own_address", Method: "loopback"}, nil
	}

	// Queue-first accept path. We are past self-send / hold / block, so this is the
	// real external-delivery path. Compose
	// once and durably persist the message (delivery_status='accepted') + enqueue
	// the outbound_send job + complete the idempotency key, all in ONE transaction:
	// an accepted row can never exist without a send job, and a retry replays the
	// cached 'accepted' response rather than re-persisting. The River worker
	// (internal/outboundsend) then submits to SES and records the terminal outcome
	// (email.sent / email.failed + metering). Missing queue wiring is a startup bug;
	// fail closed here as defense in depth and never submit inline.
	if a.outboundEnq == nil {
		log.Printf("[api] outbound queue unavailable: agent=%s to_count=%d to_domains=%v", agent.Domain, len(req.To), logredact.AddressDomains(req.To))
		return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "outbound delivery queue unavailable"}
	}
	comp, cerr := a.sender.ComposeForAccept(agent, req)
	if cerr != nil {
		if sizeErr := composedSizeOutboundError(cerr); sizeErr != nil {
			return nil, sizeErr
		}
		if outbound.IsValidationError(cerr) {
			return nil, &OutboundError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: cerr.Error()}
		}
		log.Printf("[api] async compose failed: agent=%s to_count=%d to_domains=%v error=%v", agent.Domain, len(req.To), logredact.AddressDomains(req.To), cerr)
		return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: fmt.Sprintf("compose failed: %v", cerr)}
	}
	// Scheduled send (migration 079): a future req.ScheduledAt defers WHEN the
	// send job runs (river ScheduledAt) — the message is still accepted + queued
	// atomically here, and the row stays delivery_status='accepted'. The edge has
	// already validated that ScheduledAt, when set, is in the future and within
	// the max horizon; a nil/past value is an ordinary immediate send.
	scheduledAt := req.ScheduledAt
	acceptStatus := "accepted"
	if scheduledAt != nil {
		acceptStatus = "scheduled"
	}
	var accepted *identity.Message
	// Crash boundary:
	//   - Before Commit returns, WithTx rolls back the message, River job, and
	//     idempotency completion together; no accepted delivery is left behind.
	//   - After Commit returns, all three are durable. If the process dies before
	//     the HTTP response is written, River still delivers the message and a
	//     byte-identical retry with the same Idempotency-Key replays the original
	//     202/message id. Without a key, that ambiguous retry is a new request and
	//     may enqueue a duplicate (the public guarantee is at-least-once).
	if txErr := a.store.WithTx(ctx, func(tx pgx.Tx) error {
		msg, err := a.store.CreateOutboundMessageTx(ctx, tx, agent.ID, comp.To, comp.CC, comp.BCC, req.Subject, msgType, comp.Method, "", req.ConversationID, comp.Raw, "accepted", comp.EnvelopeFrom, comp.SentAs)
		if err != nil {
			return err
		}
		// Stamp the schedule marker + enqueue the job to run at that instant. Both
		// happen in this same tx, so scheduled_at and the River job's ScheduledAt
		// are committed together with the message.
		var jobID int64
		if scheduledAt != nil {
			if err := a.store.StampScheduledAtTx(ctx, tx, msg.ID, *scheduledAt); err != nil {
				return err
			}
			jobID, err = a.outboundEnq.EnqueueScheduledSendTx(ctx, tx, msg.ID, *scheduledAt)
		} else {
			jobID, err = a.outboundEnq.EnqueueSendTx(ctx, tx, msg.ID)
		}
		if err != nil {
			return err
		}
		if err := a.store.StampSendJobIDTx(ctx, tx, msg.ID, jobID); err != nil {
			return err
		}
		if idemCompleteTx != nil {
			if err := idemCompleteTx(ctx, tx, &OutboundResult{MessageID: msg.ID, Status: acceptStatus, ScheduledAt: scheduledAt}); err != nil {
				return err
			}
		}
		accepted = msg
		return nil
	}); txErr != nil {
		log.Printf("[api] async accept tx failed: agent=%s to_count=%d to_domains=%v error=%v", agent.Domain, len(req.To), logredact.AddressDomains(req.To), txErr)
		return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "failed to accept message for send"}
	}
	if verdict.Annotate() {
		a.annotateAndAudit(ctx, agent, accepted.ID, req, verdict)
	}
	slug, _, _ := strings.Cut(agent.EmailAddress(), "@")
	scheduledLog := "-"
	if scheduledAt != nil {
		scheduledLog = scheduledAt.UTC().Format(time.RFC3339)
	}
	log.Printf("[mail:%s] dir=outbound type=%s status=%s scheduled_at=%s from=%s to_count=%d to_domains=%v slug=%s conv_id=%s subject_len=%d", accepted.ID, msgType, acceptStatus, scheduledLog, agent.EmailAddress(), len(comp.To), logredact.AddressDomains(comp.To), slug, req.ConversationID, utf8.RuneCountInString(req.Subject))
	return &OutboundResult{MessageID: accepted.ID, Status: acceptStatus, ScheduledAt: scheduledAt, SentAs: comp.SentAs, Method: comp.Method}, nil
}

// SendTestCore accepts (or HITL-holds) a platform test email to the agent's
// own address. HTTP-free; shared by the legacy handler and the v1 layer. The
// caller has already authed, resolved + owned the agent, domain-verified,
// rate-limited, and run the message-send cap.
//
// The message is PLATFORM-originated (From/envelope = noreply@<from_domain>)
// and traverses the real external SMTP → inbound route back to the agent's
// public address — that round-trip is the endpoint's purpose, so it must never
// route through the agent self-send loopback. Delivery is queue-first like
// every other send: a durable msg_ resource + outbound_send job are committed
// before any provider I/O (status=accepted), and the terminal outcome arrives
// via GET /v1/messages/{id} and the email.sent / email.failed events.
func (a *API) SendTestCore(ctx context.Context, agent *identity.AgentIdentity) (*OutboundResult, *OutboundError) {
	to := []string{agent.EmailAddress()}
	subject := "Test email from e2a"
	body := fmt.Sprintf("This is a test email for %s.\n\nYour agent is set up and ready to receive emails.", agent.EmailAddress())
	testReq := outbound.SendRequest{To: to, Subject: subject, Body: body}

	// Suppression enforcement — the same recipient gate DeliverOutbound runs
	// (decision 9): a suppressed agent address must never be submitted.
	if supErr := a.checkSuppression(ctx, agent.UserID, agent.ID, testReq); supErr != nil {
		return nil, supErr
	}

	// Mint the thread anchor exactly like DeliverOutbound does for a fresh send
	// (#328), so the inbound copy — arriving over real SMTP with the
	// X-E2A-Conversation-ID header — threads onto this outbound row.
	testReq.ConversationID = identity.NewConversationID()

	// A platform test is a normal outbound send through the same screening:
	// block ⇒ refuse, review ⇒ hold, flag ⇒ send + annotate.
	verdict := a.screenOutbound(ctx, agent, testReq)
	if verdict.Block() {
		a.auditRowless(ctx, agent, blockAuditID(agent.ID, testReq), testReq, verdict)
		a.emitBlockedOutbound(agent, blockAuditID(agent.ID, testReq), testReq, verdict)
		return nil, &OutboundError{Status: http.StatusForbidden, Code: "blocked_by_policy", Msg: "test message blocked by outbound policy"}
	}
	if verdict.Review() {
		msg, err := a.HoldForApprovalCore(ctx, agent, testReq, "test", "")
		if err != nil {
			return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "failed to hold message for approval"}
		}
		if verdict.Annotate() {
			a.annotateAndAudit(ctx, agent, msg.ID, testReq, verdict)
		}
		return &OutboundResult{Held: true, PendingMessageID: msg.ID, ApprovalExpiresAt: msg.ApprovalExpiresAt}, nil
	}

	res, oerr := a.acceptPlatformSend(ctx, agent, testReq, "test")
	if oerr != nil {
		return nil, oerr
	}
	if verdict.Annotate() {
		a.annotateAndAudit(ctx, agent, res.MessageID, testReq, verdict)
	}
	return res, nil
}

// acceptPlatformSend durably persists + enqueues a PLATFORM-originated
// outbound message (From/envelope = noreply@<from_domain>, never an agent
// identity) on the same queue-first pipeline as DeliverOutbound's external
// branch: message row (delivery_status='accepted') + outbound_send job +
// job-id stamp in ONE transaction, with provider I/O strictly in the River
// worker (internal/outboundsend), which records the terminal outcome
// (email.sent / email.failed) and meters on success.
//
// It exists for the agent test send (msgType="test"), whose recipient is the
// agent's OWN address: the agent-identity compose strips the agent's own
// address ("no valid recipients") and the self-send predicate would reroute
// delivery to local loopback — either would defeat the endpoint's purpose of
// exercising the real external SMTP → inbound route. Callers run suppression
// + screening first; this is the smallest accept seam, not a policy layer.
func (a *API) acceptPlatformSend(ctx context.Context, agent *identity.AgentIdentity, req outbound.SendRequest, msgType string) (*OutboundResult, *OutboundError) {
	// Missing queue wiring is a startup bug; fail closed before provider I/O
	// and never submit inline (same contract as DeliverOutbound).
	if a.outboundEnq == nil {
		log.Printf("[api] outbound queue unavailable: agent=%s to_count=%d to_domains=%v (platform %s)", agent.Domain, len(req.To), logredact.AddressDomains(req.To), msgType)
		return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "outbound delivery queue unavailable"}
	}
	comp, cerr := a.sender.ComposePlatformForAccept(req)
	if cerr != nil {
		if sizeErr := composedSizeOutboundError(cerr); sizeErr != nil {
			return nil, sizeErr
		}
		if outbound.IsValidationError(cerr) {
			return nil, &OutboundError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: cerr.Error()}
		}
		log.Printf("[api] platform compose failed: agent=%s to_count=%d to_domains=%v error=%v", agent.Domain, len(req.To), logredact.AddressDomains(req.To), cerr)
		return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: fmt.Sprintf("compose failed: %v", cerr)}
	}
	var accepted *identity.Message
	if txErr := a.store.WithTx(ctx, func(tx pgx.Tx) error {
		msg, err := a.store.CreateOutboundMessageTx(ctx, tx, agent.ID, comp.To, comp.CC, comp.BCC, req.Subject, msgType, comp.Method, "", req.ConversationID, comp.Raw, "accepted", comp.EnvelopeFrom, comp.SentAs)
		if err != nil {
			return err
		}
		jobID, err := a.outboundEnq.EnqueueSendTx(ctx, tx, msg.ID)
		if err != nil {
			return err
		}
		if err := a.store.StampSendJobIDTx(ctx, tx, msg.ID, jobID); err != nil {
			return err
		}
		accepted = msg
		return nil
	}); txErr != nil {
		log.Printf("[api] platform accept tx failed: agent=%s to_count=%d to_domains=%v error=%v", agent.Domain, len(req.To), logredact.AddressDomains(req.To), txErr)
		return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "failed to accept message for send"}
	}
	slug, _, _ := strings.Cut(agent.EmailAddress(), "@")
	log.Printf("[mail:%s] dir=outbound type=%s status=accepted from=%s to_count=%d to_domains=%v slug=%s conv_id=%s subject_len=%d platform_originated=true",
		accepted.ID, msgType, comp.EnvelopeFrom, len(comp.To), logredact.AddressDomains(comp.To), slug, req.ConversationID, utf8.RuneCountInString(req.Subject))
	return &OutboundResult{MessageID: accepted.ID, Status: "accepted", SentAs: comp.SentAs, Method: comp.Method}, nil
}

// --- Send Email ---

// ForwardRequest is the JSON body for /v1/agents/{email}/messages/{id}/forward.
type ForwardRequest struct {
	To             []string              `json:"to"`
	CC             []string              `json:"cc,omitempty"`
	BCC            []string              `json:"bcc,omitempty"`
	Body           string                `json:"text,omitempty"`
	HTMLBody       string                `json:"html,omitempty"`
	ConversationID string                `json:"conversation_id,omitempty"`
	Attachments    []outbound.Attachment `json:"attachments,omitempty"`
}

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (a *API) handleFeedback(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if ok, retryAfter := a.feedbackLimit.AllowWithRetryAfter(ip); !ok {
		writeTooManyRequests(w, retryAfter, "rate limit exceeded — max 10 feedback submissions per hour per IP")
		return
	}

	var req struct {
		Email    string `json:"email"`
		Category string `json:"category"`
		Message  string `json:"message"`
	}
	if err := readJSON(w, r, &req, maxRequestBytesSmall); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}
	if len([]rune(req.Message)) > 5000 {
		http.Error(w, "message too long (max 5000 characters)", http.StatusBadRequest)
		return
	}
	if len(req.Email) > 254 {
		http.Error(w, "email too long", http.StatusBadRequest)
		return
	}
	if req.Category == "" {
		req.Category = "general"
	}
	if req.Category != "bug" && req.Category != "feature" && req.Category != "general" {
		http.Error(w, "category must be bug, feature, or general", http.StatusBadRequest)
		return
	}

	// If user is authenticated, use their email
	if a.userAuth != nil {
		if user := a.userAuth.AuthenticateRequest(r); user != nil {
			if req.Email == "" {
				req.Email = user.Email
			}
		}
	}

	// Create GitHub issue
	labelMap := map[string]string{
		"bug":     "bug",
		"feature": "enhancement",
		"general": "feedback",
	}
	label := labelMap[req.Category]

	// Sanitize user input to prevent GitHub @mention spam and Markdown injection
	sanitize := func(s string) string {
		return strings.ReplaceAll(s, "@", "@\u200B") // zero-width space breaks @mentions
	}

	title := fmt.Sprintf("[%s] %s", req.Category, truncate(sanitize(req.Message), 80))

	body := sanitize(req.Message)
	if req.Email != "" {
		body += fmt.Sprintf("\n\n---\nSubmitted by: `%s`", sanitize(req.Email))
	}

	// Fan out to every configured delivery channel. A submission succeeds if
	// AT LEAST ONE channel delivers — one broken integration (e.g. an expired
	// GitHub token) must not lose feedback while another channel still works.
	attempted, delivered := 0, 0

	// Channel 1: GitHub issue. Credential resolution (GitHub App preferred,
	// PAT fallback) lives in feedback_github.go.
	issueURL := ""
	ghCtx, cancelGitHub := context.WithTimeout(r.Context(), feedbackGitHubTimeout)
	ghClient, ghErr := feedbackGitHubClient(ghCtx)
	switch {
	case ghErr != nil:
		attempted++
		log.Printf("feedback: github auth: %v", ghErr)
	case ghClient != nil:
		attempted++
		ghRepo := os.Getenv("GITHUB_FEEDBACK_REPO")
		if ghRepo == "" {
			ghRepo = "tokencanopy/e2a"
		}
		parts := strings.SplitN(ghRepo, "/", 2)
		if len(parts) != 2 {
			log.Printf("feedback: invalid GITHUB_FEEDBACK_REPO: %s", ghRepo)
		} else {
			issue, _, err := ghClient.Issues.Create(ghCtx, parts[0], parts[1], &github.IssueRequest{
				Title:  github.Ptr(title),
				Body:   github.Ptr(body),
				Labels: &[]string{label},
			})
			if err != nil {
				log.Printf("feedback: GitHub API error: %v", err)
			} else {
				delivered++
				issueURL = issue.GetHTMLURL()
			}
		}
	}
	cancelGitHub()

	// Channel 2: notification email via the platform relay (hitlnotify-style
	// compose + direct submit; the durable outbound pipeline requires an agent
	// identity, which feedback has none). Recipients are deployment config via
	// FEEDBACK_NOTIFY_TO / FEEDBACK_NOTIFY_CC (comma-separated) — empty means
	// the channel is off (self-host default).
	notifyTo := splitFeedbackAddrs(os.Getenv("FEEDBACK_NOTIFY_TO"))
	notifyCC := splitFeedbackAddrs(os.Getenv("FEEDBACK_NOTIFY_CC"))
	if len(notifyTo) > 0 || len(notifyCC) > 0 {
		attempted++
		// Surface the GitHub outcome in the email body: when the issue
		// channel breaks, the notification doubles as the alert for it.
		ghNote := ""
		if ghClient != nil || ghErr != nil {
			if issueURL != "" {
				ghNote = "GitHub issue: " + issueURL
			} else {
				ghNote = "GitHub issue: creation FAILED (see server logs)"
			}
		}
		emailCtx, cancelEmail := context.WithTimeout(r.Context(), feedbackEmailTimeout)
		err := a.sendFeedbackEmail(emailCtx, title, req.Category, req.Message, req.Email, ghNote, notifyTo, notifyCC)
		cancelEmail()
		if err != nil {
			log.Printf("feedback: notify email failed: %v", err)
		} else {
			delivered++
		}
	}

	if attempted == 0 {
		// No delivery channel configured — log-only graceful fallback.
		safeMsg := logredact.Truncate(strings.ReplaceAll(req.Message, "\n", " "), 60)
		log.Printf("feedback: no delivery channel configured, logging only: [%s] message_len=%d preview=%q", req.Category, utf8.RuneCountInString(req.Message), safeMsg)
	} else if delivered == 0 {
		http.Error(w, "failed to submit feedback", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]string{"status": "ok"})
}

// sendFeedbackEmail emails a copy of a feedback submission to the configured
// recipients through the platform relay (From: "e2a" <noreply@<from_domain>>).
// Reply-To carries the submitter's address when given, so replying to the
// notification reaches them directly; compose-layer header sanitization
// neutralizes any CR/LF in that user-controlled value.
func (a *API) sendFeedbackEmail(ctx context.Context, title, category, message, submitterEmail, ghNote string, to, cc []string) error {
	if a.smtpRelay == nil || !a.smtpRelay.Configured() || a.fromDomain == "" {
		return fmt.Errorf("outbound SMTP relay not configured")
	}

	from := outbound.PlatformEnvelopeFrom(a.fromDomain)
	headerFrom := fmt.Sprintf("%q <%s>", outbound.PlatformDisplayName, from)

	var b strings.Builder
	fmt.Fprintf(&b, "Category: %s\n", category)
	if submitterEmail != "" {
		fmt.Fprintf(&b, "Submitted by: %s\n", submitterEmail)
	}
	if ghNote != "" {
		fmt.Fprintf(&b, "%s\n", ghNote)
	}
	fmt.Fprintf(&b, "\n%s\n", message)

	raw, err := outbound.ComposeMessage(headerFrom, to, cc, "[e2a feedback] "+title, b.String(), "text/plain", "", nil, a.fromDomain, submitterEmail, "")
	if err != nil {
		return fmt.Errorf("compose: %w", err)
	}

	rcpts := make([]string, 0, len(to)+len(cc))
	rcpts = append(rcpts, to...)
	rcpts = append(rcpts, cc...)

	// Send (not SendOnce) — no job queue owns retries for this path, so the
	// relay's own transient-4xx backoff is the only retry envelope.
	if _, err := a.smtpRelay.SendWithContext(ctx, from, rcpts, raw); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}

// splitFeedbackAddrs parses a comma-separated address list from env config,
// trimming whitespace and dropping empties.
func splitFeedbackAddrs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func truncate(s string, maxLen int) string {
	// Truncate to first line, then to maxLen (rune-safe)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	runes := []rune(s)
	if maxLen <= 3 {
		return "..."
	}
	if len(runes) > maxLen {
		return string(runes[:maxLen-3]) + "..."
	}
	return s
}

// validateConversationID rejects values containing CR or LF. The
// composer treats conversation_id as a passthrough for the
// X-E2A-Conversation-ID header; allowing CRLF would let any
// authenticated caller smuggle additional headers (Bcc, fake
// DKIM-Signature, body smuggling) into the outbound MIME message.
// Defense-in-depth: the composer also strips CRLF, but rejecting
// at the boundary gives the caller a clear 400 instead of silently
// neutralising their input.
func validateConversationID(id string) error {
	if strings.ContainsAny(id, "\r\n") {
		return errors.New("conversation_id must not contain CR or LF")
	}
	return nil
}

// Label validation constants. Public so tests can reference them.
const (
	// MaxLabelLength bounds a single label's length to keep the GIN
	// index entries small and prevent multi-KB tags from being smuggled
	// into the array.
	MaxLabelLength = 64

	// MaxLabelsPerOp caps the per-request add/remove list size.
	// Modeled on Gmail's 100/100 cap. Bigger than AgentMail's
	// (unspecified) but defensive enough that one PATCH can't try
	// to set thousands of labels.
	MaxLabelsPerOp = 50

	// LabelSystemPrefix marks server-applied system labels. User
	// writes that try to set a label starting with this prefix are
	// rejected with 400 — the namespace is reserved so future system
	// labels (auto-tagged, hitl-approved, …) don't collide with user
	// tags.
	LabelSystemPrefix = "e2a:"
)

// normalizeAndValidateLabel canonicalizes a single label and rejects it
// if it would violate the labels invariants:
//   - lowercase
//   - charset `[a-z0-9:_-]+` (colon allowed for namespacing, but only
//     the server may set `e2a:*`)
//   - 1..MaxLabelLength chars after trimming
//
// Returns the normalized form on success. Lowercasing is the only
// transformation — colons / dashes / underscores stay as-is so a label
// from a query param is byte-identical to the same label set via PATCH.
func normalizeAndValidateLabel(raw string, allowSystemPrefix bool) (string, error) {
	l := strings.ToLower(strings.TrimSpace(raw))
	if l == "" {
		return "", errors.New("label must not be empty")
	}
	if len(l) > MaxLabelLength {
		return "", fmt.Errorf("label too long (max %d chars)", MaxLabelLength)
	}
	for _, r := range l {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == ':':
		default:
			return "", fmt.Errorf("label %q has invalid character; allowed: a-z 0-9 : - _", l)
		}
	}
	if !allowSystemPrefix && strings.HasPrefix(l, LabelSystemPrefix) {
		return "", fmt.Errorf("labels starting with %q are reserved for system use", LabelSystemPrefix)
	}
	return l, nil
}

// NormalizeAndValidateLabelList runs each entry through
// normalizeAndValidateLabel, dedups within the slice, and rejects if
// the slice is empty after trimming or exceeds the per-op cap. The
// `op` argument is used only to shape the error message.
func NormalizeAndValidateLabelList(raw []string, op string) ([]string, error) {
	if len(raw) > MaxLabelsPerOp {
		return nil, fmt.Errorf("%s_labels exceeds per-request cap of %d", op, MaxLabelsPerOp)
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		l, err := normalizeAndValidateLabel(r, false)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	return out, nil
}

// validateRecipients ensures every entry in the joined To/CC/BCC slices
// is a syntactically valid RFC 5322 address. We use net/mail.ParseAddress
// rather than a custom regex because it handles bare local@domain,
// display-name forms ("Bob Smith <bob@x.com>"), quoted local parts, and
// IDN domains uniformly. Semantic validity (the mailbox actually exists,
// the user can receive) is checked downstream by the SMTP relay on a
// per-recipient basis — that's the right layer for best-effort delivery.
// The API layer's job is only to reject syntactic garbage that could
// never route through SMTP at all (no @, whitespace, etc.).
//
// Returns the first invalid address found, with a wrapped parser error
// suitable for surfacing to the caller. Empty slices are not an error
// here — handlers already enforce "at least one recipient" separately
// with a more specific message.
// ValidateRecipients is the exported seam over validateRecipients so the v1
// httpapi layer reuses the same RFC 5322 recipient check.
func ValidateRecipients(groups ...[]string) error { return validateRecipients(groups...) }

// MaxAddressLen bounds a single email-address-bearing request string — a
// recipient item in to/cc/bcc, a reply_to override, or an agent email — as the
// FULL string (optional display name + <addr>), so a multi-megabyte display
// name can't ride in on an otherwise-valid address. 320 is the classic
// historical addr-spec envelope with the display-name form held to the same
// budget. Counted in Unicode code points (runes), NOT bytes, to match the
// OpenAPI maxLength semantics of the /v1 request schemas (JSON Schema counts
// code points; Huma validates with utf8.RuneCountInString). The /v1 schemas
// declare the same value declaratively; this runtime check is the shared
// backstop for every recipient list that reaches the send path. After parsing,
// outbound.ValidateMailboxAddress separately enforces SMTP's octet limits on
// the addr-spec itself.
const MaxAddressLen = 320

func validateRecipients(groups ...[]string) error {
	for _, group := range groups {
		for _, addr := range group {
			if addr == "" {
				return errors.New("recipient address must not be empty")
			}
			// Bound BEFORE parsing so an oversized string is rejected on a
			// cheap length count, never handed to the address parser.
			if n := utf8.RuneCountInString(addr); n > MaxAddressLen {
				return fmt.Errorf("recipient address too long — %d characters, max %d (display name + address combined)", n, MaxAddressLen)
			}
			parsed, err := mail.ParseAddress(addr)
			if err != nil {
				return fmt.Errorf("invalid recipient %q: %w", addr, err)
			}
			if err := outbound.ValidateMailboxAddress(parsed.Address); err != nil {
				return fmt.Errorf("invalid recipient %q: %w", addr, err)
			}
		}
	}
	return nil
}

// validateDomain runs the IDNA "Lookup" profile against a user-supplied
// domain string. Lookup is the strictest of the four IDNA profiles and
// is what DNS resolvers themselves apply when converting a name into
// a wire-format query — it rejects whitespace, control characters,
// invalid label combinations, and over-length names; converts Unicode
// IDN to Punycode along the way. We additionally require at least one
// period because IDNA accepts bare labels like "localhost" which
// aren't legal as a user-claimable domain.
//
// Returns the ASCII-normalized form on success so callers can persist
// the canonical wire-format (e.g. "xn--e1afmkfd.xn--p1ai" for
// "пример.рф") instead of the raw input.
// ValidateDomain is the exported seam over validateDomain so the v1 httpapi
// layer reuses the exact IDN/punycode normalization + label-length checks
// instead of replicating the security-relevant parsing. Returns the
// normalized (Punycode) form.
func ValidateDomain(domain string) (string, error) {
	return validateDomain(domain)
}

func validateDomain(domain string) (string, error) {
	if domain == "" {
		return "", errors.New("domain is required")
	}
	// Reject IP literals outright: every all-numeric label is valid
	// IDNA, so "192.168.1.1" would otherwise parse as four legal
	// labels and register as a "domain" — burning quota on a name
	// that can never carry the MX/TXT/DKIM records we provision and
	// can never verify. Check the raw input (catches IPv4, IPv6, and
	// bracketed IPv6 before IDNA rejects the colons with a more
	// confusing error) and, below, the IDNA-normalized form (catches
	// Unicode digit forms like "１０.０.０.５" that only become an IP
	// literal after mapping).
	if isIPLiteral(domain) {
		return "", errors.New("invalid domain: IP address is not a registrable domain")
	}
	if !strings.Contains(domain, ".") {
		return "", errors.New("domain must contain at least one period")
	}
	ascii, err := idna.Lookup.ToASCII(domain)
	if err != nil {
		return "", fmt.Errorf("invalid domain: %w", err)
	}
	if isIPLiteral(ascii) {
		return "", errors.New("invalid domain: IP address is not a registrable domain")
	}
	// IDNA's VerifyDNSLength option only enforces the 253-char total
	// length; the 63-char per-label DNS limit (RFC 1035) is not
	// checked. Walk labels explicitly so we don't accept domains that
	// would fail downstream at the resolver.
	for _, label := range strings.Split(ascii, ".") {
		if label == "" {
			return "", errors.New("invalid domain: empty label")
		}
		if len(label) > 63 {
			return "", errors.New("invalid domain: label exceeds 63 characters")
		}
	}
	return ascii, nil
}

// isIPLiteral reports whether s is a bare IP literal (IPv4 or IPv6),
// including the bracketed IPv6 form used in URLs/SMTP address literals.
func isIPLiteral(s string) bool {
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		s = s[1 : len(s)-1]
	}
	return net.ParseIP(s) != nil
}

// clientIP keys the per-IP limiters on the same trusted source as the
// DCR limiter: CF-Connecting-IP only, never the client-controlled
// X-Forwarded-For (see dcrSourceIP for the full rationale and the
// origin-firewall caveat). Delegating keeps every per-IP surface
// identical and impossible to drift apart.
func clientIP(r *http.Request) string {
	return dcrSourceIP(r)
}
