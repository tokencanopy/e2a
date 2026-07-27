"use client";

import { Suspense, useCallback, useEffect, useState } from "react";
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

  // Keep deep links and browser navigation in sync, while letting row clicks
  // update the accordion immediately. A same-page router.replace can lag
  // briefly after resolving a hold; deriving expansion only from
  // useSearchParams made the next row appear inert until that transition
  // committed (or the page was refreshed).
  useEffect(() => {
    setSelectedId(routeSelectedId);
  }, [routeSelectedId]);

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
      if (id === selectedId) {
        setSelectedId("");
        router.replace("/reviews", { scroll: false });
      } else {
        setSelectedId(id);
        router.replace(`/reviews?id=${encodeURIComponent(id)}`, {
          scroll: false,
        });
      }
    },
    [selectedId, router],
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
    setSelectedId("");
    router.replace("/reviews", { scroll: false });
    await mutate(pendingMessagesKey);
  }, [router, selectedId, messages]);

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
