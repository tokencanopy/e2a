package telemetry

import (
	"strings"
	"testing"
)

func TestSendingBudgetMetricsBoundLabelsAndCountDeferrals(t *testing.T) {
	p := NewProm("")
	for _, scope := range []string{"global_all", "account_daily", "account_shared_daily", "global_probation", "global_critical", "global_violation"} {
		p.SendingBudgetDecision(scope, "allow")
		p.SendingBudgetDecision(scope, "would_hold")
		p.SendingBudgetDecision(scope, "hold")
	}
	p.SendingBudgetDecision("private-account@example.test", "private-message")
	body := scrape(t, p)
	for _, want := range []string{
		`e2a_sending_budget_decisions_total{decision="would_hold",scope="account_shared_daily"} 1`,
		`e2a_sending_budget_deferrals_total{scope="global_violation"} 1`,
		`e2a_sending_budget_decisions_total{decision="other",scope="other"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, "private-") {
		t.Fatal("unbounded label leaked")
	}
}

func TestSendingBudgetSnapshotExportsOnlyGlobalScopes(t *testing.T) {
	p := NewProm("")
	p.SendingBudgetSnapshot(map[string]float64{"global_all": 1.5, "account_daily": 99, "private@example.test": 99}, 7, true, 123)
	body := scrape(t, p)
	for _, want := range []string{`e2a_sending_budget_used_ratio{scope="global_all"} 1.5`, `e2a_sending_budget_used_ratio{scope="global_violation"} 0`, `e2a_sending_protection_policy_generation 7`, `e2a_sending_protection_policy_config_mismatch 1`, `e2a_sending_budget_observation_last_success_timestamp_seconds 123`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "e2a_sending_budget_used_ratio") && (strings.Contains(line, "account_daily") || strings.Contains(line, "private")) {
			t.Fatal(line)
		}
	}
	p.SendingBudgetSnapshot(nil, 0, false, 124)
	if !strings.Contains(scrape(t, p), `e2a_sending_budget_used_ratio{scope="global_all"} 0`) {
		t.Fatal("stale gauge")
	}
}
