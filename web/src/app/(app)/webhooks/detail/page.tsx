"use client";

// One webhook subscription: what it is, what it is scoped to, and what it has
// actually been delivered. The list answers "do I have a problem"; this page
// answers "what is this endpoint receiving, and is it working".
//
// Addressed by query param (?id=wh_…) rather than a dynamic route segment:
// the dashboard is a static export (next.config `output: "export"`) and the
// app contains no dynamic segments — /webhooks/[id] would not build. This
// matches /inboxes/messages?email= and /inboxes/settings?email=.

import { Suspense, useState } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import useSWR from "swr";
import { Chip, Dot } from "@e2a/ui";
import { PageShell } from "../../../components/loft/PageShell";
import { ApiError, getWebhook } from "../../../components/onboarding/api";
import {
  classifyWebhookHealth,
  describeScope,
  describeWebhookToggleError,
  HEALTH_LABEL,
  type WebhookView,
} from "../../../../lib/webhooks";
import { webhookKey } from "../../../../lib/swrKeys";
import { DeliveriesFeed } from "./DeliveriesFeed";

export default function WebhookDetailPage() {
  return (
    <Suspense fallback={null}>
      <WebhookDetailRouter />
    </Suspense>
  );
}

function WebhookDetailRouter() {
  const searchParams = useSearchParams();
  const id = searchParams.get("id") ?? "";
  // Keyed by id so navigating between subscriptions remounts rather than
  // showing the previous endpoint's data while a new fetch is in flight.
  return <WebhookDetailContent key={id} id={id} />;
}

function WebhookDetailContent({ id }: { id: string }) {
  const {
    data: webhook,
    error,
    isLoading,
    mutate,
  } = useSWR(id ? webhookKey(id) : null, () => getWebhook(id));

  if (!id) {
    return (
      <DetailMessage
        title="No webhook selected"
        body="This page needs a webhook id — open it from the Webhooks list."
      />
    );
  }

  if (isLoading) {
    return (
      <PageShell eyebrow="Workspace" title={<>Webhook</>}>
        <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
          Loading…
        </p>
      </PageShell>
    );
  }

  // A missing webhook and a broken server are different problems and must not
  // share a message: the id comes from a user-editable query string and
  // delivery rows outlive their subscription, so 404 is an ordinary state —
  // but reporting "not found" for a 500 sends the reader to debug the address
  // bar instead of the outage.
  if (error) {
    const notFound = error instanceof ApiError && error.status === 404;
    return notFound ? (
      <DetailMessage
        title="Couldn't find that webhook"
        body="It may have been deleted, or the id in the address bar may be wrong."
      />
    ) : (
      <DetailMessage
        title="Couldn't load that webhook"
        body="Something went wrong fetching it. Try again in a moment."
      />
    );
  }

  if (!webhook) {
    return (
      <DetailMessage
        title="Couldn't find that webhook"
        body="It may have been deleted, or the id in the address bar may be wrong."
      />
    );
  }

  const scope = describeScope(webhook.filters);
  const health = classifyWebhookHealth(webhook, new Date());
  const healthTone =
    health.kind === "active"
      ? "success"
      : health.kind === "auto_disabled"
        ? "danger"
        : "warn";

  return (
    <PageShell
      eyebrow="Workspace"
      title={<>Webhook</>}
      subtitle={
        <span className="font-mono text-[13px] break-all">{webhook.url}</span>
      }
    >
      <BackLink />

      <section
        className="rounded-[var(--r-lg)] border p-5 mt-4"
        style={{ background: "var(--bg-panel)", borderColor: "var(--border)" }}
      >
        <dl className="grid gap-4 md:grid-cols-3">
          <Field label="Status">
            <Chip tone={healthTone}>
              <Dot tone={healthTone} />
              {HEALTH_LABEL[health.kind]}
            </Chip>
            {health.lastDeliveredAt ? (
              <span
                className="block font-mono text-[11px] mt-1.5"
                style={{ color: "var(--fg-subtle)" }}
              >
                last delivery {health.lastDeliveredAt.toLocaleString()}
              </span>
            ) : null}
          </Field>

          <Field label="Events">
            <span className="font-mono text-[11px]" style={{ color: "var(--fg-muted)" }}>
              {(webhook.events ?? []).length > 0
                ? (webhook.events ?? []).join(", ")
                : "all"}
            </span>
          </Field>

          {/* Scope carries the same warn treatment as the list row: an
              unscoped subscription receives every agent's events, and this
              page is where that gets audited. */}
          <Field label="Scope">
            {scope.scoped ? (
              <span
                className="font-mono text-[11px]"
                style={{ color: "var(--fg-muted)" }}
              >
                {scope.parts.join(" · ")}
              </span>
            ) : (
              <span
                className="font-mono text-[11px]"
                style={{ color: "var(--warn-strong)" }}
                title="No filters set — this endpoint receives events for every agent on the account."
              >
                all agents
              </span>
            )}
          </Field>
        </dl>
      </section>

      {health.kind === "auto_disabled" ? (
        <AutoDisabledBanner webhook={webhook} onChanged={() => mutate()} />
      ) : null}

      <DeliveriesFeed webhookId={webhook.id} />
    </PageShell>
  );
}

// AutoDisabledBanner is the recovery surface for an auto-disabled webhook:
// it names the concrete failure (auto_disabled_reason) and carries a real
// Re-enable button — the dashboard must never tell the user to go use the
// API. A 409 webhook_cooldown renders as expected-state copy, never a raw
// error, and the page refetches after any outcome-changing PATCH rather
// than trusting local state (re-enable is the action most likely to run
// against a stale page).
function AutoDisabledBanner({
  webhook,
  onChanged,
}: {
  webhook: Pick<WebhookView, "id" | "auto_disabled_reason">;
  onChanged: () => void;
}) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");

  const reenable = async () => {
    setPending(true);
    setError("");
    try {
      const res = await fetch(`/v1/webhooks/${encodeURIComponent(webhook.id)}`, {
        method: "PATCH",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ enabled: true }),
      });
      if (!res.ok) {
        const text = await res.text().catch(() => "");
        setError(describeWebhookToggleError(res.status, text));
        return;
      }
      onChanged();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setPending(false);
    }
  };

  return (
    <div
      className="rounded-[var(--r-lg)] border p-4 mt-4 text-[13px]"
      style={{
        background: "var(--danger-bg)",
        borderColor: "var(--danger)",
        color: "var(--danger-strong)",
      }}
    >
      <strong>Delivery paused.</strong> e2a disabled this webhook after
      repeated delivery failures
      {webhook.auto_disabled_reason ? (
        <>
          {" "}
          (most recent error:{" "}
          <code
            className="font-mono text-[12px] px-1 py-0.5"
            style={{
              background: "var(--bg-panel)",
              color: "var(--fg)",
              borderRadius: "var(--r-sm)",
            }}
          >
            {webhook.auto_disabled_reason}
          </code>
          )
        </>
      ) : null}
      . Fix the endpoint, then re-enable it here.
      <div className="mt-3 flex items-center gap-3 flex-wrap">
        <button
          onClick={reenable}
          disabled={pending}
          className="px-3 py-1.5 text-[12px] font-medium transition disabled:opacity-50"
          style={{
            background: "var(--danger)",
            color: "#fff",
            borderRadius: "var(--r-sm)",
          }}
        >
          {pending ? "Re-enabling…" : "Re-enable webhook"}
        </button>
        {error && <span>{error}</span>}
      </div>
    </div>
  );
}

function Field({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="min-w-0">
      <dt
        className="font-mono text-[11px] mb-1.5"
        style={{ color: "var(--fg-subtle)", letterSpacing: "0.04em" }}
      >
        {label.toUpperCase()}
      </dt>
      <dd className="m-0">{children}</dd>
    </div>
  );
}

function BackLink() {
  return (
    <Link
      href="/webhooks"
      className="inline-flex items-center gap-1 text-[13px] hover:underline"
      style={{ color: "var(--fg-muted)", textDecoration: "none" }}
    >
      <span aria-hidden>←</span> All webhooks
    </Link>
  );
}

function DetailMessage({ title, body }: { title: string; body: string }) {
  return (
    <PageShell eyebrow="Workspace" title={<>Webhook</>}>
      <BackLink />
      <div
        className="rounded-[var(--r-lg)] border p-8 text-center mt-4"
        style={{ background: "var(--bg-panel)", borderColor: "var(--border)" }}
      >
        <p className="text-[15px] font-semibold m-0" style={{ color: "var(--fg)" }}>
          {title}
        </p>
        <p
          className="text-[13px] mt-2 mb-0"
          style={{ color: "var(--fg-muted)" }}
        >
          {body}
        </p>
      </div>
    </PageShell>
  );
}
