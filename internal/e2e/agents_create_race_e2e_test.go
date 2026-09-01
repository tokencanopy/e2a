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

// max_agents must be enforced under concurrency: N simultaneous POST
// /v1/agents for distinct new addresses on one account, capped at 1, must
// leave exactly one agent created (same class as #822/#901's max_domains
// race). Before the fix, CheckAgentCreate's count read and CreateAgent's
// insert were two independent DB calls with nothing serializing them, so
// every concurrent request could read the same pre-insert count and all pass
// the cap.
func TestCreateAgentConcurrentRequestsRespectMaxAgentsE2E(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool)
	ctx := context.Background()

	user, err := ts.Store.CreateOrGetUser(ctx, "agent-race-owner@example.com", "Agent Race Owner", "google-agent-race-owner")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	apiKey, err := ts.Store.CreateAPIKey(ctx, user.ID, "agent-race-key", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if err := limits.NewStore(pool).Upsert(ctx, user.ID, limits.Limits{
		PlanCode: "test", MaxAgents: 1, MaxDomains: 100000,
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
			// A shared-domain address (testutil's default SharedDomain,
			// agents.e2a.dev) needs no domain ownership/verification, so the
			// only thing under test is the max_agents race.
			body := []byte(fmt.Sprintf(`{"email":"race-agent-%d@agents.e2a.dev","name":"race agent"}`, i))
			req, err := http.NewRequest("POST", ts.HTTPServer.URL+"/v1/agents", bytes.NewReader(body))
			if err != nil {
				t.Errorf("build request %d: %v", i, err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+apiKey.PlaintextKey)
			req.Header.Set("Content-Type", "application/json")
			<-start
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("POST %d: %v", i, err)
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
		t.Fatalf("want 1 created and %d rejected (max_agents=1), got created=%d rejected=%d (codes=%v)",
			n-1, created, rejected, codes)
	}

	var agentCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_identities WHERE user_id = $1 AND deleted_at IS NULL`, user.ID,
	).Scan(&agentCount); err != nil {
		t.Fatalf("count agents: %v", err)
	}
	if agentCount != 1 {
		t.Fatalf("agent_identities row count = %d, want 1 (max_agents cap was bypassed)", agentCount)
	}
}
