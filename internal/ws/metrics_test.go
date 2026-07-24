package ws

// WS connection/drain SLI tests (docs/observability.md): the Hub owns
// connect/active-gauge/send-failure/shutdown, the Handler owns disconnect
// reasons + drain counts — these tests pin both owners and that neither
// double-counts the other's events.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
	"nhooyr.io/websocket"
)

// fakeWSMetrics records Metrics calls. Mutex-protected: hub and handler emit
// from server goroutines.
type fakeWSMetrics struct {
	mu          sync.Mutex
	connected   int
	disconnects []string
	rejections  []string
	drained     []int
	sendFails   int
	active      []int // every SetWSActive value, in order
}

func (f *fakeWSMetrics) WSConnected() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected++
}

func (f *fakeWSMetrics) WSDisconnected(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnects = append(f.disconnects, reason)
}

func (f *fakeWSMetrics) WSHandshakeRejected(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejections = append(f.rejections, reason)
}

func (f *fakeWSMetrics) WSDrained(count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drained = append(f.drained, count)
}

func (f *fakeWSMetrics) WSSendFailure() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendFails++
}

func (f *fakeWSMetrics) SetWSActive(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = append(f.active, n)
}

// lastActive returns the most recent gauge value (-1 if never set).
func (f *fakeWSMetrics) lastActive() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.active) == 0 {
		return -1
	}
	return f.active[len(f.active)-1]
}

// snapshotDisconnects copies the recorded reasons.
func (f *fakeWSMetrics) snapshotDisconnects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.disconnects...)
}

// snapshotRejections copies the recorded handshake-rejection reasons.
func (f *fakeWSMetrics) snapshotRejections() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rejections...)
}

// waitDisconnect polls until a disconnect with the given reason is recorded.
func waitDisconnect(t *testing.T, f *fakeWSMetrics, reason string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		for _, r := range f.snapshotDisconnects() {
			if r == reason {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for disconnect %q, got %v", reason, f.snapshotDisconnects())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestHubMetrics_ActiveGaugeUpDown(t *testing.T) {
	hub := NewHub()
	defer hub.Close()
	fm := &fakeWSMetrics{}
	hub.SetMetrics(fm)

	_, conn1 := newTestWSPair(t)
	_, conn2 := newTestWSPair(t)

	hub.Register("agent1", conn1)
	if fm.connected != 1 || fm.lastActive() != 1 {
		t.Fatalf("after first register: connected=%d active=%d, want 1/1", fm.connected, fm.lastActive())
	}
	hub.Register("agent2", conn2)
	if fm.connected != 2 || fm.lastActive() != 2 {
		t.Fatalf("after second register: connected=%d active=%d, want 2/2", fm.connected, fm.lastActive())
	}
	if !hub.Unregister("agent1", conn1) {
		t.Fatal("Unregister of the current conn should report true")
	}
	if fm.lastActive() != 1 {
		t.Fatalf("after unregister: active=%d, want 1", fm.lastActive())
	}
}

// TestHubMetrics_ReplaceKeepsGaugeStable: a takeover register counts a new
// connect but the gauge stays at the map size (1) — the superseded conn's
// disconnect is the handler's to record, not the hub's.
func TestHubMetrics_ReplaceKeepsGaugeStable(t *testing.T) {
	hub := NewHub()
	defer hub.Close()
	fm := &fakeWSMetrics{}
	hub.SetMetrics(fm)

	_, conn1 := newTestWSPair(t)
	_, conn2 := newTestWSPair(t)

	hub.Register("agent1", conn1)
	if old := hub.Register("agent1", conn2); old != conn1 {
		t.Fatal("expected old connection returned")
	}
	if fm.connected != 2 || fm.lastActive() != 1 {
		t.Fatalf("after takeover: connected=%d active=%d, want 2/1", fm.connected, fm.lastActive())
	}
	if len(fm.snapshotDisconnects()) != 0 {
		t.Fatalf("hub must not record the replaced disconnect, got %v", fm.snapshotDisconnects())
	}
	// The stale conn's unregister is a no-op: gauge untouched, reported false.
	if hub.Unregister("agent1", conn1) {
		t.Fatal("Unregister of a superseded conn should report false")
	}
	if fm.lastActive() != 1 {
		t.Fatalf("stale unregister moved the gauge to %d, want 1", fm.lastActive())
	}
}

func TestHubMetrics_SendFailure(t *testing.T) {
	hub := NewHub()
	defer hub.Close()
	fm := &fakeWSMetrics{}
	hub.SetMetrics(fm)

	_, serverConn := newTestWSPair(t)
	hub.Register("agent1", serverConn)

	// Break the registered conn so the write fails.
	serverConn.Close(websocket.StatusNormalClosure, "")
	if hub.Send("agent1", []byte("hello")) {
		t.Fatal("send on a closed conn should fail")
	}
	if fm.sendFails != 1 {
		t.Fatalf("sendFails = %d, want 1", fm.sendFails)
	}

	// An unregistered agent is "not connected", not a send failure.
	if hub.Send("nobody", []byte("hello")) {
		t.Fatal("send to an unregistered agent should fail")
	}
	if fm.sendFails != 1 {
		t.Fatalf("unregistered send counted as failure: sendFails = %d, want 1", fm.sendFails)
	}
}

func TestHubMetrics_CloseRecordsShutdown(t *testing.T) {
	hub := NewHub()
	fm := &fakeWSMetrics{}
	hub.SetMetrics(fm)

	_, conn1 := newTestWSPair(t)
	_, conn2 := newTestWSPair(t)
	hub.Register("agent1", conn1)
	hub.Register("agent2", conn2)

	hub.Close()
	got := fm.snapshotDisconnects()
	if len(got) != 2 || got[0] != "shutdown" || got[1] != "shutdown" {
		t.Fatalf("disconnects = %v, want [shutdown shutdown]", got)
	}
	if fm.lastActive() != 0 {
		t.Fatalf("active after Close = %d, want 0", fm.lastActive())
	}
}

// TestHandlerMetrics_ClientCloseAndDrain: a clean client close records ONE
// disconnect with reason "client_close", and the connect-drain count matches
// the pushed unread messages.
func TestHandlerMetrics_ClientCloseAndDrain(t *testing.T) {
	hub := NewHub()
	defer hub.Close()
	fm := &fakeWSMetrics{}
	hub.SetMetrics(fm)

	now := time.Now()
	store := &mockStore{
		user:  newTestUser(),
		agent: newTestAgent("user_1"),
		messages: []identity.Message{
			{ID: "msg_1", AgentID: "agent_test", Recipient: "bot@agents.e2a.dev", CreatedAt: now},
			{ID: "msg_2", AgentID: "agent_test", Recipient: "bot@agents.e2a.dev", CreatedAt: now},
		},
	}
	handler := NewHandler(hub, store)
	handler.SetMetrics(fm)
	srv := startServer(t, handler)

	conn, _ := dialWS(t, srv, "bot@agents.e2a.dev", "valid_key")
	if conn == nil {
		t.Fatal("expected successful WS connection")
	}
	waitConnected(t, hub, "agent_test")

	// Read the two drained frames so the close below is clean.
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read drained frame %d: %v", i+1, err)
		}
	}

	conn.Close(websocket.StatusNormalClosure, "bye")
	waitDisconnect(t, fm, "client_close")

	got := fm.snapshotDisconnects()
	if len(got) != 1 {
		t.Fatalf("disconnects = %v, want exactly [client_close]", got)
	}
	fm.mu.Lock()
	drained := append([]int(nil), fm.drained...)
	fm.mu.Unlock()
	if len(drained) != 1 || drained[0] != 2 {
		t.Fatalf("drained = %v, want [2]", drained)
	}
}

// TestHandlerMetrics_Replaced: when a newer connection takes over, the
// superseded connection's disconnect is recorded once, as "replaced".
func TestHandlerMetrics_Replaced(t *testing.T) {
	hub := NewHub()
	defer hub.Close()
	fm := &fakeWSMetrics{}
	hub.SetMetrics(fm)

	store := &mockStore{
		user:  newTestUser(),
		agent: newTestAgent("user_1"),
	}
	handler := NewHandler(hub, store)
	handler.SetMetrics(fm)
	srv := startServer(t, handler)

	first, _ := dialWS(t, srv, "bot@agents.e2a.dev", "valid_key")
	if first == nil {
		t.Fatal("expected first WS connection to open")
	}
	defer first.Close(websocket.StatusNormalClosure, "")
	waitConnected(t, hub, "agent_test")

	second, _ := dialWS(t, srv, "bot@agents.e2a.dev", "valid_key")
	if second == nil {
		t.Fatal("expected second WS connection to open")
	}
	defer second.Close(websocket.StatusNormalClosure, "")

	// Read on the superseded client so it completes the close handshake (a
	// real SDK client is always reading) — this unblocks the server's read
	// loop promptly instead of after the handshake timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := first.Read(ctx); err == nil {
		t.Fatal("expected the superseded connection to be closed")
	}

	waitDisconnect(t, fm, "replaced")
	got := fm.snapshotDisconnects()
	if len(got) != 1 {
		t.Fatalf("disconnects = %v, want exactly [replaced]", got)
	}
	if !hub.IsConnected("agent_test") {
		t.Fatal("the newer connection must remain registered")
	}
	if fm.connected != 2 {
		t.Fatalf("connected = %d, want 2", fm.connected)
	}
}

// ── Handshake-rejection SLI (docs/observability.md) ─────────────
//
// Every pre-upgrade rejection branch in serve() records exactly one
// e2a_ws_handshake_rejected_total with an enum reason; a successful
// handshake records none (the success side is e2a_ws_connects_total).

func TestHandlerMetrics_HandshakeRejections(t *testing.T) {
	cases := []struct {
		name       string
		store      *mockStore
		token      string
		plainHTTP  bool // plain GET (no upgrade) instead of a WS dial
		wantReason string
	}{
		{"missing credential", &mockStore{}, "", true, "unauthorized"},
		{"invalid token", &mockStore{user: nil}, "bad_key", false, "unauthorized"},
		{"unknown key from store", &mockStore{userErr: pgx.ErrNoRows}, "bad_key", false, "unauthorized"},
		{"auth store outage", &mockStore{userErr: errors.New("connection reset")}, "valid_key", false, "internal_error"},
		{"agent not found", &mockStore{user: newTestUser(), agentErr: pgx.ErrNoRows}, "valid_key", false, "not_found"},
		{"agent store outage", &mockStore{user: newTestUser(), agentErr: errors.New("connection reset")}, "valid_key", false, "internal_error"},
		{"cross-tenant agent", &mockStore{user: newTestUser(), agent: newTestAgent("user_other")}, "valid_key", false, "not_found"},
		{"agent-scope pin", &mockStore{user: newTestUser(), scope: identity.ScopeAgent, agentID: "agent_other", agent: newTestAgent("user_1")}, "valid_key", false, "forbidden"},
		{"upgrade failure", &mockStore{user: newTestUser(), agent: newTestAgent("user_1")}, "valid_key", true, "upgrade_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := NewHub()
			defer hub.Close()
			fm := &fakeWSMetrics{}
			handler := NewHandler(hub, tc.store)
			handler.SetMetrics(fm)
			srv := startServer(t, handler)

			if tc.plainHTTP {
				resp := doHTTP(t, srv, "bot@agents.e2a.dev", tc.token)
				resp.Body.Close()
			} else {
				conn, _ := dialWS(t, srv, "bot@agents.e2a.dev", tc.token)
				if conn != nil {
					t.Fatal("expected the handshake to be rejected")
				}
			}
			got := fm.snapshotRejections()
			if len(got) != 1 || got[0] != tc.wantReason {
				t.Fatalf("rejections = %v, want exactly [%s]", got, tc.wantReason)
			}
			fm.mu.Lock()
			connected := fm.connected
			fm.mu.Unlock()
			if connected != 0 {
				t.Errorf("connected = %d, want 0 — a rejected handshake never registers", connected)
			}
		})
	}
}

func TestHandlerMetrics_SuccessfulHandshakeRecordsNoRejection(t *testing.T) {
	hub := NewHub()
	defer hub.Close()
	fm := &fakeWSMetrics{}
	handler := NewHandler(hub, &mockStore{user: newTestUser(), agent: newTestAgent("user_1")})
	handler.SetMetrics(fm)
	srv := startServer(t, handler)

	conn, _ := dialWS(t, srv, "bot@agents.e2a.dev", "valid_key")
	if conn == nil {
		t.Fatal("expected successful WS connection")
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitConnected(t, hub, "agent_test")

	if got := fm.snapshotRejections(); len(got) != 0 {
		t.Errorf("rejections = %v, want none for a successful handshake", got)
	}
}
