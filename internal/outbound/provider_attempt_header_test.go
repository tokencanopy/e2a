package outbound

import (
	"testing"

	"github.com/tokencanopy/e2a/internal/delivery"
)

// TestProviderAttemptHeaderNameMatchesDelivery: the adapter stamps the
// attempt marker under one name and the feedback parser looks for it under
// another constant (delivery cannot import this package). They must agree,
// or the deletion-resistant correlation fallback silently never matches.
func TestProviderAttemptHeaderNameMatchesDelivery(t *testing.T) {
	if ProviderAttemptHeader != delivery.ProviderAttemptHeader {
		t.Fatalf("outbound.ProviderAttemptHeader=%q delivery.ProviderAttemptHeader=%q", ProviderAttemptHeader, delivery.ProviderAttemptHeader)
	}
}
