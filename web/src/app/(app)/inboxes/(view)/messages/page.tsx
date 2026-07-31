"use client";

// Inbox (threaded) — primary per-agent screen.
// Two-column grid: thread list (360px) | thread detail (1fr).
// Threads grouped client-side over a 100-row window of mixed inbound +
// outbound messages from `GET /v1/agents/{address}/messages?direction=all`.
// Server-side conversations endpoint is a tracked follow-up; until it
// lands, the window may starve old threads for accounts with >100
// recent messages.
//
// Selection state lives in `window.location.hash` (#thr:X for new rows, with
// #conv:X / #orphan:X retained for unambiguous legacy rows) so deep-links
// work and the back button moves between threads.

import { Suspense, useMemo, useState, useSyncExternalStore } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import useSWR from "swr";
import { listAgentMessages } from "../../../../components/onboarding/api";
import type { MessageSummary } from "../../../../components/types";
import { ThreadList } from "../../../../components/messages/ThreadList";
import { ThreadDetail } from "../../../../components/messages/ThreadDetail";
import {
  decodeThreadFragment,
  encodeThreadFragment,
  findThread,
  groupIntoThreads,
} from "../../../../components/messages/threading";
import { inboxPolling } from "../../../../../lib/livePolling";
import { agentMessagesKey } from "../../../../../lib/swrKeys";

// Sync the URL fragment into React state. useSyncExternalStore is the
// idiomatic way to read browser-owned state without effect ping-pong.
function getHash(): string {
  if (typeof window === "undefined") return "";
  return window.location.hash
    ? decodeThreadFragment(window.location.hash.slice(1))
    : "";
}
function subscribeHash(onChange: () => void) {
  window.addEventListener("hashchange", onChange);
  return () => window.removeEventListener("hashchange", onChange);
}
function useUrlHash(): string {
  return useSyncExternalStore(subscribeHash, getHash, () => "");
}

// AgentInboxPage wraps the content in <Suspense>. Next.js 16+ requires
// useSearchParams() to live inside a Suspense boundary; otherwise the
// whole route opts into client-only rendering and any future server
// component above this page silently bails the static export.
export default function AgentInboxPage() {
  return (
    <Suspense fallback={null}>
      <AgentInboxRoute />
    </Suspense>
  );
}

function AgentInboxRoute() {
  const searchParams = useSearchParams();
  const email = searchParams.get("email") ?? "";
  return <AgentInboxContent key={email} email={email} />;
}

function AgentInboxContent({ email }: { email: string }) {
  const router = useRouter();

  // Initial 100-row window. SWR keys by email so navigating between
  // agents fetches independently; review actions invalidate this query.
  const {
    data: initialPage,
    error: fetchError,
  } = useSWR(
    email ? agentMessagesKey(email, "all", "all") : null,
    () => listAgentMessages(email, { direction: "all", status: "all", pageSize: 100 }),
    // `keepPreviousData` is on globally for the smooth-revalidation
    // UX, but for per-agent keys it shows the WRONG agent's data
    // during a ?email=A → ?email=B switch. Disable here so the page
    // flashes a brief loading state instead of mis-attributing
    // messages to the new agent.
    { ...inboxPolling, keepPreviousData: false },
  );

  // "Load older" appends additional pages keyed by the prior page's
  // next_cursor. We keep these in local state because SWR's cache key
  // would need the cursor in it (defeating the dedup) — appended
  // pages are append-only so a separate state ref works fine.
  const [olderPages, setOlderPages] = useState<MessageSummary[][]>([]);
  const [latestCursor, setLatestCursor] = useState<string | null | undefined>(undefined);
  const [loadingMore, setLoadingMore] = useState(false);
  const [loadError, setLoadError] = useState("");

  // Concatenate the initial page with any imperatively-loaded older
  // pages, then de-dupe by `id`. The de-dupe matters because
  // SWR can revalidate the initial page mid-session (focus event,
  // explicit invalidation from the Review page). New
  // messages arriving at the top push the initial-page boundary
  // down, which can re-include rows that already live in
  // `olderPages`. Without this de-dupe, the same message renders
  // twice in the thread bucket and `msgCount` lies.
  const rows: MessageSummary[] = useMemo(() => {
    const initialMessages = initialPage?.items ?? [];
    const seen = new Set<string>();
    const out: MessageSummary[] = [];
    for (const m of [...initialMessages, ...olderPages.flat()]) {
      if (seen.has(m.id)) continue;
      seen.add(m.id);
      out.push(m);
    }
    return out;
  }, [initialPage?.items, olderPages]);
  // The cursor to use for the next "Load older" click is the most
  // recent next_cursor we've seen (either from the initial fetch or
  // the latest appended page).
  const nextCursor: string | null =
    latestCursor !== undefined ? latestCursor : (initialPage?.next_cursor ?? null);

  const threads = useMemo(
    () => (rows.length > 0 ? groupIntoThreads(rows) : []),
    [rows],
  );
  const hash = useUrlHash();
  // Gmail model: an empty hash shows the inbox LIST; a hash selects a
  // thread and shows that conversation full-width. (No auto-select of
  // threads[0] — the list is the default landing.)
  const selected = findThread(threads, hash);
  const pendingCount = threads.filter((t) => t.state === "pending").length;
  const error = loadError || (fetchError ? fetchError.message || "Failed to load messages" : "");

  const selectThread = (key: string) => {
    if (typeof window !== "undefined") {
      // pushState (not replace) so opening a conversation adds a history
      // entry — the browser Back button then returns to the thread list
      // instead of skipping it and jumping to the top-level inbox list.
      window.history.pushState(null, "", `#${encodeThreadFragment(key)}`);
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    }
  };
  // Back to the inbox list — strip the thread hash.
  const clearSelection = () => {
    if (typeof window !== "undefined") {
      window.history.replaceState(
        null,
        "",
        window.location.pathname + window.location.search,
      );
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    }
  };

  // Reading stays inline in the conversation. A held draft opens the same
  // account-wide Review row used by the sidebar and notification email.
  const openMessage = (m: MessageSummary) => {
    router.push(`/reviews?id=${encodeURIComponent(m.id)}`);
  };

  const loadOlder = async () => {
    if (!nextCursor) return;
    // Capture the email at call time so we can detect a navigation
    // (?email=… changed) before the response lands. Without this, a
    // late response would merge into the wrong agent's rows.
    const startEmail = email;
    setLoadingMore(true);
    setLoadError("");
    try {
      const res = await listAgentMessages(startEmail, {
        direction: "all",
        status: "all",
        pageSize: 100,
        cursor: nextCursor,
      });
      if (startEmail !== email) return;
      setOlderPages((prev) => [...prev, res.items]);
      setLatestCursor(res.next_cursor ?? null);
    } catch (err) {
      if (startEmail !== email) return;
      setLoadError(err instanceof Error ? err.message : "Failed to load older messages");
    } finally {
      if (startEmail === email) setLoadingMore(false);
    }
  };

  return (
    <div
      data-testid="agent-inbox"
      className="flex flex-col"
      style={{
        borderTop: "1px solid var(--border)",
        // Natural-height flow so the page owns a single scroll (document /
        // app-shell) instead of a nested internal scroller. The old
        // `height: calc(100vh …)` viewport-lock made the list/conversation
        // scroll internally — an email-client idiom that on mobile produced
        // two scrollbars and a header you couldn't scroll past. The header
        // still stays put on desktop via `position: sticky` (ThreadDetail),
        // which needs no second scroll container. minHeight keeps short
        // threads from collapsing the panel on tall desktop viewports.
        minHeight: 520,
      }}
    >
      {error && (
        <div
          className="m-6 p-4 text-[13px]"
          style={{
            background: "var(--danger-bg)",
            border: "1px solid var(--danger-bg)",
            color: "var(--danger-strong)",
            borderRadius: "var(--r-md)",
          }}
        >
          {error}
        </div>
      )}
      {!error && !initialPage && (
        <div
          className="px-7 py-8 text-[13px]"
          style={{ color: "var(--fg-muted)" }}
        >
          Loading inbox…
        </div>
      )}
      {initialPage && (
        <div className="flex-1 flex flex-col">
          {selected ? (
            <ThreadDetail
              thread={selected}
              agentEmail={email}
              onBack={clearSelection}
              onOpenMessage={openMessage}
              historyIncomplete={!!nextCursor}
            />
          ) : (
            <ThreadList
              threads={threads}
              selectedKey={null}
              onSelect={selectThread}
              total={threads.length}
              pendingCount={pendingCount}
              hasMore={!!nextCursor}
              onLoadMore={loadOlder}
              loadingMore={loadingMore}
            />
          )}
        </div>
      )}
    </div>
  );
}
