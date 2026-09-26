package identity_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/migrations"
)

// Account soft deletion (docs/design/account-soft-deletion.md §7): the store
// seams — trash, restore, purge, tombstones and the deleted-account summary —
// against real Postgres.

func testKeyring(t *testing.T) *identity.TombstoneKeyring {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	kr, err := identity.ParseTombstoneKeyring("v1:" + base64.StdEncoding.EncodeToString(b))
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return kr
}

func tombstoneStore(t *testing.T) (*identity.Store, *pgxpool.Pool, *identity.TombstoneKeyring) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	kr := testKeyring(t)
	store.SetTombstonePolicy(identity.TombstonePolicy{Enabled: true, Keyring: kr})
	return store, pool, kr
}

// ageTrash moves an account's trash stamp past the retention window so the
// janitor purge takes it.
func ageTrash(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET deleted_at = now() - make_interval(secs => $2) - interval '1 hour' WHERE id = $1`,
		userID, identity.AccountTrashRetention.Seconds()); err != nil {
		t.Fatalf("age trash: %v", err)
	}
}

type controlRow struct {
	State, Class, Reason, Evidence string
	UpdatedAt                      time.Time
}

func readControl(t *testing.T, pool *pgxpool.Pool, userID string) controlRow {
	t.Helper()
	var c controlRow
	if err := pool.QueryRow(context.Background(), `
		SELECT state, pause_class, reason, COALESCE(evidence_ref, ''), updated_at
		  FROM account_sending_controls WHERE user_id = $1`, userID,
	).Scan(&c.State, &c.Class, &c.Reason, &c.Evidence, &c.UpdatedAt); err != nil {
		t.Fatalf("read control: %v", err)
	}
	return c
}

func abusePause(t *testing.T, pool *pgxpool.Pool, userID, evidence string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO account_sending_controls (user_id, state, reason, actor, pause_class, evidence_ref)
		VALUES ($1, 'paused', 'synthetic abuse', 'test', 'abuse', NULLIF($2, ''))
		ON CONFLICT (user_id) DO UPDATE
		   SET state = 'paused', reason = 'synthetic abuse', pause_class = 'abuse', evidence_ref = NULLIF($2, '')`,
		userID, evidence); err != nil {
		t.Fatalf("abuse pause: %v", err)
	}
}

func TestTrashAccountSetsTrashStateAndLeavesControlsUntouched(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := seedUserData(t, store, ctx, "trasher")
	abusePause(t, pool, user.ID, "INC-TEST-1")
	before := readControl(t, pool, user.ID)

	// An agent the owner trashed themselves earlier must stay theirs.
	own, err := store.CreateAgent(ctx, "old@trasher.example.com", "trasher.example.com", "Old", "", "cloud", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDeleteAgent(ctx, own.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	var tokenBefore string
	if err := pool.QueryRow(ctx, `SELECT verification_token FROM domains WHERE domain = 'trasher.example.com'`).Scan(&tokenBefore); err != nil {
		t.Fatal(err)
	}

	var hooked []string
	res, err := store.TrashAccount(ctx, user.ID, func(ctx context.Context, tx pgx.Tx, domain string) error {
		hooked = append(hooked, domain)
		return nil
	})
	if err != nil {
		t.Fatalf("TrashAccount: %v", err)
	}
	if res.Mode != identity.AccountDeleteModeTrash || res.MessagesDeleted != 0 || res.UserDeleted {
		t.Fatalf("receipt = %+v, want mode trash, messages_deleted 0, user_deleted false", res)
	}
	if res.AgentsDeleted != 1 || res.APIKeysDeleted != 2 || res.SessionsDeleted != 1 || res.DomainsDeleted != 1 {
		t.Fatalf("receipt counts = %+v, want 1 agent trashed, 2 keys revoked, 1 session, 1 domain", res)
	}
	if len(hooked) != 1 || hooked[0] != "trasher.example.com" {
		t.Fatalf("SES teardown hook ran for %v, want the one owned domain", hooked)
	}

	var deletedAt *time.Time
	var purgeToken *string
	if err := pool.QueryRow(ctx, `SELECT deleted_at, purge_token FROM users WHERE id = $1`, user.ID).Scan(&deletedAt, &purgeToken); err != nil {
		t.Fatal(err)
	}
	if deletedAt == nil || purgeToken != nil {
		t.Fatalf("users: deleted_at=%v purge_token=%v, want stamped and NULL", deletedAt, purgeToken)
	}
	if res.PurgeAfter == nil || !res.PurgeAfter.Equal(deletedAt.Add(identity.AccountTrashRetention)) {
		t.Fatalf("purge_after = %v, want deleted_at + retention", res.PurgeAfter)
	}

	rows, err := pool.Query(ctx, `SELECT id, deleted_at IS NOT NULL, trashed_by_account FROM agent_identities WHERE user_id = $1`, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var trashed, byAccount bool
		if err := rows.Scan(&id, &trashed, &byAccount); err != nil {
			t.Fatal(err)
		}
		if !trashed {
			t.Errorf("agent %s not trashed", id)
		}
		if id == own.ID && byAccount {
			t.Errorf("owner-trashed agent %s was re-marked trashed_by_account", id)
		}
		if id != own.ID && !byAccount {
			t.Errorf("live agent %s trashed without trashed_by_account", id)
		}
	}
	rows.Close()

	var liveKeys, sessions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL`, user.ID).Scan(&liveKeys); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_sessions WHERE user_id = $1`, user.ID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if liveKeys != 0 || sessions != 0 {
		t.Fatalf("live keys=%d sessions=%d after trash, want 0/0", liveKeys, sessions)
	}

	var verified bool
	var verifiedAt *time.Time
	var tokenAfter, sendingStatus string
	if err := pool.QueryRow(ctx, `
		SELECT verified, verified_at, verification_token, sending_status FROM domains WHERE domain = 'trasher.example.com'`,
	).Scan(&verified, &verifiedAt, &tokenAfter, &sendingStatus); err != nil {
		t.Fatal(err)
	}
	if verified || sendingStatus != "none" || tokenAfter == tokenBefore {
		t.Fatalf("domain verified=%v sending_status=%s token rotated=%v, want unverified/none/rotated", verified, sendingStatus, tokenAfter != tokenBefore)
	}
	if verifiedAt == nil {
		t.Fatal("verified_at was cleared; it is the ever-verified fact the abuse tombstone reads")
	}

	if after := readControl(t, pool, user.ID); after != before {
		t.Fatalf("trash wrote to account_sending_controls:\n before=%+v\n after =%+v", before, after)
	}

	if _, err := store.TrashAccount(ctx, user.ID, nil); !errors.Is(err, identity.ErrAccountTrashed) {
		t.Fatalf("second trash err = %v, want ErrAccountTrashed", err)
	}
}

func TestTrashedAccountFailsEveryCredentialPath(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "creds@example.test", "Creds", "sub-creds")
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.CreateAPIKey(ctx, user.ID, "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachExternalPrincipal(ctx, "https://issuer.example.test", "tcusr_creds", user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	// Even a key the trash somehow missed must not authenticate.
	if _, err := pool.Exec(ctx, `UPDATE api_keys SET revoked_at = NULL WHERE user_id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPrincipalByAPIKey(ctx, key.PlaintextKey); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("API key of a trashed account: err=%v, want ErrNoRows", err)
	}
	if _, err := store.GetUserByID(ctx, user.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetUserByID of a trashed account: err=%v, want ErrNoRows", err)
	}
	if u, err := store.GetUserByExternalPrincipal(ctx, "https://issuer.example.test", "tcusr_creds"); err != nil || u != nil {
		t.Errorf("external principal of a trashed account resolved: %+v err=%v", u, err)
	}
	if _, err := store.CreateUserSession(ctx, user.ID); err == nil {
		t.Error("an ordinary session was issued to a trashed account")
	}
	if u, err := store.GetUserByIDAnyState(ctx, user.ID); err != nil || u.DeletedAt == nil {
		t.Errorf("GetUserByIDAnyState: %+v err=%v, want the trashed user", u, err)
	}

	// The restricted session resolves only through its own lookup.
	tok, err := store.CreateRestrictedUserSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetUserSession(ctx, tok); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("restricted session resolved as an ordinary session: err=%v", err)
	}
	if u, err := store.GetRestrictedSession(ctx, tok); err != nil || u.ID != user.ID {
		t.Errorf("GetRestrictedSession: %+v err=%v", u, err)
	}
}

func TestRestoreAccountClearsOnlyWhatTrashSet(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := seedUserData(t, store, ctx, "restorer")
	abusePause(t, pool, user.ID, "")
	before := readControl(t, pool, user.ID)
	own, err := store.CreateAgent(ctx, "old@restorer.example.com", "restorer.example.com", "Old", "", "cloud", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDeleteAgent(ctx, own.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	tok, err := store.CreateRestrictedUserSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	restored, err := store.RestoreAccount(ctx, user.ID, tok)
	if err != nil {
		t.Fatalf("RestoreAccount: %v", err)
	}
	if restored.DeletedAt != nil || restored.RestoredAt == nil {
		t.Fatalf("restored user deleted_at=%v restored_at=%v", restored.DeletedAt, restored.RestoredAt)
	}
	var live, ownTrashed, stillMarked int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE deleted_at IS NULL),
		       count(*) FILTER (WHERE id = $2 AND deleted_at IS NOT NULL),
		       count(*) FILTER (WHERE trashed_by_account)
		  FROM agent_identities WHERE user_id = $1`, user.ID, own.ID).Scan(&live, &ownTrashed, &stillMarked); err != nil {
		t.Fatal(err)
	}
	if live != 1 || ownTrashed != 1 || stillMarked != 0 {
		t.Fatalf("agents live=%d ownTrashed=%d marked=%d, want 1/1/0", live, ownTrashed, stillMarked)
	}
	var liveKeys int
	var verified bool
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL`, user.ID).Scan(&liveKeys); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT verified FROM domains WHERE domain = 'restorer.example.com'`).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if liveKeys != 0 || verified {
		t.Fatalf("restore revived keys=%d or verification=%v; both must stay as the trash left them", liveKeys, verified)
	}
	if after := readControl(t, pool, user.ID); after != before {
		t.Fatalf("restore changed account_sending_controls (a pause must survive):\n before=%+v\n after =%+v", before, after)
	}
	// The restricted session became an ordinary one.
	if u, err := store.GetUserSession(ctx, tok); err != nil || u.ID != user.ID {
		t.Fatalf("restricted session was not upgraded: %+v err=%v", u, err)
	}
	if _, err := store.RestoreAccount(ctx, user.ID, ""); !errors.Is(err, identity.ErrNotInTrash) {
		t.Fatalf("second restore err = %v, want ErrNotInTrash", err)
	}
}

func TestRestoreRefusesAClaimedPurge(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "claimed@example.test", "C", "sub-claimed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET purge_token = 'pur_test' WHERE id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RestoreAccount(ctx, user.ID, ""); !errors.Is(err, identity.ErrPurgeInProgress) {
		t.Fatalf("restore of a claimed purge err = %v, want ErrPurgeInProgress", err)
	}
}

func TestCreateOrGetUserFreezesATrashedRow(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "frozen@example.test", "Before", "sub-frozen")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.CreateOrGetUser(ctx, "changed@example.test", "After", "sub-frozen")
	if err != nil {
		t.Fatalf("CreateOrGetUser on a trashed subject: %v", err)
	}
	if got.ID != user.ID || got.DeletedAt == nil {
		t.Fatalf("resolved %+v, want the trashed account", got)
	}
	var email, name string
	if err := pool.QueryRow(ctx, `SELECT email, name FROM users WHERE id = $1`, user.ID).Scan(&email, &name); err != nil {
		t.Fatal(err)
	}
	if email != "frozen@example.test" || name != "Before" {
		t.Fatalf("ON CONFLICT DO UPDATE fired on a trashed row: email=%s name=%s", email, name)
	}
}

func TestProvisioningReplayOnATrashedAccountWritesNothing(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	const issuer = "https://issuer.example.test"
	user, created, err := store.ProvisionUser(ctx, "tcusr_replay", "replay@example.test", "R", "")
	if err != nil || !created {
		t.Fatalf("provision: created=%v err=%v", created, err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ProvisionUser(ctx, "tcusr_replay", "replay@example.test", "R", issuer); !errors.Is(err, identity.ErrAccountTrashed) {
		t.Fatalf("replay err = %v, want ErrAccountTrashed", err)
	}
	var mappings int
	var deletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM external_principal_mappings WHERE user_id = $1`, user.ID).Scan(&mappings); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM users WHERE id = $1`, user.ID).Scan(&deletedAt); err != nil {
		t.Fatal(err)
	}
	if mappings != 0 || deletedAt == nil {
		t.Fatalf("replay wrote: mappings=%d deleted_at=%v (want 0 and still trashed)", mappings, deletedAt)
	}
	if _, err := store.AttachExternalPrincipal(ctx, issuer, "tcusr_replay", user.ID); !errors.Is(err, identity.ErrAccountTrashed) {
		t.Fatalf("attach to a trashed account err = %v, want ErrAccountTrashed", err)
	}
}

func TestPurgeWritesAbuseTombstonesAndSummaryBeforeDeleting(t *testing.T) {
	store, pool, kr := tombstoneStore(t)
	ctx := context.Background()
	user := seedUserData(t, store, ctx, "abuser")
	if _, err := store.AttachExternalPrincipal(ctx, "https://issuer.example.test", "tcusr_abuser", user.ID); err != nil {
		t.Fatal(err)
	}
	abusePause(t, pool, user.ID, "INC-SYNTHETIC-7")
	// The seeded outbound message counts as a real send for the summary.
	if _, err := pool.Exec(ctx, `
		UPDATE messages m SET delivery_status = 'sent', to_recipients = ARRAY['Victim <victim@target.example>', 'x@other.example']
		  FROM agent_identities a WHERE a.id = m.agent_id AND a.user_id = $1 AND m.direction = 'outbound' AND m.status = 'sent'`,
		user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	ageTrash(t, pool, user.ID)

	purged, err := store.PurgeDeletedUsers(ctx, nil)
	if err != nil {
		t.Fatalf("PurgeDeletedUsers: %v", err)
	}
	if len(purged) != 1 || purged[0] != user.ID {
		t.Fatalf("purged %v, want [%s]", purged, user.ID)
	}
	if _, err := store.GetUserByIDAnyState(ctx, user.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("user row survived purge: %v", err)
	}
	var agents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_identities WHERE user_id = $1`, user.ID).Scan(&agents); err != nil {
		t.Fatal(err)
	}
	if agents != 0 {
		t.Fatalf("%d agents survived the account purge", agents)
	}

	digest := func(kind, value string) []byte {
		d, _ := kr.Digest(kr.ActiveVersion(), kind, identity.NormalizeTombstoneValue(kind, value))
		return d
	}
	for _, want := range []struct{ kind, value string }{
		{identity.TombstoneKindLoginSubject, "google-abuser"},
		{identity.TombstoneKindLoginSubject, "ext:https://issuer.example.test\ntcusr_abuser"},
		{identity.TombstoneKindEmail, "abuser@example.com"},
		{identity.TombstoneKindDomain, "abuser.example.com"},
	} {
		var class string
		var expires time.Time
		if err := pool.QueryRow(ctx,
			`SELECT class, expires_at FROM identity_tombstones WHERE kind = $1 AND digest = $2`,
			want.kind, digest(want.kind, want.value)).Scan(&class, &expires); err != nil {
			t.Fatalf("tombstone %s %q missing: %v", want.kind, want.value, err)
		}
		if class != identity.TombstoneClassAbuse || time.Until(expires) < 700*24*time.Hour {
			t.Errorf("tombstone %s: class=%s expires in %v, want abuse held ~2 years", want.kind, class, time.Until(expires))
		}
	}

	sum, err := store.GetDeletedAccountSummary(ctx, user.ID)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.RetentionClass != "abuse" || time.Until(sum.ExpiresAt) < 700*24*time.Hour {
		t.Errorf("summary retention %s until %v, want abuse ~2y", sum.RetentionClass, sum.ExpiresAt)
	}
	if sum.EvidenceRef == nil || *sum.EvidenceRef != "INC-SYNTHETIC-7" || sum.PauseClass == nil || *sum.PauseClass != "abuse" {
		t.Errorf("summary pause/evidence = %v/%v", sum.PauseClass, sum.EvidenceRef)
	}
	if sum.AgentsCount != 1 || sum.MessagesCount != 3 || sum.OutboundSendsCount != 1 {
		t.Errorf("summary counts agents=%d messages=%d sends=%d, want 1/3/1", sum.AgentsCount, sum.MessagesCount, sum.OutboundSendsCount)
	}
	domains := map[string]int64{}
	for _, d := range sum.RecipientDomains {
		domains[d.Domain] = d.Count
	}
	if domains["target.example"] != 1 || domains["other.example"] != 1 || len(domains) != 2 {
		t.Errorf("recipient domain histogram = %v", sum.RecipientDomains)
	}
	if len(sum.DailySends) != 1 || sum.DailySends[0].Count != 1 {
		t.Errorf("daily sends = %v", sum.DailySends)
	}
	if len(sum.SubjectDigests) != 1 || len(sum.VerifiedDomainDigests) != 1 {
		t.Errorf("subject digests=%v domain digests=%v", sum.SubjectDigests, sum.VerifiedDomainDigests)
	}
	if sum.VerifiedDomainDigests[0] != hex.EncodeToString(digest(identity.TombstoneKindDomain, "abuser.example.com")) {
		t.Error("verified-domain digest does not cross-reference the domain tombstone")
	}
	raw, _ := json.Marshal(sum)
	for _, leak := range []string{"victim", "Victim", "abuser@example.com", "Re: Hi", "x@other"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("summary leaks %q: %s", leak, raw)
		}
	}
}

func TestTombstonesRefuseEveryRegistrationEntryPoint(t *testing.T) {
	store, pool, _ := tombstoneStore(t)
	ctx := context.Background()
	const issuer = "https://issuer.example.test"
	user, err := store.CreateOrGetUser(ctx, "First.Last+tag@gmail.com", "X", "sub-tomb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachExternalPrincipal(ctx, issuer, "tcusr_tomb", user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, "held.example.test", user.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyDomain(ctx, "held.example.test", user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAgentWithLimit(ctx, "held-slug@agents.e2a.dev", "agents.e2a.dev", "Slug", user.ID, 0); err != nil {
		t.Fatal(err)
	}
	abusePause(t, pool, user.ID, "")
	// An abuse-paused account cannot be erased on demand ...
	if _, err := store.EraseAccount(ctx, user.ID, nil); !errors.Is(err, identity.ErrEraseHeld) {
		t.Fatalf("EraseAccount of a paused account err = %v, want ErrEraseHeld", err)
	}
	// ... it is trashed, and the janitor purge writes the abuse hold.
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	ageTrash(t, pool, user.ID)
	if purged, err := store.PurgeDeletedUsers(ctx, nil); err != nil || len(purged) != 1 {
		t.Fatalf("purge: %v %v", purged, err)
	}

	// Same subject.
	if _, err := store.CreateOrGetUser(ctx, "fresh@example.test", "Y", "sub-tomb"); !errors.Is(err, identity.ErrRegistrationRefused) {
		t.Errorf("same subject err = %v, want ErrRegistrationRefused", err)
	}
	// A trivially equivalent spelling of the email under a new subject.
	for _, email := range []string{"firstlast@gmail.com", "first.last@googlemail.com", "FIRSTLAST+other@GMAIL.com"} {
		if _, err := store.CreateOrGetUser(ctx, email, "Y", "sub-new-"+email); !errors.Is(err, identity.ErrRegistrationRefused) {
			t.Errorf("equivalent email %q err = %v, want ErrRegistrationRefused", email, err)
		}
	}
	// Provisioning with the held email.
	if _, _, err := store.ProvisionUser(ctx, "tcusr_other", "firstlast@gmail.com", "Z", ""); !errors.Is(err, identity.ErrRegistrationRefused) {
		t.Errorf("provision err = %v, want ErrRegistrationRefused", err)
	}
	// Attaching the held external principal to another account.
	other, err := store.CreateOrGetUser(ctx, "other@example.test", "O", "sub-other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachExternalPrincipal(ctx, issuer, "tcusr_tomb", other.ID); !errors.Is(err, identity.ErrRegistrationRefused) {
		t.Errorf("attach err = %v, want ErrRegistrationRefused", err)
	}
	// The abuse-verified domain is domain_taken for everyone else.
	if _, err := store.ClaimOrCreateDomain(ctx, "HELD.example.test.", other.ID); !errors.Is(err, identity.ErrDomainTaken) {
		t.Errorf("domain err = %v, want ErrDomainTaken", err)
	}
	// The shared-domain slug the abuse account held is closed too.
	if _, err := store.CreateAgentWithLimit(ctx, "Held-Slug@agents.e2a.dev", "agents.e2a.dev", "Slug", other.ID, 0); !errors.Is(err, identity.ErrAgentAddressHeld) {
		t.Errorf("held slug err = %v, want ErrAgentAddressHeld", err)
	}
	// The owner's email domain is webmail (gmail), so it is NOT held.
	if _, err := store.CreateOrGetUser(ctx, "unrelated@gmail.com", "U", "sub-unrelated-gmail"); err != nil {
		t.Errorf("a webmail domain was held: %v", err)
	}
	// An unrelated identity still registers.
	if _, err := store.CreateOrGetUser(ctx, "unrelated@example.test", "U", "sub-unrelated"); err != nil {
		t.Errorf("unrelated signup refused: %v", err)
	}
}

// TestAbuseHoldClosesACorporateEmailDomain: a non-webmail owner domain is
// held for the abuse window; every signup/provision at that domain refuses.
func TestAbuseHoldClosesACorporateEmailDomain(t *testing.T) {
	store, pool, _ := tombstoneStore(t)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "boss@scam-corp.test", "B", "sub-corp")
	if err != nil {
		t.Fatal(err)
	}
	abusePause(t, pool, user.ID, "")
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	ageTrash(t, pool, user.ID)
	if purged, err := store.PurgeDeletedUsers(ctx, nil); err != nil || len(purged) != 1 {
		t.Fatalf("purge: %v %v", purged, err)
	}
	if _, err := store.CreateOrGetUser(ctx, "newhire@SCAM-CORP.test", "N", "sub-newhire"); !errors.Is(err, identity.ErrRegistrationRefused) {
		t.Fatalf("signup at a held corporate domain err = %v", err)
	}
	if _, _, err := store.ProvisionUser(ctx, "tcusr_corp", "ops@scam-corp.test", "O", ""); !errors.Is(err, identity.ErrRegistrationRefused) {
		t.Fatalf("provision at a held corporate domain err = %v", err)
	}
}

// TestAbuseIsDerivedFromPauseHistory: an account paused as abuse, resumed and
// re-paused under another class still gets the abuse hold at purge.
func TestAbuseIsDerivedFromPauseHistory(t *testing.T) {
	store, pool, _ := tombstoneStore(t)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "history@example.test", "H", "sub-history")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO account_sending_controls (user_id, state, reason, actor, pause_class) VALUES ($1, 'paused', 'r', 'op', 'operator');
		INSERT INTO account_sending_control_events (id, account_ref, old_state, new_state, reason, actor, pause_class, expires_at)
		VALUES ('asce_hist', $1, 'active', 'paused', 'r', 'op', 'abuse', now() + interval '90 days')`, user.ID); err != nil {
		// multi-statement with params is not allowed; fall back to two calls
		t.Logf("combined insert: %v", err)
		if _, err := pool.Exec(ctx, `INSERT INTO account_sending_controls (user_id, state, reason, actor, pause_class) VALUES ($1, 'paused', 'r', 'op', 'operator')`, user.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO account_sending_control_events (id, account_ref, old_state, new_state, reason, actor, pause_class, expires_at)
			VALUES ('asce_hist', $1, 'active', 'paused', 'r', 'op', 'abuse', now() + interval '90 days')`, user.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	ageTrash(t, pool, user.ID)
	if purged, err := store.PurgeDeletedUsers(ctx, nil); err != nil || len(purged) != 1 {
		t.Fatalf("purge: %v %v", purged, err)
	}
	sum, err := store.GetDeletedAccountSummary(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.RetentionClass != "abuse" {
		t.Fatalf("retention = %s, want abuse (derived from the pause history)", sum.RetentionClass)
	}
}

// TestEscalateDeletedAccountToAbuseAfterPurge: an account erased before any
// classification can be escalated from its summary's digests alone.
func TestEscalateDeletedAccountToAbuseAfterPurge(t *testing.T) {
	store, pool, _ := tombstoneStore(t)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "late@late-corp.test", "L", "sub-late")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAgentWithLimit(ctx, "late-slug@agents.e2a.dev", "agents.e2a.dev", "S", user.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EraseAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	sum, err := store.GetDeletedAccountSummary(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.RetentionClass != "recent_deletion" || len(sum.IdentityDigests) < 4 {
		t.Fatalf("summary before escalation: %s digests=%v", sum.RetentionClass, sum.IdentityDigests)
	}
	n, err := store.EscalateDeletedAccountToAbuse(ctx, user.ID, "test-operator", "synthetic escalation")
	if err != nil || n != len(sum.IdentityDigests) {
		t.Fatalf("escalate: n=%d err=%v", n, err)
	}
	var abuse int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_tombstones WHERE account_ref = $1 AND class = 'abuse' AND expires_at > now() + interval '700 days'`, user.ID).Scan(&abuse); err != nil {
		t.Fatal(err)
	}
	if abuse != n {
		t.Fatalf("abuse tombstones = %d, want %d", abuse, n)
	}
	other, err := store.CreateOrGetUser(ctx, "someone@example.test", "S", "sub-someone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAgentWithLimit(ctx, "late-slug@agents.e2a.dev", "agents.e2a.dev", "S", other.ID, 0); !errors.Is(err, identity.ErrAgentAddressHeld) {
		t.Fatalf("escalated slug err = %v", err)
	}
	if _, err := store.CreateOrGetUser(ctx, "x@late-corp.test", "X", "sub-x"); !errors.Is(err, identity.ErrRegistrationRefused) {
		t.Fatalf("escalated corporate domain err = %v", err)
	}
	after, err := store.GetDeletedAccountSummary(ctx, user.ID)
	if err != nil || after.RetentionClass != "abuse" || time.Until(after.ExpiresAt) < 700*24*time.Hour {
		t.Fatalf("summary after escalation: %+v err=%v", after, err)
	}
}

// TestRevokeKeepsAHoldSharedWithAnotherAccount: revoking one account's
// tombstones must not reopen an identifier another purged account also holds.
func TestRevokeKeepsAHoldSharedWithAnotherAccount(t *testing.T) {
	store, pool, _ := tombstoneStore(t)
	ctx := context.Background()
	a, err := store.CreateOrGetUser(ctx, "a@shared-corp.test", "A", "sub-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.CreateOrGetUser(ctx, "b@shared-corp.test", "B", "sub-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []*identity.User{a, b} {
		abusePause(t, pool, u.ID, "")
		if _, err := store.TrashAccount(ctx, u.ID, nil); err != nil {
			t.Fatal(err)
		}
		ageTrash(t, pool, u.ID)
	}
	if purged, err := store.PurgeDeletedUsers(ctx, nil); err != nil || len(purged) != 2 {
		t.Fatalf("purge: %v %v", purged, err)
	}
	revoked, kept, err := store.RevokeAccountTombstones(ctx, a.ID, "test-operator", "synthetic revoke")
	if err != nil || revoked == 0 || kept != 1 {
		t.Fatalf("revoke: revoked=%d kept=%d err=%v (want the shared email-domain hold kept)", revoked, kept, err)
	}
	if _, err := store.CreateOrGetUser(ctx, "c@shared-corp.test", "C", "sub-c"); !errors.Is(err, identity.ErrRegistrationRefused) {
		t.Fatalf("the other account's domain hold was lost: %v", err)
	}
	if _, err := store.CreateOrGetUser(ctx, "fresh@example.test", "F", "sub-a"); err != nil {
		t.Fatalf("a's own subject still held after revoke: %v", err)
	}
}

func TestEraseIsHeldWhileAnyPauseApplies(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "held@example.test", "H", "sub-held")
	if err != nil {
		t.Fatal(err)
	}
	ag, err := store.CreateAgent(ctx, "held-bot@agents.e2a.dev", "agents.e2a.dev", "H", "", "cloud", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_sending_controls (user_id, state, reason, actor, pause_class) VALUES ($1, 'paused', 'r', 'op', 'billing')`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EraseAccount(ctx, user.ID, nil); !errors.Is(err, identity.ErrEraseHeld) {
		t.Fatalf("erase err = %v, want ErrEraseHeld", err)
	}
	if _, err := store.DeleteAgent(ctx, ag.ID, user.ID); !errors.Is(err, identity.ErrEraseHeld) {
		t.Fatalf("permanent agent delete err = %v, want ErrEraseHeld", err)
	}
	if err := store.SoftDeleteAgent(ctx, ag.ID, user.ID); err != nil {
		t.Fatalf("trashing an agent must stay available: %v", err)
	}
}

func TestRestoreIsRateLimited(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "churn@example.test", "C", "sub-churn")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
			t.Fatal(err)
		}
		_, err := store.RestoreAccount(ctx, user.ID, "")
		if i == 0 && err != nil {
			t.Fatalf("first restore: %v", err)
		}
		if i == 1 && !errors.Is(err, identity.ErrRestoreRateLimited) {
			t.Fatalf("second restore within the cooldown err = %v", err)
		}
	}
}

func TestTrashBumpsAgentAssertionVersions(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "jwt@example.test", "J", "sub-jwt")
	if err != nil {
		t.Fatal(err)
	}
	ag, err := store.CreateAgent(ctx, "jwt-bot@agents.e2a.dev", "agents.e2a.dev", "J", "", "cloud", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	var before, after int
	if err := pool.QueryRow(ctx, `SELECT assertion_version FROM agent_identities WHERE id = $1`, ag.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT assertion_version FROM agent_identities WHERE id = $1`, ag.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatalf("assertion_version %d -> %d, want a bump", before, after)
	}
}

func TestGoogleEmailCollisionWithATrashedRowIsClassified(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "collide@example.test", "C", "sub-collide")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOrGetUser(ctx, "collide@example.test", "C", "sub-other"); !errors.Is(err, identity.ErrEmailConflict) {
		t.Fatalf("live collision err = %v, want ErrEmailConflict", err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOrGetUser(ctx, "collide@example.test", "C", "sub-other"); !errors.Is(err, identity.ErrAccountTrashed) {
		t.Fatalf("trashed collision err = %v, want ErrAccountTrashed", err)
	}
}

func TestSummaryCountsSurviveMessageErasureThroughUsageEvents(t *testing.T) {
	store, pool, _ := tombstoneStore(t)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "wiper@example.test", "W", "sub-wiper")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO usage_events (id, user_id, agent_id, domain, direction) VALUES ($1, $2, 'gone@agents.e2a.dev', 'agents.e2a.dev', 'outbound')`,
			"ue_wipe_"+string(rune('a'+i)), user.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.EraseAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	sum, err := store.GetDeletedAccountSummary(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.OutboundSendsCount != 3 || sum.FirstOutboundAt == nil || len(sum.DailySends) != 1 || sum.DailySends[0].Count != 3 {
		t.Fatalf("summary from usage events: sends=%d first=%v daily=%v", sum.OutboundSendsCount, sum.FirstOutboundAt, sum.DailySends)
	}
}

func TestRecentDeletionTombstoneForAPlainErase(t *testing.T) {
	store, pool, _ := tombstoneStore(t)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "plain@example.test", "P", "sub-plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EraseAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	var classes []string
	var minExpiry time.Time
	if err := pool.QueryRow(ctx, `
		SELECT array_agg(DISTINCT class), min(expires_at) FROM identity_tombstones WHERE account_ref = $1`, user.ID,
	).Scan(&classes, &minExpiry); err != nil {
		t.Fatal(err)
	}
	if len(classes) != 1 || classes[0] != identity.TombstoneClassRecentDeletion {
		t.Fatalf("classes = %v, want only recent_deletion", classes)
	}
	if time.Until(minExpiry) < 29*24*time.Hour {
		t.Fatalf("recent_deletion hold ends in %v, want at least 30 days", time.Until(minExpiry))
	}
	var domainTombstones int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_tombstones WHERE account_ref = $1 AND kind = 'domain'`, user.ID).Scan(&domainTombstones); err != nil {
		t.Fatal(err)
	}
	if domainTombstones != 0 {
		t.Fatal("a non-abuse purge held domains")
	}
	sum, err := store.GetDeletedAccountSummary(ctx, user.ID)
	if err != nil {
		t.Fatalf("summary for a non-abuse purge: %v", err)
	}
	if sum.RetentionClass != "recent_deletion" {
		t.Fatalf("summary retention = %s", sum.RetentionClass)
	}
	if _, err := store.CreateOrGetUser(ctx, "plain@example.test", "P", "sub-plain"); !errors.Is(err, identity.ErrRegistrationRefused) {
		t.Fatalf("erase-and-return err = %v, want ErrRegistrationRefused", err)
	}
}

func TestTombstoneKeyUnavailableFailsClosed(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "nokey@example.test", "N", "sub-nokey")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	ageTrash(t, pool, user.ID)
	store.SetTombstonePolicy(identity.TombstonePolicy{Enabled: true})

	if _, err := store.CreateOrGetUser(ctx, "new@example.test", "N", "sub-new"); !errors.Is(err, identity.ErrTombstoneKeyUnavailable) {
		t.Errorf("signup without a key err = %v, want ErrTombstoneKeyUnavailable", err)
	}
	purged, err := store.PurgeDeletedUsers(ctx, nil)
	if len(purged) != 0 || !errors.Is(err, identity.ErrTombstoneKeyUnavailable) {
		t.Fatalf("purge without a key: purged=%v err=%v, want skip with ErrTombstoneKeyUnavailable", purged, err)
	}
	var deletedAt *time.Time
	var token *string
	if err := pool.QueryRow(ctx, `SELECT deleted_at, purge_token FROM users WHERE id = $1`, user.ID).Scan(&deletedAt, &token); err != nil {
		t.Fatalf("account vanished without tombstones: %v", err)
	}
	if deletedAt == nil || token != nil {
		t.Fatalf("skipped account: deleted_at=%v purge_token=%v, want still trashed and unclaimed", deletedAt, token)
	}
	if _, err := store.EraseAccount(ctx, user.ID, nil); !errors.Is(err, identity.ErrTombstoneKeyUnavailable) {
		t.Fatalf("erase without a key err = %v", err)
	}
}

func TestPurgeWithTombstonesDisabledWritesNothing(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "selfhost@example.test", "S", "sub-selfhost")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EraseAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM identity_tombstones) + (SELECT count(*) FROM deleted_account_summaries)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("tombstones disabled but %d tombstone/summary rows written", n)
	}
	if _, err := store.CreateOrGetUser(ctx, "selfhost@example.test", "S", "sub-selfhost"); err != nil {
		t.Fatalf("re-signup refused with tombstones disabled: %v", err)
	}
}

func TestPurgeDeletedAgentsSkipsAccountTrashedAgents(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := seedUserData(t, store, ctx, "agentskip")
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_identities SET deleted_at = now() - interval '400 days' WHERE user_id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PurgeDeletedAgents(ctx); err != nil || n != 0 {
		t.Fatalf("PurgeDeletedAgents took %d account-trashed agent(s) (err=%v); the account purge owns them", n, err)
	}
}

func TestPurgeSkipsAccountsInsideTheWindowAndARestoreWins(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "window@example.test", "W", "sub-window")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	if purged, err := store.PurgeDeletedUsers(ctx, nil); err != nil || len(purged) != 0 {
		t.Fatalf("purged inside the window: %v err=%v", purged, err)
	}
	if _, err := store.RestoreAccount(ctx, user.ID, ""); err != nil {
		t.Fatal(err)
	}
	if purged, err := store.PurgeDeletedUsers(ctx, nil); err != nil || len(purged) != 0 {
		t.Fatalf("a restored account was purged: %v err=%v", purged, err)
	}
}

// TestTombstoneCheckSerializesWithAConcurrentPurge proves the advisory-lock
// contract: a signup whose digest check starts while a purge's tombstone
// write is uncommitted waits for it and then refuses — it can never read
// "no tombstone" and slip in between.
func TestTombstoneCheckSerializesWithAConcurrentPurge(t *testing.T) {
	store, pool, kr := tombstoneStore(t)
	ctx := context.Background()
	norm := identity.NormalizeTombstoneValue(identity.TombstoneKindEmail, "race@example.test")
	d, _ := kr.Digest(kr.ActiveVersion(), identity.TombstoneKindEmail, norm)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "email:"+hex.EncodeToString(d)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity_tombstones (kind, digest, key_version, class, account_ref, expires_at)
		VALUES ('email', $1, $2, 'recent_deletion', 'usr_gone', now() + interval '30 days')`, d, kr.ActiveVersion()); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := store.CreateOrGetUser(ctx, "race@example.test", "R", "sub-race")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("signup finished while the purge held the digest lock (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, identity.ErrRegistrationRefused) {
			t.Fatalf("signup after the purge committed err = %v, want ErrRegistrationRefused", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("signup never resumed")
	}
}

func TestAccountSoftDeletionMigrationIsIdempotentAndConservative(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user := seedUserData(t, store, ctx, "migrator")
	if _, err := pool.Exec(ctx, `INSERT INTO account_sending_controls (user_id, state, reason, actor) VALUES ($1, 'paused', 'legacy pause', 'sql')`, user.ID); err != nil {
		t.Fatal(err)
	}
	sql, err := migrations.FS.ReadFile("122_account_soft_deletion.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
	var trashed, byAccount, restricted bool
	var class string
	if err := pool.QueryRow(ctx, `
		SELECT u.deleted_at IS NOT NULL OR u.purge_token IS NOT NULL OR u.restored_at IS NOT NULL,
		       bool_or(a.trashed_by_account), bool_or(s.restricted), c.pause_class
		  FROM users u
		  JOIN agent_identities a ON a.user_id = u.id
		  JOIN user_sessions s ON s.user_id = u.id
		  JOIN account_sending_controls c ON c.user_id = u.id
		 WHERE u.id = $1
		 GROUP BY u.deleted_at, u.purge_token, u.restored_at, c.pause_class`, user.ID,
	).Scan(&trashed, &byAccount, &restricted, &class); err != nil {
		t.Fatal(err)
	}
	if trashed || byAccount || restricted || class != "operator" {
		t.Fatalf("existing rows changed: trashed=%v byAccount=%v restricted=%v class=%s", trashed, byAccount, restricted, class)
	}
	for name, bad := range map[string]string{
		"purge token without trash":  `UPDATE users SET purge_token = 'pur_x' WHERE id = '` + user.ID + `'`,
		"unknown pause class":        `UPDATE account_sending_controls SET pause_class = 'fraud' WHERE user_id = '` + user.ID + `'`,
		"unknown tombstone class":    `INSERT INTO identity_tombstones (kind, digest, key_version, class, account_ref, expires_at) VALUES ('email', decode(repeat('ab', 32), 'hex'), 1, 'forever', 'u', now() + interval '1 day')`,
		"short digest":               `INSERT INTO identity_tombstones (kind, digest, key_version, class, account_ref, expires_at) VALUES ('email', '\x01', 1, 'abuse', 'u', now() + interval '1 day')`,
		"trashed_by_account on live": `UPDATE agent_identities SET trashed_by_account = true WHERE user_id = '` + user.ID + `'`,
	} {
		if _, err := pool.Exec(ctx, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestTrashLeavesADomainOtherAccountsDependOn: a domain another account's
// agents live on (the shared domain an operator's probe account adopted) is
// infrastructure; trashing its owner must not unverify it.
func TestTrashLeavesADomainOtherAccountsDependOn(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	const shared = "shared.agents.e2a.dev"
	if err := store.EnsureSharedDomain(ctx, shared); err != nil {
		t.Fatal(err)
	}
	probe, err := store.CreateOrGetUser(ctx, "probe@example.test", "Probe", "sub-probe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdoptSharedDomain(ctx, shared, probe.ID); err != nil {
		t.Fatal(err)
	}
	customer, err := store.CreateOrGetUser(ctx, "customer@example.test", "C", "sub-customer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAgentWithLimit(ctx, "cust@"+shared, shared, "Cust", customer.ID, 0); err != nil {
		t.Fatal(err)
	}
	var hooked []string
	if _, err := store.TrashAccount(ctx, probe.ID, func(_ context.Context, _ pgx.Tx, d string) error {
		hooked = append(hooked, d)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var verified bool
	if err := pool.QueryRow(ctx, `SELECT verified FROM domains WHERE domain = $1`, shared).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if !verified || len(hooked) != 0 {
		t.Fatalf("shared domain verified=%v teardown=%v; another account's inboxes depend on it", verified, hooked)
	}

	// Purging the probe account must not wedge on the other account's
	// agents: the domain goes back to the platform (unowned, verified).
	store.SetTombstonePolicy(identity.TombstonePolicy{Enabled: true, Keyring: testKeyring(t)})
	abusePause(t, pool, probe.ID, "")
	ageTrash(t, pool, probe.ID)
	purged, err := store.PurgeDeletedUsers(ctx, func(_ context.Context, _ pgx.Tx, d string) error {
		hooked = append(hooked, d)
		return nil
	})
	if err != nil || len(purged) != 1 {
		t.Fatalf("probe purge wedged: purged=%v err=%v", purged, err)
	}
	var owner *string
	if err := pool.QueryRow(ctx, `SELECT user_id, verified FROM domains WHERE domain = $1`, shared).Scan(&owner, &verified); err != nil {
		t.Fatal(err)
	}
	if owner != nil || !verified || len(hooked) != 0 {
		t.Fatalf("shared domain after purge: owner=%v verified=%v teardown=%v", owner, verified, hooked)
	}
	var custAgents, domainHolds int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_identities WHERE user_id = $1`, customer.ID).Scan(&custAgents); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_tombstones WHERE account_ref = $1 AND kind = 'domain'`, probe.ID).Scan(&domainHolds); err != nil {
		t.Fatal(err)
	}
	if custAgents != 1 || domainHolds != 0 {
		t.Fatalf("customer agents=%d, shared-domain abuse holds=%d; want 1 and 0", custAgents, domainHolds)
	}
}

// TestRestrictedSessionOfALiveAccountNeverResolves pins the NOT s.restricted
// predicate on its own (the trashed-user predicate would otherwise mask it).
func TestRestrictedSessionOfALiveAccountNeverResolves(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "live-restricted@example.test", "L", "sub-live-restricted")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions (token, user_id, expires_at, restricted) VALUES ('sess_live_restricted', $1, now() + interval '1 hour', true)`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetUserSession(ctx, "sess_live_restricted"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a restricted session resolved as an ordinary session: err=%v", err)
	}
}

// TestPurgeTombstoneWriteTakesTheDigestLock is the purge-side half of the
// advisory-lock contract: while a signup holds a digest lock mid-check, the
// purge's tombstone write for that identifier must wait for it.
func TestPurgeTombstoneWriteTakesTheDigestLock(t *testing.T) {
	store, pool, kr := tombstoneStore(t)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "lock@example.test", "L", "sub-lock")
	if err != nil {
		t.Fatal(err)
	}
	d, _ := kr.Digest(kr.ActiveVersion(), identity.TombstoneKindEmail, identity.NormalizeTombstoneValue(identity.TombstoneKindEmail, "lock@example.test"))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "email:"+hex.EncodeToString(d)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.EraseAccount(ctx, user.ID, nil)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the purge wrote its tombstone while a signup held the digest lock (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("erase after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the purge never resumed")
	}
}

func TestRestoreExtendsTheUpgradedSession(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "ttl@example.test", "T", "sub-ttl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TrashAccount(ctx, user.ID, nil); err != nil {
		t.Fatal(err)
	}
	tok, err := store.CreateRestrictedUserSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RestoreAccount(ctx, user.ID, tok); err != nil {
		t.Fatal(err)
	}
	var expires time.Time
	if err := pool.QueryRow(ctx, `SELECT expires_at FROM user_sessions WHERE token = $1`, tok).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if time.Until(expires) < identity.SessionTTL-time.Minute {
		t.Fatalf("upgraded session expires in %v, want the ordinary %v", time.Until(expires), identity.SessionTTL)
	}
}
