package sendingpolicy

import (
	"os"
	"strings"
	"testing"
)

func TestSendingProtectionAllControlsDisabled(t *testing.T) {
	if !DisabledPolicy().AllControlsDisabled() {
		t.Fatal("generation zero must report all controls disabled")
	}

	// RampEnabled is deliberately excluded: the custom-domain ramp predates
	// sending protection, and an existing self-host running it must not
	// suddenly be required to mount the new secrets.
	ramped := DisabledPolicy()
	ramped.RampEnabled = true
	if !ramped.AllControlsDisabled() {
		t.Error("ramp_enabled alone must not count as an enabled control")
	}

	for name, mutate := range map[string]func(*RuntimePolicy){
		"budget shadow":        func(p *RuntimePolicy) { p.BudgetMode = ModeShadow },
		"detector enforce":     func(p *RuntimePolicy) { p.DetectorMode = ModeEnforce },
		"tenant header canary": func(p *RuntimePolicy) { p.TenantHeaderMode = TenantHeaderCanary },
		"provisioning on":      func(p *RuntimePolicy) { p.TenantProvisioningMode = ToggleEnforce },
		"suppression sync on":  func(p *RuntimePolicy) { p.TenantSuppressionSyncMode = ToggleEnforce },
	} {
		t.Run(name, func(t *testing.T) {
			p := DisabledPolicy()
			mutate(&p)
			if p.AllControlsDisabled() {
				t.Error("an enabled control must flip AllControlsDisabled to false")
			}
		})
	}
}

func TestSendingProtectionLoadSecretsFromEnvPresenceRules(t *testing.T) {
	enabled := DisabledPolicy()
	enabled.BudgetMode = ModeShadow

	t.Run("self-host disabled config-source may omit both", func(t *testing.T) {
		// t.Setenv is not called, but the process environment may still carry
		// the variables (a developer shell); skip rather than mislead.
		clearSecretEnv(t)
		secrets, err := LoadSecretsFromEnv(PolicySourceConfig, DisabledPolicy())
		if err != nil {
			t.Fatalf("disabled config-source must not require secrets: %v", err)
		}
		if secrets.Keyring != nil || secrets.Recipients != nil {
			t.Error("absent env must load as absent secrets")
		}
	})

	t.Run("database source requires both", func(t *testing.T) {
		clearSecretEnv(t)
		if _, err := LoadSecretsFromEnv(PolicySourceDatabase, DisabledPolicy()); err == nil {
			t.Fatal("database source must require the secrets")
		}
	})

	t.Run("any enabled control requires both", func(t *testing.T) {
		clearSecretEnv(t)
		if _, err := LoadSecretsFromEnv(PolicySourceConfig, enabled); err == nil {
			t.Fatal("an enabled control must require the secrets")
		}
	})

	t.Run("present but invalid always fails, even fully disabled", func(t *testing.T) {
		clearSecretEnv(t)
		t.Setenv(EnvFeedbackHMACKeys, "not-json")
		if _, err := LoadSecretsFromEnv(PolicySourceConfig, DisabledPolicy()); err == nil {
			t.Fatal("a malformed present secret must fail closed regardless of mode")
		}
	})

	t.Run("valid pair loads", func(t *testing.T) {
		clearSecretEnv(t)
		t.Setenv(EnvFeedbackHMACKeys, hmacV1)
		t.Setenv(EnvOperatorRecipientsMap, operatorV1)
		secrets, err := LoadSecretsFromEnv(PolicySourceDatabase, enabled)
		if err != nil {
			t.Fatalf("valid secrets must load: %v", err)
		}
		if secrets.Keyring == nil || secrets.Keyring.ActiveVersion() != 1 {
			t.Error("keyring did not load")
		}
		if secrets.Recipients == nil || secrets.Recipients.KeyID() != goldenKeyID {
			t.Error("operator map did not load")
		}
	})
}

// clearSecretEnv guarantees the two variables are unset for the subtest,
// whatever the invoking shell carries.
func clearSecretEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvFeedbackHMACKeys, "placeholder")
	t.Setenv(EnvOperatorRecipientsMap, "placeholder")
	// t.Setenv registers restoration; os.Unsetenv after it leaves the var
	// unset for the test body and restored afterwards.
	unsetForTest(t, EnvFeedbackHMACKeys)
	unsetForTest(t, EnvOperatorRecipientsMap)
}

func unsetForTest(t *testing.T, key string) {
	t.Helper()
	// os.Unsetenv, kept behind a helper so the intent reads at the call site.
	if err := unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
}

func TestBuildCapabilitiesShape(t *testing.T) {
	recipients, err := LoadOperatorRecipients(operatorV2)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	caps := BuildCapabilities(PolicySourceDatabase, Secrets{Recipients: recipients})

	if caps.SendingProtectionContract != 0 {
		t.Errorf("contract must be 0 until Task 12 raises it, got %d", caps.SendingProtectionContract)
	}
	if caps.RuntimePolicySource != "database" {
		t.Errorf("source = %q", caps.RuntimePolicySource)
	}
	if got := caps.OperatorCommitments["1"]; got != goldenCommitmentV1 {
		t.Errorf("commitment v1 = %s, want %s", got, goldenCommitmentV1)
	}
	for version, commitment := range caps.OperatorCommitments {
		if strings.Contains(commitment, "@") || strings.Contains(version, "@") {
			t.Error("capabilities must never carry an address")
		}
	}

	empty := BuildCapabilities(PolicySourceConfig, Secrets{})
	if empty.OperatorCommitments == nil || len(empty.OperatorCommitments) != 0 {
		t.Error("absent operator map must yield an empty, non-nil commitments object")
	}
}

// unsetenv adapts os.Unsetenv for the helper above.
func unsetenv(key string) error { return os.Unsetenv(key) }
