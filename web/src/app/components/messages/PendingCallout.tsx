"use client";

// Dashed-accent cream card that appears under the bubbles when a thread
// has a pending outbound draft. Provides one-click navigation to the
// expanded row on the account-wide Review page.

export function PendingCallout({
  draftedBy,
  onReview,
}: {
  /** "agent:claude-sonnet-4-6" or similar — comes from the pending row's
   *  hint metadata. Today the wire doesn't carry this; the Review detail
   *  is the source of truth. The caller passes a best-effort label or
   *  the literal `agent` when unknown. */
  draftedBy: string;
  onReview: () => void;
}) {
  return (
    <div
      data-testid="pending-callout"
      className="flex items-center"
      style={{
        marginTop: 8,
        background: "var(--bg-panel)",
        border: "1px dashed var(--accent)",
        borderRadius: "var(--r-lg)",
        padding: "14px 16px",
        gap: 12,
      }}
    >
      <span
        aria-hidden
        className="inline-flex items-center justify-center"
        style={{
          width: 26,
          height: 26,
          borderRadius: 6,
          background: "var(--accent-soft)",
          color: "var(--accent-strong)",
          fontFamily: "var(--f-mono)",
          fontSize: 11,
          fontWeight: 700,
        }}
      >
        ⏸
      </span>
      <div className="flex-1 min-w-0">
        <div
          style={{
            fontSize: 13,
            fontWeight: 600,
            color: "var(--fg)",
          }}
        >
          Outbound reply waiting on your approval
        </div>
        <div style={{ fontSize: 12, color: "var(--fg-muted)" }}>
          Drafted by <span style={{ fontFamily: "var(--f-mono)" }}>{draftedBy}</span>
        </div>
      </div>
      <button
        type="button"
        onClick={onReview}
        style={{
          fontFamily: "var(--f-ui)",
          fontSize: 13,
          fontWeight: 500,
          padding: "8px 14px",
          background: "var(--accent-fill)",
          color: "#fff",
          border: "none",
          borderRadius: "var(--r-md)",
          cursor: "pointer",
          flexShrink: 0,
        }}
      >
        Review →
      </button>
    </div>
  );
}
