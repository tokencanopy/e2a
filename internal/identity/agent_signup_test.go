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
	first, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: "owner@example.test", DisplayName: "Build Bot", NoteToHuman: "Please approve.",
		Harness: "test-runner", SharedDomain: "agents.test", CodeHash: "hash-1", CodeExpiresAt: now.Add(48 * time.Hour),
	}, nil)
	if err != nil {
		t.Fatalf("RegisterAgentSignup(first): %v", err)
	}
	if !first.Created || first.Signup.Status != identity.AgentSignupPending || first.APIKey.PlaintextKey == "" {
		t.Fatalf("first registration = %#v", first)
	}
	if first.Signup.AgentID != "build-bot@agents.test" {
		t.Fatalf("agent id = %q", first.Signup.AgentID)
	}
	if first.Signup.UserID == "" || first.Signup.UserID == user.ID {
		t.Fatalf("pending signup owner = %q, must be isolated from human %q", first.Signup.UserID, user.ID)
	}

	if _, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: "owner@example.test", DisplayName: "Build Bot", SharedDomain: "agents.test",
		CodeHash: "hash-2", CodeExpiresAt: now.Add(48 * time.Hour),
	}, nil); !errors.Is(err, identity.ErrAgentSignupResumeRequired) {
		t.Fatalf("unowned replay error = %v", err)
	}
	second, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: "OWNER@example.test", DisplayName: " Build Bot ", NoteToHuman: "Updated note.",
		Harness: "test-runner", SharedDomain: "agents.test", CodeHash: "hash-2", CodeExpiresAt: now.Add(48 * time.Hour), CurrentAPIKey: first.APIKey.PlaintextKey,
	}, nil)
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

func TestRegisterAgentSignupNotificationFailureRollsBack(t *testing.T) {
	store, ctx, _ := signupFixture(t)
	in := identity.AgentSignupRegistration{
		HumanEmail: "rollback@example.test", DisplayName: "Rollback Bot", SharedDomain: "agents.test",
		CodeHash: "hash", CodeExpiresAt: time.Now().Add(time.Hour),
	}
	want := errors.New("mail unavailable")
	if _, err := store.RegisterAgentSignup(ctx, in, func(*identity.AgentSignup) error { return want }); !errors.Is(err, want) {
		t.Fatalf("notification failure = %v", err)
	}
	// A clean retry without current_api_key proves the failed transaction left
	// neither the pair nor a usable credential behind.
	if result, err := store.RegisterAgentSignup(ctx, in, nil); err != nil || !result.Created {
		t.Fatalf("retry after rollback = %#v, err=%v", result, err)
	}
}

func TestVerifyAgentSignupEnforcesTargetPlanCapAtomically(t *testing.T) {
	store, ctx, user := signupFixture(t)
	if _, err := store.CreateAgentWithLimit(ctx, "existing@agents.test", "agents.test", "Existing", user.ID, 0); err != nil {
		t.Fatal(err)
	}
	created, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: user.Email, DisplayName: "Over Cap", SharedDomain: "agents.test",
		CodeHash: "correct", CodeExpiresAt: time.Now().Add(time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.VerifyAgentSignup(ctx, created.Signup.AgentID, "correct", false, time.Now(), func(*identity.AgentSignup) (string, int, error) {
		return user.ID, 1, nil
	})
	var capErr *identity.AgentLimitExceededError
	if !errors.As(err, &capErr) {
		t.Fatalf("verify cap error = %v", err)
	}
	pending, err := store.ListPendingAgentSignups(ctx, user.Email, 10, time.Time{}, "")
	if err != nil || len(pending) != 1 || pending[0].AgentID != created.Signup.AgentID || pending[0].UserID == user.ID {
		t.Fatalf("post-cap signups = %#v, err=%v", pending, err)
	}
}

func TestRegisterAgentSignupNeverClaimsReservedSharedMailbox(t *testing.T) {
	store, ctx, user := signupFixture(t)
	created, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: user.Email, DisplayName: "Postmaster", SharedDomain: "agents.test",
		CodeHash: "hash", CodeExpiresAt: time.Now().Add(time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.Signup.AgentID == "postmaster@agents.test" {
		t.Fatal("public signup claimed the reserved postmaster mailbox")
	}
}

func TestRegisterAgentSignupUsesExistingHumansVerifiedCustomDomain(t *testing.T) {
	store, ctx, user := signupFixture(t)
	if _, err := store.ClaimOrCreateDomain(ctx, "bots.example.test", user.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyDomain(ctx, "bots.example.test", user.ID); err != nil {
		t.Fatal(err)
	}
	created, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: user.Email, DisplayName: "Domain Bot", SharedDomain: "agents.test",
		CodeHash: "hash", CodeExpiresAt: time.Now().Add(time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.Signup.AgentID != "domain-bot@bots.example.test" {
		t.Fatalf("signup inbox = %q, want verified custom domain", created.Signup.AgentID)
	}
	verified, err := store.VerifyAgentSignup(ctx, created.Signup.AgentID, "hash", false, time.Now(), func(*identity.AgentSignup) (string, int, error) {
		return user.ID, 3, nil
	})
	if err != nil || verified.UserID != user.ID {
		t.Fatalf("verify custom-domain signup = %#v, err=%v", verified, err)
	}
}

func TestVerifyAgentSignupAndReuseReviewPrimitive(t *testing.T) {
	store, ctx, user := signupFixture(t)
	now := time.Now().UTC()
	created, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: "owner@example.test", DisplayName: "Review Bot", SharedDomain: "agents.test",
		CodeHash: "correct", CodeExpiresAt: now.Add(time.Hour),
	}, nil)
	if err != nil {
		t.Fatalf("RegisterAgentSignup: %v", err)
	}

	for i := 0; i < 2; i++ {
		_, err = store.VerifyAgentSignup(ctx, created.Signup.AgentID, "wrong", true, now, func(*identity.AgentSignup) (string, int, error) {
			return user.ID, 3, nil
		})
		if !errors.Is(err, identity.ErrAgentSignupCodeInvalid) {
			t.Fatalf("wrong code error = %v", err)
		}
	}
	verified, err := store.VerifyAgentSignup(ctx, created.Signup.AgentID, "correct", true, now, func(*identity.AgentSignup) (string, int, error) {
		return user.ID, 3, nil
	})
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
	if agent.UserID != user.ID {
		t.Fatalf("verified owner = %q, want %q", agent.UserID, user.ID)
	}
}

func TestAgentSignupHumanApproveAndRejectAreOwnerScoped(t *testing.T) {
	store, ctx, user := signupFixture(t)
	now := time.Now().UTC()
	approvedInput, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: user.Email, DisplayName: "Approve Bot", SharedDomain: "agents.test", CodeHash: "a", CodeExpiresAt: now.Add(time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveAgentSignup(ctx, approvedInput.Signup.ID, "intruder@example.test", user.ID, 3, false, now); !errors.Is(err, identity.ErrAgentSignupNotFound) {
		t.Fatalf("cross-owner approve error = %v", err)
	}
	other, err := store.EnsureAgentSignupUser(ctx, "other@example.test", "Other Human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveAgentSignup(ctx, approvedInput.Signup.ID, user.Email, other.ID, 3, false, now); !errors.Is(err, identity.ErrAgentSignupNotFound) {
		t.Fatalf("mismatched target account error = %v", err)
	}
	approved, err := store.ApproveAgentSignup(ctx, approvedInput.Signup.ID, user.Email, user.ID, 3, false, now)
	if err != nil || approved.Status != identity.AgentSignupVerified {
		t.Fatalf("approve = %#v, err=%v", approved, err)
	}

	rejectedInput, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: user.Email, DisplayName: "Reject Bot", SharedDomain: "agents.test", CodeHash: "b", CodeExpiresAt: now.Add(time.Hour),
	}, nil)
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
	created, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: user.Email, DisplayName: "Send Bot", SharedDomain: "agents.test", CodeHash: "c", CodeExpiresAt: now.Add(time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConsumeAgentSignupSend(ctx, created.Signup.AgentID, []string{"other@example.test"}, now); !errors.Is(err, identity.ErrAgentSignupPendingVerification) {
		t.Fatalf("disallowed recipient error = %v", err)
	}
	if _, err := store.CheckAgentSignupSend(ctx, created.Signup.AgentID, []string{user.Email}, true); !errors.Is(err, identity.ErrAgentSignupPendingVerification) {
		t.Fatalf("scheduled provisional send error = %v", err)
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

func TestRejectedAgentSignupSendFailsClosed(t *testing.T) {
	store, ctx, user := signupFixture(t)
	created, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: user.Email, DisplayName: "Rejected Send Bot", SharedDomain: "agents.test",
		CodeHash: "hash", CodeExpiresAt: time.Now().Add(time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RejectAgentSignup(ctx, created.Signup.ID, user.Email, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckAgentSignupSend(ctx, created.Signup.AgentID, []string{user.Email}, false); !errors.Is(err, identity.ErrAgentSignupRejected) {
		t.Fatalf("rejected precheck error = %v", err)
	}
	if err := store.ConsumeAgentSignupSend(ctx, created.Signup.AgentID, []string{user.Email}, time.Now()); !errors.Is(err, identity.ErrAgentSignupRejected) {
		t.Fatalf("rejected consume error = %v", err)
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
