import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { format } from "node:util";
import { describe, expect, it, vi } from "vitest";
import type {
  AttachmentView,
  MessageSummaryView,
  MessageView,
  SendResultView,
} from "../../src/v1/generated/index.js";
import { ObjectSerializer } from "../../src/v1/generated/models/ObjectSerializer.js";
import { E2AValidationError } from "../../src/v1/errors.js";
import {
  InboundResource,
  type InboundMessageOperations,
  type InboundProjection,
} from "../../src/v1/inbound.js";
import type { WebhookEvent } from "../../src/v1/webhook-signature.js";

const fixtureDir = fileURLToPath(new URL("../../../testdata/inbound-email/", import.meta.url));

interface Vector {
  event: WebhookEvent;
  message: Record<string, unknown>;
  expected: InboundProjection;
}

function load<T>(name: string): T {
  return JSON.parse(readFileSync(`${fixtureDir}${name}`, "utf8")) as T;
}

function messageFromWire(value: Record<string, unknown>): MessageView {
  return ObjectSerializer.deserialize(value, "MessageView", "") as MessageView;
}

function operations(message: MessageView) {
  const pending: SendResultView = { messageId: "msg_reply", status: "pending_review" };
  const attachment: AttachmentView = {
    index: 0,
    sizeBytes: 1,
    downloadUrl: "https://download.example/one",
    expiresAt: new Date("2026-07-01T11:00:00Z"),
  };
  const ops: InboundMessageOperations = {
    get: vi.fn(async () => message),
    getAttachment: vi.fn(async () => attachment),
    reply: vi.fn(async () => pending),
    forward: vi.fn(async () => pending),
  };
  return { ops, pending, attachment };
}

describe("InboundEmail conformance", () => {
  it("deserializes optional beta thread IDs on message detail and summary models", () => {
    const detail = messageFromWire({ thread_id: "thr_0123456789abcdef0123456789abcdef" });
    const summary = ObjectSerializer.deserialize(
      { thread_id: "thr_fedcba9876543210fedcba9876543210" },
      "MessageSummaryView",
      "",
    ) as MessageSummaryView;
    const legacyDetail = messageFromWire({});

    expect(detail.threadId).toBe("thr_0123456789abcdef0123456789abcdef");
    expect(summary.threadId).toBe("thr_fedcba9876543210fedcba9876543210");
    expect(legacyDetail.threadId).toBeUndefined();
  });

  for (const name of ["full.json", "minimal.json", "adversarial.json"]) {
    it(`normalizes the shared ${name} vector`, async () => {
      const vector = load<Vector>(name);
      const { ops } = operations(messageFromWire(vector.message));

      const email = await new InboundResource(ops).fromEvent(vector.event);

      expect(email.toJSON()).toEqual(vector.expected);
      expect(email.event).toBe(vector.event);
      expect(ops.get).toHaveBeenCalledWith(
        (vector.event.data as { delivered_to: string }).delivered_to,
        (vector.event.data as { message_id: string }).message_id,
      );
      expect(JSON.stringify(email)).not.toContain("rawMessage");
      expect(JSON.stringify(email)).not.toContain("downloadUrl");
    });
  }

  it("rejects every invalid shared vector before transport", async () => {
    const base = load<Vector>("full.json");
    const cases = load<Array<{
      name: string;
      patch?: Record<string, unknown>;
      data_patch?: Record<string, unknown>;
    }>>("invalid.json");
    const { ops } = operations(messageFromWire(base.message));
    const inbound = new InboundResource(ops);

    for (const item of cases) {
      const event = structuredClone(base.event) as WebhookEvent;
      Object.assign(event, item.patch);
      if (item.data_patch && event.data && typeof event.data === "object") {
        for (const [key, value] of Object.entries(item.data_patch)) {
          if (value === "__delete__") delete (event.data as Record<string, unknown>)[key];
          else (event.data as Record<string, unknown>)[key] = value;
        }
      }

      await expect(inbound.fromEvent(event), item.name).rejects.toMatchObject({
        code: "invalid_email_received_event",
        status: 0,
        retryable: false,
      });
    }

    expect(ops.get).not.toHaveBeenCalled();
  });

  it("binds reply, forward, and attachment retrieval without changing results", async () => {
    const vector = load<Vector>("full.json");
    const { ops, pending, attachment } = operations(messageFromWire(vector.message));
    const email = await new InboundResource(ops).fromEvent(vector.event);

    await expect(email.reply({ text: "Got it" }, { idempotencyKey: "reply:evt" })).resolves.toBe(pending);
    expect(ops.reply).toHaveBeenCalledWith(
      vector.expected.inbox,
      vector.expected.id,
      { text: "Got it" },
      { idempotencyKey: "reply:evt" },
    );

    await expect(email.forward({ to: ["ops@example.com"], text: "FYI" })).resolves.toBe(pending);
    expect(ops.forward).toHaveBeenCalledWith(
      vector.expected.inbox,
      vector.expected.id,
      { to: ["ops@example.com"], text: "FYI" },
      {},
    );

    await expect(email.attachments[0].get({ inline: true })).resolves.toBe(attachment);
    expect(ops.getAttachment).toHaveBeenCalledWith(
      vector.expected.inbox,
      vector.expected.id,
      0,
      { inline: true },
    );
  });

  it("uses typed local validation errors", async () => {
    const vector = load<Vector>("full.json");
    const { ops } = operations(messageFromWire(vector.message));
    await expect(new InboundResource(ops).fromEvent({ ...vector.event, data: null }))
      .rejects.toBeInstanceOf(E2AValidationError);
  });

  it("keeps raw MIME and transport objects out of Node console formatting", async () => {
    const vector = load<Vector>("full.json");
    const { ops } = operations(messageFromWire(vector.message));
    const email = await new InboundResource(ops).fromEvent(vector.event);

    const displayed = format(email);

    expect(displayed).toContain("Order #1234 delayed");
    expect(displayed).not.toContain("UmF3IE1JTUUNCg==");
    expect(displayed).not.toContain("rawMessage");
    expect(displayed).not.toContain("message:");
    expect(displayed).not.toContain("event:");
  });
});
