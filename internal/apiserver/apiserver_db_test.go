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
	provisionErr    error
	deprovisionErr  error
	deprovisioned   []string
	providerCleaned []string
}

func (f *fakeSenderIdentity) DeprovisionBeforeDelete(ctx context.Context, domain string, deleteFn func(context.Context) error) error {
	f.providerCleaned = append(f.providerCleaned, domain)
	if f.deprovisionErr != nil {
		return f.deprovisionErr
	}
	return deleteFn(ctx)
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

	if err := deps.DeleteDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	if len(fake.deprovisioned) != 1 || fake.deprovisioned[0] != domain {
		t.Fatalf("deprovisioned = %v, want [%s] — SES teardown must be enqueued", fake.deprovisioned, domain)
	}
	if len(fake.providerCleaned) != 1 || fake.providerCleaned[0] != domain {
		t.Fatalf("providerCleaned = %v, want [%s] — 200 must mean SES teardown already completed", fake.providerCleaned, domain)
	}
	if _, err := store.LookupDomain(ctx, domain, user.ID); err == nil {
		t.Fatal("domain row still present after DeleteDomain")
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

	err = deps.DeleteDomain(ctx, domain, user.ID)
	if !errors.Is(err, hookErr) {
		t.Fatalf("DeleteDomain error = %v, want the hook error %v", err, hookErr)
	}
	// Atomicity contract (decision 4): a failed teardown enqueue rolls back
	// the domain delete too — the row must still be there.
	if _, err := store.LookupDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("domain row gone after rolled-back delete: %v", err)
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
