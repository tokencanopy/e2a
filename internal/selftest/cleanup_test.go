package selftest

// Sweep tests. The sweep is the only part of the battery that deletes data the
// battery did not create in that same tick, so the ordering (trash BEFORE
// purge), the query parameters (defaults would skip most of the inbox), and the
// best-effort contract (never surfaces an error) are all load-bearing.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type sweepCall struct {
	method string
	path   string
	query  string
}

// sweepServer serves two pages of message ids — live, then trash — and records
// every request so a test can assert on order and query shape.
func sweepServer(t *testing.T, live, trashed []string) (*httptest.Server, *[]sweepCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []sweepCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, sweepCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery})
		mu.Unlock()

		if r.Method == http.MethodGet {
			ids := live
			if r.URL.Query().Get("deleted") == "true" {
				ids = trashed
			}
			items := make([]map[string]string, 0, len(ids))
			for _, id := range ids {
				items = append(items, map[string]string{"id": id})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "next_cursor": nil})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"deleted":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestSweepMessagesTrashesLiveThenPurgesTrash(t *testing.T) {
	srv, calls := sweepServer(t, []string{"msg_live1", "msg_live2"}, []string{"msg_tr1"})
	p := failProbe(srv.URL, "", nil)

	got := p.SweepMessages()
	if got.Trashed != 2 || got.Purged != 1 {
		t.Fatalf("SweepMessages() = %+v, want {Trashed:2 Purged:1}", got)
	}

	var deletes []sweepCall
	for _, c := range *calls {
		if c.method == http.MethodDelete {
			deletes = append(deletes, c)
		}
	}
	if len(deletes) != 3 {
		t.Fatalf("got %d DELETEs, want 3: %+v", len(deletes), deletes)
	}
	// The first two are trash (no permanent flag); the last is the purge. A
	// purge before its trash would 409 not_in_trash against the real server.
	for i, c := range deletes[:2] {
		if c.query != "" {
			t.Errorf("delete[%d] query = %q, want empty (a plain trash)", i, c.query)
		}
	}
	if deletes[2].query != "permanent=true&confirm=DELETE" {
		t.Errorf("purge query = %q, want permanent=true&confirm=DELETE", deletes[2].query)
	}
}

// The list endpoint defaults to inbound-only and, for inbound, unread-only.
// Without both overrides the sweep would silently leave every outbound copy and
// every already-read inbound message live forever — which is exactly the leak
// this sweep exists to close.
func TestSweepMessagesListsAllDirectionsAndReadStates(t *testing.T) {
	srv, calls := sweepServer(t, nil, nil)
	p := failProbe(srv.URL, "", nil)
	p.SweepMessages()

	var lists []sweepCall
	for _, c := range *calls {
		if c.method == http.MethodGet {
			lists = append(lists, c)
		}
	}
	if len(lists) != 2 {
		t.Fatalf("got %d list calls, want 2 (live, then trash): %+v", len(lists), lists)
	}
	for _, c := range lists {
		for _, want := range []string{"direction=all", "read_status=all", "limit=50"} {
			if !strings.Contains(c.query, want) {
				t.Errorf("list query %q missing %q", c.query, want)
			}
		}
	}
	if strings.Contains(lists[0].query, "deleted=true") {
		t.Errorf("first list should be LIVE messages, got %q", lists[0].query)
	}
	if !strings.Contains(lists[1].query, "deleted=true") {
		t.Errorf("second list should be the TRASH, got %q", lists[1].query)
	}
}

// Cleanup is hygiene, not a health signal: a server that fails every call must
// yield zero counts and no panic, so the caller's battery verdict is untouched.
func TestSweepMessagesIsBestEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	got := failProbe(srv.URL, "", nil).SweepMessages()
	if got.Trashed != 0 || got.Purged != 0 {
		t.Fatalf("SweepMessages() = %+v, want zero counts on a failing server", got)
	}
}

// A 409 (message held for review, or provider submission in flight) must not be
// counted as deleted — it is retried on the next tick.
func TestSweepMessagesDoesNotCountConflicts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("deleted") == "true" {
				_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "next_cursor": nil})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":       []map[string]string{{"id": "msg_held"}},
				"next_cursor": nil,
			})
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"message_held"}}`))
	}))
	defer srv.Close()

	got := failProbe(srv.URL, "", nil).SweepMessages()
	if got.Trashed != 0 {
		t.Fatalf("Trashed = %d, want 0 — a 409 is skipped, not booked as done", got.Trashed)
	}
}
