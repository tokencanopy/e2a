"use client";

// Canonical observation timeline shown in conversation and Review details.

import { useState } from "react";
import useSWRInfinite from "swr/infinite";
import {
  getMessageLifecycle,
  type MessageLifecyclePageWire,
  type MessageLifecycleTransitionWire,
} from "../../../lib/messageLifecycle";
import { messageLifecycleKey } from "../../../lib/swrKeys";

type ReasonCode = MessageLifecycleTransitionWire["reason_code"];

export type LifecyclePresentation = { title: string; description: string };

export const LIFECYCLE_PRESENTATION: Record<ReasonCode, LifecyclePresentation> = {
  "acceptance.inbound_smtp": { title: "Received by e2a", description: "e2a received and saved this incoming message. It is waiting to be processed." },
  "acceptance.outbound_api": { title: "Accepted by e2a", description: "e2a accepted and saved this message. It is waiting for the next delivery step." },
  "acceptance.local_loopback": { title: "Accepted for local delivery", description: "e2a accepted this message for delivery to another e2a inbox." },
  "authentication.dmarc_pass": { title: "Sender authentication passed", description: "The sender's domain authentication checks passed." },
  "authentication.dmarc_fail": { title: "Sender authentication failed", description: "The message failed its sender-domain authentication check. Treat it with caution." },
  "authentication.dmarc_none": { title: "No DMARC policy found", description: "The sender's domain does not publish a DMARC policy, so this check could not confirm the sender." },
  "authentication.dmarc_temporary_error": { title: "Authentication temporarily unavailable", description: "e2a could not complete the sender authentication check because of a temporary error." },
  "authentication.dmarc_permanent_error": { title: "Authentication check failed", description: "e2a could not complete the sender authentication check." },
  "review.hold_created": { title: "Waiting for review", description: "This message is waiting for approval before e2a sends it." },
  "review.approved": { title: "Review approved", description: "A reviewer approved this message. e2a can now send it." },
  "review.rejected": { title: "Review rejected", description: "A reviewer rejected this message, so e2a will not send it." },
  "review.expired_approved": { title: "Automatically approved", description: "The review period expired, and the configured policy approved the message." },
  "review.expired_rejected": { title: "Automatically rejected", description: "The review period expired, and the configured policy rejected the message." },
  "suppression.recipient_blocked": { title: "Delivery blocked", description: "e2a did not send the message because this recipient is blocked." },
  "suppression.hard_bounce_applied": { title: "Recipient suppressed", description: "Future messages to this recipient were blocked after a permanent delivery failure." },
  "suppression.complaint_applied": { title: "Recipient suppressed", description: "Future messages to this recipient were blocked after a spam complaint." },
  "queue.inbound_processing": { title: "Queued for processing", description: "The incoming message is waiting for e2a to process it." },
  "queue.outbound_submission": { title: "Queued for delivery", description: "The message is waiting to be handed off to the delivery provider." },
  "submission.upstream_accepted": { title: "Handed off to delivery provider", description: "e2a successfully handed off the message, and the provider agreed to process it." },
  "submission.local_loopback_accepted": { title: "Handed off within e2a", description: "e2a accepted the message for delivery to another e2a inbox." },
  "submission.temporary_failure": { title: "Delivery handoff delayed", description: "e2a could not hand off the message because of a temporary error. It may be retried." },
  "submission.provider_rejected": { title: "Delivery provider rejected message", description: "The delivery provider refused the message, so it was not handed off." },
  "submission.local_retries_exhausted": { title: "Delivery failed", description: "e2a could not hand off the message after repeated attempts." },
  "submission.cancelled": { title: "Delivery cancelled", description: "Delivery was stopped before the message was handed off." },
  "submission.policy_budget_expired": { title: "Delivery failed", description: "The message waited for sending capacity for seven days and was not handed off." },
  "submission.sending_setup_expired": { title: "Delivery failed", description: "Sending setup for this account did not complete in time, so the message was not handed off." },
  "delivery.recipient_server_accepted": { title: "Accepted by recipient server", description: "The recipient's mail server accepted the message. This does not confirm inbox placement." },
  "delivery.temporary_delay": { title: "Delivery delayed", description: "The delivery provider reported a temporary delay." },
  "delivery.permanent_bounce": { title: "Delivery failed permanently", description: "The recipient's mail server permanently rejected the message." },
  "delivery.transient_bounce": { title: "Temporary delivery failure", description: "The provider reported a temporary delivery failure." },
  "delivery.undetermined_bounce": { title: "Delivery failed", description: "The provider reported a bounce but did not identify whether it was temporary or permanent." },
  "complaint.recipient_reported": { title: "Spam complaint received", description: "The recipient reported this message as spam. Future messages may be blocked." },
};

function lifecycleSummary(last: MessageLifecycleTransitionWire): string {
  switch (last.reason_code) {
    case "review.hold_created":
      return "Pending review";
    case "acceptance.inbound_smtp":
    case "acceptance.outbound_api":
    case "acceptance.local_loopback":
      return "Accepted · awaiting next observation";
    case "queue.inbound_processing":
      return "Queued · awaiting processing";
    case "queue.outbound_submission":
      return "Queued · awaiting submission";
    case "submission.temporary_failure":
    case "delivery.temporary_delay":
      return last.retryable ? "Delayed · retrying" : "Delayed";
    case "submission.upstream_accepted":
    case "submission.local_loopback_accepted":
      return "Sent";
    case "delivery.recipient_server_accepted":
      return "Delivered to recipient server";
    case "delivery.permanent_bounce":
    case "delivery.transient_bounce":
    case "delivery.undetermined_bounce":
      return "Bounced";
    case "complaint.recipient_reported":
      return "Complaint reported";
    case "review.rejected":
    case "review.expired_rejected":
    case "submission.provider_rejected":
    case "submission.local_retries_exhausted":
    case "submission.cancelled":
    case "submission.policy_budget_expired":
    case "submission.sending_setup_expired":
    case "suppression.recipient_blocked":
      return "Failed";
    default:
      return LIFECYCLE_PRESENTATION[last.reason_code].title;
  }
}

function observationTone(row: MessageLifecycleTransitionWire): string {
  if (["failed", "rejected", "blocked", "bounced", "reported"].includes(row.outcome)) {
    return "var(--danger-strong)";
  }
  if (row.outcome === "pending" || row.outcome === "deferred" || row.retryable) {
    return "var(--warn-strong)";
  }
  if (["passed", "approved", "delivered", "accepted"].includes(row.outcome)) {
    return "var(--success)";
  }
  return "var(--fg-muted)";
}

function formatTimestamp(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  return date.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
    second: "2-digit",
  });
}

function stableValue(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(stableValue);
  if (value !== null && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value as Record<string, unknown>)
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([key, nested]) => [key, stableValue(nested)]),
    );
  }
  return value;
}

function stableJSON(value: unknown): string {
  return JSON.stringify(stableValue(value));
}

export function formatLifecycleDiagnostics(row: MessageLifecycleTransitionWire): string {
  return [
    ["Transition ID", row.id],
    ["Message ID", row.message_id],
    ["Direction", row.direction],
    ["Stage", row.stage],
    ["Outcome", row.outcome],
    ["Reason code", row.reason_code],
    ["Occurred at", row.occurred_at],
    ["Retryable", String(row.retryable)],
    ["Reconstructed", String(row.reconstructed)],
    ["Recipient", row.recipient ?? ""],
    ["Evidence", stableJSON(row.evidence)],
    ["Correlation IDs", stableJSON(row.correlation_ids)],
  ].map(([label, value]) => `${label}: ${value}`).join("\n");
}

function DiagnosticMap({ title, values }: { title: string; values: Record<string, unknown> }) {
  const entries = Object.entries(values);
  if (entries.length === 0) return null;
  return (
    <div>
      <div style={{ color: "var(--fg-subtle)", marginBottom: 3 }}>{title}</div>
      {entries.map(([key, value]) => (
        <div key={key} className="flex" style={{ gap: 8 }}>
          <span style={{ color: "var(--fg-muted)" }}>{key}</span>
          <span style={{ color: "var(--fg)" }}>{typeof value === "string" ? value : JSON.stringify(value)}</span>
        </div>
      ))}
    </div>
  );
}

function DiagnosticField({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex" style={{ gap: 8 }}>
      <span style={{ color: "var(--fg-muted)" }}>{label}</span>
      <span style={{ color: "var(--fg)" }}>{value || "—"}</span>
    </div>
  );
}

function CanonicalObservation({ row, last }: { row: MessageLifecycleTransitionWire; last: boolean }) {
  const [diagnosticsOpen, setDiagnosticsOpen] = useState(false);
  const [copyStatus, setCopyStatus] = useState<"idle" | "copied" | "failed">("idle");
  const presentation = LIFECYCLE_PRESENTATION[row.reason_code];
  const copyDiagnostics = async () => {
    try {
      await navigator.clipboard.writeText(formatLifecycleDiagnostics(row));
      setCopyStatus("copied");
    } catch {
      setCopyStatus("failed");
    }
  };
  return (
    <li style={{ position: "relative", minWidth: 0 }}>
      {!last && (
        <span
          aria-hidden
          style={{
            position: "absolute",
            left: 11,
            right: -23,
            top: 5,
            height: 1,
            background: "var(--border-sub)",
          }}
        />
      )}
      <span
        aria-hidden
        style={{
          width: 11,
          height: 11,
          borderRadius: "50%",
          background: observationTone(row),
          boxShadow: last ? "0 0 0 3px var(--bg-elev)" : "none",
          display: "block",
          position: "relative",
        }}
      />
      <div style={{ minWidth: 0, marginTop: 10 }}>
        <div>
          <div style={{ fontSize: 12, fontWeight: last ? 650 : 500, color: "var(--fg)" }}>{presentation.title}</div>
          <div style={{ fontSize: 11, lineHeight: 1.45, color: "var(--fg-muted)", marginTop: 3 }}>
            {presentation.description}
          </div>
          <div style={{ fontFamily: "var(--f-mono)", fontSize: 10, color: "var(--fg-subtle)", marginTop: 4 }}>
            {formatTimestamp(row.occurred_at)}
          </div>
        </div>
        <button
          type="button"
          onClick={() => setDiagnosticsOpen((open) => !open)}
          aria-expanded={diagnosticsOpen}
          style={{
            marginTop: 5,
            padding: 0,
            border: 0,
            background: "transparent",
            color: "var(--accent-strong)",
            cursor: "pointer",
            fontFamily: "var(--f-mono)",
            fontSize: 10,
          }}
        >
          {diagnosticsOpen ? "Hide diagnostics" : "Diagnostics"}
        </button>
        {diagnosticsOpen && (
          <div
            style={{
              display: "grid",
              gap: 7,
              marginTop: 7,
              padding: "8px 10px",
              background: "var(--bg-elev)",
              border: "1px solid var(--border-sub)",
              borderRadius: "var(--r-sm)",
              fontFamily: "var(--f-mono)",
              fontSize: 10,
              lineHeight: 1.5,
              overflowWrap: "anywhere",
            }}
          >
            <div style={{ color: "var(--fg-muted)", fontFamily: "var(--f-sans)" }}>
              Include these details when filing feedback or reporting a bug.
            </div>
            <button
              type="button"
              onClick={() => void copyDiagnostics()}
              style={{
                justifySelf: "start",
                padding: 0,
                border: 0,
                background: "transparent",
                color: "var(--accent-strong)",
                cursor: "pointer",
                fontFamily: "inherit",
                fontSize: "inherit",
              }}
            >
              {copyStatus === "copied" ? "Copied" : copyStatus === "failed" ? "Copy failed — try again" : "Copy diagnostics"}
            </button>
            <div style={{ display: "grid", gap: 2 }}>
              <DiagnosticField label="Transition ID" value={row.id} />
              <DiagnosticField label="Message ID" value={row.message_id} />
              <DiagnosticField label="Direction" value={row.direction} />
              <DiagnosticField label="Stage" value={row.stage} />
              <DiagnosticField label="Outcome" value={row.outcome} />
              <DiagnosticField label="Reason code" value={row.reason_code} />
              <DiagnosticField label="Occurred at" value={row.occurred_at} />
              <DiagnosticField label="Retryable" value={String(row.retryable)} />
              <DiagnosticField label="Reconstructed" value={String(row.reconstructed)} />
              {row.reconstructed && <span style={{ color: "var(--fg-muted)" }}>Reconstructed from durable history</span>}
              <DiagnosticField label="Recipient" value={row.recipient ?? ""} />
            </div>
            <DiagnosticMap title="Evidence" values={row.evidence} />
            <DiagnosticMap title="Correlation IDs" values={row.correlation_ids} />
          </div>
        )}
      </div>
    </li>
  );
}

export function MessageLifecycleTimeline({
  transitions,
}: {
  transitions: MessageLifecycleTransitionWire[];
}) {
  if (transitions.length === 0) return null;
  return (
    <div data-testid="lifecycle-timeline" style={{ padding: "14px 18px 16px", position: "relative" }}>
      <div style={{ marginBottom: 13 }}>
        <div className="flex items-center" style={{ gap: 7, marginBottom: 5 }}>
          <span style={{ fontSize: 11, fontWeight: 650, color: "var(--fg)" }}>Lifecycle</span>
          <span
            style={{
              padding: "1px 4px",
              borderRadius: 3,
              background: "var(--info-bg)",
              color: "var(--info-strong)",
              fontFamily: "var(--f-mono)",
              fontSize: 8,
              fontWeight: 700,
              letterSpacing: "0.08em",
              textTransform: "uppercase",
            }}
          >
            Beta
          </span>
        </div>
        <div style={{ fontSize: 13, fontWeight: 650, color: "var(--fg)" }}>
          {lifecycleSummary(transitions[transitions.length - 1])}
        </div>
        <div style={{ marginTop: 2, fontFamily: "var(--f-mono)", fontSize: 10, color: "var(--fg-subtle)" }}>
          Observations recorded by e2a
        </div>
      </div>
      <ol
        aria-label="Message lifecycle observations"
        style={{
          display: "grid",
          gridAutoFlow: "column",
          gridAutoColumns: "minmax(210px, 1fr)",
          gap: 18,
          margin: 0,
          padding: "0 0 4px",
          listStyle: "none",
          overflowX: "auto",
          overflowY: "hidden",
        }}
      >
        {transitions.map((row, index) => (
          <CanonicalObservation key={row.id} row={row} last={index === transitions.length - 1} />
        ))}
      </ol>
    </div>
  );
}

export function MessageLifecycleData({
  email,
  messageId,
}: {
  email: string;
  messageId: string;
}) {
  const { data: pages, error, isLoading, isValidating, size, setSize } = useSWRInfinite<MessageLifecyclePageWire>(
    (pageIndex, previousPage) => {
      if (previousPage && previousPage.next_cursor === null) return null;
      return [
        ...messageLifecycleKey(email, messageId),
        pageIndex,
        previousPage?.next_cursor ?? null,
      ] as const;
    },
    ([, lifecycleEmail, lifecycleMessageId, , cursor]: readonly [
      string,
      string,
      string,
      number,
      string | null,
    ]) =>
      getMessageLifecycle(
        lifecycleEmail,
        lifecycleMessageId,
        cursor ? { cursor, limit: 100 } : { limit: 100 },
      ),
    { shouldRetryOnError: false, revalidateFirstPage: false },
  );
  const items = pages?.flatMap((page) => page.items) ?? [];
  const nextCursor = pages?.[pages.length - 1]?.next_cursor ?? null;

  if (isLoading) {
    return (
      <div role="status" style={{ padding: "14px 18px", fontSize: 12, color: "var(--fg-muted)" }}>
        Loading lifecycle…
      </div>
    );
  }
  if (error) {
    return (
      <div role="alert" style={{ padding: "14px 18px", fontSize: 12, color: "var(--danger-strong)" }}>
        Lifecycle unavailable. Try again shortly.
      </div>
    );
  }
  if (items.length === 0) {
    return (
      <div style={{ padding: "14px 18px", fontSize: 12, color: "var(--fg-muted)" }}>
        No lifecycle observations were recorded for this message.
      </div>
    );
  }
  return (
    <>
      <MessageLifecycleTimeline transitions={items} />
      {nextCursor && (
        <button
          type="button"
          onClick={() => void setSize(size + 1)}
          disabled={isValidating}
          style={{
            width: "100%",
            padding: "9px 18px",
            background: "var(--bg-elev)",
            border: 0,
            borderTop: "1px solid var(--border-sub)",
            color: "var(--accent-strong)",
            cursor: isValidating ? "default" : "pointer",
            fontFamily: "var(--f-mono)",
            fontSize: 10,
          }}
        >
          {isValidating ? "Loading more…" : "Load more observations"}
        </button>
      )}
    </>
  );
}
