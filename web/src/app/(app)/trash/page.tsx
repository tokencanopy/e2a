"use client";

// Account-wide Trash — deleted inboxes (agents), Gmail-style. A trashed
// inbox stops receiving mail and disappears from the Inboxes list, but can
// be restored — messages and configuration intact — for 30 days, after
// which it is purged permanently together with its messages. Deleted
// MESSAGES live in each inbox's own Trash tab; this page points there.

import { useState } from "react";
import useSWR from "swr";
import Link from "next/link";
import {
  listDeletedAgents,
  restoreAgent,
  permanentDeleteAgent,
} from "../../components/onboarding/api";
import { PageShell } from "../../components/loft/PageShell";
import { Avatar } from "@e2a/ui";
import { formatRelativeAge } from "../../../lib/relativeTime";
import {
  deletedAgentsKey,
  invalidateAgentUnread,
  invalidateAgents,
} from "../../../lib/swrKeys";
import { TRASH_RETENTION_DAYS, daysLeft } from "../../../lib/trash";
import type { DashboardAgent } from "../../components/types";

export default function TrashPage() {
  const { data, error, isLoading, mutate } = useSWR(
    deletedAgentsKey,
    listDeletedAgents,
  );
  const agents = data ?? [];

  const [busy, setBusy] = useState<string | null>(null);
  const [rowError, setRowError] = useState<{ email: string; msg: string } | null>(null);
  // Two-click "Delete forever": first click arms, second click fires.
  const [armed, setArmed] = useState<string | null>(null);

  const run = async (
    email: string,
    op: () => Promise<unknown>,
    afterSuccess?: () => void,
  ) => {
    setBusy(email);
    setRowError(null);
    try {
      await op();
      afterSuccess?.();
      await mutate(); // refresh the trash list
      void invalidateAgents(); // a restore re-adds the inbox to the live list
    } catch (err) {
      setRowError({
        email,
        msg: err instanceof Error ? err.message : "Operation failed",
      });
    } finally {
      setBusy(null);
      setArmed(null);
    }
  };

  return (
    <PageShell
      eyebrow="Workspace"
      title={<>Trash</>}
      subtitle={
        <>
          Deleted inboxes are kept for {TRASH_RETENTION_DAYS}{" "}days, then
          purged permanently together with their messages. Deleted messages
          live in each inbox&apos;s own{" "}
          <span style={{ color: "var(--fg)" }}>Trash</span> tab.
        </>
      }
    >
      {error && (
        <div
          className="mb-6 p-3 text-[13px]"
          style={{
            background: "var(--danger-bg)",
            color: "var(--danger-strong)",
            border: "1px solid var(--danger-bg)",
            borderRadius: "var(--r-md)",
          }}
        >
          {error instanceof Error ? error.message : "Failed to load trash"}
        </div>
      )}

      {isLoading ? (
        <div className="text-[13px] py-12 text-center" style={{ color: "var(--fg-muted)" }}>
          Loading...
        </div>
      ) : agents.length === 0 ? (
        <div
          data-testid="trash-empty"
          className="text-[13px] py-16 text-center"
          style={{ color: "var(--fg-muted)" }}
        >
          The trash is empty. Deleting an inbox (from its Settings page)
          moves it here.{" "}
          <Link href="/inboxes" style={{ color: "var(--accent-strong)" }}>
            Back to Inboxes →
          </Link>
        </div>
      ) : (
        <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
          {agents.map((a: DashboardAgent) => (
            <li
              key={a.email}
              data-testid="trash-inbox-row"
              className="flex items-center gap-3 flex-wrap"
              style={{
                padding: "14px 16px",
                background: "var(--bg-panel)",
                border: "1px solid var(--border)",
                borderRadius: "var(--r-lg)",
                marginBottom: 10,
              }}
            >
              <Avatar email={a.email} name={a.name} size={28} />
              <div className="min-w-0 flex-1">
                <div style={{ fontSize: 14, fontWeight: 600, color: "var(--fg)" }}>
                  {a.name || a.email.split("@")[0]}
                </div>
                <div
                  className="truncate"
                  style={{
                    fontFamily: "var(--f-mono)",
                    fontSize: 11,
                    color: "var(--fg-subtle)",
                  }}
                >
                  {a.email}
                  {a.deleted_at && (
                    <>
                      {" · "}deleted {formatRelativeAge(a.deleted_at)}
                      {" · "}purges in {daysLeft(a.deleted_at)}d
                    </>
                  )}
                </div>
                {rowError?.email === a.email && (
                  <div className="mt-1 text-[12px]" style={{ color: "var(--danger-strong)" }}>
                    {rowError.msg}
                  </div>
                )}
              </div>
              <button
                type="button"
                disabled={busy === a.email}
                onClick={() =>
                  run(
                    a.email,
                    () => restoreAgent(a.email),
                    () => {
                      void invalidateAgentUnread(a.email);
                    },
                  )
                }
                className="text-[12px] px-3 py-1.5 hover:bg-[var(--bg-elev)] transition"
                style={{
                  background: "var(--bg-panel)",
                  border: "1px solid var(--border)",
                  borderRadius: "var(--r-md)",
                  color: "var(--fg)",
                  cursor: busy === a.email ? "default" : "pointer",
                }}
              >
                {busy === a.email ? "Working…" : "Restore"}
              </button>
              <button
                type="button"
                disabled={busy === a.email}
                onClick={() => {
                  if (armed !== a.email) {
                    setArmed(a.email);
                    return;
                  }
                  void run(a.email, () => permanentDeleteAgent(a.email));
                }}
                className="text-[12px] px-3 py-1.5 transition"
                style={{
                  background: armed === a.email ? "var(--danger-bg)" : "var(--bg-panel)",
                  border: "1px solid var(--danger-strong)",
                  borderRadius: "var(--r-md)",
                  color: "var(--danger-strong)",
                  cursor: busy === a.email ? "default" : "pointer",
                }}
              >
                {armed === a.email ? "Click again to confirm" : "Delete forever"}
              </button>
            </li>
          ))}
        </ul>
      )}
    </PageShell>
  );
}
