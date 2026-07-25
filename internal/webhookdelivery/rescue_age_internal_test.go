package webhookdelivery

import (
	"testing"
	"time"
)

// TestRescueQuietForCoversRetryEnvelope pins the relationship the rescue age
// gate depends on: rescueQuietFor must exceed the full retry envelope
// (sum(retryBackoffs), first→final attempt), or the dead-job rescue scan would
// consider rows whose live job is still legitimately mid-backoff. White-box on
// purpose — both constants are unexported. If either changes, re-derive the
// other.
func TestRescueQuietForCoversRetryEnvelope(t *testing.T) {
	var envelope time.Duration
	for _, d := range retryBackoffs {
		envelope += d
	}
	// rescueQuietFor is a SQL interval literal; keep this parse in sync with it.
	if rescueQuietFor != "30 hours" {
		t.Fatalf("rescueQuietFor = %q — update this test's parsed duration alongside it", rescueQuietFor)
	}
	quiet := 30 * time.Hour
	if quiet <= envelope {
		t.Errorf("rescueQuietFor (%v) must exceed the retry envelope (%v)", quiet, envelope)
	}
	// Sanity on the documented numbers: envelope is 29h21m, margin ~39m.
	if want := 29*time.Hour + 21*time.Minute; envelope != want {
		t.Errorf("retry envelope = %v, want %v — retryBackoffs changed; re-derive rescueQuietFor", envelope, want)
	}
}
