/**
 * Live ergonomic coverage: the rest of client.messages.* not already exercised
 * by test/e2e.test.ts (send/list/get/reply). See that file's header for the
 * coverage-gate contract this suite participates in.
 *
 * This file owns: messages.getLifecycle, messages.updateLabels,
 * messages.forward, messages.delete, messages.restore, messages.getAttachment.
 */
import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { E2AClient } from "../src/v1/index.js";
import { walkErgonomicSurface } from "./coverage/introspect.js";
import { recordSurface, recordCovered, flushCoverage } from "./coverage/recorder.js";
import { loadLiveEnv, uniqueSlug, poll } from "./coverage/helpers.js";

const env = loadLiveEnv();

describe.skipIf(!env)("ts sdk live e2e: messages", () => {
  let client: E2AClient;
  let bot: string;
  const cleanup: Array<() => Promise<unknown>> = [];

  beforeAll(async () => {
    client = new E2AClient({ apiKey: env!.apiKey, baseUrl: env!.baseUrl });
    recordSurface(walkErgonomicSurface(client));
    bot = `${uniqueSlug("msgcov")}@${env!.sharedDomain}`;
    const created = await client.agents.create({ email: bot, name: "coverage messages" });
    expect(created.email).toBe(bot);
    cleanup.push(() => client.agents.delete(bot));
  }, 20_000);

  afterAll(async () => {
    for (const fn of cleanup.splice(0)) {
      await fn().catch(() => {});
    }
    flushCoverage();
  });

  it(
    "getLifecycle, updateLabels, forward, delete, restore on a real loopback message",
    async () => {
      const subject = `msgs-coverage ${Date.now()}`;
      const sent = await client.messages.send(bot, { to: [bot], subject, text: "loopback for messages coverage" });
      expect(sent.messageId).toBeTruthy();

      const found = await poll(
        async () => {
          const msgs = await client.messages.list(bot, { direction: "inbound", limit: 20 }).toArray({ limit: 20 });
          return msgs.find((m) => m.subject === subject);
        },
        { attempts: 12, delayMs: 1500 },
      );
      expect(found, `an inbound message with subject "${subject}" must appear within ~18s`).toBeTruthy();
      const id = found!.id;

      const lifecycle = await client.messages.getLifecycle(bot, id);
      expect(Array.isArray(lifecycle.items)).toBe(true);
      expect(lifecycle.items.length).toBeGreaterThan(0);
      for (const t of lifecycle.items) {
        expect(t.id).toBeTruthy();
        expect(t.messageId).toBe(id);
        expect(t.stage).toBeTruthy();
        expect(t.outcome).toBeTruthy();
      }
      recordCovered("messages.getLifecycle");

      const label = `coverage-${uniqueSlug("label")}`;
      const labeled = await client.messages.updateLabels(bot, id, { addLabels: [label] });
      expect(labeled.messageId).toBe(id);
      expect(labeled.labels).toContain(label);
      const afterLabel = await client.messages.get(bot, id);
      expect(afterLabel.labels).toContain(label);
      recordCovered("messages.updateLabels");

      const forwardTarget = env!.sinkEmail;
      const forwarded = await client.messages.forward(bot, id, {
        to: [forwardTarget],
        text: "forwarded during messages coverage",
      });
      expect(forwarded.messageId).toBeTruthy();
      expect(["sent", "accepted"]).toContain(forwarded.status);
      recordCovered("messages.forward");

      const deleted = await client.messages.delete(bot, id);
      expect(deleted.deleted).toBe(true);
      expect(deleted.id).toBe(id);
      recordCovered("messages.delete");

      const trashed = await client.messages.list(bot, { deleted: true, limit: 20 }).toArray({ limit: 20 });
      expect(trashed.some((m) => m.id === id)).toBe(true);

      const restored = await client.messages.restore(bot, id);
      expect(restored.id).toBe(id);
      expect(restored.deletedAt).toBeUndefined();
      recordCovered("messages.restore");

      // read_status defaults to "unread" for direction=inbound (per
      // api/openapi.yaml) — the earlier messages.get() calls above already
      // marked this message read, so the live-listing check must say so
      // explicitly (mirrors tests/e2e-prod/suites/24-trash-lifecycle.test.ts's
      // read_status:"all" convention) rather than silently see zero results.
      const live = await client.messages
        .list(bot, { direction: "inbound", readStatus: "all", limit: 20 })
        .toArray({ limit: 20 });
      expect(live.some((m) => m.id === id)).toBe(true);
    },
    30_000,
  );

  it(
    "getAttachment returns metadata + a download_url for a real sent attachment",
    async () => {
      const attachText = "hello ts-sdk coverage attachment";
      const attachB64 = Buffer.from(attachText, "utf8").toString("base64");
      const filename = "coverage.txt";
      const contentType = "text/plain";

      const sent = await client.messages.send(bot, {
        to: [env!.sinkEmail],
        subject: `msgs-coverage attachment ${Date.now()}`,
        text: "see attached",
        attachments: [{ filename, contentType, data: attachB64 }],
      });
      expect(sent.messageId).toBeTruthy();
      const messageId = sent.messageId;

      const withAttachment = await poll(
        async () => {
          const msg = await client.messages.get(bot, messageId);
          return msg.attachments.length > 0 ? msg : undefined;
        },
        { attempts: 24, delayMs: 1000 },
      );
      expect(
        withAttachment,
        "no attachment fixture available — a missing fixture is a broken test, not a reason to pass",
      ).toBeTruthy();

      const attachment = await client.messages.getAttachment(bot, messageId, 0);
      expect(attachment.index).toBe(0);
      expect(attachment.filename).toBe(filename);
      expect(attachment.contentType).toBe(contentType);
      expect(attachment.sizeBytes).toBe(attachText.length);
      expect(attachment.downloadUrl.length).toBeGreaterThan(0);
      recordCovered("messages.getAttachment");
    },
    30_000,
  );
});
