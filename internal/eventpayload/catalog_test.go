package eventpayload_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

func TestStableCatalogPartitionsKnownEvents(t *testing.T) {
	stable := map[string]bool{}
	schemas := map[string]bool{}
	fixtures := map[string]bool{}
	for _, entry := range eventpayload.StableEvents {
		if stable[entry.Type] {
			t.Errorf("duplicate stable event type %q", entry.Type)
		}
		stable[entry.Type] = true
		if entry.SchemaName == "" {
			t.Errorf("stable event %q has no schema name", entry.Type)
		} else if schemas[entry.SchemaName] {
			t.Errorf("duplicate stable schema name %q", entry.SchemaName)
		}
		schemas[entry.SchemaName] = true
		if entry.Payload == nil {
			t.Errorf("stable event %q has no payload type", entry.Type)
		}
		if entry.Fixture == "" {
			t.Errorf("stable event %q has no full fixture", entry.Type)
		} else if fixtures[entry.Fixture] {
			t.Errorf("duplicate stable fixture %q", entry.Fixture)
		}
		fixtures[entry.Fixture] = true
		if entry.MinimalFixture != "" {
			if fixtures[entry.MinimalFixture] {
				t.Errorf("duplicate stable fixture %q", entry.MinimalFixture)
			}
			fixtures[entry.MinimalFixture] = true
		}
	}

	experimental := map[string]bool{}
	for _, typ := range webhookpub.ExperimentalEventTypes {
		experimental[typ] = true
	}
	for _, typ := range webhookpub.AllEventTypes {
		if stable[typ] == experimental[typ] {
			t.Errorf("event %q must be exactly one of stable or experimental", typ)
		}
	}
	if len(stable)+len(experimental) != len(webhookpub.AllEventTypes) {
		t.Errorf("stable (%d) + experimental (%d) != all event types (%d)", len(stable), len(experimental), len(webhookpub.AllEventTypes))
	}

	// A beta payload is published and fixtured but NOT frozen, so it must be
	// experimental and must never also appear in the stable catalog. Promoting
	// one is then a single coordinated move (move the catalog entry, drop the
	// experimental listing) that this test fails until both halves land.
	betaTypes := map[string]bool{}
	for _, entry := range eventpayload.BetaEvents {
		if betaTypes[entry.Type] {
			t.Errorf("duplicate beta event type %q", entry.Type)
		}
		betaTypes[entry.Type] = true
		if stable[entry.Type] {
			t.Errorf("event %q is in BOTH the stable and beta catalogs", entry.Type)
		}
		if !experimental[entry.Type] {
			t.Errorf("beta payload %q is not in webhookpub.ExperimentalEventTypes — a published-but-unfrozen payload must stay marked experimental", entry.Type)
		}
		if entry.SchemaName == "" || entry.Payload == nil || entry.Fixture == "" {
			t.Errorf("beta event %q must carry a schema name, payload type, and golden fixture", entry.Type)
		}
		if schemas[entry.SchemaName] {
			t.Errorf("duplicate schema name %q across the stable and beta catalogs", entry.SchemaName)
		}
		schemas[entry.SchemaName] = true
		for _, name := range []string{entry.Fixture, entry.MinimalFixture} {
			if name == "" {
				continue
			}
			if fixtures[name] {
				t.Errorf("duplicate fixture %q", name)
			}
			fixtures[name] = true
		}
	}
}

// TestCatalogFixturesExist stops a catalog entry from naming a file that was
// never committed: the coverage gates assert presence in the catalog, not on
// disk, so a typo would otherwise pass every test that reads the catalog.
func TestCatalogFixturesExist(t *testing.T) {
	entries := append(append([]eventpayload.EventPayloadContract{}, eventpayload.StableEvents...), eventpayload.BetaEvents...)
	for _, entry := range entries {
		for _, name := range []string{entry.Fixture, entry.MinimalFixture} {
			if name == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join("testdata", name)); err != nil {
				t.Errorf("event %s names fixture %s, which does not exist: %v", entry.Type, name, err)
			}
		}
	}
}
