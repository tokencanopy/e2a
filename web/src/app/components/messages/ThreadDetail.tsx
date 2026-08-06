"use client";

// Right column of the dashboard inbox. Renders the selected thread's
// header (conv chip + state + meta + subject + between-line) and the
// chat-log of bubbles below it. Newest-at-bottom — the spec uses
// reverse-chronological in the artboard, but for an email-client
// reading flow the conversation reads oldest→newest top→bottom.

import { Chip, Dot } from "@e2a/ui";
import { CounterpartyAvatar } from "./CounterpartyAvatar";
import { ThreadBubble } from "./ThreadBubble";
import { PendingCallout } from "./PendingCallout";
import { formatRelativeAge } from "../../../lib/relativeTime";
import type { MessageSummary } from "../types";
import type { Thread } from "./threading";

const STATE_CHIP: Record<Thread["state"], { tone: "warn" | "info" | "accent" | "neutral"; label: string; dot: boolean }> = {
  pending: { tone: "warn", label: "Pending review", dot: true },
  active: { tone: "info", label: "Active", dot: false },
  "handed-off": { tone: "accent", label: "Handed off", dot: false },
  closed: { tone: "neutral", label: "Closed", dot: false },
};

export function ThreadDetail({
  thread,
  agentEmail,
  onBack,
  onOpenMessage,
  historyIncomplete = false,
}: {
  thread: Thread | null;
  agentEmail: string;
  /** Return to the inbox list (clears the selected-thread hash). */
  onBack: () => void;
  // Only the pending-review callout navigates away (to the approve/reject
  // surface). Reading a message never leaves the conversation — bodies
  // render inline in the bubbles.
  onOpenMessage: (message: MessageSummary) => void;
  /** More message pages remain, so the loaded count is a lower bound. */
  historyIncomplete?: boolean;
}) {
  if (!thread) {
    return (
      <div
        data-testid="thread-detail-empty"
        className="flex flex-col items-center justify-center text-center"
        style={{
          background: "var(--bg)",
          padding: "0 24px",
          color: "var(--fg-muted)",
          fontSize: 13,
        }}
      >
        Select a conversation from the inbox.
      </div>
    );
  }

  const stateChip = STATE_CHIP[thread.state];
  const pendingDraft = thread.messages.find(
    (m) => m.review_status === "pending_review",
  );

  return (
    <div
      data-testid="thread-detail"
      className="flex-1 flex flex-col min-w-0"
      style={{ background: "var(--bg)" }}
    >
      {/* Header. Not pinned — it scrolls away with the thread like Gmail's
          conversation view. (The old viewport-locked layout stranded it
          above a nested scroller, which is what produced the second
          scrollbar / can't-scroll-past-header behavior.) */}
      <div
        style={{
          padding: "18px 28px",
          borderBottom: "1px solid var(--border)",
          background: "var(--bg-panel)",
        }}
      >
        <button
          type="button"
          onClick={onBack}
          className="inline-flex items-center hover:opacity-80 transition"
          style={{
            gap: 6,
            marginBottom: 12,
            fontSize: 12,
            color: "var(--fg-muted)",
            background: "transparent",
            border: "none",
            cursor: "pointer",
            padding: 0,
          }}
        >
          <span aria-hidden>←</span> Inbox
        </button>
        <div className="flex items-center min-w-0" style={{ gap: 8, marginBottom: 10 }}>
          {thread.conversationId && (
            <code
              style={{
                fontFamily: "var(--f-mono)",
                fontSize: 11,
                color: "var(--fg-muted)",
                background: "var(--bg-elev)",
                padding: "1px 6px",
                borderRadius: 4,
                border: "1px solid var(--border-sub)",
                minWidth: 0,
                wordBreak: "break-all",
              }}
            >
              {thread.conversationId}
            </code>
          )}
          <Chip tone={stateChip.tone}>
            {stateChip.dot && <Dot tone={stateChip.tone === "warn" ? "warn" : "neutral"} />}
            {stateChip.label}
          </Chip>
          <span className="flex-1" />
          <span
            style={{
              fontFamily: "var(--f-mono)",
              fontSize: 11,
              color: "var(--fg-subtle)",
            }}
          >
            {thread.msgCount}
            {historyIncomplete ? "+" : ""}{" "}
            {thread.msgCount === 1 && !historyIncomplete
              ? "message"
              : "messages"}{" "}
            ·
            started {formatRelativeAge(thread.startedAt)}
          </span>
        </div>
        <h2
          style={{
            fontFamily: "var(--f-ui)",
            fontSize: 22,
            fontWeight: 700,
            letterSpacing: "-0.012em",
            color: "var(--fg)",
            margin: 0,
          }}
        >
          {thread.subject}
        </h2>
        <div
          className="flex items-center"
          style={{
            marginTop: 8,
            gap: 10,
            fontFamily: "var(--f-mono)",
            fontSize: 12,
            color: "var(--fg-muted)",
          }}
        >
          <CounterpartyAvatar
            email={thread.counterparty.email}
            name={thread.counterparty.name}
            size={20}
          />
          <span>
            <span style={{ color: "var(--fg-subtle)" }}>between</span>{" "}
            {agentEmail}{" "}
            <span style={{ color: "var(--fg-subtle)" }}>↔</span>{" "}
            {thread.counterparty.email}
          </span>
        </div>
      </div>

      {/* Body: chat log. Flows in the page's single scroll — no internal
          overflow container (that was half of the two-scrollbar bug). */}
      <div
        className="flex-1"
        style={{ padding: "24px 28px 28px" }}
      >
        {thread.messages.map((m) => (
          <ThreadBubble key={m.id} message={m} agentEmail={agentEmail} />
        ))}

        {pendingDraft && (
          <PendingCallout
            draftedBy="inbox"
            onReview={() => onOpenMessage(pendingDraft)}
          />
        )}
      </div>
    </div>
  );
}
