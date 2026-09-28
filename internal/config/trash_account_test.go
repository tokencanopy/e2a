package config

import (
	"os"
	"path/filepath"
	"strings"
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
	if cfg.Trash.RecentSenderEraseDefer() != 14 {
		t.Fatalf("recent_sender_erase_defer_days default = %d, want 14", cfg.Trash.RecentSenderEraseDefer())
	}
	cfg, err = Load(writeTrashConfig(t, "trash:\n  retention_days: 30\n  recent_sender_erase_defer_days: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Trash.RecentSenderEraseDefer() != 0 {
		t.Fatalf("explicit 0 = %d, want 0 (disabled)", cfg.Trash.RecentSenderEraseDefer())
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
	if cfg.Trash.RecentSenderEraseDefer() != 3 {
		t.Fatalf("env override = %d, want 3", cfg.Trash.RecentSenderEraseDefer())
	}
}

func TestRecentSenderEraseDeferMustFitTheTrashWindows(t *testing.T) {
	if _, err := Load(writeTrashConfig(t, "trash:\n  retention_days: 10\n  recent_sender_erase_defer_days: 11\n")); err == nil ||
		!strings.Contains(err.Error(), "retention_days") {
		t.Fatalf("defer window longer than retention_days accepted: %v", err)
	}
	if _, err := Load(writeTrashConfig(t, "trash:\n  retention_days: 30\n  account_retention_days: 7\n  recent_sender_erase_defer_days: 8\n")); err == nil ||
		!strings.Contains(err.Error(), "account_retention_days") {
		t.Fatalf("defer window longer than account_retention_days accepted: %v", err)
	}
	// account trash disabled: only the agent/message trash window binds.
	if _, err := Load(writeTrashConfig(t, "trash:\n  retention_days: 30\n  account_retention_days: 0\n  recent_sender_erase_defer_days: 20\n")); err != nil {
		t.Fatalf("defer window with account trash disabled rejected: %v", err)
	}
	// Unset: the default is lowered to the shortest window, never rejected.
	cfg, err := Load(writeTrashConfig(t, "trash:\n  retention_days: 7\n"))
	if err != nil {
		t.Fatalf("unset defer window with a 7-day trash rejected: %v", err)
	}
	if got := cfg.Trash.RecentSenderEraseDefer(); got != 7 {
		t.Fatalf("effective default = %d, want 7 (the shorter trash window)", got)
	}
}

func TestEraseDeferExemptDomains(t *testing.T) {
	cfg, err := Load(writeTrashConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Trash.EraseDeferExemptDomains) != 1 || cfg.Trash.EraseDeferExemptDomains[0] != "simulator.amazonses.com" {
		t.Fatalf("default exempt domains = %v, want [simulator.amazonses.com]", cfg.Trash.EraseDeferExemptDomains)
	}
	cfg, err = Load(writeTrashConfig(t, "trash:\n  retention_days: 30\n  erase_defer_exempt_domains: [sim.example.test, mailbox.example.test]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Trash.EraseDeferExemptDomains) != 2 {
		t.Fatalf("configured exempt domains = %v", cfg.Trash.EraseDeferExemptDomains)
	}
	if _, err := Load(writeTrashConfig(t, "trash:\n  retention_days: 30\n  erase_defer_exempt_domains: [\"x@example.test\"]\n")); err == nil {
		t.Fatal("an address in erase_defer_exempt_domains was accepted")
	}
	t.Setenv("E2A_TRASH_ERASE_DEFER_EXEMPT_DOMAINS", " a.example.test , b.example.test ")
	cfg, err = Load(writeTrashConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Trash.EraseDeferExemptDomains) != 2 || cfg.Trash.EraseDeferExemptDomains[1] != "b.example.test" {
		t.Fatalf("env exempt domains = %v", cfg.Trash.EraseDeferExemptDomains)
	}
}

// C1/C1b/C2: the two trash windows are validated and lowered independently,
// and each error names its exact field.
func TestRecentSenderEraseDeferAgainstEachTrashWindow(t *testing.T) {
	// account_retention_days larger than retention_days: retention_days binds.
	_, err := Load(writeTrashConfig(t, "trash:\n  retention_days: 10\n  account_retention_days: 60\n  recent_sender_erase_defer_days: 12\n"))
	if err == nil || !strings.Contains(err.Error(), "must not exceed trash.retention_days (10)") ||
		strings.Contains(err.Error(), "account_retention_days") {
		t.Fatalf("err = %v, want one naming trash.retention_days only", err)
	}
	// account_retention_days shorter than retention_days: it binds.
	_, err = Load(writeTrashConfig(t, "trash:\n  retention_days: 30\n  account_retention_days: 9\n  recent_sender_erase_defer_days: 10\n"))
	if err == nil || !strings.Contains(err.Error(), "must not exceed trash.account_retention_days (9)") {
		t.Fatalf("err = %v, want one naming trash.account_retention_days", err)
	}
	// Unset window, account trash shorter: lowered to account_retention_days.
	cfg, err := Load(writeTrashConfig(t, "trash:\n  retention_days: 30\n  account_retention_days: 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Trash.RecentSenderEraseDefer(); got != 5 {
		t.Fatalf("effective window = %d, want 5 (account_retention_days)", got)
	}
	// Unset window, account trash disabled (0): only retention_days lowers it.
	cfg, err = Load(writeTrashConfig(t, "trash:\n  retention_days: 8\n  account_retention_days: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Trash.RecentSenderEraseDefer(); got != 8 {
		t.Fatalf("effective window = %d, want 8 (retention_days; account trash disabled)", got)
	}
	// Unset window, account_retention_days unset: it reuses retention_days.
	cfg, err = Load(writeTrashConfig(t, "trash:\n  retention_days: 40\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Trash.RecentSenderEraseDefer(); got != DefaultRecentSenderEraseDeferDays {
		t.Fatalf("effective window = %d, want the default %d", got, DefaultRecentSenderEraseDeferDays)
	}
}

func TestEraseDeferExemptDomainListAddsTheSharedDomain(t *testing.T) {
	cfg, err := Load(writeTrashConfig(t, "shared_domain: Agents.Example.Test\ntrash:\n  retention_days: 30\n  erase_defer_exempt_domains: [\" Sim.Example.Test \"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.EraseDeferExemptDomainList()
	if len(got) != 2 || got[0] != "sim.example.test" || got[1] != "agents.example.test" {
		t.Fatalf("exempt list = %v, want [sim.example.test agents.example.test]", got)
	}
	cfg.SharedDomain = ""
	if got := cfg.EraseDeferExemptDomainList(); len(got) != 1 {
		t.Fatalf("exempt list without a shared domain = %v, want only the configured entry", got)
	}
}
