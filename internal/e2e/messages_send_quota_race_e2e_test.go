//go:build integration

package e2e_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// sendMessageURL is the immediate-send endpoint WITHOUT ?wait=sent: these
// tests only care about accept-time admission, and the server is built with
// WithManualJobs so nothing drains the outbound queue.
func sendMessageURL(base, agentEmail string) string {
	return base + "/v1/agents/" + agentEmail + "/messages"
}

// quotaMessageBody is a minimal, valid immediate-send request body.
func quotaMessageBody(i int) string {
	return fmt.Sprintf(`{"to":["alice@example.com"],"subject":"quota %d","text":"quota send #%d"}`, i, i)
}

func postQuotaSend(t *testing.T, url, apiKey string, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// Accepted immediate sends must each consume max_messages_month at accept
// time. The accept-path pre-check reads usage_summaries, which is only written
// once a message terminally sends, so without an accept-time reservation the
// counter stays at 0 for every send in flight and the cap admits an unbounded
// number. This is the sequential boundary: cap 2, three sends, the third must
// be refused.
func TestImmediateSendConsumesMonthlyQuotaAtAccept(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithManualJobs())
	user, key, agent := setupDomainAndAgent(t, ts, "agent@quota.example.com", "quota.example.com", "", "")
	if err := limits.NewStore(pool).Upsert(context.Background(), user.ID, limits.Limits{
		PlanCode: "test", MaxAgents: 1000, MaxDomains: 1000,
		MaxMessagesMonth: 2, MaxStorageBytes: 1 << 40,
	}); err != nil {
		t.Fatalf("Upsert limits: %v", err)
	}

	url := sendMessageURL(ts.HTTPServer.URL, agent.EmailAddress())
	for i := 1; i <= 2; i++ {
		code, body := postQuotaSend(t, url, key.PlaintextKey, quotaMessageBody(i))
		if code != http.StatusAccepted {
			t.Fatalf("send %d: status=%d body=%s, want 202 (cap not consumed per send)", i, code, body)
		}
	}
	code, body := postQuotaSend(t, url, key.PlaintextKey, quotaMessageBody(3))
	if code != http.StatusPaymentRequired || !strings.Contains(string(body), `"limit_exceeded"`) {
		t.Fatalf("send at the cap: status=%d body=%s, want 402 limit_exceeded", code, body)
	}
	for _, needle := range []string{`"resource":"messages_month"`, `"limit":2`, `"current":2`} {
		if !strings.Contains(string(body), needle) {
			t.Errorf("402 body missing %s: %s", needle, body)
		}
	}
}

// The per-day cap uses the same accept-time reservation, so it must also count
// a send as soon as it is accepted rather than only once it is metered.
func TestImmediateSendConsumesDailyQuotaAtAccept(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithManualJobs())
	user, key, agent := setupDomainAndAgent(t, ts, "agent@daily.example.com", "daily.example.com", "", "")
	one := 1
	if err := limits.NewStore(pool).Upsert(context.Background(), user.ID, limits.Limits{
		PlanCode: "test", MaxAgents: 1000, MaxDomains: 1000,
		MaxMessagesMonth: 1000, MaxMessagesDay: &one, MaxStorageBytes: 1 << 40,
	}); err != nil {
		t.Fatalf("Upsert limits: %v", err)
	}

	url := sendMessageURL(ts.HTTPServer.URL, agent.EmailAddress())
	if code, body := postQuotaSend(t, url, key.PlaintextKey, quotaMessageBody(1)); code != http.StatusAccepted {
		t.Fatalf("first send: status=%d body=%s, want 202", code, body)
	}
	code, body := postQuotaSend(t, url, key.PlaintextKey, quotaMessageBody(2))
	if code != http.StatusPaymentRequired || !strings.Contains(string(body), `"resource":"messages_day"`) {
		t.Fatalf("second send: status=%d body=%s, want 402 messages_day", code, body)
	}
}

// A burst of concurrent immediate sends must not all pass the same
// pre-increment count. With cap 3 and 10 simultaneous sends exactly 3 may be
// accepted, and exactly 3 accepted rows may exist: the rest must be refused
// with 402 at accept.
func TestConcurrentImmediateSendsRespectMonthlyQuota(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithManualJobs())
	user, key, agent := setupDomainAndAgent(t, ts, "agent@burst.example.com", "burst.example.com", "", "")
	if err := limits.NewStore(pool).Upsert(context.Background(), user.ID, limits.Limits{
		PlanCode: "test", MaxAgents: 1000, MaxDomains: 1000,
		MaxMessagesMonth: 3, MaxStorageBytes: 1 << 40,
	}); err != nil {
		t.Fatalf("Upsert limits: %v", err)
	}

	const n = 10
	url := sendMessageURL(ts.HTTPServer.URL, agent.EmailAddress())
	codes := make([]int, n)
	bodies := make([][]byte, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequest("POST", url, strings.NewReader(quotaMessageBody(i)))
			if err != nil {
				t.Errorf("build request %d: %v", i, err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+key.PlaintextKey)
			req.Header.Set("Content-Type", "application/json")
			<-start
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("POST %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			out, _ := io.ReadAll(resp.Body)
			codes[i] = resp.StatusCode
			bodies[i] = out
		}(i)
	}
	close(start)
	wg.Wait()

	var accepted, rejected int
	for i, code := range codes {
		switch code {
		case http.StatusAccepted:
			accepted++
		case http.StatusPaymentRequired:
			rejected++
			if !strings.Contains(string(bodies[i]), `"limit_exceeded"`) {
				t.Errorf("send %d: 402 body without limit_exceeded: %s", i, bodies[i])
			}
		default:
			t.Errorf("send %d: unexpected status %d body=%s", i, code, bodies[i])
		}
	}
	if accepted != 3 || rejected != n-3 {
		t.Fatalf("want 3 accepted and %d rejected (max_messages_month=3), got accepted=%d rejected=%d (codes=%v)",
			n-3, accepted, rejected, codes)
	}

	var acceptedRows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM messages WHERE agent_id = $1 AND direction = 'outbound' AND delivery_status = 'accepted'`,
		agent.ID,
	).Scan(&acceptedRows); err != nil {
		t.Fatalf("count accepted messages: %v", err)
	}
	if acceptedRows != 3 {
		t.Fatalf("accepted message rows = %d, want 3 (max_messages_month was oversold)", acceptedRows)
	}
}
