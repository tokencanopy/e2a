package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/eventpayload"
)

func TestEventEnvelopeIsOpenAndMapped(t *testing.T) {
	raw, err := json.Marshal(New(Deps{}).API.OpenAPI())
	if err != nil {
		t.Fatalf("render OpenAPI: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	envelope, ok := doc.Components.Schemas["EventEnvelope"]
	if !ok {
		t.Fatal("EventEnvelope component is missing")
	}
	if got := envelope["additionalProperties"]; got != true {
		t.Errorf("EventEnvelope additionalProperties = %#v, want true", got)
	}
	if _, ok := envelope["oneOf"]; ok {
		t.Error("EventEnvelope must not contain oneOf")
	}
	if _, ok := envelope["anyOf"]; ok {
		t.Error("EventEnvelope must not contain anyOf")
	}
	if _, ok := envelope["discriminator"]; ok {
		t.Error("EventEnvelope must not contain a discriminator")
	}

	wantRequired := []string{"type", "id", "schema_version", "created_at", "data"}
	gotRequired := stringsFromAny(t, envelope["required"])
	if !reflect.DeepEqual(gotRequired, wantRequired) {
		t.Errorf("EventEnvelope required = %v, want %v", gotRequired, wantRequired)
	}

	properties, ok := envelope["properties"].(map[string]any)
	if !ok {
		t.Fatalf("EventEnvelope properties = %#v", envelope["properties"])
	}
	for _, name := range []string{"type", "schema_version"} {
		property := properties[name].(map[string]any)
		if property["type"] != "string" {
			t.Errorf("EventEnvelope.%s type = %#v, want string", name, property["type"])
		}
		if _, ok := property["enum"]; ok {
			t.Errorf("EventEnvelope.%s must remain an open string", name)
		}
	}

	data, ok := properties["data"].(map[string]any)
	if !ok {
		t.Fatalf("EventEnvelope.data = %#v", properties["data"])
	}
	if data["type"] != "object" || data["additionalProperties"] != true {
		t.Errorf("EventEnvelope.data must be an open object, got %#v", data)
	}
	mapping, ok := data["x-e2a-event-data-schemas"].(map[string]any)
	if !ok {
		t.Fatalf("EventEnvelope.data mapping = %#v", data["x-e2a-event-data-schemas"])
	}
	if len(mapping) != len(eventpayload.StableEvents) {
		t.Errorf("event mapping has %d entries, want %d", len(mapping), len(eventpayload.StableEvents))
	}
	for _, event := range eventpayload.StableEvents {
		want := "#/components/schemas/" + event.SchemaName
		if got := mapping[event.Type]; got != want {
			t.Errorf("mapping[%s] = %#v, want %q", event.Type, got, want)
		}
	}
}

// TestEventEnvelopePublishesBetaEventPayloads pins the beta half of the
// contract: every catalogued beta payload is a real component, is marked beta
// (so no client mistakes it for frozen), and is reachable from the envelope's
// SEPARATE beta mapping — never from the stable one.
func TestEventEnvelopePublishesBetaEventPayloads(t *testing.T) {
	raw, err := json.Marshal(New(Deps{}).API.OpenAPI())
	if err != nil {
		t.Fatalf("render OpenAPI: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}

	envelope := doc.Components.Schemas["EventEnvelope"]
	data := envelope["properties"].(map[string]any)["data"].(map[string]any)
	mapping, ok := data["x-e2a-beta-event-data-schemas"].(map[string]any)
	if !ok {
		t.Fatalf("beta event mapping = %#v", data["x-e2a-beta-event-data-schemas"])
	}
	if len(mapping) != len(eventpayload.BetaEvents) {
		t.Errorf("beta mapping has %d entries, want %d", len(mapping), len(eventpayload.BetaEvents))
	}
	stableMapping, _ := data["x-e2a-event-data-schemas"].(map[string]any)

	for _, event := range eventpayload.BetaEvents {
		schema, ok := doc.Components.Schemas[event.SchemaName]
		if !ok {
			t.Errorf("%s component is missing", event.SchemaName)
			continue
		}
		if got := schema["x-stability-level"]; got != "beta" {
			t.Errorf("%s stability = %v, want beta", event.SchemaName, got)
		}
		if got := schema["x-stability"]; got != nil {
			t.Errorf("%s must use only the canonical stability marker, got x-stability=%v", event.SchemaName, got)
		}
		if got := schema["additionalProperties"]; got != true {
			t.Errorf("%s additionalProperties = %#v, want true (consumer-direction payloads stay additive)", event.SchemaName, got)
		}
		want := "#/components/schemas/" + event.SchemaName
		if got := mapping[event.Type]; got != want {
			t.Errorf("beta mapping[%s] = %v, want %q", event.Type, got, want)
		}
		if _, listed := stableMapping[event.Type]; listed {
			t.Errorf("beta event %s must NOT appear in the stable x-e2a-event-data-schemas mapping", event.Type)
		}
	}

	// A stable sibling must stay unmarked, or "beta" would carry no signal.
	if stable := doc.Components.Schemas["DomainSuppressionAddedData"]; stable["x-stability-level"] != nil || stable["x-stability"] != nil {
		t.Fatalf("DomainSuppressionAddedData must remain stable and unmarked, got %#v", stable)
	}

	for schemaName, properties := range map[string][]string{
		"AgentSuppressionAddedData": {"agent_email", "address", "source"},
		"ContactDueData":            {"agent_email", "address", "stage", "next_action_at", "replied", "outbound_count", "contact"},
		"ContactDueContact":         {"address", "display_name", "metadata"},
	} {
		schema, ok := doc.Components.Schemas[schemaName]
		if !ok {
			t.Errorf("%s component is missing", schemaName)
			continue
		}
		props, _ := schema["properties"].(map[string]any)
		for _, name := range properties {
			if _, ok := props[name]; !ok {
				t.Errorf("%s.%s is missing", schemaName, name)
			}
		}
	}

	// ContactDueContact is introduced only by a beta payload, so it inherits
	// the marker; a schema shared with a stable payload must not.
	if got := doc.Components.Schemas["ContactDueContact"]["x-stability-level"]; got != "beta" {
		t.Errorf("ContactDueContact stability = %v, want beta", got)
	}
	if got := doc.Components.Schemas["AttachmentMetaView"]["x-stability-level"]; got != nil {
		t.Errorf("AttachmentMetaView is reachable from a stable payload and must stay unmarked, got %v", got)
	}
}

// TestBetaEventFixturesValidateAgainstEnvelopeAndMappedData is the beta twin of
// the stable fixture gate: the committed bytes must satisfy both the generic
// envelope and the schema the beta mapping points at, so the published shape
// and the emitted shape cannot drift apart.
func TestBetaEventFixturesValidateAgainstEnvelopeAndMappedData(t *testing.T) {
	server := New(Deps{})
	registry := server.API.OpenAPI().Components.Schemas
	envelope := registry.Map()["EventEnvelope"]
	if envelope == nil {
		t.Fatal("EventEnvelope component is missing")
	}
	for _, event := range eventpayload.BetaEvents {
		for _, fixture := range []string{event.Fixture, event.MinimalFixture} {
			if fixture == "" {
				continue
			}
			fixture := fixture
			t.Run(fixture, func(t *testing.T) {
				raw, err := os.ReadFile(filepath.Join("..", "eventpayload", "testdata", fixture))
				if err != nil {
					t.Fatalf("read fixture: %v", err)
				}
				var decoded map[string]any
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatalf("decode fixture: %v", err)
				}
				validateSchema(t, registry, envelope, "event", decoded)
				if decoded["type"] != event.Type {
					t.Fatalf("fixture type = %#v, want %q", decoded["type"], event.Type)
				}
				payload := registry.Map()[event.SchemaName]
				if payload == nil {
					t.Fatalf("mapped payload component %s is missing", event.SchemaName)
				}
				validateSchema(t, registry, payload, "data", decoded["data"])
			})
		}
	}
}

func TestStandaloneSchemaRefsResolveInRenderedDocument(t *testing.T) {
	raw, err := json.Marshal(New(Deps{}).API.OpenAPI())
	if err != nil {
		t.Fatalf("render OpenAPI: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}

	anchors, ok := doc["x-e2a-standalone-schema-refs"].(map[string]any)
	if !ok {
		t.Fatalf("x-e2a-standalone-schema-refs = %#v, want object", doc["x-e2a-standalone-schema-refs"])
	}
	components, ok := doc["components"].(map[string]any)
	if !ok {
		t.Fatalf("components = %#v, want object", doc["components"])
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatalf("components.schemas = %#v, want object", components["schemas"])
	}
	want := map[string]string{
		"EventEnvelope": "#/components/schemas/EventEnvelope",
	}
	for _, event := range eventpayload.StableEvents {
		want[event.SchemaName] = "#/components/schemas/" + event.SchemaName
	}
	for _, event := range eventpayload.BetaEvents {
		want[event.SchemaName] = "#/components/schemas/" + event.SchemaName
	}
	for _, entry := range errorCodeCatalog {
		if entry.DetailsSchema != "" {
			want[entry.DetailsSchema] = "#/components/schemas/" + entry.DetailsSchema
		}
	}
	if len(anchors) != len(want) {
		t.Errorf("x-e2a-standalone-schema-refs has %d entries, want %d", len(anchors), len(want))
	}
	for name, ref := range want {
		anchor, ok := anchors[name].(map[string]any)
		if !ok {
			t.Errorf("x-e2a-standalone-schema-refs[%s] = %#v, want $ref object", name, anchors[name])
			continue
		}
		if got := anchor["$ref"]; got != ref {
			t.Errorf("x-e2a-standalone-schema-refs[%s].$ref = %#v, want %q", name, got, ref)
		}
		if schemas[name] == nil {
			t.Errorf("anchored component schema %s is missing from the rendered document", name)
		}
	}
	if schemas["FieldError"] == nil {
		t.Error("transitively referenced component schema FieldError is missing from the rendered document")
	}
}

func TestStableEventFixturesValidateAgainstEnvelopeAndMappedData(t *testing.T) {
	server := New(Deps{})
	registry := server.API.OpenAPI().Components.Schemas
	envelope := registry.Map()["EventEnvelope"]
	if envelope == nil {
		t.Fatal("EventEnvelope component is missing")
	}

	for _, event := range eventpayload.StableEvents {
		fixtures := []string{event.Fixture}
		if event.MinimalFixture != "" {
			fixtures = append(fixtures, event.MinimalFixture)
		}
		for _, fixture := range fixtures {
			fixture := fixture
			t.Run(fixture, func(t *testing.T) {
				raw, err := os.ReadFile(filepath.Join("..", "eventpayload", "testdata", fixture))
				if err != nil {
					t.Fatalf("read fixture: %v", err)
				}
				var decoded map[string]any
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatalf("decode fixture: %v", err)
				}
				validateSchema(t, registry, envelope, "event", decoded)
				if decoded["type"] != event.Type {
					t.Fatalf("fixture type = %#v, want %q", decoded["type"], event.Type)
				}
				payload := registry.Map()[event.SchemaName]
				if payload == nil {
					t.Fatalf("mapped payload component %s is missing", event.SchemaName)
				}
				validateSchema(t, registry, payload, "data", decoded["data"])
			})
		}
	}
}

func TestEventEnvelopeAcceptsUnknownFutureEventAndVersion(t *testing.T) {
	server := New(Deps{})
	registry := server.API.OpenAPI().Components.Schemas
	decoded := map[string]any{
		"type":            "email.future_event",
		"id":              "evt_future",
		"schema_version":  "2",
		"created_at":      "2030-01-02T03:04:05Z",
		"data":            map[string]any{"future_field": map[string]any{"nested": true}},
		"future_envelope": "preserved",
	}
	validateSchema(t, registry, registry.Map()["EventEnvelope"], "event", decoded)
}

func validateSchema(t *testing.T, registry huma.Registry, schema *huma.Schema, path string, value any) {
	t.Helper()
	result := &huma.ValidateResult{}
	huma.Validate(registry, schema, huma.NewPathBuffer([]byte(path), len(path)), huma.ModeReadFromServer, value, result)
	if len(result.Errors) > 0 {
		t.Fatalf("schema validation failed: %v", result.Errors)
	}
}

func stringsFromAny(t *testing.T, value any) []string {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("value = %#v, want array", value)
	}
	out := make([]string, len(raw))
	for i, value := range raw {
		var ok bool
		out[i], ok = value.(string)
		if !ok {
			t.Fatalf("array value %d = %#v, want string", i, value)
		}
	}
	return out
}
