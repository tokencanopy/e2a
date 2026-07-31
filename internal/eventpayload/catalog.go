package eventpayload

// EventPayloadContract describes one PUBLISHED event `data` payload: the wire
// event type, the OpenAPI component schema name it is registered under, the Go
// payload type, and the golden fixtures that lock its bytes. It is the source
// consumed by OpenAPI registration and fixture coverage.
//
// Two catalogs use it. StableEvents is the GA-frozen set; BetaEvents is the
// published-but-unfrozen set. Together they need not cover the publisher's
// whole vocabulary — an event type may still be entirely untyped.
type EventPayloadContract struct {
	Type           string
	SchemaName     string
	Payload        any
	Fixture        string
	MinimalFixture string
}

// StableEvents is the canonical event-type → payload-schema catalog for the
// GA-frozen payloads. Keep the slice ordered for deterministic OpenAPI output
// and documentation tests.
var StableEvents = []EventPayloadContract{
	{"email.received", "EmailReceivedData", EmailReceivedData{}, "email.received.json", "email.received.min.json"},
	{"email.sent", "EmailSentData", EmailSentData{}, "email.sent.json", "email.sent.min.json"},
	{"email.failed", "EmailFailedData", EmailFailedData{}, "email.failed.json", "email.failed.min.json"},
	{"email.delivered", "EmailDeliveredData", EmailDeliveredData{}, "email.delivered.json", "email.delivered.min.json"},
	{"email.bounced", "EmailBouncedData", EmailBouncedData{}, "email.bounced.json", "email.bounced.min.json"},
	{"email.complained", "EmailComplainedData", EmailComplainedData{}, "email.complained.json", "email.complained.min.json"},
	{"domain.sending_verified", "DomainSendingVerifiedData", DomainSendingVerifiedData{}, "domain.sending_verified.json", ""},
	{"domain.sending_failed", "DomainSendingFailedData", DomainSendingFailedData{}, "domain.sending_failed.json", "domain.sending_failed.min.json"},
	{"domain.suppression_added", "DomainSuppressionAddedData", DomainSuppressionAddedData{}, "domain.suppression_added.json", "domain.suppression_added.min.json"},
}

// BetaEvents is the catalog of BETA payloads: shapes that are published as
// named component schemas (so generated SDKs and spec-validating clients can
// see them) and locked by golden fixtures (so a change is a conscious,
// reviewed regeneration) while `x-stability-level: beta` keeps them OUTSIDE
// the GA compatibility freeze. They are pointed at by
// `x-e2a-beta-event-data-schemas` on the event envelope, never by the stable
// `x-e2a-event-data-schemas` mapping.
//
// Publishing a beta payload is deliberately weaker than freezing it: the field
// set may still change before the event is declared stable. Publishing it is
// still strictly better than silence — a documented shape with a drift lock
// beats prose no consumer can verify against.
//
// Every entry's type MUST also appear in webhookpub.ExperimentalEventTypes;
// catalog_test.go enforces that, so promoting an event to stable is a single
// coordinated move (catalog entry + experimental list) rather than a drift.
var BetaEvents = []EventPayloadContract{
	{"agent.suppression_added", "AgentSuppressionAddedData", AgentSuppressionAddedData{}, "agent.suppression_added.json", ""},
	{"contact.due", "ContactDueData", ContactDueData{}, "contact.due.json", "contact.due.min.json"},
}
