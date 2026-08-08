package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// seedRawWithAttachment is a raw RFC 5322 multipart message carrying ONE
// base64 attachment whose DECODED payload is "hello world" (11 bytes). The
// export must surface it as typed AttachmentMetaView with size_bytes = 11 (the
// decoded size — not the 16-byte base64 wire size inside the MIME).
const seedRawWithAttachment = "From: alice@gmail.com\r\n" +
	"Subject: Hi\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"b\"\r\n" +
	"\r\n" +
	"--b\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"body\r\n" +
	"--b\r\n" +
	"Content-Type: application/pdf; name=\"report.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"aGVsbG8gd29ybGQ=\r\n" + // "hello world"
	"--b--\r\n"

// seedUserData creates one user with: 1 verified domain, 2 agents (one
// custom-domain, one shared-domain), 1 inbound (with one MIME attachment) +
// 1 outbound message + 1 held pending_review draft (with one staged
// attachment), 2 API keys, 1 user_session, and 2 usage_events. Returns the
// user so the caller can drive Export/Delete against a known fixture.
func seedUserData(t *testing.T, store *identity.Store, ctx context.Context, label string) *identity.User {
	t.Helper()

	user, err := store.CreateOrGetUser(ctx, label+"@example.com", label, "google-"+label)
	if err != nil {
		t.Fatalf("seed: CreateOrGetUser: %v", err)
	}

	if _, err := store.ClaimOrCreateDomain(ctx, label+".example.com", user.ID); err != nil {
		t.Fatalf("seed: ClaimOrCreateDomain: %v", err)
	}
	if err := store.VerifyDomain(ctx, label+".example.com", user.ID); err != nil {
		t.Fatalf("seed: VerifyDomain: %v", err)
	}

	customAgent, err := store.CreateAgent(ctx,
		"bot@"+label+".example.com", label+".example.com", "Bot",
		"https://"+label+".example.com/hook", "cloud", user.ID)
	if err != nil {
		t.Fatalf("seed: CreateAgent custom: %v", err)
	}

	// Inbound message on the custom-domain agent. Populate reply_to so
	// the export covers the GDPR-Art.15 right-of-access claim that all
	// stored data about the user is returned.
	domain := "gmail.com"
	aligned := true
	policy := emailauth.DMARCPolicyReject
	authentication := &emailauth.Authentication{
		SPF:   emailauth.SPFResult{Status: emailauth.StatusPass, Domain: &domain, Aligned: &aligned},
		DKIM:  []emailauth.DKIMResult{},
		DMARC: emailauth.DMARCResult{Status: emailauth.StatusPass, Domain: &domain, Policy: &policy, AlignedBy: []emailauth.AlignmentMechanism{emailauth.AlignedBySPF}},
	}
	if _, err := store.CreateInboundMessageAuthenticated(ctx,
		"msg_in_"+label, customAgent.ID,
		identity.InboundAuth{HeaderFrom: "alice@gmail.com", EnvelopeFrom: "alice@gmail.com", Authentication: authentication}, customAgent.EmailAddress(),
		"<orig@gmail.com>", "Hi there", "", "delivered",
		[]byte(seedRawWithAttachment),
		false, "",
		nil, nil, []string{"real-alice@example.com"},
		identity.InboundScreening{}); err != nil {
		t.Fatalf("seed: CreateInboundMessage: %v", err)
	}

	// Outbound message on the same agent.
	if _, err := store.CreateOutboundMessage(ctx, customAgent.ID,
		[]string{"alice@gmail.com"}, nil, nil,
		"Re: Hi", "reply", "smtp", "<sent@e2a.example>", "", nil); err != nil {
		t.Fatalf("seed: CreateOutboundMessage: %v", err)
	}

	// Held pending_review draft with one staged attachment ("hello", 5 bytes
	// decoded). Its internal storage blob carries inline base64 `data`; the
	// export must surface typed AttachmentMetaView and never that blob.
	draftAtts := []byte(`[{"filename":"notes.txt","content_type":"text/plain","data":"aGVsbG8="}]`)
	if _, err := store.CreatePendingOutboundMessage(ctx, customAgent.ID,
		[]string{"alice@gmail.com"}, nil, nil,
		"Draft subject", "draft body", "", draftAtts,
		"send", "", "", "", 3600); err != nil {
		t.Fatalf("seed: CreatePendingOutboundMessage: %v", err)
	}

	if _, err := store.CreateAPIKey(ctx, user.ID, "primary", nil); err != nil {
		t.Fatalf("seed: CreateAPIKey 1: %v", err)
	}
	if _, err := store.CreateAPIKey(ctx, user.ID, "ci", nil); err != nil {
		t.Fatalf("seed: CreateAPIKey 2: %v", err)
	}

	if _, err := store.CreateUserSession(ctx, user.ID); err != nil {
		t.Fatalf("seed: CreateUserSession: %v", err)
	}

	// A suppression + a protection-event audit row, so the export covers both.
	if _, err := store.AddSuppression(ctx, user.ID, "blocked@spam.com", "hard bounce", "bounce", ""); err != nil {
		t.Fatalf("seed: AddSuppression: %v", err)
	}
	if err := store.CreateProtectionEvent(ctx, identity.ProtectionEvent{
		MessageID: "msg_in_" + label, AgentID: customAgent.ID, Direction: "inbound",
		Source: identity.ScreeningSourceGate, Reason: identity.ReviewReasonSenderGate,
		Action: "flag", SubjectAddr: "alice@gmail.com",
	}); err != nil {
		t.Fatalf("seed: CreateProtectionEvent: %v", err)
	}

	return user
}

// TestExportUserData verifies the right-of-access flow. The export
// should contain the user's profile, every domain/agent/key/message
// they own, and exclude internal identifiers (google_subject, key
// hashes, session tokens).
func TestExportUserData(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user := seedUserData(t, store, ctx, "exporter")
	if _, err := pool.Exec(ctx,
		`UPDATE agent_identities SET suppress_notifications = true WHERE user_id = $1`, user.ID); err != nil {
		t.Fatalf("enable notification suppression: %v", err)
	}

	dump, err := store.ExportUserData(ctx, user.ID)
	if err != nil {
		t.Fatalf("ExportUserData: %v", err)
	}

	if dump.User.ID != user.ID || dump.User.Email != user.Email {
		t.Errorf("user mismatch: got id=%s email=%s, want %s/%s",
			dump.User.ID, dump.User.Email, user.ID, user.Email)
	}
	if len(dump.Domains) != 1 {
		t.Errorf("domains: got %d, want 1", len(dump.Domains))
	}
	if len(dump.Agents) != 1 {
		t.Errorf("agents: got %d, want 1", len(dump.Agents))
	}
	if len(dump.Agents) == 1 && !dump.Agents[0].SuppressNotifications {
		t.Error("exported agent lost suppress_notifications=true")
	}
	// The export keys an agent on `email` only. The #436 rename dropped the
	// redundant `id` (id == email) from every other surface; guard that the
	// export doesn't reintroduce it as a re-exposed identifier.
	if len(dump.Agents) == 1 {
		agentJSON, err := json.Marshal(dump.Agents[0])
		if err != nil {
			t.Fatalf("marshal exported agent: %v", err)
		}
		if jsonContains(agentJSON, "id") {
			t.Error("exported agent leaks `id` — it's redundant with `email` (id == email)")
		}
		if !jsonContains(agentJSON, "email") {
			t.Error("exported agent is missing `email` — the agent's identifier")
		}
		// webhook_status must be computed (not the zero value) in the
		// export: the fixture has no webhook subscriber, so the agent
		// reports `none` — the state the old webhook_healthy bool could
		// not express. The dropped bool must not resurface on the wire.
		if got := dump.Agents[0].WebhookStatus; got != identity.WebhookStatusNone {
			t.Errorf("exported agent WebhookStatus = %q, want %q (no webhook subscriber seeded)",
				got, identity.WebhookStatusNone)
		}
		if jsonContains(agentJSON, "webhook_healthy") {
			t.Error("exported agent leaks `webhook_healthy` — replaced by the webhook_status enum pre-GA")
		}
	}
	if len(dump.APIKeys) != 2 {
		t.Errorf("api_keys: got %d, want 2", len(dump.APIKeys))
	}
	if len(dump.Messages) != 3 {
		t.Errorf("messages: got %d, want 3 (1 inbound + 1 outbound + 1 held draft)", len(dump.Messages))
	}
	for _, message := range dump.Messages {
		if message.Direction == "outbound" && message.HeaderFrom != "bot@exporter.example.com" {
			t.Errorf("outbound export header_from = %q, want agent sender", message.HeaderFrom)
		}
	}
	if len(dump.Suppressions) != 1 || dump.Suppressions[0].Address != "blocked@spam.com" {
		t.Errorf("suppressions: got %+v, want 1 (blocked@spam.com)", dump.Suppressions)
	}
	if len(dump.ProtectionEvents) != 1 || dump.ProtectionEvents[0].SubjectAddr != "alice@gmail.com" {
		t.Errorf("protection_events: got %+v, want 1 (subject alice@gmail.com)", dump.ProtectionEvents)
	}

	// Right-of-access requires every stored header field to round-trip
	// through the export. Reply-To regression-guard: if a future SELECT
	// drops the column, the user's export silently loses data — exactly
	// the kind of gap that fails a data-rights audit.
	var inbound *identity.Message
	for i := range dump.Messages {
		if dump.Messages[i].Direction == "inbound" {
			inbound = &dump.Messages[i]
			break
		}
	}
	if inbound == nil {
		t.Fatal("no inbound message in export")
	}
	if inbound.ExpiresAt != nil {
		t.Errorf("inbound ExpiresAt in export = %v, want nil (retained indefinitely)", inbound.ExpiresAt)
	}
	wantReplyTo := []string{"real-alice@example.com"}
	if !reflect.DeepEqual(inbound.ReplyTo, wantReplyTo) {
		t.Errorf("inbound ReplyTo in export = %v, want %v", inbound.ReplyTo, wantReplyTo)
	}

	// size_bytes on an exported message is the RAW MIME length of the whole
	// stored message (the storage-accounting basis), not an attachment size.
	if inbound.SizeBytes != len(seedRawWithAttachment) {
		t.Errorf("inbound SizeBytes = %d, want %d (raw MIME length)",
			inbound.SizeBytes, len(seedRawWithAttachment))
	}

	// Typed attachments (one shape everywhere): the inbound message's
	// attachments come parsed from raw_message as AttachmentMetaView, with
	// size_bytes = the DECODED payload (11, "hello world") — not the 16-byte
	// base64 wire size.
	if len(inbound.Attachments) != 1 {
		t.Fatalf("inbound attachments in export = %d, want 1", len(inbound.Attachments))
	}
	if a := inbound.Attachments[0]; a.Filename != "report.pdf" ||
		a.ContentType != "application/pdf" || a.SizeBytes != 11 || a.Index != 0 {
		t.Errorf("inbound attachment meta = %+v, want {report.pdf application/pdf 11 0}", a)
	}

	// The held draft maps its internal attachments_json blob to the same
	// AttachmentMetaView shape (size_bytes = decoded base64 length, 5 for
	// "hello") — the inline base64 bytes must NOT be exported.
	var draft *identity.Message
	for i := range dump.Messages {
		if dump.Messages[i].Status == identity.MessageStatusPendingReview {
			draft = &dump.Messages[i]
			break
		}
	}
	if draft == nil {
		t.Fatal("no pending_review draft in export")
	}
	if len(draft.Attachments) != 1 {
		t.Fatalf("draft attachments in export = %d, want 1", len(draft.Attachments))
	}
	if a := draft.Attachments[0]; a.Filename != "notes.txt" ||
		a.ContentType != "text/plain" || a.SizeBytes != 5 || a.Index != 0 {
		t.Errorf("draft attachment meta = %+v, want {notes.txt text/plain 5 0}", a)
	}

	// Confirm the export doesn't leak internal identifiers. We marshal
	// to JSON because the most likely accidental leak path is a struct
	// field with a `json:` tag we forgot.
	raw, err := json.Marshal(dump)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	if jsonContains(raw, "google_subject") {
		t.Error("export leaks google_subject — that's an internal OAuth identifier")
	}
	if jsonContains(raw, "key_hash") {
		t.Error("export leaks key_hash — that's a credential equivalent")
	}
	if !bytes.Contains(raw, []byte(`"expires_at":null`)) {
		t.Errorf("export must encode indefinite message retention as expires_at: null: %s", raw)
	}
	// The held draft's internal attachments_json blob carries inline base64
	// under a `data` key; the export types attachments as AttachmentMetaView and
	// must not emit that internal storage shape.
	if jsonContains(raw, "data") {
		t.Error("export leaks `data` — the held-draft attachment blob's inline base64 must not be exported")
	}
	// Schema metadata should be present so consumers can detect format
	// changes across versions.
	if dump.SchemaVersion == "" {
		t.Error("export is missing schema_version — consumers can't detect format changes")
	}
	if dump.GeneratedAt.IsZero() {
		t.Error("export is missing generated_at")
	}
}

func TestUserExportAgentSuppressionScopedAndDoesNotLeakTokens(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, agent := suppressionUserAndAgent(t, store, "export-agent-suppression")
	other, otherAgent := suppressionUserAndAgent(t, store, "export-agent-suppression-other")

	if _, err := store.AddSuppression(ctx, user.ID, "account@example.com", "bounce", "manual", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddAgentSuppression(ctx, user.ID, agent.ID, "agent@example.com", "asked", "unsubscribe", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddAgentSuppression(ctx, other.ID, otherAgent.ID, "other@example.com", "", "manual", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.PutUnsubscribeToken(ctx, []byte("secret-token-hash-not-for-export"), user.ID, agent.ID, "token-only@example.com"); err != nil {
		t.Fatal(err)
	}

	dump, err := store.ExportUserData(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dump.SchemaVersion != "4" {
		t.Fatalf("export schema_version = %q, want 4 for canonical message authentication fields", dump.SchemaVersion)
	}
	if len(dump.Suppressions) != 2 {
		t.Fatalf("export suppressions = %+v, want account + agent rows", dump.Suppressions)
	}
	byAddress := make(map[string]identity.SuppressionExportEntry, len(dump.Suppressions))
	for _, row := range dump.Suppressions {
		byAddress[row.Address] = row
	}
	if got := byAddress["account@example.com"].AgentEmail; got != "" {
		t.Errorf("account suppression agent_email = %q, want omitted", got)
	}
	if got := byAddress["agent@example.com"].AgentEmail; got != agent.ID {
		t.Errorf("agent suppression agent_email = %q, want %q", got, agent.ID)
	}
	raw, err := json.Marshal(dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret-token-hash-not-for-export", "token-only@example.com", "other@example.com"} {
		if jsonContains(raw, forbidden) {
			t.Errorf("export leaked %q: %s", forbidden, raw)
		}
	}
}

// TestDeleteUserData verifies the right-of-deletion flow does the full
// cascade and reports per-table counts. After deletion, the user record
// must be gone and orphan rows must not exist for any of: domains,
// agents, messages, api_keys, sessions, usage events/summaries.
func TestDeleteUserData(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user := seedUserData(t, store, ctx, "deleter")
	agentID := "bot@deleter.example.com"
	if _, added, err := store.AddAgentSuppression(ctx, user.ID, agentID, "opted-out@example.net", "", "manual", nil); err != nil || !added {
		t.Fatalf("seed agent suppression: added=%v err=%v", added, err)
	}
	if err := store.PutUnsubscribeToken(ctx, []byte("delete-audit-token-hash"), user.ID, agentID, "token@example.net"); err != nil {
		t.Fatalf("seed unsubscribe token: %v", err)
	}

	res, err := store.DeleteUserData(ctx, user.ID)
	if err != nil {
		t.Fatalf("DeleteUserData: %v", err)
	}

	if !res.UserDeleted {
		t.Error("UserDeleted should be true")
	}
	if res.DomainsDeleted != 1 {
		t.Errorf("DomainsDeleted = %d, want 1", res.DomainsDeleted)
	}
	if res.AgentsDeleted != 1 {
		t.Errorf("AgentsDeleted = %d, want 1", res.AgentsDeleted)
	}
	if res.MessagesDeleted != 3 {
		t.Errorf("MessagesDeleted = %d, want 3", res.MessagesDeleted)
	}
	if res.APIKeysDeleted != 2 {
		t.Errorf("APIKeysDeleted = %d, want 2", res.APIKeysDeleted)
	}
	if res.SessionsDeleted != 1 {
		t.Errorf("SessionsDeleted = %d, want 1", res.SessionsDeleted)
	}
	if res.AgentSuppressionsDeleted != 1 {
		t.Errorf("AgentSuppressionsDeleted = %d, want 1", res.AgentSuppressionsDeleted)
	}
	if res.AgentUnsubscribeTokensDeleted != 1 {
		t.Errorf("AgentUnsubscribeTokensDeleted = %d, want 1", res.AgentUnsubscribeTokensDeleted)
	}

	// Verify the user row itself is gone.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, user.ID,
	).Scan(&exists); err != nil {
		t.Fatalf("post-delete user exists: %v", err)
	}
	if exists {
		t.Error("user row still exists after DeleteUserData")
	}

	// Verify no orphan rows in any user-scoped table. The cascade is in
	// the schema (ON DELETE CASCADE), but a regression that drops a
	// cascade clause would leak rows — checking explicitly catches it.
	checks := []struct {
		name  string
		query string
	}{
		{"domains", `SELECT count(*) FROM domains WHERE user_id = $1`},
		{"agents", `SELECT count(*) FROM agent_identities WHERE user_id = $1`},
		{"api_keys", `SELECT count(*) FROM api_keys WHERE user_id = $1`},
		{"sessions", `SELECT count(*) FROM user_sessions WHERE user_id = $1`},
		{"usage_events", `SELECT count(*) FROM usage_events WHERE user_id = $1`},
		{"usage_summaries", `SELECT count(*) FROM usage_summaries WHERE user_id = $1`},
		{"agent_suppressions", `SELECT count(*) FROM agent_suppressions WHERE user_id = $1`},
		{"agent_unsubscribe_tokens", `SELECT count(*) FROM agent_unsubscribe_tokens WHERE user_id = $1`},
	}
	for _, c := range checks {
		var n int
		if err := pool.QueryRow(ctx, c.query, user.ID).Scan(&n); err != nil {
			t.Fatalf("post-delete count(%s): %v", c.name, err)
		}
		if n != 0 {
			t.Errorf("orphan rows in %s: %d (cascade missed)", c.name, n)
		}
	}
}

func TestDeleteUserDataCancelsLinkedSendJobs(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	canceller := &recordingOutboundJobCanceller{}
	store.SetOutboundJobCanceller(canceller)
	ctx := context.Background()
	user := seedUserData(t, store, ctx, "delete-user-jobs")

	var messageID string
	if err := pool.QueryRow(ctx,
		`SELECT m.id
		   FROM messages m
		   JOIN agent_identities a ON a.id = m.agent_id
		  WHERE a.user_id = $1
		    AND m.direction = 'outbound'
		    AND m.status <> 'pending_review'
		  ORDER BY m.id
		  LIMIT 1`,
		user.ID).Scan(&messageID); err != nil {
		t.Fatalf("find message: %v", err)
	}
	linkTrashTestSendJob(t, pool, messageID, 401)

	if _, err := store.DeleteUserData(ctx, user.ID); err != nil {
		t.Fatalf("DeleteUserData: %v", err)
	}
	if got := canceller.jobIDs; len(got) != 1 || got[0] != 401 {
		t.Fatalf("cancelled jobs = %v, want [401]", got)
	}
}

func TestDeleteUserDataBlocksFreshSendClaim(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	canceller := &recordingOutboundJobCanceller{}
	store.SetOutboundJobCanceller(canceller)
	ctx := context.Background()
	user := seedUserData(t, store, ctx, "delete-user-sending")

	if _, err := pool.Exec(ctx,
		`UPDATE messages m
		    SET delivery_status = 'sending',
		        send_claimed_at = now(),
		        send_job_id = 402
		   FROM agent_identities a
		  WHERE a.id = m.agent_id
		    AND a.user_id = $1
		    AND m.direction = 'outbound'`,
		user.ID); err != nil {
		t.Fatalf("mark sending: %v", err)
	}

	if _, err := store.DeleteUserData(ctx, user.ID); !errors.Is(err, identity.ErrSendInProgress) {
		t.Fatalf("DeleteUserData = %v, want ErrSendInProgress", err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, user.ID).Scan(&exists); err != nil {
		t.Fatalf("check user: %v", err)
	}
	if !exists {
		t.Fatal("user was deleted while an outbound provider call had a fresh lease")
	}
	if len(canceller.jobIDs) != 0 {
		t.Fatalf("jobs cancelled despite active send guard: %v", canceller.jobIDs)
	}
}

// TestDeleteUserData_DoesNotAffectOtherUsers is the cross-tenant
// isolation check. Two users; deleting one must not touch the other's
// data even when both have the same domain/agent shape.
func TestDeleteUserData_DoesNotAffectOtherUsers(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	target := seedUserData(t, store, ctx, "target")
	bystander := seedUserData(t, store, ctx, "bystander")

	if _, err := store.DeleteUserData(ctx, target.ID); err != nil {
		t.Fatalf("DeleteUserData: %v", err)
	}

	// Bystander should still be intact.
	dump, err := store.ExportUserData(ctx, bystander.ID)
	if err != nil {
		t.Fatalf("bystander export: %v", err)
	}
	if dump.User.ID != bystander.ID {
		t.Errorf("bystander user wrong: got %s, want %s", dump.User.ID, bystander.ID)
	}
	if len(dump.Agents) != 1 {
		t.Errorf("bystander agents: got %d, want 1 — cross-tenant cascade leaked", len(dump.Agents))
	}
	if len(dump.Messages) != 3 {
		t.Errorf("bystander messages: got %d, want 3 — cross-tenant cascade leaked", len(dump.Messages))
	}
}

// TestDeleteUserData_NoSuchUser returns UserDeleted=false rather than
// erroring, so the operation is idempotent — re-running a deletion
// after a partial failure or a duplicate request doesn't blow up.
func TestDeleteUserData_NoSuchUser(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	res, err := store.DeleteUserData(ctx, "usr_does_not_exist")
	if err != nil {
		t.Fatalf("DeleteUserData on missing user should not error, got: %v", err)
	}
	if res.UserDeleted {
		t.Error("UserDeleted should be false for non-existent user")
	}
}

// TestExportUserData_AfterDeletion ensures the export query handles a
// deleted user gracefully (returns an error rather than panicking on
// the missing user row).
func TestExportUserData_AfterDeletion(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user := seedUserData(t, store, ctx, "ghost")
	if _, err := store.DeleteUserData(ctx, user.ID); err != nil {
		t.Fatalf("DeleteUserData: %v", err)
	}

	if _, err := store.ExportUserData(ctx, user.ID); err == nil {
		t.Error("ExportUserData on deleted user should error, got nil")
	}
}

// jsonContains reports whether the JSON byte slice contains the given
// field name as a top-level or nested key. Naive substring match is
// fine for this test — we're only checking that internal identifiers
// don't appear anywhere in the export.
func jsonContains(b []byte, field string) bool {
	// Quote the field so we match `"field":` and not the substring
	// inside a value that happens to contain the word.
	needle := []byte("\"" + field + "\":")
	return containsBytes(b, needle)
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}

// silence unused-import lint when the time package isn't otherwise used.
var _ = time.Time{}
