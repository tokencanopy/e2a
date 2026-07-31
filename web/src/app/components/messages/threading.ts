// Pure threading logic for the dashboard inbox.
//
// We group new MessageSummary rows by their server-owned thread_id.
// Legacy rows without thread_id retain the conversation_id/orphan fallback.
// To prevent values from colliding across namespaces, every kind is prefixed:
//   - topology thread → key `thr:<thread_id>`
//   - legacy conv_id  → key `conv:<conv_id>`
//   - legacy orphan   → key `orphan:<id>`
// Prefixes are added by this code, never extracted from operator input
// — even if the SDK caller sends `conversation_id="orphan:foo"`, the
// resulting thread key is `conv:orphan:foo`, which does not collide
// with the synthetic key `orphan:foo`. URL fragments encode the
// operator-controlled suffix and decode it before key comparison.
//
// All inputs are pure: no Date.now() side effects (caller passes `now`),
// no I/O. That keeps the unit tests deterministic.

import type { MessageSummary } from "../types";

export type ThreadState = "pending" | "active" | "handed-off" | "closed";

export function encodeThreadFragment(key: string): string {
  const separator = key.indexOf(":");
  if (separator < 0) return encodeURIComponent(key);
  return (
    key.slice(0, separator + 1) +
    encodeURIComponent(key.slice(separator + 1))
  );
}

export function decodeThreadFragment(fragment: string): string {
  try {
    return decodeURIComponent(fragment);
  } catch {
    // Old links may contain an unescaped literal "%". Keep them readable
    // rather than turning a malformed escape into an empty selection.
    return fragment;
  }
}

export type Counterparty = {
  email: string;
  name: string;
};

export type Thread = {
  /** Stable key — `thr:<thread_id>`, or a legacy `conv:` / `orphan:` fallback. */
  key: string;
  /** Real conversation_id when present; undefined for synthetic threads. */
  conversationId?: string;
  counterparty: Counterparty;
  subject: string;
  state: ThreadState;
  /** ISO timestamp of the most recent message in the thread. */
  lastMessageAt: string;
  /** ISO timestamp of the oldest message in the thread (used for "started"). */
  startedAt: string;
  msgCount: number;
  /** Direction of the most recent message — drives the row's preview icon. */
  lastDirection: "inbound" | "outbound";
  /** Short preview text (subject of the latest message — body parts aren't on the wire yet). */
  lastPreview: string;
  /** Messages ordered oldest → newest, ready to render as a chat log bottom-up. */
  messages: MessageSummary[];
};

const SEVEN_DAYS_MS = 7 * 24 * 60 * 60 * 1000;

function counterpartyEmail(m: MessageSummary): string {
  if (m.direction === "inbound") return m.from;
  return m.to?.[0] ?? m.recipient;
}

function nameFromEmail(email: string): string {
  const local = email.split("@")[0] || email;
  const parts = local.split(/[._-]+/).filter(Boolean);
  if (parts.length === 0) return email;
  return parts
    .map((p) => p.charAt(0).toUpperCase() + p.slice(1))
    .join(" ");
}

function deriveState(messages: MessageSummary[], nowMs: number): ThreadState {
  // pending wins over everything else — operator action needed.
  if (messages.some((m) => m.review_status === "pending_review")) {
    return "pending";
  }

  // closed if everything is older than 7d.
  const latest = messages.reduce(
    (acc, m) => Math.max(acc, new Date(m.created_at).getTime()),
    0,
  );
  if (nowMs - latest > SEVEN_DAYS_MS) return "closed";

  // handed-off if there are 2+ outbound rows with different recipients —
  // means the agent forwarded somewhere new during the conversation.
  const outboundRecipients = new Set<string>();
  for (const m of messages) {
    if (m.direction === "outbound") {
      const target = m.to?.[0] ?? m.recipient;
      if (target) outboundRecipients.add(target);
    }
  }
  if (outboundRecipients.size >= 2) return "handed-off";

  return "active";
}

/**
 * Groups a flat list of messages into thread summaries for the inbox.
 *
 * @param rows Messages from `listAgentMessages` (any order — we sort inside).
 * @param now Reference timestamp for "closed >7d" — caller injects so tests can pin it.
 */
export function groupIntoThreads(
  rows: MessageSummary[],
  now: Date = new Date(),
): Thread[] {
  const nowMs = now.getTime();
  const buckets = new Map<string, MessageSummary[]>();

  for (const m of rows) {
    const key =
      m.thread_id != null
        ? `thr:${m.thread_id}`
        : m.conversation_id
          ? `conv:${m.conversation_id}`
          : `orphan:${m.id}`;
    const bucket = buckets.get(key);
    if (bucket) bucket.push(m);
    else buckets.set(key, [m]);
  }

  const threads: Thread[] = [];
  for (const [key, bucket] of buckets) {
    // Order oldest → newest. Stable on created_at; ties broken by id
    // so the order is deterministic even at sub-second resolution.
    bucket.sort((a, b) => {
      const ad = new Date(a.created_at).getTime();
      const bd = new Date(b.created_at).getTime();
      if (ad !== bd) return ad - bd;
      return a.id.localeCompare(b.id);
    });

    const latest = bucket[bucket.length - 1];
    const oldest = bucket[0];

    // Subject: first non-empty in the thread, else "(no subject)".
    const subject =
      bucket.map((m) => m.subject).find((s) => s && s.trim() !== "") ||
      "(no subject)";

    // Counterparty: derived from any non-agent participant. We use the
    // latest message's "other side" — typically what the user thinks of
    // as "who this thread is with".
    const cpEmail = counterpartyEmail(latest);
    const counterparty: Counterparty = {
      email: cpEmail,
      name: nameFromEmail(cpEmail),
    };

    threads.push({
      key,
      conversationId: latest.conversation_id || undefined,
      counterparty,
      subject,
      state: deriveState(bucket, nowMs),
      lastMessageAt: latest.created_at,
      startedAt: oldest.created_at,
      msgCount: bucket.length,
      lastDirection: latest.direction,
      // Subject as preview for v1 — body parts aren't in the wire payload
      // yet. When a server-side preview field lands, switch to that.
      lastPreview: (latest.subject || "(no subject)").slice(0, 80),
      messages: bucket,
    });
  }

  // Sort threads: pending first (operator attention), then by lastMessageAt DESC.
  threads.sort((a, b) => {
    if (a.state === "pending" && b.state !== "pending") return -1;
    if (b.state === "pending" && a.state !== "pending") return 1;
    return (
      new Date(b.lastMessageAt).getTime() -
      new Date(a.lastMessageAt).getTime()
    );
  });

  return threads;
}

export function findThread(
  threads: Thread[],
  key: string | null | undefined,
): Thread | null {
  if (!key) return null;
  const exact = threads.find((thread) => thread.key === key);
  if (!exact) return null;

  // An old conversation link used to identify every row sharing the workflow
  // conversation_id. After topology grouping, that value can span several
  // groups. Even if a legacy null-thread bucket still has the exact conv key,
  // selecting it would show only one arbitrary slice of the old conversation.
  if (key.startsWith("conv:")) {
    const conversationId = key.slice("conv:".length);
    const matchingGroups = threads.filter((thread) =>
      thread.messages.some(
        (message) => message.conversation_id === conversationId,
      ),
    );
    if (matchingGroups.length !== 1) return null;
  }

  return exact;
}
