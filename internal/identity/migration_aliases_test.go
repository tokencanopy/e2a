package identity

import (
	"io/fs"
	"testing"

	"github.com/tokencanopy/e2a/migrations"
)

func TestMigrationFilenameAliasesAreOneToOneAndCurrentFilesExist(t *testing.T) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	files := make(map[string]bool, len(entries))
	for _, entry := range entries {
		files[entry.Name()] = true
	}
	legacyOwners := make(map[string]string, len(migrationFilenameAliases))
	for current, legacy := range migrationFilenameAliases {
		if !files[current] {
			t.Errorf("alias current migration %s is missing from embedded migrations", current)
		}
		if previous, exists := legacyOwners[legacy]; exists {
			t.Errorf("legacy migration %s aliases both %s and %s", legacy, previous, current)
		}
		legacyOwners[legacy] = current
	}
}
