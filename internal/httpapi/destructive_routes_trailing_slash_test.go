package httpapi

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// specParamPattern matches the {param} placeholders in an OpenAPI path template.
var specParamPattern = regexp.MustCompile(`\{[^}]+\}`)

// TestDestructiveRoutesRejectTrailingSlash pins the routing behavior the
// dot-segment collapse depends on. A client that interpolates ".." into a path
// segment has it resolved by the URL parser before the request leaves, so
// DELETE /v1/account/api-keys/.. reaches the server as DELETE /v1/account/ —
// the destructive parent route, carrying a trailing slash. chi matches routes
// exactly and does not redirect trailing slashes, so that form matches nothing
// and never reaches a handler.
//
// Nothing asserted it, so a chi upgrade or a StrictSlash-style change would
// make every collapse live at once with no test failing. The path without the
// trailing slash is asserted to match first: a 404 on both forms would mean the
// path never matched anything, and the test would pass for the wrong reason.
func TestDestructiveRoutesRejectTrailingSlash(t *testing.T) {
	srv := testServer(t)
	defer srv.Close()

	paths := specDeletePaths(t)
	if len(paths) == 0 {
		t.Fatal("no DELETE paths in the committed spec")
	}

	for _, specPath := range paths {
		path := specParamPattern.ReplaceAllString(specPath, "x")
		t.Run(specPath, func(t *testing.T) {
			// An unauthorized request that matched a route stops at the auth
			// middleware with 401; one that matched nothing is a router 404.
			if code, body := sendJSON(t, http.MethodDelete, srv.URL+path+"?confirm=DELETE", "unauthorized", nil); code == http.StatusNotFound {
				t.Fatalf("DELETE %s = 404 (%v), so the path did not match a destructive route and the trailing-slash assertion below would be vacuous", path, body)
			}

			if code, body := sendJSON(t, http.MethodDelete, srv.URL+path+"/?confirm=DELETE", "unauthorized", nil); code != http.StatusNotFound {
				t.Fatalf("DELETE %s/ = %d (%v), want 404: a trailing slash must not reach the handler", path, code, body)
			}
		})
	}
}

// specDeletePaths reads the committed v1 spec and returns the paths that
// declare a DELETE operation, so a new destructive route is covered without
// editing this test.
func specDeletePaths(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(specGoldenPath)
	if err != nil {
		t.Fatal(err)
	}

	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}

	paths := make([]string, 0, len(spec.Paths))
	for path, operations := range spec.Paths {
		if _, ok := operations["delete"]; ok {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	return paths
}
