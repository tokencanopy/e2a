import { Chip, Dot, type ChipTone } from "@e2a/ui";

export type MessageStatusInput = {
  direction: "inbound" | "outbound";
  /** Outbound delivery lifecycle. Empty on inbound. */
  delivery_status?: string;
  /** Outbound HITL/send lifecycle. Empty on inbound. */
  review_status?: string;
  /** Inbound unread→read marker. Empty on outbound. */
  inbox_status?: string;
  /**
   * Future instant a scheduled outbound send is queued to be submitted
   * (outbound only). The feature deliberately introduces no new
   * delivery_status, so a scheduled send stays "accepted"; this timestamp is
   * what distinguishes it from an ordinary immediate send.
   */
  scheduled_at?: string;
};

export type MessageStatusSpec = {
  tone: ChipTone;
  label: string;
  dot?: boolean;
  /** Render a clock glyph before the label (scheduled sends). */
  clock?: boolean;
  attention: boolean;
};

// A scheduled send only reads as "Scheduled" while its instant is still in the
// future. Once it fires the row advances to sending/sent/… (handled by other
// delivery_status cases, which win); a past-but-still-"accepted" row (the
// scheduler's brief poll window) falls back to "Queued". scheduled_at is
// retained on the row after the send, so we must gate on the future check —
// not merely on the field being present.
export function isFutureScheduled(m: MessageStatusInput): boolean {
  if (m.direction !== "outbound" || !m.scheduled_at) return false;
  const t = new Date(m.scheduled_at).getTime();
  return !Number.isNaN(t) && t > Date.now();
}

export function deriveStatusChip(m: MessageStatusInput): MessageStatusSpec | null {
  if (m.direction === "inbound") {
    if (m.inbox_status === "unread") {
      return { tone: "info", label: "Unread", attention: false };
    }
    return { tone: "neutral", label: "Read", attention: false };
  }

  switch (m.review_status) {
    case "pending_review":
      return {
        tone: "warn",
        label: "Pending review",
        dot: true,
        attention: true,
      };
    case "review_rejected":
      return { tone: "danger", label: "Rejected", attention: true };
    case "review_expired_rejected":
      return { tone: "danger", label: "Auto-rejected", attention: true };
  }

  if (m.delivery_status) {
    switch (m.delivery_status) {
      case "accepted":
        // Reuse the existing `info` tone (Josh: use an existing color); the
        // clock glyph + label + the "Sends …" time carry the distinction from
        // an immediate "Queued".
        if (isFutureScheduled(m)) {
          return { tone: "info", label: "Scheduled", clock: true, attention: true };
        }
        return { tone: "info", label: "Queued", attention: true };
      case "sending":
        return { tone: "info", label: "Sending", attention: true };
      case "deferred":
        return { tone: "warn", label: "Delayed", attention: true };
      case "failed":
        return { tone: "danger", label: "Failed", attention: true };
      case "bounced":
        return { tone: "danger", label: "Bounced", attention: true };
      case "complained":
        return { tone: "danger", label: "Complaint", attention: true };
      case "sent":
        return { tone: "success", label: "Sent", attention: false };
      case "delivered":
        return { tone: "success", label: "Delivered", attention: false };
      default:
        return {
          tone: "neutral",
          label: m.delivery_status,
          attention: false,
        };
    }
  }

  switch (m.review_status) {
    case "sent":
      return { tone: "success", label: "Sent", attention: false };
    case "review_expired_approved":
      return { tone: "success", label: "Sent (auto)", attention: false };
    default:
      return null;
  }
}

// Small clock glyph for the "Scheduled" chip. Inline SVG (the app has no icon
// dependency) using currentColor so it inherits the chip's tone, mirroring the
// MessageDirectionIcon pattern.
function ClockGlyph() {
  return (
    <svg
      aria-hidden
      width={11}
      height={11}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      style={{ marginRight: 3, flexShrink: 0 }}
    >
      <circle cx="12" cy="12" r="9" />
      <path d="M12 7v5l3 2" />
    </svg>
  );
}

export function MessageStatusChip({
  className,
  ...props
}: MessageStatusInput & { className?: string }) {
  const spec = deriveStatusChip(props);
  if (!spec) return null;

  const dotTone =
    spec.tone === "warn"
      ? "warn"
      : spec.tone === "danger"
        ? "danger"
        : null;

  return (
    <Chip tone={spec.tone} className={className}>
      {spec.dot && dotTone && <Dot tone={dotTone} />}
      {spec.clock && <ClockGlyph />}
      {spec.label}
    </Chip>
  );
}
