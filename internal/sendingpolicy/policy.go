// Package sendingpolicy owns the authoritative sending-protection runtime
// policy: the typed payload, its closed enums and numeric invariants, its RFC
// 8785 canonical form, and the compare-and-swap activation that advances it.
//
// Nothing here enforces anything. Task 2 establishes the authority that later
// slices read; every mode in the generation-zero policy is disabled, and the
// module is deliberately inert until a reviewed activation says otherwise.
package sendingpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"strings"

	"github.com/gowebpki/jcs"
)

// SchemaVersion is the only policy schema this binary understands. A stored
// policy carrying any other version is rejected before it can reach a provider
// authorization decision — an older binary must never silently reinterpret a
// newer operator's payload.
const SchemaVersion = 1

// Mode is a three-state rollout control. The three states are independent per
// control: a policy may run the budget in shadow while the detector is still
// disabled, and vice versa.
type Mode string

const (
	// ModeDisabled computes nothing and blocks nothing.
	ModeDisabled Mode = "disabled"
	// ModeShadow computes the decision and records it, but never denies.
	ModeShadow Mode = "shadow"
	// ModeEnforce computes the decision and acts on it.
	ModeEnforce Mode = "enforce"
)

func (m Mode) valid() bool {
	switch m {
	case ModeDisabled, ModeShadow, ModeEnforce:
		return true
	}
	return false
}

// TenantHeaderMode controls the SES tenant header. Unlike Mode, its middle
// state is a canary over an explicit account list rather than a shadow
// computation — a header is either sent or not; there is nothing to simulate.
type TenantHeaderMode string

const (
	TenantHeaderDisabled TenantHeaderMode = "disabled"
	TenantHeaderCanary   TenantHeaderMode = "canary"
	TenantHeaderEnforce  TenantHeaderMode = "enforce"
)

func (m TenantHeaderMode) valid() bool {
	switch m {
	case TenantHeaderDisabled, TenantHeaderCanary, TenantHeaderEnforce:
		return true
	}
	return false
}

// ToggleMode is a two-state control for operations that have no meaningful
// shadow: provisioning either creates tenants or it does not, and suppression
// sync either writes to the provider or it does not.
type ToggleMode string

const (
	ToggleDisabled ToggleMode = "disabled"
	ToggleEnforce  ToggleMode = "enforce"
)

func (m ToggleMode) valid() bool {
	switch m {
	case ToggleDisabled, ToggleEnforce:
		return true
	}
	return false
}

// PolicySource selects where the runtime policy is read from. Self-hosted
// deployments stay on the config file; the hosted deployment selects the
// database so that activation is an audited CAS rather than a redeploy.
type PolicySource string

const (
	// PolicySourceConfig is the default for every deployment, including
	// self-host, and is what a binary uses before B15 flips the hosted service.
	PolicySourceConfig PolicySource = "config"
	// PolicySourceDatabase reads the singleton policy row.
	PolicySourceDatabase PolicySource = "database"
)

func (s PolicySource) valid() bool {
	switch s {
	case PolicySourceConfig, PolicySourceDatabase:
		return true
	}
	return false
}

// maxBasisPoints bounds a rate threshold below 100%. A threshold of 10,000 bps
// would mean "pause only when every single outcome is a bounce", which is not a
// safety control; requiring < 10,000 keeps the value meaningful.
const maxBasisPoints = 9999

// RuntimePolicy is the whole sending-protection policy as one immutable typed
// value. Field names and JSON keys are load-bearing: the canonical form of this
// struct is hashed, reviewed by a human, and then required by hash at
// activation, so renaming a key is a policy-breaking change.
type RuntimePolicy struct {
	AllCustomerGlobalDailyRecipients int              `json:"all_customer_global_daily_recipients"`
	BounceMinOutcomes                int              `json:"bounce_min_outcomes"`
	BouncePauseBasisPoints           int              `json:"bounce_pause_basis_points"`
	BudgetHoldMaxDays                int              `json:"budget_hold_max_days"`
	BudgetMode                       Mode             `json:"budget_mode"`
	ComplaintPauseBasisPoints        int              `json:"complaint_pause_basis_points"`
	CriticalOperationalDailyRecip    int              `json:"critical_operational_daily_recipients"`
	DailyUnlimitedPlanCodes          []string         `json:"daily_unlimited_plan_codes"`
	DefaultAccountDailyRecipients    int              `json:"default_account_daily_recipients"`
	DetectorIntervalSeconds          int              `json:"detector_interval_seconds"`
	DetectorMode                     Mode             `json:"detector_mode"`
	DetectorWindowDays               int              `json:"detector_window_days"`
	OperatorNoticeRecipientVersion   int              `json:"operator_notice_recipient_version"`
	ProbationGlobalDailyRecipients   int              `json:"probation_global_daily_recipients"`
	RampDays                         int              `json:"ramp_days"`
	RampEnabled                      bool             `json:"ramp_enabled"`
	RampStartDaily                   int              `json:"ramp_start_daily"`
	RampTargetDaily                  int              `json:"ramp_target_daily"`
	SendingControlAuditRetentionDays int              `json:"sending_control_audit_retention_days"`
	SendingFeedbackPostAcctRetention int              `json:"sending_feedback_post_account_retention_days"`
	SharedDomainAccountDailyRecip    int              `json:"shared_domain_account_daily_recipients"`
	SharedReputationBounceMinOutcome int              `json:"shared_reputation_bounce_min_outcomes"`
	TenantHeaderCanaryAccountIDs     []string         `json:"tenant_header_canary_account_ids"`
	TenantHeaderMode                 TenantHeaderMode `json:"tenant_header_mode"`
	TenantProvisioningMode           ToggleMode       `json:"tenant_provisioning_mode"`
	TenantSuppressionSyncMode        ToggleMode       `json:"tenant_suppression_sync_mode"`
	ViolationOperationalDailyRecip   int              `json:"violation_operational_daily_recipients"`
}

// DisabledPolicy returns the generation-zero policy: every control off, every
// numeric bound at its documented default. Migration 112 seeds exactly this
// value, so canonicalizing it must reproduce the hash that migration recorded.
// The keyring and registry tests use that equality as their anchor fixture.
func DisabledPolicy() RuntimePolicy {
	return RuntimePolicy{
		AllCustomerGlobalDailyRecipients: 5000,
		BounceMinOutcomes:                50,
		BouncePauseBasisPoints:           400,
		BudgetHoldMaxDays:                7,
		BudgetMode:                       ModeDisabled,
		ComplaintPauseBasisPoints:        8,
		CriticalOperationalDailyRecip:    100,
		DailyUnlimitedPlanCodes:          []string{"starter", "pro", "scale"},
		DefaultAccountDailyRecipients:    100,
		DetectorIntervalSeconds:          300,
		DetectorMode:                     ModeDisabled,
		DetectorWindowDays:               7,
		OperatorNoticeRecipientVersion:   1,
		ProbationGlobalDailyRecipients:   150,
		RampDays:                         30,
		RampEnabled:                      false,
		RampStartDaily:                   150,
		RampTargetDaily:                  2000,
		SendingControlAuditRetentionDays: 90,
		SendingFeedbackPostAcctRetention: 30,
		SharedDomainAccountDailyRecip:    50,
		SharedReputationBounceMinOutcome: 1,
		TenantHeaderCanaryAccountIDs:     []string{},
		TenantHeaderMode:                 TenantHeaderDisabled,
		TenantProvisioningMode:           ToggleDisabled,
		TenantSuppressionSyncMode:        ToggleDisabled,
		ViolationOperationalDailyRecip:   100,
	}
}

// positiveField pairs a policy value with its JSON key so validation errors
// name the key an operator actually wrote, not the Go field.
type positiveField struct {
	key   string
	value int
}

// Validate enforces every closed enum and numeric invariant. It is called on
// the config-parsed policy at startup and again on any policy read from the
// database, because a row written by a newer binary must not be trusted just
// because it parsed.
func (p RuntimePolicy) Validate() error {
	if !p.BudgetMode.valid() {
		return fmt.Errorf("sendingpolicy: budget_mode must be disabled, shadow, or enforce (got %q)", p.BudgetMode)
	}
	if !p.DetectorMode.valid() {
		return fmt.Errorf("sendingpolicy: detector_mode must be disabled, shadow, or enforce (got %q)", p.DetectorMode)
	}
	if !p.TenantHeaderMode.valid() {
		return fmt.Errorf("sendingpolicy: tenant_header_mode must be disabled, canary, or enforce (got %q)", p.TenantHeaderMode)
	}
	if !p.TenantProvisioningMode.valid() {
		return fmt.Errorf("sendingpolicy: tenant_provisioning_mode must be disabled or enforce (got %q)", p.TenantProvisioningMode)
	}
	if !p.TenantSuppressionSyncMode.valid() {
		return fmt.Errorf("sendingpolicy: tenant_suppression_sync_mode must be disabled or enforce (got %q)", p.TenantSuppressionSyncMode)
	}

	// Every cap, window, and retention is a positive count. Zero is not a
	// permissive default here — a zero daily cap would silently hold all mail,
	// and a zero retention would delete evidence the detector needs.
	for _, f := range []positiveField{
		{"all_customer_global_daily_recipients", p.AllCustomerGlobalDailyRecipients},
		{"bounce_min_outcomes", p.BounceMinOutcomes},
		{"budget_hold_max_days", p.BudgetHoldMaxDays},
		{"critical_operational_daily_recipients", p.CriticalOperationalDailyRecip},
		{"default_account_daily_recipients", p.DefaultAccountDailyRecipients},
		{"detector_interval_seconds", p.DetectorIntervalSeconds},
		{"detector_window_days", p.DetectorWindowDays},
		{"operator_notice_recipient_version", p.OperatorNoticeRecipientVersion},
		{"probation_global_daily_recipients", p.ProbationGlobalDailyRecipients},
		{"ramp_days", p.RampDays},
		{"ramp_start_daily", p.RampStartDaily},
		{"ramp_target_daily", p.RampTargetDaily},
		{"sending_control_audit_retention_days", p.SendingControlAuditRetentionDays},
		{"sending_feedback_post_account_retention_days", p.SendingFeedbackPostAcctRetention},
		{"shared_domain_account_daily_recipients", p.SharedDomainAccountDailyRecip},
		{"shared_reputation_bounce_min_outcomes", p.SharedReputationBounceMinOutcome},
		{"violation_operational_daily_recipients", p.ViolationOperationalDailyRecip},
	} {
		if f.value <= 0 {
			return fmt.Errorf("sendingpolicy: %s must be positive (got %d)", f.key, f.value)
		}
	}

	// Basis points are a rate threshold in (0, 100%). Both bounds matter: 0
	// would pause on the first outcome, 10,000 could never fire.
	for _, f := range []positiveField{
		{"bounce_pause_basis_points", p.BouncePauseBasisPoints},
		{"complaint_pause_basis_points", p.ComplaintPauseBasisPoints},
	} {
		if f.value < 1 || f.value > maxBasisPoints {
			return fmt.Errorf("sendingpolicy: %s must be between 1 and %d (got %d)", f.key, maxBasisPoints, f.value)
		}
	}

	if p.RampTargetDaily < p.RampStartDaily {
		return fmt.Errorf("sendingpolicy: ramp_target_daily (%d) must be >= ramp_start_daily (%d)",
			p.RampTargetDaily, p.RampStartDaily)
	}

	if err := validateCodeSet("daily_unlimited_plan_codes", p.DailyUnlimitedPlanCodes); err != nil {
		return err
	}
	return validateCodeSet("tenant_header_canary_account_ids", p.TenantHeaderCanaryAccountIDs)
}

// validateCodeSet rejects blank and duplicate entries. A blank code would match
// an account whose plan_code was never set; a duplicate makes the reviewed hash
// depend on ordering noise rather than on intent.
func validateCodeSet(key string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("sendingpolicy: %s must not contain a blank entry", key)
		}
		if _, dup := seen[v]; dup {
			return fmt.Errorf("sendingpolicy: %s must not contain duplicate entry %q", key, v)
		}
		seen[v] = struct{}{}
	}
	return nil
}

// normalized returns a copy whose slices are non-nil and independent of the
// caller's backing arrays.
//
// Element ORDER is deliberately preserved. RFC 8785 canonicalizes object key
// order but leaves arrays exactly as written, and migration 112 seeded
// daily_unlimited_plan_codes in tier order (starter, pro, scale) rather than
// sorted. Sorting here would make every computed hash disagree with the seeded
// generation zero, so array order is part of the reviewed policy: reordering a
// list is a policy change and correctly produces a different hash.
//
// The nil case still needs normalizing. A nil slice marshals to JSON null, but
// the migration seeded [] for the empty canary list, and the two must not hash
// differently for the same meaning.
func (p RuntimePolicy) normalized() RuntimePolicy {
	p.DailyUnlimitedPlanCodes = nonNilCopy(p.DailyUnlimitedPlanCodes)
	p.TenantHeaderCanaryAccountIDs = nonNilCopy(p.TenantHeaderCanaryAccountIDs)
	return p
}

// nonNilCopy defends against both a nil slice and later mutation of the
// caller's array, without touching order.
func nonNilCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// canonicalJSON is the single place the pinned RFC 8785 implementation is
// called. Keeping the dependency behind one function means the canonical form
// has exactly one definition, and swapping the implementation is a one-line
// change with the fixtures in policy_test.go as the contract.
func canonicalJSON(raw []byte) ([]byte, error) {
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("sendingpolicy: canonicalize: %w", err)
	}
	return canonical, nil
}

// canonicalPolicyBytes produces the stable byte form of a policy. Everything
// that needs one — hashing, storage, the operator readback — goes through here.
func canonicalPolicyBytes(p RuntimePolicy) ([]byte, error) {
	raw, err := json.Marshal(p.normalized())
	if err != nil {
		return nil, fmt.Errorf("sendingpolicy: marshal policy: %w", err)
	}
	return canonicalJSON(raw)
}

// CanonicalBytes exposes the canonical form for storage and operator readback.
func CanonicalBytes(p RuntimePolicy) ([]byte, error) { return canonicalPolicyBytes(p) }

// Hash returns the lowercase hex SHA-256 of the canonical form. This is the
// value an operator reviews and then passes back as -expected-policy-sha256, so
// it must be derived from exactly the bytes that get stored.
func Hash(p RuntimePolicy) (string, error) {
	canonical, err := canonicalPolicyBytes(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// HashBytes returns the lowercase hex SHA-256 of already-canonical bytes. The
// store uses it to re-derive a stored row's hash without re-canonicalizing, so
// a row whose bytes drifted from its recorded hash is detected rather than
// silently re-blessed.
func HashBytes(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// ParsePolicy decodes and validates a stored or configured policy. Unknown
// fields are rejected: a payload written by a newer binary carrying a control
// this one does not implement must fail closed, not be silently ignored.
func ParsePolicy(raw []byte) (RuntimePolicy, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()

	var p RuntimePolicy
	if err := dec.Decode(&p); err != nil {
		return RuntimePolicy{}, fmt.Errorf("sendingpolicy: decode policy: %w", err)
	}
	if dec.More() {
		return RuntimePolicy{}, fmt.Errorf("sendingpolicy: trailing content after policy object")
	}
	if err := p.Validate(); err != nil {
		return RuntimePolicy{}, err
	}
	return p.normalized(), nil
}
