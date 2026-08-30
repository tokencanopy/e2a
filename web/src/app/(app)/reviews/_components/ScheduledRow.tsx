"use client";

// One row in the Pending page's "Scheduled" tab: an outbound message accepted
// and waiting for a future send_at to fire. Collapsed it reads like an inbox row
// (avatar · subject · inbox → recipient · Sends-time chip). Clicking expands it
// READ-ONLY — a scheduled send is NOT a hold, so there is nothing to approve or
// reject; the expansion just shows what will go out (recipients + body) and
// when. The body fetch is lazy (GET /v1/agents/{email}/messages/{id}), cached
// under the shared messageDetailKey.

import { useState } from "react";
import useSWR from "swr";
import { messageDetailKey } from "../../../../lib/swrKeys";
import {
  getMessageDetailWire,
  projectPending,
} from "../../../components/onboarding/api";
import type { PendingMessageSummary } from "../../../components/types";
import { Chip } from "@e2a/ui";
import { formatScheduledSend } from "../../../../lib/scheduledTime";
import { joinCSV } from "./edits";
import { EmailHtmlBody } from "../../../components/messages/EmailHtmlBody";

function initialsFor(email: string): string {
  const local = email.split("@")[0] || email;
  return local.slice(0, 2).toUpperCase();
}

// Stable hue from the agent address so each inbox keeps one avatar color.
function hueFor(email: string): number {
  let h = 0;
  for (let i = 0; i < email.length; i++) {
    h = (h * 31 + email.charCodeAt(i)) % 360;
  }
  return h;
}

export function ScheduledRow({ summary }: { summary: PendingMessageSummary }) {
  const agentEmail = summary.agent_email;
  const id = summary.id;
  const hue = hueFor(agentEmail);
  const sendsLabel = formatScheduledSend(summary.scheduled_at);
  const [expanded, setExpanded] = useState(false);
  const [hovered, setHovered] = useState(false);

  // Lazy: only fetch the full message once the row is opened. Shares the wire
  // cache with the inbox thread + review surfaces (messageDetailKey).
  const { data: wire, error, isLoading } = useSWR(
    expanded ? messageDetailKey(id) : null,
    () => getMessageDetailWire(agentEmail, id),
  );
  const msg = wire ? projectPending(agentEmail, wire) : null;

  return (
    <div style={{ borderBottom: "1px solid var(--border)" }}>
      <button
        type="button"
        data-testid="scheduled-row"
        onClick={() => setExpanded((e) => !e)}
        onMouseEnter={() => setHovered(true)}
        onMouseLeave={() => setHovered(false)}
        className="flex items-center gap-3 text-left w-full"
        style={{
          padding: "12px 16px",
          background: expanded || hovered ? "var(--bg-elev)" : "transparent",
        }}
        aria-expanded={expanded}
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
          <span
            className="block text-[13px] font-semibold truncate"
            style={{ color: "var(--fg)" }}
          >
            {summary.subject || "(no subject)"}
          </span>
          <span
            className="block font-mono text-[11px] truncate"
            style={{ color: "var(--fg-subtle)" }}
          >
            {agentEmail} → {(summary.to ?? [])[0] || "—"}
            {summary.to && summary.to.length > 1 && ` +${summary.to.length - 1}`}
          </span>
        </span>
        {sendsLabel && <Chip tone="success">{sendsLabel}</Chip>}
        <span
          aria-hidden
          className="shrink-0 text-[11px]"
          style={{ color: "var(--fg-subtle)" }}
        >
          {expanded ? "▾" : "▸"}
        </span>
      </button>

      {expanded && (
        <div style={{ padding: "0 16px 16px" }}>
          {isLoading && !msg ? (
            <p className="text-[13px] py-4" style={{ color: "var(--fg-muted)" }}>
              Loading message…
            </p>
          ) : !msg ? (
            <p
              className="text-[13px] py-4"
              style={{ color: "var(--danger-strong)" }}
            >
              {error instanceof Error ? error.message : "Message not found."}
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
              {sendsLabel && (
                <p
                  className="text-[12px] px-4 py-2"
                  style={{ background: "var(--success-bg)", color: "var(--success-strong)" }}
                >
                  {sendsLabel} — queued to send automatically. This is a preview;
                  it has not been sent yet.
                </p>
              )}
              <div className="px-4 py-3">
                <div
                  className="font-mono text-[11px] mb-2 grid gap-0.5"
                  style={{ color: "var(--fg-subtle)" }}
                >
                  <span>from {msg.agent_email}</span>
                  <span>to {joinCSV(msg.to) || "—"}</span>
                  {joinCSV(msg.cc) && <span>cc {joinCSV(msg.cc)}</span>}
                  {msg.conversation_id && (
                    <span>conversation {msg.conversation_id}</span>
                  )}
                </div>
                {msg.body_html && msg.body_html.trim() !== "" ? (
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
            </div>
          )}
        </div>
      )}
    </div>
  );
}
