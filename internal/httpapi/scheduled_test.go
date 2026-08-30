package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
)

func scheduledServer(t *testing.T, list func(ctx context.Context, userID string, limit int, after time.Time, afterID string) ([]identity.ScheduledListItem, error)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			if r.Header.Get("Authorization") == "Bearer good" {
				return &identity.User{ID: "u_1", Email: "owner@acme.com"}, nil
			}
			return nil, errors.New("unauthorized")
		},
		ListScheduled: list,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestScheduled_ListReturnsAcceptedFutureSends(t *testing.T) {
	soon := time.Unix(1800000100, 0).UTC()
	later := time.Unix(1800000200, 0).UTC()
	srv := scheduledServer(t, func(_ context.Context, userID string, _ int, _ time.Time, _ string) ([]identity.ScheduledListItem, error) {
		if userID != "u_1" {
			return nil, errors.New("unexpected user")
		}
		return []identity.ScheduledListItem{
			{ID: "msg_1", AgentID: "support@acme.dev", Direction: "outbound", Sender: "support@acme.dev", To: []string{"cust@x.com"}, Subject: "soonest", DeliveryStatus: "accepted", CreatedAt: soon, ScheduledAt: &soon},
			{ID: "msg_2", AgentID: "support@acme.dev", Direction: "outbound", Sender: "support@acme.dev", To: []string{"cust@x.com"}, Subject: "later", DeliveryStatus: "accepted", CreatedAt: soon, ScheduledAt: &later},
		}, nil
	})
	code, body := getJSON(t, srv.URL+"/v1/scheduled", "good")
	if code != 200 {
		t.Fatalf("status %d body %v", code, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("want 2 scheduled items, got %v", body)
	}
	first, _ := items[0].(map[string]any)
	if first["direction"] != "outbound" {
		t.Errorf("scheduled item direction = %v, want outbound", first["direction"])
	}
	if first["delivery_status"] != "accepted" {
		t.Errorf("scheduled item delivery_status = %v, want accepted", first["delivery_status"])
	}
	if first["scheduled_at"] == nil {
		t.Errorf("scheduled item must carry scheduled_at: %v", first)
	}
	if _, exists := first["authentication"]; exists {
		t.Errorf("scheduled summary must omit full authentication evidence: %v", first)
	}
}

func TestScheduled_RequiresAccountScope(t *testing.T) {
	srv := scheduledServer(t, func(_ context.Context, _ string, _ int, _ time.Time, _ string) ([]identity.ScheduledListItem, error) {
		t.Fatal("ListScheduled must not be called without auth")
		return nil, nil
	})
	code, _ := getJSON(t, srv.URL+"/v1/scheduled", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
}

func TestScheduled_NotImplementedWhenUnwired(t *testing.T) {
	srv := scheduledServer(t, nil)
	code, body := getJSON(t, srv.URL+"/v1/scheduled", "good")
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d body %v, want 501", code, body)
	}
}

func TestScheduled_PaginationEmitsNextCursor(t *testing.T) {
	sa := time.Unix(1800000100, 0).UTC()
	srv := scheduledServer(t, func(_ context.Context, _ string, limit int, _ time.Time, _ string) ([]identity.ScheduledListItem, error) {
		// Return limit rows so the handler's limit+1 over-fetch detects a further page.
		out := make([]identity.ScheduledListItem, 0, limit)
		for i := 0; i < limit; i++ {
			out = append(out, identity.ScheduledListItem{ID: "msg_x", AgentID: "support@acme.dev", Direction: "outbound", Sender: "support@acme.dev", To: []string{"cust@x.com"}, Subject: "s", DeliveryStatus: "accepted", CreatedAt: sa, ScheduledAt: &sa})
		}
		return out, nil
	})
	code, body := getJSON(t, srv.URL+"/v1/scheduled?limit=1", "good")
	if code != 200 {
		t.Fatalf("status %d body %v", code, body)
	}
	if next, ok := body["next_cursor"].(string); !ok || next == "" {
		t.Fatalf("expected a next_cursor on the first page, got %v", body["next_cursor"])
	}
}
