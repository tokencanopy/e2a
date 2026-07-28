import { describeScope } from "./webhooks";

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
