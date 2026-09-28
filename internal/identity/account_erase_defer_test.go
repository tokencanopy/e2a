package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// Deferred erase for recent external senders
// (docs/design/account-soft-deletion.md, "Deferred erase for recent senders").

const deferSharedDomain = "agents.localhost"

type deferFixture struct {
	store  *identity.Store
	pool   *pgxpool.Pool
	userID string
	agent  string
}

// newDeferFixture seeds an account with one shared-domain agent, a sending
// control row and a bounce/complaint aggregate row.
func newDeferFixture(t *testing.T, slug string) deferFixture {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	if err := store.EnsureSharedDomain(ctx, deferSharedDomain); err != nil {
		t.Fatal(err)
	}
	user, err := store.CreateOrGetUser(ctx, slug+"@example.test", "Owner", "sub-"+slug)
	if err != nil {
		t.Fatal(err)
	}
	ag, err := store.CreateAgent(ctx, slug+"-bot@"+deferSharedDomain, deferSharedDomain, "Bot", "", "cloud", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO account_sending_controls (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO account_sending_outcomes_daily (user_id, outcome_epoch, day, shared_reputation, delivered_count, complaint_count)
		VALUES ($1, 1, current_date, true, 10, 0)`, user.ID); err != nil {
		t.Fatal(err)
	}
	return deferFixture{store: store, pool: pool, userID: user.ID, agent: ag.ID}
}

// sent records an outbound message from the fixture's agent that the
// provider accepted `ago` in the past, with one sent recipient row per
// address — the rows the send worker writes on provider acceptance.
func (f deferFixture) sent(t *testing.T, id string, ago time.Duration, recipients ...string) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().Add(-ago)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO messages (id, agent_id, direction, sender, recipient, subject, delivery_status, created_at, provider_accepted_at)
		VALUES ($1, $2, 'outbound', $2, $3, 'hello', 'sent', $4, $4)`,
		id, f.agent, recipients[0], at); err != nil {
		t.Fatalf("seed outbound message: %v", err)
	}
	for i, r := range recipients {
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO message_recipients (id, message_id, address, kind, status, updated_at)
			VALUES ($1, $2, $3, 'to', 'sent', $4)`,
			id+"_r"+string(rune('a'+i)), id, r, at); err != nil {
			t.Fatalf("seed recipient: %v", err)
		}
	}
}

func (f deferFixture) userExists(t *testing.T) (exists bool, deletedAt *time.Time) {
	t.Helper()
	err := f.pool.QueryRow(context.Background(),
		`SELECT true, deleted_at FROM users WHERE id = $1`, f.userID).Scan(&exists, &deletedAt)
	if err != nil {
		return false, nil
	}
	return exists, deletedAt
}

func TestEraseIsDeferredForARecentExternalSender(t *testing.T) {
	f := newDeferFixture(t, "recent")
	ctx := context.Background()
	f.sent(t, "msg_defer_recent", 24*time.Hour, "someone@example.com")

	res, err := f.store.EraseAccount(ctx, f.userID, nil)
	if err != nil {
		t.Fatalf("EraseAccount: %v", err)
	}
	if res.Mode != identity.AccountDeleteModeTrash || !res.EraseDeferred || res.UserDeleted || res.Message == "" {
		t.Fatalf("receipt = %+v, want mode trash, erase_deferred, a message, user not deleted", res)
	}
	exists, deletedAt := f.userExists(t)
	if !exists || deletedAt == nil {
		t.Fatalf("user exists=%v deleted_at=%v, want a trashed row", exists, deletedAt)
	}
	if res.PurgeAfter == nil || !res.PurgeAfter.Equal(deletedAt.Add(identity.AccountTrashRetention)) {
		t.Fatalf("purge_after = %v, want deleted_at + account retention (%v)", res.PurgeAfter, deletedAt.Add(identity.AccountTrashRetention))
	}
	if res.AgentsDeleted != 1 {
		t.Fatalf("agents trashed = %d, want 1 (the trash receipt counts)", res.AgentsDeleted)
	}
	// The sending control row and the feedback aggregates survive, so late
	// provider feedback still has something to count against.
	var controls, aggregates, msgs int
	if err := f.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM account_sending_controls WHERE user_id = $1),
		       (SELECT count(*) FROM account_sending_outcomes_daily WHERE user_id = $1),
		       (SELECT count(*) FROM messages WHERE id = 'msg_defer_recent')`, f.userID,
	).Scan(&controls, &aggregates, &msgs); err != nil {
		t.Fatal(err)
	}
	if controls != 1 || aggregates != 1 || msgs != 1 {
		t.Fatalf("controls=%d aggregates=%d messages=%d after a deferred erase, want 1/1/1", controls, aggregates, msgs)
	}

	// A second "erase now" on the trashed account (the restore interstitial)
	// is deferred again, with nothing new trashed.
	again, err := f.store.EraseAccount(ctx, f.userID, nil)
	if err != nil {
		t.Fatalf("second EraseAccount: %v", err)
	}
	if !again.EraseDeferred || again.Mode != identity.AccountDeleteModeTrash || again.AgentsDeleted != 0 ||
		again.PurgeAfter == nil || !again.PurgeAfter.Equal(*res.PurgeAfter) {
		t.Fatalf("second receipt = %+v, want deferred trash receipt with the same purge_after and zero counts", again)
	}

	// Restore works exactly as for any trashed account.
	tok, err := f.store.CreateRestrictedUserSession(ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := f.store.RestoreAccount(ctx, f.userID, tok)
	if err != nil {
		t.Fatalf("RestoreAccount after a deferred erase: %v", err)
	}
	if restored.DeletedAt != nil {
		t.Fatalf("restored deleted_at = %v, want nil", restored.DeletedAt)
	}
}

func TestEraseIsImmediateWhenTheLastExternalSendIsOutsideTheWindow(t *testing.T) {
	f := newDeferFixture(t, "old")
	f.sent(t, "msg_defer_old", 15*24*time.Hour, "someone@example.com")

	res, err := f.store.EraseAccount(context.Background(), f.userID, nil)
	if err != nil {
		t.Fatalf("EraseAccount: %v", err)
	}
	if res.Mode != identity.AccountDeleteModePermanent || res.EraseDeferred || !res.UserDeleted {
		t.Fatalf("receipt = %+v, want an immediate permanent erase", res)
	}
	if exists, _ := f.userExists(t); exists {
		t.Fatal("user row survived an erase whose last external send was 15 days ago")
	}
}

func TestEraseIsImmediateWhenEverySendWasInternal(t *testing.T) {
	f := newDeferFixture(t, "internal")
	ctx := context.Background()
	// A second agent of the same account on a custom domain, a verified owner
	// mailbox, and another account's agent on the shared domain.
	if _, err := f.store.ClaimOrCreateDomain(ctx, "internal.example.test", f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateAgent(ctx, "desk@internal.example.test", "internal.example.test", "Desk", "", "cloud", f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE users SET owner_email_verified_address = lower(email), owner_email_verified_at = now(), owner_email_verified_source = 'google_oauth'
		 WHERE id = $1`, f.userID); err != nil {
		t.Fatal(err)
	}
	f.sent(t, "msg_defer_int", time.Hour,
		"desk@internal.example.test", "internal@example.test", "stranger-bot@"+deferSharedDomain, f.agent)

	res, err := f.store.EraseAccount(ctx, f.userID, nil)
	if err != nil {
		t.Fatalf("EraseAccount: %v", err)
	}
	if res.EraseDeferred || !res.UserDeleted {
		t.Fatalf("receipt = %+v, want an immediate erase (no external recipient)", res)
	}
}

func TestUnverifiedOwnerMailboxCountsAsExternal(t *testing.T) {
	f := newDeferFixture(t, "unproven")
	f.sent(t, "msg_defer_unproven", time.Hour, "unproven@example.test")

	res, err := f.store.EraseAccount(context.Background(), f.userID, nil)
	if err != nil {
		t.Fatalf("EraseAccount: %v", err)
	}
	if !res.EraseDeferred {
		t.Fatalf("receipt = %+v, want deferred: an unproven owner mailbox is external, as for external sending access", res)
	}
}

func TestRecentlyApprovedOldMessageCountsAsARecentSend(t *testing.T) {
	f := newDeferFixture(t, "held")
	ctx := context.Background()
	f.sent(t, "msg_defer_held", 20*24*time.Hour, "someone@example.com")
	// Created 20 days ago but submitted to the provider an hour ago (a
	// review hold or a schedule).
	if _, err := f.pool.Exec(ctx,
		`UPDATE messages SET provider_accepted_at = now() - interval '1 hour' WHERE id = 'msg_defer_held'`); err != nil {
		t.Fatal(err)
	}
	res, err := f.store.EraseAccount(ctx, f.userID, nil)
	if err != nil {
		t.Fatalf("EraseAccount: %v", err)
	}
	if !res.EraseDeferred {
		t.Fatalf("receipt = %+v, want deferred for a send accepted an hour ago", res)
	}
}

func TestEraseDeferralWindowZeroNeverDefers(t *testing.T) {
	prev := identity.RecentSenderEraseDefer
	identity.RecentSenderEraseDefer = 0
	t.Cleanup(func() { identity.RecentSenderEraseDefer = prev })

	f := newDeferFixture(t, "zero")
	f.sent(t, "msg_defer_zero", time.Hour, "someone@example.com")
	res, err := f.store.EraseAccount(context.Background(), f.userID, nil)
	if err != nil {
		t.Fatalf("EraseAccount: %v", err)
	}
	if res.EraseDeferred || !res.UserDeleted {
		t.Fatalf("receipt = %+v, want an immediate erase with the window disabled", res)
	}
}

func TestPausedRecentSenderIsStillEraseHeld(t *testing.T) {
	f := newDeferFixture(t, "paused")
	ctx := context.Background()
	f.sent(t, "msg_defer_paused", time.Hour, "someone@example.com")
	if _, err := f.pool.Exec(ctx,
		`UPDATE account_sending_controls SET state = 'paused', reason = 'r', actor = 'op', pause_class = 'operator' WHERE user_id = $1`,
		f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EraseAccount(ctx, f.userID, nil); !errors.Is(err, identity.ErrEraseHeld) {
		t.Fatalf("EraseAccount of a paused recent sender err = %v, want ErrEraseHeld", err)
	}
	if exists, deletedAt := f.userExists(t); !exists || deletedAt != nil {
		t.Fatalf("user exists=%v deleted_at=%v, want the account untouched by a held erase", exists, deletedAt)
	}
}

func TestOperatorForcePurgeStillPurgesADeferredAccount(t *testing.T) {
	f := newDeferFixture(t, "force")
	ctx := context.Background()
	f.sent(t, "msg_defer_force", time.Hour, "someone@example.com")
	if res, err := f.store.EraseAccount(ctx, f.userID, nil); err != nil || !res.EraseDeferred {
		t.Fatalf("EraseAccount = %+v err=%v, want deferred", res, err)
	}
	// Runbook: backdate deleted_at past the window, then let the janitor run.
	ageTrash(t, f.pool, f.userID)
	purged, err := f.store.PurgeDeletedUsers(ctx, nil)
	if err != nil {
		t.Fatalf("PurgeDeletedUsers: %v", err)
	}
	if len(purged) != 1 || purged[0] != f.userID {
		t.Fatalf("purged = %v, want the deferred account", purged)
	}
}
