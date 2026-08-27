"use client";

// Per-inbox Trash — soft-deleted messages, Gmail-style. Rows can be
// restored (back to the inbox, retention clock resumes) or deleted
// forever (two-click confirm; irreversible). Everything left in the
// trash is purged automatically ~30 days after deletion.

import { Suspense, useState } from "react";
import { useSearchParams } from "next/navigation";
import useSWR from "swr";
import {
  listAgentMessages,
  restoreMessage,
  purgeMessage,
} from "../../../../components/onboarding/api";
import { Avatar } from "@e2a/ui";
import { formatRelativeAge } from "../../../../../lib/relativeTime";
import {
  agentTrashKey,
  invalidateAgentMessages,
  invalidateAgentUnread,
} from "../../../../../lib/swrKeys";
import { TRASH_RETENTION_DAYS, daysLeft } from "../../../../../lib/trash";
import type { MessageSummary } from "../../../../components/types";

export default function AgentTrashPage() {
  return (
    <Suspense fallback={null}>
      <AgentTrashContent />
    </Suspense>
  );
}

function AgentTrashContent() {
  const searchParams = useSearchParams();
  const email = searchParams.get("email") ?? "";

  const { data, error, isLoading, mutate } = useSWR(
    email ? agentTrashKey(email) : null,
    () => listAgentMessages(email, { deleted: true, pageSize: 100 }),
    { keepPreviousData: false },
  );
  const rows = data?.items ?? [];

  // Per-row in-flight + error state, keyed by message id.
  const [busy, setBusy] = useState<string | null>(null);
  const [rowError, setRowError] = useState<{ id: string; msg: string } | null>(null);
  // Two-click "Delete forever": first click arms, second click fires.
  const [armed, setArmed] = useState<string | null>(null);

  const run = async (id: string, op: () => Promise<void>) => {
    setBusy(id);
    setRowError(null);
    try {
      await op();
      await mutate(); // refresh the trash list
      // The live inbox views are stale after a restore.
      void invalidateAgentMessages(email);
      void invalidateAgentUnread(email);
    } catch (err) {
      setRowError({
        id,
        msg: err instanceof Error ? err.message : "Operation failed",
      });
    } finally {
      setBusy(null);
      setArmed(null);
    }
  };

  return (
    <div
      data-testid="agent-trash"
      style={{ borderTop: "1px solid var(--border)", minHeight: 520 }}
    >
      <div style={{ padding: "20px 28px 8px" }}>
        <p style={{ fontSize: 13, color: "var(--fg-muted)", margin: 0 }}>
          Messages you delete are kept here for {TRASH_RETENTION_DAYS} days,
          then deleted forever. Restoring a message returns it to the inbox.
        </p>
      </div>

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
          {error instanceof Error ? error.message : "Failed to load trash"}
        </div>
      )}
      {!error && isLoading && (
        <div className="px-7 py-8 text-[13px]" style={{ color: "var(--fg-muted)" }}>
          Loading trash…
        </div>
      )}
      {!error && !isLoading && rows.length === 0 && (
        <div
          data-testid="trash-empty"
          className="px-7 py-16 text-center text-[13px]"
          style={{ color: "var(--fg-muted)" }}
        >
          The trash is empty.
        </div>
      )}

      {rows.length > 0 && (
        <ul style={{ listStyle: "none", margin: 0, padding: "8px 28px 28px" }}>
          {rows.map((m: MessageSummary) => {
            const counterparty = m.direction === "inbound" ? m.from : (m.to[0] ?? "");
            return (
              <li
                key={m.id}
                data-testid="trash-row"
                className="flex items-center gap-3 flex-wrap"
                style={{
                  padding: "12px 14px",
                  borderBottom: "1px solid var(--border-sub)",
                }}
              >
                <Avatar email={counterparty} size={22} />
                <div className="min-w-0 flex-1">
                  <div
                    className="truncate"
                    style={{ fontSize: 13, fontWeight: 500, color: "var(--fg)" }}
                  >
                    {m.subject || "(no subject)"}
                  </div>
                  <div
                    className="truncate"
                    style={{
                      fontFamily: "var(--f-mono)",
                      fontSize: 11,
                      color: "var(--fg-subtle)",
                    }}
                  >
                    {m.direction === "inbound" ? "from" : "to"} {counterparty}
                    {m.deleted_at && (
                      <>
                        {" · "}deleted {formatRelativeAge(m.deleted_at)}
                        {" · "}purges in {daysLeft(m.deleted_at)}d
                      </>
                    )}
                  </div>
                  {rowError?.id === m.id && (
                    <div
                      className="mt-1 text-[12px]"
                      style={{ color: "var(--danger-strong)" }}
                    >
                      {rowError.msg}
                    </div>
                  )}
                </div>
                <button
                  type="button"
                  disabled={busy === m.id}
                  onClick={() => run(m.id, () => restoreMessage(email, m.id))}
                  className="text-[12px] px-3 py-1.5 hover:bg-[var(--bg-elev)] transition"
                  style={{
                    background: "var(--bg-panel)",
                    border: "1px solid var(--border)",
                    borderRadius: "var(--r-md)",
                    color: "var(--fg)",
                    cursor: busy === m.id ? "default" : "pointer",
                  }}
                >
                  {busy === m.id ? "Working…" : "Restore"}
                </button>
                <button
                  type="button"
                  disabled={busy === m.id}
                  onClick={() => {
                    if (armed !== m.id) {
                      setArmed(m.id);
                      return;
                    }
                    void run(m.id, () => purgeMessage(email, m.id));
                  }}
                  className="text-[12px] px-3 py-1.5 transition"
                  style={{
                    background: armed === m.id ? "var(--danger-bg)" : "var(--bg-panel)",
                    border: "1px solid var(--danger-strong)",
                    borderRadius: "var(--r-md)",
                    color: "var(--danger-strong)",
                    cursor: busy === m.id ? "default" : "pointer",
                  }}
                >
                  {armed === m.id ? "Click again to confirm" : "Delete forever"}
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
