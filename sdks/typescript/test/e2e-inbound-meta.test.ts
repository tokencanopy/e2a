/**
 * Live ergonomic coverage: client.inbound.fromEvent and the top-level
 * client.listen() WebSocket stream. See test/e2e.test.ts's header for the
 * coverage-gate contract this suite participates in (that file owns `info`).
 *
 * This file owns: inbound.fromEvent, listen.
 *
 * `client.meta` (the raw generated PromiseMetaApi — `getInfo` /
 * `getInfoWithHttpInfo`) is INTERNAL and is not part of this gate's
 * denominator at all: see test/coverage/introspect.ts's module comment for
 * why TypeScript `private` can't be used to detect that, and what
 * runtime-observable signal is used instead. `client.info()` — the actual
 * public entry point wrapping meta.getInfo — is what's covered (in
 * test/e2e.test.ts).
 */
import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { E2AClient } from "../src/v1/index.js";
import type { WebhookEvent } from "../src/v1/index.js";
import { walkErgonomicSurface } from "./coverage/introspect.js";
import { recordSurface, recordCovered, flushCoverage } from "./coverage/recorder.js";
import { loadLiveEnv, uniqueSlug, poll } from "./coverage/helpers.js";

const env = loadLiveEnv();

describe.skipIf(!env)("ts sdk live e2e: inbound + meta + listen", () => {
  let client: E2AClient;
  let bot: string;
  const cleanup: Array<() => Promise<unknown>> = [];

  beforeAll(async () => {
    client = new E2AClient({ apiKey: env!.apiKey, baseUrl: env!.baseUrl });
    recordSurface(walkErgonomicSurface(client));
    bot = `${uniqueSlug("inboundcov")}@${env!.sharedDomain}`;
    const created = await client.agents.create({ email: bot, name: "coverage inbound+meta+listen" });
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
    "inbound.fromEvent resolves a real email.received envelope into an InboundEmail facade",
    async () => {
      const subject = `inbound-fromEvent-coverage ${Date.now()}`;
      const sent = await client.messages.send(bot, { to: [bot], subject, text: "inbound.fromEvent coverage" });
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

      const email = await client.inbound.fromEvent(event);
      expect(email.id).toBe(found!.id);
      expect(email.inbox).toBe(bot);
      expect(email.subject).toBe(subject);
      expect(email.text).toContain("inbound.fromEvent coverage");
      recordCovered("inbound.fromEvent");
    },
    30_000,
  );

  it(
    "listen() streams a real WSEvent for a self-send",
    async () => {
      const stream = client.listen(bot);
      try {
        const subject = `listen-coverage ${Date.now()}`;
        // Fire the send AFTER the stream is open (connect() is called
        // synchronously in the WSStream constructor) so we don't race the
        // handshake.
        const sendPromise = client.messages.send(bot, { to: [bot], subject, text: "listen coverage" });

        const iterator = stream[Symbol.asyncIterator]();
        let matchedDeliveredTo: string | undefined;
        const deadline = Date.now() + 20_000;
        while (matchedDeliveredTo === undefined && Date.now() < deadline) {
          const remaining = Math.max(1, deadline - Date.now());
          const timedOut = Symbol("timeout");
          const next = await Promise.race([
            iterator.next(),
            new Promise<typeof timedOut>((resolve) => setTimeout(() => resolve(timedOut), remaining)),
          ]);
          if (next === timedOut) break;
          if (next.done) break;
          const data = next.value.data as { message_id?: string; delivered_to?: string } | undefined;
          if (next.value.type === "email.received" && data?.delivered_to === bot) {
            matchedDeliveredTo = data.delivered_to;
          }
        }
        await sendPromise;
        expect(matchedDeliveredTo, "listen() must yield an email.received event for the self-send within ~20s").toBe(
          bot,
        );
        recordCovered("listen");
      } finally {
        stream.close();
      }
    },
    30_000,
  );
});
