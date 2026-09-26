package identity_test

import (
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
)

func TestNormalizeTombstoneValue(t *testing.T) {
	cases := []struct{ kind, in, want string }{
		{identity.TombstoneKindEmail, "  Alice@Example.COM ", "alice@example.com"},
		{identity.TombstoneKindEmail, "alice+promo@example.com", "alice@example.com"},
		{identity.TombstoneKindEmail, "a.l.i.c.e@example.com", "a.l.i.c.e@example.com"}, // dots matter off gmail
		{identity.TombstoneKindEmail, "A.Lice+x@gmail.com", "alice@gmail.com"},
		{identity.TombstoneKindEmail, "a.lice@googlemail.com", "alice@gmail.com"},
		{identity.TombstoneKindEmail, "user@Bücher.example", "user@xn--bcher-kva.example"},
		{identity.TombstoneKindDomain, "Bücher.Example.", "xn--bcher-kva.example"},
		{identity.TombstoneKindDomain, " EXAMPLE.test ", "example.test"},
		{identity.TombstoneKindLoginSubject, "Sub-ABC ", "Sub-ABC "}, // byte-exact
	}
	for _, c := range cases {
		if got := identity.NormalizeTombstoneValue(c.kind, c.in); got != c.want {
			t.Errorf("Normalize(%s, %q) = %q, want %q", c.kind, c.in, got, c.want)
		}
	}
}

func TestParseTombstoneKeyring(t *testing.T) {
	key32 := "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=" // 32 bytes, std base64
	if kr, err := identity.ParseTombstoneKeyring(""); kr != nil || err != nil {
		t.Fatalf("empty = %v, %v; want not configured", kr, err)
	}
	kr, err := identity.ParseTombstoneKeyring("v1:" + key32 + ", v3:" + strings.TrimRight(key32, "="))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if kr.ActiveVersion() != 3 {
		t.Fatalf("active = %d, want the highest version 3", kr.ActiveVersion())
	}
	a, _ := kr.Digest(1, identity.TombstoneKindEmail, "x@example.test")
	b, _ := kr.Digest(1, identity.TombstoneKindDomain, "x@example.test")
	if string(a) == string(b) {
		t.Fatal("kind is not bound into the digest")
	}
	for name, raw := range map[string]string{
		"no version":       key32,
		"bad version":      "vx:" + key32,
		"zero version":     "v0:" + key32,
		"short secret":     "v1:AAEC",
		"not base64":       "v1:%%%%",
		"duplicate":        "v1:" + key32 + ",v1:" + key32,
		"missing v prefix": "1:" + key32,
	} {
		_, err := identity.ParseTombstoneKeyring(raw)
		if err == nil {
			t.Errorf("%s: parsed, want an error", name)
			continue
		}
		if strings.Contains(err.Error(), key32) {
			t.Errorf("%s: error echoes the secret", name)
		}
	}
}

func TestRecentDeletionHold(t *testing.T) {
	saved := identity.AccountTrashRetention
	t.Cleanup(func() { identity.AccountTrashRetention = saved })

	identity.AccountTrashRetention = 30 * 24 * time.Hour
	// Early in a 31-day month the remainder of the quota period wins.
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := identity.RecentDeletionHold(start); got != 31*24*time.Hour {
		t.Errorf("hold at month start = %v, want 31 days (rest of the quota period)", got)
	}
	// Late in a month the 30-day floor wins.
	late := time.Date(2026, 1, 30, 0, 0, 0, 0, time.UTC)
	if got := identity.RecentDeletionHold(late); got != 30*24*time.Hour {
		t.Errorf("hold at month end = %v, want the 30-day floor", got)
	}
	// A longer trash window wins over both.
	identity.AccountTrashRetention = 45 * 24 * time.Hour
	if got := identity.RecentDeletionHold(late); got != 45*24*time.Hour {
		t.Errorf("hold with a 45-day window = %v", got)
	}
}

func TestTombstoneKeyringActiveSelector(t *testing.T) {
	key32 := "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	raw := "v1:" + key32 + ",v2:" + key32
	kr, err := identity.ParseTombstoneKeyringWithActive(raw, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if kr.ActiveVersion() != 1 || !kr.Has(2) {
		t.Fatalf("active=%d has(v2)=%v, want v1 active with v2 known (two-phase rotation)", kr.ActiveVersion(), kr.Has(2))
	}
	if kr, _ := identity.ParseTombstoneKeyringWithActive(raw, ""); kr.ActiveVersion() != 2 {
		t.Fatalf("default active = %d, want the highest", kr.ActiveVersion())
	}
	for _, bad := range []string{"v3", "2", "vx"} {
		if _, err := identity.ParseTombstoneKeyringWithActive(raw, bad); err == nil {
			t.Errorf("active selector %q accepted", bad)
		}
	}
	if _, err := identity.ParseTombstoneKeyringWithActive("", "v1"); err == nil {
		t.Error("an active selector without a key was accepted")
	}
}
