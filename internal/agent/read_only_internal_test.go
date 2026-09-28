package agent

import (
	"net/http"
	"sort"
	"testing"

	"github.com/gorilla/mux"
	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/usage"
)

// TestLegacyWriteRoutesAreClassified walks every route RegisterRoutes
// registers (with the dashboard and OIDC routes enabled) and requires each
// non-GET/HEAD method to carry an explicit read-only classification, so a new
// legacy write route cannot silently bypass read-only enforcement.
func TestLegacyWriteRoutesAreClassified(t *testing.T) {
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	userAuth := auth.NewUserAuth(&config.OAuthConfig{}, nil, false)
	a := NewAPI(nil, outbound.NewSender(relay, "test.e2a.dev"), relay, userAuth, usage.NewNoopUsageTracker(),
		"e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	r := mux.NewRouter()
	a.RegisterRoutes(r)

	seen := map[string]bool{}
	var unclassified []string
	err := r.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tmpl, err := route.GetPathTemplate()
		if err != nil {
			return nil
		}
		methods, err := route.GetMethods()
		if err != nil {
			unclassified = append(unclassified, "ANY "+tmpl+" (route without a method restriction)")
			return nil
		}
		for _, m := range methods {
			if m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions {
				continue
			}
			key := m + " " + tmpl
			seen[key] = true
			if _, ok := legacyWriteRoutes[key]; !ok {
				unclassified = append(unclassified, key)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(unclassified)
	for _, k := range unclassified {
		t.Errorf("legacy write route %s is not classified in legacyWriteRoutes (read_only.go)", k)
	}
	for k := range legacyWriteRoutes {
		if !seen[k] {
			t.Errorf("legacyWriteRoutes classifies %q, which RegisterRoutes does not register (stale entry)", k)
		}
	}
	if len(seen) < 10 {
		t.Fatalf("walk saw only %d write routes; the router walk is broken", len(seen))
	}
}
