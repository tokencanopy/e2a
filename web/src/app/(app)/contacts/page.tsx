"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { AgentPromptCard } from "../../components/AgentPromptCard";
import { Button } from "../../components/loft/Button";
import { Chip } from "../../components/loft/Chip";
import { PageShell } from "../../components/loft/PageShell";
import { useAgents } from "../../components/hooks/useAgents";
import {
  getCsvMetadataColumns,
  mapCsvRows,
  parseCsv,
  type ImportPreviewRow,
} from "./_lib/csv";
import { ViewTabs } from "./_lib/ViewTabs";

type Contact = {
  address: string;
  display_name: string;
  metadata: Record<string, unknown>;
  source: string;
  import_batch_id?: string;
  created_at: string;
  updated_at: string;
};

type ImportResult = {
  batch_id: string;
  created: number;
  updated: number;
  skipped: number;
  failed: number;
  results: Array<{
    index: number;
    address?: string;
    status: string;
    code?: string;
    message?: string;
    suppressed?: boolean;
    enrolled?: boolean;
  }>;
};

type ImportReversalResult = {
  deleted: boolean;
  batch_id: string;
  contacts_deleted: number;
  contacts_retained: number;
  engagements_deleted: number;
};

const fieldClass =
  "w-full rounded-[var(--r-md)] border px-3 py-2 text-[13px] outline-none focus:border-[var(--accent)]";
const fieldStyle = {
  background: "var(--bg)",
  borderColor: "var(--border)",
  color: "var(--fg)",
};

const metadataInlineLimit = 3;
const metadataInlineValueLimit = 80;

function formatMetadataValue(value: unknown): string {
  if (typeof value === "string") return value;
  if (value === null) return "null";
  if (["number", "boolean", "bigint", "undefined"].includes(typeof value)) {
    return String(value);
  }
  try {
    return JSON.stringify(value) ?? String(value);
  } catch {
    return "Unable to display value";
  }
}

function truncateMetadataValue(value: string): string {
  if (value.length <= metadataInlineValueLimit) return value;
  return `${value.slice(0, metadataInlineValueLimit - 1)}…`;
}

function MetadataFields({ metadata }: { metadata: Record<string, unknown> }) {
  const entries = Object.entries(metadata);
  if (entries.length === 0) return null;
  const previewEntries = entries.slice(0, metadataInlineLimit);

  return (
    <div className="mt-3 max-w-[420px] text-[11px]" style={{ color: "var(--fg-muted)" }}>
      <p className="font-semibold" style={{ color: "var(--fg)" }}>Metadata</p>
      <dl className="mt-1 space-y-1">
        {previewEntries.map(([key, value]) => (
          <div key={key} className="grid grid-cols-[minmax(0,1fr)_minmax(0,2fr)] gap-2">
            <dt className="break-words font-medium">{key}</dt>
            <dd className="break-all">{truncateMetadataValue(formatMetadataValue(value))}</dd>
          </div>
        ))}
      </dl>
      <details className="mt-2">
        <summary className="cursor-pointer font-medium" style={{ color: "var(--accent-strong)" }}>
          {entries.length > metadataInlineLimit
            ? `View all ${entries.length} metadata fields (${entries.length - metadataInlineLimit} more)`
            : `View full metadata (${entries.length} field${entries.length === 1 ? "" : "s"})`}
        </summary>
        <dl className="mt-2 max-h-64 space-y-2 overflow-auto rounded-[var(--r-md)] border p-3"
          style={{ borderColor: "var(--border)", background: "var(--bg)" }}>
          {entries.map(([key, value]) => (
            <div key={key}>
              <dt className="break-words font-medium" style={{ color: "var(--fg)" }}>{key}</dt>
              <dd className="mt-0.5 whitespace-pre-wrap break-all">{formatMetadataValue(value)}</dd>
            </div>
          ))}
        </dl>
      </details>
    </div>
  );
}

async function responseError(response: Response): Promise<string> {
  try {
    const body = await response.json();
    return body?.error?.message ?? `Request failed (HTTP ${response.status})`;
  } catch {
    return `Request failed (HTTP ${response.status})`;
  }
}

async function fetchContactsPage(cursor = "", source = "", signal?: AbortSignal): Promise<{
  items: Contact[];
  nextCursor: string;
}> {
  const params = new URLSearchParams({ limit: "100" });
  if (cursor) params.set("cursor", cursor);
  if (source) params.set("source", source);
  const response = await fetch(`/v1/contacts?${params}`, { credentials: "include", signal });
  if (!response.ok) throw new Error(await responseError(response));
  const body: { items: Contact[]; next_cursor?: string | null } = await response.json();
  return { items: body.items ?? [], nextCursor: body.next_cursor ?? "" };
}

export default function ContactsPage() {
  const { agents } = useAgents();
  const [contacts, setContacts] = useState<Contact[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [query, setQuery] = useState("");
  const [source, setSource] = useState("");
  const [showCreate, setShowCreate] = useState(false);
  const [showImport, setShowImport] = useState(false);
  const [editing, setEditing] = useState<Contact | null>(null);
  const [receipt, setReceipt] = useState<ImportResult | null>(null);
  const [nextCursor, setNextCursor] = useState("");
  const [loadingMore, setLoadingMore] = useState(false);

  const refresh = useCallback(async (signal?: AbortSignal) => {
    setLoading(true);
    try {
      const page = await fetchContactsPage("", source, signal);
      setContacts(page.items);
      setNextCursor(page.nextCursor);
      setError("");
    } catch (err) {
      if (!signal?.aborted) {
        setError(err instanceof Error ? err.message : "Failed to load contacts");
      }
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, [source]);

  useEffect(() => {
    const controller = new AbortController();
    void refresh(controller.signal);
    return () => controller.abort();
  }, [refresh]);

  const shown = useMemo(() => {
    const needle = query.trim().toLowerCase();
    return contacts.filter((contact) =>
      !needle ||
        contact.address.toLowerCase().includes(needle) ||
        contact.display_name.toLowerCase().includes(needle),
    );
  }, [contacts, query]);

  const loadMore = async () => {
    if (!nextCursor) return;
    setLoadingMore(true);
    try {
      const page = await fetchContactsPage(nextCursor, source);
      setContacts((current) => [...current, ...page.items]);
      setNextCursor(page.nextCursor);
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load more contacts");
    } finally {
      setLoadingMore(false);
    }
  };

  const deleteContact = async (contact: Contact) => {
    if (!confirm(`Delete ${contact.address}? Suppression and consent history will remain.`)) return;
    try {
      const response = await fetch(
        `/v1/contacts/${encodeURIComponent(contact.address)}?confirm=DELETE`,
        { method: "DELETE", credentials: "include" },
      );
      if (!response.ok) throw new Error(await responseError(response));
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to delete contact");
    }
  };

  return (
    <PageShell
      eyebrow="Workspace"
      title={<span className="inline-flex items-center gap-2.5">Contacts <Chip tone="warn">Beta</Chip></span>}
      subtitle="Account-wide contact identity and import history. Outreach stage and scheduling stay separate for each inbox."
      actions={
        <>
          <Button variant="ghost" onClick={() => setShowImport((value) => !value)}>Import CSV</Button>
          <Button onClick={() => setShowCreate((value) => !value)}>New contact</Button>
        </>
      }
    >
      <ViewTabs active="contacts" />
      <div className="space-y-6">
        <AgentPromptCard
          blurb="Your coding agent can import contacts, enroll them with an inbox, and manage outreach over MCP."
          prompt="Help me set up e2a contacts and outreach using https://api.e2a.dev/mcp"
        />

        {showCreate && <CreateContactPanel onCreated={async () => {
          setShowCreate(false);
          await refresh();
        }} />}

        {editing && (
          <EditContactPanel
            contact={editing}
            onCancel={() => setEditing(null)}
            onSaved={async () => {
              setEditing(null);
              await refresh();
            }}
          />
        )}

        {showImport && (
          <ImportPanel
            agents={agents.map((agent) => agent.email)}
            onImported={async (result) => {
              setReceipt(result);
              await refresh();
            }}
          />
        )}

        {receipt && (
          <ImportReceipt
            result={receipt}
            onDismiss={() => setReceipt(null)}
            onReversed={refresh}
          />
        )}

        <section aria-label="Contacts">
          <div className="mb-3 flex flex-col gap-2 sm:flex-row">
            <input
              className={fieldClass}
              style={fieldStyle}
              aria-label="Search contacts"
              placeholder="Search name or address"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
            />
            <select
              className={`${fieldClass} sm:max-w-[190px]`}
              style={fieldStyle}
              aria-label="Filter by source"
              value={source}
              onChange={(event) => setSource(event.target.value)}
            >
              <option value="">All sources</option>
              <option value="manual">Manual</option>
              <option value="import">Import</option>
              <option value="inbound">Inbound</option>
            </select>
          </div>
          {query && nextCursor && (
            <p className="mb-3 text-[11px]" style={{ color: "var(--fg-muted)" }}>
              Search covers the contacts loaded so far. Load more to include the next page.
            </p>
          )}

          {error && (
            <div role="alert" className="mb-3 rounded-[var(--r-md)] p-3 text-[13px]"
              style={{ background: "var(--danger-bg)", color: "var(--danger-strong)" }}>
              {error} <button className="underline" onClick={() => void refresh()}>Try again</button>
            </div>
          )}

          <div className="space-y-2 md:hidden">
            {loading ? (
              <div className="rounded-[var(--r-lg)] border p-4 text-[13px]"
                style={{ borderColor: "var(--border)", background: "var(--bg-panel)", color: "var(--fg-muted)" }}>
                Loading contacts…
              </div>
            ) : shown.length === 0 ? (
              <div className="rounded-[var(--r-lg)] border p-4 text-[13px]"
                style={{ borderColor: "var(--border)", background: "var(--bg-panel)", color: "var(--fg-muted)" }}>
                {contacts.length === 0 ? "No contacts yet. Create one or import a CSV." : "No contacts match these filters."}
              </div>
            ) : shown.map((contact) => (
              <article key={contact.address} className="rounded-[var(--r-lg)] border p-4"
                style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
                <div className="flex items-start justify-between gap-3">
                  <div className="min-w-0">
                    <h3 className="truncate text-[14px] font-semibold" style={{ color: "var(--fg)" }}>
                      {contact.display_name || contact.address}
                    </h3>
                    {contact.display_name && (
                      <p className="mt-0.5 truncate font-mono text-[11px]" style={{ color: "var(--fg-muted)" }}>
                        {contact.address}
                      </p>
                    )}
                  </div>
                  <Chip>{contact.source}</Chip>
                </div>
                <p className="mt-3 text-[11px]" style={{ color: "var(--fg-muted)" }}>
                  Added {new Date(contact.created_at).toLocaleDateString()}
                </p>
                <MetadataFields metadata={contact.metadata} />
                <div className="mt-3 grid grid-cols-2 gap-2">
                  <Button variant="ghost" aria-label={`Edit contact ${contact.address}`}
                    onClick={() => setEditing(contact)}>Edit</Button>
                  <Button variant="ghost" aria-label={`Delete contact ${contact.address}`}
                    onClick={() => void deleteContact(contact)}
                    style={{ color: "var(--danger-strong)" }}>Delete</Button>
                </div>
              </article>
            ))}
          </div>

          <div className="hidden overflow-x-auto rounded-[var(--r-lg)] border md:block"
            style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
            <table className="w-full min-w-[720px] text-left text-[13px]">
              <thead>
                <tr style={{ borderBottom: "1px solid var(--border)" }}>
                  <th className="px-4 py-3 font-medium">Contact</th>
                  <th className="px-4 py-3 font-medium">Source</th>
                  <th className="px-4 py-3 font-medium">Added</th>
                  <th className="px-4 py-3 font-medium text-right">Actions</th>
                </tr>
              </thead>
              <tbody>
                {loading ? (
                  <tr><td className="px-4 py-7" colSpan={4} style={{ color: "var(--fg-muted)" }}>Loading contacts…</td></tr>
                ) : shown.length === 0 ? (
                  <tr><td className="px-4 py-7" colSpan={4} style={{ color: "var(--fg-muted)" }}>
                    {contacts.length === 0 ? "No contacts yet. Create one or import a CSV." : "No contacts match these filters."}
                  </td></tr>
                ) : shown.map((contact) => (
                  <tr key={contact.address} style={{ borderBottom: "1px solid var(--border-sub)" }}>
                    <td className="px-4 py-3">
                      <div className="font-medium" style={{ color: "var(--fg)" }}>
                        {contact.display_name || contact.address}
                      </div>
                      {contact.display_name && <div className="mt-0.5 font-mono text-[11px]" style={{ color: "var(--fg-muted)" }}>{contact.address}</div>}
                      <MetadataFields metadata={contact.metadata} />
                    </td>
                    <td className="px-4 py-3"><Chip>{contact.source}</Chip></td>
                    <td className="px-4 py-3" style={{ color: "var(--fg-muted)" }}>
                      {new Date(contact.created_at).toLocaleDateString()}
                    </td>
                    <td className="px-4 py-3 text-right">
                      <Button variant="ghost" aria-label={`Edit ${contact.address}`}
                        onClick={() => setEditing(contact)}>Edit</Button>{" "}
                      <Button variant="ghost" aria-label={`Delete ${contact.address}`}
                        onClick={() => void deleteContact(contact)}
                        style={{ color: "var(--danger-strong)" }}>Delete</Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {nextCursor && (
            <div className="mt-3 flex justify-center">
              <Button variant="ghost" disabled={loadingMore} onClick={() => void loadMore()}>
                {loadingMore ? "Loading…" : "Load more contacts"}
              </Button>
            </div>
          )}
        </section>
      </div>
    </PageShell>
  );
}

function Panel({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="rounded-[var(--r-lg)] border p-5"
      style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
      <h2 className="mb-4 text-[16px] font-semibold" style={{ color: "var(--fg)" }}>{title}</h2>
      {children}
    </section>
  );
}

function CreateContactPanel({ onCreated }: { onCreated: () => Promise<void> }) {
  const [address, setAddress] = useState("");
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [idempotencyKey, setIdempotencyKey] = useState(() => crypto.randomUUID());
  return (
    <Panel title="New contact">
      <form className="grid gap-3 sm:grid-cols-[1fr_1fr_auto]" onSubmit={async (event) => {
        event.preventDefault();
        setSaving(true);
        setError("");
        try {
          const response = await fetch("/v1/contacts", {
            method: "POST",
            credentials: "include",
            headers: {
              "Content-Type": "application/json",
              "Idempotency-Key": idempotencyKey,
            },
            body: JSON.stringify({ address, display_name: name }),
          });
          if (!response.ok) throw new Error(await responseError(response));
          setIdempotencyKey(crypto.randomUUID());
          await onCreated();
        } catch (err) {
          setError(err instanceof Error ? err.message : "Failed to create contact");
        } finally {
          setSaving(false);
        }
      }}>
        <input required type="email" aria-label="Email address" placeholder="partner@example.com"
          className={fieldClass} style={fieldStyle} value={address} onChange={(event) => setAddress(event.target.value)} />
        <input aria-label="Display name" placeholder="Display name (optional)"
          className={fieldClass} style={fieldStyle} value={name} onChange={(event) => setName(event.target.value)} />
        <Button type="submit" disabled={saving}>{saving ? "Saving…" : "Create"}</Button>
      </form>
      {error && <p role="alert" className="mt-2 text-[12px]" style={{ color: "var(--danger-strong)" }}>{error}</p>}
    </Panel>
  );
}

function EditContactPanel({
  contact,
  onCancel,
  onSaved,
}: {
  contact: Contact;
  onCancel: () => void;
  onSaved: () => Promise<void>;
}) {
  const [name, setName] = useState(contact.display_name);
  const [metadata, setMetadata] = useState(contact.metadata);
  const [etag, setETag] = useState("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    const controller = new AbortController();
    setName(contact.display_name);
    setMetadata(contact.metadata);
    setETag("");
    setLoading(true);
    setError("");
    void (async () => {
      try {
        const response = await fetch(`/v1/contacts/${encodeURIComponent(contact.address)}`, {
          credentials: "include",
          signal: controller.signal,
        });
        if (!response.ok) throw new Error(await responseError(response));
        const freshETag = response.headers.get("ETag");
        if (!freshETag) {
          throw new Error("The latest contact version is unavailable. Close this editor and try again.");
        }
        const current: Contact = await response.json();
        setName(current.display_name);
        setMetadata(current.metadata ?? {});
        setETag(freshETag);
      } catch (err) {
        if (!controller.signal.aborted) {
          setError(err instanceof Error ? err.message : "Failed to load the contact");
        }
      } finally {
        if (!controller.signal.aborted) setLoading(false);
      }
    })();
    return () => controller.abort();
  }, [contact.address, contact.display_name, contact.metadata]);

  const save = async (event: React.FormEvent) => {
    event.preventDefault();
    if (!etag) {
      setError("Load the latest contact version before saving.");
      return;
    }
    setSaving(true);
    setError("");
    try {
      const response = await fetch(`/v1/contacts/${encodeURIComponent(contact.address)}`, {
        method: "PATCH",
        credentials: "include",
        headers: { "Content-Type": "application/json", "If-Match": etag },
        body: JSON.stringify({ display_name: name }),
      });
      if (!response.ok) {
        if (response.status === 412) {
          throw new Error("This contact changed in another session. Close this editor, review the latest value, and try again.");
        }
        throw new Error(await responseError(response));
      }
      await onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to save contact");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Panel title={`Edit ${contact.address}`}>
      <form onSubmit={save} className="grid gap-3 sm:grid-cols-[1fr_auto] sm:items-end">
        <label className="text-[12px]" style={{ color: "var(--fg-muted)" }}>
          Display name
          <input
            aria-label="Display name"
            autoFocus
            className={`${fieldClass} mt-1`}
            style={fieldStyle}
            value={name}
            onChange={(event) => setName(event.target.value)}
            disabled={loading || saving || !etag}
          />
        </label>
        <div className="flex gap-2">
          <Button type="button" variant="ghost" onClick={onCancel} disabled={saving}>Cancel</Button>
          <Button type="submit" disabled={loading || saving || !etag}>
            {saving ? "Saving…" : "Save contact"}
          </Button>
        </div>
      </form>
      {Object.keys(metadata).length > 0 && (
        <div className="mt-4 border-t pt-4" style={{ borderColor: "var(--border)" }}>
          <p className="text-[12px]" style={{ color: "var(--fg-muted)" }}>
            Metadata is read-only here. Manage it through CSV import or the API.
          </p>
          <MetadataFields metadata={metadata} />
        </div>
      )}
      {error && <p role="alert" className="mt-3 text-[12px]" style={{ color: "var(--danger-strong)" }}>{error}</p>}
    </Panel>
  );
}

function ImportPanel({ agents, onImported }: {
  agents: string[];
  onImported: (result: ImportResult) => Promise<void>;
}) {
  const [table, setTable] = useState<string[][]>([]);
  const [fileName, setFileName] = useState("");
  const [emailColumn, setEmailColumn] = useState("");
  const [nameColumn, setNameColumn] = useState("");
  const [agent, setAgent] = useState("");
  const [stage, setStage] = useState("");
  const [rows, setRows] = useState<ImportPreviewRow[]>([]);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [idempotencyKey, setIdempotencyKey] = useState(() => crypto.randomUUID());
  const headers = (table[0] ?? []).map((header) => header.trim());
  const metadataColumns = useMemo(() => {
    if (!emailColumn || table.length < 2) return [];
    try {
      return getCsvMetadataColumns(table, emailColumn, nameColumn || undefined);
    } catch {
      return [];
    }
  }, [emailColumn, nameColumn, table]);
  const invalidateKey = () => setIdempotencyKey(crypto.randomUUID());

  const preview = (nextEmail = emailColumn, nextName = nameColumn) => {
    if (!nextEmail) return;
    try {
      setRows(mapCsvRows(table, nextEmail, nextName || undefined));
      setError("");
    } catch (err) {
      setRows([]);
      setError(err instanceof Error ? err.message : "Could not map CSV");
    }
  };

  return (
    <Panel title="Import CSV">
      <p className="mb-4 text-[13px] leading-[1.6]" style={{ color: "var(--fg-muted)" }}>
        Preview and map the file here. Import records contacts only; selecting an inbox also enrolls valid rows but never sends mail.
      </p>
      <div className="grid gap-3 md:grid-cols-2">
        <label className="text-[12px]" style={{ color: "var(--fg-muted)" }}>
          CSV file
          <input className={`${fieldClass} mt-1`} style={fieldStyle} type="file" accept=".csv,text/csv"
            onChange={async (event) => {
              const file = event.target.files?.[0];
              if (!file) return;
              invalidateKey();
              try {
                const parsed = parseCsv(await file.text());
                setTable(parsed);
                setFileName(file.name);
                const trimmedHeaders = parsed[0]?.map((header) => header.trim()) ?? [];
                const normalized = trimmedHeaders.map((header) => header.toLowerCase());
                const email = trimmedHeaders[normalized.findIndex((header) => ["email", "address"].includes(header))] ?? trimmedHeaders[0] ?? "";
                const name = trimmedHeaders[normalized.findIndex((header) => ["name", "display name", "full name"].includes(header))] ?? "";
                setEmailColumn(email);
                setNameColumn(name);
                setRows(mapCsvRows(parsed, email, name || undefined));
                setError("");
              } catch (err) {
                setError(err instanceof Error ? err.message : "Could not read CSV");
              }
            }} />
        </label>
        <div className="grid grid-cols-2 gap-2">
          <label className="text-[12px]" style={{ color: "var(--fg-muted)" }}>Email column
            <select className={`${fieldClass} mt-1`} style={fieldStyle} value={emailColumn}
              onChange={(event) => { invalidateKey(); setEmailColumn(event.target.value); preview(event.target.value, nameColumn); }}>
              {headers.map((header) => <option key={header}>{header}</option>)}
            </select>
          </label>
          <label className="text-[12px]" style={{ color: "var(--fg-muted)" }}>Name column
            <select className={`${fieldClass} mt-1`} style={fieldStyle} value={nameColumn}
              onChange={(event) => { invalidateKey(); setNameColumn(event.target.value); preview(emailColumn, event.target.value); }}>
              <option value="">None</option>
              {headers.map((header) => <option key={header}>{header}</option>)}
            </select>
          </label>
        </div>
        <label className="text-[12px]" style={{ color: "var(--fg-muted)" }}>Enroll with inbox (optional)
          <select className={`${fieldClass} mt-1`} style={fieldStyle} value={agent}
            onChange={(event) => { invalidateKey(); setAgent(event.target.value); }}>
            <option value="">Do not enroll</option>
            {agents.map((email) => <option key={email}>{email}</option>)}
          </select>
        </label>
        <label className="text-[12px]" style={{ color: "var(--fg-muted)" }}>Initial stage (opaque)
          <input disabled={!agent} className={`${fieldClass} mt-1`} style={fieldStyle}
            value={stage} onChange={(event) => { invalidateKey(); setStage(event.target.value); }} placeholder="prospect" />
        </label>
      </div>
      {fileName && <p className="mt-3 text-[12px]" style={{ color: "var(--fg-muted)" }}>
        {fileName}: {rows.length} row{rows.length === 1 ? "" : "s"} ready. Preview: {rows.slice(0, 3).map((row) => row.address || "(blank)").join(", ")}
      </p>}
      {fileName && (
        <section aria-label="CSV metadata mapping" className="mt-3 rounded-[var(--r-lg)] border p-3"
          style={{ borderColor: "var(--border)", background: "var(--bg)" }}>
          <h3 className="text-[12px] font-semibold" style={{ color: "var(--fg)" }}>Metadata mapping</h3>
          {metadataColumns.length === 0 ? (
            <p className="mt-1 text-[12px]" style={{ color: "var(--fg-muted)" }}>
              No columns will be stored as metadata.
            </p>
          ) : (
            <>
              <p className="mt-1 text-[12px]" style={{ color: "var(--fg-muted)" }}>
                These columns will be stored as metadata. Sample values are from the first row.
              </p>
              <dl className="mt-2 grid gap-2 sm:grid-cols-2">
                {metadataColumns.map((column) => (
                  <div key={column.name} className="min-w-0 rounded-[var(--r-md)] border px-3 py-2"
                    style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
                    <dt className="break-words text-[11px] font-semibold" style={{ color: "var(--fg)" }}>{column.name}</dt>
                    <dd className="mt-0.5 truncate text-[12px]" style={{ color: "var(--fg-muted)" }}>
                      {column.sampleValue || "(empty)"}
                    </dd>
                  </div>
                ))}
              </dl>
            </>
          )}
        </section>
      )}
      {error && <p role="alert" className="mt-3 text-[12px]" style={{ color: "var(--danger-strong)" }}>{error}</p>}
      <div className="mt-4"><Button disabled={saving || rows.length === 0} onClick={async () => {
        setSaving(true);
        setError("");
        try {
          const response = await fetch("/v1/contacts/import", {
            method: "POST",
            credentials: "include",
            headers: {
              "Content-Type": "application/json",
              "Idempotency-Key": idempotencyKey,
            },
            body: JSON.stringify({
              contacts: rows,
              on_conflict: "merge",
              ...(agent ? { agent_email: agent, stage } : {}),
            }),
          });
          if (!response.ok) throw new Error(await responseError(response));
          await onImported(await response.json());
          setIdempotencyKey(crypto.randomUUID());
        } catch (err) {
          setError(err instanceof Error ? err.message : "Failed to import contacts");
        } finally {
          setSaving(false);
        }
      }}>{saving ? "Importing…" : `Import ${rows.length || ""} contact${rows.length === 1 ? "" : "s"}`}</Button></div>
    </Panel>
  );
}

function ImportReceipt({
  result,
  onDismiss,
  onReversed,
}: {
  result: ImportResult;
  onDismiss: () => void;
  onReversed: () => Promise<void>;
}) {
  const exceptions = result.results.filter((item) => item.status === "failed" || item.status === "skipped" || item.suppressed);
  const [reversing, setReversing] = useState(false);
  const [reversal, setReversal] = useState<ImportReversalResult | null>(null);
  const [error, setError] = useState("");

  const reverseImport = async () => {
    if (!confirm("Reverse this import? Untouched contacts and inbox enrolments created by this batch will be removed. Existing outreach, correspondence history, and suppressions remain.")) return;
    setReversing(true);
    setError("");
    try {
      const response = await fetch(
        `/v1/contacts/imports/${encodeURIComponent(result.batch_id)}?confirm=DELETE`,
        { method: "DELETE", credentials: "include" },
      );
      if (!response.ok) throw new Error(await responseError(response));
      setReversal(await response.json());
      await onReversed();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to reverse import");
    } finally {
      setReversing(false);
    }
  };

  return (
    <section role="status" className="rounded-[var(--r-lg)] border p-5"
      style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}>
      <div className="flex items-start justify-between gap-3">
        <div>
          <h2 className="text-[16px] font-semibold">Import complete</h2>
          <p className="mt-1 text-[13px]" style={{ color: "var(--fg-muted)" }}>
            {reversal
              ? `${reversal.contacts_deleted} contacts and ${reversal.engagements_deleted ?? 0} enrolments removed · ${reversal.contacts_retained} contacts retained because they have correspondence history`
              : `${result.created} created · ${result.updated} updated · ${result.skipped} skipped · ${result.failed} failed`}
          </p>
          <code className="mt-2 block text-[11px]" style={{ color: "var(--fg-subtle)" }}>{result.batch_id}</code>
        </div>
        <div className="flex flex-wrap justify-end gap-2">
          {!reversal && (
            <Button variant="ghost" disabled={reversing} onClick={() => void reverseImport()}>
              {reversing ? "Reversing…" : "Reverse import"}
            </Button>
          )}
          <Button variant="ghost" onClick={onDismiss}>Dismiss</Button>
        </div>
      </div>
      {error && <p role="alert" className="mt-3 text-[12px]" style={{ color: "var(--danger-strong)" }}>{error}</p>}
      {!reversal && exceptions.length > 0 && (
        <ul className="mt-4 space-y-1 text-[12px]">
          {exceptions.map((item) => <li key={item.index}>
            Row {item.index + 1}: {item.address || "invalid address"} — {item.suppressed ? "suppressed" : item.message || item.code || item.status}
          </li>)}
        </ul>
      )}
    </section>
  );
}
