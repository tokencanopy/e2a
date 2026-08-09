"use client";

// One batch send: how many messages it fanned out, how many were accepted vs
// dropped by the suppression filter, and — the live part — where its child
// messages are in the delivery lifecycle. The dashboard doesn't compose
// batches (agents do, via the API/CLI/MCP); this page is how a human MONITORS
// one, the batch analogue of the per-message delivery status.
//
// Addressed by query param (?id=bat_…) rather than a dynamic route segment:
// the dashboard is a static export (next.config `output: "export"`), so the
// app has no dynamic segments — /batches/[id] would not build. Matches
// /webhooks/detail?id= and /inboxes/messages?email=.

import { Suspense } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import useSWR from "swr";
import { Chip, type ChipTone } from "@e2a/ui";
import { PageShell } from "../../../components/loft/PageShell";
import {
  ApiError,
  getBatch,
  type BatchStatusRollup,
  type BatchSuppressedItem,
} from "../../../components/onboarding/api";
import { batchKey } from "../../../../lib/swrKeys";

// The rollup keys in display order, each with its label + chip tone. Tones
// mirror MessageStatusChip so a batch reads the same as its individual
// messages: green = done well, amber = slow, red = needs attention, blue = in
// flight.
const ROLLUP_STATUSES: ReadonlyArray<{
  key: keyof BatchStatusRollup;
  label: string;
  tone: ChipTone;
}> = [
  { key: "accepted", label: "Queued", tone: "info" },
  { key: "sending", label: "Sending", tone: "info" },
  { key: "sent", label: "Sent", tone: "success" },
  { key: "delivered", label: "Delivered", tone: "success" },
  { key: "deferred", label: "Delayed", tone: "warn" },
  { key: "bounced", label: "Bounced", tone: "danger" },
  { key: "complained", label: "Complaint", tone: "danger" },
  { key: "failed", label: "Failed", tone: "danger" },
];

export default function BatchDetailPage() {
  return (
    <Suspense fallback={null}>
      <BatchDetailRouter />
    </Suspense>
  );
}

function BatchDetailRouter() {
  const searchParams = useSearchParams();
  const id = searchParams.get("id") ?? "";
  // Keyed by id so navigating between batches remounts rather than showing the
  // previous batch's rollup while a new fetch is in flight.
  return <BatchDetailContent key={id} id={id} />;
}

function BatchDetailContent({ id }: { id: string }) {
  const { data: batch, error, isLoading } = useSWR(
    id ? batchKey(id) : null,
    () => getBatch(id),
    // The rollup is computed on read, so poll while the page is open to show
    // delivery progress without a manual refresh.
    { refreshInterval: 10_000 },
  );

  if (!id) {
    return (
      <DetailMessage
        title="No batch selected"
        body="This page needs a batch id — open it with ?id=bat_… (a batch id is returned by a batch send via the API, CLI, or MCP)."
      />
    );
  }

  if (isLoading) {
    return (
      <PageShell eyebrow="Workspace" title={<>Batch</>}>
        <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
          Loading…
        </p>
      </PageShell>
    );
  }

  // A missing batch and a broken server are different problems: the id comes
  // from a user-editable query string, so 404 is an ordinary state — but
  // reporting "not found" for a 500 sends the reader to debug the address bar
  // instead of the outage.
  if (error) {
    const notFound = error instanceof ApiError && error.status === 404;
    return notFound ? (
      <DetailMessage
        title="Couldn't find that batch"
        body="It may belong to another account, or the id in the address bar may be wrong."
      />
    ) : (
      <DetailMessage
        title="Couldn't load that batch"
        body="Something went wrong fetching it. Try again in a moment."
      />
    );
  }

  if (!batch) {
    return (
      <DetailMessage
        title="Couldn't find that batch"
        body="It may belong to another account, or the id in the address bar may be wrong."
      />
    );
  }

  const rollup = batch.status_rollup;
  const nonZero = ROLLUP_STATUSES.filter((s) => (rollup[s.key] ?? 0) > 0);
  const suppressedCount = batch.suppressed.length;

  return (
    <PageShell
      eyebrow="Workspace"
      title={<>Batch</>}
      subtitle={<span className="font-mono text-[13px] break-all">{batch.batch_id}</span>}
    >
      <BackLink />

      <section
        className="rounded-[var(--r-lg)] border p-5 mt-4"
        style={{ background: "var(--bg-panel)", borderColor: "var(--border)" }}
      >
        <dl className="grid gap-4 md:grid-cols-4">
          <Field label="Requested">
            <span className="text-[15px] font-semibold" style={{ color: "var(--fg)" }}>
              {batch.requested}
            </span>
          </Field>
          <Field label="Accepted">
            <span className="text-[15px] font-semibold" style={{ color: "var(--fg)" }}>
              {batch.accepted}
            </span>
          </Field>
          <Field label="Suppressed">
            <span
              className="text-[15px] font-semibold"
              style={{ color: suppressedCount > 0 ? "var(--warn-strong)" : "var(--fg)" }}
            >
              {suppressedCount}
            </span>
          </Field>
          <Field label="Created">
            <span className="font-mono text-[11px]" style={{ color: "var(--fg-muted)" }}>
              {new Date(batch.created_at).toLocaleString()}
            </span>
          </Field>
        </dl>
      </section>

      {/* Live delivery rollup — the point of the page. */}
      <section className="mt-4">
        <h2
          className="font-mono text-[11px] mb-2"
          style={{ color: "var(--fg-subtle)", letterSpacing: "0.04em" }}
        >
          DELIVERY STATUS
        </h2>
        {nonZero.length > 0 ? (
          <div className="flex flex-wrap gap-2">
            {nonZero.map((s) => (
              <Chip key={s.key} tone={s.tone}>
                {s.label} · {rollup[s.key]}
              </Chip>
            ))}
          </div>
        ) : (
          <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
            No child messages yet — an all-suppressed batch, or delivery hasn&apos;t started.
          </p>
        )}
      </section>

      {suppressedCount > 0 ? (
        <section className="mt-6">
          <h2
            className="font-mono text-[11px] mb-2"
            style={{ color: "var(--fg-subtle)", letterSpacing: "0.04em" }}
          >
            SUPPRESSED AT ACCEPT ({suppressedCount})
          </h2>
          <div
            className="rounded-[var(--r-lg)] border overflow-hidden"
            style={{ borderColor: "var(--border)" }}
          >
            {batch.suppressed.map((s: BatchSuppressedItem) => (
              <div
                key={s.item_index}
                className="flex items-center justify-between gap-4 px-4 py-2.5 border-b last:border-b-0 text-[13px]"
                style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}
              >
                <span className="font-mono break-all" style={{ color: "var(--fg)" }}>
                  {s.address}
                </span>
                <span className="font-mono text-[11px] shrink-0" style={{ color: "var(--fg-subtle)" }}>
                  item {s.item_index} · {s.reason}
                </span>
              </div>
            ))}
          </div>
        </section>
      ) : null}
    </PageShell>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
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
      href="/dashboard"
      className="inline-flex items-center gap-1 text-[13px] hover:underline"
      style={{ color: "var(--fg-muted)", textDecoration: "none" }}
    >
      <span aria-hidden>←</span> Dashboard
    </Link>
  );
}

function DetailMessage({ title, body }: { title: string; body: string }) {
  return (
    <PageShell eyebrow="Workspace" title={<>Batch</>}>
      <BackLink />
      <div
        className="rounded-[var(--r-lg)] border p-8 text-center mt-4"
        style={{ background: "var(--bg-panel)", borderColor: "var(--border)" }}
      >
        <p className="text-[15px] font-semibold m-0" style={{ color: "var(--fg)" }}>
          {title}
        </p>
        <p className="text-[13px] mt-2 mb-0" style={{ color: "var(--fg-muted)" }}>
          {body}
        </p>
      </div>
    </PageShell>
  );
}
