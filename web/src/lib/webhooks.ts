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
// next-attempt time for the first and nothing for the second.
//
// Observed against a local instance: immediately after a failed attempt,
// next_retry_at still equals created_at and therefore sits in the PAST, so a
// normal failing-but-not-exhausted delivery classifies as retry_due. Treat
// retry_due as the ordinary between-attempts state, not an anomaly — the
// split exists so we never render a countdown we'd have to negate, not to
// flag lateness. (Whether next_retry_at advances into the future once the
// retry worker reschedules was not observable in that run; see the design
// doc's open questions.)
//
// `unknown` exists because status is an open set (the redeliver endpoint
// documents a `scheduled` value absent from the deliveries enum); it fails
// closed, never collapsing an unrecognized status into success.
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

// How long a subscription can go without delivering before the list calls it
// quiet. 7 days rather than 24h: a low-volume inbox can legitimately see no
// traffic for a day, and a signal that cries wolf daily gets ignored.
export const HEALTH_STALE_AFTER_DAYS = 7;

// Health derived ONLY from fields already present on the webhook list
// response. A real success rate needs a server-side rollup that does not
// exist yet; probing the deliveries endpoint per row to synthesize one would
// add a request per webhook on every render and poll — the same N+1 shape as
// the per-agent unread probe. A weaker honest signal at zero cost beats an
// expensive one.
export type WebhookHealthKind =
  | "auto_disabled"
  | "disabled"
  | "never_delivered"
  | "stale"
  | "active";

export type WebhookHealth = {
  kind: WebhookHealthKind;
  lastDeliveredAt: Date | null;
};

export function classifyWebhookHealth(
  webhook: Pick<
    WebhookView,
    "enabled" | "auto_disabled_at" | "last_delivered_at"
  >,
  now: Date,
): WebhookHealth {
  const parsed = webhook.last_delivered_at
    ? new Date(webhook.last_delivered_at)
    : null;
  const lastDeliveredAt =
    parsed !== null && !Number.isNaN(parsed.getTime()) ? parsed : null;

  // Checked before `enabled`: e2a switching an endpoint off on the user's
  // behalf is a different, louder fact than the user switching it off, and
  // the auto-disabled row also has enabled=false.
  if (webhook.auto_disabled_at) {
    return { kind: "auto_disabled", lastDeliveredAt };
  }
  if (!webhook.enabled) {
    return { kind: "disabled", lastDeliveredAt };
  }
  if (lastDeliveredAt === null) {
    return { kind: "never_delivered", lastDeliveredAt };
  }

  const ageMs = now.getTime() - lastDeliveredAt.getTime();
  const windowMs = HEALTH_STALE_AFTER_DAYS * 24 * 60 * 60 * 1000;
  return {
    kind: ageMs > windowMs ? "stale" : "active",
    lastDeliveredAt,
  };
}

// Label and tone live beside the classifier, not in each page: the list and
// the detail view must not report the same state in different words. The
// stale label is derived from the window constant so the two cannot drift.
export const HEALTH_LABEL: Record<WebhookHealthKind, string> = {
  auto_disabled: "auto-disabled",
  disabled: "disabled",
  never_delivered: "never delivered",
  stale: `no deliveries in ${HEALTH_STALE_AFTER_DAYS}d`,
  active: "delivering",
};

// CSS custom-property names, resolved by the consuming component.
export function healthColor(kind: WebhookHealthKind): string {
  switch (kind) {
    case "active":
      return "var(--success)";
    // e2a turned this off on the user's behalf — the state most likely to be
    // silently dropping events, so it gets the loudest tone.
    case "auto_disabled":
      return "var(--danger-strong)";
    case "stale":
    case "never_delivered":
      return "var(--warn-strong)";
    default:
      return "var(--fg-subtle)";
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
