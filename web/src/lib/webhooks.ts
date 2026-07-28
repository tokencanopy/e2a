// Shared webhook view types and scope semantics, consumed by the webhooks
// list and the webhook detail page. Kept out of either page component so the
// two cannot drift on what "scoped" means.

// Mirrors WebhookFiltersView (api/openapi.yaml). NOTE the wire/persistence
// name split: the API field is `agent_emails`, while the value persisted in
// the webhooks.filters jsonb column is keyed `agent_ids`
// (internal/identity/webhooks.go). Both hold email addresses — an agent's id
// IS its normalized email — so the values are interchangeable, but a direct
// SQL query against the column must use the other key.
export type WebhookFiltersView = {
  agent_emails?: string[] | null;
  conversation_ids?: string[] | null;
  labels?: string[] | null;
};

// WebhookView (GET /v1/webhooks → { items: [...] }). GET never returns
// `signing_secret` — it's only present on create / rotate responses.
export type WebhookView = {
  id: string;
  url: string;
  description?: string;
  events?: string[] | null;
  enabled: boolean;
  created_at: string;
  last_delivered_at?: string;
  auto_disabled_at?: string;
  signing_secret?: string;
  previous_secret_expires_at?: string;
  filters?: WebhookFiltersView | null;
};

// Mirrors WebhookDeliveryView (api/openapi.yaml). One row is ONE attempt
// series against ONE subscriber — distinct from the event that triggered it.
//
// There is deliberately no event_id here: the API does not expose one yet, so
// a delivery cannot currently be navigated back to its event or message.
export type WebhookDeliveryView = {
  id: string;
  type: string;
  status: string;
  attempts: number;
  next_retry_at: string;
  created_at: string;
  last_attempt_at?: string;
  last_error?: string;
  last_status_code?: number;
};

// `retrying` and `retry_due` are both IN FLIGHT — split so the UI can show a
// countdown for the first and nothing for the second. `unknown` exists
// because status is an open set (the redeliver endpoint documents a
// `scheduled` value absent from the deliveries enum); it fails closed, never
// collapsing an unrecognized status into success.
export type DeliveryStateKind =
  | "delivered"
  | "failed"
  | "retrying"
  | "retry_due"
  | "unknown";

export type DeliveryClassification = {
  kind: DeliveryStateKind;
  // The server's status string, always preserved so an unrecognized value can
  // be shown verbatim rather than guessed at.
  raw: string;
};

// `now` is a required parameter rather than a call to Date.now() so the
// retrying/retry_due boundary is testable without faking timers.
export function classifyDelivery(
  delivery: WebhookDeliveryView,
  now: Date,
): DeliveryClassification {
  const raw = delivery.status ?? "";
  switch (raw) {
    case "delivered":
      return { kind: "delivered", raw };
    case "failed":
      return { kind: "failed", raw };
    case "pending": {
      const next = delivery.next_retry_at
        ? new Date(delivery.next_retry_at)
        : null;
      // A missing or unparseable schedule means we know it is in flight but
      // not when it resumes. Claiming a future time we do not have would be
      // a fabrication; retry_due renders without a countdown.
      const hasFutureRetry =
        next !== null && !Number.isNaN(next.getTime()) && next > now;
      return { kind: hasFutureRetry ? "retrying" : "retry_due", raw };
    }
    default:
      return { kind: "unknown", raw };
  }
}

export type WebhookScope =
  | { scoped: false }
  | { scoped: true; parts: string[] };

// Summarize a subscription's scope. The unscoped case is a distinct variant
// rather than an empty string so callers are forced to handle it deliberately
// — an unscoped subscription receives events for every agent on the account,
// which is the opposite of the "nothing notable" a blank cell implies.
//
// Filter types are ANDed by the server and ORed within a type
// (internal/webhookpub/publisher.go), so every populated type belongs in the
// summary.
export function describeScope(
  filters?: WebhookFiltersView | null,
): WebhookScope {
  const parts: string[] = [];
  const agents = filters?.agent_emails ?? [];
  const conversations = filters?.conversation_ids ?? [];
  const labels = filters?.labels ?? [];

  if (agents.length > 0) parts.push(agents.join(", "));
  if (conversations.length > 0) {
    parts.push(
      `${conversations.length} conversation${conversations.length === 1 ? "" : "s"}`,
    );
  }
  if (labels.length > 0) parts.push(`labels: ${labels.join(", ")}`);

  return parts.length > 0 ? { scoped: true, parts } : { scoped: false };
}
