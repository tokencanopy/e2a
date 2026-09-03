package identity_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/migrations"
)

// The Go enum and the SQL CHECK are two copies of one list. Read the
// CHECK back out of the migration so adding a value to only one side
// fails here, without a database.
func TestAcquisitionSourcesMatchMigrationEnum(t *testing.T) {
	sql, err := migrations.FS.ReadFile("120_users_acquisition_survey.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)acquisition_source IN \((.*?)\)`).FindStringSubmatch(string(sql))
	if m == nil {
		t.Fatal("no `acquisition_source IN (...)` CHECK found in migration 120")
	}
	var fromSQL []string
	for _, q := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(m[1], -1) {
		fromSQL = append(fromSQL, q[1])
	}
	if got, want := strings.Join(identity.AcquisitionSources, ","), strings.Join(fromSQL, ","); got != want {
		t.Fatalf("Go enum %q != migration CHECK %q", got, want)
	}
	for _, s := range identity.AcquisitionSources {
		if !identity.IsAcquisitionSource(s) {
			t.Errorf("IsAcquisitionSource(%q) = false", s)
		}
	}
	for _, bad := range []string{"", "Search", "carrier_pigeon", " github"} {
		if identity.IsAcquisitionSource(bad) {
			t.Errorf("IsAcquisitionSource(%q) = true", bad)
		}
	}
	if identity.AcquisitionSourceSkipped != "skipped" || !identity.IsAcquisitionSource("skipped") {
		t.Errorf("AcquisitionSourceSkipped = %q", identity.AcquisitionSourceSkipped)
	}
}
