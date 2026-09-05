//go:build integration

package e2e_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/tokencanopy/e2a/internal/testutil"
)

// TestParallelSendsFromOneAgentAllAccept: eight concurrent sends from one
// agent must all be accepted. Each accept transaction inserts the message and
// then prepares its sending operation under the gate; the v1.9.0 staging
// conformance gate caught the two steps deadlocking against each other
// (SQLSTATE 40P01 → 500) when the gate locked the agent FOR UPDATE.
func TestParallelSendsFromOneAgentAllAccept(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool, testutil.WithOutboundSMTP("127.0.0.1", 1025, "test.e2a.dev"))
	_, key, agent := setupDomainAndAgent(t, ts, "agent@conc.example.com", "conc.example.com", "", "")

	const n = 8
	type result struct {
		status int
		body   []byte
		err    error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// No t.Fatal from a worker goroutine: collect and assert after Wait.
			body := fmt.Sprintf(`{"to":["alice@example.com"],"subject":"parallel %d","text":"parallel send #%d"}`, i, i)
			req, err := http.NewRequest("POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), strings.NewReader(body))
			if err != nil {
				results[i].err = err
				return
			}
			req.Header.Set("Authorization", "Bearer "+key.PlaintextKey)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results[i].err = err
				return
			}
			defer resp.Body.Close()
			out, _ := io.ReadAll(resp.Body)
			results[i] = result{status: resp.StatusCode, body: out}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Errorf("send %d: %v", i, r.err)
			continue
		}
		if r.status != 200 && r.status != 202 {
			t.Errorf("send %d: status=%d body=%s", i, r.status, r.body)
		}
		if !strings.Contains(string(r.body), `"message_id":"msg_`) {
			t.Errorf("send %d: no message id in %s", i, r.body)
		}
	}
}
