package webhooknotify_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/webhooknotify"
)

// The unit tests cover each seam separately: internal/identity proves the sweep
// enqueues inside its transaction, and worker_test proves the worker delivers.
// Nothing joined them, and the seam BETWEEN them is where all four of this
// feature's merge-blocking defects lived. These tests drive the whole durable
// path with a real River client and a real notifier against a fake SMTP:
//
//	failing deliveries → sweep (UPDATE + enqueue in ONE tx) → River → worker
//	                   → notifier → SMTP
//
// mirroring hitlnotify's TestEndToEnd_AcceptTxThroughRiverToSMTP.

// e2eHarness wires a live River client bound to a real notifier.
type e2eHarness struct {
	pool  *pgxpool.Pool
	store *identity.Store
	jobs  *webhooknotify.Jobs
	drain func() []testutil.SMTPMessage
}

func newE2EHarness(t *testing.T, replyTo string) *e2eHarness {
	t.Helper()
	ctx := context.Background()
	pool := testutil.TestDB(t)
	if err := jobs.Migrate(ctx, pool); err != nil {
		t.Fatalf("jobs.Migrate: %v", err)
	}
	store := identity.NewStore(pool)

	smtpAddr, smtpDone := testutil.FakeSMTPServer(t)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{
		Host: smtpAddr.Host, Port: smtpAddr.Port, FromDomain: "notify.test",
	})
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	notifier := webhooknotify.New(store, outbound.NewProviderSubmitter(relay, gate), "notify.test", "", replyTo, "https://app.example.test")

	j := webhooknotify.NewJobs(store).WithGate(gate, pool)
	client, err := jobs.New(pool, jobs.Config{}, j)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	j.SetEnqueuer(client)
	j.SetDeliverer(notifier)

	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})

	return &e2eHarness{pool: pool, store: store, jobs: j, drain: smtpDone}
}

// seedWebhook creates an owner + one webhook.
func (h *e2eHarness) seedWebhook(t *testing.T, slug string) (*identity.User, *identity.Webhook) {
	t.Helper()
	ctx := context.Background()
	user, err := h.store.CreateOrGetUser(ctx, "owner-"+slug+"@reviewer.test", "Owner", "google-whe2e-"+slug)
	if err != nil {
		t.Fatal(err)
	}
	wh, err := h.store.CreateWebhook(ctx, user.ID, "https://hooks.example.com/e2a", "",
		[]string{"email.received"}, identity.WebhookFilters{})
	if err != nil {
		t.Fatal(err)
	}
	return user, wh
}

// seedDeliveries inserts n delivery rows in the given terminal/attempt state.
func (h *e2eHarness) seedDeliveries(t *testing.T, whID, slug, status string, attempts, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := h.pool.Exec(ctx,
			`INSERT INTO webhook_subscriber_deliveries
			    (id, webhook_id, event_type, event_payload, status, attempts, last_error, created_at, last_attempt_at)
			 VALUES ($1, $2, 'email.received', '{}'::jsonb, $3, $4, 'HTTP 404', now() - interval '1 hour', now() - interval '1 hour')`,
			fmt.Sprintf("whd_e2e_%s_%d_%s", slug, i, whID), whID, status, attempts,
		); err != nil {
			t.Fatalf("seed delivery: %v", err)
		}
	}
}

// awaitOneEmail waits for River to finish the notify job, then collects the
// mail. It polls the JOB rather than the mailbox on purpose: testutil's drain
// closure closes the SMTP listener, so it is a one-shot "stop and collect" and
// polling it would kill the server before the worker ever connected.
//
// Scoped to THIS test's webhook id: testutil.TestDB hands out a shared
// database, so an unscoped count sees a sibling test's completed job and
// returns before this test's own mail has been sent.
func (h *e2eHarness) awaitOneEmail(t *testing.T, webhookID string) testutil.SMTPMessage {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		var done int
		if err := h.pool.QueryRow(ctx,
			`SELECT count(*) FROM river_job
			  WHERE kind = 'webhook_notify' AND state = 'completed'
			    AND args->>'webhook_id' = $1`, webhookID,
		).Scan(&done); err != nil {
			t.Fatalf("poll river_job: %v", err)
		}
		if done > 0 {
			msgs := h.drain()
			if len(msgs) != 1 {
				t.Fatalf("notify job completed but fake SMTP got %d messages, want exactly 1", len(msgs))
			}
			return msgs[0]
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("notify job never completed — the sweep→River→worker→SMTP path did not finish. Job states: %s",
		h.jobStates(t, webhookID))
	return testutil.SMTPMessage{}
}

// jobStates summarises the notify jobs for a readable timeout failure: a
// discarded or retryable job with its error is far more useful than "no email".
func (h *e2eHarness) jobStates(t *testing.T, webhookID string) string {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT state, attempt, coalesce(array_to_string(errors, ' | '), '')
		   FROM river_job WHERE kind = 'webhook_notify' AND args->>'webhook_id' = $1`, webhookID)
	if err != nil {
		return fmt.Sprintf("(could not read river_job: %v)", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var state, errs string
		var attempt int
		if err := rows.Scan(&state, &attempt, &errs); err != nil {
			continue
		}
		out = append(out, fmt.Sprintf("state=%s attempt=%d errors=%s", state, attempt, errs))
	}
	if len(out) == 0 {
		return "(no webhook_notify job was ever enqueued)"
	}
	return strings.Join(out, "; ")
}

// The auto-disable path end to end. The sweep flips the webhook AND enqueues the
// job in one transaction; River then drives the worker to a real send.
func TestEndToEnd_DisableSweepThroughRiverToSMTP(t *testing.T) {
	h := newE2EHarness(t, "support@inbox.test")
	ctx := context.Background()
	user, wh := h.seedWebhook(t, "disable")

	// Terminal failures, well past the breaker threshold, none delivered.
	h.seedDeliveries(t, wh.ID, "disable", "failed", 8, identity.AutoDisableThreshold)

	n, err := h.store.AutoDisableFailingWebhooks(ctx, h.jobs.EnqueueDisabledTx)
	if err != nil {
		t.Fatalf("AutoDisableFailingWebhooks: %v", err)
	}
	if n != 1 {
		t.Fatalf("sweep disabled %d webhooks, want 1", n)
	}

	sent := h.awaitOneEmail(t, wh.ID)
	body := string(sent.Data)

	if sent.To != user.Email {
		t.Errorf("notification went to %q, want the account owner %q", sent.To, user.Email)
	}
	if !strings.Contains(body, "Subject: [e2a] webhook disabled: hooks.example.com") {
		t.Errorf("wrong subject for the disable path:\n%s", headBlock(body))
	}
	if !strings.Contains(body, "HTTP 404") {
		t.Errorf("email does not quote the failure reason the sweep captured")
	}
	if !strings.Contains(body, "Reply-To: support@inbox.test") {
		t.Errorf("configured reply_to did not survive the full path:\n%s", headBlock(body))
	}

	// The state transition and the email are two halves of one transaction —
	// assert the row really moved, not just that mail went out.
	after, err := h.store.GetWebhookByID(ctx, wh.ID, user.ID)
	if err != nil {
		t.Fatalf("GetWebhookByID: %v", err)
	}
	if after.Enabled {
		t.Errorf("webhook still enabled after the disable sweep")
	}
	if after.AutoDisabledAt == nil {
		t.Errorf("auto_disabled_at not stamped")
	}
	if after.AutoDisableReason != "HTTP 404" {
		t.Errorf("auto_disable_reason = %q, want the captured endpoint error", after.AutoDisableReason)
	}
}

// The early-warning path end to end. Keyed on ATTEMPT-level failures, so it must
// fire while every delivery is still pending and the breaker cannot yet see them.
func TestEndToEnd_WarnSweepThroughRiverToSMTP(t *testing.T) {
	h := newE2EHarness(t, "support@inbox.test")
	ctx := context.Background()
	user, wh := h.seedWebhook(t, "warn")

	// Pending rows with one failed attempt each: no terminal failure exists yet.
	h.seedDeliveries(t, wh.ID, "warn", "pending", 1, identity.WarnThreshold)

	// The breaker must NOT fire on this evidence — that is the whole point of
	// warning on attempts rather than terminal rows.
	if n, err := h.store.AutoDisableFailingWebhooks(ctx, h.jobs.EnqueueDisabledTx); err != nil || n != 0 {
		t.Fatalf("disable sweep fired on attempt-level failures: n=%d err=%v", n, err)
	}

	n, err := h.store.WarnFailingWebhooks(ctx, h.jobs.EnqueueWarningTx)
	if err != nil {
		t.Fatalf("WarnFailingWebhooks: %v", err)
	}
	if n != 1 {
		t.Fatalf("warn sweep stamped %d webhooks, want 1", n)
	}

	sent := h.awaitOneEmail(t, wh.ID)
	body := string(sent.Data)

	if sent.To != user.Email {
		t.Errorf("warning went to %q, want the account owner %q", sent.To, user.Email)
	}
	if !strings.Contains(body, "Subject: [e2a] webhook delivery failing: hooks.example.com") {
		t.Errorf("wrong subject for the warning path:\n%s", headBlock(body))
	}
	// The warning must stay a warning: the endpoint is still enabled and still
	// being retried, and saying otherwise would send the customer hunting for a
	// disable that has not happened.
	if strings.Contains(body, "has disabled one of your webhooks") {
		t.Errorf("warning email used the disabled copy")
	}
	after, err := h.store.GetWebhookByID(ctx, wh.ID, user.ID)
	if err != nil {
		t.Fatalf("GetWebhookByID: %v", err)
	}
	if !after.Enabled {
		t.Errorf("warn sweep must not disable the webhook")
	}
	if after.WarnNotifiedAt == nil {
		t.Errorf("warn_notified_at not stamped")
	}
}

// A sweep that transitions nothing must send nothing. Guards against a future
// change that enqueues per candidate row rather than per state transition —
// which would mail a customer every five minutes for as long as the endpoint
// stayed broken.
func TestEndToEnd_NoTransitionSendsNoEmail(t *testing.T) {
	h := newE2EHarness(t, "")
	ctx := context.Background()
	_, wh := h.seedWebhook(t, "quiet")

	// One short of both thresholds.
	h.seedDeliveries(t, wh.ID, "quiet", "failed", 8, identity.AutoDisableThreshold-1)

	if n, err := h.store.AutoDisableFailingWebhooks(ctx, h.jobs.EnqueueDisabledTx); err != nil || n != 0 {
		t.Fatalf("disable sweep: n=%d err=%v, want 0", n, err)
	}

	// Give River a real chance to deliver something if a job was wrongly enqueued.
	time.Sleep(2 * time.Second)
	if msgs := h.drain(); len(msgs) != 0 {
		t.Fatalf("a sweep that transitioned nothing sent %d email(s)", len(msgs))
	}
}

// headBlock trims a composed message to its header block for readable output.
func headBlock(data string) string {
	if i := strings.Index(data, "\r\n\r\n"); i > 0 {
		return data[:i]
	}
	if len(data) > 600 {
		return data[:600]
	}
	return data
}
