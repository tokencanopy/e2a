package agent_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/tokencanopy/e2a/internal/limits"
)

// max_agents must be enforced under concurrency on the OAuth auto-provision
// path the same way it is on the REST create path (#942's
// TestCreateAgentConcurrentRequestsRespectMaxAgentsE2E). Before this fix,
// issueOAuthCodeWithNewAgent's CheckAgentCreate count read and the
// CreateAgentTx insert were two independent steps with nothing serializing
// them across concurrent requests, so N simultaneous consent submissions
// for the same user could all read the same pre-insert count and all pass.
func TestHTTP_Consent_ConcurrentCreateNewRespectsMaxAgents(t *testing.T) {
	f := newConsentFixture(t)
	ctx := context.Background()

	if err := limits.NewStore(f.pool).Upsert(ctx, f.userID, limits.Limits{
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
			_, challenge := newPKCE(t)
			state := fmt.Sprintf("racestate%07d", i)
			form := authorizeParams(challenge, f.clientID, state)
			form.Set("action", "allow")
			form.Set("agent_choice", "create_new")
			form.Set("new_agent_slug", fmt.Sprintf("raceconsentbot%d", i))
			<-start
			resp := f.consentPOST(t, form)
			codes[i] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	close(start)
	wg.Wait()

	var created, rejected int
	for _, code := range codes {
		switch code {
		case http.StatusSeeOther:
			created++
		case http.StatusPaymentRequired:
			rejected++
		default:
			t.Errorf("unexpected status code %d", code)
		}
	}
	if created != 1 || rejected != n-1 {
		t.Fatalf("want 1 created (303) and %d rejected (402) for max_agents=1, got created=%d rejected=%d (codes=%v)",
			n-1, created, rejected, codes)
	}

	var agentCount int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_identities WHERE user_id = $1 AND deleted_at IS NULL`, f.userID,
	).Scan(&agentCount); err != nil {
		t.Fatalf("count agents: %v", err)
	}
	if agentCount != 1 {
		t.Fatalf("agent_identities row count = %d, want 1 (max_agents cap was bypassed)", agentCount)
	}

	var codeCount int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM oauth_auth_codes WHERE user_id = $1`, f.userID,
	).Scan(&codeCount); err != nil {
		t.Fatalf("count auth codes: %v", err)
	}
	if codeCount != 1 {
		t.Fatalf("oauth_auth_codes row count = %d, want 1 (must match the single created agent)", codeCount)
	}
}
