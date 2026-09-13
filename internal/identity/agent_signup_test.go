package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func signupFixture(t *testing.T) (*identity.Store, context.Context, *identity.User) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	if err := store.EnsureSharedDomain(ctx, "agents.test"); err != nil {
		t.Fatalf("EnsureSharedDomain: %v", err)
	}
	user, err := store.EnsureAgentSignupUser(ctx, "owner@example.test", "Build Bot")
	if err != nil {
		t.Fatalf("EnsureAgentSignupUser: %v", err)
	}
	return store, ctx, user
}

func TestRegisterAgentSignupIsIdempotentAndRotatesKey(t *testing.T) {
	store, ctx, user := signupFixture(t)
	now := time.Now().UTC()
	first, err := store.RegisterAgentSignup(ctx, user.ID, identity.AgentSignupRegistration{
		HumanEmail: "owner@example.test", DisplayName: "Build Bot", NoteToHuman: "Please approve.",
		Harness: "test-runner", SharedDomain: "agents.test", CodeHash: "hash-1", CodeExpiresAt: now.Add(48 * time.Hour), MaxAgents: 3,
	})
	if err != nil {
		t.Fatalf("RegisterAgentSignup(first): %v", err)
	}
	if !first.Created || first.Signup.Status != identity.AgentSignupPending || first.APIKey.PlaintextKey == "" {
		t.Fatalf("first registration = %#v", first)
	}
	if first.Signup.AgentID != "build-bot@agents.test" {
		t.Fatalf("agent id = %q", first.Signup.AgentID)
	}

	second, err := store.RegisterAgentSignup(ctx, user.ID, identity.AgentSignupRegistration{
		HumanEmail: "OWNER@example.test", DisplayName: " Build Bot ", NoteToHuman: "Updated note.",
		Harness: "test-runner", SharedDomain: "agents.test", CodeHash: "hash-2", CodeExpiresAt: now.Add(48 * time.Hour), MaxAgents: 3,
	})
	if err != nil {
		t.Fatalf("RegisterAgentSignup(second): %v", err)
	}
	if second.Created || second.Signup.ID != first.Signup.ID || second.Signup.AgentID != first.Signup.AgentID {
		t.Fatalf("second registration = %#v, first = %#v", second, first)
	}
	if second.APIKey.PlaintextKey == first.APIKey.PlaintextKey {
		t.Fatal("re-signup did not rotate the API key")
	}
	if _, err := store.GetPrincipalByAPIKey(ctx, first.APIKey.PlaintextKey); err == nil {
		t.Fatal("old API key still authenticates after rotation")
	}
	if p, err := store.GetPrincipalByAPIKey(ctx, second.APIKey.PlaintextKey); err != nil || p.AgentID != first.Signup.AgentID || p.Scope != identity.ScopeAgent {
		t.Fatalf("new principal = %#v, err=%v", p, err)
	}
}

func TestVerifyAgentSignupAndReuseReviewPrimitive(t *testing.T) {
	store, ctx, user := signupFixture(t)
	now := time.Now().UTC()
	created, err := store.RegisterAgentSignup(ctx, user.ID, identity.AgentSignupRegistration{
		HumanEmail: "owner@example.test", DisplayName: "Review Bot", SharedDomain: "agents.test",
		CodeHash: "correct", CodeExpiresAt: now.Add(time.Hour), MaxAgents: 3,
	})
	if err != nil {
		t.Fatalf("RegisterAgentSignup: %v", err)
	}

	for i := 0; i < 2; i++ {
		_, err = store.VerifyAgentSignup(ctx, created.Signup.AgentID, "wrong", true, now)
		if !errors.Is(err, identity.ErrAgentSignupCodeInvalid) {
			t.Fatalf("wrong code error = %v", err)
		}
	}
	verified, err := store.VerifyAgentSignup(ctx, created.Signup.AgentID, "correct", true, now)
	if err != nil {
		t.Fatalf("VerifyAgentSignup: %v", err)
	}
	if verified.Status != identity.AgentSignupVerified || !verified.ReviewOutbound {
		t.Fatalf("verified signup = %#v", verified)
	}
	agent, err := store.GetAgentByID(ctx, created.Signup.AgentID)
	if err != nil {
		t.Fatalf("GetAgentByID: %v", err)
	}
	if agent.OutboundPolicy != "allowlist" || agent.OutboundPolicyAction != "review" || len(agent.OutboundAllowlist) != 1 || agent.OutboundAllowlist[0] != "owner@example.test" {
		t.Fatalf("review posture = policy %q action %q allowlist %#v", agent.OutboundPolicy, agent.OutboundPolicyAction, agent.OutboundAllowlist)
	}
}

func TestAgentSignupHumanApproveAndRejectAreOwnerScoped(t *testing.T) {
	store, ctx, user := signupFixture(t)
	now := time.Now().UTC()
	approvedInput, err := store.RegisterAgentSignup(ctx, user.ID, identity.AgentSignupRegistration{
		HumanEmail: user.Email, DisplayName: "Approve Bot", SharedDomain: "agents.test", CodeHash: "a", CodeExpiresAt: now.Add(time.Hour), MaxAgents: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveAgentSignup(ctx, approvedInput.Signup.ID, "intruder@example.test", false, now); !errors.Is(err, identity.ErrAgentSignupNotFound) {
		t.Fatalf("cross-owner approve error = %v", err)
	}
	approved, err := store.ApproveAgentSignup(ctx, approvedInput.Signup.ID, user.Email, false, now)
	if err != nil || approved.Status != identity.AgentSignupVerified {
		t.Fatalf("approve = %#v, err=%v", approved, err)
	}

	rejectedInput, err := store.RegisterAgentSignup(ctx, user.ID, identity.AgentSignupRegistration{
		HumanEmail: user.Email, DisplayName: "Reject Bot", SharedDomain: "agents.test", CodeHash: "b", CodeExpiresAt: now.Add(time.Hour), MaxAgents: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RejectAgentSignup(ctx, rejectedInput.Signup.ID, user.Email, now); err != nil {
		t.Fatalf("RejectAgentSignup: %v", err)
	}
	if _, err := store.GetPrincipalByAPIKey(ctx, rejectedInput.APIKey.PlaintextKey); err == nil {
		t.Fatal("rejected signup key still authenticates")
	}
	if _, err := store.GetAgentByID(ctx, rejectedInput.Signup.AgentID); err == nil {
		t.Fatal("rejected signup agent is still live")
	}
}

func TestConsumeAgentSignupSendEnforcesRecipientAndRollingLimit(t *testing.T) {
	store, ctx, user := signupFixture(t)
	now := time.Now().UTC()
	created, err := store.RegisterAgentSignup(ctx, user.ID, identity.AgentSignupRegistration{
		HumanEmail: user.Email, DisplayName: "Send Bot", SharedDomain: "agents.test", CodeHash: "c", CodeExpiresAt: now.Add(time.Hour), MaxAgents: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConsumeAgentSignupSend(ctx, created.Signup.AgentID, []string{"other@example.test"}, now); !errors.Is(err, identity.ErrAgentSignupPendingVerification) {
		t.Fatalf("disallowed recipient error = %v", err)
	}
	for i := 0; i < identity.AgentSignupSendLimit; i++ {
		if err := store.ConsumeAgentSignupSend(ctx, created.Signup.AgentID, []string{"Owner <OWNER@example.test>"}, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("allowed send %d: %v", i+1, err)
		}
	}
	if err := store.ConsumeAgentSignupSend(ctx, created.Signup.AgentID, []string{user.Email}, now.Add(10*time.Minute)); !errors.Is(err, identity.ErrAgentSignupSendLimit) {
		t.Fatalf("sixth send error = %v", err)
	}
	if err := store.ConsumeAgentSignupSend(ctx, created.Signup.AgentID, []string{user.Email}, now.Add(25*time.Hour)); err != nil {
		t.Fatalf("send after rolling window: %v", err)
	}
}

func TestHumanLoginClaimsOnlyAgentSignupPlaceholder(t *testing.T) {
	store, ctx, placeholder := signupFixture(t)
	claimed, err := store.CreateOrGetUser(ctx, placeholder.Email, "Human Owner", "google-human-owner")
	if err != nil {
		t.Fatalf("CreateOrGetUser claim: %v", err)
	}
	if claimed.ID != placeholder.ID || claimed.GoogleSubject != "google-human-owner" {
		t.Fatalf("claimed user = %#v, placeholder = %#v", claimed, placeholder)
	}

	ordinary, err := store.CreateOrGetUser(ctx, "ordinary@example.test", "Ordinary", "google-ordinary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOrGetUser(ctx, ordinary.Email, "Intruder", "google-other"); err == nil {
		t.Fatal("ordinary account email was merged into a different subject")
	}
}

func TestControlPlaneProvisionClaimsAgentSignupPlaceholder(t *testing.T) {
	store, ctx, placeholder := signupFixture(t)
	claimed, created, err := store.ProvisionUser(ctx, "external-owner", placeholder.Email, "Human Owner", "")
	if err != nil {
		t.Fatalf("ProvisionUser claim: %v", err)
	}
	if created || claimed.ID != placeholder.ID || claimed.GoogleSubject != "bootstrap:external-owner" {
		t.Fatalf("claim = %#v created=%v placeholder=%#v", claimed, created, placeholder)
	}
}
