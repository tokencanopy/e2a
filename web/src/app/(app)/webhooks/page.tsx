"use client";

import { useCallback, useEffect, useState } from "react";
import { PageShell } from "../../components/loft/PageShell";
import {
  AgentPromptCard,
  AGENT_PROMPTS,
} from "../../components/AgentPromptCard";
import Link from "next/link";
import {
  classifyWebhookHealth,
  describeScope,
  HEALTH_LABEL,
  healthColor,
  type WebhookView,
} from "../../../lib/webhooks";

// /webhooks owns the user's webhook lifecycle — create, reveal
// the one-time signing secret, rotate it, delete. In the /v1 redesign
// signing secrets are per-webhook (not account-wide shared HMAC
// secrets), so this page manages webhook endpoints over /v1/webhooks
// and reveals each webhook's signing_secret once at create/rotate time.

// Inline-code chip used inline in the subtitle copy. Pulled out so
// the two usages in the page header stay byte-identical.
// Inline code inside running prose. Sized in `em` so the chip tracks the
// paragraph's font size instead of jumping to a fixed 12px mid-sentence,
// and given a tight line-height so the border box stays inside the 1.6
// line box — otherwise the chip crowds the lines above and below it.
const inlineCodeStyle: React.CSSProperties = {
  fontFamily: "var(--f-mono)",
  fontSize: "0.92em",
  lineHeight: 1,
  padding: "1px 6px",
  background: "var(--bg-elev)",
  border: "1px solid var(--border-sub)",
  borderRadius: "var(--r-sm)",
  color: "var(--fg)",
  whiteSpace: "nowrap",
};

// The curated set of common event types shown in the create picker. The
// server's CreateWebhookRequest.events enum (api/openapi.yaml) accepts more
// (bounced/complained/review_*/domain.*), but we surface only the ones most
// subscribers want — covering the inbound, outbound-lifecycle, and review/
// screening paths. The full set stays valid over the API for advanced callers.
const EVENT_TYPES = [
  "email.received",
  "email.sent",
  "email.delivered",
  "email.review_requested",
  "email.flagged",
  "email.blocked",
] as const;

// One-line, user-facing descriptions for the event picker. Mirrors the
// canonical catalog in internal/webhookpub/event.go — keep in sync when the
// backend event set changes.
const EVENT_DESCRIPTIONS: Record<(typeof EVENT_TYPES)[number], string> = {
  "email.received": "An inbound email was delivered to the inbox.",
  "email.sent": "An outbound message was accepted for delivery.",
  "email.delivered": "The provider confirmed an outbound message reached the recipient.",
  "email.review_requested": "A message is held for human review (inbound screening or an outbound HITL hold).",
  "email.flagged": "An inbound message was delivered but didn't match the inbox's ingestion policy.",
  "email.blocked": "A message was refused by screening (policy action: block).",
};

// A revealed secret (from a create or a rotate). `kind` drives the
// banner copy; `webhookUrl` lets the reviewer confirm which endpoint
// the secret belongs to.
type RevealedSecret = {
  kind: "created" | "rotated";
  secret: string;
  webhookUrl: string;
};

export default function WebhooksPage() {
  const [webhooks, setWebhooks] = useState<WebhookView[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [revealed, setRevealed] = useState<RevealedSecret | null>(null);
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState("");
  const [newURL, setNewURL] = useState("");
  const [newEvents, setNewEvents] = useState<string[]>([
    "email.received",
  ]);
  const [showCreateForm, setShowCreateForm] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    setLoadError("");
    try {
      const res = await fetch("/v1/webhooks", {
        credentials: "include",
      });
      if (!res.ok) {
        setLoadError(`Failed to load (HTTP ${res.status})`);
        setLoading(false);
        return;
      }
      const body = await res.json();
      setWebhooks(body.items ?? []);
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const toggleEvent = (ev: string) => {
    setNewEvents((prev) =>
      prev.includes(ev) ? prev.filter((e) => e !== ev) : [...prev, ev],
    );
  };

  const handleCreate = async () => {
    setCreating(true);
    setCreateError("");
    try {
      const res = await fetch("/v1/webhooks", {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ url: newURL.trim(), events: newEvents }),
      });
      if (!res.ok) {
        const text = await res.text().catch(() => `HTTP ${res.status}`);
        setCreateError(text.trim() || `HTTP ${res.status}`);
        return;
      }
      const body: WebhookView = await res.json();
      if (body.signing_secret) {
        setRevealed({
          kind: "created",
          secret: body.signing_secret,
          webhookUrl: body.url,
        });
      }
      setShowCreateForm(false);
      setNewURL("");
      setNewEvents(["email.received"]);
      await refresh();
    } catch (err) {
      setCreateError(err instanceof Error ? err.message : String(err));
    } finally {
      setCreating(false);
    }
  };

  return (
    <PageShell
      eyebrow="Workspace"
      title={<>Webhooks</>}
      subtitle={
        <>
          <span style={{ display: "block" }}>
            <strong style={{ color: "var(--fg)" }}>For webhook delivery.</strong>{" "}
            When your inbox receives mail via a webhook subscription, e2a{" "}
            {/* The hyphen in "HMAC-signs" is a valid break opportunity, so it
                lands at end-of-line and reads as a hyphenation artifact.
                nowrap keeps the compound intact. */}
            <span style={{ whiteSpace: "nowrap" }}>HMAC-signs</span>{" "}
            every payload it POSTs to the endpoint with that webhook&apos;s
            signing secret so your handler can confirm the request really came
            from e2a.
          </span>
          <span style={{ display: "block", marginTop: 10 }}>
            The signing secret is shown <strong style={{ color: "var(--fg)" }}>once</strong>{" "}
            when you create or rotate a webhook — copy it then. Pass it to{" "}
            {/* Keep the two chips and their separator on one line: split
                across a line break they read as two unrelated fragments. */}
            <span style={{ whiteSpace: "nowrap" }}>
              <code style={inlineCodeStyle}>constructEvent()</code> /{" "}
              <code style={inlineCodeStyle}>construct_event()</code>
            </span>{" "}
            in the SDK to validate. Rotation keeps the previous secret valid
            for a short overlap so you can swap it in without dropping
            deliveries.
          </span>
          <span
            style={{
              display: "block",
              marginTop: 10,
              color: "var(--fg-subtle)",
              fontSize: 12,
            }}
          >
            Local-mode inboxes pull messages via WebSocket and don&apos;t
            need this — the WS auth handshake covers it.
          </span>
        </>
      }
    >
      {revealed && (
        <RevealedSecretBanner
          revealed={revealed}
          onDismiss={() => setRevealed(null)}
        />
      )}

      <div className="mb-10">
        <AgentPromptCard {...AGENT_PROMPTS.webhooks} />
      </div>

      {loading ? (
        <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
          Loading…
        </p>
      ) : loadError ? (
        <p className="text-[13px]" style={{ color: "var(--danger-strong)" }}>
          {loadError}
        </p>
      ) : (
        <WebhooksTable
          webhooks={webhooks}
          onChange={refresh}
          onRevealRotated={(secret, webhookUrl) =>
            setRevealed({ kind: "rotated", secret, webhookUrl })
          }
        />
      )}

      <div className="mt-4">
        {!showCreateForm ? (
          <button
            onClick={() => {
              setShowCreateForm(true);
              setCreateError("");
            }}
            className="px-4 py-2 text-[13px] font-medium transition disabled:opacity-50"
            style={{
              background: "var(--accent-fill)",
              color: "var(--accent-fg)",
              borderRadius: "var(--r-md)",
            }}
          >
            Add webhook
          </button>
        ) : (
          <div className="space-y-3 max-w-md">
            <label className="block">
              <span className="text-[13px]" style={{ color: "var(--fg)" }}>
                Endpoint URL
              </span>
              <input
                autoFocus
                type="url"
                value={newURL}
                onChange={(e) => setNewURL(e.target.value)}
                placeholder="https://your-app.com/inbox"
                className="mt-1 w-full px-3 py-2 text-[13px]"
                style={{
                  background: "var(--bg-panel)",
                  border: "1px solid var(--border)",
                  borderRadius: "var(--r-md)",
                  color: "var(--fg)",
                }}
              />
            </label>
            <div>
              <span className="text-[13px]" style={{ color: "var(--fg)" }}>
                Subscribed events
              </span>
              <div className="mt-1.5 flex flex-col gap-1">
                {EVENT_TYPES.map((ev) => {
                  const on = newEvents.includes(ev);
                  return (
                    <button
                      type="button"
                      key={ev}
                      onClick={() => toggleEvent(ev)}
                      aria-pressed={on}
                      className="flex items-baseline gap-3 px-3 py-2 text-left transition"
                      style={{
                        background: on ? "var(--accent-fill)" : "var(--bg-panel)",
                        border: `1px solid ${on ? "var(--accent-fill)" : "var(--border)"}`,
                        borderRadius: "var(--r-sm)",
                      }}
                    >
                      <span
                        className="font-mono text-[11px] shrink-0"
                        style={{ color: on ? "var(--accent-fg)" : "var(--fg)", minWidth: 150 }}
                      >
                        {ev}
                      </span>
                      <span
                        className="text-[11px]"
                        style={{ color: on ? "var(--accent-fg)" : "var(--fg-muted)" }}
                      >
                        {EVENT_DESCRIPTIONS[ev]}
                      </span>
                    </button>
                  );
                })}
              </div>
            </div>
            {createError && (
              <p
                className="text-[13px]"
                style={{ color: "var(--danger-strong)" }}
              >
                {createError}
              </p>
            )}
            <div className="flex gap-2">
              <button
                onClick={handleCreate}
                disabled={creating || !newURL.trim() || newEvents.length === 0}
                className="px-4 py-2 text-[13px] font-medium transition disabled:opacity-50"
                style={{
                  background: "var(--accent-fill)",
                  color: "var(--accent-fg)",
                  borderRadius: "var(--r-md)",
                }}
              >
                {creating ? "Creating…" : "Create"}
              </button>
              <button
                onClick={() => {
                  setShowCreateForm(false);
                  setNewURL("");
                  setNewEvents(["email.received"]);
                  setCreateError("");
                }}
                disabled={creating}
                className="px-4 py-2 text-[13px] transition"
                style={{
                  background: "var(--bg-panel)",
                  color: "var(--fg)",
                  border: "1px solid var(--border)",
                  borderRadius: "var(--r-md)",
                }}
              >
                Cancel
              </button>
            </div>
          </div>
        )}
      </div>
    </PageShell>
  );
}

function RevealedSecretBanner({
  revealed,
  onDismiss,
}: {
  revealed: RevealedSecret;
  onDismiss: () => void;
}) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(revealed.secret);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // No clipboard in test env — silently noop.
    }
  };

  return (
    <div
      className="mb-4 p-4"
      style={{
        background: "var(--warn-bg)",
        border: "1px solid var(--warn-bg)",
        borderRadius: "var(--r-md)",
      }}
    >
      <h3
        className="text-[14px] font-semibold mb-1"
        style={{ color: "var(--warn-strong)" }}
      >
        {revealed.kind === "created"
          ? "Webhook created — copy this signing secret now"
          : "Secret rotated — copy the new signing secret now"}
      </h3>
      <p className="text-[13px] mb-3" style={{ color: "var(--warn-strong)" }}>
        This is the only time we&apos;ll show it. It belongs to{" "}
        <code
          className="font-mono text-[12px] px-1 py-0.5 break-all"
          style={{
            background: "var(--bg-panel)",
            color: "var(--fg)",
            borderRadius: "var(--r-sm)",
          }}
        >
          {revealed.webhookUrl}
        </code>
        . Store it in your environment (commonly{" "}
        <code
          className="font-mono text-[12px] px-1 py-0.5"
          style={{
            background: "var(--bg-panel)",
            color: "var(--fg)",
            borderRadius: "var(--r-sm)",
          }}
        >
          E2A_WEBHOOK_SECRET
        </code>
        ).
      </p>
      <div className="flex gap-2 items-start flex-wrap">
        <input
          readOnly
          value={revealed.secret}
          aria-label="Plaintext signing secret"
          onFocus={(e) => e.currentTarget.select()}
          className="flex-1 min-w-[200px] font-mono text-[12px] px-3 py-2"
          style={{
            background: "var(--bg-panel)",
            border: "1px solid var(--border)",
            borderRadius: "var(--r-md)",
            color: "var(--fg)",
          }}
        />
        <button
          onClick={copy}
          className="px-3 py-2 text-[13px] whitespace-nowrap transition"
          style={{
            background: "var(--bg-panel)",
            color: "var(--fg)",
            border: "1px solid var(--border)",
            borderRadius: "var(--r-md)",
          }}
        >
          {copied ? "Copied" : "Copy"}
        </button>
        <button
          onClick={onDismiss}
          className="px-3 py-2 text-[13px] transition"
          style={{
            background: "var(--bg-panel)",
            color: "var(--fg)",
            border: "1px solid var(--border)",
            borderRadius: "var(--r-md)",
          }}
        >
          Dismiss
        </button>
      </div>
    </div>
  );
}

function WebhooksTable({
  webhooks,
  onChange,
  onRevealRotated,
}: {
  webhooks: WebhookView[];
  onChange: () => Promise<void> | void;
  onRevealRotated: (secret: string, webhookUrl: string) => void;
}) {
  if (webhooks.length === 0) {
    return (
      <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
        No webhooks yet. Add one to start receiving signed event payloads.
      </p>
    );
  }
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
            <th className="px-4 py-2 font-semibold">URL</th>
            <th className="px-4 py-2 font-semibold">Events</th>
            <th className="px-4 py-2 font-semibold">Scope</th>
            <th className="px-4 py-2 font-semibold">Status</th>
            <th className="px-4 py-2 font-semibold">Created</th>
            <th className="px-4 py-2"></th>
          </tr>
        </thead>
        <tbody>
          {webhooks.map((w, i) => (
            <WebhookRow
              key={w.id}
              webhook={w}
              onChange={onChange}
              onRevealRotated={onRevealRotated}
              isFirstRow={i === 0}
            />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function WebhookRow({
  webhook,
  onChange,
  onRevealRotated,
  isFirstRow,
}: {
  webhook: WebhookView;
  onChange: () => Promise<void> | void;
  onRevealRotated: (secret: string, webhookUrl: string) => void;
  isFirstRow: boolean;
}) {
  const [confirming, setConfirming] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [rotating, setRotating] = useState(false);
  const [error, setError] = useState("");

  const handleDelete = async () => {
    setDeleting(true);
    setError("");
    try {
      const res = await fetch(
        `/v1/webhooks/${encodeURIComponent(webhook.id)}?confirm=DELETE`,
        {
          method: "DELETE",
          credentials: "include",
        },
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

  const handleRotate = async () => {
    setRotating(true);
    setError("");
    try {
      const res = await fetch(
        `/v1/webhooks/${encodeURIComponent(webhook.id)}/rotate-secret`,
        {
          method: "POST",
          credentials: "include",
        },
      );
      if (!res.ok) {
        const text = await res.text().catch(() => `HTTP ${res.status}`);
        setError(text.trim() || `HTTP ${res.status}`);
        return;
      }
      const body: { signing_secret?: string } = await res.json();
      if (body.signing_secret) {
        onRevealRotated(body.signing_secret, webhook.url);
      }
      await onChange();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setRotating(false);
    }
  };

  const busy = deleting || rotating;
  const scope = describeScope(webhook.filters);
  const health = classifyWebhookHealth(webhook, new Date());

  return (
    <tr
      style={{
        borderTop: isFirstRow ? undefined : "1px solid var(--border-sub)",
      }}
    >
      {/* The URL is the row's entry point into the per-endpoint view. Query
          param, not a route segment — the dashboard is a static export with
          no dynamic segments. */}
      <td className="px-4 py-3 font-mono text-[12px] break-all">
        <Link
          href={`/webhooks/detail?id=${encodeURIComponent(webhook.id)}`}
          className="hover:underline"
          style={{ color: "var(--fg)", textDecoration: "none" }}
        >
          {webhook.url}
        </Link>
      </td>
      <td className="px-4 py-3 font-mono text-[11px]" style={{ color: "var(--fg-muted)" }}>
        {(webhook.events ?? []).length > 0
          ? (webhook.events ?? []).join(", ")
          : "all"}
      </td>
      {/* Scope. An unscoped subscription receives every agent's events, so it
          is called out in warn tone rather than rendered as blank/"none" —
          a silent empty cell reads as "nothing notable here", which is the
          opposite of the truth. */}
      <td className="px-4 py-3 font-mono text-[11px]">
        {scope.scoped ? (
          <span style={{ color: "var(--fg-muted)" }}>
            {scope.parts.join(" · ")}
          </span>
        ) : (
          <span
            style={{ color: "var(--warn-strong)" }}
            title="No filters set — this endpoint receives events for every agent on the account."
          >
            all agents
          </span>
        )}
      </td>
      {/* Status carries the health signal rather than just the enabled flag:
          "enabled" alone is true of an endpoint that has silently never
          delivered, or that e2a switched off. Derived from fields already on
          this response — no per-row probe. */}
      <td className="px-4 py-3 font-mono text-[11px]">
        <span style={{ color: healthColor(health.kind) }}>
          {HEALTH_LABEL[health.kind]}
        </span>
        {health.lastDeliveredAt ? (
          <span className="block" style={{ color: "var(--fg-subtle)" }}>
            last {health.lastDeliveredAt.toLocaleDateString()}
          </span>
        ) : null}
      </td>
      <td
        className="px-4 py-3 font-mono text-[12px]"
        style={{ color: "var(--fg-muted)" }}
      >
        {formatDate(webhook.created_at)}
      </td>
      <td className="px-4 py-3 text-right">
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
          <span className="inline-flex gap-1">
            <button
              onClick={handleRotate}
              disabled={busy}
              title="Generate a new signing secret (the old one stays valid for a short overlap)"
              className="px-2 py-1 text-[11px] transition disabled:opacity-50"
              style={{
                background: "var(--bg-panel)",
                border: "1px solid var(--border)",
                color: "var(--fg)",
                borderRadius: "var(--r-sm)",
              }}
            >
              {rotating ? "Rotating…" : "Rotate secret"}
            </button>
            <button
              onClick={() => setConfirming(true)}
              disabled={busy}
              className="px-2 py-1 text-[11px] transition disabled:opacity-50 disabled:cursor-not-allowed"
              style={{
                background: "var(--bg-panel)",
                border: "1px solid var(--border)",
                color: "var(--fg)",
                borderRadius: "var(--r-sm)",
              }}
            >
              Delete
            </button>
          </span>
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
