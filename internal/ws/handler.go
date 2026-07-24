package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/httpapi"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/webhookpub"
	"nhooyr.io/websocket"
)

// HandlerStore is the subset of identity.Store that the WS handler needs.
type HandlerStore interface {
	// GetPrincipalByAPIKey returns the scoped principal (User + Scope + AgentID)
	// so the WS transport can enforce the SAME agent-scope confinement the REST
	// API does (HIGH-1): an agent-scoped key must not open another agent's stream.
	GetPrincipalByAPIKey(ctx context.Context, apiKey string) (*identity.Principal, error)
	GetAgentByEmail(ctx context.Context, email string) (*identity.AgentIdentity, error)
	GetMessagesByAgent(ctx context.Context, f identity.MessageListFilter) ([]identity.Message, error)
}

type eventEnvelopeStore interface {
	GetEventEnvelope(ctx context.Context, messageID, eventType string) ([]byte, error)
}

// Handler upgrades HTTP connections to WebSocket. Open to any agent the
// caller owns — the legacy local-mode gate was removed (migration 029).
type Handler struct {
	hub     *Hub
	store   HandlerStore
	metrics Metrics
}

func NewHandler(hub *Hub, store HandlerStore) *Handler {
	return &Handler{hub: hub, store: store, metrics: noopMetrics{}}
}

// SetMetrics wires the observability backend. Default is a no-op; nil resets
// to it. The handler owns the per-connection disconnect reasons + drain
// counts; connect/gauge/send-failure/shutdown live on the Hub (see Metrics).
func (h *Handler) SetMetrics(m Metrics) {
	if m == nil {
		m = noopMetrics{}
	}
	h.metrics = m
}

// ServeWithEmail handles the WebSocket upgrade for an explicitly-provided
// agent email — the router owns path extraction (and, for routers like chi
// that return params still percent-encoded, DECODING; see the /v1 mount in
// internal/httpapi). The old gorilla/mux `Handle` variant was removed with
// the retired /api/v1 surface: this is the transport's only entry point.
func (h *Handler) ServeWithEmail(w http.ResponseWriter, r *http.Request, rawEmail string) {
	h.serve(w, r, rawEmail)
}

// reject counts the handshake rejection and writes the canonical error
// envelope — the two always happen together, so a future early return can't
// skip the counter. reason ∈ the WSHandshakeRejected enum; status/code/msg
// shape the (unchanged) client-facing response.
func (h *Handler) reject(w http.ResponseWriter, r *http.Request, reason string, status int, code, msg string) {
	h.metrics.WSHandshakeRejected(reason)
	if status == http.StatusUnauthorized {
		// RFC 6750 §3 — set before WriteError writes the status line, since
		// headers can't be added once the status is flushed.
		w.Header().Set("WWW-Authenticate", `Bearer realm="e2a"`)
	}
	httpapi.WriteError(w, r, status, code, msg)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rawEmail string) {
	// Authenticate via the `Authorization: Bearer <api_key>` handshake header —
	// the standard shape for non-browser WebSocket clients (matches OpenAI
	// Realtime server-side, Slack Socket Mode, Kubernetes). The credential never
	// touches the URL, so it can't leak to access logs / browser history /
	// Referer the way a `?token=` query param does. All e2a WS clients (the TS +
	// Python SDKs and the CLI) are non-browser and set the header on the
	// handshake. (A short-lived connect ticket is the path to add later if an
	// in-browser client ever needs to connect — browsers can't set headers.)
	// Handshake rejections happen BEFORE the WebSocket upgrade (the connection
	// is still a plain HTTP request, not yet hijacked), so they return the same
	// canonical error envelope every /v1 REST endpoint does — httpapi.WriteError
	// emits {error:{code,message,request_id}} + the X-Request-Id header, reusing
	// the request id the chi-root middleware already stamped. A client's shared
	// envelope-based error handling then works identically on a failed WS
	// handshake and a failed REST call. Status codes are UNCHANGED from the prior
	// behavior. WWW-Authenticate (RFC 6750 §3) is set before WriteError writes the
	// status line, since headers can't be added once the status is flushed; on the
	// prod stack the authChallenge middleware also (re)asserts it on any 401.
	token := bearerToken(r)
	if token == "" {
		h.reject(w, r, "unauthorized", http.StatusUnauthorized, "unauthorized",
			"missing credential — send Authorization: Bearer <api_key>")
		return
	}

	principal, err := h.store.GetPrincipalByAPIKey(r.Context(), token)
	if err != nil || principal == nil || principal.User == nil {
		// A genuinely unknown/revoked/expired key surfaces as pgx.ErrNoRows
		// (or a nil principal) — a client fault. Any OTHER store error is an
		// e2a-side failure to serve the handshake and is counted
		// internal_error so a DB outage burns the handshake SLI instead of
		// hiding inside the client-fault bucket. The 401 response is
		// unchanged either way.
		reason := "unauthorized"
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			reason = "internal_error"
		}
		h.reject(w, r, reason, http.StatusUnauthorized, "unauthorized", "invalid token")
		return
	}

	// Resolve agent and verify ownership. Canonicalize the email so that
	// `ws://.../UPPER@x.dev/ws?token=…` resolves identically to the
	// lower-case form — matches the REST API's `normalizeEmail` policy.
	email := identity.NormalizeEmail(rawEmail)
	agent, err := h.store.GetAgentByEmail(r.Context(), email)
	if err != nil {
		// Same split as the credential check: pgx.ErrNoRows is a genuinely
		// unknown address (client fault); any other store error is e2a-side
		// (internal_error). The 404 response is unchanged either way.
		reason := "not_found"
		if !errors.Is(err, pgx.ErrNoRows) {
			reason = "internal_error"
		}
		h.reject(w, r, reason, http.StatusNotFound, "not_found", "agent not found")
		return
	}
	// Tenant ownership: the agent must belong to the credential's user. A
	// cross-tenant miss returns 404 not_found — the SAME response as a
	// nonexistent agent above — so an authenticated caller can't tell "this
	// address doesn't exist" from "it exists but isn't yours". Emitting 403
	// here would make the WS handshake an agent-existence enumeration oracle
	// across tenants; collapsing both into 404 closes it (mirrors the REST
	// resolveOwnedAgent, which likewise refuses to distinguish the two).
	if agent.UserID != principal.User.ID {
		h.reject(w, r, "not_found", http.StatusNotFound, "not_found", "agent not found")
		return
	}
	// Agent-scope confinement (HIGH-1): an agent-scoped credential is pinned to
	// its one bound agent — it may not open a different agent's stream even
	// within the same account. Mirrors the REST resolveOwnedAgent pin.
	if principal.Scope == identity.ScopeAgent && principal.AgentID != agent.ID {
		h.reject(w, r, "forbidden", http.StatusForbidden, "forbidden", "not authorized for this agent")
		return
	}

	// Upgrade to WebSocket. Origin checks are intentionally disabled because
	// authentication is the `Authorization: Bearer` header, not cookies — and a
	// browser cannot set that header on a cross-site WebSocket, so there is no
	// cross-site-WebSocket-hijacking (CSWSH) vector to defend against here.
	// CLI/SDK clients have no Origin to check. (Header auth is strictly safer
	// than the old `?token=` query param it replaced, which leaked the key to
	// access logs / history / Referer.)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		h.metrics.WSHandshakeRejected("upgrade_failed")
		log.Printf("[ws] upgrade failed for %s: %v", email, err)
		return
	}

	log.Printf("[ws] connected: %s (user=%s)", email, principal.User.ID)

	// Register connection; close old one if exists. The superseded connection
	// gets the application close code 4000 "replaced" — NOT 1008 (policy
	// violation) — because this is the benign one-connection-per-agent
	// takeover, not a rejection of the closed client. SDKs treat 4000 as
	// terminal (no auto-reconnect): reconnecting would steal the socket back
	// from the newer connection and loop. See closecodes.go for the table.
	if old := h.hub.Register(agent.ID, conn); old != nil {
		log.Printf("[ws] replacing existing connection for %s", email)
		old.Close(StatusReplaced, ReasonReplaced)
	}

	// Drain unread messages as notifications
	h.drainUnread(agent)

	// Read loop: keepalive + disconnect detection
	reason := h.readLoop(conn, agent)

	// Cleanup on disconnect. If the unregister didn't remove us we were no
	// longer the current connection: a newer one superseded us ("replaced" —
	// override whatever the read loop saw, since its error is just our own
	// takeover close), or the hub already dropped us at shutdown (counted
	// there as "shutdown" — stay silent).
	if !h.hub.Unregister(agent.ID, conn) {
		if h.hub.IsConnected(agent.ID) {
			reason = "replaced"
		} else {
			reason = ""
		}
	}
	if reason != "" {
		h.metrics.WSDisconnected(reason)
	}
	conn.Close(websocket.StatusNormalClosure, "")
	log.Printf("[ws] disconnected: %s", email)
}

// bearerToken extracts the API key from the `Authorization: Bearer <key>`
// handshake header (scheme match is case-insensitive per RFC 7235). Returns ""
// when the header is absent or not a Bearer credential.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// drainUnread sends notifications for all unread messages. Notifications are
// best-effort; messages are marked read only when fetched via the REST API.
func (h *Handler) drainUnread(agent *identity.AgentIdentity) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	messages, err := h.store.GetMessagesByAgent(ctx, identity.MessageListFilter{
		AgentID:   agent.ID,
		Status:    "unread",
		Direction: "inbound",
		Limit:     100,
	})
	if err != nil {
		log.Printf("[ws] drain error for %s: %v", agent.ID, err)
		return
	}

	sent := 0
	for _, msg := range messages {
		notification := NotificationForMessage(ctx, h.store, &msg)
		if len(notification) > 0 && h.hub.Send(agent.ID, notification) {
			sent++
		}
	}
	h.metrics.WSDrained(sent)

	if len(messages) > 0 {
		log.Printf("[ws] drained %d unread messages for %s", len(messages), agent.ID)
	}
}

// NotificationForMessage prefers the immutable envelope persisted by the
// transactional outbox. The fallback preserves compatibility for test/legacy
// stores without the outbox lookup seam.
func NotificationForMessage(ctx context.Context, store HandlerStore, msg *identity.Message) []byte {
	if events, ok := store.(eventEnvelopeStore); ok {
		if envelope, err := events.GetEventEnvelope(ctx, msg.ID, webhookpub.EventEmailReceived); err == nil && len(envelope) > 0 {
			return envelope
		}
		// A production store promises exact cross-channel envelopes. If its
		// durable event is unavailable, fail closed instead of inventing a
		// different payload under the deterministic event id.
		return nil
	}
	return BuildNotification(msg)
}

// BuildNotification reconstructs the same versioned envelope SHAPE for stores
// that cannot return the durable event —
// {type:"email.received", id, schema_version, created_at, data} with the
// canonical typed eventpayload.EmailReceivedData — so a consumer can share one
// parser across both channels. The event id uses the same deterministic
// derivation the outbox uses (sha256(message_id|type)), so a subscriber that
// receives the message on both channels can dedup on it.
//
// This drain-path rebuild populates what the message row provides, including
// the canonical SMTP authentication evidence persisted at intake. Consumers
// therefore get the same DMARC verdict whether the frame arrives live or on a
// reconnect drain.
//
// Production reconnect drain uses NotificationForMessage and therefore reuses
// the exact persisted envelope. This builder remains the compatibility fallback
// for stores that do not expose durable events.
//
// The live relay path (internal/relay) marshals the actual outbox event: the
// same event envelope — identical fields and event id — as the webhook
// delivery (byte layout may differ across channels; JSON key order/escaping
// is not contractual).
func BuildNotification(msg *identity.Message) []byte {
	to := msg.ToRecipients
	if to == nil {
		to = []string{}
	}
	cc := msg.CC
	if cc == nil {
		cc = []string{}
	}
	replyTo := msg.ReplyTo
	if replyTo == nil {
		replyTo = []string{}
	}
	ev := webhookpub.Event{
		ID:        webhookpub.DeterministicEventID(msg.ID, webhookpub.EventEmailReceived),
		Type:      webhookpub.EventEmailReceived,
		CreatedAt: msg.CreatedAt.UTC(),
		Data: eventpayload.EmailReceivedData{
			MessageID:      msg.ID,
			AgentEmail:     msg.Recipient,
			Direction:      "inbound",
			ConversationID: msg.ConversationID,
			HeaderFrom:     nullableString(msg.HeaderFrom),
			EnvelopeFrom:   nullableString(msg.EnvelopeFrom),
			VerifiedDomain: msg.Authentication.VerifiedDomain(),
			To:             to,
			CC:             cc,
			ReplyTo:        replyTo,
			Authentication: msg.Authentication,
			DeliveredTo:    msg.Recipient,
			Subject:        msg.Subject,
			ReceivedAt:     msg.CreatedAt.UTC(),
			Attachments:    eventpayload.AttachmentMetadata(msg.RawMessage),
		},
	}
	data, _ := json.Marshal(ev.AsEnvelope())
	return data
}

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// readLoop blocks until the client disconnects or sends a close frame.
// It ignores client messages and sends periodic pings. Returns the metric
// disconnect reason it observed — "ping_timeout" when the keepalive dropped
// the connection, "client_close" on a clean client close frame (1000/1001),
// "error" otherwise; the caller overrides it for the replaced/shutdown cases
// it alone can see.
func (h *Handler) readLoop(conn *websocket.Conn, agent *identity.AgentIdentity) (reason string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Ping goroutine: send a ping every 30 seconds, fail if no pong within 10 seconds.
	var pingFailed atomic.Bool
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(pingCtx)
				pingCancel()
				if err != nil {
					log.Printf("[ws] ping failed for %s: %v", agent.Email, err)
					// Close the connection to unblock the Read below. 1001 +
					// the stable "ping_timeout" token: a transient condition —
					// clients reconnect with backoff (the peer is usually
					// already gone and sees a 1006 abnormal close instead).
					// The flag is set BEFORE the close so the Read below
					// attributes its error to the keepalive, not the client.
					pingFailed.Store(true)
					conn.Close(websocket.StatusGoingAway, ReasonPingTimeout)
					return
				}
			}
		}
	}()

	// Read loop: consume client messages and control frames until disconnect.
	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			if pingFailed.Load() {
				return "ping_timeout"
			}
			switch websocket.CloseStatus(err) {
			case websocket.StatusNormalClosure, websocket.StatusGoingAway:
				return "client_close" // clean close frame from the client
			default:
				return "error" // abnormal drop (no close frame / network error)
			}
		}
	}
}
