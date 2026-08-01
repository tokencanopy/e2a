package httpapi

import (
	"fmt"
	"strings"
)

// contactMetadataSchemas are the request schemas whose `metadata` property is
// validated by validateContactMetadata. Every one of them routes through that
// single function, so they share one published bound set. A schema that stops
// calling it must be removed from this list, or the spec would promise a limit
// the server no longer applies.
var contactMetadataSchemas = []string{
	"CreateContactRequest",    // createContact
	"UpdateContactRequest",    // updateContact
	"UpsertEngagementRequest", // upsertEngagement
	"ContactImportRow",        // importContacts (per row)
}

// applyContactMetadataBounds publishes the machine-checkable half of the
// contact-metadata contract on the request schemas.
//
// The bounds are real and exactly enforced (validateContactMetadata): at most
// 50 keys, each key at most 128 bytes, each string value at most 4 KiB, the
// encoded object at most 16 KiB, scalars only. Before this the schema said
// `additionalProperties: {}` and the prose said "see the field bounds in the
// API docs" — so a generated SDK or a spec-validating client had no way to
// discover, let alone enforce, any of it.
//
// Only the two JSON Schema keywords that map EXACTLY onto an enforced rule are
// published:
//
//   - maxProperties: 50 — identical to the key-count check.
//   - propertyNames.maxLength: 128 — the server counts BYTES and JSON Schema
//     counts code points, so the published keyword is never STRICTER than the
//     server: a name of 129 code points is at least 129 bytes, which the server
//     rejects too. Publishing the code-point form cannot reject anything the
//     server accepts.
//
// The value-size, total-size, and scalars-only rules stay in prose. They are
// expressible in JSON Schema, but only as constraints on `additionalProperties`
// — which would turn the generated SDK's metadata type from a plain
// string-keyed map into a union, a far larger change than documenting a bound.
//
// WHY EXTENSIONS RATHER THAN STRUCT TAGS: huma's `maxProperties` struct tag
// also makes huma's own request validator enforce the bound, which would move
// an over-limit request from the handler's 400 invalid_request to a 422 at the
// edge. That is a behavior change, and this is a documentation change — the
// server's rejection must stay exactly as it is. Extensions are marshaled into
// the schema object verbatim, so the keywords appear in the published document
// while the runtime validator (which reads huma.Schema's typed fields) does not
// act on them. contact_metadata_schema_test.go pins both halves.
func (s *Server) applyContactMetadataBounds() {
	schemas := s.API.OpenAPI().Components.Schemas.Map()
	for _, name := range contactMetadataSchemas {
		schema, ok := schemas[name]
		if !ok {
			panic(fmt.Sprintf("httpapi: contact-metadata bound targets unknown schema %q", name))
		}
		metadata := schema.Properties["metadata"]
		if metadata == nil {
			panic(fmt.Sprintf("httpapi: %s has no metadata property to bound", name))
		}
		if metadata.Extensions == nil {
			metadata.Extensions = map[string]any{}
		}
		metadata.Extensions["maxProperties"] = maxContactMetadataKeys
		metadata.Extensions["propertyNames"] = map[string]any{
			"maxLength": maxContactMetadataKeyBytes,
		}
		// The struct tag carries the field's own lead-in (what writing it
		// means here); the shared bounds sentence is appended once so four
		// descriptions cannot drift from each other or from the keywords.
		metadata.Description = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(metadata.Description), contactMetadataDoc)) + " " + contactMetadataDoc
	}
}

// contactMetadataDoc is the shared prose for a caller-owned metadata object on
// a contact write. It states the bounds JSON Schema cannot express alongside
// the two it now can, so the description and the keywords never disagree.
const contactMetadataDoc = "Bounds, all enforced (400 invalid_request on violation): at most 50 keys; each key at most 128 bytes; each value must be a string, number, boolean, or null — nested objects and arrays are rejected, never flattened; each string value at most 4096 bytes; the whole object at most 16384 bytes once JSON-encoded. The byte-counted limits are UTF-8 octets, so a non-ASCII key or value reaches its limit sooner than its character count suggests."
