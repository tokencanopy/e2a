import {
  classifyDelivery,
  classifyWebhookHealth,
  describeScope,
} from "./webhooks";

describe("classifyWebhookHealth", () => {
  const NOW = new Date("2026-07-27T12:00:00Z");
  const base = {
    id: "wh_1",
    url: "https://x.test/hook",
    enabled: true,
    created_at: "2026-07-01T00:00:00Z",
  };

  // e2a disabling an endpoint on the user's behalf is the loudest thing this
  // signal can say, and it is not the same fact as a user switching it off.
  it("reports auto-disabled ahead of a plain disabled flag", () => {
    expect(
      classifyWebhookHealth(
        { ...base, enabled: false, auto_disabled_at: "2026-07-20T00:00:00Z" },
        NOW,
      ).kind,
    ).toBe("auto_disabled");
  });

  it("reports a user-disabled subscription", () => {
    expect(classifyWebhookHealth({ ...base, enabled: false }, NOW).kind).toBe(
      "disabled",
    );
  });

  // A subscription that has never fired usually means the scope matches
  // nothing, or nothing has happened yet — different from having gone quiet.
  it("distinguishes never-delivered from stale", () => {
    expect(classifyWebhookHealth(base, NOW).kind).toBe("never_delivered");
  });

  it("reports a subscription delivering inside the window as active", () => {
    expect(
      classifyWebhookHealth(
        { ...base, last_delivered_at: "2026-07-26T12:00:00Z" },
        NOW,
      ).kind,
    ).toBe("active");
  });

  it("reports a subscription quiet for longer than the window as stale", () => {
    expect(
      classifyWebhookHealth(
        { ...base, last_delivered_at: "2026-07-10T12:00:00Z" },
        NOW,
      ).kind,
    ).toBe("stale");
  });

  // 7 days exactly is still inside the window — the boundary should not flip
  // a healthy endpoint to stale a moment early.
  it("treats the window boundary as active", () => {
    expect(
      classifyWebhookHealth(
        { ...base, last_delivered_at: "2026-07-20T12:00:00Z" },
        NOW,
      ).kind,
    ).toBe("active");
  });

  it("treats an unparseable last_delivered_at as never delivered", () => {
    expect(
      classifyWebhookHealth({ ...base, last_delivered_at: "nonsense" }, NOW)
        .kind,
    ).toBe("never_delivered");
  });
});

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

  // The OpenAPI schema declares status an open set ("tolerate unknown
  // values"), even though migration 026's CHECK currently pins the column to
  // pending/delivered/failed. Forward-compat: whatever a future value is, it
  // must surface verbatim rather than being bucketed as success.
  it("classifies an unrecognized status as unknown, preserving the raw value", () => {
    expect(classifyDelivery({ ...base, status: "deferred" }, NOW)).toEqual({
      kind: "unknown",
      raw: "deferred",
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
