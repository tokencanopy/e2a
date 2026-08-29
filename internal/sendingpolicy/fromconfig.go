package sendingpolicy

import (
	"fmt"

	"github.com/tokencanopy/e2a/internal/config"
)

// FromConfig assembles the typed runtime policy from the validated config the
// server itself would use.
//
// The schedule fields come from the `sending_ramp` block and the rest from
// `sending_protection`, because sending_ramp stays the custom-domain ramp SSOT
// and duplicating its numbers into a second block is how the two drift apart.
// The result is validated here, so an operator command and the server agree on
// what the config means before either acts on it.
func FromConfig(cfg *config.Config) (RuntimePolicy, error) {
	sp := cfg.SendingProtect

	policy := RuntimePolicy{
		AllCustomerGlobalDailyRecipients: sp.AllCustomerGlobalDailyRecipients,
		BounceMinOutcomes:                sp.BounceMinOutcomes,
		BouncePauseBasisPoints:           sp.BouncePauseBasisPoints,
		BudgetHoldMaxDays:                sp.BudgetHoldMaxDays,
		BudgetMode:                       Mode(sp.BudgetMode),
		ComplaintPauseBasisPoints:        sp.ComplaintPauseBasisPoints,
		CriticalOperationalDailyRecip:    sp.CriticalOperationalDaily,
		DailyUnlimitedPlanCodes:          sp.DailyUnlimitedPlanCodes,
		DefaultAccountDailyRecipients:    sp.DefaultAccountDailyRecipients,
		DetectorIntervalSeconds:          sp.DetectorIntervalSeconds,
		DetectorMode:                     Mode(sp.DetectorMode),
		DetectorWindowDays:               sp.DetectorWindowDays,
		OperatorNoticeRecipientVersion:   sp.OperatorNoticeRecipientVersion,
		ProbationGlobalDailyRecipients:   sp.ProbationGlobalDailyRecipients,

		// Schedule half, owned by sending_ramp.
		RampDays:        cfg.SendingRamp.RampDays,
		RampEnabled:     cfg.SendingRamp.Enabled,
		RampStartDaily:  cfg.SendingRamp.StartDaily,
		RampTargetDaily: cfg.SendingRamp.TargetDaily,

		SendingControlAuditRetentionDays: sp.AuditRetentionDays,
		SendingFeedbackPostAcctRetention: sp.FeedbackPostAccountRetention,
		SharedDomainAccountDailyRecip:    sp.SharedDomainAccountDaily,
		SharedReputationBounceMinOutcome: sp.SharedReputationBounceMin,
		TenantHeaderCanaryAccountIDs:     sp.TenantHeaderCanaryIDs,
		TenantHeaderMode:                 TenantHeaderMode(sp.TenantHeaderMode),
		TenantProvisioningMode:           ToggleMode(sp.TenantProvisioningMode),
		TenantSuppressionSyncMode:        ToggleMode(sp.TenantSuppressionSyncMode),
		ViolationOperationalDailyRecip:   sp.ViolationOperationalDaily,
	}

	if err := policy.Validate(); err != nil {
		return RuntimePolicy{}, fmt.Errorf("sending_protection: %w", err)
	}
	return policy.normalized(), nil
}

// SourceFromConfig returns the validated policy source. An absent value is the
// config source, matching config.Validate: a deployment that never mentions
// sending protection stays on the file it already has.
func SourceFromConfig(cfg *config.Config) (PolicySource, error) {
	raw := cfg.SendingProtect.RuntimePolicySource
	if raw == "" {
		return PolicySourceConfig, nil
	}
	source := PolicySource(raw)
	if !source.valid() {
		return "", fmt.Errorf("sendingpolicy: unknown runtime_policy_source %q", source)
	}
	return source, nil
}
