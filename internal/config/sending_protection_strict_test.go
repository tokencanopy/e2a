package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

const esaHead = "sending_protection:\n  external_sending_access:\n    mode: enforce\n    accounts_created_at_or_after: \"1970-01-01T00:00:00Z\"\n"

// The sending_protection block is decoded strictly: a misspelled or
// mis-cased key (or a wrong indentation that lands a key in the wrong
// mapping) fails startup instead of silently meaning "every unlock".
func TestSendingProtectionBlockIsStrict(t *testing.T) {
	for name, body := range map[string]string{
		"misspelled unlocks key":   esaHead + "    unlock: [operator_approval]\n",
		"wrong-case unlocks key":   esaHead + "    Unlocks: [operator_approval]\n",
		"unknown sibling key":      esaHead + "    allow_all: true\n",
		"key indented a level off": esaHead + "  unlocks: [operator_approval]\n",
		"unknown top-level sp key": "sending_protection:\n  budget_mod: enforce\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadYAML(t, body); err == nil {
				t.Fatal("expected a startup error")
			}
		})
	}
}

// Other blocks stay lenient: self-hosters carry keys from older docs.
func TestUnknownKeysOutsideSendingProtectionStillLoad(t *testing.T) {
	if _, err := loadYAML(t, "some_retired_block:\n  x: 1\nhttp:\n  listen_addr: \":8080\"\n  retired_knob: true\n"); err != nil {
		t.Fatalf("lenient keys outside sending_protection must still load: %v", err)
	}
}

// An explicit null/blank unlock list must fail like `[]`, never read as
// "absent = every unlock". Absent keeps all three (nil).
func TestExternalSendingUnlocksPresence(t *testing.T) {
	cfg, err := loadYAML(t, esaHead)
	if err != nil {
		t.Fatalf("absent unlocks: %v", err)
	}
	if cfg.SendingProtect.ExternalSendingAccess.Unlocks != nil {
		t.Fatalf("absent unlocks must decode to nil, got %v", cfg.SendingProtect.ExternalSendingAccess.Unlocks)
	}
	for name, line := range map[string]string{
		"null":  "    unlocks: null\n",
		"blank": "    unlocks:\n",
		"tilde": "    unlocks: ~\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadYAML(t, esaHead+line)
			if err == nil || !strings.Contains(err.Error(), "unlocks") {
				t.Fatalf("expected an unlocks error, got %v", err)
			}
		})
	}
	// Content errors (empty, missing operator_approval, unknown, wrong case)
	// load here and are rejected by sendingpolicy.FromConfig at startup; see
	// internal/sendingpolicy TestFromConfigExternalSendingUnlocks.
	cfg, err = loadYAML(t, esaHead+"    unlocks: []\n")
	if err != nil {
		t.Fatalf("`[]` decodes here and is rejected by policy validation: %v", err)
	}
	if u := cfg.SendingProtect.ExternalSendingAccess.Unlocks; u == nil || len(u) != 0 {
		t.Fatalf("`[]` must decode to an empty non-nil list, got %#v", u)
	}
}

func TestExampleConfigStillLoads(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "config.example.yaml")); err != nil {
		t.Fatalf("config.example.yaml: %v", err)
	}
}

// Anchors, aliases and merge keys defined outside the block resolve in the
// strict pass exactly as in the lenient one — and strictness and the null
// check still apply through them.
func TestSendingProtectionStrictResolvesAnchors(t *testing.T) {
	const anchors = "x-unlocks: &unl [operator_approval]\n" +
		"x-esa: &esa\n  mode: enforce\n  accounts_created_at_or_after: \"1970-01-01T00:00:00Z\"\n"
	cfg, err := loadYAML(t, anchors+"sending_protection:\n  external_sending_access:\n    <<: *esa\n    unlocks: *unl\n")
	if err != nil {
		t.Fatalf("anchors/aliases/merge keys must load: %v", err)
	}
	esa := cfg.SendingProtect.ExternalSendingAccess
	if esa == nil || esa.Mode != "enforce" || len(esa.Unlocks) != 1 || esa.Unlocks[0] != "operator_approval" {
		t.Fatalf("aliased block decoded wrong: %+v", esa)
	}
	whole := "x-sp: &sp\n  external_sending_access:\n    mode: enforce\n    accounts_created_at_or_after: \"1970-01-01T00:00:00Z\"\n    unlocks: [operator_approval]\nsending_protection: *sp\n"
	if _, err := loadYAML(t, whole); err != nil {
		t.Fatalf("an aliased sending_protection block must load: %v", err)
	}
	if _, err := loadYAML(t, "x-esa: &esa\n  mode: enforce\n  accounts_created_at_or_after: \"1970-01-01T00:00:00Z\"\n  unlock: [operator_approval]\nsending_protection:\n  external_sending_access: *esa\n"); err == nil {
		t.Fatal("a misspelled key reached through an alias must still fail")
	}
	if _, err := loadYAML(t, "x-null: &n null\n"+esaHead+"    unlocks: *n\n"); err == nil {
		t.Fatal("a null unlocks reached through an alias must still fail")
	}
}
