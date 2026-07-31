// Contract guard for the message/review projection. The v1 wire splits
// message state into delivery_status (delivery rollup) + review_status
// (the review/HITL lifecycle: pending_review | sent | review_*). The app
// once read the now-removed hitl_status/`status === "pending_approval"`
// shape, which silently disabled the whole pending queue. These tests
// pin the projection to the v1 field names so that can't recur.

import {
  listAgents,
  listAgentMessages,
  listPendingMessages,
  findPendingMessage,
  getInboxUnread,
  getMessageDetailWire,
  getReviewDetailWire,
  projectInbound,
  projectMessageDetail,
  projectPending,
  UNREAD_BADGE_CAP,
  getWebhook,
  listWebhookDeliveries,
  type MessageViewWire,
} from "./api";

const mockFetch = jest.fn();
beforeEach(() => {
  mockFetch.mockReset();
  global.fetch = mockFetch as unknown as typeof fetch;
});

function okJson(obj: unknown) {
  return Promise.resolve({
    ok: true,
    status: 200,
    text: () => Promise.resolve(JSON.stringify(obj)),
  });
}
function notFound() {
  return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve("nf") });
}

describe("listAgents", () => {
  it("follows the next cursor and combines agent pages in order", async () => {
    const first = {
      domain: "agents.test",
      email: "first@agents.test",
      name: "First",
      domain_verified: true,
      created_at: "2026-07-22T00:00:00Z",
    };
    const second = {
      ...first,
      email: "second@agents.test",
      name: "Second",
    };
    mockFetch.mockImplementation((url: string) => {
      if (url === "/v1/agents") {
        return okJson({ items: [first], next_cursor: "next page" });
      }
      if (url === "/v1/agents?cursor=next%20page") {
        return okJson({ items: [second], next_cursor: null });
      }
      return notFound();
    });

    await expect(listAgents()).resolves.toEqual([first, second]);
    expect(mockFetch.mock.calls.map(([url]) => url)).toEqual([
      "/v1/agents",
      "/v1/agents?cursor=next%20page",
    ]);
  });

  it("rejects instead of returning a partial list when the page cap is exhausted", async () => {
    mockFetch.mockImplementation(() =>
      okJson({ items: [], next_cursor: `cursor-${mockFetch.mock.calls.length}` }),
    );

    await expect(listAgents()).rejects.toThrow(
      "Failed to load all agents: pagination exceeded 20 pages",
    );
    expect(mockFetch).toHaveBeenCalledTimes(20);
  });
});

describe("message projection (v1 contract)", () => {
  it("maps v1 review_status/delivery_status onto the app fields", async () => {
    mockFetch.mockImplementation((url: string) =>
      url.includes("/messages")
        ? okJson({
            items: [
              {
                message_id: "m1",
                direction: "outbound",
                header_from: "a@x.com",
                envelope_from: null,
                verified_domain: null,
                to: ["b@y.com"],
                recipient: "b@y.com",
                subject: "s",
                review_status: "pending_review",
                delivery_status: "queued",
                created_at: "2026-01-01T00:00:00Z",
              },
            ],
            next_cursor: null,
          })
        : notFound(),
    );
    const res = await listAgentMessages("a@x.com", { direction: "outbound" });
    expect(res.items[0].review_status).toBe("pending_review");
    expect(res.items[0].status).toBe("queued"); // delivery rollup
  });

  it("maps inbound read_status (delivery_status is outbound-only)", async () => {
    // Regression: inbound unread state lives in read_status, not status —
    // dropping it silently disabled the inbox's unread/bold affordance.
    mockFetch.mockImplementation((url: string) =>
      url.includes("/messages")
        ? okJson({
            items: [
              {
                message_id: "m1",
                direction: "inbound",
                header_from: "b@y.com",
                envelope_from: "b@y.com",
                verified_domain: "y.com",
                to: ["a@x.com"],
                recipient: "a@x.com",
                subject: "hi",
                read_status: "unread",
                created_at: "2026-01-01T00:00:00Z",
              },
            ],
            next_cursor: null,
          })
        : notFound(),
    );
    const res = await listAgentMessages("a@x.com", { direction: "all" });
    expect(res.items[0].read_status).toBe("unread");
  });

  it("maps GET /v1/reviews items into the pending queue (both directions)", async () => {
    mockFetch.mockImplementation((url: string) =>
      url === "/v1/reviews"
        ? okJson({
            items: [
              {
                id: "m1",
                agent_email: "a@x.com",
                direction: "outbound",
                header_from: "a@x.com",
                envelope_from: null,
                verified_domain: null,
                to: ["b@y.com"],
                subject: "held draft",
                review_status: "pending_review",
                created_at: "2026-01-01T00:00:00Z",
              },
              {
                id: "m2",
                agent_email: "a@x.com",
                direction: "inbound",
                header_from: "spammer@evil.com",
                envelope_from: "bounce@evil.com",
                verified_domain: null,
                to: ["a@x.com"],
                subject: "screened inbound",
                review_status: "pending_review",
                created_at: "2026-01-01T00:00:00Z",
              },
            ],
            next_cursor: null,
          })
        : notFound(),
    );
    const pending = await listPendingMessages();
    expect(pending).toHaveLength(2);
    expect(pending[0]).toMatchObject({ id: "m1", agent_email: "a@x.com", direction: "outbound" });
    expect(pending[1]).toMatchObject({ id: "m2", direction: "inbound", from: "spammer@evil.com" });
  });

  it("does not hit the agent /messages endpoint for the queue (reviews only)", async () => {
    // The queue is a single account-scoped /v1/reviews call — never a
    // per-agent fan-out over /messages (which would never surface inbound).
    mockFetch.mockImplementation((url: string) =>
      url === "/v1/reviews" ? okJson({ items: [], next_cursor: null }) : notFound(),
    );
    await listPendingMessages();
    const urls = mockFetch.mock.calls.map((c) => c[0] as string);
    expect(urls).toEqual(["/v1/reviews"]);
  });

  it("follows review cursors until it finds a deep-linked hold", async () => {
    mockFetch.mockImplementation((url: string) => {
      if (url === "/v1/reviews?limit=100") {
        return okJson({
          items: [{ id: "newer", agent_email: "a@x.com", direction: "outbound", header_from: "a@x.com", envelope_from: null, verified_domain: null, to: ["b@y.com"], subject: "newer", review_status: "pending_review", created_at: "2026-01-02T00:00:00Z" }],
          next_cursor: "page-2",
        });
      }
      if (url === "/v1/reviews?limit=100&cursor=page-2") {
        return okJson({
          items: [{ id: "target", agent_email: "a@x.com", direction: "outbound", header_from: "a@x.com", envelope_from: null, verified_domain: null, to: ["b@y.com"], subject: "target hold", review_status: "pending_review", created_at: "2026-01-01T00:00:00Z" }],
          next_cursor: null,
        });
      }
      return notFound();
    });

    await expect(findPendingMessage("target")).resolves.toMatchObject({
      id: "target",
      subject: "target hold",
    });
    expect(mockFetch.mock.calls.map((c) => c[0])).toEqual([
      "/v1/reviews?limit=100",
      "/v1/reviews?limit=100&cursor=page-2",
    ]);
  });
});

// Every per-message SWR entry holds ONE shape — the raw MessageViewWire —
// because two surfaces read the same message through two different
// endpoints under one shared cache key (see lib/swrKeys.ts). That
// invariant only holds if (a) both fetchers return the wire unprojected
// and (b) the projectors stay pure functions applied at the point of use.
// The tests below pin both halves; PendingRow and ThreadBubble render tests
// pin the cross-surface consequence.

// A MessageView as the REVIEW read returns it (GET /v1/reviews/{id}) —
// the superset: it alone carries hold_reason + protection.
const REVIEW_WIRE: MessageViewWire = {
  id: "msg_1",
  direction: "outbound",
  header_from: null,
  envelope_from: null,
  verified_domain: null,
  authentication: null,
  to: ["customer@bigco.com"],
  cc: ["cc@bigco.com"],
  reply_to: [],
  delivered_to: "customer@bigco.com",
  subject: "Re: refund",
  conversation_id: "conv_1",
  review_status: "pending_review",
  delivery_status: "",
  created_at: "2026-01-01T00:00:00Z",
  hold_reason: {
    type: "scan",
    code: "outbound_scan",
    summary: "Content screening found a potential risk.",
  },
  protection: [{ source: "scan", detector: "gemini" }],
  body: { text: "Hello, your refund is on the way.", html: "<p>refund</p>" },
};

// A MessageView as the AGENT-SCOPED read returns it
// (GET /v1/agents/{address}/messages/{id}) for an inbound row.
const INBOUND_WIRE: MessageViewWire = {
  id: "msg_in",
  direction: "inbound",
  header_from: "james@x.com",
  envelope_from: "bounce@x.com",
  verified_domain: null,
  authentication: null,
  to: ["support@acme.dev"],
  delivered_to: "support@acme.dev",
  subject: "Hi",
  conversation_id: "conv_in",
  delivery_status: "received",
  read_status: "unread",
  created_at: "2026-01-01T00:00:00Z",
  parsed: { text: "plain body" },
  attachments: [],
  raw_message: "cmF3",
};

describe("message-detail projectors (shared-cache invariant)", () => {
  it("projectPending maps the review wire onto the review surfaces' shape", () => {
    const d = projectPending("support@acme.dev", REVIEW_WIRE);
    // agent_email is a fetch parameter, not a wire field — the review read
    // is account-scoped and never echoes the owning inbox back.
    expect(d.agent_email).toBe("support@acme.dev");
    // A held draft's lifecycle state is review_status; delivery_status is
    // empty until it's approved and sent. Reading the delivery rollup here
    // is what made the whole queue think nothing was pending.
    expect(d.status).toBe("pending_review");
    expect(d.body_text).toBe("Hello, your refund is on the way.");
    expect(d.body_html).toBe("<p>refund</p>");
    // hold_reason/protection exist ONLY on the review read — dropping them
    // in the projection silently removes the "Why this message was held"
    // banner and the screening disclosure.
    expect(d.hold_reason?.code).toBe("outbound_scan");
    expect(d.protection?.[0].detector).toBe("gemini");
  });

  it("projectPending falls back to parsed.* for a legacy row without body columns", () => {
    // Legacy outbound rows can have content only in `parsed`.
    // Without the fallback the review row renders "(empty body)".
    const d = projectPending("support@acme.dev", {
      ...REVIEW_WIRE,
      body: undefined,
      parsed: { text: "legacy-path text", html: "<p>legacy</p>" },
    });
    expect(d.body_text).toBe("legacy-path text");
    expect(d.body_html).toBe("<p>legacy</p>");
  });

  it("projectInbound maps delivered_to → recipient and read_status → status", () => {
    const d = projectInbound(INBOUND_WIRE);
    // The wire field is `delivered_to`; the app type calls it `recipient`.
    // They are NOT the same name, so a passthrough would leave the
    // attachment endpoint without an agent address to key by.
    expect(d.recipient).toBe("support@acme.dev");
    expect(d.status).toBe("unread");
    expect(d.header_from).toBe("james@x.com");
    expect(d.envelope_from).toBe("bounce@x.com");
    expect(d.parsed?.text).toBe("plain body");
  });

  it("projectInbound defaults absent list/scalar fields instead of leaking undefined", () => {
    // PendingRow and ThreadBubble index into these without guards
    // (cc.join, attachments.map) — undefined here is
    // a crash there.
    const d = projectInbound({
      id: "msg_bare",
      direction: "inbound",
      header_from: "a@x.com",
      envelope_from: null,
      verified_domain: null,
      authentication: null,
      delivered_to: "support@acme.dev",
      subject: "",
      created_at: "2026-01-01T00:00:00Z",
    });
    expect(d.to).toEqual([]);
    expect(d.cc).toEqual([]);
    expect(d.reply_to).toEqual([]);
    expect(d.attachments).toEqual([]);
    expect(d.authentication).toBeNull();
    expect(d.conversation_id).toBe("");
    expect(d.raw_message).toBe("");
  });

  it("projectMessageDetail wraps by the authoritative wire direction", () => {
    const out = projectMessageDetail("support@acme.dev", REVIEW_WIRE, "inbound");
    expect(out.direction).toBe("outbound");
    // Outbound rows read the draft body; the discriminated union is what
    // lets each consuming surface narrow safely.
    expect(out.direction === "outbound" && out.data.body_text).toBe(
      "Hello, your refund is on the way.",
    );

    const inb = projectMessageDetail("support@acme.dev", INBOUND_WIRE, "outbound");
    expect(inb.direction).toBe("inbound");
    expect(inb.direction === "inbound" && inb.data.recipient).toBe(
      "support@acme.dev",
    );
  });

  it("projectMessageDetail reads direction without a query fallback", () => {
    const d = projectMessageDetail("support@acme.dev", REVIEW_WIRE);
    expect(d.direction).toBe("outbound");
  });
});

describe("message-detail fetchers return the RAW wire", () => {
  // Both fetchers write into the same SWR entry. If either projects before
  // caching, the other surface reads a shape it doesn't expect — the
  // original crash (`msg.data` undefined → 500 error boundary). These
  // assert on wire-only field names that no projection preserves.

  it("getMessageDetailWire hits the agent-scoped path and returns the wire unprojected", async () => {
    mockFetch.mockImplementation((url: string) =>
      url === "/v1/agents/support%40acme.dev/messages/msg_in"
        ? okJson(INBOUND_WIRE)
        : notFound(),
    );
    const w = await getMessageDetailWire("support@acme.dev", "msg_in");
    // Wire-only names survive: a projection would have renamed
    // delivered_to → recipient and wrapped the result in {direction,data}.
    expect(w.delivered_to).toBe("support@acme.dev");
    expect(w).not.toHaveProperty("data");
    expect(w).not.toHaveProperty("recipient");
    expect(w).not.toHaveProperty("body_text");
  });

  it("getReviewDetailWire hits /v1/reviews/{id} and preserves hold_reason on the wire", async () => {
    // The review read is account-scoped (id is globally unique) and is the
    // ONLY endpoint carrying hold_reason/protection — it must not be
    // flattened into the review-surface shape before it reaches the cache.
    mockFetch.mockImplementation((url: string) =>
      url === "/v1/reviews/msg_1" ? okJson(REVIEW_WIRE) : notFound(),
    );
    const w = await getReviewDetailWire("msg_1");
    expect(w.hold_reason?.code).toBe("outbound_scan");
    expect(w.review_status).toBe("pending_review");
    expect(w.delivered_to).toBe("customer@bigco.com");
    expect(w).not.toHaveProperty("agent_email");
    expect(w).not.toHaveProperty("body_text");
  });
});

describe("getInboxUnread (Inboxes list badge probe)", () => {
  it("queries inbound unread with the capped limit and reports the count", async () => {
    mockFetch.mockImplementation((url: string) =>
      url.includes("/messages")
        ? okJson({
            items: [
              { message_id: "m1" },
              { message_id: "m2" },
              { message_id: "m3" },
            ],
            next_cursor: null,
          })
        : notFound(),
    );
    const res = await getInboxUnread("billing@acme.dev");
    expect(res).toEqual({ count: 3, more: false });

    // Must filter on the backend's inbound read-state param (read_status),
    // not the ignored `status` param, and only pull one capped page.
    const url = mockFetch.mock.calls[0][0] as string;
    expect(url).toContain("/v1/agents/billing%40acme.dev/messages");
    expect(url).toContain("direction=inbound");
    expect(url).toContain("read_status=unread");
    expect(url).toContain(`limit=${UNREAD_BADGE_CAP}`);
  });

  it("flags more=true when the capped page is full (cursor present)", async () => {
    // A returned cursor means there are more unread than the cap — the card
    // renders "N+" off this flag.
    mockFetch.mockImplementation((url: string) =>
      url.includes("/messages")
        ? okJson({
            items: Array.from({ length: UNREAD_BADGE_CAP }, (_, i) => ({
              message_id: `m${i}`,
            })),
            next_cursor: "cursor-abc",
          })
        : notFound(),
    );
    const res = await getInboxUnread("billing@acme.dev");
    expect(res.count).toBe(UNREAD_BADGE_CAP);
    expect(res.more).toBe(true);
  });

  it("reports zero unread on an empty page", async () => {
    mockFetch.mockImplementation((url: string) =>
      url.includes("/messages") ? okJson({ items: [], next_cursor: null }) : notFound(),
    );
    expect(await getInboxUnread("billing@acme.dev")).toEqual({ count: 0, more: false });
  });
});

describe("getWebhook", () => {
  it("fetches one webhook by id, percent-encoding the id", async () => {
    mockFetch.mockImplementation(() =>
      okJson({ id: "wh_1", url: "https://x.test/h", enabled: true, created_at: "2026-07-01T00:00:00Z" }),
    );
    const wh = await getWebhook("wh_1");
    expect(mockFetch.mock.calls[0][0]).toBe("/v1/webhooks/wh_1");
    expect(wh.id).toBe("wh_1");
  });

  // The detail page routes on a query param the user can edit, so a bad or
  // deleted id must surface as a 404 the page can render cleanly rather than
  // an unhandled throw.
  it("throws ApiError with status 404 for an unknown id", async () => {
    mockFetch.mockImplementation(() => notFound());
    await expect(getWebhook("wh_missing")).rejects.toMatchObject({ status: 404 });
  });
});

describe("listWebhookDeliveries", () => {
  it("requests the delivery log for the webhook with no filter by default", async () => {
    mockFetch.mockImplementation(() => okJson({ items: [], next_cursor: null }));
    await listWebhookDeliveries("wh_1");
    expect(mockFetch.mock.calls[0][0]).toBe("/v1/webhooks/wh_1/deliveries");
  });

  it("forwards the status filter and cursor", async () => {
    mockFetch.mockImplementation(() => okJson({ items: [], next_cursor: null }));
    await listWebhookDeliveries("wh_1", { status: "failed", cursor: "abc", pageSize: 25 });
    const url = mockFetch.mock.calls[0][0] as string;
    expect(url).toContain("status=failed");
    expect(url).toContain("cursor=abc");
    expect(url).toContain("limit=25");
  });

  it("normalizes a missing items array and next_cursor to empty values", async () => {
    mockFetch.mockImplementation(() => okJson({}));
    expect(await listWebhookDeliveries("wh_1")).toEqual({ items: [], next_cursor: null });
  });

  it("passes delivery rows through without dropping open-set fields", async () => {
    mockFetch.mockImplementation(() =>
      okJson({
        items: [
          {
            id: "whd_1",
            type: "some.future.event",
            status: "scheduled",
            attempts: 2,
            next_retry_at: "2026-07-27T12:30:00Z",
            created_at: "2026-07-27T12:00:00Z",
            last_status_code: 503,
            last_error: "upstream unavailable",
          },
        ],
        next_cursor: "next",
      }),
    );
    const page = await listWebhookDeliveries("wh_1");
    expect(page.next_cursor).toBe("next");
    expect(page.items[0]).toMatchObject({
      type: "some.future.event",
      status: "scheduled",
      last_status_code: 503,
    });
  });
});
