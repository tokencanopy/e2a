package httpapi

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/webhookpub"
)

var betaOperationIDs = []string{
	"approveReview",
	"createAgentSuppression",
	"createContact",
	"createTemplate",
	"deleteAgentSuppression",
	"deleteContact",
	"deleteEngagement",
	"deleteImportBatch",
	"deleteTemplate",
	"getAccountMetrics",
	"getAgentMetrics",
	"getAgentProtection",
	"getContact",
	"getEngagement",
	"importContacts",
	"getMessageLifecycle",
	"getReview",
	"getStarterTemplate",
	"getTemplate",
	"listAgentSuppressions",
	"listContacts",
	"listEngagements",
	"listReviews",
	"listScheduledMessages",
	"listStarterTemplates",
	"listTemplates",
	"putAgentProtection",
	"rejectReview",
	"updateContact",
	"updateTemplate",
	"upsertEngagement",
	"validateTemplate",
}

// These tests pin the forward-compatibility stance stamped by
// applyEvolutionStance (GA review #22/#23) so it can't silently regress:
//   - response schemas open (additionalProperties: true), request schemas
//     strict (additionalProperties: false), and never both for one schema;
//   - canonical x-stability-level: beta on every beta operation, derived onto
//     schemas only they use, with no duplicate lifecycle extension;
//   - x-experimental-values on the event-type fields whose value set has beta
//     members.

// specReachability recomputes request-body vs response-body schema
// reachability from the rendered document (independently of the stamping
// code, so the test doesn't just re-run the implementation).
func specReachability(t *testing.T, doc map[string]any) (request, response map[string]bool) {
	t.Helper()
	comps, _ := doc["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)

	var refsIn func(node any, out map[string]bool)
	refsIn = func(node any, out map[string]bool) {
		switch n := node.(type) {
		case map[string]any:
			if ref, ok := n["$ref"].(string); ok {
				const schemaPrefix = "#/components/schemas/"
				if strings.HasPrefix(ref, schemaPrefix) {
					out[strings.TrimPrefix(ref, schemaPrefix)] = true
				}
			}
			for _, v := range n {
				refsIn(v, out)
			}
		case []any:
			for _, v := range n {
				refsIn(v, out)
			}
		}
	}
	closure := func(roots map[string]bool) map[string]bool {
		seen := map[string]bool{}
		stack := make([]string, 0, len(roots))
		for name := range roots {
			stack = append(stack, name)
		}
		for len(stack) > 0 {
			name := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[name] {
				continue
			}
			seen[name] = true
			sc, ok := schemas[name].(map[string]any)
			if !ok {
				t.Fatalf("schema %q referenced but not defined", name)
			}
			next := map[string]bool{}
			refsIn(sc, next)
			for n := range next {
				if !seen[n] {
					stack = append(stack, n)
				}
			}
		}
		return seen
	}

	reqRoots, respRoots := map[string]bool{}, map[string]bool{}
	paths, _ := doc["paths"].(map[string]any)
	for _, pi := range paths {
		item, _ := pi.(map[string]any)
		for _, op := range item {
			opm, ok := op.(map[string]any)
			if !ok {
				continue
			}
			refsIn(opm["requestBody"], reqRoots)
			refsIn(opm["responses"], respRoots)
		}
	}
	return closure(reqRoots), closure(respRoots)
}

// The stance itself: every response-reachable object schema tolerates unknown
// fields, every request-reachable one rejects them, and no schema is both.
func TestSpecEvolutionStance(t *testing.T) {
	doc := renderSpec(t)
	request, response := specReachability(t, doc)

	for name := range response {
		if request[name] {
			t.Errorf("schema %q is reachable from both a request and a response body — split the Go type (see stability.go)", name)
		}
	}
	if len(request) == 0 || len(response) == 0 {
		t.Fatal("reachability came back empty — test wiring is wrong")
	}

	comps, _ := doc["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)
	var checkObjects func(name string, node any, wantOpen bool)
	checkObjects = func(name string, node any, wantOpen bool) {
		switch n := node.(type) {
		case map[string]any:
			if n["type"] == "object" {
				if _, isStruct := n["properties"]; isStruct {
					ap := n["additionalProperties"]
					if wantOpen && ap != true {
						t.Errorf("%s: response object schema must carry additionalProperties: true (got %v) — clients must tolerate additive fields", name, ap)
					}
					if !wantOpen && ap != false {
						t.Errorf("%s: request object schema must carry additionalProperties: false (got %v) — strict input validation is intentional", name, ap)
					}
				}
			}
			for _, v := range n {
				checkObjects(name, v, wantOpen)
			}
		case []any:
			for _, v := range n {
				checkObjects(name, v, wantOpen)
			}
		}
	}
	for name := range response {
		checkObjects(name, schemas[name], true)
	}
	for name := range request {
		checkObjects(name, schemas[name], false)
	}

	// Anchor both halves on the two operations whose behavior is most
	// load-bearing, so an accidental inversion is caught by name.
	if sc, _ := schemas["SendEmailRequest"].(map[string]any); sc["additionalProperties"] != false {
		t.Error("SendEmailRequest must stay strict (unknown field like `body` -> 422)")
	}
	if sc, _ := schemas["MessageView"].(map[string]any); sc["additionalProperties"] != true {
		t.Error("MessageView must be open (server may add response fields additively)")
	}
}

// The stance must also hold for operation-UNREACHABLE components. Most typed
// per-event payload schemas (Email*Data / Domain*Data, published by
// registerEventPayloadSchemas) are consumer-direction (server → client) but
// referenced by no operation's request or response, so the response-reachability
// pass in applyEvolutionStance never sees them — they open themselves at
// registration. Shared components can also become operation-reachable over
// time: AttachmentMetaView through account export, and now
// MessageLifecycleTransition through the lifecycle endpoint. This test closes
// the gap the two-pass
// design leaves: every component schema that is not reachable from a request
// body (i.e. everything that is not strict-by-design input) must be open, so
// neither enforcement point can drift without failing here.
func TestSpecEvolutionStanceCoversUnreachableComponents(t *testing.T) {
	doc := renderSpec(t)
	request, response := specReachability(t, doc)

	comps, _ := doc["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)

	var checkOpen func(name string, node any)
	checkOpen = func(name string, node any) {
		switch n := node.(type) {
		case map[string]any:
			if _, isRef := n["$ref"]; isRef {
				return
			}
			if n["type"] == "object" {
				if _, isStruct := n["properties"]; isStruct {
					if ap := n["additionalProperties"]; ap != true {
						t.Errorf("%s: non-request component object schema must carry additionalProperties: true (got %v) — consumers must tolerate additive fields", name, ap)
					}
				}
			}
			for _, v := range n {
				checkOpen(name, v)
			}
		case []any:
			for _, v := range n {
				checkOpen(name, v)
			}
		}
	}

	unreachable := map[string]bool{}
	for name := range schemas {
		if request[name] {
			continue // strict by design (input side of the stance)
		}
		checkOpen(name, schemas[name])
		if !response[name] {
			unreachable[name] = true
		}
	}

	// Anchor: event payload components must exist and remain response-only. Most
	// are operation-unreachable documentation/codegen components; the conscious
	// response-reachable exceptions below retain explicit policy assertions.
	//
	// Conscious exception: AttachmentMetaView. Since the user-data export's Message
	// schema typed its `attachments` as []AttachmentMetaView (one shape everywhere),
	// AttachmentMetaView IS response-reachable (GET /v1/account/export → UserExport
	// → Message → AttachmentMetaView) and is opened by the normal response pass in
	// applyEvolutionStance. It must still never become request-reachable.
	for _, name := range eventPayloadComponentNames {
		if _, ok := schemas[name]; !ok {
			t.Errorf("event payload component %s missing from the rendered spec", name)
			continue
		}
		if request[name] {
			t.Errorf("event payload component %s became request-reachable — it would now be forced strict, breaking additive payload evolution", name)
		}
		if name == "AttachmentMetaView" {
			if !response[name] {
				t.Error("AttachmentMetaView expected response-reachable via the export's Message.attachments — if that changed, revisit this exception")
			}
			continue
		}
		if name == "MessageLifecycleTransition" {
			if !response[name] {
				t.Error("MessageLifecycleTransition must remain response-reachable through getMessageLifecycle and mapped event schemas")
			}
			for _, field := range []string{"direction", "stage", "outcome", "reason_code"} {
				key := name + "." + field
				reason, explicitlyClosed := closedResponseEnumAllowlist[key]
				if !explicitlyClosed || !strings.Contains(reason, "versioned contract change") {
					t.Errorf("%s must retain the explicit closed/versioned lifecycle enum policy; got %q", key, reason)
				}
			}
			continue
		}
		if !unreachable[name] {
			t.Errorf("event payload component %s expected to be operation-unreachable (got reachable) — update this test's assumptions consciously", name)
		}
	}
	if len(unreachable) == 0 {
		t.Fatal("no operation-unreachable components found — test wiring is wrong")
	}
}

// x-stability-level: beta on exactly the beta surfaces; no duplicate
// x-stability alias is emitted, and the stable core carries neither marker.
func TestSpecBetaMarkers(t *testing.T) {
	doc := renderSpec(t)

	opFor := func(operationID string) map[string]any {
		paths, _ := doc["paths"].(map[string]any)
		for _, pi := range paths {
			item, _ := pi.(map[string]any)
			for _, op := range item {
				if opm, ok := op.(map[string]any); ok && opm["operationId"] == operationID {
					return opm
				}
			}
		}
		t.Fatalf("operation %q not found", operationID)
		return nil
	}
	opExt := func(operationID, extension string) any { return opFor(operationID)[extension] }

	for _, id := range betaOperationIDs {
		if got := opExt(id, "x-stability"); got != nil {
			t.Errorf("%s must not carry duplicate x-stability alias, got %v", id, got)
		}
		if got := opExt(id, "x-stability-level"); got != "beta" {
			t.Errorf("%s must carry canonical x-stability-level: beta, got %v", id, got)
		}
	}
	const suppressionBetaSentence = "Beta: agent-scoped suppression management may change before it is declared stable."
	for _, id := range []string{"listAgentSuppressions", "createAgentSuppression", "deleteAgentSuppression"} {
		op := opFor(id)
		if summary, _ := op["summary"].(string); !strings.Contains(summary, "(beta)") {
			t.Errorf("%s summary must visibly say (beta), got %q", id, summary)
		}
		if desc, _ := op["description"].(string); !strings.Contains(desc, suppressionBetaSentence) {
			t.Errorf("%s description must contain shared beta sentence, got %q", id, desc)
		}
	}
	lifecycleOp := opFor("getMessageLifecycle")
	if summary, _ := lifecycleOp["summary"].(string); !strings.Contains(summary, "(beta)") {
		t.Errorf("getMessageLifecycle summary must visibly say (beta), got %q", summary)
	}
	if desc, _ := lifecycleOp["description"].(string); !strings.Contains(desc, "Beta: message lifecycle") {
		t.Errorf("getMessageLifecycle description must describe its beta status, got %q", desc)
	}
	for _, id := range []string{"sendMessage", "replyToMessage", "forwardMessage", "listSuppressions", "deleteSuppression", "createAgent", "listMessages", "createWebhook", "listEvents", "deleteMessage", "restoreMessage", "restoreAgent", "deleteAgent"} {
		if got := opExt(id, "x-stability"); got != nil {
			t.Errorf("%s is stable GA surface and must NOT carry x-stability, got %v", id, got)
		}
		if got := opExt(id, "x-stability-level"); got != nil {
			t.Errorf("%s is stable GA surface and must NOT carry x-stability-level, got %v", id, got)
		}
	}

	// listMessages stays GA at the operation level, but its `filter` query
	// parameter is beta (the grammar may still evolve). The marker rides on
	// the parameter's inline schema — huma.Param has no extension slot.
	{
		params, _ := opFor("listMessages")["parameters"].([]any)
		var filterSchema map[string]any
		for _, p := range params {
			if pm, _ := p.(map[string]any); pm["name"] == "filter" {
				filterSchema, _ = pm["schema"].(map[string]any)
			}
		}
		if filterSchema == nil {
			t.Fatalf("listMessages filter parameter (with inline schema) not found")
		}
		if got := filterSchema["x-stability-level"]; got != "beta" {
			t.Errorf("listMessages filter parameter schema must carry x-stability-level: beta, got %v", got)
		}
	}

	comps, _ := doc["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)
	schemaExt := func(name, extension string) any {
		sc, ok := schemas[name].(map[string]any)
		if !ok {
			t.Fatalf("schema %q not found", name)
		}
		return sc[extension]
	}
	for _, name := range []string{"AgentSuppressionView", "CreateAgentSuppressionRequest", "PageAgentSuppressionView", "UnsubscribeOptions", "TemplateView", "CreateTemplateRequest", "StarterTemplateView", "ProtectionConfigView", "ProtectionConfigRequest", "ReviewView", "PageReviewView", "ApproveRequest", "RejectRequest", "RejectResultView", "HoldReasonView", "ProtectionFindingView", "ThreatCategoryView", "MessageLifecycleTransition", "PageMessageLifecycleTransition"} {
		if got := schemaExt(name, "x-stability"); got != nil {
			t.Errorf("schema %s must not carry duplicate x-stability alias, got %v", name, got)
		}
		if got := schemaExt(name, "x-stability-level"); got != "beta" {
			t.Errorf("schema %s must carry canonical x-stability-level: beta, got %v", name, got)
		}
	}

	// Lifecycle data is beta even when embedded in an otherwise-stable event
	// payload. Keep the marker on the optional property so existing event
	// schemas and envelopes retain their GA stability.
	for _, name := range []string{"EmailReceivedData", "EmailSentData", "EmailFailedData", "EmailDeliveredData", "EmailBouncedData", "EmailComplainedData", "DomainSuppressionAddedData"} {
		property, _ := schemaProps(t, doc, name)["lifecycle_transitions"].(map[string]any)
		if property == nil || property["x-stability-level"] != "beta" {
			t.Errorf("%s.lifecycle_transitions must carry canonical x-stability-level: beta", name)
		}
		if property != nil && property["x-stability"] != nil {
			t.Errorf("%s.lifecycle_transitions must not carry duplicate x-stability alias", name)
		}
		if got := schemaExt(name, "x-stability-level"); got != nil {
			t.Errorf("stable parent schema %s must remain unmarked, got %v", name, got)
		}
	}
	for _, name := range []string{"MessageView", "MessageSummaryView", "SendResultView", "AgentView", "WebhookView", "SendEmailRequest", "ReplyRequest", "ForwardRequest", "SuppressionView", "PageSuppressionView", "DeleteSuppressionResult", "ErrorEnvelope", "DeleteMessageResult"} {
		if got := schemaExt(name, "x-stability"); got != nil {
			t.Errorf("schema %s is stable and must NOT carry x-stability, got %v", name, got)
		}
		if got := schemaExt(name, "x-stability-level"); got != nil {
			t.Errorf("schema %s is stable and must NOT carry x-stability-level, got %v", name, got)
		}
	}

	// Field-level: the template hooks on the stable send op are beta.
	props := schemaProps(t, doc, "SendEmailRequest")
	for _, f := range []string{"template_alias", "template_id", "template_data"} {
		p, _ := props[f].(map[string]any)
		if p != nil && p["x-stability"] != nil {
			t.Errorf("SendEmailRequest.%s must not carry duplicate x-stability alias", f)
		}
		if p == nil || p["x-stability-level"] != "beta" {
			t.Errorf("SendEmailRequest.%s must carry canonical x-stability-level: beta", f)
		}
	}

	// Review detail reuses stable MessageView, so its optional technical
	// evidence needs a field-level marker as well as beta nested components.
	messageProps := schemaProps(t, doc, "MessageView")
	protection, _ := messageProps["protection"].(map[string]any)
	if protection == nil || protection["x-stability-level"] != "beta" {
		t.Error("MessageView.protection must carry canonical x-stability-level: beta")
	}
	for _, schema := range []string{"MessageView", "ReviewView"} {
		holdReason, _ := schemaProps(t, doc, schema)["hold_reason"].(map[string]any)
		if holdReason == nil || holdReason["x-stability-level"] != "beta" {
			t.Errorf("%s.hold_reason must carry canonical x-stability-level: beta", schema)
		}
	}

	// A delivered flag verdict remains visible to polling agents, but the two
	// fields are beta while their shape and vocabulary evolve.
	for _, schema := range []string{"MessageView", "MessageSummaryView", "ReviewView"} {
		props := schemaProps(t, doc, schema)
		for _, field := range []string{"flagged", "flag_reason"} {
			property, _ := props[field].(map[string]any)
			if property == nil || property["x-stability-level"] != "beta" {
				t.Errorf("%s.%s must carry canonical x-stability-level: beta", schema, field)
			}
		}
	}

	// The error discriminator remains stable; only the two values produced by
	// controls that ship disabled — the outbound gate policy and the sending
	// abuse pause — are experimental.
	errorCode, _ := schemaProps(t, doc, "ErrorBody")["code"].(map[string]any)
	rawErrorValues, _ := errorCode["x-experimental-values"].([]any)
	if len(rawErrorValues) != 2 || rawErrorValues[0] != "blocked_by_policy" || rawErrorValues[1] != "sending_paused" {
		t.Errorf("ErrorBody.code x-experimental-values = %v, want [blocked_by_policy sending_paused]", rawErrorValues)
	}

	// Managed unsubscribe is a beta opt-in nested inside otherwise-stable
	// outbound request schemas and operations.
	for _, schema := range []string{"SendEmailRequest", "ReplyRequest", "ForwardRequest"} {
		p, _ := schemaProps(t, doc, schema)["unsubscribe"].(map[string]any)
		if p != nil && p["x-stability"] != nil {
			t.Errorf("%s.unsubscribe must not carry duplicate x-stability alias", schema)
		}
		if p == nil || p["x-stability-level"] != "beta" {
			t.Errorf("%s.unsubscribe must carry canonical x-stability-level: beta", schema)
		}
		if desc, _ := p["description"].(string); !strings.Contains(desc, "Beta:") {
			t.Errorf("%s.unsubscribe must describe its beta status for generated SDK docs", schema)
		}
	}
	if schema, _ := schemas["UnsubscribeOptions"].(map[string]any); schema != nil {
		desc, _ := schema["description"].(string)
		if !strings.Contains(desc, "Beta:") {
			t.Error("UnsubscribeOptions must describe its beta status for generated SDK docs")
		}
	}

	// Value-level: the screening + review-hold event types, everywhere event
	// types are enumerated, matching the canonical Go list.
	for _, name := range []string{"CreateWebhookRequest", "UpdateWebhookRequest", "WebhookView", "CreateWebhookResponse"} {
		p, _ := schemaProps(t, doc, name)["events"].(map[string]any)
		if p == nil {
			t.Errorf("%s.events missing", name)
			continue
		}
		raw, _ := p["x-experimental-values"].([]any)
		got := make([]string, 0, len(raw))
		for _, v := range raw {
			s, _ := v.(string)
			got = append(got, s)
		}
		if !setEqual(got, webhookpub.ExperimentalEventTypes...) {
			t.Errorf("%s.events x-experimental-values = %v, want %v", name, got, webhookpub.ExperimentalEventTypes)
		}
	}
}

func TestDocumentedBetaOperationsMatchOpenAPI(t *testing.T) {
	doc := renderSpec(t)
	var marked []string
	paths, _ := doc["paths"].(map[string]any)
	for _, pathItem := range paths {
		item, _ := pathItem.(map[string]any)
		for _, operation := range item {
			op, ok := operation.(map[string]any)
			if !ok || op["x-stability-level"] != "beta" {
				continue
			}
			if id, ok := op["operationId"].(string); ok {
				marked = append(marked, id)
			}
		}
	}
	sort.Strings(marked)
	want := append([]string(nil), betaOperationIDs...)
	sort.Strings(want)
	if !reflect.DeepEqual(marked, want) {
		t.Errorf("OpenAPI beta operations = %v, want exact reviewed inventory %v", marked, want)
	}

	apiDoc, err := os.ReadFile(filepath.Join("..", "..", "docs", "api.md"))
	if err != nil {
		t.Fatalf("read docs/api.md: %v", err)
	}
	text := string(apiDoc)
	start := strings.Index(text, "### Beta operations")
	if start < 0 {
		t.Fatal("docs/api.md is missing the exact Beta operations inventory")
	}
	section := text[start+len("### Beta operations"):]
	if end := strings.Index(section, "\n### "); end >= 0 {
		section = section[:end]
	}
	re := regexp.MustCompile("`([A-Za-z][A-Za-z0-9-]*)`")
	var documented []string
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		if match := re.FindStringSubmatch(cells[1]); len(match) == 2 && match[1] != "operationId" {
			documented = append(documented, match[1])
		}
	}
	sortedDocumented := append([]string(nil), documented...)
	sort.Strings(sortedDocumented)
	if !reflect.DeepEqual(documented, sortedDocumented) {
		t.Errorf("docs/api.md beta operation table must be sorted by operationId: got %v", documented)
	}
	sort.Strings(documented)
	if !reflect.DeepEqual(documented, marked) {
		t.Errorf("docs/api.md beta operations = %v, OpenAPI marks %v", documented, marked)
	}
}

// The stability policy must ship inside the document itself (info.description
// is the contract's constitution).
func TestSpecStabilityPolicyPresent(t *testing.T) {
	doc := renderSpec(t)
	info, _ := doc["info"].(map[string]any)
	desc, _ := info["description"].(string)
	for _, needle := range []string{"Stability policy", "additive", "x-stability-level: beta", "additionalProperties: true", "additionalProperties: false", "x-experimental-values"} {
		if !strings.Contains(desc, needle) {
			t.Errorf("info.description must state the stability policy; missing %q", needle)
		}
	}
}
