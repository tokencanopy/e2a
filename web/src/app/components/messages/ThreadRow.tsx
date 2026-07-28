"use client";

// One row in the Gmail-style inbox list: a single dense line —
// sender · subject — preview ……… [pending] [count] time. Unread threads
// render bold. Clicking opens the conversation full-width.

import { useState } from "react";
import { CounterpartyAvatar } from "./CounterpartyAvatar";
import { MessageStatusChip, deriveStatusChip } from "./MessageStatusChip";
import { formatRelativeAge } from "../../../lib/relativeTime";
import { formatScheduledSend } from "../../../lib/scheduledTime";
import type { Thread } from "./threading";

export function ThreadRow({
  thread,
  active,
  onSelect,
}: {
  thread: Thread;
  active: boolean;
  onSelect: (key: string) => void;
}) {
  // Unread = any inbound message still marked unread. v1 carries inbound
  // read state in read_status (delivery_status is outbound-only). Drives
  // Gmail's bold row.
  const unread = thread.messages.some(
    (m) => m.direction === "inbound" && m.read_status === "unread",
  );
  const pending = thread.state === "pending";
  const latest = thread.messages[thread.messages.length - 1];
  const latestStatus =
    latest?.direction === "outbound"
      ? deriveStatusChip({
          direction: "outbound",
          delivery_status: latest.status,
          review_status: latest.review_status,
          scheduled_at: latest.scheduled_at,
        })
      : null;
  const scheduled = latestStatus?.label === "Scheduled";
  const fw = unread ? 600 : 400;
  // Hover highlight via state, not a `hover:bg-*` class: the inline
  // `background` below (active/unread tinting) would otherwise win over a
  // Tailwind hover utility and the highlight would never show.
  const [hovered, setHovered] = useState(false);

  return (
    <div
      data-testid="thread-row"
      data-thread-key={thread.key}
      data-selected={active ? "true" : "false"}
      role="button"
      tabIndex={0}
      onClick={() => onSelect(thread.key)}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSelect(thread.key);
        }
      }}
      onMouseEnter={() => setHovered(true)}
      onMouseLeave={() => setHovered(false)}
      className="flex items-center transition"
      style={{
        gap: 12,
        padding: "10px 18px",
        borderBottom: "1px solid var(--border-sub)",
        background:
          active || hovered
            ? "var(--bg-elev)"
            : unread
              ? "var(--bg-panel)"
              : "transparent",
        boxShadow: active ? "inset 2px 0 0 var(--accent)" : "none",
        cursor: "pointer",
      }}
    >
      <CounterpartyAvatar
        email={thread.counterparty.email}
        name={thread.counterparty.name}
        size={26}
      />

      {/* Sender — fixed-ish column so subjects line up like Gmail. */}
      <span
        style={{
          fontSize: 13,
          fontWeight: fw,
          color: "var(--fg)",
          width: 170,
          flexShrink: 0,
          whiteSpace: "nowrap",
          overflow: "hidden",
          textOverflow: "ellipsis",
        }}
      >
        {thread.counterparty.name}
        {thread.msgCount > 1 && (
          <span style={{ color: "var(--fg-subtle)", fontWeight: 400 }}>
            {" "}
            {thread.msgCount}
          </span>
        )}
      </span>

      {/* Subject — preview, single line, takes the remaining width. */}
      <span className="flex-1 min-w-0" style={{ fontSize: 13, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>
        <span style={{ color: "var(--fg)", fontWeight: fw }}>{thread.subject}</span>
        {thread.lastPreview && thread.lastPreview !== thread.subject && (
          <span style={{ color: "var(--fg-subtle)" }}> — {thread.lastPreview}</span>
        )}
      </span>

      {/* Right meta: attention status + timestamp. */}
      {latestStatus?.attention && (
        <MessageStatusChip
          className="shrink-0 whitespace-nowrap"
          direction="outbound"
          delivery_status={latest.status}
          review_status={latest.review_status}
          scheduled_at={latest.scheduled_at}
        />
      )}
      {scheduled && (
        <span
          className="shrink-0"
          style={{
            fontSize: 11,
            color: "var(--fg-subtle)",
            whiteSpace: "nowrap",
          }}
        >
          {formatScheduledSend(latest.scheduled_at)}
        </span>
      )}
      {pending && !latestStatus?.attention && (
        <span
          className="shrink-0"
          style={{
            fontSize: 10,
            fontWeight: 600,
            color: "var(--warn-strong)",
            background: "var(--warn-bg)",
            borderRadius: 999,
            padding: "1px 8px",
            whiteSpace: "nowrap",
          }}
        >
          Pending
        </span>
      )}
      <span
        className="shrink-0"
        style={{
          fontFamily: "var(--f-mono)",
          fontSize: 11,
          color: unread ? "var(--fg)" : "var(--fg-subtle)",
          fontWeight: unread ? 600 : 400,
          whiteSpace: "nowrap",
          minWidth: 52,
          textAlign: "right",
        }}
      >
        {formatRelativeAge(thread.lastMessageAt)}
      </span>
    </div>
  );
}
