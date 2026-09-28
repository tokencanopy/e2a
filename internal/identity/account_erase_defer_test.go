package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

// ── Agent- and message-level deferral: the account check's evidence cannot
// be purged on demand inside the window. ──

func (f deferFixture) agentCreatedAt(t *testing.T, agentID string) time.Time {
	t.Helper()
	var at time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT created_at FROM agent_identities WHERE id = $1`, agentID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func (f deferFixture) agentDeletedAt(t *testing.T, agentID string) (exists bool, deletedAt *time.Time) {
	t.Helper()
	err := f.pool.QueryRow(context.Background(),
		`SELECT true, deleted_at FROM agent_identities WHERE id = $1`, agentID).Scan(&exists, &deletedAt)
	if err != nil {
		return false, nil
	}
	return exists, deletedAt
}

func TestPermanentAgentDeleteIsDeferredForARecentExternalSender(t *testing.T) {
	f := newDeferFixture(t, "agentrecent")
	ctx := context.Background()
	f.sent(t, "msg_agent_recent", 24*time.Hour, "someone@example.com")

	res, err := f.store.PermanentDeleteAgentIncarnation(ctx, f.agent, f.userID, f.agentCreatedAt(t, f.agent))
	if err != nil {
		t.Fatalf("PermanentDeleteAgentIncarnation: %v", err)
	}
	if !res.EraseDeferred || res.MessagesDeleted != 0 || res.PurgeAfter == nil {
		t.Fatalf("result = %+v, want deferred with purge_after and nothing deleted", res)
	}
	exists, deletedAt := f.agentDeletedAt(t, f.agent)
	if !exists || deletedAt == nil {
		t.Fatalf("agent exists=%v deleted_at=%v, want a trashed row", exists, deletedAt)
	}
	if !res.PurgeAfter.Equal(deletedAt.Add(identity.TrashRetention)) {
		t.Fatalf("purge_after = %v, want deleted_at + trash retention", res.PurgeAfter)
	}
	var msgs int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM message_recipients WHERE message_id = 'msg_agent_recent'`).Scan(&msgs); err != nil {
		t.Fatal(err)
	}
	if msgs != 1 {
		t.Fatalf("recipient evidence rows = %d after a deferred agent delete, want 1", msgs)
	}
	// A second permanent delete of the now-trashed agent is deferred again,
	// and the int-returning wrapper reports it as ErrPurgeDeferred.
	if _, err := f.store.DeleteAgent(ctx, f.agent, f.userID); !errors.Is(err, identity.ErrPurgeDeferred) {
		t.Fatalf("DeleteAgent of the trashed recent sender err = %v, want ErrPurgeDeferred", err)
	}
	// Restore works as for any trashed agent.
	if _, err := f.store.RestoreAgent(ctx, f.agent, f.userID); err != nil {
		t.Fatalf("RestoreAgent after a deferred delete: %v", err)
	}
	// And the account erase is still deferred.
	acct, err := f.store.EraseAccount(ctx, f.userID, nil)
	if err != nil || !acct.EraseDeferred {
		t.Fatalf("EraseAccount after the agent delete = %+v err=%v, want deferred", acct, err)
	}
}

func TestPermanentAgentDeleteOfAnInternalOnlySenderPurges(t *testing.T) {
	f := newDeferFixture(t, "agentinternal")
	ctx := context.Background()
	f.sent(t, "msg_agent_internal", time.Hour, "peer-bot@"+deferSharedDomain, f.agent)

	res, err := f.store.PermanentDeleteAgentIncarnation(ctx, f.agent, f.userID, f.agentCreatedAt(t, f.agent))
	if err != nil {
		t.Fatalf("PermanentDeleteAgentIncarnation: %v", err)
	}
	if res.EraseDeferred || res.MessagesDeleted != 1 {
		t.Fatalf("result = %+v, want an immediate purge of 1 message", res)
	}
	if exists, _ := f.agentDeletedAt(t, f.agent); exists {
		t.Fatal("agent row survived a purge with only internal sends")
	}
}

func TestPermanentAgentDeleteOutsideTheWindowPurges(t *testing.T) {
	f := newDeferFixture(t, "agentold")
	f.sent(t, "msg_agent_old", 15*24*time.Hour, "someone@example.com")
	if n, err := f.store.DeleteAgent(context.Background(), f.agent, f.userID); err != nil || n != 1 {
		t.Fatalf("DeleteAgent = %d err=%v, want 1 message purged", n, err)
	}
}

func TestPausedAccountAgentDeleteStaysEraseHeld(t *testing.T) {
	f := newDeferFixture(t, "agentpaused")
	ctx := context.Background()
	f.sent(t, "msg_agent_paused", time.Hour, "someone@example.com")
	if _, err := f.pool.Exec(ctx,
		`UPDATE account_sending_controls SET state = 'paused', reason = 'r', actor = 'op', pause_class = 'operator' WHERE user_id = $1`,
		f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DeleteAgent(ctx, f.agent, f.userID); !errors.Is(err, identity.ErrEraseHeld) {
		t.Fatalf("DeleteAgent on a paused account err = %v, want ErrEraseHeld", err)
	}
	if _, deletedAt := f.agentDeletedAt(t, f.agent); deletedAt != nil {
		t.Fatal("a held permanent delete trashed the agent")
	}
}

func TestPermanentMessageDeleteIsDeferredForARecentExternalSend(t *testing.T) {
	f := newDeferFixture(t, "msgrecent")
	ctx := context.Background()
	f.sent(t, "msg_purge_recent", time.Hour, "someone@example.com")
	f.sent(t, "msg_purge_internal", time.Hour, "peer-bot@"+deferSharedDomain)
	for _, id := range []string{"msg_purge_recent", "msg_purge_internal"} {
		if err := f.store.SoftDeleteMessage(ctx, id, f.agent); err != nil {
			t.Fatalf("SoftDeleteMessage %s: %v", id, err)
		}
	}

	res, err := f.store.PurgeMessageOrDefer(ctx, "msg_purge_recent", f.agent)
	if err != nil {
		t.Fatalf("PurgeMessageOrDefer: %v", err)
	}
	var deletedAt time.Time
	if err := f.pool.QueryRow(ctx, `SELECT deleted_at FROM messages WHERE id = 'msg_purge_recent'`).Scan(&deletedAt); err != nil {
		t.Fatalf("externally sent message was purged: %v", err)
	}
	if !res.EraseDeferred || res.PurgeAfter == nil || !res.PurgeAfter.Equal(deletedAt.Add(identity.TrashRetention)) {
		t.Fatalf("result = %+v, want deferred with purge_after = deleted_at + trash retention", res)
	}
	if err := f.store.PurgeMessage(ctx, "msg_purge_recent", f.agent); !errors.Is(err, identity.ErrPurgeDeferred) {
		t.Fatalf("PurgeMessage err = %v, want ErrPurgeDeferred", err)
	}

	// A message sent only internally is purged at once.
	if res, err := f.store.PurgeMessageOrDefer(ctx, "msg_purge_internal", f.agent); err != nil || res.EraseDeferred {
		t.Fatalf("internal message purge = %+v err=%v, want purged", res, err)
	}
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE id = 'msg_purge_internal'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("internal message rows = %d err=%v, want 0", n, err)
	}
}

func TestWindowZeroDisablesAgentAndMessageDeferral(t *testing.T) {
	prev := identity.RecentSenderEraseDefer
	identity.RecentSenderEraseDefer = 0
	t.Cleanup(func() { identity.RecentSenderEraseDefer = prev })

	f := newDeferFixture(t, "zeroagent")
	ctx := context.Background()
	f.sent(t, "msg_zero_a", time.Hour, "someone@example.com")
	f.sent(t, "msg_zero_b", time.Hour, "someone@example.com")
	if err := f.store.SoftDeleteMessage(ctx, "msg_zero_a", f.agent); err != nil {
		t.Fatal(err)
	}
	if res, err := f.store.PurgeMessageOrDefer(ctx, "msg_zero_a", f.agent); err != nil || res.EraseDeferred {
		t.Fatalf("message purge with window 0 = %+v err=%v, want purged", res, err)
	}
	if n, err := f.store.DeleteAgent(ctx, f.agent, f.userID); err != nil || n != 1 {
		t.Fatalf("agent purge with window 0 = %d err=%v, want 1 message purged", n, err)
	}
}

// The original bypass, end to end: send externally, try to permanently
// delete the sending agent (and its sent message), then erase the account.
// The evidence survives and the account erase is deferred.
func TestBypassDeleteAgentsThenEraseIsStillDeferred(t *testing.T) {
	f := newDeferFixture(t, "bypass")
	ctx := context.Background()
	f.sent(t, "msg_bypass", time.Hour, "someone@example.com")

	if err := f.store.SoftDeleteMessage(ctx, "msg_bypass", f.agent); err != nil {
		t.Fatal(err)
	}
	if res, err := f.store.PurgeMessageOrDefer(ctx, "msg_bypass", f.agent); err != nil || !res.EraseDeferred {
		t.Fatalf("message purge = %+v err=%v, want deferred", res, err)
	}
	if res, err := f.store.PermanentDeleteAgentIncarnation(ctx, f.agent, f.userID, f.agentCreatedAt(t, f.agent)); err != nil || !res.EraseDeferred {
		t.Fatalf("agent purge = %+v err=%v, want deferred", res, err)
	}
	acct, err := f.store.EraseAccount(ctx, f.userID, nil)
	if err != nil {
		t.Fatalf("EraseAccount: %v", err)
	}
	if !acct.EraseDeferred || acct.UserDeleted {
		t.Fatalf("account erase after the bypass sequence = %+v, want deferred", acct)
	}
	var recipients int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM message_recipients WHERE message_id = 'msg_bypass'`).Scan(&recipients); err != nil || recipients != 1 {
		t.Fatalf("recipient evidence rows = %d err=%v, want 1", recipients, err)
	}
}

// ── Review hardening: exemptions, parsing, unsettled sends, scheduled sends ──

func TestProviderSimulatorRecipientsNeverDefer(t *testing.T) {
	f := newDeferFixture(t, "simulator")
	ctx := context.Background()
	f.sent(t, "msg_sim", time.Hour, "success@simulator.amazonses.com", "bounce@SIMULATOR.amazonses.com")
	if n, err := f.store.DeleteAgent(ctx, f.agent, f.userID); err != nil || n != 1 {
		t.Fatalf("DeleteAgent of a simulator-only sender = %d err=%v, want purged", n, err)
	}
	res, err := f.store.EraseAccount(ctx, f.userID, nil)
	if err != nil || res.EraseDeferred || !res.UserDeleted {
		t.Fatalf("EraseAccount = %+v err=%v, want an immediate erase", res, err)
	}
}

func TestConfiguredExemptDomainsAreRespected(t *testing.T) {
	prev := identity.EraseDeferExemptDomains
	identity.EraseDeferExemptDomains = []string{"sink.example.test"}
	t.Cleanup(func() { identity.EraseDeferExemptDomains = prev })

	f := newDeferFixture(t, "exemptcfg")
	f.sent(t, "msg_exempt_sink", time.Hour, "probe@sink.example.test")
	f.sent(t, "msg_exempt_sim", time.Hour, "success@simulator.amazonses.com")
	res, err := f.store.PermanentDeleteAgentIncarnation(context.Background(), f.agent, f.userID, f.agentCreatedAt(t, f.agent))
	if err != nil {
		t.Fatal(err)
	}
	// The configured list replaces the default: the simulator is external now.
	if !res.EraseDeferred {
		t.Fatalf("result = %+v, want deferred (simulator no longer exempt)", res)
	}
}

// The shared agent domain is exempt BY NAME even when its domains row is
// owned by an account (the probe account adopts it on some deployments).
func TestSharedDomainByNameIsInternalEvenWhenOwned(t *testing.T) {
	f := newDeferFixture(t, "shareowned")
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE domains SET user_id = $1 WHERE domain = $2`, f.userID, deferSharedDomain); err != nil {
		t.Fatal(err)
	}
	f.sent(t, "msg_share_owned", time.Hour, "someone-elses-bot@"+deferSharedDomain)

	sent, err := f.store.AccountSentExternallySince(ctx, f.userID, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !sent {
		t.Fatal("without the name exemption an owned shared domain should count as external (test precondition)")
	}
	prev := identity.EraseDeferExemptDomains
	identity.EraseDeferExemptDomains = append(append([]string(nil), prev...), deferSharedDomain)
	t.Cleanup(func() { identity.EraseDeferExemptDomains = prev })
	if sent, err := f.store.AccountSentExternallySince(ctx, f.userID, time.Now().Add(-24*time.Hour)); err != nil || sent {
		t.Fatalf("with the shared domain exempt by name: sent=%v err=%v, want false", sent, err)
	}
}

func TestSystemAndInternalAccountsNeverDefer(t *testing.T) {
	for _, class := range []string{"system", "internal"} {
		t.Run(class, func(t *testing.T) {
			f := newDeferFixture(t, "class"+class)
			ctx := context.Background()
			if _, err := f.pool.Exec(ctx, `UPDATE users SET account_class = $2 WHERE id = $1`, f.userID, class); err != nil {
				t.Fatal(err)
			}
			f.sent(t, "msg_class_"+class, time.Hour, "someone@example.com")
			if n, err := f.store.DeleteAgent(ctx, f.agent, f.userID); err != nil || n != 1 {
				t.Fatalf("DeleteAgent = %d err=%v, want purged", n, err)
			}
			if res, err := f.store.EraseAccount(ctx, f.userID, nil); err != nil || res.EraseDeferred {
				t.Fatalf("EraseAccount = %+v err=%v, want an immediate erase", res, err)
			}
		})
	}
}

// A quoted local part containing '@' must not pose as an internal domain:
// the domain is the part after the LAST '@'.
func TestQuotedLocalPartCannotPoseAsTheSharedDomain(t *testing.T) {
	f := newDeferFixture(t, "quoted")
	f.sent(t, "msg_quoted", time.Hour, `"x@`+deferSharedDomain+`@y"@victim.example.test`)
	res, err := f.store.PermanentDeleteAgentIncarnation(context.Background(), f.agent, f.userID, f.agentCreatedAt(t, f.agent))
	if err != nil {
		t.Fatal(err)
	}
	if !res.EraseDeferred {
		t.Fatalf("result = %+v, want deferred: the recipient's domain is victim.example.test", res)
	}
}

// A message the provider accepted but that has not settled yet has no
// message_recipients rows; its own recipient lists decide.
func TestUnsettledProviderAcceptedSendCounts(t *testing.T) {
	f := newDeferFixture(t, "unsettled")
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO messages (id, agent_id, direction, sender, recipient, subject, delivery_status,
		                      provider_message_id, send_claimed_at, to_recipients, cc)
		VALUES ('msg_unsettled_int', $1, 'outbound', $1, $2, 's', 'sending', 'ses-int-1', now(), ARRAY[$2], ARRAY['peer@`+deferSharedDomain+`']),
		       ('msg_unsettled_ext', $1, 'outbound', $1, 'x@example.com', 's', 'sending', 'ses-ext-1', now(), ARRAY[$2], ARRAY['Someone@Example.com'])`,
		f.agent, f.agent); err != nil {
		t.Fatal(err)
	}
	if sent, err := f.store.AccountSentExternallySince(ctx, f.userID, time.Now().Add(-time.Hour)); err != nil || !sent {
		t.Fatalf("unsettled external send: sent=%v err=%v, want true", sent, err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM messages WHERE id = 'msg_unsettled_ext'`); err != nil {
		t.Fatal(err)
	}
	if sent, err := f.store.AccountSentExternallySince(ctx, f.userID, time.Now().Add(-time.Hour)); err != nil || sent {
		t.Fatalf("unsettled internal-only send: sent=%v err=%v, want false", sent, err)
	}
}

// Arm B: an old review-held message approved inside the window counts even
// though created_at is beyond the retry-lag lower bound.
func TestOldHeldMessageApprovedRecentlyCounts(t *testing.T) {
	f := newDeferFixture(t, "oldheld")
	ctx := context.Background()
	f.sent(t, "msg_old_held", 60*24*time.Hour, "someone@example.com")
	if _, err := f.pool.Exec(ctx,
		`UPDATE messages SET reviewed_at = now() - interval '1 hour', provider_accepted_at = NULL WHERE id = 'msg_old_held'`); err != nil {
		t.Fatal(err)
	}
	if sent, err := f.store.AccountSentExternallySince(ctx, f.userID, time.Now().Add(-24*time.Hour)); err != nil || !sent {
		t.Fatalf("old held message approved an hour ago: sent=%v err=%v, want true", sent, err)
	}
}

type deferRecordingCanceller struct{ jobIDs []int64 }

func (c *deferRecordingCanceller) CancelTx(_ context.Context, _ pgx.Tx, jobID int64) error {
	c.jobIDs = append(c.jobIDs, jobID)
	return nil
}

// A deferred permanent agent delete cancels the agent's pending scheduled
// sends: a later restore must not re-arm them.
func TestDeferredAgentDeleteCancelsScheduledSends(t *testing.T) {
	f := newDeferFixture(t, "schedcancel")
	ctx := context.Background()
	canceller := &deferRecordingCanceller{}
	f.store.SetOutboundJobCanceller(canceller)
	wireTrashScheduledFinalizer(f.store, f.pool)
	f.sent(t, "msg_sched_evidence", time.Hour, "someone@example.com")
	pending := trashOutbound(t, f.store, f.agent, "scheduled")
	linkTrashTestSendJob(t, f.pool, pending.ID, 701)
	if _, err := f.pool.Exec(ctx,
		`UPDATE messages SET scheduled_at = now() + interval '1 day' WHERE id = $1`, pending.ID); err != nil {
		t.Fatal(err)
	}

	res, err := f.store.PermanentDeleteAgentIncarnation(ctx, f.agent, f.userID, f.agentCreatedAt(t, f.agent))
	if err != nil || !res.EraseDeferred {
		t.Fatalf("result = %+v err=%v, want deferred", res, err)
	}
	if len(canceller.jobIDs) != 1 || canceller.jobIDs[0] != 701 {
		t.Fatalf("cancelled jobs = %v, want [701]", canceller.jobIDs)
	}
	if _, err := f.store.RestoreAgent(ctx, f.agent, f.userID); err != nil {
		t.Fatalf("RestoreAgent: %v", err)
	}
	var status, detail string
	if err := f.pool.QueryRow(ctx,
		`SELECT delivery_status, COALESCE(delivery_detail, '') FROM messages WHERE id = $1`, pending.ID,
	).Scan(&status, &detail); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || detail != identity.ScheduledCancelDeferredPurge {
		t.Fatalf("scheduled message after deferred delete + restore = %q %q, want failed with the deferred-purge detail", status, detail)
	}
	if len(canceller.jobIDs) != 1 {
		t.Fatalf("restore re-touched jobs: %v", canceller.jobIDs)
	}
}

// A send claimed inside the window (the provider call may have gone out) with
// no provider id yet also counts, classified by its own recipient lists.
func TestClaimedUnacceptedSendCounts(t *testing.T) {
	f := newDeferFixture(t, "claimed")
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO messages (id, agent_id, direction, sender, recipient, subject, delivery_status,
		                      send_claimed_at, to_recipients)
		VALUES ('msg_claimed', $1, 'outbound', $1, 'x@example.com', 's', 'sending', now(), ARRAY['x@example.com'])`,
		f.agent); err != nil {
		t.Fatal(err)
	}
	if sent, err := f.store.AccountSentExternallySince(ctx, f.userID, time.Now().Add(-time.Hour)); err != nil || !sent {
		t.Fatalf("claimed external send: sent=%v err=%v, want true", sent, err)
	}
}

// M4c: provider evidence alone defers, whatever the row's status and claim —
// e.g. a row locally inferred failed that the provider did accept.
func TestProviderAcceptedUnsettledSendCountsWithoutARecentClaim(t *testing.T) {
	for name, claim := range map[string]string{"null claim": "NULL", "old claim": "now() - interval '40 days'"} {
		t.Run(name, func(t *testing.T) {
			f := newDeferFixture(t, "evidence")
			ctx := context.Background()
			if _, err := f.pool.Exec(ctx, `
				INSERT INTO messages (id, agent_id, direction, sender, recipient, subject, delivery_status,
				                      provider_message_id, send_claimed_at, to_recipients)
				VALUES ('msg_evidence', $1, 'outbound', $1, 'x@example.com', 's', 'failed', 'ses-evidence-1', `+claim+`,
				        ARRAY['x@example.com'])`, f.agent); err != nil {
				t.Fatal(err)
			}
			res, err := f.store.PermanentDeleteAgentIncarnation(ctx, f.agent, f.userID, f.agentCreatedAt(t, f.agent))
			if err != nil || !res.EraseDeferred {
				t.Fatalf("result = %+v err=%v, want deferred on provider evidence alone", res, err)
			}
		})
	}
}

// M6b: owner-mailbox proof is for the CURRENT email only; after an email
// change the old verified mailbox is an external recipient.
func TestStaleOwnerMailboxProofCountsAsExternal(t *testing.T) {
	f := newDeferFixture(t, "stalemailbox")
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		UPDATE users SET owner_email_verified_address = 'old-owner@example.test', owner_email_verified_at = now(),
		                 owner_email_verified_source = 'google_oauth', email = 'new-owner@example.test'
		 WHERE id = $1`, f.userID); err != nil {
		t.Fatal(err)
	}
	f.sent(t, "msg_stale_owner", time.Hour, "old-owner@example.test")
	if sent, err := f.store.AccountSentExternallySince(ctx, f.userID, time.Now().Add(-24*time.Hour)); err != nil || !sent {
		t.Fatalf("send to the previously verified mailbox: sent=%v err=%v, want external", sent, err)
	}
	// Control: proof for the current email makes the same mailbox internal.
	if _, err := f.pool.Exec(ctx, `UPDATE users SET email = 'old-owner@example.test' WHERE id = $1`, f.userID); err != nil {
		t.Fatal(err)
	}
	if sent, err := f.store.AccountSentExternallySince(ctx, f.userID, time.Now().Add(-24*time.Hour)); err != nil || sent {
		t.Fatalf("send to the current verified mailbox: sent=%v err=%v, want internal", sent, err)
	}
}

// S8: with account trash disabled there is no window to hold the account in,
// so a recent external sender's erase is immediate.
func TestAccountTrashDisabledErasesARecentSenderImmediately(t *testing.T) {
	prev := identity.AccountTrashRetention
	identity.AccountTrashRetention = 0
	t.Cleanup(func() { identity.AccountTrashRetention = prev })

	f := newDeferFixture(t, "notrash")
	f.sent(t, "msg_notrash", time.Hour, "someone@example.com")
	res, err := f.store.EraseAccount(context.Background(), f.userID, nil)
	if err != nil {
		t.Fatalf("EraseAccount: %v", err)
	}
	if res.EraseDeferred || !res.UserDeleted {
		t.Fatalf("receipt = %+v, want an immediate erase with account trash disabled", res)
	}
}
