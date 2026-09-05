export const MESSAGE_LIFECYCLE_DIRECTIONS = ["inbound", "outbound"] as const;
export const MESSAGE_LIFECYCLE_STAGES = [
  "accepted", "authentication", "review", "suppression", "queued",
  "submission", "delivery", "complaint",
] as const;
export const MESSAGE_LIFECYCLE_OUTCOMES = [
  "accepted", "passed", "failed", "indeterminate", "pending", "approved",
  "rejected", "blocked", "applied", "enqueued", "deferred", "delivered",
  "bounced", "reported",
] as const;
export const MESSAGE_LIFECYCLE_REASON_CODES = [
  "acceptance.inbound_smtp", "acceptance.outbound_api", "acceptance.local_loopback",
  "authentication.dmarc_pass", "authentication.dmarc_fail", "authentication.dmarc_none",
  "authentication.dmarc_temporary_error", "authentication.dmarc_permanent_error",
  "review.hold_created", "review.approved", "review.rejected",
  "review.expired_approved", "review.expired_rejected",
  "suppression.recipient_blocked", "suppression.hard_bounce_applied",
  "suppression.complaint_applied", "queue.inbound_processing", "queue.outbound_submission",
  "submission.upstream_accepted", "submission.local_loopback_accepted",
  "submission.temporary_failure", "submission.provider_rejected",
  "submission.local_retries_exhausted", "submission.cancelled",
  "submission.policy_budget_expired", "submission.sending_setup_expired",
  "delivery.recipient_server_accepted", "delivery.temporary_delay",
  "delivery.permanent_bounce", "delivery.transient_bounce",
  "delivery.undetermined_bounce", "complaint.recipient_reported",
] as const;

type Member<T extends readonly string[]> = T[number];

export interface MessageLifecycleTransitionWire {
  id: string;
  message_id: string;
  direction: Member<typeof MESSAGE_LIFECYCLE_DIRECTIONS>;
  recipient?: string | null;
  stage: Member<typeof MESSAGE_LIFECYCLE_STAGES>;
  outcome: Member<typeof MESSAGE_LIFECYCLE_OUTCOMES>;
  reason_code: Member<typeof MESSAGE_LIFECYCLE_REASON_CODES>;
  retryable: boolean;
  evidence: Record<string, unknown>;
  correlation_ids: Record<string, string>;
  occurred_at: string;
  reconstructed: boolean;
}

export interface MessageLifecyclePageWire {
  items: MessageLifecycleTransitionWire[];
  next_cursor: string | null;
}

function record(value: unknown, name: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`Invalid message lifecycle ${name}`);
  }
  return value as Record<string, unknown>;
}

function stringField(value: unknown, name: string): string {
  if (typeof value !== "string") throw new Error(`Invalid message lifecycle ${name}`);
  return value;
}

function enumField<const T extends readonly string[]>(
  value: unknown,
  values: T,
  name: string,
): T[number] {
  if (typeof value !== "string" || !(values as readonly string[]).includes(value)) {
    throw new Error(`Invalid message lifecycle ${name}`);
  }
  return value as T[number];
}

function parseTransition(value: unknown): MessageLifecycleTransitionWire {
  const row = record(value, "transition");
  const correlationIds = record(row.correlation_ids, "correlation_ids");
  for (const id of Object.values(correlationIds)) {
    if (typeof id !== "string") throw new Error("Invalid message lifecycle correlation_ids");
  }
  if (typeof row.retryable !== "boolean" || typeof row.reconstructed !== "boolean") {
    throw new Error("Invalid message lifecycle boolean field");
  }
  if (row.recipient !== undefined && row.recipient !== null && typeof row.recipient !== "string") {
    throw new Error("Invalid message lifecycle recipient");
  }
  return {
    id: stringField(row.id, "id"),
    message_id: stringField(row.message_id, "message_id"),
    direction: enumField(row.direction, MESSAGE_LIFECYCLE_DIRECTIONS, "direction"),
    ...(row.recipient !== undefined ? { recipient: row.recipient as string | null } : {}),
    stage: enumField(row.stage, MESSAGE_LIFECYCLE_STAGES, "stage"),
    outcome: enumField(row.outcome, MESSAGE_LIFECYCLE_OUTCOMES, "outcome"),
    reason_code: enumField(row.reason_code, MESSAGE_LIFECYCLE_REASON_CODES, "reason_code"),
    retryable: row.retryable,
    evidence: record(row.evidence, "evidence"),
    correlation_ids: correlationIds as Record<string, string>,
    occurred_at: stringField(row.occurred_at, "occurred_at"),
    reconstructed: row.reconstructed,
  };
}

export function parseMessageLifecyclePage(value: unknown): MessageLifecyclePageWire {
  const page = record(value, "page");
  if (!Array.isArray(page.items)) throw new Error("Invalid message lifecycle items");
  if (page.next_cursor !== null && typeof page.next_cursor !== "string") {
    throw new Error("Invalid message lifecycle next_cursor");
  }
  return {
    items: page.items.map(parseTransition),
    next_cursor: page.next_cursor,
  };
}

/**
 * Beta: fetch the ordered observations e2a recorded for one message. The
 * lifecycle contract may change before it is declared stable.
 */
export async function getMessageLifecycle(
  email: string,
  messageId: string,
  params: { cursor?: string; limit?: number } = {},
): Promise<MessageLifecyclePageWire> {
  const query = new URLSearchParams();
  if (params.cursor !== undefined) query.set("cursor", params.cursor);
  if (params.limit !== undefined) query.set("limit", String(params.limit));
  const suffix = query.size > 0 ? `?${query.toString()}` : "";
  const response = await fetch(
    `/v1/agents/${encodeURIComponent(email)}/messages/${encodeURIComponent(messageId)}/lifecycle${suffix}`,
    { credentials: "include" },
  );
  if (!response.ok) throw new Error(`Message lifecycle request failed (${response.status})`);
  return parseMessageLifecyclePage(await response.json());
}
