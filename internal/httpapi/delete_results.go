package httpapi

// Uniform DELETE responses (GA review Tier-1 #54, Option B / Stripe-style).
//
// Every /v1 DELETE returns 200 OK with a small per-resource deletion object
// instead of 204 No Content. Base shape:
//
//	{"deleted": true, "<identity key>": "..."}
//
// where the identity key matches the resource's identity field (webhook/
// template/api-key → id; agent → email; domain → domain; suppression →
// address). A 204 is an evolutionary dead end — it can never grow a body —
// while this shape is additive forever: cascading deletes can carry receipt
// counts (see DeleteAgentResult.messages_deleted and the account's
// DeleteUserDataResult) and future async deletes could carry state, all as
// optional additions. It also gives agents/SDKs an explicit, parseable
// confirmation instead of an empty response.
//
// `deleted` is a required boolean that is always true: a delete that fails is
// an error envelope, never `deleted:false`, so the field is a constant receipt
// marker. (It is deliberately NOT pinned with a schema-level enum:[true] —
// OpenAPI Generator's typescript generator renders a boolean enum as a broken
// STRING enum (`True = 'true'`), so the always-true contract lives in the
// field docs instead.)

// DeleteAgentResult confirms an agent delete. For a soft delete the agent is
// inactive in trash and messages_deleted is zero. For a permanent delete it
// is the number of message rows removed with the agent (webhook-delivery rows
// cascade from those and are not counted separately).
type DeleteAgentResult struct {
	Deleted         bool   `json:"deleted" doc:"Always true — the agent is no longer active. A failed delete is an error envelope, never deleted:false."`
	Email           string `json:"email" doc:"Email address of the deleted agent."`
	MessagesDeleted int64  `json:"messages_deleted" doc:"Number of messages permanently removed by the cascade; zero when the agent is moved to trash."`
} // @name DeleteAgentResult

// Sending-identity teardown outcomes surfaced by deleteDomain. "confirmed"
// means e2a-managed teardown is complete: provider absence was confirmed, or
// no provider is configured and the durable ledger proves e2a never managed
// an identity for the domain. "manual_review" means a provider identity exists
// but its ownership tag is absent, so e2a cannot safely mutate it or claim
// absence.
const (
	SendingTeardownConfirmed    = "confirmed"
	SendingTeardownPending      = "pending"
	SendingTeardownManualReview = "manual_review"
)

// DeleteDomainResult confirms a domain delete.
type DeleteDomainResult struct {
	Deleted bool   `json:"deleted" doc:"Always true — the domain no longer exists. A failed delete is an error envelope, never deleted:false."`
	Domain  string `json:"domain" doc:"The deleted domain."`
	// omitempty keeps the field OPTIONAL in the schema: generated clients must
	// accept responses from older servers that predate it.
	SendingTeardown string `json:"sending_teardown,omitempty" doc:"Durable state of e2a-managed sending-identity teardown (open set — treat missing or unknown values as not confirmed). 'confirmed' means provider absence was proved, or no provider is configured and e2a's ledger proves it never managed an identity for this domain. 'pending' means durable asynchronous teardown is retrying, including when the provider is temporarily disabled but the ledger records a managed identity. 'manual_review' means a provider identity exists but e2a cannot establish ownership and will not mutate it. A retry with the original Idempotency-Key replays this logical deletion's original receipt and cannot delete a later registration; while the domain remains absent, an unkeyed repeat reads the current owner-scoped teardown receipt. Keep the domain's DNS records published unless this is 'confirmed', or the still-live identity may emit provider verification-failure notices."`
} // @name DeleteDomainResult

// DeleteSuppressionResult confirms a suppression-list removal.
type DeleteSuppressionResult struct {
	Deleted bool   `json:"deleted" doc:"Always true — the address is no longer suppressed. A failed delete is an error envelope, never deleted:false."`
	Address string `json:"address" doc:"The un-suppressed recipient address."`
} // @name DeleteSuppressionResult

// DeleteApiKeyResult confirms an API-key revocation.
type DeleteApiKeyResult struct {
	Deleted bool   `json:"deleted" doc:"Always true — the key is revoked. A failed delete is an error envelope, never deleted:false."`
	ID      string `json:"id" doc:"ID of the revoked API key."`
} // @name DeleteApiKeyResult

// DeleteTemplateResult confirms a template delete.
type DeleteTemplateResult struct {
	Deleted bool   `json:"deleted" doc:"Always true — the template no longer exists. A failed delete is an error envelope, never deleted:false."`
	ID      string `json:"id" doc:"ID of the deleted template."`
} // @name DeleteTemplateResult

// DeleteWebhookResult confirms a webhook delete.
type DeleteWebhookResult struct {
	Deleted bool   `json:"deleted" doc:"Always true — the webhook no longer exists. A failed delete is an error envelope, never deleted:false."`
	ID      string `json:"id" doc:"ID of the deleted webhook."`
} // @name DeleteWebhookResult

// DeleteMessageResult confirms a message delete.
type DeleteMessageResult struct {
	Deleted bool   `json:"deleted" doc:"Always true — the message is deleted (moved to trash or purged). A failed delete is an error envelope, never deleted:false."`
	ID      string `json:"id" doc:"ID of the deleted message."`
} // @name DeleteMessageResult
