package httpapi

import (
	"reflect"
	"strings"
	"testing"
)

// Slice D: template references on POST /v1/agents/{email}/messages.
// The fixture template (sampleTemplate, tmpl_1 / alias "welcome"):
//   subject:   "Hello {{name}}"
//   body:      "Hi {{name}}, your plan is {{plan.tier}}."
//   html_body: "<p>Hi {{name}}: {{{markup}}}</p>"

func TestSendWithTemplateID(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to":          []string{"alice@x.com"},
		"template_id": "tmpl_1",
		"template_data": map[string]any{
			"name": "A&B", "plan": map[string]any{"tier": "pro"}, "markup": "<i>hi</i>",
		},
	})
	if code != 200 || body["status"] != "sent" {
		t.Fatalf("want 200 sent, got %d %v", code, body)
	}
	// The delivery seam must receive RENDERED content, never template source.
	req := lastDeliveredReq()
	if req.Subject != "Hello A&B" {
		t.Errorf("delivered subject = %q, want rendered %q", req.Subject, "Hello A&B")
	}
	if req.Body != "Hi A&B, your plan is pro." {
		t.Errorf("delivered body = %q (dot path + no HTML escaping in text)", req.Body)
	}
	// HTML part: {{name}} escaped, {{{markup}}} raw.
	if req.HTMLBody != "<p>Hi A&amp;B: <i>hi</i></p>" {
		t.Errorf("delivered html = %q", req.HTMLBody)
	}
}

func TestSendWithTemplateAlias(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to":             []string{"alice@x.com"},
		"template_alias": "welcome",
		"template_data":  map[string]any{"name": "Zoe"},
	})
	if code != 200 || body["status"] != "sent" {
		t.Fatalf("want 200 sent, got %d %v", code, body)
	}
	req := lastDeliveredReq()
	if req.Subject != "Hello Zoe" {
		t.Errorf("delivered subject = %q", req.Subject)
	}
	// Missing vars render empty (permissive model).
	if req.Body != "Hi Zoe, your plan is ." {
		t.Errorf("delivered body = %q, want missing plan.tier as empty", req.Body)
	}
}

// TestSendTemplateHeldShowsRenderedBody: rendering happens BEFORE the hold —
// the fake DeliverOutbound (which owns the HITL hold, persisting the draft
// verbatim) holds when the subject contains HOLD, and must already see the
// rendered content the reviewer will be shown.
func TestSendTemplateHeldShowsRenderedBody(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to":            []string{"alice@x.com"},
		"template_id":   "tmpl_1",
		"template_data": map[string]any{"name": "HOLD reviewer", "plan": map[string]any{"tier": "pro"}},
	})
	if code != 202 || body["status"] != "pending_review" {
		t.Fatalf("want 202 pending_review, got %d %v", code, body)
	}
	req := lastDeliveredReq()
	if req.Subject != "Hello HOLD reviewer" || req.Body != "Hi HOLD reviewer, your plan is pro." {
		t.Errorf("held draft must carry rendered content, got subject=%q body=%q", req.Subject, req.Body)
	}
	if strings.Contains(req.Body, "{{") {
		t.Errorf("held draft still contains template syntax: %q", req.Body)
	}
}

func TestSendTemplateIDAndAliasMutuallyExclusive(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "template_id": "tmpl_1", "template_alias": "welcome",
	})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("want 400 invalid_request, got %d %v", code, body)
	}
}

func TestSendTemplateExclusiveWithLiteralContent(t *testing.T) {
	srv := testServer(t)
	for name, extra := range map[string]map[string]any{
		"subject": {"subject": "literal"},
		"text":    {"text": "literal"},
		"html":    {"html": "<p>literal</p>"},
	} {
		payload := map[string]any{"to": []string{"alice@x.com"}, "template_id": "tmpl_1"}
		for k, v := range extra {
			payload[k] = v
		}
		code, body := postJSON(t, srv.URL+sendURL, "good", payload)
		if code != 400 || errCode(body) != "invalid_request" {
			t.Fatalf("%s + template_id: want 400 invalid_request, got %d %v", name, code, body)
		}
	}
}

func TestSendTemplateDataWithoutReference(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "subject": "S", "text": "B",
		"template_data": map[string]any{"name": "x"},
	})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("want 400 invalid_request, got %d %v", code, body)
	}
}

func TestSendTemplateNotFound(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "template_id": "tmpl_missing",
	})
	if code != 404 || errCode(body) != "template_not_found" {
		t.Fatalf("want 404 template_not_found, got %d %v", code, body)
	}
	code, body = postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "template_alias": "no-such-alias",
	})
	if code != 404 || errCode(body) != "template_not_found" {
		t.Fatalf("alias: want 404 template_not_found, got %d %v", code, body)
	}
}

// TestSendOmittedSubjectBodyKeys pins the wire contract for the schema
// loosening that made templates possible: subject/body moved from
// schema-required (a Huma 422) to handler-enforced, so a literal send with
// the keys ENTIRELY ABSENT — not just "" — must be a 400 invalid_request.
// (TestSendMissingSubjectBody covers the explicit-empty-string case.)
func TestSendOmittedSubjectBodyKeys(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"},
	})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("omitted subject/body keys: want 400 invalid_request, got %d %v", code, body)
	}
}

// TestSendTemplateStoreErrorIs500 pins the miss/failure split on the send
// path: a store error that is NOT ErrTemplateNotFound (timeout, connection
// loss) must surface as 500 internal_error — a 404 would tell the caller
// their template is gone when the DB merely hiccuped.
func TestSendTemplateStoreErrorIs500(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "template_id": "tmpl_dberr",
	})
	if code != 500 || errCode(body) != "internal_error" {
		t.Fatalf("want 500 internal_error for store failure, got %d %v", code, body)
	}
}

func TestSendTemplateRenderFailureBadSource(t *testing.T) {
	srv := testServer(t)
	// tmpl_stale's stored body no longer parses ({{#section}}).
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "template_id": "tmpl_stale",
	})
	if code != 400 || errCode(body) != "template_render_failed" {
		t.Fatalf("want 400 template_render_failed, got %d %v", code, body)
	}
	details, _ := body["error"].(map[string]any)["details"].(map[string]any)
	if details["part"] != "text" {
		t.Fatalf("details must name the failing part, got %v", body)
	}
}

func TestSendTemplateRenderFailureDeepData(t *testing.T) {
	srv := testServer(t)
	// template_data nested past the depth cap fails at render.
	deep := map[string]any{}
	cur := deep
	for i := 0; i < 9; i++ {
		next := map[string]any{}
		cur["k"] = next
		cur = next
	}
	cur["leaf"] = "v"
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "template_id": "tmpl_1", "template_data": deep,
	})
	if code != 400 || errCode(body) != "template_render_failed" {
		t.Fatalf("want 400 template_render_failed for deep data, got %d %v", code, body)
	}
}

// TestSendTemplateRenderedEmptySubject: a part that renders EMPTY (variable-
// only source + no data) is a specific 400 template_rendered_empty naming
// the part and its variables — not the generic "subject and body are
// required", which is baffling for a templated send.
func TestSendTemplateRenderedEmptySubject(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "template_id": "tmpl_onlyvar",
		"template_data": map[string]any{},
	})
	if code != 400 || errCode(body) != "template_rendered_empty" {
		t.Fatalf("want 400 template_rendered_empty, got %d %v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "subject") || !strings.Contains(msg, "name") {
		t.Errorf("message must name the empty part and its variables, got %q", msg)
	}
	details, _ := errObj["details"].(map[string]any)
	if details["part"] != "subject" {
		t.Errorf("details.part = %v, want subject", details["part"])
	}
	vars, _ := details["variables"].([]any)
	if len(vars) != 1 || vars[0] != "name" {
		t.Errorf("details.variables = %v, want [name]", details["variables"])
	}
}

// TestSendTemplateRenderedSubjectStillValidated: rendered values flow through
// the same validateOutboundBody checks as literal ones — data that smuggles
// CR/LF into the subject is rejected exactly like a literal CR/LF subject.
func TestSendTemplateRenderedSubjectStillValidated(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to":            []string{"alice@x.com"},
		"template_id":   "tmpl_1",
		"template_data": map[string]any{"name": "x\r\nInjected: header"},
	})
	if code != 400 || errCode(body) != "invalid_request" {
		t.Fatalf("want 400 invalid_request for rendered CRLF subject, got %d %v", code, body)
	}
}

// TestValidatePreviewMatchesSendRender is the differential guard on the
// shared templateParts table: the validate endpoint's rendered preview and
// the send path's delivered content must be byte-equal for the same source +
// data — if the two surfaces ever disagreed on escape modes, a user could
// approve a preview that differs from what actually ships.
func TestValidatePreviewMatchesSendRender(t *testing.T) {
	srv := testServer(t)
	tp := sampleTemplate()
	data := map[string]any{
		"name": "A&B <script>", "plan": map[string]any{"tier": "pro"}, "markup": "<i>hi</i>",
	}

	code, body := postJSON(t, srv.URL+"/v1/templates/validate", "good", map[string]any{
		"subject": tp.Subject, "text": tp.Body, "html": tp.HTMLBody,
		"test_data": data,
	})
	if code != 200 || body["valid"] != true {
		t.Fatalf("validate: want 200 valid, got %d %v", code, body)
	}
	preview, _ := body["rendered"].(map[string]any)

	code, sendBody := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to": []string{"alice@x.com"}, "template_id": "tmpl_1", "template_data": data,
	})
	if code != 200 {
		t.Fatalf("send: want 200, got %d %v", code, sendBody)
	}
	delivered := lastDeliveredReq()

	for part, got := range map[string]string{
		"subject": delivered.Subject,
		"text":    delivered.Body,
		"html":    delivered.HTMLBody,
	} {
		want, _ := preview[part].(string)
		if got != want {
			t.Errorf("%s: delivered %q != validate preview %q", part, got, want)
		}
	}
}

// TestSendTemplateBigIntPrecision: template_data decodes with UseNumber, so
// integers beyond float64's 2^53 mantissa render digit-exact (plain
// encoding/json would deliver "123456789012345680").
func TestSendTemplateBigIntPrecision(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+sendURL, "good", map[string]any{
		"to":            []string{"alice@x.com"},
		"template_id":   "tmpl_1",
		"template_data": map[string]any{"name": int64(123456789012345678)},
	})
	if code != 200 || body["status"] != "sent" {
		t.Fatalf("want 200 sent, got %d %v", code, body)
	}
	req := lastDeliveredReq()
	if req.Subject != "Hello 123456789012345678" {
		t.Errorf("delivered subject = %q, want exact big-int digits", req.Subject)
	}
}

// TestReplyRequestHasNoTemplateFields locks the scope decision: template
// references exist on SendEmailRequest only, not reply/forward.
func TestReplyRequestHasNoTemplateFields(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf(ReplyRequest{}),
		reflect.TypeOf(ForwardRequest{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			if strings.HasPrefix(typ.Field(i).Name, "Template") {
				t.Errorf("%s must not carry template fields, found %s", typ.Name(), typ.Field(i).Name)
			}
		}
	}
}

// TestTemplateBetaDocOnSendFields guards the beta-marking convention: every
// template field on the send body carries the templatesBetaDoc sentence.
func TestTemplateBetaDocOnSendFields(t *testing.T) {
	typ := reflect.TypeOf(SendEmailRequest{})
	for _, name := range []string{"TemplateID", "TemplateAlias", "TemplateData"} {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Fatalf("SendEmailRequest missing field %s", name)
		}
		if !strings.Contains(f.Tag.Get("doc"), templatesBetaDoc) {
			t.Errorf("%s doc tag must contain the beta sentence %q", name, templatesBetaDoc)
		}
	}
}
