//go:build integration

package webhook_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/webhook"
)

var deliveryBase = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func seedDeliveryAccount(t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	userID := "usr_" + slug
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, email, name, google_subject) VALUES ($1, $2, '', $1)
	`, userID, userID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	return userID
}

func seedWebhook(t *testing.T, pool *pgxpool.Pool, id, userID, url string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO webhooks (id, user_id, url, signing_secret, events)
		VALUES ($1, $2, $3, 'whsec_test', ARRAY['email.received'])
	`, id, userID, url); err != nil {
		t.Fatal(err)
	}
}

// seedDelivery writes one delivery row. statusCode 0 means "nothing ever
// answered", which is exactly how the worker records a connect failure.
func seedDelivery(t *testing.T, pool *pgxpool.Pool, id, webhookID, status string, statusCode int, at time.Time) {
	t.Helper()
	var code any
	if statusCode > 0 {
		code = statusCode
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO webhook_subscriber_deliveries
		  (id, webhook_id, event_type, event_payload, status, last_status_code, last_attempt_at, created_at)
		VALUES ($1, $2, 'email.received', '{}'::jsonb, $3, $4, $5, $5)
	`, id, webhookID, status, code, at); err != nil {
		t.Fatal(err)
	}
}

// TestDeliveryMetricsSplitsRespondingFromUnreachable pins the only attribution
// the table can prove: an endpoint that ANSWERED with a non-2xx versus one that
// never answered at all. Both are status='failed' with no stored discriminator.
func TestDeliveryMetricsSplitsRespondingFromUnreachable(t *testing.T) {
	pool := testutil.TestDB(t)
	store := webhook.NewSubscriberStore(pool)
	userID := seedDeliveryAccount(t, pool, "whsplit")
	seedWebhook(t, pool, "wh_split", userID, "https://hooks.example.test/ingest?token=secret")

	seedDelivery(t, pool, "wsd_1", "wh_split", "delivered", 200, deliveryBase)
	seedDelivery(t, pool, "wsd_2", "wh_split", "failed", 405, deliveryBase)
	seedDelivery(t, pool, "wsd_3", "wh_split", "failed", 500, deliveryBase)
	seedDelivery(t, pool, "wsd_4", "wh_split", "failed", 0, deliveryBase) // never answered
	seedDelivery(t, pool, "wsd_5", "wh_split", "pending", 0, deliveryBase)

	m, err := store.CountDeliveriesForAccount(context.Background(), userID,
		deliveryBase.Add(-time.Hour), deliveryBase.Add(time.Hour))
	if err != nil {
		t.Fatalf("CountDeliveriesForAccount: %v", err)
	}
	if m.Totals.Total != 5 || m.Totals.Delivered != 1 || m.Totals.Pending != 1 {
		t.Errorf("totals = %+v, want 5/1 delivered/1 pending", m.Totals)
	}
	if m.Totals.EndpointRejected != 2 {
		t.Errorf("endpoint_rejected = %d, want 2 (405 and 500 both answered)", m.Totals.EndpointRejected)
	}
	if m.Totals.NoResponse != 1 {
		t.Errorf("no_response = %d, want 1", m.Totals.NoResponse)
	}
	if m.Totals.Failed() != 3 {
		t.Errorf("failed = %d, want 3", m.Totals.Failed())
	}
}

// TestDeliveryMetricsReportsHostNotFullURL: a webhook URL can carry a shared
// secret in its query string, and a metrics payload must not echo it back.
func TestDeliveryMetricsReportsHostNotFullURL(t *testing.T) {
	pool := testutil.TestDB(t)
	store := webhook.NewSubscriberStore(pool)
	userID := seedDeliveryAccount(t, pool, "whhost")
	seedWebhook(t, pool, "wh_host", userID, "https://hooks.example.test/ingest?token=supersecret")
	seedDelivery(t, pool, "wsd_host", "wh_host", "delivered", 200, deliveryBase)

	m, err := store.CountDeliveriesForAccount(context.Background(), userID,
		deliveryBase.Add(-time.Hour), deliveryBase.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Endpoints) != 1 {
		t.Fatalf("endpoints = %d, want 1", len(m.Endpoints))
	}
	if got := m.Endpoints[0].URLHost; got != "hooks.example.test" {
		t.Errorf("url_host = %q, want the bare host", got)
	}
	if strings.Contains(m.Endpoints[0].URLHost, "supersecret") {
		t.Error("the endpoint secret leaked into the metrics payload")
	}
}

// TestDeliveryMetricsExcludesOtherAccounts guards the tenancy join.
func TestDeliveryMetricsExcludesOtherAccounts(t *testing.T) {
	pool := testutil.TestDB(t)
	store := webhook.NewSubscriberStore(pool)
	mine := seedDeliveryAccount(t, pool, "whmine")
	theirs := seedDeliveryAccount(t, pool, "whtheirs")
	seedWebhook(t, pool, "wh_mine", mine, "https://mine.example.test/h")
	seedWebhook(t, pool, "wh_theirs", theirs, "https://theirs.example.test/h")
	seedDelivery(t, pool, "wsd_mine", "wh_mine", "delivered", 200, deliveryBase)
	for i := 0; i < 4; i++ {
		seedDelivery(t, pool, fmt.Sprintf("wsd_theirs_%d", i), "wh_theirs", "delivered", 200, deliveryBase)
	}

	m, err := store.CountDeliveriesForAccount(context.Background(), mine,
		deliveryBase.Add(-time.Hour), deliveryBase.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if m.Totals.Total != 1 {
		t.Errorf("total = %d, want 1: another account's deliveries leaked in", m.Totals.Total)
	}
}

// TestDeliveryMetricsFlagsRetentionHorizon: delivery history is pruned at 30
// days, unlike messages. Without the flag a 90-day view reads as a collapse in
// webhook volume rather than a retention boundary.
func TestDeliveryMetricsFlagsRetentionHorizon(t *testing.T) {
	pool := testutil.TestDB(t)
	store := webhook.NewSubscriberStore(pool)
	userID := seedDeliveryAccount(t, pool, "whretain")
	now := time.Now()

	// The default window sits exactly on the boundary and must stay quiet —
	// a warning that fires on every page load is ignored by the time a 90-day
	// view actually needs it.
	atBoundary, err := store.CountDeliveriesForAccount(context.Background(), userID, now.Add(-webhook.DeliveryRetention), now)
	if err != nil {
		t.Fatal(err)
	}
	if atBoundary.WindowExceedsRetention {
		t.Error("the default 30-day window sits on the horizon and must not be flagged")
	}

	within, err := store.CountDeliveriesForAccount(context.Background(), userID, now.Add(-20*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if within.WindowExceedsRetention {
		t.Error("a 20-day window is inside retention and must not be flagged")
	}
	beyond, err := store.CountDeliveriesForAccount(context.Background(), userID, now.Add(-90*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if !beyond.WindowExceedsRetention {
		t.Error("a 90-day window reaches past the 30-day horizon and must be flagged")
	}
}

func TestDeliveryMetricsRejectsInvalidInput(t *testing.T) {
	pool := testutil.TestDB(t)
	store := webhook.NewSubscriberStore(pool)
	ctx := context.Background()
	if _, err := store.CountDeliveriesForAccount(ctx, "", deliveryBase, deliveryBase.Add(time.Hour)); err == nil {
		t.Error("expected an error for an empty user id")
	}
	if _, err := store.CountDeliveriesForAccount(ctx, "usr_x", deliveryBase, deliveryBase); err == nil {
		t.Error("expected an error when end == start")
	}
}
