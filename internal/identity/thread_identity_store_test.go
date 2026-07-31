//go:build integration

package identity_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/migrations"
)

func TestThreadIdentityMigrationsDoNotBackfillExistingMessages(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()

	const schema = "thread_identity_migration_probe"
	if _, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		t.Fatalf("drop stale probe schema: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create probe schema: %v", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SET search_path TO public`)
		_, _ = conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	}()
	if _, err := conn.Exec(ctx, `SET search_path TO `+schema); err != nil {
		t.Fatalf("set probe search path: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		CREATE TABLE messages (
			id text PRIMARY KEY,
			agent_id text NOT NULL,
			direction text NOT NULL,
			email_message_id text NOT NULL DEFAULT '',
			provider_message_id text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL DEFAULT now()
		);
		INSERT INTO messages (id, agent_id, direction, email_message_id)
		VALUES ('msg_legacy_probe', 'agent@example.test', 'inbound', '<legacy@example.test>')
	`); err != nil {
		t.Fatalf("seed legacy message: %v", err)
	}

	for _, name := range []string{
		"085_message_thread_identity.sql",
		"086_messages_agent_thread_created_idx.sql",
		"087_messages_agent_rfc_message_id_idx.sql",
		"088_messages_thread_parent_idx.sql",
		"089_messages_agent_inbound_message_id_idx.sql",
	} {
		sql, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}

	var threadNull, parentNull, keyNull bool
	if err := conn.QueryRow(ctx,
		`SELECT thread_id IS NULL, thread_parent_id IS NULL, rfc_message_id_key IS NULL
		   FROM messages
		  WHERE id = 'msg_legacy_probe'`,
	).Scan(&threadNull, &parentNull, &keyNull); err != nil {
		t.Fatalf("read legacy message after migrations: %v", err)
	}
	if !threadNull || !parentNull || !keyNull {
		t.Fatalf("migration backfilled legacy row: thread null=%v parent null=%v key null=%v", threadNull, parentNull, keyNull)
	}
}

func TestLegacyInboundAnchorLookupUsesPartialIndexWithGenericPlan(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "legacy-inbound-index")
	if _, err := pool.Exec(ctx,
		`INSERT INTO messages (id, agent_id, direction, email_message_id)
		 SELECT 'msg_legacy_plan_' || g,
		        $1,
		        'inbound',
		        '<legacy-plan-' || g || '@example.test>'
		   FROM generate_series(1, 5000) AS g`,
		agentID,
	); err != nil {
		t.Fatalf("seed planner distribution: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE messages`); err != nil {
		t.Fatalf("analyze messages: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SET plan_cache_mode = force_generic_plan`); err != nil {
		t.Fatalf("force generic plan: %v", err)
	}
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("disable sequential scan: %v", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `RESET plan_cache_mode`)
		_, _ = conn.Exec(context.Background(), `RESET enable_seqscan`)
	}()

	const statement = "thread_legacy_inbound_probe"
	if _, err := conn.Exec(ctx, `DEALLOCATE ALL`); err != nil {
		t.Fatalf("deallocate existing prepared statements: %v", err)
	}
	if _, err := conn.Exec(ctx, `PREPARE `+statement+` (text, text[]) AS
		SELECT id, COALESCE(thread_id, '')
		  FROM messages
		 WHERE agent_id = $1
		   AND direction = 'inbound'
		   AND email_message_id <> ''
		   AND email_message_id = ANY($2)
		 ORDER BY created_at, id`); err != nil {
		t.Fatalf("prepare legacy lookup: %v", err)
	}

	rows, err := conn.Query(ctx,
		`EXPLAIN (FORMAT TEXT) EXECUTE `+statement+` ('`+
			strings.ReplaceAll(agentID, `'`, `''`)+`', ARRAY['<legacy-plan-2500@example.test>'])`)
	if err != nil {
		t.Fatalf("explain generic legacy lookup: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	if !strings.Contains(plan.String(), "messages_agent_inbound_message_id_idx") {
		t.Fatalf("generic legacy lookup did not use partial index:\n%s", plan.String())
	}
}

func TestCreateOutboundMessageTxAssignsDistinctFreshThreads(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "fresh-thread")

	var first, second *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		first, err = store.CreateOutboundMessageTx(ctx, tx, agentID,
			[]string{"alice@example.net"}, nil, nil, "Same", "send", "smtp", "",
			"conv_reused", nil, "accepted", agentID, "relay")
		if err != nil {
			return err
		}
		second, err = store.CreateOutboundMessageTx(ctx, tx, agentID,
			[]string{"alice@example.net"}, nil, nil, "Same", "send", "smtp", "",
			"conv_reused", nil, "accepted", agentID, "relay")
		return err
	}); err != nil {
		t.Fatalf("create fresh messages: %v", err)
	}

	if !identity.IsValidThreadID(first.ThreadID) || !identity.IsValidThreadID(second.ThreadID) {
		t.Fatalf("fresh thread IDs = (%q, %q), want valid server-owned IDs", first.ThreadID, second.ThreadID)
	}
	if first.ThreadID == second.ThreadID {
		t.Fatalf("fresh sends sharing conversation_id collapsed into %q", first.ThreadID)
	}
	if first.ThreadParentID != "" || second.ThreadParentID != "" {
		t.Fatalf("fresh sends unexpectedly have parents: (%q, %q)", first.ThreadParentID, second.ThreadParentID)
	}
}

func TestCreateOutboundMessageThreadedTxLazyAdoptsReplyParent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "reply-thread")

	parent, err := store.CreateInboundMessage(ctx, "", agentID, "alice@example.net", agentID,
		"<Parent@MAIL.Example.NET>", "Question", "conv_old", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET thread_id=NULL, rfc_message_id_key=NULL WHERE id=$1`, parent.ID); err != nil {
		t.Fatalf("make legacy parent: %v", err)
	}

	var reply *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		reply, txErr = store.CreateOutboundMessageThreadedTx(ctx, tx, parent.ID, agentID,
			[]string{"alice@example.net"}, nil, nil, "Re: Question", "reply", "smtp", "",
			"conv_changed", nil, "accepted", agentID, "relay")
		return txErr
	}); err != nil {
		t.Fatalf("create reply: %v", err)
	}

	var parentThread, parentKey string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(thread_id,''), COALESCE(rfc_message_id_key,'') FROM messages WHERE id=$1`,
		parent.ID,
	).Scan(&parentThread, &parentKey); err != nil {
		t.Fatalf("load adopted parent: %v", err)
	}
	if !identity.IsValidThreadID(parentThread) {
		t.Fatalf("adopted parent thread_id = %q", parentThread)
	}
	if reply.ThreadID != parentThread || reply.ThreadParentID != parent.ID {
		t.Fatalf("reply topology = thread %q parent %q, want %q / %q", reply.ThreadID, reply.ThreadParentID, parentThread, parent.ID)
	}
	if parentKey != "<Parent@mail.example.net>" {
		t.Fatalf("lazy canonical key = %q, want preserved left and lowercase domain", parentKey)
	}
	if reply.ConversationID != "conv_changed" {
		t.Fatalf("conversation_id = %q, want caller-owned value preserved", reply.ConversationID)
	}
}

func TestCreateOutboundMessageThreadedTxForwardStartsNewThread(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "forward-thread")

	parent, err := store.CreateInboundMessage(ctx, "", agentID, "alice@example.net", agentID,
		"<forward-parent@example.net>", "Document", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	var forward *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		forward, txErr = store.CreateOutboundMessageThreadedTx(ctx, tx, parent.ID, agentID,
			[]string{"bob@example.net"}, nil, nil, "Fwd: Document", "forward", "smtp", "",
			parent.ConversationID, nil, "accepted", agentID, "relay")
		return txErr
	}); err != nil {
		t.Fatalf("create forward: %v", err)
	}

	if !identity.IsValidThreadID(forward.ThreadID) {
		t.Fatalf("forward thread_id = %q", forward.ThreadID)
	}
	if forward.ThreadID == parent.ThreadID {
		t.Fatalf("forward inherited source thread %q", forward.ThreadID)
	}
	if forward.ThreadParentID != "" {
		t.Fatalf("forward thread_parent_id = %q, want empty", forward.ThreadParentID)
	}
}

func TestCreateInboundMessageAuthenticatedThreadedInTxResolvesExactParent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "inbound-reply-thread")

	parent, err := store.CreateOutboundMessage(ctx, agentID,
		[]string{"alice@example.net"}, nil, nil, "Question", "send", "smtp",
		"<CaseSensitive.Root@MAIL.Example.NET>", "conv_parent", nil)
	if err != nil {
		t.Fatalf("create outbound parent: %v", err)
	}

	var reply *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		reply, txErr = store.CreateInboundMessageAuthenticatedThreadedInTx(
			ctx, tx, "", agentID,
			identity.InboundAuth{HeaderFrom: "alice@example.net", EnvelopeFrom: "bounce@example.net"},
			agentID, "<Alice.Reply@GMAIL.COM>", "Re: Question", "conv_parent", "unread",
			nil, false, "", nil, nil, nil, identity.InboundScreening{},
			identity.InboundThreadEvidence{InReplyTo: []identity.RFCMessageIDCandidate{{
				Original:  "<CaseSensitive.Root@MAIL.Example.NET>",
				Canonical: "<CaseSensitive.Root@mail.example.net>",
			}}},
		)
		return txErr
	}); err != nil {
		t.Fatalf("create inbound reply: %v", err)
	}

	if reply.ThreadID != parent.ThreadID || reply.ThreadParentID != parent.ID {
		t.Fatalf("inbound topology = thread %q parent %q, want %q / %q", reply.ThreadID, reply.ThreadParentID, parent.ThreadID, parent.ID)
	}
	if reply.RFCMessageIDKey != "<Alice.Reply@gmail.com>" {
		t.Fatalf("inbound own canonical key = %q", reply.RFCMessageIDKey)
	}
}

func TestInboundDuplicateAnchorConsensusIgnoresNullRows(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "inbound-consensus")

	established, err := store.CreateInboundMessage(ctx, "", agentID, "a@example.net", agentID,
		"<duplicate@example.net>", "One", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("create established anchor: %v", err)
	}
	nullAnchor, err := store.CreateInboundMessage(ctx, "", agentID, "b@example.net", agentID,
		"<other@example.net>", "Two", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("create null anchor: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages
		    SET thread_id=NULL, rfc_message_id_key='<duplicate@example.net>'
		  WHERE id=$1`,
		nullAnchor.ID,
	); err != nil {
		t.Fatalf("prepare null duplicate: %v", err)
	}

	var reply *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		reply, txErr = store.CreateInboundMessageAuthenticatedThreadedInTx(
			ctx, tx, "", agentID, identity.InboundAuth{HeaderFrom: "reply@example.net"},
			agentID, "<reply@example.net>", "Re", "", "unread", nil, false, "",
			nil, nil, nil, identity.InboundScreening{},
			identity.InboundThreadEvidence{InReplyTo: []identity.RFCMessageIDCandidate{{
				Original: "<duplicate@example.net>", Canonical: "<duplicate@example.net>",
			}}},
		)
		return txErr
	}); err != nil {
		t.Fatalf("create consensus reply: %v", err)
	}

	if reply.ThreadID != established.ThreadID {
		t.Fatalf("reply thread_id = %q, want established %q", reply.ThreadID, established.ThreadID)
	}
	if reply.ThreadParentID != "" {
		t.Fatalf("ambiguous direct parent = %q, want empty", reply.ThreadParentID)
	}
	var stillNull bool
	if err := pool.QueryRow(ctx, `SELECT thread_id IS NULL FROM messages WHERE id=$1`, nullAnchor.ID).Scan(&stillNull); err != nil {
		t.Fatalf("load null duplicate: %v", err)
	}
	if !stillNull {
		t.Fatal("null duplicate was implicitly adopted")
	}
}

func TestInboundCanonicalAndLegacyAnchorConflictDoesNotMerge(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "inbound-canonical-legacy-conflict")

	canonicalAnchor, err := store.CreateInboundMessage(ctx, "", agentID, "a@example.net", agentID,
		"<same@example.net>", "Canonical", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("create canonical anchor: %v", err)
	}
	legacyAnchor, err := store.CreateInboundMessage(ctx, "", agentID, "b@example.net", agentID,
		"<legacy-other@example.net>", "Legacy", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("create legacy anchor: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages
		    SET email_message_id='<same@example.net>', rfc_message_id_key=NULL
		  WHERE id=$1`,
		legacyAnchor.ID,
	); err != nil {
		t.Fatalf("prepare legacy conflict: %v", err)
	}

	var reply *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		reply, txErr = store.CreateInboundMessageAuthenticatedThreadedInTx(
			ctx, tx, "", agentID, identity.InboundAuth{HeaderFrom: "reply@example.net"},
			agentID, "<new-reply@example.net>", "Re", "", "unread", nil, false, "",
			nil, nil, nil, identity.InboundScreening{},
			identity.InboundThreadEvidence{InReplyTo: []identity.RFCMessageIDCandidate{{
				Original: "<same@example.net>", Canonical: "<same@example.net>",
			}}},
		)
		return txErr
	}); err != nil {
		t.Fatalf("create conflicting reply: %v", err)
	}

	if reply.ThreadID == canonicalAnchor.ThreadID || reply.ThreadID == legacyAnchor.ThreadID {
		t.Fatalf("conflicting established anchors were merged into %q", reply.ThreadID)
	}
	if reply.ThreadParentID != "" {
		t.Fatalf("conflicting anchors selected parent %q", reply.ThreadParentID)
	}
}

func TestInboundAmbiguousImmediateParentFallsBackToNewestReference(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "inbound-reference-precedence")

	firstAmbiguous, err := store.CreateInboundMessage(ctx, "", agentID, "a@example.net", agentID,
		"<ambiguous@example.net>", "Ambiguous one", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("create first ambiguous anchor: %v", err)
	}
	secondAmbiguous, err := store.CreateInboundMessage(ctx, "", agentID, "b@example.net", agentID,
		"<other@example.net>", "Ambiguous two", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("create second ambiguous anchor: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages
		    SET email_message_id = '<ambiguous@example.net>',
		        rfc_message_id_key = '<ambiguous@example.net>'
		  WHERE id = $1`,
		secondAmbiguous.ID,
	); err != nil {
		t.Fatalf("prepare conflicting immediate parent: %v", err)
	}
	if firstAmbiguous.ThreadID == secondAmbiguous.ThreadID {
		t.Fatal("ambiguous fixtures unexpectedly share a thread")
	}

	olderReference, err := store.CreateInboundMessage(ctx, "", agentID, "c@example.net", agentID,
		"<older-reference@example.net>", "Older reference", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("create older reference: %v", err)
	}
	newerReference, err := store.CreateInboundMessage(ctx, "", agentID, "d@example.net", agentID,
		"<newer-reference@example.net>", "Newer reference", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("create newer reference: %v", err)
	}

	var reply *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		reply, txErr = store.CreateInboundMessageAuthenticatedThreadedInTx(
			ctx, tx, "", agentID, identity.InboundAuth{HeaderFrom: "reply@example.net"},
			agentID, "<reference-reply@example.net>", "Re", "", "unread", nil, false, "",
			nil, nil, nil, identity.InboundScreening{},
			identity.InboundThreadEvidence{
				InReplyTo: []identity.RFCMessageIDCandidate{{
					Original: "<ambiguous@example.net>", Canonical: "<ambiguous@example.net>",
				}},
				References: []identity.RFCMessageIDCandidate{
					{Original: "<older-reference@example.net>", Canonical: "<older-reference@example.net>"},
					{Original: "<newer-reference@example.net>", Canonical: "<newer-reference@example.net>"},
				},
			},
		)
		return txErr
	}); err != nil {
		t.Fatalf("create inbound reply: %v", err)
	}
	if reply.ThreadID != newerReference.ThreadID || reply.ThreadParentID != newerReference.ID {
		t.Fatalf("reference fallback = thread %q parent %q, want newest reference %q / %q",
			reply.ThreadID, reply.ThreadParentID, newerReference.ThreadID, newerReference.ID)
	}
	if reply.ThreadID == olderReference.ThreadID {
		t.Fatal("References precedence ran left-to-right instead of right-to-left")
	}
}

func TestInboundAllNullDuplicateAnchorsRemainUntouchedAndStartFresh(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "inbound-all-null-duplicates")

	var anchorIDs []string
	for i, sender := range []string{"a@example.net", "b@example.net"} {
		anchor, err := store.CreateInboundMessage(ctx, "", agentID, sender, agentID,
			"<all-null-"+string(rune('a'+i))+"@example.net>", "Duplicate", "", "unread", nil, nil, nil,
			false, "", nil, nil, nil, identity.InboundScreening{})
		if err != nil {
			t.Fatalf("create duplicate anchor %d: %v", i, err)
		}
		anchorIDs = append(anchorIDs, anchor.ID)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages
		    SET thread_id = NULL,
		        email_message_id = '<all-null@example.net>',
		        rfc_message_id_key = '<all-null@example.net>'
		  WHERE id = ANY($1)`,
		anchorIDs,
	); err != nil {
		t.Fatalf("prepare all-null duplicates: %v", err)
	}

	var reply *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		reply, txErr = store.CreateInboundMessageAuthenticatedThreadedInTx(
			ctx, tx, "", agentID, identity.InboundAuth{HeaderFrom: "reply@example.net"},
			agentID, "<all-null-reply@example.net>", "Re", "", "unread", nil, false, "",
			nil, nil, nil, identity.InboundScreening{},
			identity.InboundThreadEvidence{InReplyTo: []identity.RFCMessageIDCandidate{{
				Original: "<all-null@example.net>", Canonical: "<all-null@example.net>",
			}}},
		)
		return txErr
	}); err != nil {
		t.Fatalf("create inbound reply: %v", err)
	}
	if !identity.IsValidThreadID(reply.ThreadID) || reply.ThreadParentID != "" {
		t.Fatalf("ambiguous all-null reply = thread %q parent %q", reply.ThreadID, reply.ThreadParentID)
	}
	var adopted int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE id = ANY($1) AND thread_id IS NOT NULL`,
		anchorIDs,
	).Scan(&adopted); err != nil {
		t.Fatalf("count adopted duplicates: %v", err)
	}
	if adopted != 0 {
		t.Fatalf("%d ambiguous all-null anchors were adopted", adopted)
	}
}

func TestInboundResolutionStaysMailboxLocalAndIncludesSoftDeletedAnchors(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	firstAgent := convoTestSetup(t, store, "thread-mailbox-one")
	secondAgent := convoTestSetup(t, store, "thread-mailbox-two")

	parent, err := store.CreateInboundMessage(ctx, "", firstAgent, "sender@example.net", firstAgent,
		"<mailbox-local@example.net>", "Root", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("create first mailbox parent: %v", err)
	}
	if err := store.SoftDeleteMessage(ctx, parent.ID, firstAgent); err != nil {
		t.Fatalf("soft delete parent: %v", err)
	}

	createReply := func(agentID, ownID string) (*identity.Message, error) {
		var reply *identity.Message
		err := store.WithTx(ctx, func(tx pgx.Tx) error {
			var txErr error
			reply, txErr = store.CreateInboundMessageAuthenticatedThreadedInTx(
				ctx, tx, "", agentID, identity.InboundAuth{HeaderFrom: "reply@example.net"},
				agentID, ownID, "Re", "", "unread", nil, false, "",
				nil, nil, nil, identity.InboundScreening{},
				identity.InboundThreadEvidence{InReplyTo: []identity.RFCMessageIDCandidate{{
					Original: "<mailbox-local@example.net>", Canonical: "<mailbox-local@example.net>",
				}}},
			)
			return txErr
		})
		return reply, err
	}

	sameMailbox, err := createReply(firstAgent, "<same-mailbox@example.net>")
	if err != nil {
		t.Fatalf("create same-mailbox reply: %v", err)
	}
	if sameMailbox.ThreadID != parent.ThreadID || sameMailbox.ThreadParentID != parent.ID {
		t.Fatalf("soft-deleted anchor was excluded: thread %q parent %q", sameMailbox.ThreadID, sameMailbox.ThreadParentID)
	}

	otherMailbox, err := createReply(secondAgent, "<other-mailbox@example.net>")
	if err != nil {
		t.Fatalf("create cross-mailbox reply: %v", err)
	}
	if otherMailbox.ThreadID == parent.ThreadID || otherMailbox.ThreadParentID != "" {
		t.Fatalf("cross-mailbox resolution leaked topology: thread %q parent %q", otherMailbox.ThreadID, otherMailbox.ThreadParentID)
	}
}

func TestConcurrentAPIAndInboundLazyAdoptionConverge(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "concurrent-thread-adoption")

	parent, err := store.CreateInboundMessage(ctx, "", agentID, "sender@example.net", agentID,
		"<concurrent-parent@example.net>", "Root", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET thread_id = NULL, rfc_message_id_key = NULL WHERE id = $1`,
		parent.ID,
	); err != nil {
		t.Fatalf("prepare legacy parent: %v", err)
	}

	start := make(chan struct{})
	results := make(chan *identity.Message, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		var reply *identity.Message
		err := store.WithTx(ctx, func(tx pgx.Tx) error {
			var txErr error
			reply, txErr = store.CreateOutboundMessageThreadedTx(
				ctx, tx, parent.ID, agentID, []string{"sender@example.net"}, nil, nil,
				"Re: Root", "reply", "smtp", "", "api-conversation", nil,
				"accepted", agentID, "relay",
			)
			return txErr
		})
		results <- reply
		errs <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		var reply *identity.Message
		err := store.WithTx(ctx, func(tx pgx.Tx) error {
			var txErr error
			reply, txErr = store.CreateInboundMessageAuthenticatedThreadedInTx(
				ctx, tx, "", agentID, identity.InboundAuth{HeaderFrom: "sender@example.net"},
				agentID, "<concurrent-reply@example.net>", "Re: Root", "smtp-conversation",
				"unread", nil, false, "", nil, nil, nil, identity.InboundScreening{},
				identity.InboundThreadEvidence{InReplyTo: []identity.RFCMessageIDCandidate{{
					Original: "<concurrent-parent@example.net>", Canonical: "<concurrent-parent@example.net>",
				}}},
			)
			return txErr
		})
		results <- reply
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent adoption: %v", err)
		}
	}

	var threadID string
	for reply := range results {
		if reply == nil {
			t.Fatal("concurrent adoption returned nil reply")
		}
		if threadID == "" {
			threadID = reply.ThreadID
		} else if reply.ThreadID != threadID {
			t.Fatalf("concurrent replies diverged: %q versus %q", threadID, reply.ThreadID)
		}
		if reply.ThreadParentID != parent.ID {
			t.Fatalf("concurrent reply parent = %q, want %q", reply.ThreadParentID, parent.ID)
		}
	}
	var storedThread string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(thread_id, '') FROM messages WHERE id = $1`,
		parent.ID,
	).Scan(&storedThread); err != nil {
		t.Fatalf("load adopted parent: %v", err)
	}
	if storedThread != threadID {
		t.Fatalf("parent adopted thread %q, replies use %q", storedThread, threadID)
	}
}

func TestAuthenticatedPlatformDeliveryTwinCopiesSourceThread(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "authenticated-platform-twin")
	agentEmail := "bot@authenticated-platform-twin.example.com"

	var source *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		source, txErr = store.CreateOutboundMessageTx(ctx, tx, agentID,
			[]string{agentEmail}, nil, nil, "Test email from e2a", "test", "smtp",
			"<platform-test@US-EAST-2.AMAZONSES.COM>", "conv_test", nil, "sent",
			"noreply@send.e2a.dev", "relay")
		return txErr
	}); err != nil {
		t.Fatalf("create platform source: %v", err)
	}
	spfDomain := "mail.send.e2a.dev"
	auth := identity.InboundAuth{
		HeaderFrom:   "noreply@send.e2a.dev",
		EnvelopeFrom: "bounce@mail.send.e2a.dev",
		Authentication: &emailauth.Authentication{SPF: emailauth.SPFResult{
			Status: emailauth.StatusPass,
			Domain: &spfDomain,
		}},
	}

	var twin *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		twin, txErr = store.CreateInboundMessageAuthenticatedThreadedInTx(
			ctx, tx, "", agentID, auth, agentEmail,
			"<platform-test@us-east-2.amazonses.com>", "Test email from e2a",
			"conv_test", "unread", nil, false, "", nil, nil, nil,
			identity.InboundScreening{},
			identity.InboundThreadEvidence{DeliveryTwinSourceID: source.ID},
		)
		return txErr
	}); err != nil {
		t.Fatalf("create authenticated twin: %v", err)
	}

	if twin.ThreadID != source.ThreadID {
		t.Fatalf("twin thread_id = %q, want source %q", twin.ThreadID, source.ThreadID)
	}
	if twin.ThreadParentID != "" {
		t.Fatalf("physical delivery twin has reply parent %q", twin.ThreadParentID)
	}

	var wrongRecipient *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		wrongRecipient, txErr = store.CreateInboundMessageAuthenticatedThreadedInTx(
			ctx, tx, "", agentID, auth, "different@example.com",
			"<platform-test@us-east-2.amazonses.com>", "Test email from e2a",
			"conv_test", "unread", nil, false, "", nil, nil, nil,
			identity.InboundScreening{},
			identity.InboundThreadEvidence{DeliveryTwinSourceID: source.ID},
		)
		return txErr
	}); err != nil {
		t.Fatalf("wrong-recipient twin evidence should fall back: %v", err)
	}
	if wrongRecipient.ThreadID == source.ThreadID {
		t.Fatal("authenticated twin correlation ignored the source recipient")
	}
}

func TestUnauthenticatedOrStaleDeliveryTwinEvidenceFallsBack(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "unauthenticated-platform-twin")
	agentEmail := "bot@unauthenticated-platform-twin.example.com"

	source, err := store.CreateOutboundMessage(ctx, agentID,
		[]string{agentEmail}, nil, nil, "Ordinary", "send", "smtp",
		"<ordinary@us-east-2.amazonses.com>", "conv", nil)
	if err != nil {
		t.Fatalf("create ordinary source: %v", err)
	}

	for _, sourceID := range []string{source.ID, "msg_missing"} {
		var inbound *identity.Message
		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			var txErr error
			inbound, txErr = store.CreateInboundMessageAuthenticatedThreadedInTx(
				ctx, tx, "", agentID,
				identity.InboundAuth{HeaderFrom: "attacker@example.net", EnvelopeFrom: "attacker@example.net"},
				agentEmail, "<spoof@example.net>", "Spoof", "", "unread", nil, false, "",
				nil, nil, nil, identity.InboundScreening{},
				identity.InboundThreadEvidence{DeliveryTwinSourceID: sourceID},
			)
			return txErr
		}); err != nil {
			t.Fatalf("source %q should fall back, got error: %v", sourceID, err)
		}
		if inbound.ThreadID == source.ThreadID {
			t.Fatalf("source %q copied thread without authenticated test-twin proof", sourceID)
		}
	}
}

func TestPendingOutboundThreadAssignment(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "pending-thread")
	parent, err := store.CreateInboundMessage(ctx, "", agentID, "alice@example.net", agentID,
		"<pending-parent@example.net>", "Question", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	var reply, forward *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		reply, txErr = store.CreatePendingOutboundMessageManagedThreadedTx(
			ctx, tx, parent.ID, agentID, []string{"alice@example.net"}, nil, nil,
			"Re: Question", "reply", "", nil, "reply", "conv_changed",
			"<pending-parent@example.net>", "", 60, false,
		)
		if txErr != nil {
			return txErr
		}
		forward, txErr = store.CreatePendingOutboundMessageManagedThreadedTx(
			ctx, tx, parent.ID, agentID, []string{"bob@example.net"}, nil, nil,
			"Fwd: Question", "forward", "", nil, "forward", "conv_changed",
			"<pending-parent@example.net>", "", 60, false,
		)
		return txErr
	}); err != nil {
		t.Fatalf("create pending messages: %v", err)
	}

	if reply.ThreadID != parent.ThreadID || reply.ThreadParentID != parent.ID {
		t.Fatalf("pending reply topology = %q / %q, want %q / %q", reply.ThreadID, reply.ThreadParentID, parent.ThreadID, parent.ID)
	}
	if !identity.IsValidThreadID(forward.ThreadID) || forward.ThreadID == parent.ThreadID || forward.ThreadParentID != "" {
		t.Fatalf("pending forward topology = %q / %q, source %q", forward.ThreadID, forward.ThreadParentID, parent.ThreadID)
	}
}

func TestPurgeMessageClearsSurvivingThreadParent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "purge-thread-parent")
	parent, err := store.CreateInboundMessage(ctx, "", agentID, "a@example.net", agentID,
		"<purge-parent@example.net>", "Parent", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatal(err)
	}
	var child *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		child, txErr = store.CreateOutboundMessageThreadedTx(ctx, tx, parent.ID, agentID,
			[]string{"a@example.net"}, nil, nil, "Re", "reply", "smtp", "", "",
			nil, "sent", agentID, "relay")
		return txErr
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDeleteMessage(ctx, parent.ID, agentID); err != nil {
		t.Fatal(err)
	}
	if err := store.PurgeMessage(ctx, parent.ID, agentID); err != nil {
		t.Fatal(err)
	}

	var parentID, threadID string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(thread_parent_id,''), thread_id FROM messages WHERE id=$1`,
		child.ID,
	).Scan(&parentID, &threadID); err != nil {
		t.Fatal(err)
	}
	if parentID != "" || threadID != child.ThreadID {
		t.Fatalf("surviving child topology = parent %q thread %q, want empty / %q", parentID, threadID, child.ThreadID)
	}
}

func TestDeleteExpiredMessagesClearsSurvivingThreadParent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "retention-thread-parent")
	parent, err := store.CreateInboundMessage(ctx, "", agentID, "a@example.net", agentID,
		"<retention-parent@example.net>", "Parent", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatal(err)
	}
	var child *identity.Message
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		var txErr error
		child, txErr = store.CreateOutboundMessageThreadedTx(ctx, tx, parent.ID, agentID,
			[]string{"a@example.net"}, nil, nil, "Re", "reply", "smtp", "", "",
			nil, "sent", agentID, "relay")
		return txErr
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET deleted_at=now()-interval '31 days' WHERE id=$1`,
		parent.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteExpiredMessages(ctx); err != nil {
		t.Fatal(err)
	}

	var parentID, threadID string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(thread_parent_id,''), thread_id FROM messages WHERE id=$1`,
		child.ID,
	).Scan(&parentID, &threadID); err != nil {
		t.Fatal(err)
	}
	if parentID != "" || threadID != child.ThreadID {
		t.Fatalf("surviving child topology = parent %q thread %q, want empty / %q", parentID, threadID, child.ThreadID)
	}
}
