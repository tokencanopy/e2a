import { classifyDelivery, describeScope } from "./webhooks";

describe("classifyDelivery", () => {
  const NOW = new Date("2026-07-27T12:00:00Z");
  const base = {
    id: "whd_1",
    type: "email.received",
    attempts: 1,
    created_at: "2026-07-27T11:00:00Z",
    next_retry_at: "2026-07-27T11:00:00Z",
  };

  it("classifies a delivered row", () => {
    expect(classifyDelivery({ ...base, status: "delivered" }, NOW)).toEqual({
      kind: "delivered",
      raw: "delivered",
    });
  });

  it("classifies a terminally failed row", () => {
    expect(classifyDelivery({ ...base, status: "failed" }, NOW)).toEqual({
      kind: "failed",
      raw: "failed",
    });
  });

  // A pending delivery with a future retry is IN FLIGHT, not broken. The
  // retry envelope runs 72h, so this state is long-lived and must not read
  // as failure or people escalate on a delivery that is still working.
  it("classifies pending with a future retry as retrying", () => {
    expect(
      classifyDelivery(
        { ...base, status: "pending", next_retry_at: "2026-07-27T12:30:00Z" },
        NOW,
      ),
    ).toEqual({ kind: "retrying", raw: "pending" });
  });

  // Worker lag or clock skew puts next_retry_at in the past. Rendering a
  // countdown here would produce a negative duration.
  it("classifies pending with a past retry as retry_due", () => {
    expect(
      classifyDelivery(
        { ...base, status: "pending", next_retry_at: "2026-07-27T11:30:00Z" },
        NOW,
      ),
    ).toEqual({ kind: "retry_due", raw: "pending" });
  });

  it("classifies pending with an unparseable retry time as retry_due", () => {
    expect(
      classifyDelivery(
        { ...base, status: "pending", next_retry_at: "not-a-date" },
        NOW,
      ),
    ).toEqual({ kind: "retry_due", raw: "pending" });
  });

  it("classifies pending with a missing retry time as retry_due", () => {
    // next_retry_at is required by the schema, but the UI must not crash or
    // fabricate a countdown if the server ever omits it.
    const noRetry = {
      id: base.id,
      type: base.type,
      attempts: base.attempts,
      created_at: base.created_at,
      status: "pending",
    } as unknown as Parameters<typeof classifyDelivery>[0];
    expect(classifyDelivery(noRetry, NOW)).toEqual({
      kind: "retry_due",
      raw: "pending",
    });
  });

  // The status field is an open set. `scheduled` is not hypothetical: the
  // redeliver endpoint's own description says a fanned-out redelivery lands
  // as "scheduled". Anything unrecognized must surface as unknown with the
  // server's string preserved — never silently bucketed as success.
  it("classifies an unrecognized status as unknown, preserving the raw value", () => {
    expect(classifyDelivery({ ...base, status: "scheduled" }, NOW)).toEqual({
      kind: "unknown",
      raw: "scheduled",
    });
  });

  it("classifies an empty status as unknown", () => {
    expect(classifyDelivery({ ...base, status: "" }, NOW)).toEqual({
      kind: "unknown",
      raw: "",
    });
  });
});

describe("describeScope", () => {
  // An absent or empty filter object means the subscription receives events
  // for EVERY agent on the account. That is the state that went unnoticed in
  // production, so both spellings of it are pinned here.
  it("treats a missing filters object as unscoped", () => {
    expect(describeScope(undefined)).toEqual({ scoped: false });
  });

  it("treats a null filters object as unscoped", () => {
    expect(describeScope(null)).toEqual({ scoped: false });
  });

  it("treats an empty filters object as unscoped", () => {
    expect(describeScope({})).toEqual({ scoped: false });
  });

  it("treats explicitly empty filter arrays as unscoped", () => {
    expect(
      describeScope({ agent_emails: [], conversation_ids: [], labels: [] }),
    ).toEqual({ scoped: false });
  });

  it("treats null filter arrays as unscoped", () => {
    expect(
      describeScope({ agent_emails: null, conversation_ids: null, labels: null }),
    ).toEqual({ scoped: false });
  });

  it("lists a single agent", () => {
    expect(describeScope({ agent_emails: ["a@example.com"] })).toEqual({
      scoped: true,
      parts: ["a@example.com"],
    });
  });

  it("lists multiple agents comma-separated", () => {
    expect(
      describeScope({ agent_emails: ["a@example.com", "b@example.com"] }),
    ).toEqual({ scoped: true, parts: ["a@example.com, b@example.com"] });
  });

  it("summarizes conversations by count, singular", () => {
    expect(describeScope({ conversation_ids: ["conv_a"] })).toEqual({
      scoped: true,
      parts: ["1 conversation"],
    });
  });

  it("summarizes conversations by count, plural", () => {
    expect(describeScope({ conversation_ids: ["conv_a", "conv_b"] })).toEqual({
      scoped: true,
      parts: ["2 conversations"],
    });
  });

  it("lists labels with a prefix", () => {
    expect(describeScope({ labels: ["urgent", "vip"] })).toEqual({
      scoped: true,
      parts: ["labels: urgent, vip"],
    });
  });

  // Filter types are ANDed by the server (internal/webhookpub/publisher.go),
  // so all present types belong in the summary, in a stable order.
  it("combines all three filter types in a stable order", () => {
    expect(
      describeScope({
        labels: ["urgent"],
        agent_emails: ["a@example.com"],
        conversation_ids: ["conv_x", "conv_y"],
      }),
    ).toEqual({
      scoped: true,
      parts: ["a@example.com", "2 conversations", "labels: urgent"],
    });
  });
});
