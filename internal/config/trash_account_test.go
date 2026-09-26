package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTrashConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("database:\n  url: \"postgres://x\"\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAccountRetentionDefaultsToTrashRetention(t *testing.T) {
	cfg, err := Load(writeTrashConfig(t, "trash:\n  retention_days: 12\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Trash.AccountRetention() != 12 || cfg.Trash.IdentityTombstones {
		t.Fatalf("account retention = %d tombstones=%v, want 12/false", cfg.Trash.AccountRetention(), cfg.Trash.IdentityTombstones)
	}
}

func TestAccountRetentionZeroOptsOut(t *testing.T) {
	cfg, err := Load(writeTrashConfig(t, "trash:\n  retention_days: 30\n  account_retention_days: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Trash.AccountRetention() != 0 {
		t.Fatalf("account retention = %d, want 0 (immediate erase)", cfg.Trash.AccountRetention())
	}
}

func TestAccountTrashEnvOverridesFailClosedWhenMalformed(t *testing.T) {
	p := writeTrashConfig(t, "")
	t.Setenv("E2A_TRASH_ACCOUNT_RETENTION_DAYS", "thirty")
	if _, err := Load(p); err == nil {
		t.Fatal("a malformed E2A_TRASH_ACCOUNT_RETENTION_DAYS was ignored")
	}
	t.Setenv("E2A_TRASH_ACCOUNT_RETENTION_DAYS", "-1")
	if _, err := Load(p); err == nil {
		t.Fatal("a negative account retention was accepted")
	}
	t.Setenv("E2A_TRASH_ACCOUNT_RETENTION_DAYS", "7")
	t.Setenv("E2A_TRASH_IDENTITY_TOMBSTONES", "yes please")
	if _, err := Load(p); err == nil {
		t.Fatal("a malformed E2A_TRASH_IDENTITY_TOMBSTONES was ignored")
	}
	t.Setenv("E2A_TRASH_IDENTITY_TOMBSTONES", "true")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Trash.AccountRetention() != 7 || !cfg.Trash.IdentityTombstones {
		t.Fatalf("env overrides = %d/%v", cfg.Trash.AccountRetention(), cfg.Trash.IdentityTombstones)
	}
}
