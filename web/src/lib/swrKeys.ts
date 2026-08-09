// Canonical SWR cache keys + invalidation helpers.
//
// SWR's mutate accepts a string, an array, or a predicate function.
// Centralizing key shapes here prevents drift between the consumer
// hooks (useSWR("agents", ...)) and the mutation sites that need to
// invalidate them (mutate("agents") after createAgent).
//
// Convention: single-resource lists use a bare string ("agents");
// parameterized resources use a tuple keyed by the params
// (["agent-messages", email, "all"]) so SWR caches separately by
// param combination.

import { mutate } from "swr";

// ── Keys ─────────────────────────────────────────────────

export const agentsKey = "agents";
export const domainsKey = "domains";
export const pendingMessagesKey = "pending-messages";
export const accountUnreadKey = "account-unread";

// Account plan + entitlements + month-to-date usage (GET /v1/account),
// read by the Billing page. Inbox count and domain count are two of the
// four metered dimensions, so agent and domain mutations invalidate this
// too — see invalidateAgents / invalidateDomains below.
export const limitsKey = "limits";

// Account-wide delivery metrics (GET /v1/metrics), read by the Metrics page.
// Keyed by the window so switching 7d/30d/90d caches separately instead of
// showing the previous range's numbers under the new range's label.
export const metricsKey = (days: number, groupByAgent: boolean) =>
  ["metrics", days, groupByAgent] as const;

// Trash views (soft-deleted resources, restorable ~30 days).
// deletedAgentsKey backs the account-wide /trash page
// (GET /v1/agents?deleted=true); agentTrashKey backs the per-inbox
// message trash (GET …/messages?deleted=true), keyed by inbox email.
export const deletedAgentsKey = "deleted-agents";
export const agentTrashKey = (email: string) =>
  ["agent-trash", email] as const;

// Per-agent protection config (GET /v1/agents/{address}/protection),
// keyed by the owning agent's email. The inbox-settings Review-queue
// editor reads + writes the `holds` section through this key.
export const protectionKey = (email: string) =>
  ["protection", email] as const;

// Key tuple includes every filter that affects the response so two
// surfaces fetching the same email under different filters don't
// poison each other's cache. Pre-fix the key omitted `status`, so a
// future surface filtering by status would collide with the existing
// all-status inbox view.
export const agentMessagesKey = (
  email: string,
  direction: string = "all",
  status: string = "all",
  token?: string,
) =>
  ["agent-messages", email, direction, status, token ?? null] as const;

// Per-inbox unread flag for the Inboxes list (getInboxHasUnread →
// GET /v1/agents/{address}/messages?read_status=unread&limit=1), keyed
// by the owning agent's email so each card caches independently.
export const agentUnreadKey = (email: string) =>
  ["agent-unread", email] as const;

// One cache entry per message, keyed by id ALONE — message ids are
// globally unique, so the id is the identity; the owning agent's email is
// a fetch parameter, not part of it.
//
// INVARIANT: the value under this key is always the raw MessageViewWire,
// never a projection. Several surfaces read the same message through
// different endpoints (the mail surfaces via GET /v1/agents/{address}/
// messages/{id}, the review queue via the superset GET /v1/reviews/{id}),
// and they used to cache their own projected shapes under a shared key —
// so whichever rendered first decided the shape and the other crashed
// dereferencing a field that wasn't there. Caching the wire keeps the
// shape uniform no matter which endpoint filled the entry; each surface
// projects at the point of use (see projectMessageDetail / projectPending
// in components/onboarding/api.ts).
//
// The endpoints return different SUPERSETS of the same message: only the
// review read carries `hold_reason`/`protection`. A wire written by the
// agent-scoped read therefore lacks them, and the review surfaces fall
// back to the summary row's `hold_reason` — degraded, never broken.
export const messageDetailKey = (id: string) =>
  ["message-detail", id] as const;

// Per-message canonical lifecycle observations (GET /v1/agents/{address}/
// messages/{id}/lifecycle). MessageLifecycleData pages through the endpoint
// with useSWRInfinite, so live keys carry trailing page-index/cursor slots
// beyond this prefix — invalidation matches the shared prefix only.
export const messageLifecycleKey = (email: string, id: string) =>
  ["message-lifecycle", email, id] as const;

// One batch (GET /v1/batches/{id}), for the batch monitor page. Keyed by the
// batch id alone — batch ids are globally unique and account-scoped, so the id
// is the identity. The status rollup is computed on read, so re-fetching this
// key (e.g. SWR refresh) is how the page shows live delivery progress.
export const batchKey = (id: string) => ["batch", id] as const;

// One webhook subscription (GET /v1/webhooks/{id}), for the detail page.
export const webhookKey = (id: string) => ["webhook", id] as const;

// The per-webhook delivery log (GET /v1/webhooks/{id}/deliveries). The status
// slot is part of the key because the server filters server-side: switching
// the filter is a different query, not a client-side narrowing of one cache
// entry.
export const webhookDeliveriesKey = (
  id: string,
  status: string,
  cursor: string,
) => ["webhook-deliveries", id, status, cursor] as const;

// ── Invalidation helpers ─────────────────────────────────

// After any agent mutation (create/update/delete/test) the agents
// list — which carries the per-agent enrichment fields the dashboard
// renders — needs to refetch.
export function invalidateAgents() {
  return Promise.all([mutate(agentsKey), invalidateLimits()]);
}

// Account limits carry the metered usage the Billing page renders. It's
// folded into the agent and domain helpers rather than left for each call
// site to remember, because a missed call here is invisible: the bars just
// keep showing a pre-mutation count. Cheap when nothing reads it — SWR
// only refetches keys that have a live subscriber.
export function invalidateLimits() {
  return mutate(limitsKey);
}

// After approve / reject the user-wide pending list needs to drop
// the resolved row.
export function invalidatePendingList() {
  return mutate(pendingMessagesKey);
}

// After approve / reject of a specific message, every surface showing
// that message's detail needs to refetch (status changes from
// pending_review to sent/rejected and the hold
// context goes away). One key per message means one entry to drop,
// whichever surface filled it.
export function invalidateMessageDetail(id: string) {
  return mutate(
    (key) =>
      Array.isArray(key) && key[0] === "message-detail" && key[1] === id,
  );
}

// After approve / reject the message's lifecycle ledger gains new
// transitions (review resolution, queueing, submission), so any open
// lifecycle panel for the message is stale. Matches the shared prefix of
// every paginated key for the message, across all loaded pages.
export function invalidateMessageLifecycle(email: string, id: string) {
  return mutate(
    (key) =>
      Array.isArray(key) &&
      key[0] === "message-lifecycle" &&
      key[1] === email &&
      key[2] === id,
  );
}

export function invalidateAgentMessages(email: string) {
  return mutate(
    (key) =>
      Array.isArray(key) &&
      key[0] === "agent-messages" &&
      key[1] === email,
  );
}

// Variant for invalidating EVERY cached inbox query at once. Used by
// the user-wide pending page, which doesn't know which specific
// agent's inbox is open elsewhere in the dashboard. A small
// over-invalidation: every per-agent inbox view refetches on next
// render. Cheap relative to the alternative ("Sidebar / inbox stale
// until the user navigates").
export function invalidateAllAgentMessages() {
  return mutate(
    (key) => Array.isArray(key) && key[0] === "agent-messages",
  );
}

// After a message is read (its detail fetch flips inbox_status → read on
// the backend), both its per-agent badge and the account-wide total are
// stale. Leave every other agent's independent unread probe untouched.
export function invalidateAgentUnread(email: string) {
  return mutate(
    (key) =>
      key === accountUnreadKey ||
      (Array.isArray(key) && key[0] === "agent-unread" && key[1] === email),
  );
}

export function invalidateDomains() {
  return Promise.all([mutate(domainsKey), invalidateLimits()]);
}

// After an inbox is trashed / restored / purged, the account-wide trash
// page is stale (the live agents list is handled by invalidateAgents).
export function invalidateDeletedAgents() {
  return mutate(deletedAgentsKey);
}

// After a message is trashed / restored / purged, the inbox's trash view
// is stale (the live views are handled by invalidateAgentMessages).
export function invalidateAgentTrash(email: string) {
  return mutate(agentTrashKey(email));
}

// After the Review-queue editor saves a new TTL / on-expiry, the per-
// agent protection fetch needs to refetch so the collapsed summary
// reflects the change.
export function invalidateProtection(email: string) {
  return mutate(protectionKey(email));
}
