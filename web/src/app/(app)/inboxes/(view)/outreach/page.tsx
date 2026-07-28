"use client";

import { Suspense, useCallback, useEffect, useState } from "react";
import { useSearchParams } from "next/navigation";
import { Button } from "../../../../components/loft/Button";
import { Chip } from "../../../../components/loft/Chip";

type Outreach = {
  address: string;
  agent_email: string;
  stage: string;
  next_action_at: string | null;
  replied: boolean;
  suppressed: boolean;
  suppression_reason?: string;
  last_outbound_at?: string | null;
  last_inbound_at?: string | null;
  outbound_count: number;
  inbound_count: number;
  contact: { display_name: string; metadata: Record<string, unknown> };
};

async function apiError(response: Response): Promise<string> {
  try {
    const body = await response.json();
    return body?.error?.message ?? `Request failed (HTTP ${response.status})`;
  } catch {
    return `Request failed (HTTP ${response.status})`;
  }
}

export default function OutreachPage() {
  return <Suspense fallback={null}><OutreachRouter /></Suspense>;
}

function OutreachRouter() {
  const email = useSearchParams().get("email") ?? "";
  return <OutreachContent key={email} email={email} />;
}

function OutreachContent({ email }: { email: string }) {
  const [rows, setRows] = useState<Outreach[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [stageDraft, setStageDraft] = useState("");
  const [stage, setStage] = useState("");
  const [dueOnly, setDueOnly] = useState(false);
  const [unrepliedOnly, setUnrepliedOnly] = useState(true);
  const [mailableOnly, setMailableOnly] = useState(true);
  const [editing, setEditing] = useState<Outreach | null>(null);
  const [nextCursor, setNextCursor] = useState("");
  const [loadingMore, setLoadingMore] = useState(false);

  const refresh = useCallback(async (cursor = "", append = false, signal?: AbortSignal) => {
    if (!email) return;
    if (append) setLoadingMore(true);
    else setLoading(true);
    try {
      const params = new URLSearchParams({ limit: "100" });
      if (cursor) params.set("cursor", cursor);
      if (stage) params.set("stage", stage);
      if (unrepliedOnly) params.set("replied", "false");
      if (mailableOnly) params.set("suppressed", "false");
      if (dueOnly) params.set("next_action_before", new Date().toISOString());
      const response = await fetch(
        `/v1/agents/${encodeURIComponent(email)}/contacts?${params}`,
        { credentials: "include", signal },
      );
      if (!response.ok) throw new Error(await apiError(response));
      const body: { items: Outreach[]; next_cursor?: string | null } = await response.json();
      setRows((current) => append ? [...current, ...(body.items ?? [])] : (body.items ?? []));
      setNextCursor(body.next_cursor ?? "");
      setError("");
    } catch (err) {
      if (!signal?.aborted) {
        setError(err instanceof Error ? err.message : "Failed to load outreach");
      }
    } finally {
      if (!signal?.aborted) {
        setLoading(false);
        setLoadingMore(false);
      }
    }
  }, [dueOnly, email, mailableOnly, stage, unrepliedOnly]);

  useEffect(() => {
    const timer = setTimeout(() => setStage(stageDraft), 300);
    return () => clearTimeout(timer);
  }, [stageDraft]);

  useEffect(() => {
    const controller = new AbortController();
    void refresh("", false, controller.signal);
    return () => controller.abort();
  }, [refresh]);

  const unenroll = async (row: Outreach) => {
    if (!confirm(`Remove ${row.address} from this inbox's outreach? Contact identity and suppression remain.`)) return;
    try {
      const response = await fetch(
        `/v1/agents/${encodeURIComponent(email)}/contacts/${encodeURIComponent(row.address)}?confirm=DELETE`,
        { method: "DELETE", credentials: "include" },
      );
      if (!response.ok) throw new Error(await apiError(response));
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to remove outreach");
    }
  };

  return (
    <div className="mx-auto w-full px-6 py-7 md:px-8" style={{ maxWidth: 1080 }}>
      <div className="mb-5 flex flex-col gap-3 md:flex-row md:items-end md:justify-between">
        <div>
          <div className="flex items-center gap-2">
            <h2 className="text-[20px] font-semibold" style={{ color: "var(--fg)" }}>Outreach</h2>
            <Chip tone="warn">Beta</Chip>
          </div>
          <p className="mt-1 max-w-[680px] text-[13px] leading-[1.6]" style={{ color: "var(--fg-muted)" }}>
            State and real reply history for this inbox. A due next action emits contact.due to configured webhooks for a deployed agent; it does not launch local coding agents or send email.
          </p>
        </div>
        <Button variant="ghost" onClick={() => void refresh()}>Refresh</Button>
      </div>

      <div className="mb-4 grid gap-2 rounded-[var(--r-lg)] border p-3 sm:grid-cols-[1fr_auto_auto_auto]"
        style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
        <input
          aria-label="Filter by stage"
          placeholder="Exact stage (all when blank)"
          value={stageDraft}
          onChange={(event) => setStageDraft(event.target.value)}
          className="rounded-[var(--r-md)] border px-3 py-2 text-[13px] outline-none"
          style={{ borderColor: "var(--border)", background: "var(--bg)", color: "var(--fg)" }}
        />
        <FilterToggle label="Due now" checked={dueOnly} onChange={setDueOnly} />
        <FilterToggle label="Unreplied" checked={unrepliedOnly} onChange={setUnrepliedOnly} />
        <FilterToggle label="Mailable" checked={mailableOnly} onChange={setMailableOnly} />
      </div>

      {editing && (
        <EditOutreachPanel
          email={email}
          row={editing}
          onCancel={() => setEditing(null)}
          onSaved={async () => {
            setEditing(null);
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
            Loading outreach…
          </div>
        ) : rows.length === 0 ? (
          <div className="rounded-[var(--r-lg)] border p-4 text-[13px]"
            style={{ borderColor: "var(--border)", background: "var(--bg-panel)", color: "var(--fg-muted)" }}>
            No outreach contacts match. Import contacts with this inbox or enroll one over the API/MCP.
          </div>
        ) : rows.map((row) => (
          <article key={row.address} className="rounded-[var(--r-lg)] border p-4"
            style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0">
                <h3 className="truncate text-[14px] font-semibold" style={{ color: "var(--fg)" }}>
                  {row.contact.display_name || row.address}
                </h3>
                <p className="mt-0.5 truncate font-mono text-[11px]" style={{ color: "var(--fg-muted)" }}>{row.address}</p>
              </div>
              <Chip tone="neutral">{row.stage || "No stage"}</Chip>
            </div>
            <div className="mt-3 flex flex-wrap gap-2">
              <Chip tone={row.replied ? "success" : "neutral"}>{row.replied ? "Replied" : "No reply"}</Chip>
              {row.suppressed && <Chip tone="danger">Suppressed{row.suppression_reason ? ` · ${row.suppression_reason}` : ""}</Chip>}
            </div>
            <dl className="mt-3 grid grid-cols-2 gap-3 text-[11px]">
              <div>
                <dt style={{ color: "var(--fg-muted)" }}>Next action</dt>
                <dd className="mt-0.5" style={{ color: "var(--fg)" }}>
                  {row.next_action_at ? new Date(row.next_action_at).toLocaleString() : "Not scheduled"}
                </dd>
              </div>
              <div>
                <dt style={{ color: "var(--fg-muted)" }}>Activity</dt>
                <dd className="mt-0.5" style={{ color: "var(--fg)" }}>
                  {row.outbound_count} sent · {row.inbound_count} received
                </dd>
              </div>
            </dl>
            <div className="mt-4 grid grid-cols-2 gap-2">
              <Button variant="ghost" aria-label={`Edit outreach ${row.address}`}
                onClick={() => setEditing(row)}>Edit</Button>
              <Button variant="ghost" aria-label={`Remove outreach ${row.address}`}
                style={{ color: "var(--danger-strong)" }} onClick={() => void unenroll(row)}>Remove</Button>
            </div>
          </article>
        ))}
      </div>

      <div className="hidden overflow-x-auto rounded-[var(--r-lg)] border md:block"
        style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
        <table className="w-full min-w-[820px] text-left text-[13px]">
          <thead><tr style={{ borderBottom: "1px solid var(--border)" }}>
            <th className="px-4 py-3 font-medium">Contact</th>
            <th className="px-4 py-3 font-medium">Stage</th>
            <th className="px-4 py-3 font-medium">Next action</th>
            <th className="px-4 py-3 font-medium">Activity</th>
            <th className="px-4 py-3 font-medium text-right">Actions</th>
          </tr></thead>
          <tbody>
            {loading ? (
              <tr><td colSpan={5} className="px-4 py-8" style={{ color: "var(--fg-muted)" }}>Loading outreach…</td></tr>
            ) : rows.length === 0 ? (
              <tr><td colSpan={5} className="px-4 py-8" style={{ color: "var(--fg-muted)" }}>
                No outreach contacts match. Import contacts with this inbox or enroll one over the API/MCP.
              </td></tr>
            ) : rows.map((row) => (
              <tr key={row.address} style={{ borderBottom: "1px solid var(--border-sub)" }}>
                <td className="px-4 py-3">
                  <div className="font-medium" style={{ color: "var(--fg)" }}>{row.contact.display_name || row.address}</div>
                  <div className="mt-0.5 font-mono text-[11px]" style={{ color: "var(--fg-muted)" }}>{row.address}</div>
                  {row.suppressed && <div className="mt-1"><Chip tone="danger">Suppressed{row.suppression_reason ? ` · ${row.suppression_reason}` : ""}</Chip></div>}
                </td>
                <td className="px-4 py-3"><Chip tone="neutral">{row.stage || "No stage"}</Chip></td>
                <td className="px-4 py-3" style={{ color: "var(--fg-muted)" }}>
                  {row.next_action_at ? new Date(row.next_action_at).toLocaleString() : "Not scheduled"}
                </td>
                <td className="px-4 py-3">
                  <Chip tone={row.replied ? "success" : "neutral"}>{row.replied ? "Replied" : "No reply"}</Chip>
                  <div className="mt-1 text-[11px]" style={{ color: "var(--fg-muted)" }}>
                    {row.outbound_count} sent · {row.inbound_count} received
                  </div>
                </td>
                <td className="px-4 py-3 text-right">
                  <Button variant="ghost" aria-label={`Edit ${row.address}`}
                    onClick={() => setEditing(row)}>Edit</Button>{" "}
                  <Button variant="ghost" aria-label={`Remove ${row.address}`}
                    style={{ color: "var(--danger-strong)" }} onClick={() => void unenroll(row)}>Remove</Button>
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
            {loadingMore ? "Loading…" : "Load more outreach"}
          </Button>
        </div>
      )}
    </div>
  );
}

function toLocalDateTime(value: string | null): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 16);
}

function EditOutreachPanel({
  email,
  row,
  onCancel,
  onSaved,
}: {
  email: string;
  row: Outreach;
  onCancel: () => void;
  onSaved: () => Promise<void>;
}) {
  const [stage, setStage] = useState(row.stage);
  const [nextAction, setNextAction] = useState(toLocalDateTime(row.next_action_at));
  const [etag, setETag] = useState("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const path = `/v1/agents/${encodeURIComponent(email)}/contacts/${encodeURIComponent(row.address)}`;

  useEffect(() => {
    const controller = new AbortController();
    setStage(row.stage);
    setNextAction(toLocalDateTime(row.next_action_at));
    setETag("");
    setLoading(true);
    setError("");
    void (async () => {
      try {
        const response = await fetch(path, { credentials: "include", signal: controller.signal });
        if (!response.ok) throw new Error(await apiError(response));
        const freshETag = response.headers.get("ETag");
        if (!freshETag) {
          throw new Error("The latest outreach version is unavailable. Close this editor and try again.");
        }
        const current: Outreach = await response.json();
        setStage(current.stage);
        setNextAction(toLocalDateTime(current.next_action_at));
        setETag(freshETag);
      } catch (err) {
        if (!controller.signal.aborted) {
          setError(err instanceof Error ? err.message : "Failed to load outreach state");
        }
      } finally {
        if (!controller.signal.aborted) setLoading(false);
      }
    })();
    return () => controller.abort();
  }, [path, row.next_action_at, row.stage]);

  const save = async (event: React.FormEvent) => {
    event.preventDefault();
    if (!etag) {
      setError("Load the latest outreach version before saving.");
      return;
    }
    const parsed = nextAction ? new Date(nextAction) : null;
    if (nextAction && Number.isNaN(parsed!.getTime())) {
      setError("Next action must be a valid local date and time");
      return;
    }
    setSaving(true);
    setError("");
    try {
      const response = await fetch(path, {
        method: "PUT",
        credentials: "include",
        headers: { "Content-Type": "application/json", "If-Match": etag },
        body: JSON.stringify({
          stage,
          next_action_at: parsed ? parsed.toISOString() : null,
        }),
      });
      if (!response.ok) {
        if (response.status === 412) {
          throw new Error("This outreach record changed in another session. Close this editor, review the latest state, and try again.");
        }
        throw new Error(await apiError(response));
      }
      await onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to save outreach");
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="mb-4 rounded-[var(--r-lg)] border p-4"
      style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
      <div className="mb-3">
        <h3 className="text-[14px] font-semibold" style={{ color: "var(--fg)" }}>
          Edit {row.contact.display_name || row.address}
        </h3>
        <p className="mt-0.5 font-mono text-[11px]" style={{ color: "var(--fg-muted)" }}>{row.address}</p>
      </div>
      <form onSubmit={save} className="grid gap-3 sm:grid-cols-[1fr_1fr_auto] sm:items-end">
        <label className="text-[12px]" style={{ color: "var(--fg-muted)" }}>
          Stage
          <input aria-label="Stage" autoFocus value={stage}
            onChange={(event) => setStage(event.target.value)}
            disabled={loading || saving || !etag}
            className="mt-1 w-full rounded-[var(--r-md)] border px-3 py-2 text-[13px] outline-none"
            style={{ borderColor: "var(--border)", background: "var(--bg)", color: "var(--fg)" }} />
        </label>
        <label className="text-[12px]" style={{ color: "var(--fg-muted)" }}>
          Next action <span style={{ color: "var(--fg-faint)" }}>(local time)</span>
          <input aria-label="Next action" type="datetime-local" value={nextAction}
            onChange={(event) => setNextAction(event.target.value)}
            disabled={loading || saving || !etag}
            className="mt-1 w-full rounded-[var(--r-md)] border px-3 py-2 text-[13px] outline-none"
            style={{ borderColor: "var(--border)", background: "var(--bg)", color: "var(--fg)" }} />
        </label>
        <div className="flex gap-2">
          <Button type="button" variant="ghost" onClick={onCancel} disabled={saving}>Cancel</Button>
          <Button type="submit" disabled={loading || saving || !etag}>
            {saving ? "Saving…" : "Save outreach"}
          </Button>
        </div>
      </form>
      <p className="mt-2 text-[11px]" style={{ color: "var(--fg-muted)" }}>
        Clear next action to remove the schedule. A due action emits contact.due to configured webhooks; it does not launch local coding agents or send email.
      </p>
      {error && <p role="alert" className="mt-2 text-[12px]" style={{ color: "var(--danger-strong)" }}>{error}</p>}
    </section>
  );
}

function FilterToggle({ label, checked, onChange }: {
  label: string;
  checked: boolean;
  onChange: (value: boolean) => void;
}) {
  return (
    <label className="flex cursor-pointer items-center gap-2 rounded-[var(--r-md)] px-2.5 text-[12px]"
      style={{ color: "var(--fg-muted)" }}>
      <input type="checkbox" checked={checked} onChange={(event) => onChange(event.target.checked)} />
      {label}
    </label>
  );
}
