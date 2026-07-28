"use client";

import { Suspense, useCallback, useEffect, useRef, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import useSWR, { mutate } from "swr";
import { listPendingMessages } from "../../components/onboarding/api";
import {
  invalidateAgents,
  invalidateAllAgentMessages,
  invalidateMessageDetail,
  invalidateMessageLifecycle,
  pendingMessagesKey,
} from "../../../lib/swrKeys";
import type { PendingMessageSummary } from "../../components/types";
import { PageShell } from "../../components/loft/PageShell";
import { PendingRow } from "./_components/PendingRow";

// Pending review — a single-column "outbound holds" inbox. Each row is an
// agent-drafted reply awaiting approval; clicking expands it read-first
// (body + Details) with an Approve / Edit / Reject action bar (accordion:
// one open at a time, tracked via ?id=). This converges the pending
// surface onto the same row language as the agent inbox and folds in the
// old master-detail PendingDetailPanel.

function PendingContent() {
  const searchParams = useSearchParams();
  const router = useRouter();
  const routeSelectedId = searchParams.get("id") ?? "";
  const [selectedId, setSelectedId] = useState(routeSelectedId);

  // The id our own last router.replace is heading for, or null when the URL
  // is already caught up. Next's app router queues navigations and commits
  // every one of them in order, so a replace issued while an earlier one is
  // still in flight produces an intermediate render at the SUPERSEDED value.
  // Syncing from that echo would collapse the row the user just opened (and
  // tear down its in-flight detail fetch) before the real target lands.
  const pendingId = useRef<string | null>(null);

  // Keep deep links and browser navigation in sync, while letting row clicks
  // update the accordion immediately. A same-page router.replace can lag
  // briefly after resolving a hold; deriving expansion only from
  // useSearchParams made the next row appear inert until that transition
  // committed (or the page was refreshed). Route changes we did not initiate
  // are still authoritative — that is what deep links and back/forward use.
  useEffect(() => {
    if (pendingId.current !== null) {
      // Still catching up to our own navigation: ignore superseded echoes,
      // and stop guarding once the target lands (local state already matches).
      if (routeSelectedId === pendingId.current) pendingId.current = null;
      return;
    }
    setSelectedId(routeSelectedId);
  }, [routeSelectedId]);

  // Single place that moves the selection: optimistic local state first, then
  // the URL. Passing "" collapses to the bare /reviews route.
  const select = useCallback(
    (id: string) => {
      pendingId.current = id === routeSelectedId ? null : id;
      setSelectedId(id);
      router.replace(id ? `/reviews?id=${encodeURIComponent(id)}` : "/reviews", {
        scroll: false,
      });
    },
    [router, routeSelectedId],
  );

  // Shared SWR key with the Sidebar's usePendingCount so the queue and
  // the badge share one fetch + cache entry.
  const {
    data: messages = [],
    error: swrError,
    isLoading,
  } = useSWR<PendingMessageSummary[]>(pendingMessagesKey, listPendingMessages);
  const loading = isLoading && messages.length === 0;
  const error = swrError
    ? swrError instanceof Error
      ? swrError.message
      : "Failed to load pending messages"
    : "";

  // Accordion toggle: open a row (?id=) or collapse it if already open.
  const handleToggle = useCallback(
    (id: string) => {
      select(id === selectedId ? "" : id);
    },
    [selectedId, select],
  );

  // After approve/reject: refetch the queue, collapse to a clean list,
  // and invalidate the derived caches (sidebar badge, agent cards, the
  // inbox views, the resolved message's lifecycle panel) so the resolved
  // row drops everywhere — mirroring what the focus page used to do.
  const handleResolved = useCallback(async () => {
    const resolved = messages.find((m) => m.id === selectedId);
    void Promise.all([
      selectedId ? invalidateMessageDetail(selectedId) : Promise.resolve(),
      resolved
        ? invalidateMessageLifecycle(resolved.agent_email, resolved.id)
        : Promise.resolve(),
      invalidateAgents(),
      invalidateAllAgentMessages(),
    ]);
    select("");
    await mutate(pendingMessagesKey);
  }, [select, selectedId, messages]);

  return (
    <PageShell
      eyebrow="Review · Message holds"
      title={<>Pending review</>}
      subtitle={
        messages.length > 0
          ? `${messages.length} held ${messages.length === 1 ? "message" : "messages"} awaiting review`
          : "Inbound or outbound messages held by a review gate land here. Approve or reject each one."
      }
      maxWidth={900}
    >
      {error && (
        <div
          className="mb-4 p-3 text-[13px]"
          style={{
            background: "var(--danger-bg)",
            color: "var(--danger-strong)",
            border: "1px solid var(--danger-bg)",
            borderRadius: "var(--r-md)",
          }}
        >
          {error}
        </div>
      )}

      {loading ? (
        <div
          className="text-[13px] py-12 text-center"
          style={{ color: "var(--fg-muted)" }}
        >
          Loading…
        </div>
      ) : messages.length === 0 ? (
        <div
          data-testid="pending-empty"
          className="p-12 text-center"
          style={{
            background: "var(--bg-panel)",
            border: "1px solid var(--border)",
            borderRadius: "var(--r-lg)",
          }}
        >
          <p className="text-[14px]" style={{ color: "var(--fg-muted)" }}>
            Nothing waiting for review.
          </p>
          <p className="text-[12px] mt-1" style={{ color: "var(--fg-subtle)" }}>
            Inbound or outbound messages held by an inbox&apos;s review gate
            appear here. Configure holds in an inbox&apos;s Settings →
            Protection.
          </p>
        </div>
      ) : (
        <div
          style={{
            background: "var(--bg-panel)",
            border: "1px solid var(--border)",
            borderRadius: "var(--r-lg)",
            overflow: "hidden",
          }}
        >
          {messages.map((m) => (
            <PendingRow
              key={m.id}
              summary={m}
              expanded={m.id === selectedId}
              onToggle={() => handleToggle(m.id)}
              onResolved={handleResolved}
            />
          ))}
        </div>
      )}
    </PageShell>
  );
}

export default function PendingPage() {
  return (
    <Suspense
      fallback={
        <PageShell>
          <div
            className="text-[13px] py-12 text-center"
            style={{ color: "var(--fg-muted)" }}
          >
            Loading…
          </div>
        </PageShell>
      }
    >
      <PendingContent />
    </Suspense>
  );
}
