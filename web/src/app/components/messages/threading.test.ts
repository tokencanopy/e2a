// Threading-logic contract for the dashboard inbox.

import type { MessageSummary } from "../types";
import {
  decodeThreadFragment,
  encodeThreadFragment,
  groupIntoThreads,
  findThread,
} from "./threading";

function isoMinutesAgo(now: Date, n: number): string {
  return new Date(now.getTime() - n * 60_000).toISOString();
}

function msg(partial: Partial<MessageSummary>): MessageSummary {
  return {
    id: "msg_" + Math.random().toString(36).slice(2, 8),
    direction: "inbound",
    from: "sender@example.com",
    to: ["agent@example.com"],
    recipient: "agent@example.com",
    subject: "subject",
    status: "read",
    created_at: new Date().toISOString(),
    ...partial,
  };
}

describe("thread fragment codec", () => {
  it("round-trips spaces, Unicode, and literal percent signs", () => {
    const key = "conv:客户 1%ready";
    const encoded = encodeThreadFragment(key);
    expect(encoded).toBe("conv:%E5%AE%A2%E6%88%B7%201%25ready");
    expect(decodeThreadFragment(encoded)).toBe(key);
  });

  it("leaves a malformed legacy percent fragment readable", () => {
    expect(decodeThreadFragment("conv:100%ready")).toBe("conv:100%ready");
  });
});

describe("groupIntoThreads", () => {
  const NOW = new Date("2026-05-24T12:00:00Z");

  it("groups messages with the same conversation_id into one thread", () => {
    const rows: MessageSummary[] = [
      msg({ id: "m1", conversation_id: "conv_A", subject: "Q3 contract", created_at: isoMinutesAgo(NOW, 25) }),
      msg({ id: "m2", conversation_id: "conv_A", direction: "outbound", to: ["maya@stripe.com"], recipient: "maya@stripe.com", subject: "Re: Q3 contract", created_at: isoMinutesAgo(NOW, 13) }),
    ];
    const threads = groupIntoThreads(rows, NOW);
    expect(threads).toHaveLength(1);
    expect(threads[0].key).toBe("conv:conv_A");
    expect(threads[0].conversationId).toBe("conv_A");
    expect(threads[0].msgCount).toBe(2);
    // Ordered oldest → newest inside the thread.
    expect(threads[0].messages.map((m) => m.id)).toEqual(["m1", "m2"]);
  });

  it("orphan messages (no conversation_id) become single-message threads keyed orphan:<id>", () => {
    const rows: MessageSummary[] = [
      msg({ id: "m_solo", subject: "GitHub PR merged", created_at: isoMinutesAgo(NOW, 30) }),
    ];
    const threads = groupIntoThreads(rows, NOW);
    expect(threads).toHaveLength(1);
    expect(threads[0].key).toBe("orphan:m_solo");
    expect(threads[0].conversationId).toBeUndefined();
    expect(threads[0].msgCount).toBe(1);
  });

  // Real and synthetic thread keys live in separate prefixed namespaces
  // (`conv:` and `orphan:`) so an operator-controlled `conversation_id`
  // can't collide with the synthetic key namespace. Pin the invariant
  // so a future change can't silently regress it.
  it("real conversation_id starting with 'orphan:' does NOT collide with the synthetic namespace", () => {
    const rows: MessageSummary[] = [
      msg({
        id: "m_collide",
        conversation_id: "orphan:nefarious",
        subject: "Operator-controlled conv_id",
        created_at: isoMinutesAgo(NOW, 30),
      }),
      msg({
        id: "nefarious",
        subject: "Real orphan",
        created_at: isoMinutesAgo(NOW, 25),
      }),
    ];
    const threads = groupIntoThreads(rows, NOW);
    // Two distinct threads — the dual-prefix scheme means the keys
    // can't collide even when the operator tries.
    expect(threads).toHaveLength(2);
    const keys = threads.map((t) => t.key).sort();
    expect(keys).toEqual(["conv:orphan:nefarious", "orphan:nefarious"]);
    const real = threads.find((t) => t.key === "conv:orphan:nefarious");
    const synthetic = threads.find((t) => t.key === "orphan:nefarious");
    expect(real?.messages[0].id).toBe("m_collide");
    expect(real?.conversationId).toBe("orphan:nefarious");
    expect(synthetic?.messages[0].id).toBe("nefarious");
    expect(synthetic?.conversationId).toBeUndefined();
  });

  it("derives state='pending' when any message in the thread is pending_approval", () => {
    const rows: MessageSummary[] = [
      msg({ id: "m1", conversation_id: "conv_A", from: "maya@stripe.com", created_at: isoMinutesAgo(NOW, 25) }),
      msg({
        id: "m2",
        conversation_id: "conv_A",
        direction: "outbound",
        to: ["maya@stripe.com"],
        recipient: "maya@stripe.com",
        review_status: "pending_review",
        created_at: isoMinutesAgo(NOW, 13),
      }),
    ];
    const threads = groupIntoThreads(rows, NOW);
    expect(threads[0].state).toBe("pending");
  });

  it("derives state='closed' when all messages are older than 7 days", () => {
    const rows: MessageSummary[] = [
      msg({ id: "m1", conversation_id: "conv_A", created_at: isoMinutesAgo(NOW, 8 * 24 * 60) }),
      msg({ id: "m2", conversation_id: "conv_A", direction: "outbound", to: ["x@example.com"], created_at: isoMinutesAgo(NOW, 7 * 24 * 60 + 60) }),
    ];
    const threads = groupIntoThreads(rows, NOW);
    expect(threads[0].state).toBe("closed");
  });

  it("derives state='handed-off' when outbound messages address two different recipients", () => {
    const rows: MessageSummary[] = [
      msg({ id: "m1", conversation_id: "conv_A", from: "customer@example.com", created_at: isoMinutesAgo(NOW, 60) }),
      msg({ id: "m2", conversation_id: "conv_A", direction: "outbound", to: ["agent@partner.com"], recipient: "agent@partner.com", created_at: isoMinutesAgo(NOW, 50) }),
      msg({ id: "m3", conversation_id: "conv_A", direction: "outbound", to: ["billing@acme.io"], recipient: "billing@acme.io", created_at: isoMinutesAgo(NOW, 40) }),
    ];
    const threads = groupIntoThreads(rows, NOW);
    expect(threads[0].state).toBe("handed-off");
  });

  it("derives state='active' for recent threads with single outbound target", () => {
    const rows: MessageSummary[] = [
      msg({ id: "m1", conversation_id: "conv_A", created_at: isoMinutesAgo(NOW, 30) }),
      msg({ id: "m2", conversation_id: "conv_A", direction: "outbound", to: ["x@example.com"], recipient: "x@example.com", created_at: isoMinutesAgo(NOW, 20) }),
    ];
    const threads = groupIntoThreads(rows, NOW);
    expect(threads[0].state).toBe("active");
  });

  it("pending threads sort to the top, then by lastMessageAt DESC", () => {
    const rows: MessageSummary[] = [
      msg({ id: "a1", conversation_id: "conv_OLD", created_at: isoMinutesAgo(NOW, 200) }),
      msg({ id: "b1", conversation_id: "conv_NEW", created_at: isoMinutesAgo(NOW, 10) }),
      msg({
        id: "c1",
        conversation_id: "conv_PEND",
        direction: "outbound",
        to: ["x@example.com"],
        review_status: "pending_review",
        created_at: isoMinutesAgo(NOW, 60),
      }),
    ];
    const threads = groupIntoThreads(rows, NOW);
    expect(threads.map((t) => t.key)).toEqual(["conv:conv_PEND", "conv:conv_NEW", "conv:conv_OLD"]);
  });

  it("counterparty for inbound uses the sender; for outbound uses to[0]", () => {
    const rows: MessageSummary[] = [
      msg({
        id: "m1",
        conversation_id: "conv_X",
        direction: "outbound",
        from: "support@acme.io",
        to: ["maya@stripe.com"],
        recipient: "maya@stripe.com",
        created_at: isoMinutesAgo(NOW, 5),
      }),
    ];
    const threads = groupIntoThreads(rows, NOW);
    expect(threads[0].counterparty.email).toBe("maya@stripe.com");
  });

  it("subject falls back to the first non-empty subject in the thread, else (no subject)", () => {
    const rows: MessageSummary[] = [
      msg({ id: "m1", conversation_id: "conv_X", subject: "", created_at: isoMinutesAgo(NOW, 30) }),
      msg({ id: "m2", conversation_id: "conv_X", subject: "Re: real subject", created_at: isoMinutesAgo(NOW, 10) }),
    ];
    expect(groupIntoThreads(rows, NOW)[0].subject).toBe("Re: real subject");

    const orphan = groupIntoThreads(
      [msg({ id: "m_orphan", subject: "" })],
      NOW,
    );
    expect(orphan[0].subject).toBe("(no subject)");
  });
});

describe("findThread", () => {
  const NOW = new Date("2026-05-24T12:00:00Z");
  const threads = groupIntoThreads(
    [
      msg({ id: "m1", conversation_id: "conv_A", created_at: isoMinutesAgo(NOW, 30) }),
      msg({ id: "m_solo", created_at: isoMinutesAgo(NOW, 20) }),
    ],
    NOW,
  );

  it("returns the thread matching the key", () => {
    const t = findThread(threads, "conv:conv_A");
    expect(t?.key).toBe("conv:conv_A");
  });

  it("matches synthetic orphan:<id> keys", () => {
    const t = findThread(threads, "orphan:m_solo");
    expect(t?.key).toBe("orphan:m_solo");
  });

  it("falls back to the first thread when the key is unknown or null", () => {
    expect(findThread(threads, null)?.key).toBe(threads[0].key);
    expect(findThread(threads, "conv_DOES_NOT_EXIST")?.key).toBe(threads[0].key);
  });

  it("returns null for an empty thread list", () => {
    expect(findThread([], "anything")).toBeNull();
  });
});
