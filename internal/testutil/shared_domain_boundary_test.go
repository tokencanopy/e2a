package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sharedDomainBoundaryGuardedFiles configure the contract harness's shared
// domain (#829). AGENTS.md: an agents.e2a.dev address is a customer
// identifier, not a safe example value; agents.localhost is approved.
var sharedDomainBoundaryGuardedFiles = []string{
	"internal/testutil/contract_server.go",
	"internal/testutil/server.go",
	"tests/contract/scenarios.yaml",
}

func TestContractHarnessDoesNotUseCustomerSharedDomain(t *testing.T) {
	root := moduleRoot(t)
	for _, rel := range sharedDomainBoundaryGuardedFiles {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if strings.Contains(string(raw), "agents.e2a.dev") {
			t.Errorf("%s references agents.e2a.dev, the customer shared domain AGENTS.md forbids in test infrastructure; use agents.localhost instead", rel)
		}
	}
}

// moduleRoot walks up from the test's working directory to the directory
// holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
