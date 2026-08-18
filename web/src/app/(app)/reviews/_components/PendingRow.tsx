"use client";

// One accordion row in the converged Pending review list. Collapsed it
// reads like an inbox row (avatar · subject · inbox → recipient · queued
// age). Expanded it shows the agent-drafted reply READ-FIRST (body +
// Details) with an action bar — Approve & send / Edit draft / Reject. The
// full field editor is opt-in via "Edit draft", so the common path
// (glance + approve) stays one interaction and the editor never clutters
// the list. Replaces the old master-detail PendingDetailPanel.
//
// The body/recipients fetch is lazy — it fires only when the row is
// expanded (GET /v1/reviews/{id}). Approve/reject reuse the diff-only
// edit helpers so untouched fields keep their agent-authored original.

import { useEffect, useMemo, useRef, useState } from "react";
import useSWR from "swr";
import { messageDetailKey } from "../../../../lib/swrKeys";
import {
  approvePendingMessage,
  getReviewDetailWire,
  projectPending,
  rejectPendingMessage,
} from "../../../components/onboarding/api";
import type {
  PendingMessageDetail,
  PendingMessageSummary,
} from "../../../components/types";
import { Chip, Dot } from "@e2a/ui";
import { diffApproveEdits, joinCSV } from "./edits";
import { categoryLabel, holdReasonSummary } from "./reviewReason";
import { MessageLifecycleData } from "../../../components/messages/MessageLifecycleTimeline";
import { EmailHtmlBody } from "../../../components/messages/EmailHtmlBody";

function formatQueuedAgo(iso: string): string {
  const sec = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
  if (sec < 60) return `${sec}s ago`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ago`;
  const h = Math.floor(min / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

// A held outbound draft can carry a future send_at (#815): the schedule survives
// the hold, so the reviewer sees when it will send once approved. Renders the
// local date-time; if the instant has already passed, approval sends immediately.
function formatScheduledFor(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  if (at.getTime() <= Date.now()) return "Sends on approval";
  return `Sends ${at.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  })}`;
}

function initialsFor(email: string): string {
  const local = email.split("@")[0] || email;
  return (
    local
      .split(/[.\-_]/)
      .slice(0, 2)
      .map((s) => s.charAt(0).toUpperCase())
      .join("") || "?"
  );
}

function hueFor(email: string): number {
  const local = email.split("@")[0] || email;
  let hash = 0;
  for (let i = 0; i < local.length; i++)
    hash = (hash * 31 + local.charCodeAt(i)) | 0;
  return Math.abs(hash) % 360;
}

const inputStyle: React.CSSProperties = {
  background: "var(--bg-panel)",
  border: "1px solid var(--border)",
  borderRadius: "var(--r-md)",
  color: "var(--fg)",
};

const fieldLabel = "block text-[11px] font-medium mb-1";

export function PendingRow({
  summary,
  expanded,
  onToggle,
  onResolved,
}: {
  summary: PendingMessageSummary;
  expanded: boolean;
  onToggle: () => void;
  // Called after a successful approve/reject so the parent can refetch
  // the queue, collapse this row, and update the sidebar badge.
  onResolved: () => void;
}) {
  const agentEmail = summary.agent_email;
  const id = summary.id;
  // "Why held" line, computed once (pure; stable within a render).
  const reasonLabel = holdReasonSummary(summary.hold_reason);
  // Inbound holds are screened *incoming* messages: approve = release to
  // the inbox, reject = block. They aren't editable drafts, so the editor
  // and "& send" wording are outbound-only.
  const isInbound = summary.direction === "inbound";

  // Lazy: only fetch the body/recipients once the row is open. Caches the
  // RAW wire under the shared per-message key — inbox thread bubbles read the
  // same entry, so both must agree on the shape (see lib/swrKeys.ts) —
  // and projects below. Uses the review endpoint, the only one that
  // carries `hold_reason`/`protection`.
  const { data: wire, error: fetchError, isLoading } = useSWR(
    expanded ? messageDetailKey(id) : null,
    () => getReviewDetailWire(id),
    { keepPreviousData: false },
  );

  const msg: PendingMessageDetail | undefined = useMemo(
    () => (wire ? projectPending(agentEmail, wire) : undefined),
    [wire, agentEmail],
  );

  const [editing, setEditing] = useState(false);
  // Flips once the reviewer opens the editor or types, so a background
  // revalidation can't stomp an in-progress edit.
  const hasUserEditedRef = useRef(false);
  const [showDetails, setShowDetails] = useState(false);
  const [showLifecycle, setShowLifecycle] = useState(false);
  const [showScreeningDetails, setShowScreeningDetails] = useState(false);
  const [rejectOpen, setRejectOpen] = useState(false);

  const [subject, setSubject] = useState("");
  const [bodyText, setBodyText] = useState("");
  const [bodyHTML, setBodyHTML] = useState("");
  const [to, setTo] = useState("");
  const [cc, setCC] = useState("");
  const [bcc, setBCC] = useState("");
  const [rejectReason, setRejectReason] = useState("");

  const [approving, setApproving] = useState(false);
  const [rejecting, setRejecting] = useState(false);
  const [actionError, setActionError] = useState("");
  const [hovered, setHovered] = useState(false);

  // Seed the editor from the loaded draft. This is an EFFECT, not SWR's
  // onSuccess, because onSuccess does not fire when the value is served from
  // cache — and this row shares its per-message entry with inbox threads, so
  // a warm entry is the common case. Seeding only on a real fetch left the
  // fields empty, and diffApproveEdits then read "" as a deliberate edit and
  // sent it: approving blanked the subject, body and recipients.
  useEffect(() => {
    if (hasUserEditedRef.current || !msg) return;
    setSubject(msg.subject ?? "");
    setBodyText(msg.body_text ?? "");
    setBodyHTML(msg.body_html ?? "");
    setTo(joinCSV(msg.to));
    setCC(joinCSV(msg.cc));
    setBCC(joinCSV(msg.bcc));
  }, [msg]);

  // Collapsing resets the transient edit/reject UI so reopening starts
  // clean (and an in-progress edit on a different row can't leak here).
  useEffect(() => {
    if (!expanded) {
      hasUserEditedRef.current = false;
      setEditing(false);
      setRejectOpen(false);
      setShowDetails(false);
      setShowLifecycle(false);
      setShowScreeningDetails(false);
      setActionError("");
    }
  }, [expanded]);

  const busy = approving || rejecting;
  const error =
    actionError || (fetchError ? fetchError.message || "Failed to load" : "");

  const handleApprove = async () => {
    if (!msg) return;
    setApproving(true);
    setActionError("");
    try {
      // Only an open editor can override anything. Untouched, the
      // agent-authored draft is sent exactly as written — belt and braces
      // alongside the seeding effect above.
      const overrides = editing
        ? diffApproveEdits(msg, { subject, bodyText, bodyHTML, to, cc, bcc })
        : {};
      await approvePendingMessage(agentEmail, id, overrides);
      onResolved();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : "Approve failed");
      setApproving(false);
    }
  };

  const handleReject = async () => {
    if (!msg) return;
    setRejecting(true);
    setActionError("");
    try {
      await rejectPendingMessage(agentEmail, id, rejectReason);
      onResolved();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : "Reject failed");
      setRejecting(false);
    }
  };

  const hue = hueFor(agentEmail);
  const notPending = !!msg && msg.status !== "pending_review";
  const holdReason = msg?.hold_reason ?? summary.hold_reason;
  const scanFinding = msg?.protection?.find((finding) => finding.source === "scan");
  const confidence = holdReason?.confidence;
  const hasValidConfidence =
    typeof confidence === "number" &&
    Number.isFinite(confidence) &&
    confidence >= 0 &&
    confidence <= 1;
  const hasScreeningDetails =
    holdReason?.type === "scan" &&
    (hasValidConfidence || !!holdReason.category || !!scanFinding?.detector);

  return (
    <div
      data-testid="pending-row"
      data-pending-id={id}
      style={{ borderBottom: "1px solid var(--border-sub)" }}
    >
      {/* Collapsed header — always rendered; toggles expansion. */}
      <button
        onClick={onToggle}
        aria-expanded={expanded}
        onMouseEnter={() => setHovered(true)}
        onMouseLeave={() => setHovered(false)}
        className="w-full text-left px-4 py-3 flex items-center gap-3 transition cursor-pointer"
        style={{
          background:
            expanded || hovered ? "var(--bg-elev)" : "transparent",
        }}
      >
        <span
          className="flex items-center justify-center font-mono text-[10px] font-semibold shrink-0"
          style={{
            width: 28,
            height: 28,
            borderRadius: "50%",
            background: `hsl(${hue}, 45%, 35%)`,
            color: "#fff",
          }}
        >
          {initialsFor(agentEmail)}
        </span>
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-2">
            <span
              className="text-[13px] font-semibold truncate"
              style={{ color: "var(--fg)" }}
            >
              {summary.subject || "(no subject)"}
            </span>
          </span>
          <span
            className="block font-mono text-[11px] truncate"
            style={{ color: "var(--fg-subtle)" }}
          >
            {summary.direction === "inbound" ? (
              <>
                {summary.from || "—"} → {agentEmail}
              </>
            ) : (
              <>
                {agentEmail} → {(summary.to ?? [])[0] || "—"}
                {summary.to && summary.to.length > 1 && ` +${summary.to.length - 1}`}
              </>
            )}
          </span>
          {/* The API-provided explanation is always visible when present. */}
          {reasonLabel && (
            <span
              className="flex items-center gap-1 text-[11px] mt-0.5 truncate"
              style={{ color: "var(--warn-strong)" }}
              title={reasonLabel}
            >
              <span aria-hidden>⚑</span>
              <span className="truncate">{reasonLabel}</span>
            </span>
          )}
        </span>
        <Chip tone={summary.direction === "inbound" ? "info" : "neutral"}>
          {summary.direction === "inbound" ? "Inbound" : "Outbound"}
        </Chip>
        <Chip tone="warn">
          <Dot tone="warn" /> Pending
        </Chip>
        {summary.scheduled_at && formatScheduledFor(summary.scheduled_at) && (
          <Chip tone="info">{formatScheduledFor(summary.scheduled_at)}</Chip>
        )}
        <span
          className="font-mono text-[11px] shrink-0"
          style={{ color: "var(--fg-subtle)" }}
        >
          {formatQueuedAgo(summary.created_at)}
        </span>
        <span
          aria-hidden
          className="shrink-0 text-[11px]"
          style={{ color: "var(--fg-subtle)" }}
        >
          {expanded ? "▾" : "▸"}
        </span>
      </button>

      {/* Expanded detail */}
      {expanded && (
        <div style={{ padding: "0 16px 16px" }}>
          {isLoading && !msg ? (
            <p className="text-[13px] py-4" style={{ color: "var(--fg-muted)" }}>
              Loading draft…
            </p>
          ) : !msg ? (
            <p className="text-[13px] py-4" style={{ color: "var(--danger-strong)" }}>
              {error || "Message not found."}
            </p>
          ) : (
            <div
              style={{
                background: "var(--bg-panel)",
                border: "1px solid var(--border-sub)",
                borderRadius: "var(--r-md)",
                overflow: "hidden",
              }}
            >
              {notPending && (
                <p
                  className="text-[12px] px-4 py-2"
                  style={{
                    background: "var(--warn-bg)",
                    color: "var(--warn-strong)",
                  }}
                >
                  This message is no longer pending ({msg.status}). It may
                  have been resolved elsewhere or its review hold expired.
                </p>
              )}

              {holdReason && (
                <div
                  className="text-[12px] px-4 py-3"
                  style={{
                    background: "var(--warn-bg)",
                    color: "var(--warn-strong)",
                  }}
                >
                  <p className="font-semibold">Why this message was held</p>
                  <p className="mt-0.5">
                    {holdReason.category ? (
                      <strong>{categoryLabel(holdReason.category)} — </strong>
                    ) : null}
                    {holdReason.detail || holdReason.summary}
                  </p>
                  {hasScreeningDetails && (
                    <div className="mt-1.5">
                      <button
                        type="button"
                        onClick={() => setShowScreeningDetails((shown) => !shown)}
                        className="font-mono text-[11px] cursor-pointer hover:underline"
                        aria-expanded={showScreeningDetails}
                      >
                        Screening details {showScreeningDetails ? "▴" : "▾"}
                      </button>
                      {showScreeningDetails && (
                        <div className="font-mono text-[11px] mt-1 grid gap-0.5">
                          {holdReason.category && (
                            <span>category {holdReason.category}</span>
                          )}
                          {scanFinding?.detector && (
                            <span>detector {scanFinding.detector}</span>
                          )}
                          {hasValidConfidence && (
                            <span>confidence {confidence.toFixed(2)}</span>
                          )}
                        </div>
                      )}
                    </div>
                  )}
                </div>
              )}

              {!editing ? (
                /* READ-FIRST view */
                <div className="px-4 py-3">
                  <div className="flex items-center justify-between gap-2 mb-2">
                    <span
                      className="font-mono text-[11px] truncate"
                      style={{ color: "var(--fg-muted)" }}
                    >
                      {isInbound
                        ? `from ${summary.from || "—"}`
                        : `to ${joinCSV(msg.to) || "—"}`}
                    </span>
                    <div className="flex items-center gap-2 shrink-0">
                      <button
                        type="button"
                        onClick={() => setShowDetails((s) => !s)}
                        className="font-mono text-[11px] cursor-pointer hover:underline"
                        style={{ color: "var(--fg-subtle)" }}
                      >
                        Details {showDetails ? "▴" : "▾"}
                      </button>
                      <button
                        type="button"
                        onClick={() => setShowLifecycle((shown) => !shown)}
                        aria-expanded={showLifecycle}
                        className="font-mono text-[11px] cursor-pointer hover:underline"
                        style={{ color: "var(--accent-strong)" }}
                      >
                        {showLifecycle ? "Hide lifecycle ▴" : "Lifecycle ▾"}
                      </button>
                    </div>
                  </div>
                  {showDetails && (
                    <div
                      className="font-mono text-[11px] mb-2 grid gap-0.5"
                      style={{ color: "var(--fg-subtle)" }}
                    >
                      <span>from {msg.agent_email}</span>
                      <span>to {joinCSV(msg.to) || "—"}</span>
                      {joinCSV(msg.cc) && <span>cc {joinCSV(msg.cc)}</span>}
                      {joinCSV(msg.bcc) && <span>bcc {joinCSV(msg.bcc)}</span>}
                      {msg.conversation_id && (
                        <span>conversation {msg.conversation_id}</span>
                      )}
                    </div>
                  )}
                  {showLifecycle && (
                    <div
                      className="mb-3"
                      style={{
                        border: "1px solid var(--border-sub)",
                        borderRadius: "var(--r-md)",
                        overflow: "hidden",
                      }}
                    >
                      <MessageLifecycleData email={agentEmail} messageId={id} />
                    </div>
                  )}
                  {msg.body_html && msg.body_html.trim() !== "" ? (
                    // Render the way it would actually look in the recipient's
                    // inbox (Gmail-style) — sanitized + sandboxed, same
                    // component the inbox thread view uses.
                    <EmailHtmlBody
                      html={msg.body_html}
                      attachments={msg.attachments}
                      email={agentEmail}
                      messageId={id}
                    />
                  ) : (
                    <div
                      className="text-[13px] whitespace-pre-wrap"
                      style={{ color: "var(--fg)", lineHeight: 1.6 }}
                    >
                      {msg.body_text || "(empty body)"}
                    </div>
                  )}
                </div>
              ) : (
                /* EDITOR — opt-in via Edit draft */
                <div className="px-4 py-3 space-y-2.5">
                  <div>
                    <label className={fieldLabel} style={{ color: "var(--fg-muted)" }}>
                      Subject
                    </label>
                    <input
                      value={subject}
                      onChange={(e) => setSubject(e.target.value)}
                      className="w-full px-3 py-2 text-[13px]"
                      style={inputStyle}
                    />
                  </div>
                  <div className="grid grid-cols-1 sm:grid-cols-3 gap-2.5">
                    {([
                      ["To", to, setTo],
                      ["Cc", cc, setCC],
                      ["Bcc", bcc, setBCC],
                    ] as const).map(([label, val, set]) => (
                      <div key={label}>
                        <label className={fieldLabel} style={{ color: "var(--fg-muted)" }}>
                          {label}
                        </label>
                        <input
                          value={val}
                          onChange={(e) => set(e.target.value)}
                          className="w-full px-3 py-2 text-[13px] font-mono"
                          style={inputStyle}
                        />
                      </div>
                    ))}
                  </div>
                  <div>
                    <label className={fieldLabel} style={{ color: "var(--fg-muted)" }}>
                      Body
                    </label>
                    <textarea
                      value={bodyText}
                      onChange={(e) => setBodyText(e.target.value)}
                      rows={8}
                      className="w-full px-3 py-2 text-[13px]"
                      style={{ ...inputStyle, resize: "vertical", lineHeight: 1.6 }}
                    />
                  </div>
                </div>
              )}

              {/* Action bar */}
              {!notPending && (
                <div
                  className="px-4 py-2.5 flex items-center gap-2 flex-wrap"
                  style={{ borderTop: "1px solid var(--border-sub)" }}
                >
                  <button
                    onClick={handleApprove}
                    disabled={busy}
                    className="text-[13px] font-medium px-3.5 py-1.5 transition cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed hover:opacity-90"
                    style={{
                      background: "var(--accent-fill)",
                      color: "var(--accent-fg)",
                      borderRadius: "var(--r-md)",
                    }}
                  >
                    {approving
                      ? isInbound
                        ? "Releasing…"
                        : "Sending…"
                      : isInbound
                        ? "Approve & release"
                        : editing
                          ? "Approve & send edited"
                          : "Approve & send"}
                  </button>
                  {!isInbound &&
                    (!editing ? (
                    <button
                      onClick={() => {
                        hasUserEditedRef.current = true;
                        setEditing(true);
                      }}
                      disabled={busy}
                      className="text-[13px] px-3 py-1.5 transition cursor-pointer disabled:opacity-50 hover:bg-[var(--bg-elev)]"
                      style={{
                        background: "var(--bg-panel)",
                        color: "var(--fg)",
                        border: "1px solid var(--border)",
                        borderRadius: "var(--r-md)",
                      }}
                    >
                      Edit draft
                    </button>
                  ) : (
                    <button
                      onClick={() => setEditing(false)}
                      disabled={busy}
                      className="text-[13px] px-3 py-1.5 transition cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed hover:underline"
                      style={{ color: "var(--fg-muted)" }}
                    >
                      Cancel edit
                    </button>
                  ))}
                  <button
                    onClick={() => setRejectOpen((s) => !s)}
                    disabled={busy}
                    className="text-[13px] px-3 py-1.5 transition cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed hover:underline"
                    style={{ color: "var(--danger-strong)" }}
                  >
                    Reject {rejectOpen ? "▴" : "▾"}
                  </button>
                  <span className="flex-1" />
                  <code
                    className="font-mono text-[11px] hidden md:inline"
                    style={{ color: "var(--fg-subtle)" }}
                  >
                    client.reviews.approve(&quot;{id}&quot;)
                  </code>
                </div>
              )}

              {rejectOpen && !notPending && (
                <div
                  className="px-4 py-2.5 flex items-center gap-2 flex-wrap"
                  style={{ borderTop: "1px solid var(--border-sub)" }}
                >
                  <input
                    value={rejectReason}
                    onChange={(e) => setRejectReason(e.target.value)}
                    placeholder="reject reason (optional)"
                    className="flex-1 min-w-[160px] px-3 py-2 text-[13px]"
                    style={inputStyle}
                  />
                  <button
                    onClick={handleReject}
                    disabled={busy}
                    className="text-[13px] font-medium px-3.5 py-1.5 transition cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed hover:opacity-90"
                    style={{
                      background: "var(--danger-strong)",
                      color: "#fff",
                      borderRadius: "var(--r-md)",
                    }}
                  >
                    {rejecting ? "Rejecting…" : "Reject draft"}
                  </button>
                </div>
              )}

              {error && (
                <p
                  className="text-[12px] px-4 py-2"
                  style={{ color: "var(--danger-strong)" }}
                >
                  {error}
                </p>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
