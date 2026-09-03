package identity_test

import (
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
)

func TestAcquisitionSourcesMatchMigrationEnum(t *testing.T) {
	want := []string{"search", "ai_assistant", "github", "x_twitter", "hn_reddit",
		"content", "mcp_directory", "word_of_mouth", "other", "skipped"}
	if len(identity.AcquisitionSources) != len(want) {
		t.Fatalf("len = %d, want %d", len(identity.AcquisitionSources), len(want))
	}
	for i, s := range want {
		if identity.AcquisitionSources[i] != s {
			t.Errorf("[%d] = %q, want %q", i, identity.AcquisitionSources[i], s)
		}
		if !identity.IsAcquisitionSource(s) {
			t.Errorf("IsAcquisitionSource(%q) = false", s)
		}
	}
	for _, bad := range []string{"", "Search", "carrier_pigeon", " github"} {
		if identity.IsAcquisitionSource(bad) {
			t.Errorf("IsAcquisitionSource(%q) = true", bad)
		}
	}
	if identity.AcquisitionSourceSkipped != "skipped" {
		t.Errorf("AcquisitionSourceSkipped = %q", identity.AcquisitionSourceSkipped)
	}
}
