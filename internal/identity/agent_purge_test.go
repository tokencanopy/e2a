package identity_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// purgeFixture is one owner + one agent on its own domain, ready to be filled.
type purgeFixture struct {
	pool    *pgxpool.Pool
	store   *identity.Store
	userID  string
	agentID string
}

func newPurgeFixture(t *testing.T, slug string) purgeFixture {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, err := store.CreateOrGetUser(ctx, slug+"@example.com", "Owner", "google-"+slug)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	domain := slug + ".example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	agent, err := store.CreateAgent(ctx, "bot@"+domain, domain, "", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return purgeFixture{pool: pool, store: store, userID: user.ID, agentID: agent.ID}
}

// seedMessages bulk-inserts n inbound messages for the agent. Raw SQL, because
// the store's CreateInboundMessage is far too slow at the tens of thousands of
// rows these tests need to cross the chunk boundary several times.
func (f purgeFixture) seedMessages(t *testing.T, n int, prefix string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO messages (id, agent_id, direction, sender, recipient, subject)
		 SELECT $2 || i, $1, 'inbound', 'sender@example.test', $1, 'seed'
		   FROM generate_series(1, $3) AS i`,
		f.agentID, prefix, n); err != nil {
		t.Fatalf("seed %d messages: %v", n, err)
	}
}

// seedCascadeChildren attaches one row in each of three tables that hang off
// messages via ON DELETE CASCADE, to every message the agent currently has.
// The chunked delete must take these with it exactly as the single-statement
// delete's cascade did.
func (f purgeFixture) seedCascadeChildren(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO webhook_deliveries (message_id) SELECT id FROM messages WHERE agent_id = $1`,
		f.agentID); err != nil {
		t.Fatalf("seed webhook_deliveries: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO message_recipients (id, message_id, address)
		 SELECT 'mr_' || id, id, 'recipient@example.test' FROM messages WHERE agent_id = $1`,
		f.agentID); err != nil {
		t.Fatalf("seed message_recipients: %v", err)
	}
	// message_lifecycle_transitions carries a composite FK into the reason-code
	// catalog, so borrow a real catalog row rather than inventing one.
	var code, stage, outcome string
	var retryable bool
	if err := f.pool.QueryRow(ctx,
		`SELECT code, stage, outcome, retryable FROM message_lifecycle_reason_codes LIMIT 1`,
	).Scan(&code, &stage, &outcome, &retryable); err != nil {
		t.Fatalf("read lifecycle reason code: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO message_lifecycle_transitions
		     (id, message_id, dedupe_key, direction, stage, outcome, reason_code, retryable, occurred_at)
		 SELECT 'mlt_' || id, id, 'seed', 'inbound', $2, $3, $4, $5, now()
		   FROM messages WHERE agent_id = $1`,
		f.agentID, stage, outcome, code, retryable); err != nil {
		t.Fatalf("seed message_lifecycle_transitions: %v", err)
	}
}

// seedEngagement gives the agent outreach state. contact_engagements has NO FK
// to agent_identities, so nothing cascades it — the purge has to delete it
// explicitly, exactly as the atomic path does.
func (f purgeFixture) seedEngagement(t *testing.T, contact string) {
	t.Helper()
	enroll(t, f.store, f.userID, f.agentID, contact+"@example.com", "touch1")
}

func (f purgeFixture) markDeleted(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE agent_identities SET deleted_at = now() WHERE id = $1`, f.agentID); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
}

func TestRestoreAgentRefusesClaimedPurge(t *testing.T) {
	f := newPurgeFixture(t, "purgerefuserestore")
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE agent_identities
		    SET deleted_at = now(), purge_token = 'pur_test_claim'
		  WHERE id = $1`, f.agentID); err != nil {
		t.Fatalf("claim purge: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE agent_identities SET deleted_at = NULL WHERE id = $1`, f.agentID); err == nil {
		t.Fatal("legacy restore update succeeded despite the purge-token constraint")
	}

	_, err := f.store.RestoreAgent(context.Background(), f.agentID, f.userID)
	if err == nil || err.Error() != "permanent purge is already in progress" {
		t.Fatalf("RestoreAgent(claimed purge) = %v, want purge-in-progress refusal", err)
	}
	if got := f.countTable(t, "agent_identities"); got != 1 {
		t.Fatalf("claimed agent rows = %d, want 1", got)
	}
	var deletedAt *time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT deleted_at FROM agent_identities WHERE id = $1`, f.agentID).Scan(&deletedAt); err != nil {
		t.Fatalf("read claimed agent: %v", err)
	}
	if deletedAt == nil {
		t.Fatal("restore cleared deleted_at on a claimed purge")
	}
}

func TestFinalSealCountsLateInboundWriter(t *testing.T) {
	f := newPurgeFixture(t, "purgesealinbound")
	token, err := f.store.ClaimAgentPurgeForTest(context.Background(), f.agentID, f.userID)
	if err != nil {
		t.Fatalf("claim purge: %v", err)
	}

	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(),
		`INSERT INTO messages (id, agent_id, direction, sender, recipient, subject)
		 VALUES ('msg_late_inbound', $1, 'inbound', 'sender@example.test', $1, 'late')`,
		f.agentID); err != nil {
		t.Fatalf("insert late inbound: %v", err)
	}

	type sealResult struct {
		sealed, gone bool
		deleted      int64
		err          error
	}
	done := make(chan sealResult, 1)
	go func() {
		sealed, gone, deleted, err := f.store.SealAgentPurgeForTest(
			context.Background(), f.agentID, f.userID, token)
		done <- sealResult{sealed, gone, deleted, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("seal completed before the pre-existing FK writer committed: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit late inbound: %v", err)
	}
	got := <-done
	if got.err != nil || got.sealed || got.gone || got.deleted != 1 {
		t.Fatalf("first seal = %+v, want one explicit late-message delete and a retry", got)
	}
	sealed, gone, deleted, err := f.store.SealAgentPurgeForTest(
		context.Background(), f.agentID, f.userID, token)
	if err != nil || !sealed || gone || deleted != 0 {
		t.Fatalf("second seal = sealed:%v gone:%v deleted:%d err:%v", sealed, gone, deleted, err)
	}
}

func TestFinalSealCancelsLateOutboundJob(t *testing.T) {
	f := newPurgeFixture(t, "purgesealoutbound")
	token, err := f.store.ClaimAgentPurgeForTest(context.Background(), f.agentID, f.userID)
	if err != nil {
		t.Fatalf("claim purge: %v", err)
	}
	canceller := &txCountingCanceller{}
	f.store.SetOutboundJobCanceller(canceller)

	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(),
		`INSERT INTO messages
		     (id, agent_id, direction, sender, recipient, subject, delivery_status, send_job_id)
		 VALUES ('msg_late_outbound', $1, 'outbound', $1, 'recipient@example.test', 'late', 'accepted', 4242)`,
		f.agentID); err != nil {
		t.Fatalf("insert late outbound: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		sealed, gone, deleted, err := f.store.SealAgentPurgeForTest(
			context.Background(), f.agentID, f.userID, token)
		if err == nil && (sealed || gone || deleted != 1) {
			err = errors.New("unexpected seal result")
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("seal completed before the pre-existing FK writer committed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit late outbound: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("seal late outbound: %v", err)
	}
	if len(canceller.jobIDs) != 1 || canceller.jobIDs[0] != 4242 {
		t.Fatalf("canceled jobs = %v, want [4242]", canceller.jobIDs)
	}
}

func TestStaleTokenCannotDeleteSameOwnerRecreation(t *testing.T) {
	f := newPurgeFixture(t, "purgesameowneraba")
	oldToken, err := f.store.ClaimAgentPurgeForTest(context.Background(), f.agentID, f.userID)
	if err != nil {
		t.Fatalf("claim old purge: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(),
		`DELETE FROM agent_identities WHERE id = $1`, f.agentID); err != nil {
		t.Fatalf("delete old incarnation: %v", err)
	}
	domain := "purgesameowneraba.example.com"
	if _, err := f.store.CreateAgent(context.Background(), f.agentID, domain, "", "", "", f.userID); err != nil {
		t.Fatalf("recreate agent: %v", err)
	}
	newToken, err := f.store.ClaimAgentPurgeForTest(context.Background(), f.agentID, f.userID)
	if err != nil {
		t.Fatalf("claim new purge: %v", err)
	}
	if newToken == oldToken {
		t.Fatal("fixture reused a purge token")
	}
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO messages (id, agent_id, direction, sender, recipient, subject)
		 VALUES ('msg_new_incarnation', $1, 'inbound', 'sender@example.test', $1, 'new')`,
		f.agentID); err != nil {
		t.Fatalf("seed new incarnation: %v", err)
	}

	deleted, err := f.store.DrainAgentWithTokenForTest(
		context.Background(), f.agentID, f.userID, oldToken)
	if err != nil || deleted != 0 {
		t.Fatalf("stale drain = deleted:%d err:%v, want clean no-op", deleted, err)
	}
	if got := f.countTable(t, "messages"); got != 1 {
		t.Fatalf("new incarnation messages = %d, want 1 untouched", got)
	}
	if got := f.countTable(t, "agent_identities"); got != 1 {
		t.Fatalf("new incarnation agent rows = %d, want 1", got)
	}
}

func TestDeleteAgentIncarnationRefusesSameOwnerRecreation(t *testing.T) {
	f := newPurgeFixture(t, "purgepreclaimaba")
	var oldCreatedAt time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT created_at FROM agent_identities WHERE id = $1`, f.agentID).Scan(&oldCreatedAt); err != nil {
		t.Fatalf("read old incarnation: %v", err)
	}
	if _, err := f.store.DeleteAgent(context.Background(), f.agentID, f.userID); err != nil {
		t.Fatalf("delete old incarnation: %v", err)
	}
	domain := "purgepreclaimaba.example.com"
	if _, err := f.store.CreateAgent(context.Background(), f.agentID, domain, "", "", "", f.userID); err != nil {
		t.Fatalf("recreate agent: %v", err)
	}
	f.seedMessages(t, 1, "mpa_")

	deleted, err := f.store.DeleteAgentIncarnation(
		context.Background(), f.agentID, f.userID, oldCreatedAt)
	if !errors.Is(err, identity.ErrAgentNotFound) || deleted != 0 {
		t.Fatalf("delayed old request = deleted:%d err:%v, want ErrAgentNotFound", deleted, err)
	}
	if got := f.countTable(t, "messages"); got != 1 {
		t.Fatalf("new incarnation messages = %d, want 1 untouched", got)
	}
}

func TestAtomicClassifierLocksConcurrentJobTransition(t *testing.T) {
	f := newPurgeFixture(t, "purgeatomicjobrace")
	const transitioned = identity.AgentPurgeCancelChunkRowsForTest + 1
	f.seedMessages(t, transitioned, "maj_")
	canceller := &txCountingCanceller{}
	f.store.SetOutboundJobCanceller(canceller)

	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin transition: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(),
		`UPDATE messages
		    SET direction = 'outbound', delivery_status = 'accepted',
		        send_job_id = substring(id from 5)::bigint
		  WHERE agent_id = $1`, f.agentID); err != nil {
		t.Fatalf("stage job transition: %v", err)
	}
	type result struct {
		deleted int64
		err     error
	}
	done := make(chan result, 1)
	go func() {
		deleted, err := f.store.DeleteAgent(context.Background(), f.agentID, f.userID)
		done <- result{deleted, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("delete passed an uncommitted message transition: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit job transition: %v", err)
	}
	got := <-done
	if got.err != nil || got.deleted != transitioned {
		t.Fatalf("DeleteAgent = %+v, want %d deleted", got, transitioned)
	}
	if len(canceller.jobIDs) != transitioned {
		t.Fatalf("canceled %d jobs, want %d", len(canceller.jobIDs), transitioned)
	}
	if max := canceller.maxPerTx(); max > identity.AgentPurgeCancelChunkRowsForTest {
		t.Fatalf("one transaction canceled %d jobs, bound is %d", max, identity.AgentPurgeCancelChunkRowsForTest)
	}
}

func TestFinalSealLocksConcurrentJobTransition(t *testing.T) {
	f := newPurgeFixture(t, "purgesealjobrace")
	f.seedMessages(t, 1, "msj_")
	token, err := f.store.ClaimAgentPurgeForTest(context.Background(), f.agentID, f.userID)
	if err != nil {
		t.Fatalf("claim purge: %v", err)
	}
	canceller := &txCountingCanceller{}
	f.store.SetOutboundJobCanceller(canceller)

	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin transition: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(),
		`UPDATE messages
		    SET direction = 'outbound', delivery_status = 'accepted', send_job_id = 5252
		  WHERE agent_id = $1`, f.agentID); err != nil {
		t.Fatalf("stage job transition: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		sealed, gone, deleted, err := f.store.SealAgentPurgeForTest(
			context.Background(), f.agentID, f.userID, token)
		if err == nil && (sealed || gone || deleted != 1) {
			err = errors.New("unexpected seal result")
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("seal passed an uncommitted message transition: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit job transition: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("seal transitioned job: %v", err)
	}
	if len(canceller.jobIDs) != 1 || canceller.jobIDs[0] != 5252 {
		t.Fatalf("canceled jobs = %v, want [5252]", canceller.jobIDs)
	}
}

func TestFinalSealLocksConcurrentEngagementUpdate(t *testing.T) {
	f := newPurgeFixture(t, "purgesealengagementrace")
	f.seedEngagement(t, "partner")
	token, err := f.store.ClaimAgentPurgeForTest(context.Background(), f.agentID, f.userID)
	if err != nil {
		t.Fatalf("claim purge: %v", err)
	}

	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin engagement update: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(),
		`UPDATE contact_engagements
		    SET stage = 'touch2', updated_at = now()
		  WHERE user_id = $1 AND agent_id = $2`, f.userID, f.agentID); err != nil {
		t.Fatalf("stage engagement update: %v", err)
	}

	type sealResult struct {
		sealed, gone bool
		deleted      int64
		err          error
	}
	done := make(chan sealResult, 1)
	go func() {
		sealed, gone, deleted, err := f.store.SealAgentPurgeForTest(
			context.Background(), f.agentID, f.userID, token)
		done <- sealResult{sealed, gone, deleted, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("seal passed an uncommitted engagement update: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit engagement update: %v", err)
	}
	got := <-done
	if got.err != nil || got.sealed || got.gone || got.deleted != 0 {
		t.Fatalf("first seal = %+v, want engagement drain and a retry", got)
	}
	if remaining := f.countTable(t, "contact_engagements"); remaining != 0 {
		t.Fatalf("engagements after first seal = %d, want 0", remaining)
	}

	sealed, gone, deleted, err := f.store.SealAgentPurgeForTest(
		context.Background(), f.agentID, f.userID, token)
	if err != nil || !sealed || gone || deleted != 0 {
		t.Fatalf("second seal = sealed:%v gone:%v deleted:%d err:%v", sealed, gone, deleted, err)
	}
}

func (f purgeFixture) countTable(t *testing.T, table string) int64 {
	t.Helper()
	var n int64
	q := map[string]string{
		"messages":            `SELECT count(*) FROM messages WHERE agent_id = $1`,
		"contact_engagements": `SELECT count(*) FROM contact_engagements WHERE agent_id = $1`,
		"agent_identities":    `SELECT count(*) FROM agent_identities WHERE id = $1`,
	}[table]
	if q == "" {
		t.Fatalf("countTable: no query for %q", table)
	}
	if err := f.pool.QueryRow(context.Background(), q, f.agentID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// countCascadeChildren counts the seeded cascade rows by MESSAGE-ID PREFIX,
// never by joining messages. A join would make the assertion vacuous: once the
// messages are gone the join is empty whether the children cascaded with them
// or were left orphaned — and an orphan is exactly the failure being looked
// for.
func (f purgeFixture) countCascadeChildren(t *testing.T, msgPrefix string) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	counts := map[string]int64{}
	for table, q := range map[string]string{
		"webhook_deliveries":            `SELECT count(*) FROM webhook_deliveries WHERE message_id LIKE $1`,
		"message_recipients":            `SELECT count(*) FROM message_recipients WHERE message_id LIKE $1`,
		"message_lifecycle_transitions": `SELECT count(*) FROM message_lifecycle_transitions WHERE message_id LIKE $1`,
	} {
		var n int64
		if err := f.pool.QueryRow(ctx, q, msgPrefix+"%").Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = n
	}
	return counts
}

// onFirstChunkCommitted runs fn once, as soon as the agent's message count has
// dropped below start — i.e. as soon as one chunk has COMMITTED — and stops
// polling when the test ends. It is how these tests reach INTO a running drain
// without a hook in production code.
func (f purgeFixture) onFirstChunkCommitted(t *testing.T, start int64, fn func()) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			var n int64
			if err := f.pool.QueryRow(context.Background(),
				`SELECT count(*) FROM messages WHERE agent_id = $1`, f.agentID).Scan(&n); err == nil && n < start {
				fn()
				return
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
}

// TestDrainAgentChunksMatchesSingleShotDelete is the central equivalence claim:
// draining an inbox in committed chunks removes exactly what one unbounded
// DELETE's cascade would, and the summed chunk counts equal that statement's
// RowsAffected. Two identical agents, one deleted each way.
func TestDrainAgentChunksMatchesSingleShotDelete(t *testing.T) {
	const seeded = 12000 // > 2 chunks, so the loop genuinely iterates

	chunked := newPurgeFixture(t, "purgechunked")
	chunked.seedMessages(t, seeded, "mc_")
	chunked.seedCascadeChildren(t)
	chunked.seedEngagement(t, "partner")
	chunked.markDeleted(t)

	// The baseline agent lives in the same database; give it its own domain so
	// the two never share rows.
	ctx := context.Background()
	baselineUser, err := chunked.store.CreateOrGetUser(ctx, "baseline@example.com", "Owner", "google-baseline")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := chunked.store.ClaimOrCreateDomain(ctx, "baseline.example.com", baselineUser.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	baselineAgent, err := chunked.store.CreateAgent(ctx, "bot@baseline.example.com", "baseline.example.com", "", "", "", baselineUser.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	baseline := purgeFixture{pool: chunked.pool, store: chunked.store, userID: baselineUser.ID, agentID: baselineAgent.ID}
	baseline.seedMessages(t, seeded, "mb_")
	baseline.seedCascadeChildren(t)

	// Prove the fixture actually put rows in every cascade table before
	// claiming they are all zero afterwards — otherwise the assertions below
	// pass on an empty database.
	for table, n := range chunked.countCascadeChildren(t, "mc_") {
		if n != seeded {
			t.Fatalf("fixture seeded %d %s rows, want %d — the post-purge assertion would be vacuous", n, table, seeded)
		}
	}

	singleShot, err := chunked.pool.Exec(ctx, `DELETE FROM messages WHERE agent_id = $1`, baseline.agentID)
	if err != nil {
		t.Fatalf("single-shot delete: %v", err)
	}

	got, err := chunked.store.DrainAgentChunksForTest(ctx, chunked.agentID, chunked.userID)
	if err != nil {
		t.Fatalf("DrainAgentChunksForTest: %v", err)
	}
	if got != singleShot.RowsAffected() {
		t.Errorf("chunked total = %d, single-statement RowsAffected = %d — the receipt must not depend on the path taken",
			got, singleShot.RowsAffected())
	}
	if got != seeded {
		t.Errorf("chunked total = %d, want %d", got, seeded)
	}

	// Everything the agent owned directly is gone, agent row included.
	for _, table := range []string{"messages", "contact_engagements", "agent_identities"} {
		if n := chunked.countTable(t, table); n != 0 {
			t.Errorf("%s: %d rows survived the chunked purge, want 0", table, n)
		}
	}
	// And so is everything that hangs off those messages. Compared against the
	// single-shot agent's own children so the claim is "the two paths leave the
	// database in the same state", not merely "some rows disappeared".
	chunkedChildren := chunked.countCascadeChildren(t, "mc_")
	baselineChildren := baseline.countCascadeChildren(t, "mb_")
	for table, n := range chunkedChildren {
		if n != 0 {
			t.Errorf("%s: %d orphaned rows survived the chunked purge, want 0", table, n)
		}
		if baselineChildren[table] != 0 {
			t.Errorf("%s: %d rows survived the SINGLE-SHOT delete — the cascade this test compares against does not exist, so the chunked assertion above proves nothing",
				table, baselineChildren[table])
		}
	}
}

// TestDrainAgentChunksResumesAfterInterruption drives the real failure mode: an
// attempt that dies partway (client hang-up, shutdown, deploy). The drain must
// leave a consistent committed prefix and a re-issued delete must finish the
// job — which is only true because each chunk commits on its own. This is the
// non-atomic half of the trade the threshold buys, exercised deliberately.
func TestDrainAgentChunksResumesAfterInterruption(t *testing.T) {
	const seeded = 20000 // 4 chunks

	f := newPurgeFixture(t, "purgeresume")
	f.seedMessages(t, seeded, "mr_")
	f.markDeleted(t)

	// Cancel as soon as at least one chunk has COMMITTED. The drain loop checks
	// ctx between chunks, so this reliably stops it mid-drain rather than at an
	// arbitrary point inside a transaction.
	ctx, cancel := context.WithCancel(context.Background())
	f.onFirstChunkCommitted(t, seeded, cancel)

	firstRun, err := f.store.DrainAgentChunksForTest(ctx, f.agentID, f.userID)
	if err == nil {
		t.Fatalf("interrupted drain returned nil error (deleted %d) — the cancellation never took effect", firstRun)
	}
	remaining := f.countTable(t, "messages")
	if remaining == 0 {
		t.Fatalf("interrupted drain emptied the whole inbox — nothing left to resume, the test proves nothing")
	}
	if f.countTable(t, "agent_identities") != 1 {
		t.Fatal("interrupted drain removed the agent row; the address must not be freed until the drain completes")
	}
	if firstRun+remaining != seeded {
		t.Errorf("first run deleted %d, %d remain, want them to sum to %d — a lost chunk means an uncommitted or double-counted batch",
			firstRun, remaining, seeded)
	}

	secondRun, err := f.store.DrainAgentChunksForTest(context.Background(), f.agentID, f.userID)
	if err != nil {
		t.Fatalf("resumed drain: %v", err)
	}
	if secondRun != remaining {
		t.Errorf("resumed drain deleted %d, want the %d that survived the interruption", secondRun, remaining)
	}
	if n := f.countTable(t, "messages"); n != 0 {
		t.Errorf("%d messages survived the resumed drain", n)
	}
	if n := f.countTable(t, "agent_identities"); n != 0 {
		t.Errorf("agent row survived the resumed drain")
	}

	// A third run finds nothing and must still succeed: a caller who retries a
	// delete whose response they never saw must not get an error.
	third, err := f.store.DrainAgentChunksForTest(context.Background(), f.agentID, f.userID)
	if err != nil {
		t.Fatalf("drain of an already-purged agent must be a no-op success, got %v", err)
	}
	if third != 0 {
		t.Errorf("drain of an already-purged agent reported %d messages deleted, want 0", third)
	}
}

func TestPurgeDeletedAgentsResumesFreshClaimedPurge(t *testing.T) {
	f := newPurgeFixture(t, "purgejanitorresume")
	const seeded = identity.InlinePurgeMaxMessages + 2
	f.seedMessages(t, seeded, "mjr_")
	if _, err := f.store.ClaimAgentPurgeForTest(
		context.Background(), f.agentID, f.userID); err != nil {
		t.Fatalf("claim purge: %v", err)
	}
	// Model one committed prefix from an interrupted request. The claim's
	// deleted_at is intentionally fresh, so only the token-aware janitor arm
	// can select and resume it.
	if _, err := f.pool.Exec(context.Background(),
		`DELETE FROM messages
		  WHERE ctid IN (
		        SELECT ctid FROM messages WHERE agent_id = $1 LIMIT 1)`, f.agentID); err != nil {
		t.Fatalf("commit interrupted prefix: %v", err)
	}

	purged, err := f.store.PurgeDeletedAgents(context.Background())
	if err != nil {
		t.Fatalf("PurgeDeletedAgents: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged agents = %d, want 1 resumed claim", purged)
	}
	for _, table := range []string{"messages", "agent_identities"} {
		if got := f.countTable(t, table); got != 0 {
			t.Fatalf("%s after janitor resume = %d, want 0", table, got)
		}
	}
}

// TestDrainAgentChunksRefusesRestoreMidPurge is the data-loss guard. Once the
// durable claim commits, restore is no longer a legal transition.
func TestDrainAgentChunksRefusesRestoreMidPurge(t *testing.T) {
	const seeded = 20000

	f := newPurgeFixture(t, "purgerestore")
	f.seedMessages(t, seeded, "mx_")
	f.markDeleted(t)

	restored := make(chan error, 1)
	f.onFirstChunkCommitted(t, seeded, func() {
		_, err := f.store.RestoreAgent(context.Background(), f.agentID, f.userID)
		restored <- err
	})

	deleted, err := f.store.DrainAgentChunksForTest(context.Background(), f.agentID, f.userID)
	if err != nil {
		t.Fatalf("drain with a concurrent restore attempt = %v", err)
	}
	select {
	case restoreErr := <-restored:
		if !errors.Is(restoreErr, identity.ErrPurgeInProgress) {
			t.Fatalf("RestoreAgent during purge = %v, want ErrPurgeInProgress", restoreErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("restore attempt never ran")
	}
	if deleted != seeded {
		t.Errorf("drain deleted %d messages, want %d", deleted, seeded)
	}
	if n := f.countTable(t, "agent_identities"); n != 0 {
		t.Error("claimed purge did not finish after restore was refused")
	}
}

// TestDrainAgentChunksAbortsWhenNotInTrash is the same guard at the first
// chunk: an agent that is not marked deleted is never drained at all. (The
// production entry point marks it first; this pins that the drain does not
// trust its caller.)
func TestDrainAgentChunksAbortsWhenNotInTrash(t *testing.T) {
	f := newPurgeFixture(t, "purgelive")
	f.seedMessages(t, 10, "ml_")
	// deliberately NOT marked deleted

	deleted, err := f.store.DrainAgentChunksForTest(context.Background(), f.agentID, f.userID)
	if !errors.Is(err, identity.ErrNotInTrash) {
		t.Fatalf("drain of a live agent = %v, want ErrNotInTrash", err)
	}
	if deleted != 0 {
		t.Errorf("drain of a live agent deleted %d messages, want 0", deleted)
	}
	if n := f.countTable(t, "messages"); n != 10 {
		t.Errorf("messages = %d after refused drain, want 10 untouched", n)
	}
}

// TestDrainAgentChunksAbortsForAnotherOwner: every chunk transaction re-reads
// the agent row scoped to the OWNER, not just the address. agent_identities.id
// IS the email address and addresses are reusable, so an unscoped drain that
// found "a trashed row at this address" would happily eat a different account's
// inbox. This is the narrowed form of the ABA defect that killed the deferred
// design.
func TestDrainAgentChunksAbortsForAnotherOwner(t *testing.T) {
	f := newPurgeFixture(t, "purgeowner")
	f.seedMessages(t, 10, "mo_")
	f.markDeleted(t)

	deleted, err := f.store.DrainAgentChunksForTest(context.Background(), f.agentID, "u_someone_else")
	if err != nil {
		t.Fatalf("drain for a non-owner = %v, want a clean stop", err)
	}
	if deleted != 0 {
		t.Errorf("drain for a non-owner deleted %d messages, want 0", deleted)
	}
	if n := f.countTable(t, "messages"); n != 10 {
		t.Errorf("messages = %d after a non-owner drain, want 10 untouched", n)
	}
	if n := f.countTable(t, "agent_identities"); n != 1 {
		t.Error("a non-owner drain removed the agent row")
	}
}

// TestDrainAgentChunksAbortsOnSendInProgress: a send lease can be taken between
// chunks, so every chunk re-applies the check rather than trusting the
// prologue's.
func TestDrainAgentChunksAbortsOnSendInProgress(t *testing.T) {
	f := newPurgeFixture(t, "purgesending")
	f.seedMessages(t, 10, "ms_")
	f.markDeleted(t)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE messages SET direction = 'outbound', delivery_status = 'sending', send_claimed_at = now()
		  WHERE id = 'ms_1'`); err != nil {
		t.Fatalf("claim a send: %v", err)
	}

	deleted, err := f.store.DrainAgentChunksForTest(context.Background(), f.agentID, f.userID)
	if !errors.Is(err, identity.ErrSendInProgress) {
		t.Fatalf("drain with a live send lease = %v, want ErrSendInProgress", err)
	}
	if deleted != 0 {
		t.Errorf("drain deleted %d messages while a send was in flight, want 0", deleted)
	}
	if n := f.countTable(t, "messages"); n != 10 {
		t.Errorf("messages = %d after refused drain, want 10 untouched", n)
	}
}

// TestDeleteAgentIsAtomicAtOrBelowThreshold pins the half of the trade that did
// NOT change. At or below the threshold the delete is still ONE transaction, so
// a failure anywhere in it leaves the inbox exactly as it was — messages,
// outreach state and the agent row all intact.
//
// The failure is forced with a temporary BEFORE DELETE trigger scoped by WHEN
// to this one agent id, which fires at the LAST statement of the transaction —
// after the messages and engagements have already been deleted. Nothing weaker
// would prove rollback: a failure before the deletes would leave the rows
// standing whether or not the transaction is atomic.
func TestDeleteAgentIsAtomicAtOrBelowThreshold(t *testing.T) {
	f := newPurgeFixture(t, "purgeatomic")
	const seeded = identity.InlinePurgeMaxMessages // exactly at the threshold
	f.seedMessages(t, seeded, "ma_")
	f.seedEngagement(t, "partner")
	f.markDeleted(t)

	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`CREATE OR REPLACE FUNCTION e2a_test_block_agent_delete() RETURNS trigger AS $$
		 BEGIN RAISE EXCEPTION 'forced failure at the last statement'; END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create blocking function: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`CREATE TRIGGER e2a_test_block_agent_delete_trg BEFORE DELETE ON agent_identities
		   FOR EACH ROW WHEN (OLD.id = '`+f.agentID+`')
		   EXECUTE FUNCTION e2a_test_block_agent_delete()`); err != nil {
		t.Fatalf("create blocking trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS e2a_test_block_agent_delete_trg ON agent_identities`)
		_, _ = f.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS e2a_test_block_agent_delete()`)
	})

	engagementsBefore := f.countTable(t, "contact_engagements")
	if engagementsBefore == 0 {
		t.Fatal("fixture seeded no contact_engagements — the rollback assertion below would be vacuous")
	}

	n, err := f.store.DeleteAgent(ctx, f.agentID, f.userID)
	if err == nil {
		t.Fatalf("DeleteAgent succeeded (%d) despite the blocking trigger", n)
	}
	if got := f.countTable(t, "messages"); got != seeded {
		t.Errorf("messages = %d after a failed at-threshold delete, want all %d rolled back — this path must stay atomic",
			got, seeded)
	}
	if got := f.countTable(t, "contact_engagements"); got != engagementsBefore {
		t.Errorf("contact_engagements = %d after a failed at-threshold delete, want %d rolled back", got, engagementsBefore)
	}
	if got := f.countTable(t, "agent_identities"); got != 1 {
		t.Errorf("agent_identities = %d, want the agent row still there", got)
	}
}

// TestDeleteAgentIsNotAtomicAboveThreshold is the same forced failure one
// message above the threshold, and it must behave DIFFERENTLY: the chunks that
// committed before the failure are gone for good. This is the documented cost
// of bounded transactions, asserted rather than assumed — if this test ever
// starts passing like the atomic one, the threshold branch stopped chunking.
func TestDeleteAgentIsNotAtomicAboveThreshold(t *testing.T) {
	f := newPurgeFixture(t, "purgenonatomic")
	const seeded = identity.InlinePurgeMaxMessages + 1
	f.seedMessages(t, seeded, "mz_")
	f.markDeleted(t)

	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`CREATE OR REPLACE FUNCTION e2a_test_block_agent_delete() RETURNS trigger AS $$
		 BEGIN RAISE EXCEPTION 'forced failure at the last statement'; END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create blocking function: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`CREATE TRIGGER e2a_test_block_agent_delete_trg BEFORE DELETE ON agent_identities
		   FOR EACH ROW WHEN (OLD.id = '`+f.agentID+`')
		   EXECUTE FUNCTION e2a_test_block_agent_delete()`); err != nil {
		t.Fatalf("create blocking trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS e2a_test_block_agent_delete_trg ON agent_identities`)
		_, _ = f.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS e2a_test_block_agent_delete()`)
	})

	deleted, err := f.store.DeleteAgent(ctx, f.agentID, f.userID)
	if err == nil {
		t.Fatalf("DeleteAgent succeeded (%d) despite the blocking trigger", deleted)
	}
	if deleted != seeded {
		t.Errorf("chunked delete reported %d before failing, want the %d it had already committed", deleted, seeded)
	}
	if got := f.countTable(t, "messages"); got != 0 {
		t.Errorf("messages = %d — above the threshold the committed chunks must NOT roll back", got)
	}
	if got := f.countTable(t, "agent_identities"); got != 1 {
		t.Error("the agent row went away despite the blocked delete")
	}
}

// TestDeleteAgentAtOrBelowThresholdSucceeds pins the common path end to end:
// an inbox at the threshold behaves exactly as it always has — agent gone,
// address freed, receipt from RowsAffected.
func TestDeleteAgentAtOrBelowThresholdSucceeds(t *testing.T) {
	f := newPurgeFixture(t, "purgeinline")
	f.seedMessages(t, identity.InlinePurgeMaxMessages, "mi_")
	f.seedEngagement(t, "partner")

	n, err := f.store.DeleteAgent(context.Background(), f.agentID, f.userID)
	if err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	if n != identity.InlinePurgeMaxMessages {
		t.Errorf("messages_deleted = %d, want %d", n, identity.InlinePurgeMaxMessages)
	}
	for _, table := range []string{"messages", "contact_engagements", "agent_identities"} {
		if got := f.countTable(t, table); got != 0 {
			t.Errorf("%s: %d rows survived an at-threshold permanent delete, want 0", table, got)
		}
	}
}

// TestDeleteAgentAboveThresholdCountsRowsActuallyRemoved is the receipt claim.
// messages_deleted above the threshold is the SUMMED RowsAffected of the chunks,
// not the pre-count that selected the chunked shape.
//
// The two are only distinguishable if the inbox changes after the count, so the
// test inserts more mail once the drain is under way — the state a real inbox
// is in, since it is only marked deleted at the start of the delete. A
// pre-count implementation reports the seeded number and fails here.
func TestDeleteAgentAboveThresholdCountsRowsActuallyRemoved(t *testing.T) {
	const seeded = 20000 // 4 chunks: plenty of drain left after the first commit
	const late = 500

	f := newPurgeFixture(t, "purgereceipt")
	f.seedMessages(t, seeded, "mp_")

	inserted := make(chan int64, 1)
	f.onFirstChunkCommitted(t, seeded, func() {
		tag, err := f.pool.Exec(context.Background(),
			`INSERT INTO messages (id, agent_id, direction, sender, recipient, subject)
			 SELECT 'mlate_' || i, $1, 'inbound', 'sender@example.test', $1, 'late'
			   FROM generate_series(1, $2) AS i`,
			f.agentID, late)
		if err != nil {
			inserted <- 0
			return
		}
		inserted <- tag.RowsAffected()
	})

	got, err := f.store.DeleteAgent(context.Background(), f.agentID, f.userID)
	if err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	var lateRows int64
	select {
	case lateRows = <-inserted:
	case <-time.After(10 * time.Second):
		t.Fatal("the late insert never ran — the race this test depends on was not established")
	}
	if lateRows != late {
		t.Fatalf("late insert wrote %d rows, want %d — the race was not established, so the assertion below proves nothing", lateRows, late)
	}

	if want := int64(seeded) + lateRows; got != want {
		t.Errorf("messages_deleted = %d, want %d (the %d seeded plus the %d that arrived after the threshold count) — the receipt must be the summed RowsAffected, not the pre-count",
			got, want, seeded, lateRows)
	}
	if n := f.countTable(t, "messages"); n != 0 {
		t.Errorf("%d messages survived the chunked delete", n)
	}
	if n := f.countTable(t, "agent_identities"); n != 0 {
		t.Error("agent row survived the chunked delete; the address is never freed")
	}
}

// TestDeleteAgentAboveThresholdQuarantinesClaimFromOldJanitor verifies the
// rolling-deploy guard: an old binary does not understand purge_token, so the
// claim resets an expired trash clock and keeps its legacy janitor from
// immediately taking this partial purge down the old unbounded path.
func TestDeleteAgentAboveThresholdQuarantinesClaimFromOldJanitor(t *testing.T) {
	const seeded = 20000 // 4 chunks, so the mid-drain read has room to land

	f := newPurgeFixture(t, "purgetrashts")
	f.seedMessages(t, seeded, "mt_")

	ctx := context.Background()
	if err := f.store.SoftDeleteAgent(ctx, f.agentID, f.userID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE agent_identities SET deleted_at = now() - interval '60 days' WHERE id = $1`,
		f.agentID); err != nil {
		t.Fatalf("age trash row: %v", err)
	}
	var before time.Time
	if err := f.pool.QueryRow(ctx, `SELECT deleted_at FROM agent_identities WHERE id = $1`, f.agentID).Scan(&before); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}

	// Read the timestamp back mid-drain: once the drain finishes the row is
	// gone, so the only place the clock could be observed being reset is while
	// the purge is still running.
	seen := make(chan *time.Time, 1)
	f.onFirstChunkCommitted(t, seeded, func() {
		var during *time.Time
		if err := f.pool.QueryRow(context.Background(),
			`SELECT deleted_at FROM agent_identities WHERE id = $1`, f.agentID).Scan(&during); err != nil {
			seen <- nil
			return
		}
		seen <- during
	})

	if _, err := f.store.DeleteAgent(ctx, f.agentID, f.userID); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	select {
	case during := <-seen:
		if during == nil {
			t.Fatal("could not read deleted_at during the drain — the assertion would be vacuous")
		}
		if !during.After(before) {
			t.Errorf("deleted_at did not advance from %v during claim: got %v", before, *during)
		}
		if time.Since(*during) > time.Minute {
			t.Errorf("claim timestamp %v is not a current rollback quarantine", *during)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never observed the agent mid-drain")
	}
}

// TestDeleteAgentNotFoundInBothPaths: a missing agent 404s whether or not the
// count would have crossed the threshold, and an oversized inbox belonging to
// someone else is never touched.
func TestDeleteAgentNotFoundInBothPaths(t *testing.T) {
	f := newPurgeFixture(t, "purgemissing")

	if _, err := f.store.DeleteAgent(context.Background(), "ghost@nowhere.example.com", f.userID); !errors.Is(err, identity.ErrAgentNotFound) {
		t.Errorf("DeleteAgent(missing) = %v, want ErrAgentNotFound", err)
	}
	// Wrong owner is the same answer on the chunked path too, and must not
	// touch a row.
	f.seedMessages(t, identity.InlinePurgeMaxMessages+1, "mn_")
	if _, err := f.store.DeleteAgent(context.Background(), f.agentID, "u_someone_else"); !errors.Is(err, identity.ErrAgentNotFound) {
		t.Errorf("DeleteAgent(wrong owner) = %v, want ErrAgentNotFound", err)
	}
	if n := f.countTable(t, "messages"); n != identity.InlinePurgeMaxMessages+1 {
		t.Errorf("messages = %d after a rejected delete, want them untouched", n)
	}
	var deletedAt *time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT deleted_at FROM agent_identities WHERE id = $1`, f.agentID).Scan(&deletedAt); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}
	if deletedAt != nil {
		t.Error("a rejected delete still trashed the agent")
	}
}

// TestDeleteAgentAboveThresholdRefusesSendInProgress: the chunked path's
// prologue applies the same send-in-flight refusal as the atomic one, and must
// leave the agent untouched — not trashed — when it fires.
func TestDeleteAgentAboveThresholdRefusesSendInProgress(t *testing.T) {
	f := newPurgeFixture(t, "purgesendprologue")
	f.seedMessages(t, identity.InlinePurgeMaxMessages+1, "mq_")
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE messages SET direction = 'outbound', delivery_status = 'sending', send_claimed_at = now()
		  WHERE id = 'mq_1'`); err != nil {
		t.Fatalf("claim a send: %v", err)
	}

	if _, err := f.store.DeleteAgent(context.Background(), f.agentID, f.userID); !errors.Is(err, identity.ErrSendInProgress) {
		t.Fatalf("DeleteAgent with a live send lease = %v, want ErrSendInProgress", err)
	}
	if n := f.countTable(t, "messages"); n != identity.InlinePurgeMaxMessages+1 {
		t.Errorf("messages = %d after a refused delete, want them untouched", n)
	}
	var deletedAt *time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT deleted_at FROM agent_identities WHERE id = $1`, f.agentID).Scan(&deletedAt); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}
	if deletedAt != nil {
		t.Error("a refused delete still trashed the agent")
	}
}

// txCountingCanceller records every send-job cancellation AND which
// transaction it happened in, so a test can assert both that the right jobs
// were cancelled and that no single transaction cancelled an unbounded number
// of them.
type txCountingCanceller struct {
	mu     sync.Mutex
	jobIDs []int64
	perTx  map[pgx.Tx]int
}

func (c *txCountingCanceller) CancelTx(_ context.Context, tx pgx.Tx, jobID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.perTx == nil {
		c.perTx = map[pgx.Tx]int{}
	}
	c.jobIDs = append(c.jobIDs, jobID)
	c.perTx[tx]++
	return nil
}

func (c *txCountingCanceller) maxPerTx() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	max := 0
	for _, n := range c.perTx {
		if n > max {
			max = n
		}
	}
	return max
}

// TestDeleteAgentAboveThresholdCancelsSendJobsInBoundedBatches covers the one
// piece of the chunked path that is NOT a plain delete. Every message still
// queued for delivery carries a durable River job that must be cancelled with
// it, and cancelling costs a round trip per job — so an outreach agent with a
// full scheduled-send queue is exactly the unbounded transaction this change
// exists to remove. The cancellations must all happen, and no single
// transaction may carry more than agentPurgeCancelChunkRows of them.
func TestDeleteAgentAboveThresholdCancelsSendJobsInBoundedBatches(t *testing.T) {
	const seeded = identity.InlinePurgeMaxMessages + 1
	const queued = 1200 // > 2 cancel chunks

	f := newPurgeFixture(t, "purgecancel")
	f.seedMessages(t, seeded, "mk_")
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE messages
		    SET direction = 'outbound', delivery_status = 'accepted',
		        send_job_id = substring(id from 4)::bigint
		  WHERE agent_id = $1 AND substring(id from 4)::bigint <= $2`,
		f.agentID, queued); err != nil {
		t.Fatalf("queue sends: %v", err)
	}

	canceller := &txCountingCanceller{}
	f.store.SetOutboundJobCanceller(canceller)

	deleted, err := f.store.DeleteAgent(context.Background(), f.agentID, f.userID)
	if err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	if deleted != seeded {
		t.Errorf("messages_deleted = %d, want %d", deleted, seeded)
	}
	if len(canceller.jobIDs) != queued {
		t.Errorf("cancelled %d send jobs, want %d — a job that outlives its message fires against nothing",
			len(canceller.jobIDs), queued)
	}
	seen := map[int64]bool{}
	for _, id := range canceller.jobIDs {
		if seen[id] {
			t.Fatalf("send job %d cancelled twice", id)
		}
		seen[id] = true
	}
	if got := canceller.maxPerTx(); got > identity.AgentPurgeCancelChunkRowsForTest {
		t.Errorf("one transaction cancelled %d send jobs, bound is %d — the cancel work is unbounded again",
			got, identity.AgentPurgeCancelChunkRowsForTest)
	}
	if got := canceller.maxPerTx(); got == 0 {
		t.Fatal("no cancellation was recorded against any transaction — the bound assertion above is vacuous")
	}
	if n := f.countTable(t, "messages"); n != 0 {
		t.Errorf("%d messages survived", n)
	}
}

func TestDeleteAgentCancellationLimitSelectsChunking(t *testing.T) {
	f := newPurgeFixture(t, "purgejobclassifier")
	const queued = identity.AgentPurgeCancelChunkRowsForTest + 1
	f.seedMessages(t, queued, "mjc_")
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE messages
		    SET direction = 'outbound', delivery_status = 'accepted',
		        send_job_id = substring(id from 5)::bigint
		  WHERE agent_id = $1`, f.agentID); err != nil {
		t.Fatalf("queue sends: %v", err)
	}
	f.store.SetOutboundJobCanceller(&txCountingCanceller{})

	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`CREATE OR REPLACE FUNCTION e2a_test_block_job_classifier_delete() RETURNS trigger AS $$
		 BEGIN RAISE EXCEPTION 'forced classifier failure'; END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create blocking function: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`CREATE TRIGGER e2a_test_block_job_classifier_delete_trg BEFORE DELETE ON agent_identities
		   FOR EACH ROW WHEN (OLD.id = '`+f.agentID+`')
		   EXECUTE FUNCTION e2a_test_block_job_classifier_delete()`); err != nil {
		t.Fatalf("create blocking trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS e2a_test_block_job_classifier_delete_trg ON agent_identities`)
		_, _ = f.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS e2a_test_block_job_classifier_delete()`)
	})

	deleted, err := f.store.DeleteAgent(ctx, f.agentID, f.userID)
	if err == nil || deleted != queued {
		t.Fatalf("DeleteAgent = deleted:%d err:%v, want %d committed rows then final failure", deleted, err, queued)
	}
	if got := f.countTable(t, "messages"); got != 0 {
		t.Fatalf("messages = %d, want chunked committed deletion", got)
	}
}

func TestDeleteAgentEngagementLimitSelectsChunking(t *testing.T) {
	f := newPurgeFixture(t, "purgeengagementclassifier")
	f.seedMessages(t, 1, "mec_")
	const engagements = identity.InlinePurgeMaxMessages + 1
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO contacts (id, user_id, address, source)
		 SELECT 'contact_classifier_' || i, $1,
		        'contact-' || i || '@example.test', 'manual'
		   FROM generate_series(1, $2) AS i`, f.userID, engagements); err != nil {
		t.Fatalf("seed contacts: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO contact_engagements (id, user_id, contact_id, agent_id, address)
		 SELECT 'engagement_classifier_' || i, $1,
		        'contact_classifier_' || i, $2,
		        'contact-' || i || '@example.test'
		   FROM generate_series(1, $3) AS i`, f.userID, f.agentID, engagements); err != nil {
		t.Fatalf("seed engagements: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`CREATE OR REPLACE FUNCTION e2a_test_block_engagement_classifier_delete() RETURNS trigger AS $$
		 BEGIN RAISE EXCEPTION 'forced classifier failure'; END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create blocking function: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`CREATE TRIGGER e2a_test_block_engagement_classifier_delete_trg BEFORE DELETE ON agent_identities
		   FOR EACH ROW WHEN (OLD.id = '`+f.agentID+`')
		   EXECUTE FUNCTION e2a_test_block_engagement_classifier_delete()`); err != nil {
		t.Fatalf("create blocking trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS e2a_test_block_engagement_classifier_delete_trg ON agent_identities`)
		_, _ = f.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS e2a_test_block_engagement_classifier_delete()`)
	})

	deleted, err := f.store.DeleteAgent(ctx, f.agentID, f.userID)
	if err == nil || deleted != 1 {
		t.Fatalf("DeleteAgent = deleted:%d err:%v, want one committed message then final failure", deleted, err)
	}
	if got := f.countTable(t, "messages"); got != 0 {
		t.Fatalf("messages = %d, want chunked committed deletion", got)
	}
	if got := f.countTable(t, "contact_engagements"); got != 0 {
		t.Fatalf("engagements = %d, want bounded committed deletion", got)
	}
}

func TestPurgeDeletedAgentsSkipsPoisonClaimForThisSweep(t *testing.T) {
	poison := newPurgeFixture(t, "purgejanitorpoison")
	poison.seedMessages(t, 1, "mjp_")
	if _, err := poison.store.ClaimAgentPurgeForTest(
		context.Background(), poison.agentID, poison.userID); err != nil {
		t.Fatalf("claim poison purge: %v", err)
	}
	if _, err := poison.pool.Exec(context.Background(),
		`UPDATE messages
		    SET direction = 'outbound', delivery_status = 'sending', send_claimed_at = now()
		  WHERE agent_id = $1`, poison.agentID); err != nil {
		t.Fatalf("install poison send lease: %v", err)
	}

	user, err := poison.store.CreateOrGetUser(
		context.Background(), "purgejanitorready@example.test", "Owner", "google-purgejanitorready")
	if err != nil {
		t.Fatalf("create ready owner: %v", err)
	}
	domain := "purgejanitorready.example.test"
	if _, err := poison.store.ClaimOrCreateDomain(context.Background(), domain, user.ID); err != nil {
		t.Fatalf("create ready domain: %v", err)
	}
	agent, err := poison.store.CreateAgent(
		context.Background(), "bot@"+domain, domain, "", "", "", user.ID)
	if err != nil {
		t.Fatalf("create ready agent: %v", err)
	}
	ready := purgeFixture{
		pool: poison.pool, store: poison.store, userID: user.ID, agentID: agent.ID,
	}
	ready.seedMessages(t, 1, "mjr_")
	ready.markDeleted(t)
	if _, err := ready.pool.Exec(context.Background(),
		`UPDATE agent_identities SET deleted_at = now() - interval '60 days' WHERE id = $1`,
		ready.agentID); err != nil {
		t.Fatalf("age ready agent: %v", err)
	}

	purged, err := poison.store.PurgeDeletedAgents(context.Background())
	if !errors.Is(err, identity.ErrSendInProgress) {
		t.Fatalf("PurgeDeletedAgents error = %v, want ErrSendInProgress from poison row", err)
	}
	if purged != 1 {
		t.Fatalf("PurgeDeletedAgents purged %d ready agents, want 1", purged)
	}
	if got := ready.countTable(t, "agent_identities"); got != 0 {
		t.Fatalf("ready agent rows = %d, want 0 despite poison row", got)
	}
	if got := poison.countTable(t, "agent_identities"); got != 1 {
		t.Fatalf("poison agent rows = %d, want 1 for a later retry", got)
	}
}
