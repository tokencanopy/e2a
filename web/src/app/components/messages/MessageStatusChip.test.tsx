import { deriveStatusChip, isFutureScheduled } from "./MessageStatusChip";

describe("deriveStatusChip", () => {
  it.each([
    ["accepted", "info", "Queued", true],
    ["sending", "info", "Sending", true],
    ["deferred", "warn", "Delayed", true],
    ["failed", "danger", "Failed", true],
    ["bounced", "danger", "Bounced", true],
    ["complained", "danger", "Complaint", true],
    ["sent", "success", "Sent", false],
    ["delivered", "success", "Delivered", false],
  ] as const)(
    "maps outbound delivery_status=%s to %s %s",
    (delivery_status, tone, label, attention) => {
      expect(deriveStatusChip({ direction: "outbound", delivery_status })).toEqual({
        tone,
        label,
        attention,
      });
    },
  );

  it("gives pending review precedence over delivery state", () => {
    expect(
      deriveStatusChip({
        direction: "outbound",
        delivery_status: "delivered",
        review_status: "pending_review",
      }),
    ).toEqual({
      tone: "warn",
      label: "Pending review",
      dot: true,
      attention: true,
    });
  });

  it.each([
    ["review_rejected", "Rejected"],
    ["review_expired_rejected", "Auto-rejected"],
  ] as const)("gives %s precedence over delivery state", (review_status, label) => {
    expect(
      deriveStatusChip({
        direction: "outbound",
        delivery_status: "delivered",
        review_status,
      }),
    ).toEqual({ tone: "danger", label, attention: true });
  });

  it("lets delivery_status=accepted override collapsed review_status=sent", () => {
    expect(
      deriveStatusChip({
        direction: "outbound",
        delivery_status: "accepted",
        review_status: "sent",
      }),
    ).toEqual({ tone: "info", label: "Queued", attention: true });
  });

  it.each([
    ["sent", "Sent"],
    ["review_expired_approved", "Sent (auto)"],
  ] as const)("falls back from review_status=%s when delivery state is absent", (review_status, label) => {
    expect(deriveStatusChip({ direction: "outbound", review_status })).toEqual({
      tone: "success",
      label,
      attention: false,
    });
  });

  it("surfaces an unknown non-empty delivery status as a neutral raw label", () => {
    expect(
      deriveStatusChip({ direction: "outbound", delivery_status: "new_status" }),
    ).toEqual({ tone: "neutral", label: "new_status", attention: false });
  });

  it("returns null for outbound messages without lifecycle state", () => {
    expect(deriveStatusChip({ direction: "outbound" })).toBeNull();
  });

  it("preserves inbound unread as a passive info chip", () => {
    expect(
      deriveStatusChip({ direction: "inbound", inbox_status: "unread" }),
    ).toEqual({ tone: "info", label: "Unread", attention: false });
  });

  it("preserves inbound read as a passive neutral chip", () => {
    expect(
      deriveStatusChip({ direction: "inbound", inbox_status: "read" }),
    ).toEqual({ tone: "neutral", label: "Read", attention: false });
  });
});

describe("deriveStatusChip — scheduled send", () => {
  const future = new Date(Date.now() + 24 * 3600 * 1000).toISOString();
  const past = new Date(Date.now() - 3600 * 1000).toISOString();

  it("renders a Scheduled chip when an accepted send has a future scheduled_at", () => {
    expect(
      deriveStatusChip({ direction: "outbound", delivery_status: "accepted", scheduled_at: future }),
    ).toEqual({ tone: "info", label: "Scheduled", clock: true, attention: true });
  });

  it("falls back to Queued when scheduled_at is already in the past", () => {
    expect(
      deriveStatusChip({ direction: "outbound", delivery_status: "accepted", scheduled_at: past }),
    ).toEqual({ tone: "info", label: "Queued", attention: true });
  });

  it("lets a terminal delivery status win over a still-future scheduled_at", () => {
    expect(
      deriveStatusChip({ direction: "outbound", delivery_status: "sent", scheduled_at: future }),
    ).toEqual({ tone: "success", label: "Sent", attention: false });
  });

  it("keeps a held scheduled send as Pending review", () => {
    expect(
      deriveStatusChip({
        direction: "outbound",
        delivery_status: "accepted",
        review_status: "pending_review",
        scheduled_at: future,
      }),
    ).toEqual({ tone: "warn", label: "Pending review", dot: true, attention: true });
  });
});

describe("isFutureScheduled", () => {
  const future = new Date(Date.now() + 60_000).toISOString();
  const past = new Date(Date.now() - 60_000).toISOString();

  it("is true only for an outbound row with a future scheduled_at", () => {
    expect(isFutureScheduled({ direction: "outbound", scheduled_at: future })).toBe(true);
  });

  it("is false for past, missing, or inbound", () => {
    expect(isFutureScheduled({ direction: "outbound", scheduled_at: past })).toBe(false);
    expect(isFutureScheduled({ direction: "outbound" })).toBe(false);
    expect(isFutureScheduled({ direction: "inbound", scheduled_at: future })).toBe(false);
  });
});
