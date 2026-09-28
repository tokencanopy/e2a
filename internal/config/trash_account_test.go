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

func TestRecentSenderEraseDeferDefaultsTo14AndZeroDisables(t *testing.T) {
	cfg, err := Load(writeTrashConfig(t, "trash:\n  retention_days: 30\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Trash.RecentSenderEraseDeferDays != 14 {
		t.Fatalf("recent_sender_erase_defer_days default = %d, want 14", cfg.Trash.RecentSenderEraseDeferDays)
	}
	cfg, err = Load(writeTrashConfig(t, "trash:\n  retention_days: 30\n  recent_sender_erase_defer_days: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Trash.RecentSenderEraseDeferDays != 0 {
		t.Fatalf("explicit 0 = %d, want 0 (disabled)", cfg.Trash.RecentSenderEraseDeferDays)
	}
	if _, err := Load(writeTrashConfig(t, "trash:\n  retention_days: 30\n  recent_sender_erase_defer_days: -1\n")); err == nil {
		t.Fatal("a negative recent_sender_erase_defer_days was accepted")
	}
}

func TestRecentSenderEraseDeferEnvOverride(t *testing.T) {
	p := writeTrashConfig(t, "")
	t.Setenv("E2A_TRASH_RECENT_SENDER_ERASE_DEFER_DAYS", "two weeks")
	if _, err := Load(p); err == nil {
		t.Fatal("a malformed E2A_TRASH_RECENT_SENDER_ERASE_DEFER_DAYS was ignored")
	}
	t.Setenv("E2A_TRASH_RECENT_SENDER_ERASE_DEFER_DAYS", "3")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Trash.RecentSenderEraseDeferDays != 3 {
		t.Fatalf("env override = %d, want 3", cfg.Trash.RecentSenderEraseDeferDays)
	}
}
