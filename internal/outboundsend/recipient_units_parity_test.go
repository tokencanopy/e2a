package outboundsend

import (
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
)

// TestUniqueRecipientCount_ParityWithIdentity pins this package's private
// uniqueRecipientCount (which sizes sendramp reservations) to
// identity.UniqueRecipientCount (which sizes the metered units and the
// accept-time cap pre-check). outboundsend deliberately does not import
// identity in production code — this test-only coupling is the guard that
// keeps the two normalizers from drifting: if they ever disagree, the ramp
// ledger and the billing meter would count the same message differently.
func TestUniqueRecipientCount_ParityWithIdentity(t *testing.T) {
	cases := [][]string{
		{"a@x.test"},
		{"a@x.test", "b@x.test", "c@x.test"},
		{"Alice@x.test", "alice@x.test"},
		{" a@x.test ", "a@x.test"},
		{"a@x.test", "", "B@Y.test", "b@y.test"},
		{},
		{"", "  "},
	}
	for _, recipients := range cases {
		local := uniqueRecipientCount(recipients)
		canonical := identity.UniqueRecipientCount(recipients)
		if local != canonical {
			t.Errorf("recipients %q: outboundsend counts %d, identity counts %d — normalizers drifted",
				recipients, local, canonical)
		}
	}
}
