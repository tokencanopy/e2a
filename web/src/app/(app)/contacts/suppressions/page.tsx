"use client";

import { useCallback, useEffect, useState } from "react";
import { Button } from "../../../components/loft/Button";
import { Chip } from "../../../components/loft/Chip";
import { PageShell } from "../../../components/loft/PageShell";
import { ViewTabs } from "../_lib/ViewTabs";
import { appendUniqueByAddress, encodeAddressSegment } from "../_lib/suppressionPath";

// The ACCOUNT-WIDE suppression list: auto-populated by hard bounces and
// complaints (plus server-side manual entries), enforced for sends from every
// inbox on the account. List/remove only — there is no account-level create in
// the API; manual per-inbox blocks live on each inbox's Suppressions tab.
type Suppression = {
  address: string;
  reason?: string;
  source: string; // bounce | complaint | manual (open set)
  source_message_id?: string;
  created_at: string;
};

async function apiError(response: Response): Promise<string> {
  try {
    const body = await response.json();
    return body?.error?.message ?? `Request failed (HTTP ${response.status})`;
  } catch {
    return `Request failed (HTTP ${response.status})`;
  }
}

function sourceChip(source: string) {
  if (source === "bounce") return <Chip tone="danger">Bounce</Chip>;
  if (source === "complaint") return <Chip tone="danger">Complaint</Chip>;
  if (source === "manual") return <Chip tone="neutral">Manual</Chip>;
  return <Chip tone="neutral">{source}</Chip>;
}

export default function AccountSuppressionsPage() {
  const [rows, setRows] = useState<Suppression[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [nextCursor, setNextCursor] = useState("");
  const [loadingMore, setLoadingMore] = useState(false);
  const [removing, setRemoving] = useState("");
  // Distinct from `error`: only a failed LIST load may suppress the
  // "nothing is suppressed" empty state. A failed remove is an action error —
  // the list itself is still trustworthy.
  const [listFailed, setListFailed] = useState(false);

  const refresh = useCallback(async (cursor = "", append = false, signal?: AbortSignal) => {
    if (append) setLoadingMore(true);
    else setLoading(true);
    try {
      const params = new URLSearchParams({ limit: "100" });
      if (cursor) params.set("cursor", cursor);
      const response = await fetch(`/v1/account/suppressions?${params}`, {
        credentials: "include",
        signal,
      });
      if (!response.ok) throw new Error(await apiError(response));
      const body: { items: Suppression[]; next_cursor?: string | null } = await response.json();
      setRows((current) => append
        ? appendUniqueByAddress(current, body.items ?? [])
        : (body.items ?? []));
      setNextCursor(body.next_cursor ?? "");
      setError("");
      setListFailed(false);
    } catch (err) {
      if (!signal?.aborted) {
        setError(err instanceof Error ? err.message : "Failed to load suppressions");
        // Never leave a half-loaded list behind an error: on a safety list,
        // stale-or-empty rows read as "nothing is blocked".
        if (!append) setRows([]);
        setNextCursor("");
        setListFailed(true);
      }
    } finally {
      if (!signal?.aborted) {
        setLoading(false);
        setLoadingMore(false);
      }
    }
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    void refresh("", false, controller.signal);
    return () => controller.abort();
  }, [refresh]);

  const remove = async (row: Suppression) => {
    if (removing) return; // one delete in flight at a time
    // Build the path BEFORE prompting: an address that cannot be safely
    // encoded is not actionable, so there is nothing to confirm, and no
    // request must be issued.
    let path: string;
    try {
      path = `/v1/account/suppressions/${encodeAddressSegment(row.address)}?confirm=DELETE`;
    } catch (err) {
      setError(err instanceof Error ? err.message : "Invalid address");
      return;
    }
    if (!confirm(
      `Remove ${row.address} from the suppression list? Every inbox can email it again — ` +
      "removing an address that genuinely bounced or complained hurts sender reputation. " +
      "Only continue if you know it is deliverable.",
    )) return;
    setRemoving(row.address);
    try {
      const response = await fetch(path, { method: "DELETE", credentials: "include" });
      if (!response.ok) throw new Error(await apiError(response));
      setError("");
      await refresh();
    } catch (err) {
      const message = err instanceof Error ? err.message : "Failed to remove suppression";
      // A failed delete still refetches so the list reconverges; re-set the
      // action error afterwards, since a successful refresh clears it.
      await refresh();
      setError(message);
    } finally {
      setRemoving("");
    }
  };

  return (
    <PageShell
      eyebrow="Workspace"
      title="Suppressions"
      subtitle="Recipients e2a refuses to send to from every inbox on this account — added automatically on a hard bounce or spam complaint. Sends to them fail with recipient_suppressed. Per-inbox unsubscribe blocks live on each inbox's Suppressions tab."
      actions={<Button variant="ghost" onClick={() => void refresh()}>Refresh</Button>}
    >
      <ViewTabs active="suppressions" />

      {error && (
        <div role="alert" className="mb-4 rounded-[var(--r-md)] p-3 text-[13px]"
          style={{ color: "var(--danger-strong)", background: "var(--danger-bg)" }}>{error}</div>
      )}

      <div className="space-y-2 md:hidden">
        {loading ? (
          <div className="rounded-[var(--r-lg)] border p-4 text-[13px]"
            style={{ borderColor: "var(--border)", background: "var(--bg-panel)", color: "var(--fg-muted)" }}>
            Loading suppressions…
          </div>
        ) : rows.length === 0 ? (
          listFailed ? null : (
            <div className="rounded-[var(--r-lg)] border p-4 text-[13px]"
              style={{ borderColor: "var(--border)", background: "var(--bg-panel)", color: "var(--fg-muted)" }}>
              No suppressed recipients. Hard bounces and complaints land here automatically.
            </div>
          )
        ) : rows.map((row) => (
          <article key={row.address} className="rounded-[var(--r-lg)] border p-4"
            style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
            <div className="flex items-start justify-between gap-3">
              <p className="min-w-0 truncate font-mono text-[12px]" style={{ color: "var(--fg)" }}>{row.address}</p>
              {sourceChip(row.source)}
            </div>
            {row.reason && (
              <p className="mt-2 text-[12px] break-words" style={{ color: "var(--fg-muted)" }} title={row.reason}>
                {row.reason}
              </p>
            )}
            <div className="mt-3 flex items-center justify-between gap-2">
              <span className="min-w-0 truncate text-[11px]" style={{ color: "var(--fg-muted)" }}>
                {new Date(row.created_at).toLocaleDateString()}
                {row.source_message_id && (
                  <> · <span className="font-mono">{row.source_message_id}</span></>
                )}
              </span>
              <Button variant="ghost" aria-label={`Remove ${row.address}`} disabled={removing !== ""}
                style={{ color: "var(--danger-strong)" }} onClick={() => void remove(row)}>Remove</Button>
            </div>
          </article>
        ))}
      </div>

      <div className="hidden overflow-x-auto rounded-[var(--r-lg)] border md:block"
        style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
        <table className="w-full min-w-[860px] text-left text-[13px]">
          <thead><tr style={{ borderBottom: "1px solid var(--border)" }}>
            <th className="px-4 py-3 font-medium">Address</th>
            <th className="px-4 py-3 font-medium">Source</th>
            <th className="px-4 py-3 font-medium">Reason</th>
            <th className="px-4 py-3 font-medium">Source message</th>
            <th className="px-4 py-3 font-medium">Added</th>
            <th className="px-4 py-3 font-medium text-right">Actions</th>
          </tr></thead>
          <tbody>
            {loading ? (
              <tr><td colSpan={6} className="px-4 py-8" style={{ color: "var(--fg-muted)" }}>Loading suppressions…</td></tr>
            ) : rows.length === 0 ? (
              <tr><td colSpan={6} className="px-4 py-8" style={{ color: "var(--fg-muted)" }}>
                {listFailed
                  ? "The list could not be loaded, so it is not shown."
                  : "No suppressed recipients. Hard bounces and complaints land here automatically."}
              </td></tr>
            ) : rows.map((row) => (
              <tr key={row.address} style={{ borderBottom: "1px solid var(--border-sub)" }}>
                <td className="px-4 py-3 font-mono text-[12px]" style={{ color: "var(--fg)" }}>{row.address}</td>
                <td className="px-4 py-3">{sourceChip(row.source)}</td>
                <td className="px-4 py-3 max-w-[340px] truncate" style={{ color: "var(--fg-muted)" }}
                  title={row.reason || undefined}>
                  {row.reason || "—"}
                </td>
                <td className="px-4 py-3 font-mono text-[11px]" style={{ color: "var(--fg-muted)" }}>
                  {row.source_message_id || "—"}
                </td>
                <td className="px-4 py-3" style={{ color: "var(--fg-muted)" }}>
                  {new Date(row.created_at).toLocaleDateString()}
                </td>
                <td className="px-4 py-3 text-right">
                  <Button variant="ghost" aria-label={`Remove ${row.address}`} disabled={removing !== ""}
                    style={{ color: "var(--danger-strong)" }} onClick={() => void remove(row)}>Remove</Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {nextCursor && (
        <div className="mt-3 flex justify-center">
          <Button variant="ghost" disabled={loadingMore}
            onClick={() => void refresh(nextCursor, true)}>
            {loadingMore ? "Loading…" : "Load more suppressions"}
          </Button>
        </div>
      )}
    </PageShell>
  );
}
