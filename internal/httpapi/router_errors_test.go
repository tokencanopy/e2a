package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestV1RouterErrorsUseCanonicalEnvelope(t *testing.T) {
	legacyCalls := 0
	s := New(Deps{Legacy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legacyCalls++
		w.WriteHeader(http.StatusTeapot)
	})})

	tests := []struct {
		name   string
		method string
		path   string
		status int
		code   string
	}{
		{name: "not found", method: http.MethodGet, path: "/v1/not-a-route", status: http.StatusNotFound, code: "not_found"},
		{name: "method not allowed", method: http.MethodPost, path: "/v1/info", status: http.StatusMethodNotAllowed, code: "method_not_allowed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%q", rr.Code, tc.status, rr.Body.String())
			}
			if got := rr.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			var env ErrorEnvelope
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v; body=%q", err, rr.Body.String())
			}
			if env.Err.Code != tc.code {
				t.Fatalf("error.code = %q, want %q", env.Err.Code, tc.code)
			}
			if env.Err.RequestID == "" || env.Err.RequestID != rr.Header().Get(requestIDHeader) {
				t.Fatalf("request id mismatch: body=%q header=%q", env.Err.RequestID, rr.Header().Get(requestIDHeader))
			}
		})
	}
	if legacyCalls != 0 {
		t.Fatalf("legacy handler called %d times for /v1 errors", legacyCalls)
	}
}

// RFC 9110 §15.5.6: every 405 MUST carry an Allow header naming the methods
// the resource does support. The header is derived by probing the live chi
// routing table, so these expected sets are exactly the methods registered
// for each path — if a route gains or loses a method, the assertion updates
// with it (and this test tells you).
func TestMethodNotAllowedCarriesAllowHeader(t *testing.T) {
	s := New(Deps{
		// Register the raw (non-Huma) chi WebSocket route too.
		WSHandle: func(w http.ResponseWriter, r *http.Request, address string) {},
	})

	tests := []struct {
		name    string
		method  string
		path    string
		allowed []string
	}{
		// Huma-registered operation, single method.
		{name: "huma single-method", method: http.MethodPost, path: "/v1/info", allowed: []string{http.MethodGet}},
		// Huma-registered operations sharing one path, multiple methods.
		{name: "huma multi-method", method: http.MethodPatch, path: "/v1/domains/example.com", allowed: []string{http.MethodGet, http.MethodDelete}},
		// Raw chi route registered outside Huma.
		{name: "raw chi route", method: http.MethodPost, path: "/v1/agents/a%40b.example/ws", allowed: []string{http.MethodGet}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
			if rr.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405; body=%q", rr.Code, rr.Body.String())
			}
			allow := rr.Header().Get("Allow")
			if allow == "" {
				t.Fatalf("405 response missing Allow header (RFC 9110 §15.5.6)")
			}
			got := map[string]bool{}
			for _, m := range strings.Split(allow, ",") {
				got[strings.TrimSpace(m)] = true
			}
			want := map[string]bool{}
			for _, m := range tc.allowed {
				want[m] = true
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Allow = %q (set %v), want exactly %v", allow, got, want)
			}
			// The header must not disturb the canonical JSON envelope.
			var env ErrorEnvelope
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v; body=%q", err, rr.Body.String())
			}
			if env.Err.Code != "method_not_allowed" {
				t.Fatalf("error.code = %q, want method_not_allowed", env.Err.Code)
			}
		})
	}
}

// A method chi does not know at all never reaches the routing tree: routeHTTP
// fails the methodMap lookup and dispatches MethodNotAllowedHandler with an
// empty allowed-method set, so every probe misses and the derived value is "".
// RFC 9110 §10.2.1 makes an empty Allow field value meaningful ("no methods are
// supported"), and §15.5.6 does not permit omitting the header — so the header
// must be present and empty, never absent.
func TestMethodNotAllowedAlwaysCarriesAllowHeader(t *testing.T) {
	s := New(Deps{})

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest("BREW", "/v1/not-a-route", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body=%q", rr.Code, rr.Body.String())
	}
	values, present := rr.Header()["Allow"]
	if !present {
		t.Fatalf("405 omitted the Allow header (RFC 9110 §15.5.6); headers=%v", rr.Header())
	}
	if len(values) != 1 || values[0] != "" {
		t.Fatalf("Allow = %q, want exactly one empty value", values)
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%q", err, rr.Body.String())
	}
	if env.Err.Code != "method_not_allowed" {
		t.Fatalf("error.code = %q, want method_not_allowed", env.Err.Code)
	}
}

// probedMethods mirrors chi's unexported method table. Discover the real one at
// runtime — Router.Handle registers a pattern under every method chi knows, and
// Routes() reports those back by name — and fail if the mirror has drifted (chi
// adding a method, or this repo calling the package-global chi.RegisterMethod,
// whose custom methods the probe cannot otherwise learn about).
func TestProbedMethodsMatchChiMethodTable(t *testing.T) {
	r := chi.NewRouter()
	r.Handle("/probe", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	routes := r.Routes()
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}
	chiMethods := map[string]bool{}
	for m := range routes[0].Handlers {
		if m == "*" { // chi's catch-all marker, not an HTTP method
			continue
		}
		chiMethods[m] = true
	}
	probed := map[string]bool{}
	for _, m := range probedMethods {
		probed[m] = true
	}
	if !reflect.DeepEqual(chiMethods, probed) {
		t.Fatalf("probedMethods = %v, but chi routes %v — update probedMethods", probed, chiMethods)
	}
}

func TestLegacyFallbackRemainsOutsideV1(t *testing.T) {
	legacyCalls := 0
	s := New(Deps{Legacy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legacyCalls++
		w.WriteHeader(http.StatusTeapot)
	})})

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(method, "/legacy-miss", nil))
		if rr.Code != http.StatusTeapot {
			t.Fatalf("%s status = %d, want %d", method, rr.Code, http.StatusTeapot)
		}
	}
	if legacyCalls != 2 {
		t.Fatalf("legacy handler called %d times, want 2", legacyCalls)
	}
}

// The HITL magic-link pages are raw HTML handlers (internal/agent) injected
// via Deps and registered directly on the chi root — without registration,
// routeNotFound would answer them with the JSON 404 envelope and every
// approve/reject link in notification emails would break.
func TestMagicLinkRoutesServed(t *testing.T) {
	calls := 0
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTeapot)
	})
	s := New(Deps{MagicLinkApprove: stub, MagicLinkReject: stub})

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/approve"},
		{http.MethodPost, "/v1/approve"},
		{http.MethodGet, "/v1/reject"},
		{http.MethodPost, "/v1/reject"},
	} {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path+"?t=x", nil))
		if rr.Code != http.StatusTeapot {
			t.Fatalf("%s %s status = %d, want injected magic-link handler (%d); body=%q",
				tc.method, tc.path, rr.Code, http.StatusTeapot, rr.Body.String())
		}
	}
	if calls != 4 {
		t.Fatalf("magic-link handlers called %d times, want 4", calls)
	}
}

// Without the magic-link handlers wired (Deps zero value), the /v1/approve
// and /v1/reject paths fall back to the canonical JSON 404 — they must NOT
// silently reach the legacy mux.
func TestMagicLinkRoutesAbsentWithoutDeps(t *testing.T) {
	legacyCalls := 0
	s := New(Deps{Legacy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legacyCalls++
		w.WriteHeader(http.StatusTeapot)
	})})

	for _, path := range []string{"/v1/approve", "/v1/reject"} {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want 404; body=%q", path, rr.Code, rr.Body.String())
		}
	}
	if legacyCalls != 0 {
		t.Fatalf("legacy handler called %d times for magic-link paths", legacyCalls)
	}
}
