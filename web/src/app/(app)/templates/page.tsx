"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { PageShell } from "../../components/loft/PageShell";
import { Chip } from "@e2a/ui";
import { StarterGallery } from "./_components/StarterGallery";
import { AgentPromptCard, AGENT_PROMPTS } from "../../components/AgentPromptCard";
import { StarterPreviewModal } from "./_components/StarterPreviewModal";
import type { StarterTemplateView, TemplateSummaryView } from "./_lib/types";

// /templates (beta) — the account's reusable email templates plus the
// read-only starter catalog. Templates are subject/body sources with
// {{variable}} interpolation, rendered server-side at send time via
// template_alias/template_id + template_data on POST /v1/send.

// Inline-code chip used in the subtitle copy (same treatment as the
// webhooks page header).
const inlineCodeStyle: React.CSSProperties = {
  fontFamily: "var(--f-mono)",
  fontSize: 12,
  padding: "1px 6px",
  background: "var(--bg-elev)",
  border: "1px solid var(--border-sub)",
  borderRadius: "var(--r-sm)",
  color: "var(--fg)",
};

// Follow next_cursor so accounts with more than one page of templates
// still see the full list. Cap the walk defensively.
async function fetchAllTemplates(): Promise<TemplateSummaryView[]> {
  const items: TemplateSummaryView[] = [];
  let cursor: string | null = null;
  for (let i = 0; i < 20; i++) {
    const url: string = cursor
      ? `/v1/templates?cursor=${encodeURIComponent(cursor)}`
      : "/v1/templates";
    const res = await fetch(url, { credentials: "include" });
    if (!res.ok) throw new Error(`Failed to load templates (HTTP ${res.status})`);
    const body: { items: TemplateSummaryView[]; next_cursor: string | null } =
      await res.json();
    items.push(...(body.items ?? []));
    cursor = body.next_cursor;
    if (!cursor) break;
  }
  return items;
}

export default function TemplatesPage() {
  const [templates, setTemplates] = useState<TemplateSummaryView[]>([]);
  const [starters, setStarters] = useState<StarterTemplateView[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [startersError, setStartersError] = useState("");
  const [previewAlias, setPreviewAlias] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      setTemplates(await fetchAllTemplates());
      setLoadError("");
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    (async () => {
      setLoading(true);
      await Promise.all([
        refresh(),
        (async () => {
          try {
            const res = await fetch("/v1/starter-templates", {
              credentials: "include",
            });
            if (!res.ok) {
              setStartersError(`Failed to load starters (HTTP ${res.status})`);
              return;
            }
            const body: { items: StarterTemplateView[] } = await res.json();
            setStarters(body.items ?? []);
          } catch (err) {
            setStartersError(
              err instanceof Error ? err.message : String(err),
            );
          }
        })(),
      ]);
      setLoading(false);
    })();
  }, [refresh]);

  const hasTemplates = templates.length > 0;

  const gallery = (
    <section aria-label="Starter templates">
      <div className="flex items-center gap-2 mb-1">
        <h2
          className="text-[16px] font-semibold"
          style={{ color: "var(--fg)", margin: 0 }}
        >
          Start from a starter
        </h2>
      </div>
      <p
        className="text-[13px] mb-4 leading-[1.6]"
        style={{ color: "var(--fg-muted)", maxWidth: 640 }}
      >
        Pre-built, ISP-friendly emails maintained with the platform. Using
        one copies the master verbatim into your library — edit it freely
        afterwards.
      </p>
      {startersError ? (
        <p className="text-[13px]" style={{ color: "var(--danger-strong)" }}>
          {startersError}
        </p>
      ) : (
        <StarterGallery
          starters={starters}
          onPreview={(alias) => setPreviewAlias(alias)}
        />
      )}
    </section>
  );

  const list = (
    <section aria-label="My templates">
      <h2
        className="text-[16px] font-semibold mb-3"
        style={{ color: "var(--fg)", margin: 0 }}
      >
        My templates
      </h2>
      {loadError ? (
        <p className="mt-3 text-[13px]" style={{ color: "var(--danger-strong)" }}>
          {loadError}
        </p>
      ) : hasTemplates ? (
        <div className="mt-3">
          <TemplatesTable templates={templates} onChange={refresh} />
        </div>
      ) : (
        <p className="mt-3 text-[13px]" style={{ color: "var(--fg-muted)" }}>
          No templates yet. Start from a starter below, or create one over
          the API with{" "}
          <code style={inlineCodeStyle}>POST /v1/templates</code>.
        </p>
      )}
    </section>
  );

  return (
    <PageShell
      eyebrow="Workspace"
      title={
        <span className="inline-flex items-center gap-2.5">
          Templates <Chip tone="warn">Beta</Chip>
        </span>
      }
      subtitle={
        <>
          Reusable email templates with{" "}
          <code style={inlineCodeStyle}>{"{{variable}}"}</code>{" "}
          interpolation, rendered server-side at send time. Reference one
          from a send with{" "}
          <code style={inlineCodeStyle}>template_alias</code> +{" "}
          <code style={inlineCodeStyle}>template_data</code>.
        </>
      }
    >
      {loading ? (
        <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
          Loading…
        </p>
      ) : hasTemplates ? (
        <div className="space-y-10">
          <AgentPromptCard {...AGENT_PROMPTS.templates} />
          {list}
          {gallery}
        </div>
      ) : (
        // No templates yet — the agent prompt leads (hand the whole setup
        // to a coding agent), then the starter gallery for the one-click
        // path, with the (empty) list below.
        <div className="space-y-10">
          <AgentPromptCard {...AGENT_PROMPTS.templates} />
          {gallery}
          {list}
        </div>
      )}

      {previewAlias && (
        <StarterPreviewModal
          alias={previewAlias}
          onClose={() => setPreviewAlias(null)}
        />
      )}
    </PageShell>
  );
}

function TemplatesTable({
  templates,
  onChange,
}: {
  templates: TemplateSummaryView[];
  onChange: () => Promise<void> | void;
}) {
  return (
    <div
      className="overflow-x-auto"
      style={{
        background: "var(--bg-panel)",
        border: "1px solid var(--border)",
        borderRadius: "var(--r-lg)",
      }}
    >
      <table className="w-full text-[13px] min-w-[720px]">
        <thead>
          <tr
            className="text-left font-mono text-[10px] uppercase"
            style={{
              background: "var(--bg-elev)",
              color: "var(--fg-subtle)",
              letterSpacing: "0.08em",
            }}
          >
            <th className="px-4 py-2 font-semibold">Name</th>
            <th className="px-4 py-2 font-semibold">Alias</th>
            <th className="px-4 py-2 font-semibold">Subject</th>
            <th className="px-4 py-2 font-semibold">Updated</th>
            <th className="px-4 py-2"></th>
          </tr>
        </thead>
        <tbody>
          {templates.map((t, i) => (
            <TemplateRow
              key={t.id}
              template={t}
              onChange={onChange}
              isFirstRow={i === 0}
            />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function TemplateRow({
  template,
  onChange,
  isFirstRow,
}: {
  template: TemplateSummaryView;
  onChange: () => Promise<void> | void;
  isFirstRow: boolean;
}) {
  const [confirming, setConfirming] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState("");

  const handleDelete = async () => {
    setDeleting(true);
    setError("");
    try {
      const res = await fetch(
        `/v1/templates/${encodeURIComponent(template.id)}?confirm=DELETE`,
        { method: "DELETE", credentials: "include" },
      );
      if (!res.ok) {
        const text = await res.text().catch(() => `HTTP ${res.status}`);
        setError(text.trim() || `HTTP ${res.status}`);
        return;
      }
      await onChange();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setDeleting(false);
      setConfirming(false);
    }
  };

  return (
    <tr
      style={{
        borderTop: isFirstRow ? undefined : "1px solid var(--border-sub)",
      }}
    >
      <td className="px-4 py-3">
        <Link
          href={`/templates/edit?id=${encodeURIComponent(template.id)}`}
          className="font-medium hover:underline"
          style={{ color: "var(--fg)" }}
        >
          {template.name}
        </Link>
      </td>
      <td
        className="px-4 py-3 font-mono text-[12px]"
        style={{ color: "var(--fg-muted)" }}
      >
        {template.alias || "—"}
      </td>
      <td
        className="px-4 py-3 text-[12px] max-w-[280px] truncate"
        style={{ color: "var(--fg-muted)" }}
        title={template.subject}
      >
        {template.subject}
      </td>
      <td
        className="px-4 py-3 font-mono text-[12px]"
        style={{ color: "var(--fg-muted)" }}
      >
        {formatDate(template.updated_at)}
      </td>
      <td className="px-4 py-3 text-right whitespace-nowrap">
        {error && (
          <span
            className="text-[11px] mr-2"
            style={{ color: "var(--danger-strong)" }}
          >
            {error}
          </span>
        )}
        {confirming ? (
          <span className="inline-flex gap-1">
            <button
              onClick={handleDelete}
              disabled={deleting}
              className="px-2 py-1 text-[11px] transition disabled:opacity-50"
              style={{
                background: "var(--danger)",
                color: "#fff",
                borderRadius: "var(--r-sm)",
              }}
            >
              {deleting ? "Deleting…" : "Confirm"}
            </button>
            <button
              onClick={() => {
                setConfirming(false);
                setError("");
              }}
              disabled={deleting}
              className="px-2 py-1 text-[11px] transition"
              style={{
                background: "var(--bg-panel)",
                border: "1px solid var(--border)",
                color: "var(--fg)",
                borderRadius: "var(--r-sm)",
              }}
            >
              Cancel
            </button>
          </span>
        ) : (
          <button
            onClick={() => setConfirming(true)}
            disabled={deleting}
            className="px-2 py-1 text-[11px] transition disabled:opacity-50"
            style={{
              background: "var(--bg-panel)",
              border: "1px solid var(--border)",
              color: "var(--fg)",
              borderRadius: "var(--r-sm)",
            }}
          >
            Delete
          </button>
        )}
      </td>
    </tr>
  );
}

function formatDate(iso: string): string {
  try {
    return new Date(iso).toLocaleDateString(undefined, {
      year: "numeric",
      month: "short",
      day: "numeric",
    });
  } catch {
    return iso;
  }
}
