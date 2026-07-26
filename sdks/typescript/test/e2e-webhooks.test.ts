/**
 * Live ergonomic coverage: client.webhooks.*. See test/e2e.test.ts's header
 * for the coverage-gate contract this suite participates in.
 *
 * This file owns: webhooks.create, webhooks.get, webhooks.list,
 * webhooks.update, webhooks.rotateSecret, webhooks.deliveries,
 * webhooks.test, webhooks.delete, webhooks.fetchMessage.
 */
import { describe, it, expect, beforeAll, afterAll } from "vitest";
import {
  E2AClient,
  CreateWebhookRequestEventsEnum,
  UpdateWebhookRequestEventsEnum,
  TestWebhookRequestTypeEnum,
} from "../src/v1/index.js";
import type { WebhookEvent } from "../src/v1/index.js";
import { walkErgonomicSurface } from "./coverage/introspect.js";
import { recordSurface, recordCovered, flushCoverage } from "./coverage/recorder.js";
import { loadLiveEnv, uniqueSlug, poll } from "./coverage/helpers.js";

const env = loadLiveEnv();
const HOOK_URL = "https://example.com/e2e-sdk-coverage-webhook";

describe.skipIf(!env)("ts sdk live e2e: webhooks", () => {
  let client: E2AClient;

  beforeAll(() => {
    client = new E2AClient({ apiKey: env!.apiKey, baseUrl: env!.baseUrl });
    recordSurface(walkErgonomicSurface(client));
  });

  afterAll(() => {
    flushCoverage();
  });

  it(
    "create → get → list → update → rotateSecret → test → deliveries → delete",
    async () => {
      const created = await client.webhooks.create({
        url: HOOK_URL,
        events: [CreateWebhookRequestEventsEnum.EmailReceived, CreateWebhookRequestEventsEnum.EmailSent],
        description: "sdk coverage full CRUD",
      });
      expect(created.id.startsWith("wh_")).toBe(true);
      expect(created.url).toBe(HOOK_URL);
      expect(created.enabled).toBe(true);
      expect(created.signingSecret.startsWith("whsec_")).toBe(true);
      recordCovered("webhooks.create");
      const id = created.id;
      let deleted = false;

      try {
        const got = await client.webhooks.get(id);
        expect(got.id).toBe(id);
        expect(got.url).toBe(HOOK_URL);
        recordCovered("webhooks.get");

        const list = await client.webhooks.list({ limit: 50 }).toArray({ limit: 200 });
        expect(list.some((w) => w.id === id)).toBe(true);
        recordCovered("webhooks.list");

        const updated = await client.webhooks.update(id, {
          enabled: false,
          events: [UpdateWebhookRequestEventsEnum.EmailFailed],
          description: "sdk coverage patched",
        });
        expect(updated.enabled).toBe(false);
        expect(updated.events).toEqual(["email.failed"]);
        expect(updated.description).toBe("sdk coverage patched");
        recordCovered("webhooks.update");

        const rotated = await client.webhooks.rotateSecret(id);
        expect(rotated.signingSecret.startsWith("whsec_")).toBe(true);
        expect(rotated.signingSecret).not.toBe(created.signingSecret);
        expect(rotated.previousSecretExpiresAt).toBeTruthy();
        recordCovered("webhooks.rotateSecret");

        // Re-enable so testWebhook doesn't 409 webhook_disabled.
        await client.webhooks.update(id, { enabled: true });

        const tested = await client.webhooks.test(id, { type: TestWebhookRequestTypeEnum.EmailReceived });
        expect(tested.deliveryId.length).toBeGreaterThan(0);
        recordCovered("webhooks.test");

        const deliveries = await poll(
          async () => {
            const page = await client.webhooks.deliveries(id, { limit: 20 }).toArray({ limit: 50 });
            return page.some((d) => d.id === tested.deliveryId) ? page : undefined;
          },
          { attempts: 10, delayMs: 1000 },
        );
        expect(deliveries, "the just-scheduled test delivery must appear in webhooks.deliveries").toBeTruthy();
        const mine = deliveries!.find((d) => d.id === tested.deliveryId)!;
        expect(mine.id).toBe(tested.deliveryId);
        expect(typeof mine.attempts).toBe("number");
        recordCovered("webhooks.deliveries");

        const del = await client.webhooks.delete(id);
        expect(del.deleted).toBe(true);
        expect(del.id).toBe(id);
        deleted = true;
        recordCovered("webhooks.delete");
      } finally {
        if (!deleted) await client.webhooks.delete(id).catch(() => {});
      }
    },
    30_000,
  );

  describe("fetchMessage", () => {
    let bot: string;
    const cleanup: Array<() => Promise<unknown>> = [];

    beforeAll(async () => {
      bot = `${uniqueSlug("whfetch")}@${env!.sharedDomain}`;
      const created = await client.agents.create({ email: bot, name: "coverage webhooks.fetchMessage" });
      expect(created.email).toBe(bot);
      cleanup.push(() => client.agents.delete(bot));
    }, 20_000);

    afterAll(async () => {
      for (const fn of cleanup.splice(0)) {
        await fn().catch(() => {});
      }
    });

    it(
      "resolves the real MessageView referenced by a synthetic email.received envelope",
      async () => {
        const subject = `webhooks-fetchMessage-coverage ${Date.now()}`;
        const sent = await client.messages.send(bot, { to: [bot], subject, text: "fetchMessage coverage" });
        expect(sent.messageId).toBeTruthy();

        const found = await poll(
          async () => {
            const msgs = await client.messages.list(bot, { direction: "inbound", limit: 20 }).toArray({ limit: 20 });
            return msgs.find((m) => m.subject === subject);
          },
          { attempts: 12, delayMs: 1500 },
        );
        expect(found, `an inbound message with subject "${subject}" must appear within ~18s`).toBeTruthy();

        const event: WebhookEvent = {
          id: `evt_synthetic_${Date.now()}`,
          type: "email.received",
          schema_version: "1",
          created_at: new Date().toISOString(),
          data: { message_id: found!.id, delivered_to: bot },
        };

        const resolved = await client.webhooks.fetchMessage(event);
        expect(resolved.id).toBe(found!.id);
        expect(resolved.deliveredTo).toBe(bot);
        expect(resolved.subject).toBe(subject);
        recordCovered("webhooks.fetchMessage");
      },
      30_000,
    );
  });
});
