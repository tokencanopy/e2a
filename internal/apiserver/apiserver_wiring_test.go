package apiserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhook"
)

// minimalParams mirrors the zero-value wiring the existing package test uses:
// every required dep present as an empty struct so BuildDeps can bind method
// values without a database.
func minimalParams() Params {
	return Params{
		API:             &agent.API{},
		Store:           &identity.Store{},
		Enforcer:        &limits.DBEnforcer{},
		UsageStore:      &usage.Store{},
		SubscriberStore: &webhook.SubscriberStore{},
		Pool:            &pgxpool.Pool{},
	}
}

// fakeSenderIdentity records SenderIdentityEnqueuer calls so tests can assert
// on provisioning behavior without SES/River.
type fakeSenderIdentity struct {
	provisionErr   error
	deprovisionErr error
	provisioned    []string
	deprovisioned  []string
}

func (f *fakeSenderIdentity) EnqueueProvision(_ context.Context, domain string) error {
	f.provisioned = append(f.provisioned, domain)
	return f.provisionErr
}

func (f *fakeSenderIdentity) EnqueueProvisionTx(_ context.Context, _ pgx.Tx, domain string) error {
	f.provisioned = append(f.provisioned, domain)
	return f.provisionErr
}

func (f *fakeSenderIdentity) EnqueueDeprovisionTx(_ context.Context, _ pgx.Tx, domain string) error {
	f.deprovisioned = append(f.deprovisioned, domain)
	return f.deprovisionErr
}

func (f *fakeSenderIdentity) TryDeprovisionNow(_ context.Context, domain string) error {
	f.deprovisioned = append(f.deprovisioned, domain)
	return f.deprovisionErr
}

func TestNewServesOpenAPISpec(t *testing.T) {
	srv := New(minimalParams())

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/openapi.yaml", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/openapi.yaml = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "openapi:") {
		t.Fatalf("expected a YAML OpenAPI document, got: %.200s", rec.Body.String())
	}
}

func TestNewUnknownV1RouteReturnsJSONNotFound(t *testing.T) {
	legacyCalled := false
	p := minimalParams()
	p.Legacy = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		legacyCalled = true
		w.WriteHeader(http.StatusTeapot)
	})
	srv := New(p)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/definitely-not-a-route", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown /v1 route = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("unknown /v1 route Content-Type = %q, want the JSON error envelope", ct)
	}
	if !strings.Contains(rec.Body.String(), "not_found") {
		t.Fatalf("expected not_found code in body, got: %s", rec.Body.String())
	}
	if legacyCalled {
		t.Fatal("unknown /v1 route must NOT fall back to the legacy mux")
	}
}

func TestNewNonV1RouteFallsBackToLegacy(t *testing.T) {
	legacyCalled := false
	p := minimalParams()
	p.Legacy = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		legacyCalled = true
		w.WriteHeader(http.StatusTeapot)
	})
	srv := New(p)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if !legacyCalled {
		t.Fatal("non-/v1 route did not reach the legacy handler")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("non-/v1 route = %d, want the legacy handler's 418", rec.Code)
	}
}

func TestNewNonV1RouteWithoutLegacyIsPlainNotFound(t *testing.T) {
	srv := New(minimalParams()) // no Legacy wired

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-/v1 route without legacy = %d, want 404", rec.Code)
	}
}

func TestBuildDepsPoolGatedClosures(t *testing.T) {
	t.Run("with pool", func(t *testing.T) {
		deps := BuildDeps(minimalParams())
		if deps.SendingRampSnapshot == nil {
			t.Error("BuildDeps did not wire the send-ramp snapshot with a pool present")
		}
		if deps.ListMessageLifecycle == nil {
			t.Error("BuildDeps did not wire the message lifecycle store with a pool present")
		}
	})

	t.Run("without pool", func(t *testing.T) {
		p := minimalParams()
		p.Pool = nil
		deps := BuildDeps(p)
		if deps.SendingRampSnapshot != nil {
			t.Error("BuildDeps wired the send-ramp snapshot without a pool")
		}
		if deps.ListMessageLifecycle != nil {
			t.Error("BuildDeps wired the message lifecycle store without a pool")
		}
	})
}

func TestAttachmentStoreGating(t *testing.T) {
	cases := []struct {
		name    string
		secret  string
		public  string
		wantNil bool
	}{
		{"neither secret nor public URL", "", "", true},
		{"secret only", "s3cret", "", true},
		{"public URL only", "", "https://api.example.com", true},
		{"both secret and public URL", "s3cret", "https://api.example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := minimalParams()
			p.SigningSecret = tc.secret
			p.PublicURL = tc.public
			got := attachmentStore(p)
			if tc.wantNil && got != nil {
				t.Fatal("attachment store wired without both signing secret and public URL")
			}
			if !tc.wantNil && got == nil {
				t.Fatal("attachment store not wired despite signing secret and public URL")
			}
		})
	}
}

func TestEnqueueSenderProvisionFunc(t *testing.T) {
	t.Run("nil sender identity leaves hook unwired", func(t *testing.T) {
		if got := enqueueSenderProvisionFunc(minimalParams()); got != nil {
			t.Fatal("expected a nil provision hook when SES is not configured")
		}
	})

	t.Run("successful enqueue forwards the domain", func(t *testing.T) {
		fake := &fakeSenderIdentity{}
		p := minimalParams()
		p.SenderIdentity = fake
		hook := enqueueSenderProvisionFunc(p)
		if hook == nil {
			t.Fatal("expected a provision hook when SES is configured")
		}
		hook(context.Background(), "example.com")
		if len(fake.provisioned) != 1 || fake.provisioned[0] != "example.com" {
			t.Fatalf("provisioned = %v, want [example.com]", fake.provisioned)
		}
	})

	t.Run("enqueue failure is logged, not propagated", func(t *testing.T) {
		fake := &fakeSenderIdentity{provisionErr: errors.New("river down")}
		p := minimalParams()
		p.SenderIdentity = fake
		hook := enqueueSenderProvisionFunc(p)
		// Best-effort contract: a failed enqueue must not panic or error the
		// verify request; the next POST /verify recovers it.
		hook(context.Background(), "example.com")
		if len(fake.provisioned) != 1 {
			t.Fatalf("provisioned = %v, want one attempt", fake.provisioned)
		}
	})
}

func TestDeleteDomainFuncSelection(t *testing.T) {
	t.Run("without sender identity it is the plain store delete", func(t *testing.T) {
		store := &identity.Store{}
		p := minimalParams()
		p.Store = store
		got := deleteDomainFunc(p)
		if reflect.ValueOf(got).Pointer() != reflect.ValueOf(store.DeleteDomain).Pointer() {
			t.Fatal("without SES, deleteDomainFunc should be exactly Store.DeleteDomain")
		}
	})

	t.Run("with sender identity it wraps the transactional delete", func(t *testing.T) {
		store := &identity.Store{}
		p := minimalParams()
		p.Store = store
		p.SenderIdentity = &fakeSenderIdentity{}
		got := deleteDomainFunc(p)
		if got == nil {
			t.Fatal("expected a delete function when SES is configured")
		}
		if reflect.ValueOf(got).Pointer() == reflect.ValueOf(store.DeleteDomain).Pointer() {
			t.Fatal("with SES, deleteDomainFunc should wrap DeleteDomainTx, not plain DeleteDomain")
		}
	})
}
