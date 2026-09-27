package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
)

// Accepted-but-unmetered outbound sends must count against the flow caps: the
// terminal metering write is the only thing that lands in usage_summaries, so
// without counting the accepted rows the cap reads the same pre-increment
// total for every send already in flight. This pins the counter's new
// accept-time reservation semantics and the rows it must NOT count.
func TestMessagesCountsAcceptedButUnmeteredSends(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DB-backed usage test under -short")
	}
	pool := testutil.TestDB(t)
	store := usage.NewStore(pool)
	idStore := identity.NewStore(pool)
	ctx := context.Background()

	user, err := idStore.CreateOrGetUser(ctx, "pending-quota@example.com", "Pending", "google-pending-quota")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := idStore.ClaimOrCreateDomain(ctx, "pending.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	agent, err := idStore.CreateAgent(ctx, "bot@pending.example.com", "pending.example.com", "", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Two accepted immediate sends: one with two recipients, one with one.
	accept := func(to []string, schedule time.Time) {
		t.Helper()
		if err := idStore.WithTx(ctx, func(tx pgx.Tx) error {
			msg, err := idStore.CreateOutboundMessageTx(ctx, tx, agent.ID, to, nil, nil,
				"pending quota", "send", "smtp", "", "", []byte("raw"), "accepted", "", "")
			if err != nil {
				return err
			}
			if !schedule.IsZero() {
				return idStore.StampScheduledAtTx(ctx, tx, msg.ID, schedule)
			}
			return nil
		}); err != nil {
			t.Fatalf("accept send: %v", err)
		}
	}

	accept([]string{"alice@example.com", "bob@example.com"}, time.Time{})
	accept([]string{"carol@example.com"}, time.Time{})

	got, err := store.MessagesThisMonth(ctx, user.ID)
	if err != nil {
		t.Fatalf("MessagesThisMonth: %v", err)
	}
	if got != 3 {
		t.Errorf("MessagesThisMonth = %d, want 3 (2-recipient + 1-recipient accepted sends)", got)
	}
	got, err = store.MessagesToday(ctx, user.ID)
	if err != nil {
		t.Fatalf("MessagesToday: %v", err)
	}
	if got != 3 {
		t.Errorf("MessagesToday = %d, want 3", got)
	}

	// A scheduled send's flow cap is judged against the month it fires in, and
	// a review hold is re-checked when released: neither is an accept-time
	// reservation.
	accept([]string{"dave@example.com", "erin@example.com"}, time.Now().UTC().Add(time.Hour))
	if _, err := idStore.CreatePendingOutboundMessage(ctx, agent.ID, []string{"frank@example.com"}, nil, nil,
		"held", "body", "", nil, "send", "", "", "", 3600); err != nil {
		t.Fatalf("CreatePendingOutboundMessage: %v", err)
	}
	got, err = store.MessagesThisMonth(ctx, user.ID)
	if err != nil {
		t.Fatalf("MessagesThisMonth after scheduled + held: %v", err)
	}
	if got != 3 {
		t.Errorf("MessagesThisMonth = %d, want 3 (scheduled and held sends are not reservations)", got)
	}

	// Terminal send: the metering write lands in usage_summaries and the row
	// leaves the accepted set, so the total must not double count.
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET delivery_status = 'sent' WHERE agent_id = $1 AND to_recipients = ARRAY['alice@example.com','bob@example.com']`,
		agent.ID,
	); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	if err := store.IncrementUsageSummary(ctx, user.ID, usage.CurrentDate(), "outbound", 2); err != nil {
		t.Fatalf("IncrementUsageSummary: %v", err)
	}
	got, err = store.MessagesThisMonth(ctx, user.ID)
	if err != nil {
		t.Fatalf("MessagesThisMonth after terminal: %v", err)
	}
	if got != 3 {
		t.Errorf("MessagesThisMonth = %d, want 3 (metered 2 + still-accepted 1, no double count)", got)
	}
}
