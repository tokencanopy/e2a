package httpapi

import (
	"bytes"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// updateSpec regenerates the committed OpenAPI golden instead of asserting
// against it: `go test ./internal/httpapi -update-spec` (or `make spec`).
var updateSpec = flag.Bool("update-spec", false, "regenerate the committed OpenAPI spec at api/openapi.yaml")

// specGoldenPath is the committed source-of-truth /v1 spec, relative to this
// package dir. It is the artifact SDK codegen + the docs renderer consume; the
// drift gate below guarantees it always equals what the live handlers emit.
const specGoldenPath = "../../api/openapi.yaml"

// TestSpecGoldenNoDrift is the contract-drift CI gate (api-v1-redesign §6): the
// committed spec must byte-for-byte equal the spec rendered from the live
// handlers, so the file codegen consumes can never lag the server. Regenerate
// with `make spec` after any handler/annotation change.
func TestSpecGoldenNoDrift(t *testing.T) {
	// APIURL is set here — not hardcoded in httpapi.go — to represent the
	// canonical hosted product's own `servers` entry (api-v1-redesign §1)
	// in the checked-in reference document. A real deployment (hosted or
	// self-hosted) supplies its OWN config.HTTP.APIURL at runtime; this
	// golden file is just this repo's documentation snapshot for the
	// upstream product, not a value every deployment inherits.
	yaml, err := New(Deps{APIURL: "https://api.e2a.dev"}).OpenAPIYAML()
	if err != nil {
		t.Fatalf("render spec: %v", err)
	}
	if *updateSpec {
		if err := os.WriteFile(specGoldenPath, yaml, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("regenerated %s (%d bytes)", specGoldenPath, len(yaml))
		return
	}
	want, err := os.ReadFile(specGoldenPath)
	if err != nil {
		t.Fatalf("read spec golden (first time? run `make spec`): %v", err)
	}
	if !bytes.Equal(bytes.TrimRight(want, "\n"), bytes.TrimRight(yaml, "\n")) {
		t.Errorf("committed %s is stale vs the live handlers — run `make spec` to regenerate", specGoldenPath)
	}
}

// TestSpecServersReflectsDeployment is the regression test for the OpenAPI
// `servers` block: every deployment — hosted AND self-hosted — serves this
// document at /v1/openapi and /v1/docs, so it must never advertise a
// hardcoded operator host regardless of Deps.APIURL. A self-hoster's own
// docs page rendering "api.e2a.dev" as the sole server would fire the
// reader's bearer at the operator's API via the try-it button.
func TestSpecServersReflectsDeployment(t *testing.T) {
	t.Run("advertises this deployment's own configured API host", func(t *testing.T) {
		oapi := New(Deps{APIURL: "https://api.selfhost.example.test"}).API.OpenAPI()
		if len(oapi.Servers) != 1 || oapi.Servers[0].URL != "https://api.selfhost.example.test" {
			t.Fatalf("servers = %+v, want exactly [https://api.selfhost.example.test]", oapi.Servers)
		}
	})

	t.Run("advertises no server — never a guessed operator host — when APIURL is unset", func(t *testing.T) {
		oapi := New(Deps{}).API.OpenAPI()
		for _, s := range oapi.Servers {
			if strings.Contains(s.URL, "api.e2a.dev") {
				t.Fatalf("servers = %+v: must never default to the operator's api.e2a.dev when APIURL is unset", oapi.Servers)
			}
		}
	})
}

// TestSpecGeneratedFromHandlers is the spec↔server check (api-v1-redesign §6):
// the OpenAPI document is emitted from the live, registered handlers — never
// hand-authored — so it cannot drift from what the server actually serves.
// Every registered operation must appear in the generated spec.
func TestSpecGeneratedFromHandlers(t *testing.T) {
	s := New(Deps{}) // no deps needed to render the spec
	yaml, err := s.OpenAPIYAML()
	if err != nil {
		t.Fatalf("render spec: %v", err)
	}
	spec := string(yaml)

	mustContain := []string{
		"openapi: 3.1.0",
		"operationId: getInfo",
		"operationId: listAgents",
		"operationId: getAgent",
		"operationId: createAgent",
		"operationId: updateAgent",
		"operationId: deleteAgent",
		"operationId: getMessage",
		"operationId: listMessages",
		"operationId: listConversations",
		"operationId: getConversation",
		"operationId: listDomains",
		"operationId: registerDomain",
		"operationId: deleteDomain",
		"operationId: verifyDomain",
		"operationId: createWebhook",
		"operationId: listWebhooks",
		"operationId: deleteWebhook",
		"operationId: updateWebhook",
		"operationId: rotateWebhookSecret",
		"operationId: testWebhook",
		"operationId: listWebhookDeliveries",
		"operationId: listEvents",
		"operationId: getEvent",
		"operationId: redeliverEvent",
		"operationId: getAccount",
		"operationId: exportAccount",
		"operationId: deleteAccount",
		"operationId: sendMessage",
		"operationId: replyToMessage",
		"operationId: forwardMessage",
		"operationId: listReviews",
		"operationId: getReview",
		"operationId: approveReview",
		"operationId: rejectReview",
		"operationId: testAgent",
		"/v1/account",
		"/v1/events",
		"/v1/events/{id}",
		"/v1/webhooks",
		"/v1/webhooks/{id}",
		"/v1/domains/{domain}/verify",
		"/v1/domains",
		"/v1/domains/{domain}",
		"/v1/info",
		"/v1/agents",
		"/v1/agents/{email}",
		"/v1/agents/{email}/messages",
		"/v1/agents/{email}/messages/{id}",
		// The single Bearer security scheme is declared.
		"securitySchemes",
		"bearer",
	}
	for _, want := range mustContain {
		if !strings.Contains(spec, want) {
			t.Errorf("generated spec missing %q", want)
		}
	}

	// Retired routes must NOT reappear: /v1/send relocated to
	// POST /v1/agents/{address}/messages (decision 3); the /v1/users/me/* cluster
	// was renamed to /v1/account (decision 8). Guard against a regression that
	// re-registers either.
	for _, gone := range []string{"/v1/send", "/v1/users/me"} {
		if strings.Contains(spec, gone) {
			t.Errorf("generated spec still contains retired route %q", gone)
		}
	}
}

// TestSpecServedOverHTTP confirms the spec is reachable at the versioned
// path so SDK/MCP codegen and the docs renderer can fetch it from a running
// instance.
func TestSpecServedOverHTTP(t *testing.T) {
	srv := httptest.NewServer(New(Deps{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "openapi: 3.1.0") {
		t.Fatalf("served spec is not OpenAPI 3.1: %.80s", b)
	}
}
