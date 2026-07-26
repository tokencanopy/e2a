/**
 * Live ergonomic coverage: client.conversations.* and client.events.*.
 * See test/e2e.test.ts's header for the coverage-gate contract this suite
 * participates in.
 *
 * This file owns: conversations.list, conversations.get, events.list,
 * events.get, events.redeliver.
 */
import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { E2AClient, CreateWebhookRequestEventsEnum } from "../src/v1/index.js";
import { walkErgonomicSurface } from "./coverage/introspect.js";
import { recordSurface, recordCovered, flushCoverage } from "./coverage/recorder.js";
import { loadLiveEnv, uniqueSlug, poll } from "./coverage/helpers.js";

const env = loadLiveEnv();

describe.skipIf(!env)("ts sdk live e2e: conversations + events", () => {
  let client: E2AClient;
  let bot: string;
  const cleanup: Array<() => Promise<unknown>> = [];

  beforeAll(async () => {
    client = new E2AClient({ apiKey: env!.apiKey, baseUrl: env!.baseUrl });
    recordSurface(walkErgonomicSurface(client));
    bot = `${uniqueSlug("convcov")}@${env!.sharedDomain}`;
    const created = await client.agents.create({ email: bot, name: "coverage conversations+events" });
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
    "conversations.list / conversations.get surface a real loopback thread",
    async () => {
      const subject = `conv-coverage ${Date.now()}`;
      const sent = await client.messages.send(bot, { to: [bot], subject, text: "conversations coverage" });
      expect(sent.messageId).toBeTruthy();

      const found = await poll(
        async () => {
          const msgs = await client.messages.list(bot, { direction: "inbound", limit: 20 }).toArray({ limit: 20 });
          return msgs.find((m) => m.subject === subject);
        },
        { attempts: 12, delayMs: 1500 },
      );
      expect(found, `an inbound message with subject "${subject}" must appear within ~18s`).toBeTruthy();
      expect(found!.conversationId).toBeTruthy();
      const conversationId: string = found!.conversationId!;

      // Reply so the thread carries both an inbound and an outbound leg.
      await client.messages.reply(bot, found!.id, { text: "conversations coverage reply" });

      const list = await poll(
        async () => {
          const items = await client.conversations.list(bot, { limit: 50 }).toArray({ limit: 200 });
          const mine = items.find((c) => c.id === conversationId);
          return mine && mine.messageCount >= 2 ? mine : undefined;
        },
        { attempts: 10, delayMs: 1500 },
      );
      expect(list, `conversation ${conversationId} must appear in conversations.list with both legs`).toBeTruthy();
      expect(list!.latestSubject.length).toBeGreaterThan(0);
      recordCovered("conversations.list");

      const detail = await client.conversations.get(bot, conversationId);
      expect(detail.id).toBe(conversationId);
      expect(detail.messages.length).toBeGreaterThanOrEqual(2);
      expect(detail.messages.some((m) => m.id === found!.id)).toBe(true);
      recordCovered("conversations.get");
    },
    30_000,
  );

  it(
    "events.list / events.get / events.redeliver on a real email.sent event",
    async () => {
      const hookUrl = "https://example.com/e2e-sdk-coverage-webhook";
      const hook = await client.webhooks.create({
        url: hookUrl,
        events: [CreateWebhookRequestEventsEnum.EmailSent],
        description: "conv-events coverage",
      });
      cleanup.push(() => client.webhooks.delete(hook.id));

      const subject = `events-coverage ${Date.now()}`;
      const sent = await client.messages.send(bot, { to: [env!.sinkEmail], subject, text: "events coverage" });
      expect(sent.messageId).toBeTruthy();
      const messageId = sent.messageId;

      const event = await poll(
        async () => {
          const events = await client.events
            .list({ type: "email.sent", agentEmail: bot, limit: 20 })
            .toArray({ limit: 50 });
          return events.find((e) => e.messageId === messageId);
        },
        { attempts: 20, delayMs: 1500 },
      );
      expect(event, `an email.sent event for ${messageId} must appear in events.list within ~30s`).toBeTruthy();
      expect(event!.type).toBe("email.sent");
      recordCovered("events.list");

      const got = await client.events.get(event!.id);
      expect(got.id).toBe(event!.id);
      expect(got.type).toBe("email.sent");
      expect(got.messageId).toBe(messageId);
      recordCovered("events.get");

      const redelivered = await client.events.redeliver(event!.id, { webhookId: hook.id });
      expect(redelivered.eventId).toBe(event!.id);
      expect(redelivered.status).toBeTruthy();
      recordCovered("events.redeliver");
    },
    45_000,
  );
});
