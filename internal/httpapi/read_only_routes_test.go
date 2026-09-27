package httpapi

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/tokencanopy/e2a/internal/identity"
)

// rawRouteReadOnly records every non-GET route registered directly on the
// /v1 chi root (not a Huma operation, so readOnlyGuard never sees it) with
// the reason read-only enforcement is not needed there or where it happens.
// Keyed "METHOD pattern"; "* pattern" covers every method of a route
// registered with Handle.
var rawRouteReadOnly = map[string]string{
	// Recipient-side managed unsubscribe: a bearer-capability link acted on
	// by the mail recipient, never by the account.
	"* /u/{token}": "recipient action, belongs to no account principal",
	// HITL magic links: token-authorized; the handler resolves the owning
	// account from the message and refuses a read-only one
	// (refuseMagicIfReadOnly in internal/agent/hitl_magic_api.go).
	"* /v1/approve": "enforced in the handler (refuseMagicIfReadOnly)",
	"* /v1/reject":  "enforced in the handler (refuseMagicIfReadOnly)",
	// The trashed-account interstitial (restricted session): restore and
	// erase are unchanged for a paused account by design, and erase is
	// already held (409 erase_held) while any pause applies.
	"POST /api/account/restore": "trash interstitial, unchanged by design",
	"POST /api/account/erase":   "trash interstitial; erase held while paused",
}

// TestRawRootRoutesAreClassifiedForReadOnly walks the chi root with every
// optional raw route enabled and fails on any non-GET route that is neither a
// Huma operation (covered by readOnlyGuard) nor on rawRouteReadOnly — so a new
// raw write route cannot bypass read-only enforcement unnoticed.
func TestRawRootRoutesAreClassifiedForReadOnly(t *testing.T) {
	noop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	s := New(Deps{
		WSHandle:         func(http.ResponseWriter, *http.Request, string) {},
		AttachmentStore:  NewNativeAttachmentStore(strings.Repeat("k", 32), "https://example.test"),
		MagicLinkApprove: noop,
		MagicLinkReject:  noop,
		RestrictedSession: func(*http.Request) (*identity.User, string, error) {
			return nil, "", nil
		},
		RestoreAccount: func(context.Context, string, string) (*identity.User, error) { return nil, nil },
		DeleteUserData: func(context.Context, *identity.User, bool) (*identity.DeleteUserDataResult, error) {
			return nil, nil
		},
		SameOriginRequest: func(*http.Request) bool { return true },
	})

	humaOps := map[string]bool{}
	for path, item := range s.API.OpenAPI().Paths {
		for method, op := range map[string]*huma.Operation{
			http.MethodGet: item.Get, http.MethodPost: item.Post, http.MethodPut: item.Put,
			http.MethodPatch: item.Patch, http.MethodDelete: item.Delete, http.MethodHead: item.Head,
		} {
			if op != nil {
				humaOps[method+" "+path] = true
			}
		}
	}

	byPattern := map[string]map[string]bool{}
	if err := chi.Walk(s.Router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if byPattern[route] == nil {
			byPattern[route] = map[string]bool{}
		}
		byPattern[route][method] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	var unclassified []string
	for route, methods := range byPattern {
		for method := range methods {
			switch method {
			case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodConnect:
				continue
			}
			key := method + " " + route
			if humaOps[key] {
				continue
			}
			if _, ok := rawRouteReadOnly[key]; ok {
				seen[key] = true
				continue
			}
			if _, ok := rawRouteReadOnly["* "+route]; ok {
				seen["* "+route] = true
				continue
			}
			unclassified = append(unclassified, key)
		}
	}
	sort.Strings(unclassified)
	for _, k := range unclassified {
		t.Errorf("raw /v1-root route %s is neither a Huma operation nor classified in rawRouteReadOnly", k)
	}
	for k := range rawRouteReadOnly {
		if !seen[k] {
			t.Errorf("rawRouteReadOnly classifies %q, which the root does not register (stale entry)", k)
		}
	}
	if len(humaOps) < 50 {
		t.Fatalf("saw only %d Huma operations; the walk is broken", len(humaOps))
	}
}
