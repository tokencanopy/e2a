//go:build integration

package messagelifecycle_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/messagelifecycle"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// seedAccountAgent adds another agent under an EXISTING user, which is what
// the account rollup actually aggregates over.
func seedAccountAgent(t *testing.T, pool *pgxpool.Pool, userID, slug string) string {
	t.Helper()
	ctx := context.Background()
	agentID := "agt_" + slug
	domain := slug + ".localhost"
	if _, err := pool.Exec(ctx, `
		INSERT INTO domains (domain, user_id, verified, verified_at)
		VALUES ($1, $2, true, now())
	`, domain, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_identities (id, registered_domain, user_id, name)
		VALUES ($1, $2, $3, '')
	`, agentID, domain, userID); err != nil {
		t.Fatal(err)
	}
	return agentID
}

func accountCountsByCode(metrics messagelifecycle.AgentMetrics) map[messagelifecycle.ReasonCode]messagelifecycle.ReasonCodeCount {
	return countsByCode(metrics)
}

// TestAccountMetricsSumsEveryAgentAndExcludesOtherAccounts is the tenancy
// test: totals must span all of the caller's agents and none of anybody else's.
func TestAccountMetricsSumsEveryAgentAndExcludesOtherAccounts(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	ctx := context.Background()

	first := seedMetricsAgent(t, pool, "acctfirst")
	second := seedAccountAgent(t, pool, "usr_acctfirst", "acctsecond")
	foreign := seedMetricsAgent(t, pool, "acctforeign")

	// 2 sends on the first agent, 1 on the second, 3 on a different account.
	for i, agentID := range []string{first, first, second} {
		id := fmt.Sprintf("msg_acct_%d", i)
		seedMetricsMessage(t, pool, id, agentID, "outbound", metricsBaseTime)
		appendMetricsObservation(t, pool, id, "outbound", "accept", messagelifecycle.ReasonAcceptanceOutboundAPI, metricsBaseTime)
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("msg_foreign_%d", i)
		seedMetricsMessage(t, pool, id, foreign, "outbound", metricsBaseTime)
		appendMetricsObservation(t, pool, id, "outbound", "accept", messagelifecycle.ReasonAcceptanceOutboundAPI, metricsBaseTime)
	}

	metrics, err := store.CountByReasonCodeForAccount(ctx, "usr_acctfirst",
		metricsBaseTime.Add(-time.Hour), metricsBaseTime.Add(time.Hour), false)
	if err != nil {
		t.Fatalf("CountByReasonCodeForAccount: %v", err)
	}
	if got := accountCountsByCode(metrics.Totals)[messagelifecycle.ReasonAcceptanceOutboundAPI].Messages; got != 3 {
		t.Errorf("accepted messages = %d, want 3 (2 + 1 across this account's agents only)", got)
	}
	if metrics.Totals.MessagesInWindow != 3 {
		t.Errorf("messages_in_window = %d, want 3: another account's traffic leaked in", metrics.Totals.MessagesInWindow)
	}
	if len(metrics.Agents) != 0 {
		t.Errorf("agents = %d, want 0 without group_by", len(metrics.Agents))
	}
}

// TestAccountMetricsGroupByAgentOrdersByVolume: the breakdown must be busiest
// first, and each agent's slice must carry only its own traffic.
func TestAccountMetricsGroupByAgentOrdersByVolume(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	ctx := context.Background()

	quiet := seedMetricsAgent(t, pool, "grpquiet")
	busy := seedAccountAgent(t, pool, "usr_grpquiet", "grpbusy")

	seedMetricsMessage(t, pool, "msg_grp_quiet", quiet, "outbound", metricsBaseTime)
	appendMetricsObservation(t, pool, "msg_grp_quiet", "outbound", "accept", messagelifecycle.ReasonAcceptanceOutboundAPI, metricsBaseTime)
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("msg_grp_busy_%d", i)
		seedMetricsMessage(t, pool, id, busy, "outbound", metricsBaseTime)
		appendMetricsObservation(t, pool, id, "outbound", "accept", messagelifecycle.ReasonAcceptanceOutboundAPI, metricsBaseTime)
	}

	metrics, err := store.CountByReasonCodeForAccount(ctx, "usr_grpquiet",
		metricsBaseTime.Add(-time.Hour), metricsBaseTime.Add(time.Hour), true)
	if err != nil {
		t.Fatalf("CountByReasonCodeForAccount: %v", err)
	}
	if len(metrics.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(metrics.Agents))
	}
	if metrics.Agents[0].AgentEmail != busy {
		t.Errorf("first agent = %q, want the busiest (%q)", metrics.Agents[0].AgentEmail, busy)
	}
	if got := metrics.Agents[0].Metrics.MessagesInWindow; got != 4 {
		t.Errorf("busy agent messages = %d, want 4", got)
	}
	if got := accountCountsByCode(metrics.Agents[1].Metrics)[messagelifecycle.ReasonAcceptanceOutboundAPI].Messages; got != 1 {
		t.Errorf("quiet agent accepted = %d, want 1: per-agent slices must not share counts", got)
	}
	// Totals stay independent of the breakdown.
	if metrics.Totals.MessagesInWindow != 5 {
		t.Errorf("totals messages_in_window = %d, want 5", metrics.Totals.MessagesInWindow)
	}
	if metrics.AgentsTruncated {
		t.Error("two agents must not report truncation")
	}
}

// TestAccountMetricsTruncatesBreakdownButNotTotals: past the cap the breakdown
// is cut and says so, while the account totals still count every agent —
// otherwise a large account would silently under-report its own volume.
func TestAccountMetricsTruncatesBreakdownButNotTotals(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	ctx := context.Background()

	userID := "usr_trunc"
	seedMetricsAgent(t, pool, "trunc")
	total := messagelifecycle.MaxMetricsAgents + 5
	for i := 1; i < total; i++ {
		seedAccountAgent(t, pool, userID, fmt.Sprintf("trunc%d", i))
	}
	for i := 0; i < total; i++ {
		agentID := "agt_trunc"
		if i > 0 {
			agentID = fmt.Sprintf("agt_trunc%d", i)
		}
		id := fmt.Sprintf("msg_trunc_%d", i)
		seedMetricsMessage(t, pool, id, agentID, "outbound", metricsBaseTime)
		appendMetricsObservation(t, pool, id, "outbound", "accept", messagelifecycle.ReasonAcceptanceOutboundAPI, metricsBaseTime)
	}

	metrics, err := store.CountByReasonCodeForAccount(ctx, userID,
		metricsBaseTime.Add(-time.Hour), metricsBaseTime.Add(time.Hour), true)
	if err != nil {
		t.Fatalf("CountByReasonCodeForAccount: %v", err)
	}
	if len(metrics.Agents) != messagelifecycle.MaxMetricsAgents {
		t.Errorf("agents = %d, want the %d cap", len(metrics.Agents), messagelifecycle.MaxMetricsAgents)
	}
	if !metrics.AgentsTruncated {
		t.Error("agents_truncated must be true once the breakdown is cut")
	}
	if got := metrics.Totals.MessagesInWindow; got != int64(total) {
		t.Errorf("totals messages_in_window = %d, want %d: totals must ignore the breakdown cap", got, total)
	}
	if got := accountCountsByCode(metrics.Totals)[messagelifecycle.ReasonAcceptanceOutboundAPI].Messages; got != int64(total) {
		t.Errorf("totals accepted = %d, want %d", got, total)
	}
}

func TestAccountMetricsRejectsInvalidWindows(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	seedMetricsAgent(t, pool, "acctinvalid")
	ctx := context.Background()

	cases := []struct {
		name       string
		userID     string
		start, end time.Time
		wantSubstr string
	}{
		{"empty user", "", metricsBaseTime, metricsBaseTime.Add(time.Hour), "user id required"},
		{"end equals start", "usr_acctinvalid", metricsBaseTime, metricsBaseTime, "end must be after start"},
		{"window too wide", "usr_acctinvalid", metricsBaseTime, metricsBaseTime.Add(messagelifecycle.MaxMetricsWindow + time.Second), "window exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.CountByReasonCodeForAccount(ctx, tc.userID, tc.start, tc.end, false); err == nil {
				t.Fatal("expected an error")
			} else if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantSubstr)
			}
		})
	}
}
