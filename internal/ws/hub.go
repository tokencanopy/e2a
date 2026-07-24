package ws

import (
	"context"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// Metrics is the narrow slice of telemetry.Metrics the WS transport emits
// (same pattern as internal/janitor.Metrics). Each event has exactly ONE
// owner so nothing double-counts: the Hub owns connects, the active gauge,
// send failures, and shutdown disconnects (it drops every conn at Close);
// the Handler owns handshake rejections, per-connection disconnect reasons,
// and drain counts (it is the only place the reason is known).
type Metrics interface {
	WSConnected()
	WSDisconnected(reason string) // reason ∈ {replaced, ping_timeout, client_close, error, shutdown}
	// WSHandshakeRejected counts pre-upgrade handshake rejections.
	// reason ∈ {unauthorized, not_found, forbidden, upgrade_failed,
	// internal_error}. Never pass emails or tokens.
	WSHandshakeRejected(reason string)
	WSDrained(count int)
	WSSendFailure()
	SetWSActive(n int)
}

// noopMetrics is the nil-safe default for Hub and Handler, so call sites
// never guard and tests don't have to wire anything.
type noopMetrics struct{}

func (noopMetrics) WSConnected()               {}
func (noopMetrics) WSDisconnected(string)      {}
func (noopMetrics) WSHandshakeRejected(string) {}
func (noopMetrics) WSDrained(int)              {}
func (noopMetrics) WSSendFailure()             {}
func (noopMetrics) SetWSActive(int)            {}

// Hub manages WebSocket connections for agents that live-tail inbound
// mail. Any agent may connect — WS is an opportunistic push on top of
// the durable pollable inbox + webhook subscriptions.
// One connection per agent; new connections replace old ones.
type Hub struct {
	mu      sync.RWMutex
	conns   map[string]*websocket.Conn // agentID → conn
	metrics Metrics
}

func NewHub() *Hub {
	return &Hub{
		conns:   make(map[string]*websocket.Conn),
		metrics: noopMetrics{},
	}
}

// SetMetrics wires the observability backend. Default is a no-op; nil resets
// to it. Call at wiring time, before the hub serves connections.
func (h *Hub) SetMetrics(m Metrics) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m == nil {
		m = noopMetrics{}
	}
	h.metrics = m
}

// Register stores a connection for the given agent, returning any previous connection.
// The caller should close the old connection if non-nil (and records that
// disconnect as "replaced" — the hub only counts the new connect here).
func (h *Hub) Register(agentID string, conn *websocket.Conn) (old *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	old = h.conns[agentID]
	h.conns[agentID] = conn
	h.metrics.WSConnected()
	h.metrics.SetWSActive(len(h.conns))
	return old
}

// Unregister removes the connection for the given agent, reporting whether it
// did. Only removes if the current connection matches (prevents race with
// re-register); false therefore means conn was no longer current — superseded
// by a newer connection (the caller's "replaced" signal) or already dropped
// by Close.
func (h *Hub) Unregister(agentID string, conn *websocket.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[agentID] != conn {
		return false
	}
	delete(h.conns, agentID)
	h.metrics.SetWSActive(len(h.conns))
	return true
}

// Send writes a message to the agent's WebSocket connection.
// Returns true if the message was sent successfully.
func (h *Hub) Send(agentID string, msg []byte) bool {
	h.mu.RLock()
	conn := h.conns[agentID]
	metrics := h.metrics
	h.mu.RUnlock()

	if conn == nil {
		return false // not registered — not a send failure
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		metrics.WSSendFailure()
		return false
	}
	return true
}

// IsConnected returns whether an agent has an active WebSocket connection.
func (h *Hub) IsConnected(agentID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conns[agentID] != nil
}

// Close closes all active connections.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, conn := range h.conns {
		// 1001 + the stable "shutting_down" token: transient (a deploy /
		// restart) — clients reconnect with backoff. See closecodes.go.
		conn.Close(websocket.StatusGoingAway, ReasonShuttingDown)
		delete(h.conns, id)
		// The hub owns this disconnect: the conn is gone from the map, so the
		// handler's cleanup sees "not current, nobody newer" and stays silent.
		h.metrics.WSDisconnected("shutdown")
	}
	h.metrics.SetWSActive(0)
}
