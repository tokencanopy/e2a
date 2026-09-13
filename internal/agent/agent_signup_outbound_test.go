package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

func TestDeliverOutboundEnforcesPendingSignupAcrossMessageTypes(t *testing.T) {
	api, store, _, _ := setupAsyncAPI(t)
	ctx := context.Background()
	if err := store.EnsureSharedDomain(ctx, "agents.e2a.dev"); err != nil {
		t.Fatal(err)
	}
	user, err := store.EnsureAgentSignupUser(ctx, "owner@example.test", "Guard Bot")
	if err != nil {
		t.Fatal(err)
	}
	registered, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: user.Email, DisplayName: "Guard Bot", SharedDomain: "agents.e2a.dev",
		CodeHash: "hash", CodeExpiresAt: time.Now().Add(time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	agentIdentity, err := store.GetAgentByID(ctx, registered.Signup.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"send", "reply", "forward"} {
		res, outboundErr := api.DeliverOutbound(ctx, user, agentIdentity, outbound.SendRequest{
			To: []string{"other@example.test"}, Subject: kind, Body: "body",
		}, kind, "", nil, nil)
		if res != nil || outboundErr == nil || outboundErr.Status != 403 || outboundErr.Code != "pending_human_verification" {
			t.Fatalf("%s result/error = %#v/%#v", kind, res, outboundErr)
		}
	}
}

func TestDeliverOutboundPendingSignupSendLimit(t *testing.T) {
	api, store, _, _ := setupAsyncAPI(t)
	ctx := context.Background()
	if err := store.EnsureSharedDomain(ctx, "agents.e2a.dev"); err != nil {
		t.Fatal(err)
	}
	user, err := store.EnsureAgentSignupUser(ctx, "owner@example.test", "Quota Bot")
	if err != nil {
		t.Fatal(err)
	}
	registered, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: user.Email, DisplayName: "Quota Bot", SharedDomain: "agents.e2a.dev",
		CodeHash: "hash", CodeExpiresAt: time.Now().Add(time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	agentIdentity, _ := store.GetAgentByID(ctx, registered.Signup.AgentID)
	for i := 0; i < identity.AgentSignupSendLimit; i++ {
		_, outboundErr := api.DeliverOutbound(ctx, user, agentIdentity, outbound.SendRequest{
			To: []string{user.Email}, Subject: "allowed", Body: "body",
		}, "send", "", nil, nil)
		if outboundErr != nil {
			t.Fatalf("allowed send %d: %#v", i+1, outboundErr)
		}
	}
	_, outboundErr := api.DeliverOutbound(ctx, user, agentIdentity, outbound.SendRequest{
		To: []string{user.Email}, Subject: "sixth", Body: "body",
	}, "send", "", nil, nil)
	if outboundErr == nil || outboundErr.Status != 429 || outboundErr.Code != "rate_limited" {
		t.Fatalf("sixth send error = %#v", outboundErr)
	}
	if outboundErr.Details["limit"] != identity.AgentSignupSendLimit {
		t.Fatalf("sixth send details = %#v", outboundErr.Details)
	}
}

func TestSendTestCoreRejectsPendingSignup(t *testing.T) {
	api, store, _, _ := setupAsyncAPI(t)
	ctx := context.Background()
	if err := store.EnsureSharedDomain(ctx, "agents.e2a.dev"); err != nil {
		t.Fatal(err)
	}
	registered, err := store.RegisterAgentSignup(ctx, identity.AgentSignupRegistration{
		HumanEmail: "owner@example.test", DisplayName: "Test Guard Bot", SharedDomain: "agents.e2a.dev",
		CodeHash: "hash", CodeExpiresAt: time.Now().Add(time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	agentIdentity, err := store.GetAgentByID(ctx, registered.Signup.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	result, outboundErr := api.SendTestCore(ctx, agentIdentity)
	if result != nil || outboundErr == nil || outboundErr.Status != 403 || outboundErr.Code != "pending_human_verification" {
		t.Fatalf("test send result/error = %#v/%#v", result, outboundErr)
	}
}
