package identity_test

import (
	"reflect"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
)

func TestSuppressionLookupForms(t *testing.T) {
	for in, want := range map[string][]string{
		"plain@example.test":          {"plain@example.test"},
		"leser@bücher.example":        {"leser@bücher.example", "leser@xn--bcher-kva.example"},
		"leser@xn--bcher-kva.example": {"leser@xn--bcher-kva.example", "leser@bücher.example"},
		"odd@my_host.example":         {"odd@my_host.example"},
		"no-at-sign":                  {"no-at-sign"},
	} {
		if got := identity.SuppressionLookupForms(in); !reflect.DeepEqual(got, want) {
			t.Errorf("SuppressionLookupForms(%q) = %v, want %v", in, got, want)
		}
	}
}
