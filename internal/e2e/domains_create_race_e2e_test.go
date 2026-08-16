//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// max_domains must be enforced under concurrency: N simultaneous POST
// /v1/domains for distinct new domains on one account, capped at 1, must
// leave exactly one domain claimed (#822). Before the fix, EnforceDomainCreate's
// count read and ClaimDomain's insert were two independent DB calls with
// nothing serializing them, so every concurrent request could read the same
// pre-insert count and all pass the cap.
func TestRegisterDomainConcurrentRequestsRespectMaxDomainsE2E(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool)
	ctx := context.Background()

	user, err := ts.Store.CreateOrGetUser(ctx, "race-owner@example.com", "Race Owner", "google-race-owner")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	apiKey, err := ts.Store.CreateAPIKey(ctx, user.ID, "race-key", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if err := limits.NewStore(pool).Upsert(ctx, user.ID, limits.Limits{
		PlanCode: "test", MaxAgents: 100000, MaxDomains: 1,
		MaxMessagesMonth: 100000, MaxStorageBytes: 1 << 40,
	}); err != nil {
		t.Fatalf("Upsert limits: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			domain := fmt.Sprintf("race-domain-%d.example.com", i)
			body := []byte(`{"domain":"` + domain + `"}`)
			req, err := http.NewRequest("POST", ts.HTTPServer.URL+"/v1/domains", bytes.NewReader(body))
			if err != nil {
				t.Errorf("build request for %s: %v", domain, err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+apiKey.PlaintextKey)
			req.Header.Set("Content-Type", "application/json")
			<-start
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("POST %s: %v", domain, err)
				return
			}
			codes[i] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	close(start)
	wg.Wait()

	var created, rejected int
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusPaymentRequired:
			rejected++
		default:
			t.Errorf("unexpected status code %d", code)
		}
	}
	if created != 1 || rejected != n-1 {
		t.Fatalf("want 1 created and %d rejected (max_domains=1), got created=%d rejected=%d (codes=%v)",
			n-1, created, rejected, codes)
	}

	var domainCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM domains WHERE user_id = $1`, user.ID).Scan(&domainCount); err != nil {
		t.Fatalf("count domains: %v", err)
	}
	if domainCount != 1 {
		t.Fatalf("domains row count = %d, want 1 (max_domains cap was bypassed)", domainCount)
	}
}
