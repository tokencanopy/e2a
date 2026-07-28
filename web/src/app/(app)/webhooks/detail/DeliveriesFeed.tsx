"use client";

// The per-webhook delivery log: one row per attempt series against this
// endpoint. This is the "what is this endpoint actually receiving, and is it
// working" view — the question that had no answer anywhere in the dashboard.

import { useState } from "react";
import useSWR from "swr";
import { listWebhookDeliveries } from "../../../components/onboarding/api";
import {
  classifyDelivery,
  type DeliveryStateKind,
  type WebhookDeliveryView,
} from "../../../../lib/webhooks";
import { webhookDeliveriesKey } from "../../../../lib/swrKeys";

// The server-side filter accepts these; "" means unfiltered. Kept as a
// literal list rather than derived from the delivery rows, because the point
// of the filter is to find states that are ABSENT from the current page.
const STATUS_FILTERS = [
  { value: "", label: "All" },
  { value: "delivered", label: "Delivered" },
  { value: "failed", label: "Failed" },
  { value: "pending", label: "Pending" },
] as const;

// last_error is arbitrary bytes from a customer's endpoint — an HTML error
// page, a stack trace, a megabyte of nothing. Preview a bounded slice and let
// the operator opt into the rest.
const ERROR_PREVIEW_LEN = 160;

const STATE_LABEL: Record<DeliveryStateKind, string> = {
  delivered: "delivered",
  failed: "failed",
  retrying: "retrying",
  retry_due: "retry due",
  unknown: "",
};

function stateColor(kind: DeliveryStateKind): string {
  switch (kind) {
    case "delivered":
      return "var(--success)";
    case "failed":
      return "var(--danger-strong)";
    case "retrying":
    case "retry_due":
      return "var(--warn-strong)";
    default:
      // Unknown fails closed: never styled as success.
      return "var(--fg-muted)";
  }
}

export function DeliveriesFeed({ webhookId }: { webhookId: string }) {
  const [status, setStatus] = useState("");
  const { data, error, isLoading } = useSWR(
    webhookDeliveriesKey(webhookId, status),
    () => listWebhookDeliveries(webhookId, { status: status || undefined }),
  );

  const items = data?.items ?? [];
  // `now` is captured once per render and threaded into every row so the
  // whole table classifies against a single instant.
  const now = new Date();

  return (
    <section className="mt-8">
      <div className="flex items-center justify-between gap-4 flex-wrap mb-3">
        <h2
          className="text-[16px] font-semibold m-0"
          style={{ color: "var(--fg)" }}
        >
          Deliveries
        </h2>
        <div className="flex items-center gap-1 flex-wrap">
          {STATUS_FILTERS.map((f) => (
            <button
              key={f.value || "all"}
              onClick={() => setStatus(f.value)}
              className="px-2.5 py-1 text-[12px] transition"
              style={{
                background:
                  status === f.value ? "var(--bg-elev)" : "transparent",
                color:
                  status === f.value ? "var(--fg)" : "var(--fg-muted)",
                border: "1px solid",
                borderColor:
                  status === f.value ? "var(--border)" : "transparent",
                borderRadius: "var(--r-sm)",
              }}
            >
              {f.label}
            </button>
          ))}
        </div>
      </div>

      {isLoading ? (
        <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
          Loading…
        </p>
      ) : error ? (
        <p className="text-[13px]" style={{ color: "var(--danger-strong)" }}>
          Couldn&apos;t load deliveries.
        </p>
      ) : items.length === 0 ? (
        // Two different facts. "Never received anything" points at scope or
        // wiring; "nothing matched this filter" points at the filter.
        <EmptyState filtered={status !== ""} />
      ) : (
        <div
          className="rounded-[var(--r-lg)] border overflow-x-auto"
          style={{
            background: "var(--bg-panel)",
            borderColor: "var(--border)",
          }}
        >
          <table className="w-full text-left border-collapse">
            <thead>
              <tr
                className="font-mono text-[11px]"
                style={{
                  color: "var(--fg-subtle)",
                  borderBottom: "1px solid var(--border-sub)",
                }}
              >
                <th className="px-4 py-2 font-semibold">Event</th>
                <th className="px-4 py-2 font-semibold">State</th>
                <th className="px-4 py-2 font-semibold">Attempts</th>
                <th className="px-4 py-2 font-semibold">Response</th>
                <th className="px-4 py-2 font-semibold">Last attempt</th>
              </tr>
            </thead>
            <tbody>
              {items.map((d, i) => (
                <DeliveryRow
                  key={d.id}
                  delivery={d}
                  now={now}
                  isFirstRow={i === 0}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {data?.next_cursor ? (
        <p
          className="text-[12px] mt-3 mb-0"
          style={{ color: "var(--fg-subtle)" }}
        >
          Showing the most recent {items.length}. Older deliveries are
          available over the API.
        </p>
      ) : null}
    </section>
  );
}

function EmptyState({ filtered }: { filtered: boolean }) {
  return (
    <div
      className="rounded-[var(--r-lg)] border p-8 text-center"
      style={{ background: "var(--bg-panel)", borderColor: "var(--border)" }}
    >
      <p className="text-[13px] m-0" style={{ color: "var(--fg-muted)" }}>
        {filtered
          ? "No deliveries match this filter."
          : "No deliveries yet — this endpoint hasn't received anything."}
      </p>
    </div>
  );
}

function DeliveryRow({
  delivery,
  now,
  isFirstRow,
}: {
  delivery: WebhookDeliveryView;
  now: Date;
  isFirstRow: boolean;
}) {
  const [expanded, setExpanded] = useState(false);
  const state = classifyDelivery(delivery, now);
  // An unrecognized status is shown verbatim rather than mapped to a label
  // we'd be inventing.
  const label = STATE_LABEL[state.kind] || state.raw;

  const err = delivery.last_error ?? "";
  const needsTruncation = err.length > ERROR_PREVIEW_LEN;
  const shownError =
    needsTruncation && !expanded ? err.slice(0, ERROR_PREVIEW_LEN) + "…" : err;

  return (
    <tr
      style={{
        borderTop: isFirstRow ? undefined : "1px solid var(--border-sub)",
      }}
    >
      {/* Event type is an open set — rendered as the raw string so a type
          added server-side still appears rather than vanishing. */}
      <td
        className="px-4 py-3 font-mono text-[11px]"
        style={{ color: "var(--fg)" }}
      >
        {delivery.type}
      </td>
      <td className="px-4 py-3 font-mono text-[11px]">
        <span style={{ color: stateColor(state.kind) }}>{label}</span>
        {state.kind === "retrying" && delivery.next_retry_at ? (
          <span className="block" style={{ color: "var(--fg-subtle)" }}>
            next {formatTime(delivery.next_retry_at)}
          </span>
        ) : null}
      </td>
      <td
        className="px-4 py-3 font-mono text-[11px] tabular-nums"
        style={{ color: "var(--fg-muted)" }}
      >
        {delivery.attempts}
      </td>
      <td className="px-4 py-3 font-mono text-[11px]" style={{ maxWidth: 420 }}>
        {delivery.last_status_code ? (
          <span style={{ color: "var(--fg)" }}>{delivery.last_status_code}</span>
        ) : (
          <span style={{ color: "var(--fg-subtle)" }}>—</span>
        )}
        {err ? (
          <>
            {/* Plain text node: last_error is untrusted and never rendered
                as markup. */}
            <span
              className="block break-all mt-1"
              style={{ color: "var(--fg-muted)" }}
            >
              {shownError}
            </span>
            {needsTruncation ? (
              <button
                onClick={() => setExpanded((v) => !v)}
                className="mt-1 text-[11px] hover:underline"
                style={{
                  color: "var(--accent-strong)",
                  background: "none",
                  border: "none",
                  padding: 0,
                }}
              >
                {expanded ? "Show less" : "Show full error"}
              </button>
            ) : null}
          </>
        ) : null}
      </td>
      <td
        className="px-4 py-3 font-mono text-[11px] whitespace-nowrap"
        style={{ color: "var(--fg-muted)" }}
      >
        {delivery.last_attempt_at ? formatTime(delivery.last_attempt_at) : "—"}
      </td>
    </tr>
  );
}

// Absolute local time rather than a relative duration: relative times are the
// thing that goes negative under clock skew, and an operator correlating with
// their own logs wants a timestamp anyway.
function formatTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}
