package apiserver_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/apiserver"
	"github.com/tokencanopy/e2a/internal/domainteardown"
	"github.com/tokencanopy/e2a/internal/httpapi"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhook"
)

// fakeSenderIdentity records SenderIdentityEnqueuer calls so the transactional
// teardown contract can be asserted without SES/River.
type fakeSenderIdentity struct {
	provisionErr   error
	deprovisionErr error
	tryErr         error
	tryUnconfirmed bool
	deprovisioned  []string
	tried          []string
	// onTry observes state at TryDeprovisionNow time (e.g. that the domain row
	// is already committed-deleted when the best-effort provider call runs).
	onTry func()
}

func (f *fakeSenderIdentity) TryDeprovisionNow(_ context.Context, domain string) (bool, error) {
	if f.onTry != nil {
		f.onTry()
	}
	f.tried = append(f.tried, domain)
	return !f.tryUnconfirmed, f.tryErr
}

func (f *fakeSenderIdentity) EnqueueProvision(_ context.Context, _ string) error {
	return nil
}

func (f *fakeSenderIdentity) EnqueueProvisionTx(_ context.Context, _ pgx.Tx, _ string) error {
	return f.provisionErr
}

func (f *fakeSenderIdentity) EnqueueDeprovisionTx(_ context.Context, _ pgx.Tx, domain string) error {
	f.deprovisioned = append(f.deprovisioned, domain)
	return f.deprovisionErr
}

// realParams wires BuildDeps against a real (test) database — the same shape
// the production binary and the contract harness use.
func realParams(t *testing.T) (apiserver.Params, *identity.Store) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	usageStore := usage.NewStore(pool)
	return apiserver.Params{
		API:   &agent.API{},
		Store: store,
		Enforcer: limits.NewEnforcer(limits.NewStore(pool), usageStore, limits.Defaults{
			PlanCode: "apiserver_test", MaxAgents: 100, MaxDomains: 100,
			MaxMessagesMonth: 100, MaxStorageBytes: 1 << 40,
		}, time.Minute),
		UsageStore:      usageStore,
		SubscriberStore: webhook.NewSubscriberStore(pool),
		Pool:            pool,
		SMTPDomain:      "test.e2a.dev",
	}, store
}

func TestBuildDepsDeleteDomainWithSenderIdentity(t *testing.T) {
	ctx := context.Background()
	p, store := realParams(t)
	fake := &fakeSenderIdentity{}
	p.SenderIdentity = fake
	deps := apiserver.BuildDeps(p)

	user, err := store.CreateOrGetUser(ctx, "cov-del-owner@example.com", "Owner", "google-cov-del")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "cov-del.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}

	rowGoneAtTryTime := false
	fake.onTry = func() {
		_, lookupErr := store.LookupDomain(ctx, domain, user.ID)
		rowGoneAtTryTime = lookupErr != nil
	}

	teardown, err := deps.DeleteDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	if teardown != httpapi.SendingTeardownConfirmed {
		t.Fatalf("teardown = %q, want %q when best-effort deprovision succeeded", teardown, httpapi.SendingTeardownConfirmed)
	}
	if len(fake.deprovisioned) != 1 || fake.deprovisioned[0] != domain {
		t.Fatalf("deprovisioned = %v, want [%s] — durable SES teardown must be enqueued in the delete tx", fake.deprovisioned, domain)
	}
	if len(fake.tried) != 1 || fake.tried[0] != domain {
		t.Fatalf("tried = %v, want [%s] — the best-effort immediate deprovision must run", fake.tried, domain)
	}
	if !rowGoneAtTryTime {
		t.Fatal("best-effort deprovision ran before the delete committed — a provider call inside the tx recreates the SES-coupled delete")
	}
	if _, err := store.LookupDomain(ctx, domain, user.ID); err == nil {
		t.Fatal("domain row still present after DeleteDomain")
	}
}

// TestBuildDepsDeleteDomainSucceedsWhenProviderUnavailable pins the review
// fix for the SES-coupled delete: a transient provider failure (SES outage,
// throttling) or a foreign/untagged identity must NOT fail the API delete.
// The row delete + durable teardown job committed; the provider converges
// asynchronously.
func TestBuildDepsDeleteDomainSucceedsWhenProviderUnavailable(t *testing.T) {
	ctx := context.Background()
	p, store := realParams(t)
	fake := &fakeSenderIdentity{tryErr: errors.New("ses unavailable")}
	p.SenderIdentity = fake
	deps := apiserver.BuildDeps(p)

	user, err := store.CreateOrGetUser(ctx, "cov-sesdown@example.com", "Owner", "google-cov-sesdown")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "cov-sesdown.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}

	teardown, err := deps.DeleteDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("DeleteDomain must succeed while the provider is down, got %v", err)
	}
	if teardown != httpapi.SendingTeardownPending {
		t.Fatalf("teardown = %q, want %q — callers gate DNS removal on this", teardown, httpapi.SendingTeardownPending)
	}
	if _, err := store.LookupDomain(ctx, domain, user.ID); err == nil {
		t.Fatal("domain row still present after DeleteDomain")
	}
	if len(fake.deprovisioned) != 1 {
		t.Fatalf("durable teardown job missing: %v", fake.deprovisioned)
	}
}

func TestBuildDepsDeleteDomainReportsManualReviewWhenOwnershipCannotBeConfirmed(t *testing.T) {
	ctx := context.Background()
	p, store := realParams(t)
	p.SenderIdentity = &fakeSenderIdentity{tryUnconfirmed: true}
	deps := apiserver.BuildDeps(p)

	user, err := store.CreateOrGetUser(ctx, "ownership-drift@example.test", "Owner", "ownership-drift-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "ownership-drift.example.test"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}

	teardown, err := deps.DeleteDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("DeleteDomain must commit despite ownership drift: %v", err)
	}
	if teardown != httpapi.SendingTeardownManualReview {
		t.Fatalf("teardown = %q, want %q", teardown, httpapi.SendingTeardownManualReview)
	}
	if _, err := store.LookupDomain(ctx, domain, user.ID); err == nil {
		t.Fatal("domain row still present after DeleteDomain")
	}
}

func TestBuildDepsDeleteDomainReceiptRecoversLostResponse(t *testing.T) {
	ctx := context.Background()
	p, store := realParams(t)
	p.SenderIdentity = &fakeSenderIdentity{tryErr: errors.New("provider timeout after delete commit")}
	deps := apiserver.BuildDeps(p)

	user, err := store.CreateOrGetUser(ctx, "lost-response@example.test", "Lost Response", "lost-response-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "lost-response.example.test"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}

	state, err := deps.DeleteDomain(ctx, domain, user.ID)
	if err != nil || state != httpapi.SendingTeardownPending {
		t.Fatalf("first delete = %q, %v; want pending", state, err)
	}
	state, err = deps.DeleteDomain(ctx, domain, user.ID)
	if err != nil || state != httpapi.SendingTeardownPending {
		t.Fatalf("repeat delete must return the durable receipt, got %q, %v", state, err)
	}

	if err := store.SetDomainTeardownState(ctx, domain, domainteardown.Confirmed); err != nil {
		t.Fatalf("simulate durable worker confirmation: %v", err)
	}
	state, err = deps.DeleteDomain(ctx, domain, user.ID)
	if err != nil || state != httpapi.SendingTeardownConfirmed {
		t.Fatalf("repeat delete after convergence = %q, %v; want confirmed", state, err)
	}
}

func TestBuildDepsDeleteDomainWithoutProviderDoesNotConfirmManagedIdentity(t *testing.T) {
	ctx := context.Background()
	p, store := realParams(t) // SenderIdentity deliberately nil
	deps := apiserver.BuildDeps(p)

	user, err := store.CreateOrGetUser(ctx, "disabled-provider@example.test", "Disabled Provider", "disabled-provider-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "disabled-provider.example.test"
	d, err := store.ClaimOrCreateDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := store.MarkSendingIdentityManaged(ctx, domain, d.VerificationToken); err != nil {
		t.Fatalf("MarkSendingIdentityManaged: %v", err)
	}

	state, err := deps.DeleteDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	if state != httpapi.SendingTeardownPending {
		t.Fatalf("managed identity with provider disabled = %q, want pending", state)
	}
}

func TestBuildDepsVerifyDomainProvisionJobIsAtomic(t *testing.T) {
	ctx := context.Background()
	p, store := realParams(t)
	hookErr := errors.New("river unavailable")
	p.SenderIdentity = &fakeSenderIdentity{provisionErr: hookErr}
	deps := apiserver.BuildDeps(p)
	user, err := store.CreateOrGetUser(ctx, "verify-outbox@example.com", "Owner", "verify-outbox-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "verify-outbox.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if err := deps.VerifyDomain(ctx, domain, user.ID); !errors.Is(err, hookErr) {
		t.Fatalf("VerifyDomain error = %v, want %v", err, hookErr)
	}
	got, err := store.LookupDomain(ctx, domain, user.ID)
	if err != nil {
		t.Fatalf("LookupDomain: %v", err)
	}
	if got.Verified {
		t.Fatal("verified row committed without its sender-identity outbox job")
	}
}

func TestBuildDepsDeleteDomainHookErrorRollsBack(t *testing.T) {
	ctx := context.Background()
	p, store := realParams(t)
	hookErr := errors.New("ses enqueue failed")
	fake := &fakeSenderIdentity{deprovisionErr: hookErr}
	p.SenderIdentity = fake
	deps := apiserver.BuildDeps(p)

	user, err := store.CreateOrGetUser(ctx, "cov-rb-owner@example.com", "Owner", "google-cov-rb")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "cov-rb.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}

	_, err = deps.DeleteDomain(ctx, domain, user.ID)
	if !errors.Is(err, hookErr) {
		t.Fatalf("DeleteDomain error = %v, want the hook error %v", err, hookErr)
	}
	// Atomicity contract (decision 4): a failed teardown enqueue rolls back
	// the domain delete too — the row must still be there.
	if _, err := store.LookupDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("domain row gone after rolled-back delete: %v", err)
	}
}

func TestBuildDepsDeleteDomainFKFailureDoesNotTouchProvider(t *testing.T) {
	ctx := context.Background()
	p, store := realParams(t)
	fake := &fakeSenderIdentity{}
	p.SenderIdentity = fake
	deps := apiserver.BuildDeps(p)
	user, err := store.CreateOrGetUser(ctx, "delete-race@example.com", "Owner", "delete-race-sub")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "delete-race.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	if _, err := store.CreateAgent(ctx, "bot@"+domain, domain, "Bot", "", "", user.ID); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if _, err := deps.DeleteDomain(ctx, domain, user.ID); !errors.Is(err, identity.ErrDomainHasAgents) {
		t.Fatalf("DeleteDomain error = %v, want ErrDomainHasAgents", err)
	}
	if len(fake.tried) != 0 {
		t.Fatalf("provider touched after an FK-blocked delete: %v", fake.tried)
	}
	if len(fake.deprovisioned) != 0 {
		t.Fatalf("teardown enqueued for an FK-blocked delete: %v", fake.deprovisioned)
	}
}

func TestBuildDepsGetUsageWithRealStores(t *testing.T) {
	ctx := context.Background()
	p, store := realParams(t)
	deps := apiserver.BuildDeps(p)

	user, err := store.CreateOrGetUser(ctx, "cov-usage@example.com", "Owner", "google-cov-usage")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}

	// A fresh user has zero usage on every axis; the closure swallows
	// per-metric errors, so a healthy DB must return exact zeros.
	got := deps.GetUsage(ctx, user.ID)
	if got.Agents != 0 || got.Domains != 0 || got.MessagesMonth != 0 || got.StorageBytes != 0 {
		t.Fatalf("GetUsage = %+v, want all zeros for a fresh user", got)
	}
}

func TestBuildDepsPoolClosuresWithRealStores(t *testing.T) {
	ctx := context.Background()
	p, store := realParams(t)
	deps := apiserver.BuildDeps(p)

	user, err := store.CreateOrGetUser(ctx, "cov-closures@example.com", "Owner", "google-cov-closures")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	const domain = "cov-closures.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}

	t.Run("send ramp snapshot for a claimed domain", func(t *testing.T) {
		snap, err := deps.SendingRampSnapshot(ctx, user.ID, domain, time.Now())
		if err != nil {
			t.Fatalf("SendingRampSnapshot: %v", err)
		}
		if snap.Status == "" {
			t.Fatal("SendingRampSnapshot returned an empty status for an existing domain")
		}
	})

	t.Run("list events for a fresh user is empty", func(t *testing.T) {
		events, err := deps.ListEvents(ctx, httpapi.EventQuery{UserID: user.ID, Limit: 10})
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		if len(events) != 0 {
			t.Fatalf("ListEvents = %d events, want 0 for a fresh user", len(events))
		}
	})

	t.Run("get event with an unknown id errors", func(t *testing.T) {
		if _, err := deps.GetEvent2(ctx, user.ID, "evt_does_not_exist"); err == nil {
			t.Fatal("GetEvent2 with an unknown id returned nil error")
		}
	})

	t.Run("load replay event with an unknown id errors", func(t *testing.T) {
		if _, err := deps.LoadReplayEvent(ctx, user.ID, "evt_does_not_exist"); err == nil {
			t.Fatal("LoadReplayEvent with an unknown id returned nil error")
		}
	})

	t.Run("message lifecycle for an unknown message errors", func(t *testing.T) {
		if _, err := deps.ListMessageLifecycle(ctx, "msg_does_not_exist", "agent@cov-closures.example.com"); err == nil {
			t.Fatal("ListMessageLifecycle for an unknown message returned nil error")
		}
	})
}

func TestNewSmokeWithRealDeps(t *testing.T) {
	p, _ := realParams(t)
	srv := apiserver.New(p)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/openapi.yaml", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/openapi.yaml = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "openapi:") {
		t.Fatalf("expected a YAML OpenAPI document, got: %.200s", rec.Body.String())
	}
}
