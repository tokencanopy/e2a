package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfig_MaxMessagesDay pins the whole contract of the nullable
// daily cap at the YAML seam: absent → nil (no daily policy, the self-host
// default), 0 → &0 (hard block, NOT nil), positive round-trips, negative
// fails Load loudly. *int decoding is exactly what regresses silently.
func TestLoadConfig_MaxMessagesDay(t *testing.T) {
	base := `
smtp:
  listen_addr: ":3025"
  domain: "test.e2a.dev"
http:
  listen_addr: ":9090"
database:
  url: "postgres://test:test@localhost/test"
env: "development"
`
	load := func(t *testing.T, limitsBlock string) (*Config, error) {
		t.Helper()
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(cfgPath, []byte(base+limitsBlock), 0644); err != nil {
			t.Fatal(err)
		}
		return Load(cfgPath)
	}

	t.Run("absent_is_nil", func(t *testing.T) {
		cfg, err := load(t, "")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Limits.MaxMessagesDay != nil {
			t.Errorf("MaxMessagesDay = %v, want nil when key absent", *cfg.Limits.MaxMessagesDay)
		}
	})
	t.Run("zero_is_hard_block_not_nil", func(t *testing.T) {
		cfg, err := load(t, "limits:\n  max_messages_day: 0\n")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Limits.MaxMessagesDay == nil || *cfg.Limits.MaxMessagesDay != 0 {
			t.Errorf("MaxMessagesDay = %v, want pointer to 0", cfg.Limits.MaxMessagesDay)
		}
	})
	t.Run("hundred_round_trips", func(t *testing.T) {
		cfg, err := load(t, "limits:\n  max_messages_day: 100\n")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Limits.MaxMessagesDay == nil || *cfg.Limits.MaxMessagesDay != 100 {
			t.Errorf("MaxMessagesDay = %v, want pointer to 100", cfg.Limits.MaxMessagesDay)
		}
	})
	t.Run("negative_fails_load", func(t *testing.T) {
		if _, err := load(t, "limits:\n  max_messages_day: -1\n"); err == nil {
			t.Fatal("Load accepted a negative daily cap; want a validation error")
		}
	})
}
