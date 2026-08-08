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

// TestAccountMetricsExcludesTrashedAgents: a deleted agent leaves the account
// rollup, in both the totals and the per-agent breakdown.
//
// Agent deletion is soft, so without this the rollup keeps aggregating over
// every agent the account has ever owned for the whole 30-day retention
// window. That is both inconsistent with the other account-scoped surfaces
// (quota counts, list, get) and a scaling cliff: past a few thousand
// tombstones the planner drops idx_messages_agent_created and seq-scans the
// whole table. Note this is deliberately NOT symmetric with trashed
// *messages*, which stay counted — see TestCountByReasonCodeIncludesTrashedMessages.
func TestAccountMetricsExcludesTrashedAgents(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	ctx := context.Background()

	live := seedMetricsAgent(t, pool, "accttrashlive")
	doomed := seedAccountAgent(t, pool, "usr_accttrashlive", "accttrashdead")

	// One send each, so an accidental inclusion doubles the total.
	for i, agentID := range []string{live, doomed} {
		id := fmt.Sprintf("msg_accttrash_%d", i)
		seedMetricsMessage(t, pool, id, agentID, "outbound", metricsBaseTime)
		appendMetricsObservation(t, pool, id, "outbound", "accept", messagelifecycle.ReasonAcceptanceOutboundAPI, metricsBaseTime)
	}

	// Trash the second agent, leaving its messages in place — exactly the
	// state a plain (non-permanent) DELETE /v1/agents leaves behind.
	if _, err := pool.Exec(ctx,
		`UPDATE agent_identities SET deleted_at = now() WHERE id = $1`, doomed); err != nil {
		t.Fatal(err)
	}

	metrics, err := store.CountByReasonCodeForAccount(ctx, "usr_accttrashlive",
		metricsBaseTime.Add(-time.Hour), metricsBaseTime.Add(time.Hour), true)
	if err != nil {
		t.Fatalf("CountByReasonCodeForAccount: %v", err)
	}

	if got := accountCountsByCode(metrics.Totals)[messagelifecycle.ReasonAcceptanceOutboundAPI].Messages; got != 1 {
		t.Errorf("accepted messages = %d, want 1: a trashed agent's mail is still in the totals", got)
	}
	if metrics.Totals.MessagesInWindow != 1 {
		t.Errorf("messages_in_window = %d, want 1: a trashed agent's mail is still counted", metrics.Totals.MessagesInWindow)
	}
	if len(metrics.Agents) != 1 {
		t.Fatalf("agents = %d, want 1: the trashed agent still has a breakdown row", len(metrics.Agents))
	}
	if metrics.Agents[0].AgentEmail != live {
		t.Errorf("breakdown agent = %q, want the live agent %q", metrics.Agents[0].AgentEmail, live)
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

// TestCountByDayGapFillsQuietDays: a day with no traffic must come back with
// zeroes, not vanish. A missing day lets a chart draw a straight line across
// it, which reads as steady volume rather than none.
func TestCountByDayGapFillsQuietDays(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	agentID := seedMetricsAgent(t, pool, "bucketgap")
	ctx := context.Background()

	start := metricsBaseTime.Truncate(24 * time.Hour)
	// Traffic on day 0 and day 3 only; days 1 and 2 are silent.
	for _, offset := range []int{0, 3} {
		id := fmt.Sprintf("msg_bucket_%d", offset)
		seedMetricsMessage(t, pool, id, agentID, "outbound", start.AddDate(0, 0, offset).Add(6*time.Hour))
		appendMetricsObservation(t, pool, id, "outbound", "accept",
			messagelifecycle.ReasonAcceptanceOutboundAPI, start.AddDate(0, 0, offset).Add(6*time.Hour))
	}

	buckets, err := store.CountByDayForAccount(ctx, "usr_bucketgap", start, start.AddDate(0, 0, 4))
	if err != nil {
		t.Fatalf("CountByDayForAccount: %v", err)
	}
	if len(buckets) != 4 {
		t.Fatalf("buckets = %d, want 4 contiguous days", len(buckets))
	}
	for i, b := range buckets {
		wantDay := start.AddDate(0, 0, i)
		if !b.Day.Equal(wantDay) {
			t.Errorf("bucket %d day = %s, want %s", i, b.Day, wantDay)
		}
		hasTraffic := len(b.Counts) > 0
		if wantTraffic := i == 0 || i == 3; hasTraffic != wantTraffic {
			t.Errorf("bucket %d traffic = %v, want %v", i, hasTraffic, wantTraffic)
		}
	}
}

// TestCountByDayBucketsSumToTheWindowTotal: the per-day slices must reconcile
// with the totals shown above them, or the chart and the tiles disagree.
func TestCountByDayBucketsSumToTheWindowTotal(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	agentID := seedMetricsAgent(t, pool, "bucketsum")
	ctx := context.Background()

	start := metricsBaseTime.Truncate(24 * time.Hour)
	for i := 0; i < 9; i++ {
		id := fmt.Sprintf("msg_bsum_%d", i)
		at := start.AddDate(0, 0, i%3).Add(time.Duration(i) * time.Hour)
		seedMetricsMessage(t, pool, id, agentID, "outbound", at)
		appendMetricsObservation(t, pool, id, "outbound", "accept", messagelifecycle.ReasonAcceptanceOutboundAPI, at)
	}
	end := start.AddDate(0, 0, 3)

	buckets, err := store.CountByDayForAccount(ctx, "usr_bucketsum", start, end)
	if err != nil {
		t.Fatal(err)
	}
	var summed int64
	for _, b := range buckets {
		for _, c := range b.Counts {
			if c.ReasonCode == messagelifecycle.ReasonAcceptanceOutboundAPI {
				summed += c.Messages
			}
		}
	}
	totals, err := store.CountByReasonCodeForAccount(ctx, "usr_bucketsum", start, end, false)
	if err != nil {
		t.Fatal(err)
	}
	want := accountCountsByCode(totals.Totals)[messagelifecycle.ReasonAcceptanceOutboundAPI].Messages
	if summed != want {
		t.Errorf("buckets sum to %d, window total is %d — the chart would contradict the tiles", summed, want)
	}
	if want != 9 {
		t.Errorf("window total = %d, want 9", want)
	}
}

func TestCountByDayRejectsInvalidWindows(t *testing.T) {
	pool := testutil.TestDB(t)
	store := messagelifecycle.NewStore(pool)
	ctx := context.Background()
	if _, err := store.CountByDayForAccount(ctx, "", metricsBaseTime, metricsBaseTime.Add(time.Hour)); err == nil {
		t.Error("expected an error for an empty user id")
	}
	if _, err := store.CountByDayForAccount(ctx, "usr_x", metricsBaseTime, metricsBaseTime); err == nil {
		t.Error("expected an error when end == start")
	}
	if _, err := store.CountByDayForAccount(ctx, "usr_x", metricsBaseTime,
		metricsBaseTime.Add(messagelifecycle.MaxMetricsWindow+time.Hour)); err == nil {
		t.Error("expected an error for a window past the cap")
	}
}
