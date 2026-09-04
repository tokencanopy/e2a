package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/sendramp"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
)

func seedOutboundRampAdapter(t *testing.T, suffix string) (*pgxpool.Pool, *sendramp.Store, string, string, string) {
	t.Helper()
	pool := testutil.TestDB(t)
	ctx := context.Background()
	ids := identity.NewStore(pool)
	user, err := ids.CreateOrGetUser(ctx, "adapter-"+suffix+"@example.com", "Adapter", "adapter-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	domain := "adapter-" + suffix + ".example.com"
	if _, err := ids.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE domains SET sending_status='verified' WHERE domain=$1`, domain); err != nil {
		t.Fatal(err)
	}
	ag, err := ids.CreateAgent(ctx, "agent@"+domain, domain, "", "", "local", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := ids.CreateOutboundMessage(ctx, ag.ID, []string{"one@example.net"}, nil, nil, "subject", "send", "smtp", "", "", []byte("raw"))
	if err != nil {
		t.Fatal(err)
	}
	return pool, sendramp.NewStore(pool), user.ID, domain, msg.ID
}

// assertNoRampState asserts the full "the ramp wrote nothing" contract for one
// account: the domain never left 'inactive' and no ledger row was created.
func assertNoRampState(t *testing.T, pool *pgxpool.Pool, userID, domain, messageID string) {
	t.Helper()
	ctx := context.Background()
	var status string
	if err := pool.QueryRow(ctx, `SELECT sending_ramp_status FROM domains WHERE domain=$1 AND user_id=$2`, domain, userID).Scan(&status); err != nil {
		t.Fatalf("read sending_ramp_status: %v", err)
	}
	if status != sendramp.StatusInactive {
		t.Fatalf("sending_ramp_status = %q, want %q: a disabled ramp must not grandfather a domain from the send path", status, sendramp.StatusInactive)
	}
	for _, q := range []struct{ table, sql string }{
		{"sending_ramp_scopes", `SELECT count(*) FROM sending_ramp_scopes WHERE user_id=$1`},
		{"domain_send_counters", `SELECT count(*) FROM domain_send_counters WHERE user_id=$1`},
	} {
		var n int
		if err := pool.QueryRow(ctx, q.sql, userID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", q.table, err)
		}
		if n != 0 {
			t.Fatalf("%s has %d rows, want 0", q.table, n)
		}
	}
	var reservations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sending_ramp_reservations WHERE message_id=$1`, messageID).Scan(&reservations); err != nil {
		t.Fatalf("count sending_ramp_reservations: %v", err)
	}
	if reservations != 0 {
		t.Fatalf("sending_ramp_reservations has %d rows, want 0", reservations)
	}
}

// TestOutboundRampGateDisabledIsPassThrough pins the disabled contract: allow
// the send and write NOTHING. The gate used to stamp the domain 'exempt' on
// every eligible send, which permanently grandfathered any domain that sent
// while the ramp was off — pre-empting the audited one-shot in sendingpolicy
// and, because 'exempt' also reads as "established" to the shared probation
// pool, handing away an abuse bound. Delete-and-re-register made it a repeatable
// reset primitive on top.
func TestOutboundRampGateDisabledIsPassThrough(t *testing.T) {
	pool, store, userID, domain, messageID := seedOutboundRampAdapter(t, "disabled")
	gate := agent.NewOutboundRampGate(store, sendramp.DefaultSchedule, false)
	d, err := gate.Reserve(context.Background(), outboundsend.RampRequest{MessageID: messageID, UserID: userID, Domain: domain, Units: 1})
	if err != nil || !d.Allowed {
		t.Fatalf("Reserve = %+v, %v", d, err)
	}
	assertNoRampState(t, pool, userID, domain, messageID)

	// The read surface (GET /v1/domains/{domain}.sending_ramp.status) therefore
	// reports 'inactive', not 'exempt', for a domain sending under a disabled ramp.
	snap, err := store.Snapshot(context.Background(), userID, domain, time.Now())
	if err != nil || snap.Status != sendramp.StatusInactive {
		t.Fatalf("Snapshot = %+v, %v, want status %q", snap, err, sendramp.StatusInactive)
	}
}

// TestSendWorkerDisabledRampWritesNoRampState is the same contract one level
// out: a real ramp-eligible send (own_address, message_type send) driven
// through the send worker with the ramp disabled must leave the domain
// 'inactive' and the ramp ledger empty.
func TestSendWorkerDisabledRampWritesNoRampState(t *testing.T) {
	api, store, outbox, _, pool := setupAsyncAPIWithPool(t)
	ctx := context.Background()
	user, ag := selfAgent(t, store, "rampdisabled")
	if err := store.SetSendingStatus(ctx, ag.RegisteredDomain, "verified", "verified", "verified", "", nil); err != nil {
		t.Fatalf("SetSendingStatus: %v", err)
	}
	res, oerr := api.DeliverOutbound(ctx, user, ag, outbound.SendRequest{
		To: []string{"recipient@external.test"}, Subject: "disabled ramp send", Body: "x",
	}, "send", "", nil, nil)
	if oerr != nil {
		t.Fatalf("DeliverOutbound: %+v", oerr)
	}

	ramp := agent.NewOutboundRampGate(sendramp.NewStore(pool), sendramp.DefaultSchedule, false)
	deliverer := &countingDeliverer{out: outboundsend.DeliverOutcome{ProviderMessageID: "provider-disabled-ramp", SentAs: "own_address"}}
	worker := outboundsend.NewSendWorker(
		agent.NewOutboundSendStore(store, outbox, usage.NewNoopUsageTracker()), deliverer, ramp)

	if err := worker.Work(ctx, workerJobWithID(res.MessageID, 999, 1)); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if deliverer.calls != 1 {
		t.Fatalf("deliverer calls = %d, want 1: the disabled ramp must still allow the send", deliverer.calls)
	}
	assertNoRampState(t, pool, user.ID, ag.RegisteredDomain, res.MessageID)
}

func TestOutboundRampGateInjectsDayAndDelegatesLifecycle(t *testing.T) {
	_, store, userID, domain, messageID := seedOutboundRampAdapter(t, "enabled")
	day := time.Date(2026, 7, 2, 23, 30, 0, 0, time.FixedZone("west", -7*60*60))
	gate := agent.NewOutboundRampGate(store, sendramp.NewSchedule(50, 100, 2), true, func() time.Time { return day })
	d, err := gate.Reserve(context.Background(), outboundsend.RampRequest{MessageID: messageID, UserID: userID, Domain: domain, Units: 25})
	if err != nil || !d.Allowed {
		t.Fatalf("Reserve = %+v, %v", d, err)
	}
	if err := gate.Confirm(context.Background(), messageID); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Snapshot(context.Background(), userID, domain, day)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ActiveDays != 1 || snap.UsedToday != 25 {
		t.Fatalf("Snapshot = %+v", snap)
	}
}
