package httpapi

import (
	"context"
	"errors"
	"github.com/tokencanopy/e2a/internal/sendramp"
	"testing"
	"time"
)

func TestAccountDailyLimit(t *testing.T) {
	reset := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	srv := testServer(t, func(d *Deps) {
		d.AccountDailyLimit = func(_ context.Context, user string) (*sendramp.AccountDailyLimit, error) {
			if user != "u_1" {
				t.Errorf("wrong tenant: %s", user)
			}
			return &sendramp.AccountDailyLimit{Limit: 88, Used: 17, SharedLimit: 50, SharedUsed: 6, CleanActiveDays: 1, ResetsAt: reset}, nil
		}
	})
	code, body := getJSON(t, srv.URL+"/v1/account", "good")
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	d, ok := body["daily_limit"].(map[string]any)
	if !ok || d["limit"] != float64(88) || d["used"] != float64(17) || d["resets_at"] != "2026-01-02T00:00:00Z" {
		t.Fatalf("daily_limit: %v", d)
	}
	if _, leaked := d["allowed"]; leaked {
		t.Fatal("internal decision leaked")
	}
}

func TestAccountDailyLimitUnavailable(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.AccountDailyLimit = func(context.Context, string) (*sendramp.AccountDailyLimit, error) {
			return nil, errors.New("unavailable")
		}
	})
	code, body := getJSON(t, srv.URL+"/v1/account", "good")
	if code != 503 || errCode(body) != "limits_unavailable" {
		t.Fatalf("%d %v", code, body)
	}
}
