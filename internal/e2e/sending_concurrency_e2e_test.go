//go:build integration

package e2e_test

import (
	"fmt"
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
	var wg sync.WaitGroup
	results := make([][]byte, n)
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i], results[i] = authedJSON(t, "POST", sendURL(ts.HTTPServer.URL, agent.EmailAddress()), key.PlaintextKey,
				fmt.Sprintf(`{"to":["alice@example.com"],"subject":"parallel %d","text":"parallel send #%d"}`, i, i))
		}(i)
	}
	wg.Wait()

	for i := range statuses {
		if statuses[i] != 200 && statuses[i] != 202 {
			t.Errorf("send %d: status=%d body=%s", i, statuses[i], results[i])
		}
		if !strings.Contains(string(results[i]), `"message_id":"msg_`) {
			t.Errorf("send %d: no message id in %s", i, results[i])
		}
	}
}
