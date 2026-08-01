package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestContactMetadataBoundsArePublished pins that every request schema whose
// metadata routes through validateContactMetadata carries the two JSON Schema
// keywords that map exactly onto an enforced rule. Before this, a spec-driven
// client could not discover any bound at all.
func TestContactMetadataBoundsArePublished(t *testing.T) {
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
	for _, name := range contactMetadataSchemas {
		schema, ok := doc.Components.Schemas[name]
		if !ok {
			t.Errorf("%s is missing from the document", name)
			continue
		}
		metadata, ok := schema["properties"].(map[string]any)["metadata"].(map[string]any)
		if !ok {
			t.Errorf("%s.metadata is missing", name)
			continue
		}
		if got := metadata["maxProperties"]; got != float64(maxContactMetadataKeys) {
			t.Errorf("%s.metadata maxProperties = %#v, want %d", name, got, maxContactMetadataKeys)
		}
		names, ok := metadata["propertyNames"].(map[string]any)
		if !ok {
			t.Errorf("%s.metadata propertyNames = %#v", name, metadata["propertyNames"])
			continue
		}
		if got := names["maxLength"]; got != float64(maxContactMetadataKeyBytes) {
			t.Errorf("%s.metadata propertyNames.maxLength = %#v, want %d", name, got, maxContactMetadataKeyBytes)
		}
		// The keywords cover two rules; the prose must still carry the rest,
		// or a client would read the schema as the whole contract.
		description, _ := metadata["description"].(string)
		for _, fragment := range []string{"4096", "16384", "nested objects and arrays"} {
			if !strings.Contains(description, fragment) {
				t.Errorf("%s.metadata description does not mention %q — the prose must carry the bounds JSON Schema cannot express", name, fragment)
			}
		}
	}
}

// TestContactMetadataBoundsDoNotChangeRuntimeRejection is the other half of the
// contract: publishing the keywords must not move enforcement to Huma's request
// validator. An over-limit object must still be rejected by the handler, with
// the same 400 invalid_request and the same explanatory message a client
// already sees — this PR documents behavior, it does not change it.
func TestContactMetadataBoundsDoNotChangeRuntimeRejection(t *testing.T) {
	metadata := map[string]any{}
	for i := 0; i <= maxContactMetadataKeys; i++ {
		metadata[string(rune('a'+i%26))+strings.Repeat("x", i+1)] = "v"
	}
	if err := validateContactMetadata(metadata); err == nil {
		t.Fatal("an over-limit metadata object must be rejected by the handler")
	} else {
		envelope, ok := err.(*ErrorEnvelope)
		if !ok {
			t.Fatalf("rejection is %T, want *ErrorEnvelope", err)
		}
		if envelope.GetStatus() != http.StatusBadRequest || envelope.Code() != "invalid_request" {
			t.Fatalf("rejection = %d %s, want 400 invalid_request", envelope.GetStatus(), envelope.Code())
		}
	}

	// Huma must NOT also be validating it: the keywords live in Extensions
	// precisely so the runtime keeps its typed fields unset. If a future change
	// moves them onto huma.Schema.MaxProperties, an over-limit body becomes a
	// 422 at the edge and this fails.
	schema := New(Deps{}).API.OpenAPI().Components.Schemas.Map()["CreateContactRequest"].Properties["metadata"]
	if schema.MaxProperties != nil {
		t.Error("CreateContactRequest.metadata has a typed MaxProperties — Huma would now reject at the edge with 422 instead of the handler's 400")
	}
}
