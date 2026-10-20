package outboundsend

import (
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
)

// The deferred-erase lookup bounds ordinary sends by created_at >= window
// start - identity.EraseDeferRetryLag. That is only sound while no message can
// reach the provider later than the lag after its anchor; the worker's longest
// finite hold is PolicyBudgetHoldHorizon (and SendRetryHorizon for the other
// classes). Keep at least a day of margin for in-flight retries.
func TestEraseDeferRetryLagCoversTheLongestHold(t *testing.T) {
	longest := PolicyBudgetHoldHorizon
	if SendRetryHorizon > longest {
		longest = SendRetryHorizon
	}
	const margin = 24 * time.Hour
	if identity.EraseDeferRetryLag < longest+margin {
		t.Fatalf("identity.EraseDeferRetryLag = %v, must be at least the longest send hold (%v) plus a day", identity.EraseDeferRetryLag, longest)
	}
}
