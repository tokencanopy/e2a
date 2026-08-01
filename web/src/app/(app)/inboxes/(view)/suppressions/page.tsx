"use client";

import { Suspense, useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { Button } from "../../../../components/loft/Button";
import { Chip } from "../../../../components/loft/Chip";

// Per-agent recipient blocks: unsubscribes plus manual entries, scoped to this
// exact sending inbox. Account-wide bounce/complaint suppressions are a
// separate list under Contacts → Suppressions.
type AgentSuppression = {
  agent_email: string;
  address: string;
  reason?: string;
  source: string; // unsubscribe | manual (open set)
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
  if (source === "unsubscribe") return <Chip tone="warn">Unsubscribe</Chip>;
  if (source === "manual") return <Chip tone="neutral">Manual</Chip>;
  return <Chip tone="neutral">{source}</Chip>;
}

export default function SuppressionsPage() {
  return <Suspense fallback={null}><SuppressionsRouter /></Suspense>;
}

function SuppressionsRouter() {
  const email = useSearchParams().get("email") ?? "";
  return <SuppressionsContent key={email} email={email} />;
}

function SuppressionsContent({ email }: { email: string }) {
  const [rows, setRows] = useState<AgentSuppression[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [showAdd, setShowAdd] = useState(false);
  const [nextCursor, setNextCursor] = useState("");
  const [loadingMore, setLoadingMore] = useState(false);

  const refresh = useCallback(async (cursor = "", append = false, signal?: AbortSignal) => {
    if (!email) return;
    if (append) setLoadingMore(true);
    else setLoading(true);
    try {
      const params = new URLSearchParams({ limit: "100" });
      if (cursor) params.set("cursor", cursor);
      const response = await fetch(
        `/v1/agents/${encodeURIComponent(email)}/suppressions?${params}`,
        { credentials: "include", signal },
      );
      if (!response.ok) throw new Error(await apiError(response));
      const body: { items: AgentSuppression[]; next_cursor?: string | null } = await response.json();
      setRows((current) => append ? [...current, ...(body.items ?? [])] : (body.items ?? []));
      setNextCursor(body.next_cursor ?? "");
      setError("");
    } catch (err) {
      if (!signal?.aborted) {
        setError(err instanceof Error ? err.message : "Failed to load suppressions");
      }
    } finally {
      if (!signal?.aborted) {
        setLoading(false);
        setLoadingMore(false);
      }
    }
  }, [email]);

  useEffect(() => {
    const controller = new AbortController();
    void refresh("", false, controller.signal);
    return () => controller.abort();
  }, [refresh]);

  const remove = async (row: AgentSuppression) => {
    if (!confirm(`Let ${email} email ${row.address} again? This removes only this inbox's block.`)) return;
    try {
      const response = await fetch(
        `/v1/agents/${encodeURIComponent(email)}/suppressions/${encodeURIComponent(row.address)}?confirm=DELETE`,
        { method: "DELETE", credentials: "include" },
      );
      if (!response.ok) throw new Error(await apiError(response));
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to remove suppression");
    }
  };

  return (
    <div className="mx-auto w-full px-6 py-7 md:px-8" style={{ maxWidth: 1080 }}>
      <div className="mb-5 flex flex-col gap-3 md:flex-row md:items-end md:justify-between">
        <div>
          <div className="flex items-center gap-2">
            <h2 className="text-[20px] font-semibold" style={{ color: "var(--fg)" }}>Suppressions</h2>
            <Chip tone="warn">Beta</Chip>
          </div>
          <p className="mt-1 max-w-[680px] text-[13px] leading-[1.6]" style={{ color: "var(--fg-muted)" }}>
            Recipients blocked only for this inbox: unsubscribe requests and manual blocks. Sends to them fail
            with recipient_suppressed. Account-wide bounce and complaint blocks live under{" "}
            <Link href="/contacts/suppressions" style={{ color: "var(--fg)", textDecoration: "underline" }}>
              Contacts → Suppressions
            </Link>.
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="ghost" onClick={() => void refresh()}>Refresh</Button>
          <Button onClick={() => setShowAdd((value) => !value)}>Suppress an address</Button>
        </div>
      </div>

      {showAdd && (
        <AddSuppressionPanel
          email={email}
          onCancel={() => setShowAdd(false)}
          onAdded={async () => {
            setShowAdd(false);
            await refresh();
          }}
        />
      )}

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
          <div className="rounded-[var(--r-lg)] border p-4 text-[13px]"
            style={{ borderColor: "var(--border)", background: "var(--bg-panel)", color: "var(--fg-muted)" }}>
            No blocked recipients for this inbox. Unsubscribe requests land here automatically; manual blocks
            can be added above.
          </div>
        ) : rows.map((row) => (
          <article key={row.address} className="rounded-[var(--r-lg)] border p-4"
            style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
            <div className="flex items-start justify-between gap-3">
              <p className="min-w-0 truncate font-mono text-[12px]" style={{ color: "var(--fg)" }}>{row.address}</p>
              {sourceChip(row.source)}
            </div>
            {row.reason && (
              <p className="mt-2 text-[12px]" style={{ color: "var(--fg-muted)" }} title={row.reason}>
                {row.reason}
              </p>
            )}
            <div className="mt-3 flex items-center justify-between">
              <span className="text-[11px]" style={{ color: "var(--fg-muted)" }}>
                Added {new Date(row.created_at).toLocaleDateString()}
              </span>
              <Button variant="ghost" aria-label={`Remove ${row.address}`}
                style={{ color: "var(--danger-strong)" }} onClick={() => void remove(row)}>Remove</Button>
            </div>
          </article>
        ))}
      </div>

      <div className="hidden overflow-x-auto rounded-[var(--r-lg)] border md:block"
        style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
        <table className="w-full min-w-[720px] text-left text-[13px]">
          <thead><tr style={{ borderBottom: "1px solid var(--border)" }}>
            <th className="px-4 py-3 font-medium">Address</th>
            <th className="px-4 py-3 font-medium">Source</th>
            <th className="px-4 py-3 font-medium">Reason</th>
            <th className="px-4 py-3 font-medium">Added</th>
            <th className="px-4 py-3 font-medium text-right">Actions</th>
          </tr></thead>
          <tbody>
            {loading ? (
              <tr><td colSpan={5} className="px-4 py-8" style={{ color: "var(--fg-muted)" }}>Loading suppressions…</td></tr>
            ) : rows.length === 0 ? (
              <tr><td colSpan={5} className="px-4 py-8" style={{ color: "var(--fg-muted)" }}>
                No blocked recipients for this inbox. Unsubscribe requests land here automatically; manual
                blocks can be added above.
              </td></tr>
            ) : rows.map((row) => (
              <tr key={row.address} style={{ borderBottom: "1px solid var(--border-sub)" }}>
                <td className="px-4 py-3 font-mono text-[12px]" style={{ color: "var(--fg)" }}>{row.address}</td>
                <td className="px-4 py-3">{sourceChip(row.source)}</td>
                <td className="px-4 py-3 max-w-[320px] truncate" style={{ color: "var(--fg-muted)" }}
                  title={row.reason || undefined}>
                  {row.reason || "—"}
                </td>
                <td className="px-4 py-3" style={{ color: "var(--fg-muted)" }}>
                  {new Date(row.created_at).toLocaleDateString()}
                </td>
                <td className="px-4 py-3 text-right">
                  <Button variant="ghost" aria-label={`Remove ${row.address}`}
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
    </div>
  );
}

function AddSuppressionPanel({
  email,
  onCancel,
  onAdded,
}: {
  email: string;
  onCancel: () => void;
  onAdded: () => Promise<void>;
}) {
  const [address, setAddress] = useState("");
  const [reason, setReason] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  const save = async (event: React.FormEvent) => {
    event.preventDefault();
    setSaving(true);
    setError("");
    try {
      const response = await fetch(`/v1/agents/${encodeURIComponent(email)}/suppressions`, {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          address: address.trim(),
          ...(reason.trim() ? { reason: reason.trim() } : {}),
        }),
      });
      if (!response.ok) throw new Error(await apiError(response));
      await onAdded();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to add suppression");
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="mb-4 rounded-[var(--r-lg)] border p-4"
      style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
      <h3 className="mb-3 text-[14px] font-semibold" style={{ color: "var(--fg)" }}>
        Block a recipient for this inbox
      </h3>
      <form onSubmit={save} className="grid gap-3 sm:grid-cols-[1fr_1fr_auto] sm:items-end">
        <label className="text-[12px]" style={{ color: "var(--fg-muted)" }}>
          Address
          <input aria-label="Address" autoFocus required type="email" value={address}
            onChange={(event) => setAddress(event.target.value)}
            disabled={saving}
            placeholder="person@example.com"
            className="mt-1 w-full rounded-[var(--r-md)] border px-3 py-2 text-[13px] outline-none"
            style={{ borderColor: "var(--border)", background: "var(--bg)", color: "var(--fg)" }} />
        </label>
        <label className="text-[12px]" style={{ color: "var(--fg-muted)" }}>
          Reason <span style={{ color: "var(--fg-faint)" }}>(optional)</span>
          <input aria-label="Reason" value={reason}
            onChange={(event) => setReason(event.target.value)}
            disabled={saving}
            className="mt-1 w-full rounded-[var(--r-md)] border px-3 py-2 text-[13px] outline-none"
            style={{ borderColor: "var(--border)", background: "var(--bg)", color: "var(--fg)" }} />
        </label>
        <div className="flex gap-2">
          <Button type="button" variant="ghost" onClick={onCancel} disabled={saving}>Cancel</Button>
          <Button type="submit" disabled={saving || !address.trim()}>
            {saving ? "Adding…" : "Add suppression"}
          </Button>
        </div>
      </form>
      <p className="mt-2 text-[11px]" style={{ color: "var(--fg-muted)" }}>
        Blocks sends from this inbox only; other inboxes on the account can still email the address.
      </p>
      {error && <p role="alert" className="mt-2 text-[12px]" style={{ color: "var(--danger-strong)" }}>{error}</p>}
    </section>
  );
}
