package httpapi

import (
	"fmt"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/webhookpub"
)

// Forward-compatibility stance of the /v1 contract (GA review #22/#23).
//
// The stance, stamped machine-readably onto the generated OpenAPI document:
//
//   - RESPONSE schemas carry `additionalProperties: true`: the server may add
//     response fields at any time (additive evolution), so a strict client
//     generated from the spec must tolerate unknown fields instead of turning
//     every additive release into a breaking change.
//   - REQUEST schemas keep `additionalProperties: false`: strict input
//     validation is intentional (e.g. an unknown field in a send body is a
//     422, which catches client-side typos like `body` vs `text`).
//   - Surfaces exempt from the v1 freeze carry the canonical oasdiff-native
//     `x-stability-level: beta` marker (operations declare it at registration;
//     their schemas inherit it here). Individual fields on stable schemas are
//     marked via markProperty, and individual PARAMETERS on stable operations
//     via markOperationParamSchema (the marker rides on the parameter's
//     inline schema — huma.Param has no extension slot).
//   - Event-type fields whose VALUE SET contains beta members carry
//     `x-experimental-values` listing exactly those members
//     (webhookpub.ExperimentalEventTypes).
//
// Because the two halves of the stance are incompatible on a single schema, a
// component schema must never be reachable from both a request body and a
// response body — applyEvolutionStance panics if one is, forcing the Go type
// to be split into a *Request input type and a *View output type (as was done
// for ProtectionConfig* and WebhookFilters*).

const (
	stabilityBeta         = "beta"
	extStabilityLevel     = "x-stability-level"
	extExperimentalValues = "x-experimental-values"
)

// The account export's versioned-interior exemption (GA decision).
//
// GET /v1/account/export is a STABLE operation and its top-level UserExport
// envelope (the top-level keys and schema_version) is frozen with the rest of
// the GA surface. Its INTERIOR record shapes, however, are snapshots of
// internal storage models (identity.AgentIdentity, identity.Message, …);
// freezing them would freeze the storage layer itself. They are therefore
// versioned by UserExport.schema_version instead of the v1 freeze: consumers
// MUST branch on schema_version, and every component schema reachable from
// UserExport — except the envelope itself and any schema the stable surface
// reaches on its own — carries `x-stability-level: beta` so automated
// compatibility tooling excludes interior evolution from the stable gate.
const (
	exportOperationID    = "exportAccount"
	exportEnvelopeSchema = "UserExport"
)

// beta is the operation extension marking a surface as exempt from the
// v1 freeze (may change without a major version). Returns a fresh map so no
// two operations share mutable state.
func beta() map[string]any {
	return map[string]any{
		extStabilityLevel: stabilityBeta,
	}
}

// applyEvolutionStance walks the generated OpenAPI document once, after every
// operation has been registered, and stamps the stance above onto it. It
// mutates only response-reachable schemas' additionalProperties and adds
// x-* extension fields — request validation behavior is untouched.
func (s *Server) applyEvolutionStance() {
	oapi := s.API.OpenAPI()
	registry := oapi.Components.Schemas
	schemas := registry.Map()

	// Reachability roots, per stance axis.
	requestRoots := map[string]bool{}  // referenced from a request body
	responseRoots := map[string]bool{} // referenced from a response body
	betaRoots := map[string]bool{}
	stableRoots := map[string]bool{}
	exportOpRoots := map[string]bool{} // the export operation's own roots

	// Inline (non-$ref) object schemas embedded directly in a response body
	// also need opening; collect them while walking.
	var inlineResponseSchemas []*huma.Schema

	forEachOperation(oapi, func(op *huma.Operation) {
		isBeta := op.Extensions[extStabilityLevel] == stabilityBeta
		opRoots := map[string]bool{}
		if op.RequestBody != nil {
			for _, mt := range op.RequestBody.Content {
				if mt == nil || mt.Schema == nil {
					continue
				}
				collectRefs(mt.Schema, requestRoots)
				collectRefs(mt.Schema, opRoots)
			}
		}
		for _, resp := range op.Responses {
			if resp == nil {
				continue
			}
			for _, mt := range resp.Content {
				if mt == nil || mt.Schema == nil {
					continue
				}
				collectRefs(mt.Schema, responseRoots)
				collectRefs(mt.Schema, opRoots)
				inlineResponseSchemas = append(inlineResponseSchemas, mt.Schema)
			}
		}
		dst := stableRoots
		switch {
		case isBeta:
			dst = betaRoots
		case op.OperationID == exportOperationID:
			// The export is stable, but routing its response tree into
			// stableRoots would veto the versioned-interior markers below.
			// Its non-envelope roots (error responses) rejoin stableRoots
			// after the export closure is known.
			dst = exportOpRoots
		}
		for name := range opRoots {
			dst[name] = true
		}
	})

	requestSet := refClosure(schemas, requestRoots)
	responseSet := refClosure(schemas, responseRoots)

	// Versioned-interior exemption (see the constant block above): the export
	// envelope's closure, computed before stableSet so only the envelope's own
	// subtree is withheld from the stable-surface veto.
	if _, ok := schemas[exportEnvelopeSchema]; !ok {
		panic(fmt.Sprintf("httpapi: export envelope schema %q missing from the registry — re-target the versioned-interior exemption", exportEnvelopeSchema))
	}
	exportSet := refClosure(schemas, map[string]bool{exportEnvelopeSchema: true})
	for name := range exportOpRoots {
		if !exportSet[name] {
			stableRoots[name] = true // e.g. error envelopes on the export op stay stable
		}
	}

	// The invariant that makes the request/response split total: no schema may
	// serve both masters. If this fires, split the Go type (input *Request vs
	// output *View) instead of weakening either half of the stance.
	for name := range responseSet {
		if requestSet[name] {
			panic(fmt.Sprintf("httpapi: schema %q is reachable from both a request body and a response body; "+
				"split the Go type into a *Request (strict) and a *View (open) so the forward-compat stance stays total", name))
		}
	}

	// Responses: open every object node so generated clients tolerate the
	// additive fields the stability policy reserves the right to add.
	for name := range responseSet {
		openObjectSchemas(schemas[name])
	}
	for _, sc := range inlineResponseSchemas {
		openObjectSchemas(sc)
	}

	// Schemas used exclusively by beta operations inherit the marker
	// automatically, so a new beta resource can't leave invisible holes in the
	// freeze. Schemas shared with stable operations (error envelopes, pages of
	// stable views, …) stay unmarked.
	betaSet := refClosure(schemas, betaRoots)
	stableSet := refClosure(schemas, stableRoots)
	for name := range betaSet {
		if stableSet[name] {
			continue
		}
		sc := schemas[name]
		if sc.Extensions == nil {
			sc.Extensions = map[string]any{}
		}
		sc.Extensions[extStabilityLevel] = stabilityBeta
	}

	// Versioned-interior exemption for the account export (see the constant
	// block above). Every schema reachable from the UserExport envelope gets
	// the beta marker, EXCEPT:
	//   - the envelope itself (top-level keys + schema_version stay frozen);
	//   - any schema the stable surface reaches on its own — via another
	//     stable operation (Authentication, through the message endpoints) or
	//     via a stable operation-unreachable documentation component
	//     (AttachmentMetaView, through the email.received event payload
	//     EmailReceivedData). Marking those would silently degrade stable
	//     endpoints/event payloads to beta.
	// The set is computed, not enumerated, so a new export entry type can't
	// leave an invisible hole in the freeze (same rationale as the beta-op
	// inheritance above).
	stableDocRoots := map[string]bool{}
	for name, sc := range schemas {
		if requestSet[name] || responseSet[name] || exportSet[name] {
			continue
		}
		if sc.Extensions[extStabilityLevel] == stabilityBeta {
			continue
		}
		stableDocRoots[name] = true
	}
	stableSurface := refClosure(schemas, stableDocRoots)
	for name := range stableSet {
		stableSurface[name] = true
	}
	for name := range exportSet {
		if name == exportEnvelopeSchema || stableSurface[name] {
			continue
		}
		sc := schemas[name]
		if sc.Extensions == nil {
			sc.Extensions = map[string]any{}
		}
		sc.Extensions[extStabilityLevel] = stabilityBeta
	}

	// Field-level markers on otherwise-stable schemas.
	//
	// Review detail reuses MessageView, which is also returned by stable agent
	// message operations. Mark both review-only fields and their component trees
	// explicitly so the stable parent schemas and operations stay stable.
	for _, schema := range []string{"MessageView", "ReviewView"} {
		markProperty(schemas, schema, "hold_reason", extStabilityLevel, stabilityBeta)
	}
	markProperty(schemas, "MessageView", "protection", extStabilityLevel, stabilityBeta)
	// The policy flag verdict remains visible on stable message reads because
	// flag outcomes are delivered rather than held. Keep only these properties
	// beta so their contract can evolve without degrading the parent schemas.
	for _, schema := range []string{"MessageView", "MessageSummaryView", "ReviewView"} {
		for _, property := range []string{"flagged", "flag_reason"} {
			markProperty(schemas, schema, property, extStabilityLevel, stabilityBeta)
		}
	}
	// Server-owned email thread identity is a beta read projection embedded in
	// the otherwise-stable shared message schemas. Only message list/detail
	// handlers populate it; other operations that reuse these schemas omit it.
	for _, schema := range []string{"MessageSummaryView", "MessageView"} {
		markProperty(schemas, schema, "thread_id", extStabilityLevel, stabilityBeta)
	}
	for _, schema := range []string{"HoldReasonView", "ProtectionFindingView", "ThreatCategoryView"} {
		markSchema(schemas, schema, extStabilityLevel, stabilityBeta)
	}
	// ErrorBody.code is a stable open discriminator; the outbound gate-policy
	// value and the sending-abuse pause value remain experimental — both are
	// produced by controls that ship disabled.
	markProperty(schemas, "ErrorBody", "code", extExperimentalValues, []string{"blocked_by_policy", "sending_paused"})
	//
	// The template hooks on send are beta (templates are beta) even though
	// sendMessage itself is stable.
	for _, prop := range []string{"template_alias", "template_id", "template_data"} {
		markProperty(schemas, "SendEmailRequest", prop, extStabilityLevel, stabilityBeta)
	}
	// Managed unsubscribe is a beta nested capability on stable outbound
	// operations. Mark both its reusable component and each stable request
	// property, without weakening the containing operations or schemas.
	unsubscribeSchema := markSchema(schemas, "UnsubscribeOptions", extStabilityLevel, stabilityBeta)
	unsubscribeSchema.Description = "Beta: per-message opt-in to e2a-managed unsubscribe handling. This schema may change before it is declared stable."
	for _, schema := range []string{"SendEmailRequest", "ReplyRequest", "ForwardRequest"} {
		markProperty(schemas, schema, "unsubscribe", extStabilityLevel, stabilityBeta)
	}
	// Scheduled sending is a beta capability nested inside otherwise-stable
	// message operations and views. Keep the operations and containing schemas
	// stable while marking the request/response properties and the scheduled
	// status discriminator value machine-readably.
	for _, schema := range []string{"SendEmailRequest", "ReplyRequest", "ForwardRequest"} {
		markProperty(schemas, schema, "send_at", extStabilityLevel, stabilityBeta)
	}
	// Quoted-history replies are a beta capability nested inside the
	// otherwise-stable reply operation (like send_at): the flag itself is
	// request-time-only, the composition rules may still evolve before it is
	// declared stable, and the containing operation/schema stay GA.
	markProperty(schemas, "ReplyRequest", "quote_history", extStabilityLevel, stabilityBeta)
	// "Message" is the account-export record. It gained scheduled_at with the
	// rest of the family but neither a description nor this marker, so the
	// export alone presented a beta field as though it were stable. (Its
	// containing schema is already beta-marked as an export interior record;
	// the property marker is what survives a future promotion of the record.)
	for _, schema := range []string{"MessageSummaryView", "MessageView", "SendResultView", "Message"} {
		markProperty(schemas, schema, "scheduled_at", extStabilityLevel, stabilityBeta)
	}
	markProperty(schemas, "SendResultView", "status", extExperimentalValues, []string{"scheduled"})
	// Message lifecycle is a beta capability embedded as an optional field in
	// otherwise-stable event payloads. Mark only the property: the existing
	// event types, payload schemas, envelope, and delivery semantics remain GA.
	for _, schema := range []string{"EmailReceivedData", "EmailSentData", "EmailFailedData", "EmailDeliveredData", "EmailBouncedData", "EmailComplainedData", "DomainSuppressionAddedData"} {
		markProperty(schemas, schema, "lifecycle_transitions", extStabilityLevel, stabilityBeta)
	}
	// The event-type vocabulary is stable EXCEPT the screening + review-hold
	// members (their payloads may still change). The field is stable; the beta
	// subset of its value set is machine-readable via x-experimental-values.
	for _, schema := range []string{"CreateWebhookRequest", "UpdateWebhookRequest", "WebhookView", "CreateWebhookResponse"} {
		markProperty(schemas, schema, "events", extExperimentalValues, webhookpub.ExperimentalEventTypes)
	}

	// Parameter-level markers on otherwise-stable operations.
	//
	// listMessages is GA, but its `filter` query parameter (the boolean
	// filter language) is beta: the field/operator vocabulary may still
	// evolve (e.g. has:attachment) before it is declared stable. huma.Param
	// has no extension slot, so the marker rides on the parameter's inline
	// schema — the only extension-capable node an OpenAPI parameter offers.
	markOperationParamSchema(oapi, "listMessages", "filter", extStabilityLevel, stabilityBeta)
}

// markSchema stamps an extension on a named component schema, panicking on a
// dangling name so a rename cannot silently drop a stability marker.
func markSchema(schemas map[string]*huma.Schema, schema, ext string, value any) *huma.Schema {
	sc, ok := schemas[schema]
	if !ok {
		panic(fmt.Sprintf("httpapi: stability marker targets unknown schema %q", schema))
	}
	if sc.Extensions == nil {
		sc.Extensions = map[string]any{}
	}
	sc.Extensions[ext] = value
	return sc
}

// forEachOperation visits every operation in the document.
func forEachOperation(oapi *huma.OpenAPI, visit func(op *huma.Operation)) {
	for _, item := range oapi.Paths {
		if item == nil {
			continue
		}
		for _, op := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Options, item.Head, item.Patch, item.Trace} {
			if op != nil {
				visit(op)
			}
		}
	}
}

// collectRefs walks an inline schema tree and records the component-schema
// names it references (stopping at each $ref — the closure follows them).
func collectRefs(sc *huma.Schema, out map[string]bool) {
	if sc == nil {
		return
	}
	if sc.Ref != "" {
		if i := strings.LastIndex(sc.Ref, "/"); i >= 0 {
			out[sc.Ref[i+1:]] = true
		}
		return
	}
	for _, p := range sc.Properties {
		collectRefs(p, out)
	}
	collectRefs(sc.Items, out)
	if ap, ok := sc.AdditionalProperties.(*huma.Schema); ok {
		collectRefs(ap, out)
	}
	for _, sub := range sc.OneOf {
		collectRefs(sub, out)
	}
	for _, sub := range sc.AnyOf {
		collectRefs(sub, out)
	}
	for _, sub := range sc.AllOf {
		collectRefs(sub, out)
	}
	collectRefs(sc.Not, out)
}

// refClosure expands root component names to everything transitively
// referenced through the registry.
func refClosure(schemas map[string]*huma.Schema, roots map[string]bool) map[string]bool {
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
		sc, ok := schemas[name]
		if !ok {
			panic(fmt.Sprintf("httpapi: schema %q referenced by an operation is missing from the registry", name))
		}
		next := map[string]bool{}
		collectRefs(sc, next)
		for n := range next {
			if !seen[n] {
				stack = append(stack, n)
			}
		}
	}
	return seen
}

// openObjectSchemas flips additionalProperties from the strict default (false)
// to true on every inline object node of a response schema tree. Map-typed
// nodes (whose additionalProperties is itself a schema) are left alone.
func openObjectSchemas(sc *huma.Schema) {
	if sc == nil || sc.Ref != "" {
		return
	}
	if v, ok := sc.AdditionalProperties.(bool); ok && !v {
		sc.AdditionalProperties = true
	}
	for _, p := range sc.Properties {
		openObjectSchemas(p)
	}
	openObjectSchemas(sc.Items)
	if ap, ok := sc.AdditionalProperties.(*huma.Schema); ok {
		openObjectSchemas(ap)
	}
	for _, sub := range sc.OneOf {
		openObjectSchemas(sub)
	}
	for _, sub := range sc.AnyOf {
		openObjectSchemas(sub)
	}
	for _, sub := range sc.AllOf {
		openObjectSchemas(sub)
	}
	openObjectSchemas(sc.Not)
}

// markProperty stamps an extension on a named property of a named component
// schema, panicking on a dangling name so a rename can't silently drop a
// stability marker.
func markProperty(schemas map[string]*huma.Schema, schema, property, ext string, value any) {
	sc, ok := schemas[schema]
	if !ok {
		panic(fmt.Sprintf("httpapi: stability marker targets unknown schema %q", schema))
	}
	p, ok := sc.Properties[property]
	if !ok {
		panic(fmt.Sprintf("httpapi: stability marker targets unknown property %s.%s", schema, property))
	}
	if p.Extensions == nil {
		p.Extensions = map[string]any{}
	}
	p.Extensions[ext] = value
}

// markOperationParamSchema stamps an extension on the inline schema of a
// named operation's named parameter, panicking on a dangling operation or
// parameter name so a rename can't silently drop a stability marker.
func markOperationParamSchema(oapi *huma.OpenAPI, operationID, param, ext string, value any) {
	found := false
	forEachOperation(oapi, func(op *huma.Operation) {
		if op.OperationID != operationID {
			return
		}
		for _, p := range op.Parameters {
			if p == nil || p.Name != param || p.In != "query" {
				continue
			}
			if p.Schema == nil {
				panic(fmt.Sprintf("httpapi: stability marker targets schemaless parameter %s.%s", operationID, param))
			}
			if p.Schema.Extensions == nil {
				p.Schema.Extensions = map[string]any{}
			}
			p.Schema.Extensions[ext] = value
			found = true
		}
	})
	if !found {
		panic(fmt.Sprintf("httpapi: stability marker targets unknown parameter %s.%s", operationID, param))
	}
}
