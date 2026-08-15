package identity_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/migrations"
)

func TestCreateAgent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, err := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-create-agent")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	if _, err := store.ClaimOrCreateDomain(ctx, "bot.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}

	a, err := store.CreateAgent(ctx, "agent@bot.example.com", "bot.example.com", "", "https://example.com/webhook", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	if a.ID != "agent@bot.example.com" {
		t.Errorf("ID = %q, want %q", a.ID, "agent@bot.example.com")
	}
	if a.Domain != "bot.example.com" {
		t.Errorf("Domain = %q, want %q", a.Domain, "bot.example.com")
	}
	// bot.example.com was claimed but never verified, so the create response
	// must report DomainVerified=false — matching what GetAgentByID would read
	// from domains.verified. (This is the meaningful false case: the store
	// permits agents on unverified domains.)
	if a.DomainVerified {
		t.Error("expected DomainVerified=false for agent on an unverified domain")
	}
}

// TestCreateAgentVerifiedDomain pins the bug fix: when the agent's domain is
// verified, the create response must report DomainVerified=true immediately —
// not the Go zero value that createAgent used to leave unset (which flipped to
// true only on a follow-up GET). Mirrors the value GetAgentByID reads back.
func TestCreateAgentVerifiedDomain(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, err := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-create-agent-verified")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	if _, err := store.ClaimOrCreateDomain(ctx, "verified.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.VerifyDomain(ctx, "verified.example.com", user.ID); err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}

	a, err := store.CreateAgent(ctx, "agent@verified.example.com", "verified.example.com", "", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if !a.DomainVerified {
		t.Error("expected DomainVerified=true for agent on a verified domain (create response must not return a stale zero value)")
	}

	// And the create response must agree with the authoritative read path.
	got, err := store.GetAgentByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAgentByID: %v", err)
	}
	if got.DomainVerified != a.DomainVerified {
		t.Errorf("create DomainVerified=%v disagrees with GetAgentByID=%v", a.DomainVerified, got.DomainVerified)
	}
}

func TestCreateAgentDuplicate(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-dup-agent")
	store.ClaimOrCreateDomain(ctx, "dup.example.com", user.ID)

	store.CreateAgent(ctx, "agent@dup.example.com", "dup.example.com", "", "https://example.com/webhook", "", user.ID)
	_, err := store.CreateAgent(ctx, "agent@dup.example.com", "dup.example.com", "", "https://example.com/webhook2", "", user.ID)
	if err == nil {
		t.Error("expected error for duplicate agent")
	}
}

// TestClaimOrCreateDomain_StableOnReclaim asserts that re-claiming an
// unverified domain returns the row unchanged: the verification_token
// and DKIM public key minted on the first call must survive the second.
// A caller that has already published the TXT record on DNS would
// otherwise be silently invalidated by a benign second register call
// (e.g. an agent re-fetching the records to show the user).
func TestClaimOrCreateDomain_StableOnReclaim(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner-stable@example.com", "Owner", "google-stable-token")

	first, err := store.ClaimOrCreateDomain(ctx, "stable.example.com", user.ID)
	if err != nil {
		t.Fatalf("first ClaimOrCreateDomain: %v", err)
	}
	if first.VerificationToken == "" {
		t.Fatal("first call returned empty VerificationToken")
	}

	second, err := store.ClaimOrCreateDomain(ctx, "stable.example.com", user.ID)
	if err != nil {
		t.Fatalf("second ClaimOrCreateDomain: %v", err)
	}

	if second.VerificationToken != first.VerificationToken {
		t.Errorf("VerificationToken rotated on reclaim: first=%q second=%q", first.VerificationToken, second.VerificationToken)
	}
	if second.DKIMPublicKey != first.DKIMPublicKey {
		t.Errorf("DKIMPublicKey rotated on reclaim: first=%q second=%q", first.DKIMPublicKey, second.DKIMPublicKey)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("CreatedAt changed on reclaim: first=%v second=%v", first.CreatedAt, second.CreatedAt)
	}
}

// TestClaimOrCreateDomain_CrossUserReclaimRejected asserts that a second
// user cannot take over an unverified domain that another user has
// already claimed. Combined with the stable verification_token, this
// closes the squatting window where the takeover user could verify
// against a TXT record the original owner had already published.
func TestClaimOrCreateDomain_CrossUserReclaimRejected(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	userA, _ := store.CreateOrGetUser(ctx, "owner-a@example.com", "Owner A", "google-a")
	userB, _ := store.CreateOrGetUser(ctx, "owner-b@example.com", "Owner B", "google-b")

	first, err := store.ClaimOrCreateDomain(ctx, "squat.example.com", userA.ID)
	if err != nil {
		t.Fatalf("userA ClaimOrCreateDomain: %v", err)
	}

	if _, err := store.ClaimOrCreateDomain(ctx, "squat.example.com", userB.ID); err == nil {
		t.Fatal("userB reclaim should fail when userA already owns the unverified row")
	}

	// userB cannot read the row either; userA still owns it and the
	// verification_token is unchanged.
	if _, err := store.LookupDomain(ctx, "squat.example.com", userB.ID); !errors.Is(err, identity.ErrDomainNotFound) {
		t.Errorf("userB LookupDomain error = %v, want ErrDomainNotFound", err)
	}
	after, err := store.LookupDomain(ctx, "squat.example.com", userA.ID)
	if err != nil {
		t.Fatalf("userA LookupDomain: %v", err)
	}
	if after.UserID == nil || *after.UserID != userA.ID {
		t.Errorf("ownership changed: got user_id=%v, want %s", after.UserID, userA.ID)
	}
	if after.VerificationToken != first.VerificationToken {
		t.Errorf("verification_token rotated under cross-user reclaim: first=%q after=%q", first.VerificationToken, after.VerificationToken)
	}
}

func TestGetAgentByID(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-agent-byid")
	store.ClaimOrCreateDomain(ctx, "agentid.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "agent@agentid.example.com", "agentid.example.com", "", "https://example.com/webhook", "", user.ID)

	got, err := store.GetAgentByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAgentByID: %v", err)
	}
	if got.Domain != "agentid.example.com" {
		t.Errorf("Domain = %q", got.Domain)
	}
}

func TestGetAgentByEmail(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-agent-byemail")
	store.ClaimOrCreateDomain(ctx, "lookup.example.com", user.ID)
	created, _ := store.CreateAgent(ctx, "agent@lookup.example.com", "lookup.example.com", "", "https://example.com/webhook", "", user.ID)

	got, err := store.GetAgentByEmail(ctx, "agent@lookup.example.com")
	if err != nil {
		t.Fatalf("GetAgentByEmail: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("ID mismatch")
	}
}

func TestCreateAPIKey(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, err := store.CreateOrGetUser(ctx, "apikey-owner@example.com", "Owner", "google-apikey-create")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	key, err := store.CreateAPIKey(ctx, user.ID, "test key", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if !strings.HasPrefix(key.ID, "apk_") {
		t.Errorf("ID should start with apk_, got %q", key.ID)
	}
	if !strings.HasPrefix(key.PlaintextKey, "e2a_") {
		t.Errorf("PlaintextKey should start with e2a_, got %q", key.PlaintextKey)
	}
	if key.UserID != user.ID {
		t.Errorf("UserID = %q, want %q", key.UserID, user.ID)
	}
	if key.Name != "test key" {
		t.Errorf("Name = %q, want %q", key.Name, "test key")
	}
}

func TestListAPIKeys(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "apikey-list@example.com", "Owner", "google-apikey-list")
	store.CreateAPIKey(ctx, user.ID, "key-1", nil)
	store.CreateAPIKey(ctx, user.ID, "key-2", nil)

	keys, err := store.ListAPIKeys(ctx, user.ID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
}

func TestDeleteAPIKey(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "apikey-del@example.com", "Owner", "google-apikey-del")
	key, _ := store.CreateAPIKey(ctx, user.ID, "to-delete", nil)

	err := store.DeleteAPIKey(ctx, key.ID, user.ID)
	if err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}

	keys, _ := store.ListAPIKeys(ctx, user.ID, 0, time.Time{}, "")
	if len(keys) != 0 {
		t.Errorf("expected 0 keys after delete, got %d", len(keys))
	}
}

// TestDeleteAgentReturnsMessageCascadeCount pins the DeleteAgent receipt: the
// agent's message rows are deleted in the same transaction and their
// rows-affected count is returned for the API's deletion object
// (DeleteAgentResult.messages_deleted).
func TestDeleteAgentReturnsMessageCascadeCount(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "agent-del@example.com", "Owner", "google-agent-del")
	if _, err := store.ClaimOrCreateDomain(ctx, "agentdel.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	agent, err := store.CreateAgent(ctx, "bot@agentdel.example.com", "agentdel.example.com", "", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.CreateInboundMessage(ctx, "", agent.ID, "a@gmail.com", "bot@agentdel.example.com", fmt.Sprintf("<del-%d@gmail.com>", i), "M", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{}); err != nil {
			t.Fatalf("CreateInboundMessage: %v", err)
		}
	}

	n, err := store.DeleteAgent(ctx, agent.ID, user.ID)
	if err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	if n != 3 {
		t.Errorf("messagesDeleted = %d, want 3", n)
	}
	if got, err := store.GetAgentByID(ctx, agent.ID); err == nil && got != nil {
		t.Error("agent still readable after delete")
	}

	// Not-owned / missing agent: no count, an error, and nothing deleted.
	if _, err := store.DeleteAgent(ctx, agent.ID, user.ID); err == nil {
		t.Error("expected error deleting an already-deleted agent")
	}
}

func TestGetUserByAPIKey(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "apikey-lookup@example.com", "Owner", "google-apikey-lookup")
	key, _ := store.CreateAPIKey(ctx, user.ID, "lookup-key", nil)

	got, err := store.GetUserByAPIKey(ctx, key.PlaintextKey)
	if err != nil {
		t.Fatalf("GetUserByAPIKey: %v", err)
	}
	if got.ID != user.ID {
		t.Errorf("ID = %q, want %q", got.ID, user.ID)
	}
	if got.Email != "apikey-lookup@example.com" {
		t.Errorf("Email = %q", got.Email)
	}
}

// TestAPIKey_ListReturnsLastUsedAtAndExpiresAt asserts the columns
// added/exposed by migration 011: last_used_at is populated by
// GetUserByAPIKey and surfaced by ListAPIKeys; expires_at is round-
// tripped from CreateAPIKey through the list endpoint.
func TestAPIKey_ListReturnsLastUsedAtAndExpiresAt(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "apikey-lastused@example.com", "Owner", "google-apikey-lastused")

	// One key with expiry, one without — covers both column states.
	expiresAt := time.Now().Add(7 * 24 * time.Hour).UTC().Round(time.Microsecond)
	withExpiry, _ := store.CreateAPIKey(ctx, user.ID, "with-expiry", &expiresAt)
	neverExpires, _ := store.CreateAPIKey(ctx, user.ID, "never-expires", nil)

	// Before any use, last_used_at is NULL on both rows.
	keys, err := store.ListAPIKeys(ctx, user.ID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	byID := map[string]identity.APIKey{}
	for _, k := range keys {
		byID[k.ID] = k
	}
	if k := byID[withExpiry.ID]; k.LastUsedAt != nil {
		t.Errorf("with-expiry LastUsedAt = %v, want nil before first use", k.LastUsedAt)
	}
	if k := byID[withExpiry.ID]; k.ExpiresAt == nil || !k.ExpiresAt.Equal(expiresAt) {
		t.Errorf("with-expiry ExpiresAt = %v, want %v", k.ExpiresAt, expiresAt)
	}
	if k := byID[neverExpires.ID]; k.ExpiresAt != nil {
		t.Errorf("never-expires ExpiresAt = %v, want nil", k.ExpiresAt)
	}

	// Authenticate once → last_used_at should populate on that row only.
	if _, err := store.GetUserByAPIKey(ctx, withExpiry.PlaintextKey); err != nil {
		t.Fatalf("GetUserByAPIKey: %v", err)
	}
	keys, _ = store.ListAPIKeys(ctx, user.ID, 0, time.Time{}, "")
	for _, k := range keys {
		if k.ID == withExpiry.ID {
			if k.LastUsedAt == nil {
				t.Errorf("with-expiry LastUsedAt should be set after auth")
			}
		} else if k.ID == neverExpires.ID {
			if k.LastUsedAt != nil {
				t.Errorf("never-expires LastUsedAt = %v, want nil (untouched key)", k.LastUsedAt)
			}
		}
	}
}

// TestAPIKey_ExpiredKeyRejectedAtAuth: a key whose expires_at has
// passed must fail GetUserByAPIKey. This is the auth-side gate that
// makes the expires_at column actually enforce anything.
func TestAPIKey_ExpiredKeyRejectedAtAuth(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "apikey-expired@example.com", "Owner", "google-apikey-expired")

	// Issue with a future expiry, then backdate via direct SQL — Create
	// rejects past timestamps at the handler layer, but the store itself
	// doesn't validate (it's the auth gate that does the enforcement).
	future := time.Now().Add(1 * time.Hour)
	key, _ := store.CreateAPIKey(ctx, user.ID, "soon-to-expire", &future)
	if _, err := pool.Exec(ctx, `UPDATE api_keys SET expires_at = $1 WHERE id = $2`,
		time.Now().Add(-1*time.Minute), key.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if _, err := store.GetUserByAPIKey(ctx, key.PlaintextKey); err == nil {
		t.Error("GetUserByAPIKey should reject expired keys; got success")
	}

	// Sanity: a key with NULL expires_at issued by the same user still
	// authenticates fine (i.e. the gate is per-row, not per-user).
	stillValid, _ := store.CreateAPIKey(ctx, user.ID, "still-valid", nil)
	if _, err := store.GetUserByAPIKey(ctx, stillValid.PlaintextKey); err != nil {
		t.Errorf("never-expiring key should still authenticate: %v", err)
	}
}

func TestCreateAndGetInboundMessage(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-inbound")
	store.ClaimOrCreateDomain(ctx, "inbound.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "agent@inbound.example.com", "inbound.example.com", "", "https://example.com/webhook", "", user.ID)

	msg, err := store.CreateInboundMessage(ctx, "", a.ID, "alice@gmail.com", "bot@inbound.example.com", "<abc123@gmail.com>", "Hello Bot", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("CreateInboundMessage: %v", err)
	}
	if !strings.HasPrefix(msg.ID, "msg_") {
		t.Errorf("ID should start with msg_, got %q", msg.ID)
	}
	if msg.AgentID != a.ID {
		t.Errorf("AgentID = %q, want %q", msg.AgentID, a.ID)
	}
	if msg.Direction != "inbound" {
		t.Errorf("Direction = %q, want inbound", msg.Direction)
	}
	if msg.Sender != "alice@gmail.com" {
		t.Errorf("Sender = %q", msg.Sender)
	}
	if msg.EmailMessageID != "<abc123@gmail.com>" {
		t.Errorf("EmailMessageID = %q", msg.EmailMessageID)
	}
	if msg.Subject != "Hello Bot" {
		t.Errorf("Subject = %q", msg.Subject)
	}

	got, err := store.GetInboundMessage(ctx, msg.ID)
	if err != nil {
		t.Fatalf("GetInboundMessage: %v", err)
	}
	if got.ID != msg.ID {
		t.Errorf("ID mismatch")
	}
}

func TestInboundMessageRoundTripsAuthentication(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "av-owner@example.com", "Owner", "google-authverdict")
	store.ClaimOrCreateDomain(ctx, "authverdict.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "bot@authverdict.example.com", "authverdict.example.com", "", "https://example.com/webhook", "", user.ID)

	spfDomain, dkimDomain, selector := "mail.example.com", "example.com", "s1"
	policy := emailauth.DMARCPolicyReject
	spfAligned, dkimAligned := true, true
	authentication := &emailauth.Authentication{
		SPF:   emailauth.SPFResult{Status: emailauth.StatusPass, Domain: &spfDomain, Aligned: &spfAligned},
		DKIM:  []emailauth.DKIMResult{{Status: emailauth.StatusPass, Domain: &dkimDomain, Selector: &selector, Aligned: &dkimAligned}},
		DMARC: emailauth.DMARCResult{Status: emailauth.StatusPass, Domain: &dkimDomain, Policy: &policy, AlignedBy: []emailauth.AlignmentMechanism{emailauth.AlignedBySPF, emailauth.AlignedByDKIM}},
	}
	authData := identity.InboundAuth{
		HeaderFrom:     "alice@example.com",
		EnvelopeFrom:   "bounce@mail.example.com",
		Authentication: authentication,
	}
	in, err := store.CreateInboundMessageAuthenticated(ctx, "", a.ID, authData, "bot@authverdict.example.com", "<av1@gmail.com>", "Hello", "", "unread", nil, false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("CreateInboundMessage: %v", err)
	}

	assertCanonical := func(label string, got *identity.Message) {
		t.Helper()
		if got.HeaderFrom != authData.HeaderFrom || got.EnvelopeFrom != authData.EnvelopeFrom {
			t.Fatalf("%s identities = (%q, %q), want (%q, %q)", label, got.HeaderFrom, got.EnvelopeFrom, authData.HeaderFrom, authData.EnvelopeFrom)
		}
		if !reflect.DeepEqual(got.Authentication, authentication) {
			t.Fatalf("%s authentication = %#v, want %#v", label, got.Authentication, authentication)
		}
	}

	gotInbound, err := store.GetInboundMessage(ctx, in.ID)
	if err != nil {
		t.Fatalf("GetInboundMessage: %v", err)
	}
	assertCanonical("inbound", gotInbound)

	got, err := store.GetMessageWithContent(ctx, in.ID, a.ID)
	if err != nil {
		t.Fatalf("GetMessageWithContent: %v", err)
	}
	assertCanonical("detail", got)
	listed, err := store.GetMessagesByAgent(ctx, identity.MessageListFilter{AgentID: a.ID, Direction: "inbound", Status: "all", Limit: 10})
	if err != nil || len(listed) != 1 || !reflect.DeepEqual(listed[0].Authentication, authentication) {
		t.Fatalf("list round trip = %#v, err=%v", listed, err)
	}
	reviewed, err := store.GetReviewWithContent(ctx, user.ID, in.ID)
	if err != nil {
		t.Fatalf("GetReviewWithContent: %v", err)
	}
	assertCanonical("review", reviewed)
	exported, err := store.ExportUserData(ctx, user.ID)
	if err != nil || len(exported.Messages) != 1 || !reflect.DeepEqual(exported.Messages[0].Authentication, authentication) {
		t.Fatalf("export round trip = %#v, err=%v", exported.Messages, err)
	}

	// Outbound rows never carry inbound authentication evidence.
	out, err := store.CreateOutboundMessage(ctx, a.ID, []string{"bob@gmail.com"}, nil, nil, "Re: Hello", "reply", "smtp", "<prov@authverdict.example.com>", "", nil)
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}
	gotOut, err := store.GetMessageWithContent(ctx, out.ID, a.ID)
	if err != nil {
		t.Fatalf("GetMessageWithContent outbound: %v", err)
	}
	if gotOut.Authentication != nil {
		t.Errorf("outbound Authentication = %+v, want nil", gotOut.Authentication)
	}
}

func TestLegacyInboundAuthenticationFailsClosed(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "legacy-auth@example.com", "Owner", "google-legacy-auth")
	store.ClaimOrCreateDomain(ctx, "legacy-auth.example.com", user.ID)
	agent, _ := store.CreateAgent(ctx, "bot@legacy-auth.example.com", "legacy-auth.example.com", "", "https://example.com/webhook", "", user.ID)

	legacy := []byte(`{"spf":{"status":"pass"},"dkim":{"status":"pass"},"dmarc":{"status":"pass"}}`)
	message, err := store.CreateInboundMessage(ctx, "", agent.ID, "spoof@trusted.example", agent.ID, "<legacy@example.com>", "Legacy", "", "unread", nil, nil, legacy, false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("CreateInboundMessage: %v", err)
	}
	got, err := store.GetInboundMessage(ctx, message.ID)
	if err != nil {
		t.Fatalf("GetInboundMessage: %v", err)
	}
	if got.Authentication == nil || got.Authentication.DMARC.Status != emailauth.StatusPermError {
		t.Fatalf("legacy authentication = %#v, want fail-closed permerror", got.Authentication)
	}
	if got.Authentication.Passed() {
		t.Fatal("legacy authentication must never be promoted to a pass")
	}
}

func TestInboundMessageRoundTripsToCcLists(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "tcc-owner@example.com", "Owner", "google-tcc")
	store.ClaimOrCreateDomain(ctx, "tcc.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "bot-a@tcc.example.com", "tcc.example.com", "", "https://example.com/webhook", "", user.ID)

	to := []string{"bot-a@tcc.example.com", "bot-b@tcc.example.com"}
	cc := []string{"watcher@example.com", "audit@example.com"}
	// Two addresses to exercise the multi-value path; RFC 5322 § 3.6.2
	// permits more than one Reply-To. Single-value is the common case
	// and is covered transitively by other tests that pass nil here.
	replyTo := []string{"real-user@example.com", "delegate@example.com"}

	msg, err := store.CreateInboundMessage(ctx, "", a.ID, "alice@gmail.com", "bot-a@tcc.example.com", "<x@gmail.com>", "Group thread", "", "", nil, nil, nil, false, "", to, cc, replyTo, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("CreateInboundMessage: %v", err)
	}

	got, err := store.GetInboundMessage(ctx, msg.ID)
	if err != nil {
		t.Fatalf("GetInboundMessage: %v", err)
	}
	if !reflect.DeepEqual(got.ToRecipients, to) {
		t.Errorf("ToRecipients = %v, want %v", got.ToRecipients, to)
	}
	if !reflect.DeepEqual(got.CC, cc) {
		t.Errorf("CC = %v, want %v", got.CC, cc)
	}
	if !reflect.DeepEqual(got.ReplyTo, replyTo) {
		t.Errorf("ReplyTo = %v, want %v", got.ReplyTo, replyTo)
	}

	// Also exercise the consumer-facing read path (GET /messages/{id})
	// — different SELECT columns, easy to drift independently.
	gotDetail, err := store.GetMessageWithContent(ctx, msg.ID, a.ID)
	if err != nil {
		t.Fatalf("GetMessageWithContent: %v", err)
	}
	if !reflect.DeepEqual(gotDetail.ReplyTo, replyTo) {
		t.Errorf("GetMessageWithContent ReplyTo = %v, want %v", gotDetail.ReplyTo, replyTo)
	}

	// And the list path (GET /messages) — yet another SELECT.
	msgs, err := store.GetMessagesByAgent(ctx, identity.MessageListFilter{
		AgentID:   a.ID,
		Status:    "all",
		Direction: "inbound",
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("GetMessagesByAgent: %v", err)
	}
	var found *identity.Message
	for i := range msgs {
		if msgs[i].ID == msg.ID {
			found = &msgs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("GetMessagesByAgent did not return %s", msg.ID)
	}
	if !reflect.DeepEqual(found.ReplyTo, replyTo) {
		t.Errorf("GetMessagesByAgent ReplyTo = %v, want %v", found.ReplyTo, replyTo)
	}
}

func TestGetInboundMessageNotFound(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)

	_, err := store.GetInboundMessage(context.Background(), "msg_nonexistent")
	if err == nil {
		t.Error("expected error for non-existent inbound message")
	}
}

func TestMessageRetentionIsIndefinite(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	var nullable string
	var columnDefault *string
	if err := pool.QueryRow(ctx, `
		SELECT is_nullable, column_default
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'messages'
		   AND column_name = 'expires_at'`).Scan(&nullable, &columnDefault); err != nil {
		t.Fatalf("inspect messages.expires_at: %v", err)
	}
	if nullable != "YES" {
		t.Errorf("messages.expires_at is_nullable = %q, want YES", nullable)
	}
	if columnDefault != nil {
		t.Errorf("messages.expires_at default = %q, want NULL", *columnDefault)
	}

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-indefinite-inbound")
	store.ClaimOrCreateDomain(ctx, "indefinite-inbound.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "agent@indefinite-inbound.example.com", "indefinite-inbound.example.com", "", "", "", user.ID)
	msg, err := store.CreateInboundMessage(ctx, "", a.ID, "alice@gmail.com", a.ID, "", "", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("CreateInboundMessage: %v", err)
	}
	var expiresAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT expires_at FROM messages WHERE id = $1`, msg.ID).Scan(&expiresAt); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	if expiresAt != nil {
		t.Errorf("expires_at = %v, want NULL", expiresAt)
	}
}

func TestIndefiniteRetentionMigrationPreservesLegacyTimestamps(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-indefinite-backfill")
	store.ClaimOrCreateDomain(ctx, "indefinite-backfill.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "agent@indefinite-backfill.example.com", "indefinite-backfill.example.com", "", "", "", user.ID)
	msg, err := store.CreateInboundMessage(ctx, "", a.ID, "alice@gmail.com", a.ID, "", "", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("CreateInboundMessage: %v", err)
	}
	legacyExpiry := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `UPDATE messages SET expires_at = $1 WHERE id = $2`, legacyExpiry, msg.ID); err != nil {
		t.Fatalf("seed legacy expires_at: %v", err)
	}

	migration, err := migrations.FS.ReadFile("072_indefinite_message_retention.sql")
	if err != nil {
		t.Fatalf("read migration 072: %v", err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply migration 072: %v", err)
	}

	var expiresAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT expires_at FROM messages WHERE id = $1`, msg.ID).Scan(&expiresAt); err != nil {
		t.Fatalf("read legacy expires_at: %v", err)
	}
	if expiresAt == nil || !expiresAt.Equal(legacyExpiry) {
		t.Errorf("expires_at after migration = %v, want preserved legacy value %v", expiresAt, legacyExpiry)
	}
}

func TestGetInboundMessageIgnoresLegacyExpiry(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-expired-inbound")
	store.ClaimOrCreateDomain(ctx, "expired-inbound.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "agent@expired-inbound.example.com", "expired-inbound.example.com", "", "https://example.com/webhook", "", user.ID)
	msg, _ := store.CreateInboundMessage(ctx, "", a.ID, "alice@gmail.com", "bot@expired-inbound.example.com", "", "", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})

	// Set expiry to the past
	pool.Exec(ctx, `UPDATE messages SET expires_at = $1 WHERE id = $2`, time.Now().Add(-1*time.Hour), msg.ID)

	got, err := store.GetInboundMessage(ctx, msg.ID)
	if err != nil {
		t.Fatalf("GetInboundMessage with legacy past expires_at: %v", err)
	}
	if got.ID != msg.ID {
		t.Errorf("GetInboundMessage id = %q, want %q", got.ID, msg.ID)
	}
}

func TestGetRepliableMessage_ReturnsOutbound(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-repliable-out")
	store.ClaimOrCreateDomain(ctx, "repliable-out.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "agent@repliable-out.example.com", "repliable-out.example.com", "", "https://example.com/webhook", "", user.ID)

	out, err := store.CreateOutboundMessage(ctx, a.ID, []string{"bob@gmail.com"}, []string{"carol@gmail.com"}, nil, "Update", "send", "smtp", "<sent-1@repliable-out.example.com>", "conv_x", []byte("raw"))
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}

	// GetRepliableMessage resolves the outbound row with its recipients intact.
	got, err := store.GetRepliableMessage(ctx, out.ID)
	if err != nil {
		t.Fatalf("GetRepliableMessage(outbound): %v", err)
	}
	if got.Direction != "outbound" {
		t.Errorf("Direction = %q, want outbound", got.Direction)
	}
	if len(got.ToRecipients) != 1 || got.ToRecipients[0] != "bob@gmail.com" {
		t.Errorf("ToRecipients = %v, want [bob@gmail.com]", got.ToRecipients)
	}
	if len(got.CC) != 1 || got.CC[0] != "carol@gmail.com" {
		t.Errorf("CC = %v, want [carol@gmail.com]", got.CC)
	}
	// provider_message_id is the outbound's RFC Message-ID (email_message_id is
	// empty for outbound), and ThreadMessageID resolves to it — this is what the
	// reply path anchors In-Reply-To/References on.
	if got.ProviderMessageID != "<sent-1@repliable-out.example.com>" {
		t.Errorf("ProviderMessageID = %q, want the sent Message-ID", got.ProviderMessageID)
	}
	if got.ThreadMessageID() != "<sent-1@repliable-out.example.com>" {
		t.Errorf("ThreadMessageID() = %q, want provider_message_id for outbound", got.ThreadMessageID())
	}

	// The legacy inbound-only lookup must still refuse the same outbound id —
	// this is exactly the asymmetry the feature relies on.
	if _, err := store.GetInboundMessage(ctx, out.ID); err == nil {
		t.Error("GetInboundMessage returned an outbound message (direction filter regressed)")
	}
}

func TestGetMessageByEmailMessageID_ResolvesOutboundByProviderID(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-parentlookup")
	store.ClaimOrCreateDomain(ctx, "parentlookup.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "agent@parentlookup.example.com", "parentlookup.example.com", "", "https://example.com/webhook", "", user.ID)

	// An outbound the agent sent: its RFC Message-ID lives in provider_message_id,
	// not email_message_id. The HITL approve-time References rebuild must resolve
	// the parent by that id (a held reply-to-own-message threads on it).
	providerID := "<sent-p@parentlookup.example.com>"
	out, _ := store.CreateOutboundMessage(ctx, a.ID, []string{"bob@gmail.com"}, nil, nil, "Update", "send", "smtp", providerID, "", []byte("raw"))

	got, err := store.GetMessageByEmailMessageID(ctx, a.ID, providerID)
	if err != nil {
		t.Fatalf("GetMessageByEmailMessageID(providerID): %v", err)
	}
	if got.ID != out.ID {
		t.Errorf("resolved id = %q, want the outbound %q", got.ID, out.ID)
	}

	// A different agent must not resolve it (agent-scoped).
	if _, err := store.GetMessageByEmailMessageID(ctx, "someone-else", providerID); err == nil {
		t.Error("GetMessageByEmailMessageID resolved across agents")
	}
}

func TestGetRepliableMessage_ExcludesHeldButIgnoresLegacyExpiry(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-repliable-held")
	store.ClaimOrCreateDomain(ctx, "repliable-held.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "agent@repliable-held.example.com", "repliable-held.example.com", "", "https://example.com/webhook", "", user.ID)

	out, _ := store.CreateOutboundMessage(ctx, a.ID, []string{"bob@gmail.com"}, nil, nil, "Update", "send", "smtp", "", "", nil)

	// A message still in review (not yet delivered) is not a valid reply target.
	pool.Exec(ctx, `UPDATE messages SET status = 'pending_review' WHERE id = $1`, out.ID)
	if _, err := store.GetRepliableMessage(ctx, out.ID); err == nil {
		t.Error("GetRepliableMessage returned a held (pending_review) message")
	}

	// A legacy past expires_at no longer makes the message unavailable.
	pool.Exec(ctx, `UPDATE messages SET status = 'sent', expires_at = $1 WHERE id = $2`, time.Now().Add(-1*time.Hour), out.ID)
	if _, err := store.GetRepliableMessage(ctx, out.ID); err != nil {
		t.Fatalf("GetRepliableMessage with legacy past expires_at: %v", err)
	}
}

func TestDeleteExpiredMessagesKeepsLiveLegacyRows(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-cleanup-inbound")
	store.ClaimOrCreateDomain(ctx, "cleanup-inbound.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "agent@cleanup-inbound.example.com", "cleanup-inbound.example.com", "", "https://example.com/webhook", "", user.ID)
	msg, _ := store.CreateInboundMessage(ctx, "", a.ID, "alice@gmail.com", "bot@cleanup-inbound.example.com", "", "", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})

	// Set expiry to the past
	pool.Exec(ctx, `UPDATE messages SET expires_at = $1 WHERE id = $2`, time.Now().Add(-1*time.Hour), msg.ID)

	deleted, err := store.DeleteExpiredMessages(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredMessages: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1)`, msg.ID).Scan(&exists); err != nil {
		t.Fatalf("check live message: %v", err)
	}
	if !exists {
		t.Error("live message with legacy past expires_at was deleted")
	}
}

// TestDeleteExpiredMessages_MultiBatch drives the ctid-bounded trash purge
// across more than one batch by shrinking the batch size to 2 and seeding five
// stale trash rows (batches of 2, 2, 1). A live row remains indefinitely.
func TestDeleteExpiredMessages_MultiBatch(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	restore := identity.SetExpiredDeleteBatchForTest(2)
	defer restore()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-cleanup-batch")
	store.ClaimOrCreateDomain(ctx, "cleanup-batch.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "agent@cleanup-batch.example.com", "cleanup-batch.example.com", "", "https://example.com/webhook", "", user.ID)

	// Five stale trash rows span three batches under batch size 2.
	var expiredIDs []string
	for i := 0; i < 5; i++ {
		m, _ := store.CreateInboundMessage(ctx, "", a.ID, "alice@gmail.com", "bot@cleanup-batch.example.com", "", "", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})
		expiredIDs = append(expiredIDs, m.ID)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET deleted_at = now() - interval '31 days' WHERE id = ANY($1)`, expiredIDs); err != nil {
		t.Fatalf("seed stale trash: %v", err)
	}

	// One fresh row that must survive the sweep.
	keep, _ := store.CreateInboundMessage(ctx, "", a.ID, "bob@gmail.com", "bot@cleanup-batch.example.com", "", "", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})

	deleted, err := store.DeleteExpiredMessages(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredMessages: %v", err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want 5 (all stale trash rows across 3 batches)", deleted)
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE agent_id = $1`, a.ID).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 1 {
		t.Errorf("remaining rows = %d, want 1 (the live row)", remaining)
	}
	var keepExists bool
	if err := pool.QueryRow(ctx, `SELECT exists(SELECT 1 FROM messages WHERE id = $1)`, keep.ID).Scan(&keepExists); err != nil {
		t.Fatalf("check kept row: %v", err)
	}
	if !keepExists {
		t.Errorf("live row %s was deleted", keep.ID)
	}
}

func TestCreateOutboundMessage(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-outbound")
	store.ClaimOrCreateDomain(ctx, "outbound.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "agent@outbound.example.com", "outbound.example.com", "", "https://example.com/webhook", "", user.ID)

	msg, err := store.CreateOutboundMessage(ctx, a.ID, []string{"alice@gmail.com"}, nil, nil, "Re: Hello", "reply", "smtp", "", "", nil)
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}
	if msg.Direction != "outbound" {
		t.Errorf("Direction = %q, want outbound", msg.Direction)
	}
	if msg.Method != "smtp" {
		t.Errorf("Method = %q", msg.Method)
	}
	if msg.Type != "reply" {
		t.Errorf("Type = %q", msg.Type)
	}
	if msg.Recipient != "alice@gmail.com" {
		t.Errorf("Recipient = %q, want alice@gmail.com", msg.Recipient)
	}
	if len(msg.ToRecipients) != 1 || msg.ToRecipients[0] != "alice@gmail.com" {
		t.Errorf("ToRecipients = %v, want [alice@gmail.com]", msg.ToRecipients)
	}
}

func TestListActivityByAgent(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-activity")
	store.ClaimOrCreateDomain(ctx, "activity.example.com", user.ID)
	a, _ := store.CreateAgent(ctx, "agent@activity.example.com", "activity.example.com", "", "https://example.com/webhook", "", user.ID)

	store.CreateInboundMessage(ctx, "", a.ID, "alice@gmail.com", "bot@activity.example.com", "", "Hello", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})
	store.CreateOutboundMessage(ctx, a.ID, []string{"alice@gmail.com"}, nil, nil, "Re: Hello", "reply", "smtp", "", "", nil)
	store.CreateInboundMessage(ctx, "", a.ID, "bob@gmail.com", "bot@activity.example.com", "", "Hi", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})

	activity, err := store.ListActivityByAgent(ctx, a.ID, 50)
	if err != nil {
		t.Fatalf("ListActivityByAgent: %v", err)
	}
	if len(activity) != 3 {
		t.Fatalf("got %d activities, want 3", len(activity))
	}
	// Most recent first
	if activity[0].Subject != "Hi" {
		t.Errorf("first activity subject = %q, want Hi", activity[0].Subject)
	}
	if activity[1].Direction != "outbound" {
		t.Errorf("second activity direction = %q, want outbound", activity[1].Direction)
	}
}

// -- AgentIdentity helper method tests --

func TestIsSharedDomain(t *testing.T) {
	const sharedDomain = "agents.example.com"
	tests := []struct {
		name         string
		domain       string
		sharedDomain string
		want         bool
	}{
		{"custom domain not shared", "tenant.example.com", sharedDomain, false},
		{"matches configured shared domain", sharedDomain, sharedDomain, true},
		{"empty domain", "", sharedDomain, false},
		{"shared domain unconfigured", sharedDomain, "", false},
	}
	for _, tt := range tests {
		a := &identity.AgentIdentity{Domain: tt.domain}
		if got := a.IsSharedDomain(tt.sharedDomain); got != tt.want {
			t.Errorf("%s: IsSharedDomain(domain=%q, shared=%q) = %v, want %v", tt.name, tt.domain, tt.sharedDomain, got, tt.want)
		}
	}
}

func TestActualDomain(t *testing.T) {
	tests := []struct {
		id               string
		domain           string
		registeredDomain string
		want             string
	}{
		{"bot@example.com", "example.com", "example.com", "example.com"},
		{"otto@acme.example.com", "acme.example.com", "example.com", "acme.example.com"},
		{"my-bot@agents.e2a.dev", "agents.e2a.dev", "agents.e2a.dev", "agents.e2a.dev"},
		{"", "", "", ""},
	}
	for _, tt := range tests {
		a := &identity.AgentIdentity{ID: tt.id, Domain: tt.domain, RegisteredDomain: tt.registeredDomain}
		if got := a.ActualDomain(); got != tt.want {
			t.Errorf("ActualDomain(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestRegisteredDomainName(t *testing.T) {
	a := &identity.AgentIdentity{
		ID:               "otto@acme.example.com",
		Domain:           "acme.example.com",
		RegisteredDomain: "example.com",
	}
	if got := a.RegisteredDomainName(); got != "example.com" {
		t.Fatalf("RegisteredDomainName() = %q, want example.com", got)
	}
}

func TestEmailAddress(t *testing.T) {
	tests := []struct {
		id     string
		domain string
		want   string
	}{
		{"agent@example.com", "example.com", "agent@example.com"},
		{"support@mail.co.com", "mail.co.com", "support@mail.co.com"},
		{"my-bot@agents.e2a.dev", "agents.e2a.dev", "my-bot@agents.e2a.dev"},
		{"test@agents.e2a.dev", "agents.e2a.dev", "test@agents.e2a.dev"},
	}
	for _, tt := range tests {
		a := &identity.AgentIdentity{ID: tt.id, Domain: tt.domain}
		if got := a.EmailAddress(); got != tt.want {
			t.Errorf("EmailAddress(id=%q, domain=%q) = %q, want %q", tt.id, tt.domain, got, tt.want)
		}
	}
}

func TestCreateAgentSharedDomain(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner@example.com", "Owner", "google-shared-domain")
	a, err := store.CreateAgent(ctx, "oc-bot@agents.e2a.dev", "agents.e2a.dev", "", "https://gateway.fly.dev/hooks/agent", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent shared domain: %v", err)
	}
	if a.ID != "oc-bot@agents.e2a.dev" {
		t.Errorf("ID = %q, want %q", a.ID, "oc-bot@agents.e2a.dev")
	}
	if a.Domain != "agents.e2a.dev" {
		t.Errorf("Domain = %q, want %q", a.Domain, "agents.e2a.dev")
	}
	if !a.IsSharedDomain("agents.e2a.dev") {
		t.Error("expected IsSharedDomain() = true for shared-domain agent")
	}
}

// TestLookupConversationID_EmailThread simulates the production scenario:
//
//  1. Human sends first email → inbound stored with email_message_id, no conversation_id
//  2. Agent replies → outbound stored with provider_message_id (bare SES ID) and conversation_id
//  3. Human replies again → In-Reply-To references the SES Message-ID with @region suffix
//
// The lookup must match the second inbound's In-Reply-To against the outbound's
// provider_message_id using prefix matching, and return the conversation_id.
func TestLookupConversationID_EmailThread(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner-thread@example.com", "Owner", "google-thread")
	store.ClaimOrCreateDomain(ctx, "thread.example.com", user.ID)
	agent, _ := store.CreateAgent(ctx, "bot@thread.example.com", "thread.example.com", "", "https://example.com/webhook", "", user.ID)

	// 1. First inbound email — no conversation_id yet
	_, err := store.CreateInboundMessage(ctx, "", agent.ID,
		"alice@gmail.com", "bot@thread.example.com",
		"<CAMCKtby_first@mail.gmail.com>", "Hello",
		"", // no conversation_id on first message
		"pending", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})
	if err != nil {
		t.Fatalf("CreateInboundMessage: %v", err)
	}

	// 2. Agent replies — mnexa sets conversation_id, SES returns bare Message-ID
	mnexa_conv_id := "e0533ec4-af43-4dea-9cc7-be6fff5cf440"
	_, err = store.CreateOutboundMessage(ctx, agent.ID,
		[]string{"alice@gmail.com"}, nil, nil, "Re: Hello",
		"reply", "smtp",
		"<010f019d4b3843be-53882e6f-46de-4221-a56a-ba993e8f83e8-000000>", // bare SES ID (no @region)
		mnexa_conv_id, nil)
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}

	// 3. Human replies — Gmail's In-Reply-To has the full SES Message-ID with @region
	//    References includes both the original inbound and the agent's reply
	inReplyTo := "<010f019d4b3843be-53882e6f-46de-4221-a56a-ba993e8f83e8-000000@us-east-2.amazonses.com>"
	references := "<CAMCKtby_first@mail.gmail.com>"

	lookupIDs := []string{inReplyTo, references}
	got, err := store.LookupConversationID(ctx, agent.ID, lookupIDs)
	if err != nil {
		t.Fatalf("LookupConversationID failed: %v", err)
	}
	if got != mnexa_conv_id {
		t.Errorf("LookupConversationID = %q, want %q", got, mnexa_conv_id)
	}
}

// TestLookupConversationID_ExactMatch verifies that exact matches on
// email_message_id and provider_message_id still work.
func TestLookupConversationID_ExactMatch(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner-exact@example.com", "Owner", "google-exact")
	store.ClaimOrCreateDomain(ctx, "exact.example.com", user.ID)
	agent, _ := store.CreateAgent(ctx, "bot@exact.example.com", "exact.example.com", "", "https://example.com/webhook", "", user.ID)

	convID := "conv-exact-123"

	// Outbound with full SES Message-ID (angle brackets + domain)
	_, err := store.CreateOutboundMessage(ctx, agent.ID,
		[]string{"alice@gmail.com"}, nil, nil, "Hello",
		"send", "smtp",
		"<abc123@us-east-2.amazonses.com>",
		convID, nil)
	if err != nil {
		t.Fatalf("CreateOutboundMessage: %v", err)
	}

	// Exact match on provider_message_id
	got, err := store.LookupConversationID(ctx, agent.ID, []string{"<abc123@us-east-2.amazonses.com>"})
	if err != nil {
		t.Fatalf("LookupConversationID exact match failed: %v", err)
	}
	if got != convID {
		t.Errorf("got %q, want %q", got, convID)
	}
}

// TestLookupConversationID_NoMatch verifies that lookup returns an error
// when no matching messages exist.
func TestLookupConversationID_NoMatch(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "owner-nomatch@example.com", "Owner", "google-nomatch")
	store.ClaimOrCreateDomain(ctx, "nomatch.example.com", user.ID)
	agent, _ := store.CreateAgent(ctx, "bot@nomatch.example.com", "nomatch.example.com", "", "https://example.com/webhook", "", user.ID)

	_, err := store.LookupConversationID(ctx, agent.ID, []string{"<nonexistent@example.com>"})
	if err == nil {
		t.Error("expected error for non-matching lookup, got nil")
	}
}

// --- Domain enrichment (Item #7) ---

// TestListDomainsByUser_ReturnsEnrichmentColumns: migration 013 adds
// is_primary and last_checked_at; ListDomainsByUser also computes
// agent_count via a correlated subquery. All three must round-trip
// through the JSON response for the dashboard to render the chips.
func TestListDomainsByUser_ReturnsEnrichmentColumns(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "domains-enrichment@example.com", "Owner", "google-de")

	// Two domains: one verified with an agent, one bare.
	store.ClaimOrCreateDomain(ctx, "with-agent.example.com", user.ID)
	store.VerifyDomain(ctx, "with-agent.example.com", user.ID)
	store.CreateAgent(ctx, "bot@with-agent.example.com", "with-agent.example.com", "", "https://example.com/wh", "", user.ID)

	store.ClaimOrCreateDomain(ctx, "no-agent.example.com", user.ID)

	domains, err := store.ListDomainsByUser(ctx, user.ID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListDomainsByUser: %v", err)
	}
	if len(domains) != 2 {
		t.Fatalf("expected 2 domains, got %d", len(domains))
	}
	byName := map[string]identity.Domain{}
	for _, d := range domains {
		byName[d.Domain] = d
	}
	if got := byName["with-agent.example.com"].AgentCount; got != 1 {
		t.Errorf("with-agent.example.com AgentCount = %d, want 1", got)
	}
	if got := byName["no-agent.example.com"].AgentCount; got != 0 {
		t.Errorf("no-agent.example.com AgentCount = %d, want 0", got)
	}
	// Defaults: is_primary=false, last_checked_at=nil — until something
	// actually promotes / probes them.
	for _, d := range domains {
		if d.IsPrimary {
			t.Errorf("%s IsPrimary = true, want default false", d.Domain)
		}
		if d.LastCheckedAt != nil {
			t.Errorf("%s LastCheckedAt = %v, want nil before any probe", d.Domain, d.LastCheckedAt)
		}
	}
}

// TestLookupDomain_AgentCountMatchesList pins #632: GET /v1/domains/{domain}
// (LookupDomain) must report the same agent_count as GET /v1/domains
// (ListDomainsByUser) for the same domain, not the Go zero value. Also
// checks the trashed-agent exclusion carries over, mirroring
// TestListDomains_AgentCountExcludesTrashedAgents in
// domain_trashed_agents_test.go.
func TestLookupDomain_AgentCountMatchesList(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, err := store.CreateOrGetUser(ctx, "lookup-count@example.com", "Owner", "google-lookup-count")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "lookupcount.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if _, err := store.CreateAgent(ctx, "keep@"+domain, domain, "Keep", "", "", user.ID); err != nil {
		t.Fatalf("CreateAgent keep: %v", err)
	}
	trash, err := store.CreateAgent(ctx, "trash@"+domain, domain, "Trash", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent trash: %v", err)
	}
	if err := store.SoftDeleteAgent(ctx, trash.ID, user.ID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	d, err := store.LookupDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("LookupDomain: %v", err)
	}
	if d.AgentCount != 1 {
		t.Errorf("LookupDomain AgentCount = %d, want 1 (one live agent, trashed one excluded)", d.AgentCount)
	}

	domains, err := store.ListDomainsByUser(ctx, user.ID, 50, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListDomainsByUser: %v", err)
	}
	for _, ld := range domains {
		if ld.Domain == domain {
			if ld.AgentCount != d.AgentCount {
				t.Errorf("ListDomainsByUser AgentCount = %d, LookupDomain AgentCount = %d, want equal", ld.AgentCount, d.AgentCount)
			}
			return
		}
	}
	t.Fatalf("domain %s not found in ListDomainsByUser", domain)
}

// TestClaimOrCreateDomain_ReclaimPopulatesAgentCount pins #811: a same-owner
// re-claim is idempotent and returns the existing row, which POST /v1/domains
// serializes through the same view as GET /v1/domains/{domain} — so its
// AgentCount must be the real count (trashed agents excluded, mirroring
// TestLookupDomain_AgentCountMatchesList), not the Go zero value. The
// first-claim assertion also pins the INSERT branch's genuine zero, so a
// passing count is a real count and not a constant.
func TestClaimOrCreateDomain_ReclaimPopulatesAgentCount(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, err := store.CreateOrGetUser(ctx, "reclaim-count@example.com", "Owner", "google-reclaim-count")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "reclaimcount.example.com"
	first, err := store.ClaimOrCreateDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("ClaimOrCreateDomain (create): %v", err)
	}
	if first.AgentCount != 0 {
		t.Errorf("fresh claim AgentCount = %d, want 0 (brand-new row cannot have agents)", first.AgentCount)
	}
	if _, err := store.CreateAgent(ctx, "keep@"+domain, domain, "Keep", "", "", user.ID); err != nil {
		t.Fatalf("CreateAgent keep: %v", err)
	}
	trash, err := store.CreateAgent(ctx, "trash@"+domain, domain, "Trash", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent trash: %v", err)
	}
	if err := store.SoftDeleteAgent(ctx, trash.ID, user.ID); err != nil {
		t.Fatalf("SoftDeleteAgent: %v", err)
	}

	reclaimed, err := store.ClaimOrCreateDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("ClaimOrCreateDomain (re-claim): %v", err)
	}
	if reclaimed.AgentCount != 1 {
		t.Errorf("re-claim AgentCount = %d, want 1 (one live agent, trashed one excluded)", reclaimed.AgentCount)
	}

	looked, err := store.LookupDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("LookupDomain: %v", err)
	}
	if looked.AgentCount != reclaimed.AgentCount {
		t.Errorf("LookupDomain AgentCount = %d, re-claim AgentCount = %d, want equal", looked.AgentCount, reclaimed.AgentCount)
	}
}

// TestTouchDomainLastChecked_PersistsTimestamp: ensures the column
// actually moves when called. This is the only path that writes
// last_checked_at; without the touch, the column stays NULL even after
// many verification probes.
func TestTouchDomainLastChecked_PersistsTimestamp(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "touched@example.com", "Owner", "google-touched")
	store.ClaimOrCreateDomain(ctx, "touched.example.com", user.ID)

	before := time.Now()
	if err := store.TouchDomainLastChecked(ctx, "touched.example.com", user.ID); err != nil {
		t.Fatalf("TouchDomainLastChecked: %v", err)
	}

	d, _ := store.LookupDomain(ctx, "touched.example.com", user.ID)
	if d.LastCheckedAt == nil {
		t.Fatal("LastCheckedAt should be populated after touch")
	}
	if d.LastCheckedAt.Before(before.Add(-1 * time.Second)) {
		t.Errorf("LastCheckedAt = %v, expected to be at or after %v", d.LastCheckedAt, before)
	}
}

// --- Dashboard stats (Item #1) ---

// TestGetDashboardStats_EmptyDeployment: a brand-new user with no
// activity returns zeros everywhere, no errors. The redesign uses
// these zeros to render "—" in the cards rather than crashing the
// dashboard.
func TestGetDashboardStats_EmptyDeployment(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "empty-stats@example.com", "Owner", "google-es")

	stats, err := store.GetDashboardStats(ctx, user.ID, 0)
	if err != nil {
		t.Fatalf("GetDashboardStats: %v", err)
	}
	if stats.Today.Inbound != 0 || stats.Today.Outbound != 0 {
		t.Errorf("today counts = %+v, want zero", stats.Today)
	}
	if stats.Today.InboundDeltaPct != 0 || stats.Today.OutboundDeltaPct != 0 {
		t.Errorf("delta counts = %+v, want zero (no baseline)", stats.Today)
	}
	if stats.Pending.Count != 0 || stats.Pending.OldestSeconds != 0 {
		t.Errorf("pending = %+v, want zero", stats.Pending)
	}
	if stats.DeliverySuccessPct != 0 {
		t.Errorf("delivery success = %v, want 0 (no deliveries → no ratio)", stats.DeliverySuccessPct)
	}
	if stats.SampleWindowDays != 7 {
		t.Errorf("sample_window_days = %d, want 7", stats.SampleWindowDays)
	}
}

// TestGetDashboardStats_TodayAndDelta: today's counts come from
// usage_summaries; deltas come from today-vs-yesterday. Seeds both
// rows directly to keep the test focused on the read path.
func TestGetDashboardStats_TodayAndDelta(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "today-stats@example.com", "Owner", "google-ts")

	// Today: 100 in / 50 out. Yesterday: 80 in / 50 out.
	// Expected deltas: inbound +25% (100→80 is actually +25% reverse — let
	// me re-check: (100-80)/80 = +25 ✓), outbound 0% (50/50 unchanged).
	_, err := pool.Exec(ctx,
		`INSERT INTO usage_summaries (user_id, bucket_date, inbound_count, outbound_count, total_count)
		 VALUES ($1, current_date, 100, 50, 150),
		        ($1, current_date - 1, 80, 50, 130)`,
		user.ID)
	if err != nil {
		t.Fatalf("seed usage_summaries: %v", err)
	}

	stats, err := store.GetDashboardStats(ctx, user.ID, 0)
	if err != nil {
		t.Fatalf("GetDashboardStats: %v", err)
	}
	if stats.Today.Inbound != 100 {
		t.Errorf("Inbound = %d, want 100", stats.Today.Inbound)
	}
	if stats.Today.Outbound != 50 {
		t.Errorf("Outbound = %d, want 50", stats.Today.Outbound)
	}
	if stats.Today.InboundDeltaPct != 25 {
		t.Errorf("InboundDeltaPct = %d, want 25 (100 vs 80)", stats.Today.InboundDeltaPct)
	}
	if stats.Today.OutboundDeltaPct != 0 {
		t.Errorf("OutboundDeltaPct = %d, want 0 (50 vs 50)", stats.Today.OutboundDeltaPct)
	}
}

// TestGetDashboardStats_NoYesterdayBaseline: delta_pct is 0 when there's
// no yesterday data (avoids divide-by-zero, lets UI hide the arrow).
func TestGetDashboardStats_NoYesterdayBaseline(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "no-base@example.com", "Owner", "google-nb")

	_, err := pool.Exec(ctx,
		`INSERT INTO usage_summaries (user_id, bucket_date, inbound_count, outbound_count, total_count)
		 VALUES ($1, current_date, 42, 7, 49)`,
		user.ID)
	if err != nil {
		t.Fatalf("seed usage_summaries: %v", err)
	}

	stats, err := store.GetDashboardStats(ctx, user.ID, 0)
	if err != nil {
		t.Fatalf("GetDashboardStats: %v", err)
	}
	if stats.Today.Inbound != 42 || stats.Today.Outbound != 7 {
		t.Errorf("today counts: %+v", stats.Today)
	}
	if stats.Today.InboundDeltaPct != 0 || stats.Today.OutboundDeltaPct != 0 {
		t.Errorf("deltas with no baseline = %+v, want 0 to avoid divide-by-zero", stats.Today)
	}
}

// TestGetDashboardStats_Pending: pending count + oldest_seconds come
// from the messages table joined to agent_identities. Asserts both
// the count and that oldest_seconds reflects the *oldest* row (not
// the most recent).
func TestGetDashboardStats_Pending(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "pending-stats@example.com", "Owner", "google-ps2")
	store.ClaimOrCreateDomain(ctx, "ps.example.com", user.ID)
	store.VerifyDomain(ctx, "ps.example.com", user.ID)
	agent, _ := store.CreateAgent(ctx, "bot@ps.example.com", "ps.example.com", "", "https://example.com/wh", "", user.ID)

	// Two pending — one fresh, one ~2h old.
	for i := 0; i < 2; i++ {
		store.CreatePendingOutboundMessage(ctx, agent.ID,
			[]string{"alice@example.com"}, nil, nil,
			fmt.Sprintf("subject-%d", i), "body", "", nil,
			"send", "", "", "", 3600)
	}
	// Backdate the second one to ~2 hours old. created_at and
	// approval_expires_at are both moved so the partial index still
	// considers it pending.
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET created_at = now() - interval '2 hours'
		 WHERE agent_id = $1
		   AND id = (SELECT id FROM messages WHERE agent_id = $1 ORDER BY created_at DESC LIMIT 1)`,
		agent.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	stats, err := store.GetDashboardStats(ctx, user.ID, 0)
	if err != nil {
		t.Fatalf("GetDashboardStats: %v", err)
	}
	if stats.Pending.Count != 2 {
		t.Errorf("Pending.Count = %d, want 2", stats.Pending.Count)
	}
	// Allow some slack for query latency — oldest should be ≥ ~2h.
	if stats.Pending.OldestSeconds < 7000 {
		t.Errorf("Pending.OldestSeconds = %d, want >= 7000 (~2h)", stats.Pending.OldestSeconds)
	}
}

// TestGetDashboardStats_DeliverySuccess: webhook_deliveries success
// ratio over the 7-day window. Pending rows are excluded so a healthy
// queue doesn't pull the percentage down.
func TestGetDashboardStats_DeliverySuccess(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "delivery-stats@example.com", "Owner", "google-ds")
	store.ClaimOrCreateDomain(ctx, "ds.example.com", user.ID)
	store.VerifyDomain(ctx, "ds.example.com", user.ID)
	agent, _ := store.CreateAgent(ctx, "bot@ds.example.com", "ds.example.com", "", "https://example.com/wh", "", user.ID)

	// Seed three outbound messages with three different delivery states.
	// CreateOutboundMessage doesn't auto-create webhook_deliveries; we
	// insert those rows directly to exercise the GetDashboardStats query.
	for i, status := range []string{"delivered", "delivered", "failed"} {
		m, _ := store.CreateOutboundMessage(ctx, agent.ID,
			[]string{"alice@example.com"}, nil, nil,
			fmt.Sprintf("subj-%d", i), "send", "smtp", "", "", nil)
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_deliveries (message_id, status, attempts, last_error, created_at)
			 VALUES ($1, $2, 1, '', now())`,
			m.ID, status)
		if err != nil {
			t.Fatalf("seed webhook_deliveries: %v", err)
		}
	}
	// One pending — must NOT affect the ratio.
	pendingMsg, _ := store.CreateOutboundMessage(ctx, agent.ID,
		[]string{"alice@example.com"}, nil, nil, "pending", "send", "smtp", "", "", nil)
	pool.Exec(ctx,
		`INSERT INTO webhook_deliveries (message_id, status, attempts, last_error, created_at)
		 VALUES ($1, 'pending', 0, '', now())`,
		pendingMsg.ID)

	stats, err := store.GetDashboardStats(ctx, user.ID, 0)
	if err != nil {
		t.Fatalf("GetDashboardStats: %v", err)
	}
	// 2 delivered / 3 finalized = 66.7%
	if stats.DeliverySuccessPct < 66 || stats.DeliverySuccessPct > 67 {
		t.Errorf("DeliverySuccessPct = %v, want ~66.7 (2 delivered / 3 finalized; pending excluded)", stats.DeliverySuccessPct)
	}
}

// --- Dashboard enriched DashboardAgent ---

// TestListAgentsByUser_EnrichedFields: the dashboard's GET /api/dashboard/agents
// must surface per-agent stats (Inbound7d, Outbound7d, PendingCount,
// LastDeliveryAt, WebhookStatus) so the cards can render without
// extra round-trips. Asserts the subqueries produce the right
// counts for a representative mix of activity.
func TestListAgentsByUser_EnrichedFields(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "enriched-agent@example.com", "Owner", "google-enriched")
	store.ClaimOrCreateDomain(ctx, "enriched.example.com", user.ID)
	store.VerifyDomain(ctx, "enriched.example.com", user.ID)
	agent, _ := store.CreateAgent(ctx, "bot@enriched.example.com", "enriched.example.com", "", "https://example.com/wh", "cloud", user.ID)

	// Seed:
	//   2 inbound in last 7d, 1 inbound > 7d old
	//   3 outbound (sent) in last 7d, 1 pending_approval
	//   1 enabled webhook + 1 delivered subscriber delivery (healthy)
	for i := 0; i < 2; i++ {
		store.CreateInboundMessage(ctx, "", agent.ID, "alice@gmail.com", agent.EmailAddress(), "", "in fresh", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})
	}
	old, _ := store.CreateInboundMessage(ctx, "", agent.ID, "old@gmail.com", agent.EmailAddress(), "", "in old", "", "", nil, nil, nil, false, "", nil, nil, nil, identity.InboundScreening{})
	pool.Exec(ctx, `UPDATE messages SET created_at = now() - interval '14 days' WHERE id = $1`, old.ID)

	for i := 0; i < 3; i++ {
		store.CreateOutboundMessage(ctx, agent.ID, []string{"alice@example.com"}, nil, nil, "out", "send", "smtp", "", "", nil)
	}
	pending, _ := store.CreatePendingOutboundMessage(ctx, agent.ID,
		[]string{"bob@example.com"}, nil, nil, "held", "body", "", nil,
		"send", "", "", "", 3600)
	_ = pending

	// An enabled account-wide webhook subscriber with one delivered
	// delivery — the healthy state.
	wh, err := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	store.CreateOutboundMessage(ctx, agent.ID, []string{"alice@example.com"}, nil, nil, "delivered-msg", "send", "webhook", "", "", nil)
	if _, err := pool.Exec(ctx,
		`INSERT INTO webhook_subscriber_deliveries (id, webhook_id, event_type, event_payload, status, attempts, last_attempt_at)
		 VALUES ($1, $2, 'email.sent', '{}'::jsonb, 'delivered', 1, now())`,
		"whd_enriched_ok", wh.ID); err != nil {
		t.Fatalf("seed webhook_subscriber_deliveries: %v", err)
	}

	agents, err := store.ListAgentsByUser(ctx, user.ID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListAgentsByUser: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	a := agents[0]
	if a.Inbound7d != 2 {
		t.Errorf("Inbound7d = %d, want 2 (excludes the 14-day-old row)", a.Inbound7d)
	}
	if a.Outbound7d != 5 {
		// 3 plain "out" + 1 pending + 1 "delivered-msg" = 5 in last 7d.
		// Outbound count includes pending (status=pending_approval) — the
		// pending separately surfaces under PendingCount, but it's still a
		// 7-day outbound event for the activity sparkline.
		t.Errorf("Outbound7d = %d, want 5", a.Outbound7d)
	}
	if a.PendingCount != 1 {
		t.Errorf("PendingCount = %d, want 1", a.PendingCount)
	}
	if a.LastDeliveryAt == nil {
		t.Errorf("LastDeliveryAt should be set (we created 4 sent outbound messages)")
	}
	if a.WebhookStatus != identity.WebhookStatusHealthy {
		t.Errorf("WebhookStatus = %q, want %q (enabled webhook, only delivery is status=delivered)",
			a.WebhookStatus, identity.WebhookStatusHealthy)
	}
}

// TestListAgentsByUser_WebhookFailingOnRecentFailure: a terminally-failed
// subscriber delivery in the last 24h on a webhook serving the agent
// surfaces as webhook_status=failing. Operator-visible signal so the
// dashboard can paint the badge red.
func TestListAgentsByUser_WebhookFailingOnRecentFailure(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "wh-fail@example.com", "Owner", "google-wh-fail")
	store.ClaimOrCreateDomain(ctx, "whfail.example.com", user.ID)
	store.VerifyDomain(ctx, "whfail.example.com", user.ID)
	store.CreateAgent(ctx, "bot@whfail.example.com", "whfail.example.com", "", "", "cloud", user.ID)

	wh, err := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO webhook_subscriber_deliveries (id, webhook_id, event_type, event_payload, status, attempts, last_error, last_attempt_at)
		 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 5, '500 internal', now() - interval '5 minutes')`,
		"whd_recent_fail", wh.ID); err != nil {
		t.Fatalf("seed webhook_subscriber_deliveries: %v", err)
	}

	agents, _ := store.ListAgentsByUser(ctx, user.ID, 0, time.Time{}, "")
	if agents[0].WebhookStatus != identity.WebhookStatusFailing {
		t.Errorf("WebhookStatus = %q, want %q on recent failed delivery",
			agents[0].WebhookStatus, identity.WebhookStatusFailing)
	}
}

// TestListAgentsByUser_OldFailureDoesNotPoisonHealth: failures older
// than 24h don't flip webhook_status to failing. Otherwise a one-off
// blip from last week would forever paint the agent red.
func TestListAgentsByUser_OldFailureDoesNotPoisonHealth(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "wh-old-fail@example.com", "Owner", "google-wh-of")
	store.ClaimOrCreateDomain(ctx, "wholdfail.example.com", user.ID)
	store.VerifyDomain(ctx, "wholdfail.example.com", user.ID)
	store.CreateAgent(ctx, "bot@wholdfail.example.com", "wholdfail.example.com", "", "", "cloud", user.ID)

	wh, err := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO webhook_subscriber_deliveries (id, webhook_id, event_type, event_payload, status, attempts, last_error, created_at, last_attempt_at)
		 VALUES ($1, $2, 'email.received', '{}'::jsonb, 'failed', 5, 'stale', now() - interval '3 days', now() - interval '3 days')`,
		"whd_stale_fail", wh.ID); err != nil {
		t.Fatalf("seed webhook_subscriber_deliveries: %v", err)
	}

	agents, _ := store.ListAgentsByUser(ctx, user.ID, 0, time.Time{}, "")
	if agents[0].WebhookStatus != identity.WebhookStatusHealthy {
		t.Errorf("WebhookStatus = %q, want %q (3-day-old failure shouldn't poison health)",
			agents[0].WebhookStatus, identity.WebhookStatusHealthy)
	}
}

// TestListAgentsByUser_WebhookStatusNoneWithoutSubscriber: an agent with
// no matching webhook subscriber reports webhook_status=none — the state
// the old webhook_healthy bool could not express (it read as "healthy").
// Also covers agent-filter scoping: a webhook pinned to a different agent
// must not count as "configured" for this one.
func TestListAgentsByUser_WebhookStatusNoneWithoutSubscriber(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "wh-none@example.com", "Owner", "google-wh-none")
	store.ClaimOrCreateDomain(ctx, "whnone.example.com", user.ID)
	store.VerifyDomain(ctx, "whnone.example.com", user.ID)
	covered, _ := store.CreateAgent(ctx, "covered@whnone.example.com", "whnone.example.com", "", "", "cloud", user.ID)
	store.CreateAgent(ctx, "bare@whnone.example.com", "whnone.example.com", "", "", "cloud", user.ID)

	// One webhook, filtered to `covered` only.
	if _, err := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"},
		identity.WebhookFilters{AgentIDs: []string{covered.ID}}); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	agents, err := store.ListAgentsByUser(ctx, user.ID, 0, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListAgentsByUser: %v", err)
	}
	byEmail := map[string]string{}
	for _, a := range agents {
		byEmail[a.Email] = a.WebhookStatus
	}
	if got := byEmail["covered@whnone.example.com"]; got != identity.WebhookStatusHealthy {
		t.Errorf("covered agent WebhookStatus = %q, want %q (agent-filtered webhook matches it)",
			got, identity.WebhookStatusHealthy)
	}
	if got := byEmail["bare@whnone.example.com"]; got != identity.WebhookStatusNone {
		t.Errorf("bare agent WebhookStatus = %q, want %q (no webhook matches it)",
			got, identity.WebhookStatusNone)
	}
}

// TestListAgentsByUser_WebhookStatusDisabledStates: a matching webhook that
// is disabled reports `disabled` when turned off by hand and `auto_disabled`
// when the chronic-failure sweep tripped it (auto_disabled_at set) — and an
// enabled matching webhook takes precedence over a disabled one.
func TestListAgentsByUser_WebhookStatusDisabledStates(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "wh-disabled@example.com", "Owner", "google-wh-disabled")
	store.ClaimOrCreateDomain(ctx, "whdis.example.com", user.ID)
	store.VerifyDomain(ctx, "whdis.example.com", user.ID)
	store.CreateAgent(ctx, "bot@whdis.example.com", "whdis.example.com", "", "", "cloud", user.ID)

	wh, err := store.CreateWebhook(ctx, user.ID, "https://example.com/wh", "", []string{"email.received"}, identity.WebhookFilters{})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	status := func() string {
		t.Helper()
		agents, err := store.ListAgentsByUser(ctx, user.ID, 0, time.Time{}, "")
		if err != nil {
			t.Fatalf("ListAgentsByUser: %v", err)
		}
		return agents[0].WebhookStatus
	}

	// Manually disabled: enabled=false, no auto_disabled_at.
	pool.Exec(ctx, `UPDATE webhooks SET enabled = false, auto_disabled_at = NULL WHERE id = $1`, wh.ID)
	if got := status(); got != identity.WebhookStatusDisabled {
		t.Errorf("WebhookStatus = %q, want %q (manually disabled webhook)", got, identity.WebhookStatusDisabled)
	}

	// Auto-disabled by the failure sweep: auto_disabled_at set.
	pool.Exec(ctx, `UPDATE webhooks SET enabled = false, auto_disabled_at = now() WHERE id = $1`, wh.ID)
	if got := status(); got != identity.WebhookStatusAutoDisabled {
		t.Errorf("WebhookStatus = %q, want %q (auto-disabled webhook)", got, identity.WebhookStatusAutoDisabled)
	}

	// A second, enabled webhook outranks the disabled one.
	if _, err := store.CreateWebhook(ctx, user.ID, "https://example.com/wh2", "", []string{"email.received"}, identity.WebhookFilters{}); err != nil {
		t.Fatalf("CreateWebhook 2: %v", err)
	}
	if got := status(); got != identity.WebhookStatusHealthy {
		t.Errorf("WebhookStatus = %q, want %q (enabled webhook outranks disabled)", got, identity.WebhookStatusHealthy)
	}
}

// TestGetDashboardStats_WindowedTotals: requesting ?window=N sums
// inbound + outbound over the last N days from usage_summaries.
// Seeds 4 days of data and asserts the 3-day total drops one row vs
// the 7-day total. Also confirms the window param is echoed back as
// sample_window_days.
func TestGetDashboardStats_WindowedTotals(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "windowed@example.com", "Owner", "google-windowed")

	// Seed usage_summaries for the last 4 days. Today, yesterday, 3d
	// ago, 5d ago. A 3-day window should include the first three; a
	// 7-day window picks up the 4th too.
	for _, row := range []struct {
		daysAgo int
		in, out int
	}{
		{0, 10, 5},  // today
		{1, 20, 8},  // yesterday
		{3, 30, 12}, // 3 days ago
		{5, 40, 16}, // 5 days ago
	} {
		_, err := pool.Exec(ctx,
			`INSERT INTO usage_summaries (user_id, bucket_date, inbound_count, outbound_count, total_count)
			 VALUES ($1, current_date - make_interval(days => $2), $3, $4, $5)`,
			user.ID, row.daysAgo, row.in, row.out, row.in+row.out)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// 3-day window: today + yesterday + 3d-ago. Excludes the 5d-ago row.
	// SQL: bucket_date > current_date - 3 → bucket_date >= current_date - 2.
	// That captures today (0) + yesterday (1) + 2d ago — but our data
	// has nothing 2d ago. 3d-ago is at current_date - 3, which is NOT
	// > current_date - 3. So actually we'd get only today + yesterday.
	// This test pins that boundary explicitly.
	stats, err := store.GetDashboardStats(ctx, user.ID, 3)
	if err != nil {
		t.Fatalf("GetDashboardStats(window=3): %v", err)
	}
	if stats.SampleWindowDays != 3 {
		t.Errorf("SampleWindowDays = %d, want 3", stats.SampleWindowDays)
	}
	if stats.InboundWindow != 30 {
		t.Errorf("3d InboundWindow = %d, want 30 (today 10 + yesterday 20)", stats.InboundWindow)
	}
	if stats.OutboundWindow != 13 {
		t.Errorf("3d OutboundWindow = %d, want 13 (today 5 + yesterday 8)", stats.OutboundWindow)
	}

	// 7-day window: picks up 3d-ago + 5d-ago too.
	stats7, err := store.GetDashboardStats(ctx, user.ID, 7)
	if err != nil {
		t.Fatalf("GetDashboardStats(window=7): %v", err)
	}
	if stats7.InboundWindow != 100 {
		t.Errorf("7d InboundWindow = %d, want 100 (10+20+30+40)", stats7.InboundWindow)
	}
	if stats7.OutboundWindow != 41 {
		t.Errorf("7d OutboundWindow = %d, want 41 (5+8+12+16)", stats7.OutboundWindow)
	}
}

// TestGetDashboardStats_WindowClampingAndDefault: out-of-range or
// missing window values normalize without erroring. 0 → default 7;
// values > 90 clamp to 90.
func TestGetDashboardStats_WindowClampingAndDefault(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()

	user, _ := store.CreateOrGetUser(ctx, "clamp@example.com", "Owner", "google-clamp")

	defaultStats, err := store.GetDashboardStats(ctx, user.ID, 0)
	if err != nil {
		t.Fatalf("GetDashboardStats(0): %v", err)
	}
	if defaultStats.SampleWindowDays != identity.DashboardDefaultWindowDays {
		t.Errorf("0 → SampleWindowDays = %d, want %d", defaultStats.SampleWindowDays, identity.DashboardDefaultWindowDays)
	}

	clampedStats, err := store.GetDashboardStats(ctx, user.ID, 9999)
	if err != nil {
		t.Fatalf("GetDashboardStats(9999): %v", err)
	}
	if clampedStats.SampleWindowDays != identity.DashboardMaxWindowDays {
		t.Errorf("9999 → SampleWindowDays = %d, want %d", clampedStats.SampleWindowDays, identity.DashboardMaxWindowDays)
	}
}
