package httpapi

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/eventpayload"
)

// EventEnvelope is the documentation schema for the push wire object
// shared by webhook deliveries and WebSocket frames. The runtime publisher
// uses webhookpub.Envelope; this map-typed data field is intentional so the
// published base schema stays open to unknown event types.
type EventEnvelope struct {
	Type          string         `json:"type" doc:"Open event type; clients must tolerate unknown values."`
	ID            string         `json:"id" doc:"Stable across retries and push channels; use it to deduplicate at-least-once delivery."`
	SchemaVersion string         `json:"schema_version" doc:"Open envelope-version string; the current server emits 1."`
	CreatedAt     time.Time      `json:"created_at" format:"date-time"`
	Data          map[string]any `json:"data" nullable:"false" doc:"Event-specific payload. Open at the envelope level; use x-e2a-event-data-schemas for stable event payloads."`
}

// registerEventPayloadSchemas publishes the canonical typed per-event `data`
// payloads (internal/eventpayload) as NAMED component schemas in the OpenAPI
// document — EmailReceivedData, EmailSentData, … — so SDK codegen and the
// docs renderer can reference the frozen per-event shapes.
//
// Mechanism: no operation's request/response embeds these types (the event
// envelope's `data` property deliberately stays OPEN — additionalProperties —
// so unknown/beta event types keep validating), so Huma would never emit them
// on its own. We pull each type through the API's shared schema registry
// (Components.Schemas.Schema, the same registry jsonResponse uses), which
// registers it under the hinted name and hoists it into components.schemas of
// the rendered spec. The event-type documentation (EventView.data and
// docs/events.md) references the schemas DESCRIPTIVELY — there is
// intentionally no oneOf/discriminator on the envelope.
//
// Beta events are generally open/unstable. eventpayload.BetaEvents is the
// documented subset: publishing their current shape improves generated-SDK
// discoverability while x-stability-level keeps them outside the GA freeze.
// They are published under x-e2a-beta-event-data-schemas, never under the
// stable x-e2a-event-data-schemas mapping, so a client cannot mistake a beta
// payload for a frozen one.
func (s *Server) registerEventPayloadSchemas() {
	registry := s.API.OpenAPI().Components.Schemas
	names := make([]string, 0, len(eventpayload.StableEvents))
	for _, event := range eventpayload.StableEvents {
		schema := registry.Schema(reflect.TypeOf(event.Payload), true, event.SchemaName)
		// The registry MUST intern the type under the exact hinted name — the
		// docs reference these names, and a silent rename (e.g. a name
		// collision appending a suffix) would break every published pointer.
		if schema == nil || schema.Ref != "#/components/schemas/"+event.SchemaName {
			panic(fmt.Sprintf("event payload schema %s registered under an unexpected ref: %+v", event.SchemaName, schema))
		}
		names = append(names, event.SchemaName)
	}

	// Forward-compatibility stance: these are CONSUMER-direction (server →
	// client) payload schemas, so they MUST be open for additive evolution —
	// `additionalProperties: true`, like every response schema. Huma registers
	// structs strict (`additionalProperties: false`) by default, and because
	// no operation's request/response reaches these components, a generic
	// "open every response-reachable schema" pass would never touch them —
	// they'd ship strict and a spec-generated client would break on the first
	// additive payload field. So this registration opens them itself, walking
	// each component's nested object nodes and following $refs (AttachmentMetaView
	// via EmailReceivedData.attachments) exactly like a response-schema stance
	// pass would.
	seen := map[string]bool{}
	for _, name := range names {
		openResponseComponent(registry, name, seen)
	}

	for _, event := range eventpayload.BetaEvents {
		betaSchema := registry.Schema(reflect.TypeOf(event.Payload), true, event.SchemaName)
		if betaSchema == nil || betaSchema.Ref != "#/components/schemas/"+event.SchemaName {
			panic(fmt.Sprintf("event payload schema %s registered under an unexpected ref: %+v", event.SchemaName, betaSchema))
		}
		// Snapshot first so the stability marker lands on the payload AND on
		// the components only it introduces (ContactDueContact), while a
		// component already reachable from a STABLE payload is left alone —
		// sharing a schema with a frozen event must not un-freeze it.
		introduced := newlyReachable(registry, event.SchemaName, seen)
		for _, name := range introduced {
			betaComponent := registry.Map()[name]
			if betaComponent.Extensions == nil {
				betaComponent.Extensions = map[string]any{}
			}
			betaComponent.Extensions["x-stability-level"] = "beta"
		}
	}

	envelope := registry.Schema(reflect.TypeOf(EventEnvelope{}), true, "EventEnvelope")
	if envelope == nil || envelope.Ref != "#/components/schemas/EventEnvelope" {
		panic(fmt.Sprintf("event envelope registered under an unexpected ref: %+v", envelope))
	}
	openResponseComponent(registry, "EventEnvelope", seen)
	envelopeSchema := registry.Map()["EventEnvelope"]
	data := envelopeSchema.Properties["data"]
	if data == nil {
		panic("event envelope data property missing from registered schema")
	}
	// map[string]any is rendered as an unconstrained schema-valued map by
	// Huma. Publish the simpler explicit open-object posture consumers rely on.
	data.Type = huma.TypeObject
	data.AdditionalProperties = true
	if data.Extensions == nil {
		data.Extensions = map[string]any{}
	}
	mapping := make(map[string]any, len(eventpayload.StableEvents))
	for _, event := range eventpayload.StableEvents {
		mapping[event.Type] = "#/components/schemas/" + event.SchemaName
	}
	data.Extensions["x-e2a-event-data-schemas"] = mapping
	betaMapping := make(map[string]any, len(eventpayload.BetaEvents))
	for _, event := range eventpayload.BetaEvents {
		betaMapping[event.Type] = "#/components/schemas/" + event.SchemaName
	}
	data.Extensions["x-e2a-beta-event-data-schemas"] = betaMapping
}

// newlyReachable opens a response component and returns, in sorted order, the
// component names this call was the FIRST to reach. Callers use it to stamp a
// marker on exactly the components a root introduces, without re-stamping ones
// an earlier root already owns.
func newlyReachable(registry huma.Registry, name string, seen map[string]bool) []string {
	before := make(map[string]bool, len(seen))
	for visited := range seen {
		before[visited] = true
	}
	openResponseComponent(registry, name, seen)
	introduced := make([]string, 0, len(seen)-len(before))
	for visited := range seen {
		if !before[visited] {
			introduced = append(introduced, visited)
		}
	}
	sort.Strings(introduced)
	return introduced
}

// registerStandaloneSchemaExports anchors public component schemas that are
// deliberately not referenced by an HTTP operation.
func (s *Server) registerStandaloneSchemaExports() {
	// Huma prunes component schemas that are not reachable through a real
	// OpenAPI $ref when serializing the document. The event schemas and some
	// structured error-detail schemas are intentionally operation-unreachable:
	// consumers select them using the string-valued mappings published on the
	// open EventEnvelope and ErrorBody. Publish explicit anchors so these
	// standalone schemas remain part of the public contract without turning
	// either open shape into a closed union.
	oapi := s.API.OpenAPI()
	if oapi.Extensions == nil {
		oapi.Extensions = map[string]any{}
	}
	exported := map[string]any{
		"EventEnvelope": map[string]any{"$ref": "#/components/schemas/EventEnvelope"},
	}
	for _, event := range eventpayload.StableEvents {
		exported[event.SchemaName] = map[string]any{
			"$ref": "#/components/schemas/" + event.SchemaName,
		}
	}
	for _, event := range eventpayload.BetaEvents {
		exported[event.SchemaName] = map[string]any{
			"$ref": "#/components/schemas/" + event.SchemaName,
		}
	}
	for _, entry := range errorCodeCatalog {
		if entry.DetailsSchema != "" {
			exported[entry.DetailsSchema] = map[string]any{
				"$ref": "#/components/schemas/" + entry.DetailsSchema,
			}
		}
	}
	oapi.Extensions["x-e2a-standalone-schema-refs"] = exported
}

// openResponseComponent flips additionalProperties from the strict default to
// true on a consumer-facing component and every object reachable from it.
// Event envelopes and error-detail schemas share this additive response rule.
func openResponseComponent(registry huma.Registry, name string, seen map[string]bool) {
	if seen[name] {
		return
	}
	seen[name] = true
	sc := registry.Map()[name]
	if sc == nil {
		panic(fmt.Sprintf("response schema %s missing from the registry", name))
	}
	openResponseNodes(sc, registry, seen)
}

func openResponseNodes(sc *huma.Schema, registry huma.Registry, seen map[string]bool) {
	if sc == nil {
		return
	}
	if sc.Ref != "" {
		if i := strings.LastIndex(sc.Ref, "/"); i >= 0 {
			openResponseComponent(registry, sc.Ref[i+1:], seen)
		}
		return
	}
	if v, ok := sc.AdditionalProperties.(bool); ok && !v {
		sc.AdditionalProperties = true
	}
	for _, p := range sc.Properties {
		openResponseNodes(p, registry, seen)
	}
	openResponseNodes(sc.Items, registry, seen)
	if ap, ok := sc.AdditionalProperties.(*huma.Schema); ok {
		openResponseNodes(ap, registry, seen)
	}
	for _, sub := range sc.OneOf {
		openResponseNodes(sub, registry, seen)
	}
	for _, sub := range sc.AnyOf {
		openResponseNodes(sub, registry, seen)
	}
	for _, sub := range sc.AllOf {
		openResponseNodes(sub, registry, seen)
	}
	openResponseNodes(sc.Not, registry, seen)
}
