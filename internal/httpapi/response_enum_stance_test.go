package httpapi

import (
	"encoding/json"
	"fmt"
	"testing"
)

// closedResponseEnumAllowlist is the complete set of response-side fields
// that are DELIBERATELY closed enums, keyed "Component.property.path".
//
// The governing rule (docs/api.md "Versioning & stability"): response-side
// vocabularies that can EVOLVE are OPEN sets — plain strings whose known
// values are documented in the field description — because a closed enum on a
// response is a promise the vocabulary can never grow, and breaking that
// promise breaks spec-generated clients. Only three kinds of response fields
// may keep a closed enum:
//
//   - normalized exhaustive classifications: the server actively maps every
//     input into the fixed set, with a guaranteed catch-all (bounce_type via
//     normalizeBounceType in internal/delivery/ses.go);
//   - invariants of the model: values that cannot grow by construction —
//     the binary message direction, and the single-value `code` discriminator
//     constants on the typed error envelopes.
//   - explicitly versioned canonical protocol vocabularies whose closed set is
//     the interoperability contract. Message lifecycle values are intentionally
//     in this category: extensions require a versioned contract change rather
//     than an unannounced additive response value.
//
// Adding an entry here is a contract decision, not a formality: be able to
// state which of the three categories the field falls into, and record it in a
// comment on the struct tag.
var closedResponseEnumAllowlist = map[string]string{
	"BatchResult.status":                     "binary invariant of the discriminated union (every slot is exactly accepted or suppressed)",
	"EmailBouncedData.bounce_type":           "normalized exhaustive classification (undetermined is the guaranteed catch-all)",
	"MessageSummaryView.direction":           "binary invariant of the model",
	"MessageView.direction":                  "binary invariant of the model",
	"ReviewView.direction":                   "binary invariant of the model",
	"SPFResult.status":                       "normalized exhaustive RFC 7208 result classification",
	"DKIMResult.status":                      "normalized exhaustive RFC 8601 DKIM result classification",
	"DMARCResult.status":                     "normalized exhaustive RFC 7489/9989 DMARC result classification",
	"DMARCResult.policy":                     "normalized exhaustive DMARC policy classification",
	"DMARCResult.aligned_by[]":               "model invariant: DMARC aligns through SPF and/or DKIM only",
	"MessageLifecycleTransition.direction":   "closed canonical lifecycle protocol vocabulary; extensions require a versioned contract change",
	"MessageLifecycleTransition.stage":       "closed canonical lifecycle protocol vocabulary; extensions require a versioned contract change",
	"MessageLifecycleTransition.outcome":     "closed canonical lifecycle protocol vocabulary; extensions require a versioned contract change",
	"MessageLifecycleTransition.reason_code": "closed canonical lifecycle protocol vocabulary; extensions require a versioned contract change",
	"LimitExceededErrorBody.code":            "single-value discriminator constant of the typed 402 envelope",
	"RateLimitedErrorBody.code":              "single-value discriminator constant of the typed 429 envelope",
}

// TestResponseEnumsAreOpenSets is the stance gate for the open/closed enum
// rule on the /v1 response surface. It renders the live spec, finds every
// component schema carrying the response stance (`additionalProperties:
// true` — the marker stability.go stamps on response-reachable schemas and
// registerEventPayloadSchemas stamps on the event payload components; request
// schemas stay strict), and fails on any enum inside one that is not on the
// explicit allowlist above. It also asserts no operation declares an INLINE
// response enum, so the component walk cannot be bypassed.
//
// If this test failed because you added an enum tag to a response view:
// either the vocabulary can evolve — then drop the enum and use the house
// description ("Open set: new values may be added over time, so treat these
// as strings and tolerate unknown values. Known values: …") — or it is a
// normalized exhaustive classification / model invariant, then allowlist it
// here WITH the reason and add a rule-citing comment on the struct tag.
func TestResponseEnumsAreOpenSets(t *testing.T) {
	raw, err := json.Marshal(New(Deps{}).API.OpenAPI())
	if err != nil {
		t.Fatalf("render spec: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse rendered spec: %v", err)
	}

	found := map[string]bool{}
	responseSchemas := 0
	for name, rawSchema := range doc.Components.Schemas {
		var schema map[string]any
		if err := json.Unmarshal(rawSchema, &schema); err != nil {
			t.Fatalf("parse component %s: %v", name, err)
		}
		// The response stance: stability.go opens every response-reachable
		// schema with additionalProperties: true; request schemas stay strict.
		if open, ok := schema["additionalProperties"].(bool); !ok || !open {
			continue
		}
		responseSchemas++
		walkSchemaEnums(schema, name, func(path string, enum []string) {
			found[path] = true
			if _, ok := closedResponseEnumAllowlist[path]; !ok {
				t.Errorf("response-side field %s carries a closed enum %v.\n"+
					"Response vocabularies that can evolve must be OPEN sets (docs/api.md \"Versioning & stability\"):\n"+
					"drop the enum tag and document the known values in the description, or — if this is a\n"+
					"normalized exhaustive classification or a model invariant — allowlist it in\n"+
					"closedResponseEnumAllowlist with the reason and a rule-citing comment on the struct tag.",
					path, enum)
			}
		})
	}
	if responseSchemas == 0 {
		t.Fatal("found no response-stance component schemas — the stance marker or the walk is wrong")
	}
	// Prune the allowlist when a field stops being a closed enum, so it stays
	// the exact inventory of deliberate exceptions.
	for path := range closedResponseEnumAllowlist {
		if !found[path] {
			t.Errorf("closedResponseEnumAllowlist entry %q matched no response-side enum in the spec — remove the stale entry", path)
		}
	}

	// No operation may smuggle a closed response enum past the component walk
	// as an inline response schema.
	walkEnumsUnderResponses(doc.Paths, "paths", func(path string, enum []string) {
		t.Errorf("inline response enum %v at %s — hoist the schema to a named component and apply the open-set rule", enum, path)
	})
}

// walkSchemaEnums walks a rendered schema node (properties, array items,
// map values, allOf/anyOf/oneOf) and calls fn for every enum of strings.
// $refs stop the walk: the referenced component is checked on its own.
func walkSchemaEnums(node map[string]any, path string, fn func(string, []string)) {
	if _, isRef := node["$ref"]; isRef {
		return
	}
	if enum := stringEnum(node["enum"]); enum != nil {
		fn(path, enum)
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for name, sub := range props {
			if m, ok := sub.(map[string]any); ok {
				walkSchemaEnums(m, path+"."+name, fn)
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		walkSchemaEnums(items, path+"[]", fn)
	}
	if ap, ok := node["additionalProperties"].(map[string]any); ok {
		walkSchemaEnums(ap, path+"{}", fn)
	}
	for _, comb := range []string{"allOf", "anyOf", "oneOf"} {
		if subs, ok := node[comb].([]any); ok {
			for i, sub := range subs {
				if m, ok := sub.(map[string]any); ok {
					walkSchemaEnums(m, fmt.Sprintf("%s.%s[%d]", path, comb, i), fn)
				}
			}
		}
	}
}

// walkEnumsUnderResponses reports every enum that appears anywhere below a
// `responses` key — an inline (non-component) response schema.
func walkEnumsUnderResponses(node any, path string, fn func(string, []string)) {
	switch n := node.(type) {
	case map[string]any:
		for key, sub := range n {
			subPath := path + "/" + key
			if key == "responses" {
				walkAllEnums(sub, subPath, fn)
				continue
			}
			walkEnumsUnderResponses(sub, subPath, fn)
		}
	case []any:
		for i, sub := range n {
			walkEnumsUnderResponses(sub, fmt.Sprintf("%s/%d", path, i), fn)
		}
	}
}

// walkAllEnums reports every string enum anywhere under node.
func walkAllEnums(node any, path string, fn func(string, []string)) {
	switch n := node.(type) {
	case map[string]any:
		if enum := stringEnum(n["enum"]); enum != nil {
			fn(path, enum)
		}
		for key, sub := range n {
			walkAllEnums(sub, path+"/"+key, fn)
		}
	case []any:
		for i, sub := range n {
			walkAllEnums(sub, fmt.Sprintf("%s/%d", path, i), fn)
		}
	}
}

// stringEnum returns the enum values when raw is a non-empty all-string
// enum array, else nil.
func stringEnum(raw any) []string {
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	enum := make([]string, 0, len(arr))
	for _, v := range arr {
		s, ok := v.(string)
		if !ok {
			return nil
		}
		enum = append(enum, s)
	}
	return enum
}
