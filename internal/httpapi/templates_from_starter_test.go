package httpapi

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/startertemplates"
)

// from_starter on POST /v1/templates: copy a starter master verbatim into the
// caller's template library.

func TestCreateTemplateFromStarter(t *testing.T) {
	srv := testServer(t)
	master, ok := startertemplates.Get("receipt")
	if !ok {
		t.Fatal("receipt master missing from catalog")
	}
	code, body := postJSON(t, srv.URL+"/v1/templates", "good", map[string]any{
		"from_starter": "receipt",
	})
	if code != 201 {
		t.Fatalf("want 201, got %d %v", code, body)
	}
	// Name and alias default to the master's; content is copied VERBATIM.
	if body["name"] != master.Name || body["alias"] != master.Alias {
		t.Fatalf("name/alias must default to the master's, got %v/%v", body["name"], body["alias"])
	}
	if body["subject"] != master.Subject {
		t.Errorf("subject = %v, want master subject %q", body["subject"], master.Subject)
	}
	if body["text"] != master.TextBody {
		t.Errorf("body does not equal the master text source verbatim")
	}
	if body["html"] != master.HTMLBody {
		t.Errorf("html_body does not equal the master HTML source verbatim")
	}
	// Provenance: the response records which master (and version) was copied.
	if body["from_starter_alias"] != master.Alias || body["from_starter_version"] != master.Version {
		t.Errorf("want provenance %s@%s, got %v/%v",
			master.Alias, master.Version, body["from_starter_alias"], body["from_starter_version"])
	}
}

// TestCreateTemplateLiteralHasNoProvenance: a literal create carries no
// from_starter_* fields (omitted, not empty strings).
func TestCreateTemplateLiteralHasNoProvenance(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/templates", "good", map[string]any{
		"name": "Literal", "subject": "S", "text": "B",
	})
	if code != 201 {
		t.Fatalf("want 201, got %d %v", code, body)
	}
	for _, k := range []string{"from_starter_alias", "from_starter_version"} {
		if _, present := body[k]; present {
			t.Errorf("literal create must omit %q, got %v", k, body)
		}
	}
}

func TestCreateTemplateFromStarterOverrides(t *testing.T) {
	srv := testServer(t)
	master, _ := startertemplates.Get("receipt")
	code, body := postJSON(t, srv.URL+"/v1/templates", "good", map[string]any{
		"from_starter": "receipt", "name": "My Receipt", "alias": "my-receipt",
	})
	if code != 201 {
		t.Fatalf("want 201, got %d %v", code, body)
	}
	if body["name"] != "My Receipt" || body["alias"] != "my-receipt" {
		t.Fatalf("name/alias overrides not applied: %v/%v", body["name"], body["alias"])
	}
	if body["subject"] != master.Subject {
		t.Errorf("content must still be the master's verbatim, got subject %v", body["subject"])
	}
}

func TestCreateTemplateFromStarterExclusiveWithLiteralSource(t *testing.T) {
	srv := testServer(t)
	for part, extra := range map[string]map[string]any{
		"subject": {"subject": "literal"},
		"text":    {"text": "literal"},
		"html":    {"html": "<p>literal</p>"},
	} {
		payload := map[string]any{"from_starter": "receipt"}
		for k, v := range extra {
			payload[k] = v
		}
		code, body := postJSON(t, srv.URL+"/v1/templates", "good", payload)
		if code != 400 || errCode(body) != "invalid_request" {
			t.Fatalf("%s + from_starter: want 400 invalid_request, got %d %v", part, code, body)
		}
	}
}

func TestCreateTemplateFromStarterUnknown(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/templates", "good", map[string]any{
		"from_starter": "no-such-starter",
	})
	if code != 404 || errCode(body) != "starter_template_not_found" {
		t.Fatalf("want 404 starter_template_not_found, got %d %v", code, body)
	}
}

// TestCreateTemplateFromStarterDefaultAliasConflict: the fixture user already
// owns a template with alias "welcome" (tmpl_1), so defaulting to the
// `welcome` starter's alias surfaces the existing 409 — the caller picks
// another alias via the override.
func TestCreateTemplateFromStarterDefaultAliasConflict(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/templates", "good", map[string]any{
		"from_starter": "welcome",
	})
	if code != 409 || errCode(body) != "alias_taken" {
		t.Fatalf("want 409 alias_taken, got %d %v", code, body)
	}
	// An alias override resolves the conflict.
	code, body = postJSON(t, srv.URL+"/v1/templates", "good", map[string]any{
		"from_starter": "welcome", "alias": "welcome-copy",
	})
	if code != 201 || body["alias"] != "welcome-copy" {
		t.Fatalf("override: want 201 welcome-copy, got %d %v", code, body)
	}
}

// TestCreateTemplateFromStarterCapEnforced: a starter-derived template is an
// ordinary user template — it counts against max_templates and the cap error
// is unchanged.
func TestCreateTemplateFromStarterCapEnforced(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/templates", "overcap", map[string]any{
		"from_starter": "receipt",
	})
	if code != 400 || errCode(body) != "template_limit_reached" {
		t.Fatalf("want 400 template_limit_reached, got %d %v", code, body)
	}
}

// TestTemplateBetaDocOnFromStarter guards the beta-marking convention on the
// new create field, mirroring TestTemplateBetaDocOnSendFields.
func TestTemplateBetaDocOnFromStarter(t *testing.T) {
	f, ok := reflect.TypeOf(CreateTemplateRequest{}).FieldByName("FromStarter")
	if !ok {
		t.Fatal("CreateTemplateRequest missing field FromStarter")
	}
	if !strings.Contains(f.Tag.Get("doc"), templatesBetaDoc) {
		t.Errorf("FromStarter doc tag must contain the beta sentence %q", templatesBetaDoc)
	}
}

// TestSendWithStarterDerivedTemplate: end-to-end templated send through a
// starter-derived template (slice-D pattern) — the fixture alias
// "starter-welcome" is a verbatim copy of the `welcome` master. Sending with
// full variable data must deliver fully rendered content at the delivery seam.
func TestSendWithStarterDerivedTemplate(t *testing.T) {
	srv := testServer(t)
	master, ok := startertemplates.Get("welcome")
	if !ok {
		t.Fatal("welcome master missing from catalog")
	}
	data := make(map[string]any, len(master.Variables))
	for _, v := range master.Variables {
		data[v.Name] = v.Example
	}
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to":             []string{"alice@x.com"},
		"template_alias": "starter-welcome",
		"template_data":  data,
	})
	if code != 200 || body["status"] != "sent" {
		t.Fatalf("want 200 sent, got %d %v", code, body)
	}
	req := lastDeliveredReq()
	// The example values are fixed in meta.json (company_name=Acme etc.), so
	// concrete rendered content is assertable at the seam.
	if req.Subject != "Welcome to Acme" {
		t.Errorf("delivered subject = %q, want %q", req.Subject, "Welcome to Acme")
	}
	if !strings.Contains(req.Body, "Welcome to Acme, Sam — your account is ready.") {
		t.Errorf("delivered text body missing rendered greeting: %q", req.Body)
	}
	if !strings.Contains(req.HTMLBody, "https://app.acme.com/onboarding") ||
		!strings.Contains(req.HTMLBody, "Set up your workspace") {
		t.Errorf("delivered html body missing rendered CTA")
	}
	for part, out := range map[string]string{"subject": req.Subject, "text": req.Body, "html": req.HTMLBody} {
		if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
			t.Errorf("delivered %s still contains template syntax", part)
		}
	}
}
