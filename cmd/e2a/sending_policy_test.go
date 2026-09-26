package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// spTestConfig mirrors the generation-zero policy exactly: the ramp block
// carries the hosted 150→2000/30 schedule and every control is disabled, so
// the config-derived hash must equal migration 112's seeded hash. That
// equality is what lets the activation test present a "reviewed" hash it
// computed the same way an operator would.
func spTestConfig() *config.Config {
	return &config.Config{
		SendingRamp: config.SendingRampConfig{
			Enabled:     false,
			StartDaily:  150,
			TargetDaily: 2000,
			RampDays:    30,
		},
		SendingProtect: config.SendingProtectionConfig{
			RuntimePolicySource:              "config",
			BudgetMode:                       "disabled",
			DetectorMode:                     "disabled",
			TenantHeaderMode:                 "disabled",
			TenantProvisioningMode:           "disabled",
			TenantSuppressionSyncMode:        "disabled",
			DefaultAccountDailyRecipients:    100,
			SharedDomainAccountDaily:         50,
			ProbationGlobalDailyRecipients:   150,
			AllCustomerGlobalDailyRecipients: 5000,
			CriticalOperationalDaily:         100,
			ViolationOperationalDaily:        100,
			DailyUnlimitedPlanCodes:          []string{"starter", "pro", "scale"},
			BudgetHoldMaxDays:                7,
			BouncePauseBasisPoints:           400,
			ComplaintPauseBasisPoints:        8,
			BounceMinOutcomes:                50,
			SharedReputationBounceMin:        1,
			DetectorIntervalSeconds:          300,
			DetectorWindowDays:               7,
			AuditRetentionDays:               90,
			FeedbackPostAccountRetention:     30,
			OperatorNoticeRecipientVersion:   1,
		},
	}
}

const (
	spTestCommitmentKey = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI"
	spPolicyOperatorMap = `{"commitment_key":"` + spTestCommitmentKey + `","recipients":{"1":"policy-operator@example.test"}}`
	spTestOperatorMap   = `{"commitment_key":"` + spTestCommitmentKey + `","recipients":{"910001":"cmd-operator@example.test"}}`
)

func spBillingDigest(hexDigit string) string {
	return "sha256:" + strings.Repeat(hexDigit, 64)
}

func TestSendingProtectionCommands(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	cfg := spTestConfig()
	module := sendingpolicy.NewModule(pool, sendingpolicy.Secrets{})

	run := func(f *sendingProtectionFlags) (string, error) {
		var out bytes.Buffer
		policy, err := sendingpolicy.FromConfig(cfg)
		if err != nil {
			return "", err
		}
		source, err := sendingpolicy.SourceFromConfig(cfg)
		if err != nil {
			return "", err
		}
		secrets, err := sendingpolicy.LoadSecretsFromEnv(source, policy)
		if err != nil {
			return "", err
		}
		err = runSendingProtectionCommand(ctx, cfg, pool, secrets, f, &out)
		return out.String(), err
	}

	t.Run("exactly one command", func(t *testing.T) {
		if _, err := run(&sendingProtectionFlags{}); err == nil {
			t.Fatal("no command selected must be rejected")
		}
		if _, err := run(&sendingProtectionFlags{inspect: true, capabilities: true}); err == nil {
			t.Fatal("two commands must be rejected")
		}
	})

	// The dry-run readback drives everything else: it tells the operator the
	// stored generation and the hash this config would activate.
	var configHash string
	t.Run("inspect", func(t *testing.T) {
		out, err := run(&sendingProtectionFlags{inspect: true})
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		for _, want := range []string{
			"stored_generation:",
			"config_policy_sha256:",
			"config_policy_canonical:",
			"runtime_attestation_revision:",
			"runtime_attestation_sha256:",
			"active_billing_digest:",
			"active_billing_contract:",
			"rollback_billing_digest:",
			"rollback_billing_contract:",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("inspect output missing %q", want)
			}
		}
		policy, err := sendingpolicy.FromConfig(cfg)
		if err != nil {
			t.Fatalf("policy from config: %v", err)
		}
		configHash, err = sendingpolicy.Hash(policy)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		if !strings.Contains(out, configHash) {
			t.Errorf("inspect must print the config policy hash %s", configHash)
		}
	})

	t.Run("activate requires explicit arguments", func(t *testing.T) {
		for name, f := range map[string]*sendingProtectionFlags{
			"missing reason":     {activate: true, expectedGeneration: 0, expectedPolicySHA: configHash},
			"missing generation": {activate: true, expectedGeneration: -1, expectedPolicySHA: configHash, reason: "r"},
			"missing hash":       {activate: true, expectedGeneration: 0, reason: "r"},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := run(f); err == nil {
					t.Fatal("expected rejection")
				}
			})
		}
	})

	t.Run("invalid reviewed hashes are rejected without echoing input", func(t *testing.T) {
		injected := "not-a-hash\nforged-log-line"
		_, err := run(&sendingProtectionFlags{
			activate:           true,
			expectedGeneration: 0,
			expectedPolicySHA:  injected,
			reason:             "invalid hash",
		})
		if err == nil {
			t.Fatal("expected malformed policy hash to be rejected")
		}
		if strings.Contains(err.Error(), "forged-log-line") {
			t.Errorf("policy hash error echoed untrusted input: %v", err)
		}

		_, err = run(&sendingProtectionFlags{
			attest:                      true,
			expectedAttestationRevision: 0,
			expectedAttestationSHA:      injected,
			activeBillingContract:       0,
			rollbackBillingContract:     0,
			reason:                      "invalid hash",
		})
		if err == nil {
			t.Fatal("expected malformed attestation hash to be rejected")
		}
		if strings.Contains(err.Error(), "forged-log-line") {
			t.Errorf("attestation hash error echoed untrusted input: %v", err)
		}
	})

	t.Run("activate rejects a wrong reviewed hash with zero writes", func(t *testing.T) {
		t.Setenv(sendingpolicy.EnvOperatorRecipientsMap, spPolicyOperatorMap)
		before, err := module.InspectPolicy(ctx)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		_, err = run(&sendingProtectionFlags{
			activate:           true,
			expectedGeneration: before.Generation,
			expectedPolicySHA:  strings.Repeat("0", 64),
			reason:             "wrong hash",
		})
		if err == nil || !strings.Contains(err.Error(), "zero writes") {
			t.Fatalf("expected the reviewed-hash mismatch, got %v", err)
		}
		after, err := module.InspectPolicy(ctx)
		if err != nil {
			t.Fatalf("re-inspect: %v", err)
		}
		if after.Generation != before.Generation {
			t.Error("a rejected activation advanced the generation")
		}
	})

	t.Run("activate with the reviewed hash advances one generation", func(t *testing.T) {
		t.Setenv(sendingpolicy.EnvOperatorRecipientsMap, spPolicyOperatorMap)
		recipients, err := sendingpolicy.LoadOperatorRecipients(spPolicyOperatorMap)
		if err != nil {
			t.Fatalf("load policy operator map: %v", err)
		}
		activationModule := sendingpolicy.NewModule(pool, sendingpolicy.Secrets{Recipients: recipients})
		if _, err := activationModule.RegisterOperatorRecipients(ctx, "cmd-test-bootstrap", "register selected policy operator"); err != nil {
			t.Fatalf("register selected policy operator: %v", err)
		}
		before, err := module.InspectPolicy(ctx)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		out, err := run(&sendingProtectionFlags{
			activate:           true,
			expectedGeneration: before.Generation,
			expectedPolicySHA:  strings.ToUpper(configHash), // case-insensitive review input
			reason:             "cmd-test activation",
		})
		if err != nil {
			t.Fatalf("activate: %v", err)
		}
		if !strings.Contains(out, "activated_generation:") {
			t.Errorf("activation output missing the new generation: %s", out)
		}
		after, err := module.InspectPolicy(ctx)
		if err != nil {
			t.Fatalf("re-inspect: %v", err)
		}
		if after.Generation != before.Generation+1 {
			t.Errorf("generation = %d, want %d", after.Generation, before.Generation+1)
		}
		if after.PolicySHA256 != configHash {
			t.Errorf("stored hash = %s, want the reviewed %s", after.PolicySHA256, configHash)
		}
	})

	t.Run("register is gated on the env map and idempotent", func(t *testing.T) {
		if _, err := run(&sendingProtectionFlags{register: true}); err == nil {
			t.Fatal("register without a reason must be rejected")
		}
		clearEnvForTest(t)
		if _, err := run(&sendingProtectionFlags{register: true, reason: "r"}); err == nil {
			t.Fatal("register without the env map must be rejected")
		}

		t.Setenv(sendingpolicy.EnvOperatorRecipientsMap, spTestOperatorMap)
		out, err := run(&sendingProtectionFlags{register: true, reason: "initial synthetic registry"})
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		if !strings.Contains(out, "910001") || strings.Contains(out, "cmd-operator@") {
			t.Errorf("register must print the version and never the mailbox: %s", out)
		}

		replay, err := run(&sendingProtectionFlags{register: true, reason: "replay"})
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if !strings.Contains(replay, "idempotent replay") {
			t.Errorf("replay output should say it wrote nothing: %s", replay)
		}
	})

	t.Run("attest CAS round-trip", func(t *testing.T) {
		current, err := module.InspectAttestation(ctx)
		if err != nil {
			t.Fatalf("inspect attestation: %v", err)
		}
		currentHash, err := sendingpolicy.AttestationHash(current)
		if err != nil {
			t.Fatalf("hash attestation: %v", err)
		}

		if _, err := run(&sendingProtectionFlags{
			attest:                      true,
			expectedAttestationRevision: current.Revision,
			expectedAttestationSHA:      currentHash,
			activeBillingContract:       -1, // missing contracts must be rejected
			rollbackBillingContract:     -1,
			reason:                      "missing contracts",
		}); err == nil {
			t.Fatal("attest without explicit contracts must be rejected")
		}

		out, err := run(&sendingProtectionFlags{
			attest:                      true,
			expectedAttestationRevision: current.Revision,
			expectedAttestationSHA:      currentHash,
			activeBillingDigest:         spBillingDigest("a"),
			activeBillingContract:       1,
			rollbackBillingDigest:       spBillingDigest("b"),
			rollbackBillingContract:     1,
			reason:                      "cmd-test attestation",
		})
		if err != nil {
			t.Fatalf("attest: %v", err)
		}
		if !strings.Contains(out, "attestation_revision:") || !strings.Contains(out, "attestation_sha256:") {
			t.Errorf("attest output missing revision/hash: %s", out)
		}
		after, err := module.InspectAttestation(ctx)
		if err != nil {
			t.Fatalf("re-inspect: %v", err)
		}
		if after.Revision != current.Revision+1 {
			t.Errorf("revision = %d, want %d", after.Revision, current.Revision+1)
		}
	})

	t.Run("reconcile-legacy-sending-jobs dispatches", func(t *testing.T) {
		resetRiverJobs(t, pool)
		out, err := run(&sendingProtectionFlags{reconcile: true})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if !strings.Contains(out, "scanned:   0") || !strings.Contains(out, "remaining: 0") {
			t.Errorf("reconcile output = %q", out)
		}
	})

	t.Run("print-capabilities", func(t *testing.T) {
		clearEnvForTest(t)
		out, err := run(&sendingProtectionFlags{capabilities: true})
		if err != nil {
			t.Fatalf("capabilities: %v", err)
		}
		for _, want := range []string{`"sending_protection_contract":0`, `"runtime_policy_source":"config"`, `"operator_notice_recipient_commitments":{}`, `"runtime_policy_features":["external_sending_access"]`} {
			if !strings.Contains(out, want) {
				t.Errorf("capabilities missing %s in %s", want, out)
			}
		}

		t.Setenv(sendingpolicy.EnvOperatorRecipientsMap, spTestOperatorMap)
		withMap, err := run(&sendingProtectionFlags{capabilities: true})
		if err != nil {
			t.Fatalf("capabilities with map: %v", err)
		}
		if !strings.Contains(withMap, `"910001":"`) {
			t.Errorf("capabilities should carry the commitment for version 910001: %s", withMap)
		}
		if strings.Contains(withMap, "example.test") || strings.Contains(withMap, spTestCommitmentKey) {
			t.Error("capabilities must never carry an address or key material")
		}
	})
}

// clearEnvForTest unsets both secret variables for the enclosing test,
// restoring whatever the shell carried afterwards.
func clearEnvForTest(t *testing.T) {
	t.Helper()
	for _, key := range []string{sendingpolicy.EnvFeedbackHMACKeys, sendingpolicy.EnvOperatorRecipientsMap} {
		t.Setenv(key, "placeholder")
		if err := unsetEnvKey(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
}

func unsetEnvKey(key string) error { return os.Unsetenv(key) }

func TestExternalSendingOperatorCommands(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	cfg := spTestConfig()
	cfg.SendingProtect.ExternalSendingAccess = &config.ExternalSendingAccessConfig{Mode: "enforce", AccountsCreatedAtOrAfter: "2026-01-01T00:00:00Z", PaidPlanCodes: []string{"paid_tier_test"}}
	clearEnvForTest(t)
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, google_subject) VALUES ('usr_cmd_esa', 'owner@cmd-esa.example.test', 'sub-cmd-esa')`); err != nil {
		t.Fatal(err)
	}
	run := func(f *sendingProtectionFlags) (string, error) {
		var out bytes.Buffer
		err := runSendingProtectionCommand(ctx, cfg, pool, sendingpolicy.Secrets{}, f, &out)
		return out.String(), err
	}

	if _, err := run(&sendingProtectionFlags{inspectExternal: true}); err == nil {
		t.Fatal("a missing -account-id must be refused")
	}
	out, err := run(&sendingProtectionFlags{inspectExternal: true, accountID: "usr_cmd_esa"})
	if err != nil || !strings.Contains(out, "external_sending_approved: false") || !strings.Contains(out, "external_sending_revision: 0") || !strings.Contains(out, "enforcement_applies:      true") {
		t.Fatalf("inspect = %q err=%v", out, err)
	}
	if strings.Contains(out, "@") {
		t.Fatalf("the readback must never print an address: %q", out)
	}
	if _, err := run(&sendingProtectionFlags{approveExternal: true, accountID: "usr_cmd_esa", expectedExternal: 0}); err == nil {
		t.Fatal("an approval without -reason must be refused")
	}
	if _, err := run(&sendingProtectionFlags{approveExternal: true, accountID: "usr_cmd_esa", expectedExternal: -1, reason: "x"}); err == nil {
		t.Fatal("an approval without the expected revision must be refused")
	}
	if _, err := run(&sendingProtectionFlags{approveExternal: true, accountID: "usr_cmd_esa", expectedExternal: 3, reason: "x"}); !errors.Is(err, sendingpolicy.ErrStaleExternalAccessRevision) {
		t.Fatalf("stale approval err = %v", err)
	}
	out, err = run(&sendingProtectionFlags{approveExternal: true, accountID: "usr_cmd_esa", expectedExternal: 0, reason: "reviewed request"})
	if err != nil || !strings.Contains(out, "external_sending_approved: true") || !strings.Contains(out, "external_sending_revision: 1") {
		t.Fatalf("approve = %q err=%v", out, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_limits (user_id, plan_code, max_agents, max_domains, max_messages_month, max_storage_bytes) VALUES ('usr_cmd_esa', 'paid_tier_test', 1, 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	out, err = run(&sendingProtectionFlags{revokeExternal: true, accountID: "usr_cmd_esa", expectedExternal: 1, reason: "abuse report"})
	if err != nil || !strings.Contains(out, "external_sending_approved: false") || !strings.Contains(out, "paid-base entitlement") {
		t.Fatalf("revoke = %q err=%v", out, err)
	}
}
