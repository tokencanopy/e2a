package selftest

// websocket_round_trip scenario tests, mock-driven like the rest of the
// internal failure-path suite: an httptest server stands in for the e2a WS
// endpoint + self-send API so connect, push, auth-reject, and no-frame
// timeout are each exercised without a DB.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// wsStub serves GET …/ws (upgrade, Bearer-checked), POST …/messages
// (responds with the real SendResultView field names — message_id, never
// "id" — and pushes an email.received envelope carrying the posted subject),
// and DELETE …/messages/{id} (recorded into deleted, so tests can pin the
// scenario's residue cleanup against the actual response shape).
type wsStubState struct {
	mu        sync.Mutex
	conn      *websocket.Conn
	deleted   []string
	closeSeen chan struct{} // closed once the stub reads a normal-closure frame
	closeOnce sync.Once
}

// noteCloseFrame records that the stub read the client's normal-closure frame.
// Safe to call more than once.
func (st *wsStubState) noteCloseFrame() {
	st.closeOnce.Do(func() { close(st.closeSeen) })
}

// awaitCloseFrame reports whether the stub read a normal-closure frame from the
// client, waiting up to d for the stub's read goroutine to record it.
//
// The wait is NOT a wall-clock budget on the handshake — it is the missing
// happens-before edge. The library echoes the close frame from inside
// handleControl BEFORE the error propagates out of the stub's c.Read, so
// conn.Close can return (and the scenario with it) while the stub goroutine
// has not yet been scheduled to record what it saw. Measured margin on an idle
// machine is ~6µs, so any deschedule longer than that flipped this assertion —
// which is exactly what a loaded, coverage-instrumented CI runner does.
//
// The contract this pins is unchanged: if the handshake genuinely does not
// complete, the frame is never read, closeSeen is never closed, and this
// returns false. A non-reading stub also makes conn.Close burn its full 5s
// timeout first, so d is only ever paid on an already-failing test.
func (st *wsStubState) awaitCloseFrame(d time.Duration) bool {
	select {
	case <-st.closeSeen:
		return true
	case <-time.After(d):
		return false
	}
}

func (st *wsStubState) deletedIDs() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]string(nil), st.deleted...)
}

func wsStub(t *testing.T) (*httptest.Server, *wsStubState) {
	t.Helper()
	st := &wsStubState{closeSeen: make(chan struct{})}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/ws"):
			if r.Header.Get("Authorization") != "Bearer k" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			st.mu.Lock()
			st.conn = c
			st.mu.Unlock()
			// Read loop mirroring the real handler (internal/ws/handler.go:377).
			// Control frames are only processed while something reads, so a stub
			// that never reads never answers the client's close frame — and the
			// scenario's deferred conn.Close then blocks for the library's full
			// 5s close-handshake timeout, making every WS scenario invocation
			// cost 5s of pure waiting.
			//
			// An explicit loop rather than CloseRead so the close frame is both
			// ANSWERED and OBSERVABLE — sawCloseFrame is what pins this fix.
			//
			// NOT r.Context(): Accept hijacks the connection so it outlives this
			// handler, and the request context is cancelled the moment the
			// handler returns, which tore the socket down before the push could
			// be written ("failed to read frame header: EOF").
			go func() {
				for {
					if _, _, err := c.Read(context.Background()); err != nil {
						if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
							st.noteCloseFrame()
						}
						return
					}
				}
			}()
		case r.Method == http.MethodDelete:
			parts := strings.Split(r.URL.Path, "/")
			st.mu.Lock()
			st.deleted = append(st.deleted, parts[len(parts)-1])
			st.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/messages") && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			var in struct {
				Subject string `json:"subject"`
			}
			_ = json.Unmarshal(raw, &in)
			st.mu.Lock()
			c := st.conn
			st.mu.Unlock()
			if c != nil {
				env, _ := json.Marshal(map[string]any{
					"type": "email.received",
					"data": map[string]any{"subject": in.Subject, "message_id": "msg_inbound_copy"},
				})
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = c.Write(ctx, websocket.MessageText, env)
				cancel()
			}
			w.Write([]byte(`{"status":"sent","message_id":"msg_sent_copy","method":"loopback"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return httptest.NewServer(mux), st
}

func TestScenarioWebSocketRoundTrip(t *testing.T) {
	srv, st := wsStub(t)
	defer srv.Close()
	p := failProbe(srv.URL, "", nil)
	if r := scenarioWebSocketRoundTrip(context.Background(), p); r.Status != StatusPass {
		t.Errorf("happy path: status = %s (%q), want pass", r.Status, r.Detail)
	}
	// Residue cleanup contract: BOTH copies of the probe message are trashed —
	// the inbound unread copy (from the push frame's data.message_id) and the
	// sent copy (from the send response's message_id — the real SendResultView
	// field; a wrong field name here silently leaks ~2,880 rows/day in prod).
	deleted := st.deletedIDs()
	want := map[string]bool{"msg_inbound_copy": false, "msg_sent_copy": false}
	for _, id := range deleted {
		if _, ok := want[id]; ok {
			want[id] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("message %q was not trashed (deleted: %v)", id, deleted)
		}
	}
	// Close-handshake contract. conn.Close waits for the peer's close frame and
	// gives up after 5s, so a stub that does not read makes EVERY invocation of
	// this scenario cost 5s of pure waiting. Asserting the stub observed the
	// frame pins that — without a wall-clock budget, which is the flake pattern
	// this suite already got bitten by. See awaitCloseFrame for why the wait is
	// a happens-before edge rather than a timing budget.
	if !st.awaitCloseFrame(2 * time.Second) {
		t.Error("stub never observed the client's close frame: the close handshake " +
			"did not complete, so conn.Close burned the library's full 5s timeout")
	}
}

func TestScenarioWebSocketRoundTrip_Fail(t *testing.T) {
	// Handshake rejected (bad credential) → fail.
	srv, _ := wsStub(t)
	defer srv.Close()
	p := failProbe(srv.URL, "", nil)
	p.APIKey = "wrong"
	mustFail(t, "ws 401", scenarioWebSocketRoundTrip(context.Background(), p))

	// Connects but no frame ever arrives (self-send accepted, push lost) →
	// fail on the round-trip timeout, not hang.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ws") {
			c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err == nil {
				// Hold open and push nothing — but still read, so the deferred
				// conn.Close is answered instead of burning the library's 5s
				// close-handshake timeout. The scenario must fail on its own
				// round-trip timeout, which is what this case asserts.
				c.CloseRead(context.Background())
			}
			return
		}
		w.Write([]byte(`{"method":"loopback"}`))
	})
	silent := httptest.NewServer(mux)
	defer silent.Close()
	p2 := failProbe(silent.URL, "", nil)
	start := time.Now()
	mustFail(t, "no frame", scenarioWebSocketRoundTrip(context.Background(), p2))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("no-frame case took %s, want bounded by the probe timeout", elapsed)
	}

	// Self-send rejected → fail.
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ws") {
			if c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true}); err == nil {
				c.CloseRead(context.Background())
			}
			return
		}
		w.WriteHeader(http.StatusForbidden)
	})
	deny := httptest.NewServer(mux2)
	defer deny.Close()
	mustFail(t, "self-send 403", scenarioWebSocketRoundTrip(context.Background(), failProbe(deny.URL, "", nil)))
}

func TestScenarioMCPHTTPRoundTrip_RequiredNotConfigured(t *testing.T) {
	// E2A_PROBE_REQUIRE_MCP: an unset MCP URL must FAIL instead of
	// skip-as-pass, so a misconfigured prod prober can't stay silently green
	// while never probing MCP.
	p := failProbe("http://127.0.0.1:1", "", nil)
	p.RequireMCP = true
	mustFail(t, "require-mcp unset URL", scenarioMCPHTTPRoundTrip(context.Background(), p))
}
