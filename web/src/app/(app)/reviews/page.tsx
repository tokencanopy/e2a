"use client";

import { Suspense, useCallback, useEffect, useMemo, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import useSWR, { mutate } from "swr";
import {
  findPendingMessage,
  listPendingMessages,
  listScheduledMessages,
} from "../../components/onboarding/api";
import {
  invalidateAgents,
  invalidateAllAgentMessages,
  invalidateMessageDetail,
  invalidateMessageLifecycle,
  pendingMessagesKey,
  scheduledMessagesKey,
} from "../../../lib/swrKeys";
import type { PendingMessageSummary } from "../../components/types";
import { PageShell } from "../../components/loft/PageShell";
import { PendingRow } from "./_components/PendingRow";
import { ScheduledRow } from "./_components/ScheduledRow";

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
  // update the accordion immediately. A same-page router.replace does not
  // commit until the router has fetched the route's RSC payload, so deriving
  // expansion only from useSearchParams put a full network round trip between
  // the click and the row opening. Measured against the static export with
  // 1.2s of payload latency: 1210ms to expand before, 3.7ms after.
  //
  // Route changes we did not initiate stay authoritative — that is what deep
  // links and back/forward use. Rapid successive clicks need no special
  // handling: the router supersedes an in-flight same-page navigation rather
  // than committing it, so no superseded value is ever rendered.
  useEffect(() => {
    setSelectedId(routeSelectedId);
  }, [routeSelectedId]);

  // Single place that moves the selection: optimistic local state first, then
  // the URL. Passing "" collapses to the bare /reviews route.
  const select = useCallback(
    (id: string) => {
      setSelectedId(id);
      router.replace(id ? `/reviews?id=${encodeURIComponent(id)}` : "/reviews", {
        scroll: false,
      });
    },
    [router],
  );

  // Shared SWR key with the Sidebar's usePendingCount so the queue and
  // the badge share one fetch + cache entry.
  const {
    data: messages = [],
    error: swrError,
    isLoading,
  } = useSWR<PendingMessageSummary[]>(pendingMessagesKey, listPendingMessages);
  const loading = isLoading && messages.length === 0;
  const targetMissing =
    Boolean(selectedId) &&
    !isLoading &&
    !messages.some((message) => message.id === selectedId);
  const {
    data: recoveredMessage,
    error: recoveryError,
  } = useSWR<PendingMessageSummary | null>(
    targetMissing ? ["pending-review-deep-link", selectedId] : null,
    () => findPendingMessage(selectedId),
    { shouldRetryOnError: false },
  );
  const visibleMessages = useMemo(
    () =>
      recoveredMessage &&
      !messages.some((message) => message.id === recoveredMessage.id)
        ? [recoveredMessage, ...messages]
        : messages,
    [messages, recoveredMessage],
  );
  const combinedError = swrError ?? recoveryError;
  const error = combinedError
    ? combinedError instanceof Error
      ? combinedError.message
      : "Failed to load pending messages"
    : "";

  // Scheduled-send queue (GET /v1/scheduled): outbound messages accepted and
  // waiting for a future send_at. Shown as the page's second tab. Reuses the
  // PendingMessageSummary shape so both tabs share one row vocabulary.
  const { data: scheduled = [], isLoading: scheduledLoadingRaw } = useSWR<
    PendingMessageSummary[]
  >(scheduledMessagesKey, listScheduledMessages);
  const scheduledLoading = scheduledLoadingRaw && scheduled.length === 0;

  // Tab is URL-linkable (?tab=scheduled) so it survives refresh/deep-link. A
  // deep link to a specific hold (?id=) always lands on the Holds tab.
  const activeTab: "holds" | "scheduled" =
    !routeSelectedId && searchParams.get("tab") === "scheduled"
      ? "scheduled"
      : "holds";
  const selectTab = useCallback(
    (tab: "holds" | "scheduled") => {
      router.replace(tab === "scheduled" ? "/reviews?tab=scheduled" : "/reviews", {
        scroll: false,
      });
    },
    [router],
  );

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
  // row drops everywhere.
  const handleResolved = useCallback(async () => {
    const resolved = visibleMessages.find((m) => m.id === selectedId);
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
  }, [select, selectedId, visibleMessages]);

  return (
    <PageShell
      eyebrow="Review · Message holds"
      title={<>Pending review</>}
      subtitle={
        activeTab === "scheduled"
          ? scheduled.length > 0
            ? `${scheduled.length} message${scheduled.length === 1 ? "" : "s"} queued to send later`
            : "Messages scheduled to send at a future time appear here."
          : visibleMessages.length > 0
            ? `${visibleMessages.length} held ${visibleMessages.length === 1 ? "message" : "messages"} awaiting review`
            : "Inbound or outbound messages held by a review gate land here. Approve or reject each one."
      }
      maxWidth={900}
    >
      <div
        role="tablist"
        aria-label="Pending views"
        className="flex items-center gap-1 mb-4"
      >
        {(
          [
            { key: "holds", label: "Held", count: visibleMessages.length },
            { key: "scheduled", label: "Scheduled", count: scheduled.length },
          ] as const
        ).map((tab) => {
          const active = activeTab === tab.key;
          return (
            <button
              key={tab.key}
              role="tab"
              aria-selected={active}
              data-testid={`tab-${tab.key}`}
              onClick={() => selectTab(tab.key)}
              className="text-[13px] font-medium px-3 py-1.5"
              style={{
                borderRadius: "var(--r-md)",
                background: active ? "var(--bg-elev)" : "transparent",
                color: active ? "var(--fg)" : "var(--fg-muted)",
                border: active
                  ? "1px solid var(--border)"
                  : "1px solid transparent",
              }}
            >
              {tab.label}
              {tab.count > 0 && (
                <span style={{ color: "var(--fg-subtle)" }}> {tab.count}</span>
              )}
            </button>
          );
        })}
      </div>

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

      {activeTab === "holds" &&
        (loading ? (
          <div
            className="text-[13px] py-12 text-center"
            style={{ color: "var(--fg-muted)" }}
          >
            Loading…
          </div>
        ) : visibleMessages.length === 0 ? (
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
            {visibleMessages.map((m) => (
              <PendingRow
                key={m.id}
                summary={m}
                expanded={m.id === selectedId}
                onToggle={() => handleToggle(m.id)}
                onResolved={handleResolved}
              />
            ))}
          </div>
        ))}

      {activeTab === "scheduled" &&
        (scheduledLoading ? (
          <div
            className="text-[13px] py-12 text-center"
            style={{ color: "var(--fg-muted)" }}
          >
            Loading…
          </div>
        ) : scheduled.length === 0 ? (
          <div
            data-testid="scheduled-empty"
            className="p-12 text-center"
            style={{
              background: "var(--bg-panel)",
              border: "1px solid var(--border)",
              borderRadius: "var(--r-lg)",
            }}
          >
            <p className="text-[14px]" style={{ color: "var(--fg-muted)" }}>
              No scheduled messages.
            </p>
            <p className="text-[12px] mt-1" style={{ color: "var(--fg-subtle)" }}>
              Messages sent with a future send time wait here until they go out.
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
            {scheduled.map((m) => (
              <ScheduledRow key={m.id} summary={m} />
            ))}
          </div>
        ))}
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
