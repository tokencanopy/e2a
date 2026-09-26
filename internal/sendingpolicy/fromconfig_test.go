package sendingpolicy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tokencanopy/e2a/internal/config"
)

func TestPolicySourceFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want PolicySource
	}{
		{name: "absent defaults to config", want: PolicySourceConfig},
		{name: "explicit config", raw: "config", want: PolicySourceConfig},
		{name: "hosted database", raw: "database", want: PolicySourceDatabase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SourceFromConfig(&config.Config{SendingProtect: config.SendingProtectionConfig{
				RuntimePolicySource: tc.raw,
			}})
			if err != nil {
				t.Fatalf("source from config: %v", err)
			}
			if got != tc.want {
				t.Errorf("source = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := SourceFromConfig(&config.Config{SendingProtect: config.SendingProtectionConfig{
		RuntimePolicySource: "latest",
	}}); err == nil {
		t.Fatal("unknown runtime policy source must fail closed")
	}
}

func TestFromConfigExternalSendingAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	policy, err := FromConfig(cfg)
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	if policy.ExternalSendingAccess != nil {
		t.Fatal("the default config must not carry external_sending_access (self-host behavior unchanged)")
	}

	cfg.SendingProtect.ExternalSendingAccess = &config.ExternalSendingAccessConfig{Mode: "shadow", AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z"}
	policy, err = FromConfig(cfg)
	if err != nil {
		t.Fatalf("configured block: %v", err)
	}
	if policy.ExternalSendingMode() != ModeShadow || policy.ExternalSendingAccess.AccountsCreatedAtOrAfter != "2026-10-01T00:00:00Z" {
		t.Fatalf("block not wired: %+v", policy.ExternalSendingAccess)
	}

	cfg.SendingProtect.ExternalSendingAccess.AccountsCreatedAtOrAfter = "tomorrow"
	if _, err := FromConfig(cfg); err == nil {
		t.Fatal("an invalid cutoff must fail config validation")
	}
}
