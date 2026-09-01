package sendingpolicy

import (
	"encoding/json"
	"strings"
	"testing"
)

// generationZeroCanonical is the exact policy JSON migration 112 seeds at
// generation 0, copied byte-for-byte from that file. If this literal and the
// migration ever diverge, a freshly migrated database and a running binary
// disagree about what "disabled" means, so the equality is asserted rather
// than assumed.
const generationZeroCanonical = `{"all_customer_global_daily_recipients":5000,"bounce_min_outcomes":50,"bounce_pause_basis_points":400,"budget_hold_max_days":7,"budget_mode":"disabled","complaint_pause_basis_points":8,"critical_operational_daily_recipients":100,"daily_unlimited_plan_codes":["starter","pro","scale"],"default_account_daily_recipients":100,"detector_interval_seconds":300,"detector_mode":"disabled","detector_window_days":7,"operator_notice_recipient_version":1,"probation_global_daily_recipients":150,"ramp_days":30,"ramp_enabled":false,"ramp_start_daily":150,"ramp_target_daily":2000,"sending_control_audit_retention_days":90,"sending_feedback_post_account_retention_days":30,"shared_domain_account_daily_recipients":50,"shared_reputation_bounce_min_outcomes":1,"tenant_header_canary_account_ids":[],"tenant_header_mode":"disabled","tenant_provisioning_mode":"disabled","tenant_suppression_sync_mode":"disabled","violation_operational_daily_recipients":100}`

// generationZeroSHA256 is the hash migration 112 recorded alongside those bytes.
const generationZeroSHA256 = "198d8cfb3220b6094a3b8dfe13cb0e2ff97c512ad87ae14609e580ae335c9ce6"

// TestDisabledPolicyReproducesMigrationSeed is the anchor for the whole slice.
// Activation requires an operator-reviewed hash, and generation 0 was written
// by a migration rather than by this code, so the canonicalizer has to land on
// exactly those bytes or no CAS from generation 0 can ever be expressed.
func TestDisabledPolicyReproducesMigrationSeed(t *testing.T) {
	canonical, err := canonicalPolicyBytes(DisabledPolicy())
	if err != nil {
		t.Fatalf("canonicalize disabled policy: %v", err)
	}
	if got := string(canonical); got != generationZeroCanonical {
		t.Errorf("canonical bytes drifted from migration 112 seed\n got: %s\nwant: %s", got, generationZeroCanonical)
	}
	hash, err := Hash(DisabledPolicy())
	if err != nil {
		t.Fatalf("hash disabled policy: %v", err)
	}
	if hash != generationZeroSHA256 {
		t.Errorf("hash = %s, migration 112 recorded %s", hash, generationZeroSHA256)
	}
}

// TestDisabledPolicyIsValid guards against a default that cannot be stored.
func TestDisabledPolicyIsValid(t *testing.T) {
	if err := DisabledPolicy().Validate(); err != nil {
		t.Fatalf("generation-zero policy must validate: %v", err)
	}
}

// TestParseRoundTripsGenerationZero proves the stored form decodes back to the
// same typed value, so a binary reading the seeded row agrees with a binary
// that built the policy from config.
func TestPolicyParseRoundTripsGenerationZero(t *testing.T) {
	parsed, err := ParsePolicy([]byte(generationZeroCanonical))
	if err != nil {
		t.Fatalf("parse generation zero: %v", err)
	}
	rehashed, err := Hash(parsed)
	if err != nil {
		t.Fatalf("hash parsed policy: %v", err)
	}
	if rehashed != generationZeroSHA256 {
		t.Errorf("round-trip hash = %s, want %s", rehashed, generationZeroSHA256)
	}
}

// TestModesAreIndependent covers the plan's disabled/shadow/enforce
// independence requirement: turning the budget on must not implicitly turn the
// detector on, and every combination has to validate on its own.
func TestPolicyModesAreIndependent(t *testing.T) {
	modes := []Mode{ModeDisabled, ModeShadow, ModeEnforce}
	for _, budget := range modes {
		for _, detector := range modes {
			p := DisabledPolicy()
			p.BudgetMode = budget
			p.DetectorMode = detector
			if err := p.Validate(); err != nil {
				t.Errorf("budget=%s detector=%s must be a legal combination: %v", budget, detector, err)
			}
		}
	}
}

// TestTenantModesAccepted pins the closed enums for the three tenant controls.
func TestPolicyTenantModesAccepted(t *testing.T) {
	for _, m := range []TenantHeaderMode{TenantHeaderDisabled, TenantHeaderCanary, TenantHeaderEnforce} {
		p := DisabledPolicy()
		p.TenantHeaderMode = m
		if err := p.Validate(); err != nil {
			t.Errorf("tenant_header_mode %q must be legal: %v", m, err)
		}
	}
	for _, m := range []ToggleMode{ToggleDisabled, ToggleEnforce} {
		p := DisabledPolicy()
		p.TenantProvisioningMode = m
		p.TenantSuppressionSyncMode = m
		if err := p.Validate(); err != nil {
			t.Errorf("toggle mode %q must be legal: %v", m, err)
		}
	}
}

// TestValidateRejects is the table of every invariant the plan names. Each case
// mutates exactly one field of an otherwise-valid policy, so a failure points
// at one rule rather than at a soup of them.
func TestPolicyValidateRejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*RuntimePolicy)
		wantKey string
	}{
		{"unknown budget mode", func(p *RuntimePolicy) { p.BudgetMode = "on" }, "budget_mode"},
		{"unknown detector mode", func(p *RuntimePolicy) { p.DetectorMode = "paused" }, "detector_mode"},
		{"shadow is not a tenant header mode", func(p *RuntimePolicy) { p.TenantHeaderMode = "shadow" }, "tenant_header_mode"},
		{"canary is not a provisioning mode", func(p *RuntimePolicy) { p.TenantProvisioningMode = "canary" }, "tenant_provisioning_mode"},
		{"shadow is not a suppression sync mode", func(p *RuntimePolicy) { p.TenantSuppressionSyncMode = "shadow" }, "tenant_suppression_sync_mode"},
		{"empty mode rejected", func(p *RuntimePolicy) { p.BudgetMode = "" }, "budget_mode"},

		{"zero global cap", func(p *RuntimePolicy) { p.AllCustomerGlobalDailyRecipients = 0 }, "all_customer_global_daily_recipients"},
		{"negative default cap", func(p *RuntimePolicy) { p.DefaultAccountDailyRecipients = -1 }, "default_account_daily_recipients"},
		{"zero shared-domain cap", func(p *RuntimePolicy) { p.SharedDomainAccountDailyRecip = 0 }, "shared_domain_account_daily_recipients"},
		{"zero detector interval", func(p *RuntimePolicy) { p.DetectorIntervalSeconds = 0 }, "detector_interval_seconds"},
		{"zero detector window", func(p *RuntimePolicy) { p.DetectorWindowDays = 0 }, "detector_window_days"},
		{"zero audit retention", func(p *RuntimePolicy) { p.SendingControlAuditRetentionDays = 0 }, "sending_control_audit_retention_days"},
		{"zero feedback retention", func(p *RuntimePolicy) { p.SendingFeedbackPostAcctRetention = 0 }, "sending_feedback_post_account_retention_days"},
		{"zero operator notice version", func(p *RuntimePolicy) { p.OperatorNoticeRecipientVersion = 0 }, "operator_notice_recipient_version"},
		{"zero bounce min outcomes", func(p *RuntimePolicy) { p.BounceMinOutcomes = 0 }, "bounce_min_outcomes"},

		{"bounce bps zero", func(p *RuntimePolicy) { p.BouncePauseBasisPoints = 0 }, "bounce_pause_basis_points"},
		{"bounce bps at 100%", func(p *RuntimePolicy) { p.BouncePauseBasisPoints = 10000 }, "bounce_pause_basis_points"},
		{"complaint bps negative", func(p *RuntimePolicy) { p.ComplaintPauseBasisPoints = -5 }, "complaint_pause_basis_points"},
		{"complaint bps above range", func(p *RuntimePolicy) { p.ComplaintPauseBasisPoints = 10001 }, "complaint_pause_basis_points"},

		{"ramp target below start", func(p *RuntimePolicy) { p.RampTargetDaily = p.RampStartDaily - 1 }, "ramp_target_daily"},

		{"blank plan code", func(p *RuntimePolicy) { p.DailyUnlimitedPlanCodes = []string{"pro", "  "} }, "daily_unlimited_plan_codes"},
		{"duplicate plan code", func(p *RuntimePolicy) { p.DailyUnlimitedPlanCodes = []string{"pro", "pro"} }, "daily_unlimited_plan_codes"},
		{"blank canary account", func(p *RuntimePolicy) { p.TenantHeaderCanaryAccountIDs = []string{""} }, "tenant_header_canary_account_ids"},
		{"duplicate canary account", func(p *RuntimePolicy) { p.TenantHeaderCanaryAccountIDs = []string{"acct_1", "acct_1"} }, "tenant_header_canary_account_ids"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := DisabledPolicy()
			tc.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("expected rejection naming %s, got nil", tc.wantKey)
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("error %q should name the offending key %q", err, tc.wantKey)
			}
		})
	}
}

// TestBasisPointBoundsAreInclusive pins the edges the table above only
// approaches, so a later refactor cannot quietly widen the range.
func TestPolicyBasisPointBoundsAreInclusive(t *testing.T) {
	for _, bps := range []int{1, maxBasisPoints} {
		p := DisabledPolicy()
		p.BouncePauseBasisPoints = bps
		p.ComplaintPauseBasisPoints = bps
		if err := p.Validate(); err != nil {
			t.Errorf("%d basis points must be legal: %v", bps, err)
		}
	}
}

// TestParseRejectsUnknownField is the fail-closed rule for version skew: a
// policy written by a newer binary carrying a control this one cannot enforce
// must be refused, never partially applied.
func TestPolicyParseRejectsUnknownField(t *testing.T) {
	withExtra := strings.TrimSuffix(generationZeroCanonical, "}") + `,"future_control_mode":"enforce"}`
	if _, err := ParsePolicy([]byte(withExtra)); err == nil {
		t.Fatal("expected rejection of a policy carrying an unknown control")
	}
}

// TestParseRejectsMalformed covers the remaining decode failures.
func TestPolicyParseRejectsMalformed(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":            ``,
		"not an object":    `["budget_mode"]`,
		"truncated":        `{"budget_mode":`,
		"trailing content": generationZeroCanonical + `{}`,
		"wrong type":       strings.Replace(generationZeroCanonical, `"ramp_days":30`, `"ramp_days":"thirty"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePolicy([]byte(raw)); err == nil {
				t.Fatalf("expected rejection of %s policy", name)
			}
		})
	}
}

// TestParseValidatesNotJustDecodes proves a structurally valid but semantically
// illegal stored row is refused. A database row is not trusted merely because
// it parsed.
func TestPolicyParseValidatesNotJustDecodes(t *testing.T) {
	bad := strings.Replace(generationZeroCanonical, `"bounce_pause_basis_points":400`, `"bounce_pause_basis_points":0`, 1)
	if _, err := ParsePolicy([]byte(bad)); err == nil {
		t.Fatal("expected a stored policy with 0 basis points to be rejected")
	}
}

// TestArrayOrderIsPartOfThePolicy pins RFC 8785's array rule. Object keys are
// canonically ordered; array elements are not. Migration 112 seeded the plan
// codes in tier order rather than sorted, so a canonicalizer that sorted them
// would compute a hash no stored generation could ever match. Reordering a list
// is therefore a real policy change and must produce a different hash.
func TestArrayOrderIsPartOfThePolicy(t *testing.T) {
	seeded := DisabledPolicy() // starter, pro, scale — the seeded order
	reordered := DisabledPolicy()
	reordered.DailyUnlimitedPlanCodes = []string{"pro", "scale", "starter"}

	seededHash, err := Hash(seeded)
	if err != nil {
		t.Fatalf("hash seeded: %v", err)
	}
	reorderedHash, err := Hash(reordered)
	if err != nil {
		t.Fatalf("hash reordered: %v", err)
	}
	if seededHash != generationZeroSHA256 {
		t.Errorf("seeded order must hash to the migration seed, got %s", seededHash)
	}
	if reorderedHash == seededHash {
		t.Error("reordering an array must change the policy hash; canonicalization is sorting arrays it should leave alone")
	}
}

// TestNilAndEmptySetsHashAlike catches the nil-slice trap directly: Go marshals
// a nil slice as null, but migration 112 seeded [], and the two must not
// produce different hashes for the same meaning.
func TestPolicyNilAndEmptySetsHashAlike(t *testing.T) {
	nilled := DisabledPolicy()
	nilled.TenantHeaderCanaryAccountIDs = nil

	hash, err := Hash(nilled)
	if err != nil {
		t.Fatalf("hash nil-set policy: %v", err)
	}
	if hash != generationZeroSHA256 {
		t.Errorf("a nil canary set must hash like the seeded empty set, got %s", hash)
	}
}

// TestCanonicalizationDoesNotMutateInput proves hashing is a pure read of the
// caller's policy. An earlier version of normalized() sorted in place, which
// would have silently reordered a slice the caller still held — and, worse,
// changed the meaning of a policy after an operator had reviewed its hash.
func TestPolicyCanonicalizationDoesNotMutateInput(t *testing.T) {
	codes := []string{"starter", "pro", "scale"}
	p := DisabledPolicy()
	p.DailyUnlimitedPlanCodes = codes

	if _, err := Hash(p); err != nil {
		t.Fatalf("hash: %v", err)
	}
	for i, want := range []string{"starter", "pro", "scale"} {
		if codes[i] != want {
			t.Fatalf("hashing reordered the caller's slice: got %v", codes)
		}
	}
}

// TestCanonicalSortsKeysByUTF16 pins RFC 8785's ordering rule using the
// non-ASCII example from the specification: sorting is by UTF-16 code unit, not
// by Go's byte-wise string order, and the two disagree above U+FFFF.
func TestPolicyCanonicalSortsKeysByUTF16(t *testing.T) {
	input := `{"€":"euro","ü":"umlaut","😂":"emoji","a":"ascii"}`
	canonical, err := canonicalJSON([]byte(input))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	want := "{\"a\":\"ascii\",\"ü\":\"umlaut\",\"€\":\"euro\",\"\U0001f602\":\"emoji\"}"
	if string(canonical) != want {
		t.Errorf("RFC 8785 key order\n got: %s\nwant: %s", canonical, want)
	}
}

// TestCanonicalNormalizesNumbers pins the ECMAScript number serialization RFC
// 8785 requires. These are the spec's own examples; getting them wrong would
// mean two operators reviewing the same policy could compute different hashes.
func TestPolicyCanonicalNormalizesNumbers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"n":4.50}`, `{"n":4.5}`},
		{`{"n":2e-3}`, `{"n":0.002}`},
		{`{"n":1E30}`, `{"n":1e+30}`},
		{`{"n":333333333.33333329}`, `{"n":333333333.3333333}`},
		{`{"n":-0}`, `{"n":0}`},
	} {
		got, err := canonicalJSON([]byte(tc.in))
		if err != nil {
			t.Fatalf("canonicalize %s: %v", tc.in, err)
		}
		if string(got) != tc.want {
			t.Errorf("canonicalize %s = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestHashBytesMatchesHash keeps the two hash entry points in agreement; the
// store re-derives a stored row's hash with HashBytes and must reach the same
// value the activation path computed with Hash.
func TestPolicyHashBytesMatchesHash(t *testing.T) {
	canonical, err := CanonicalBytes(DisabledPolicy())
	if err != nil {
		t.Fatalf("canonical bytes: %v", err)
	}
	viaBytes := HashBytes(canonical)
	viaPolicy, err := Hash(DisabledPolicy())
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if viaBytes != viaPolicy {
		t.Errorf("HashBytes=%s Hash=%s must agree", viaBytes, viaPolicy)
	}
}

// TestGenerationZeroLiteralIsValidJSON is a cheap guard on the fixture itself.
func TestPolicyGenerationZeroLiteralIsValidJSON(t *testing.T) {
	var probe map[string]any
	if err := json.Unmarshal([]byte(generationZeroCanonical), &probe); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if len(probe) != 27 {
		t.Errorf("generation zero should carry 27 controls, got %d", len(probe))
	}
}
