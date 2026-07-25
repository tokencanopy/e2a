/**
 * Live ergonomic e2e for the TypeScript SDK against a RUNNING server (staging).
 *
 * This exercises the real hand-written ergonomic surface (client.messages.* /
 * client.agents.* / client.info), so a green run attests the published SDK
 * actually works against a live deployment — the parity signal the contract
 * runner (raw HTTP) can't give.
 *
 * Gated on staging creds; skips cleanly when absent, so it stays inert in the
 * default `npm test`. Env is aligned with the contract runner + the Python live
 * test (E2A_TEST_* naming):
 *   E2A_TEST_BASE_URL     e.g. https://api-staging.e2a.dev (or a local tunnel)
 *   E2A_TEST_API_KEY      an API key for the target account
 *   E2A_TEST_AGENT_EMAIL  a shared-domain inbox on that account (self-send target)
 *
 * Run:
 *   E2A_TEST_BASE_URL=… E2A_TEST_API_KEY=… E2A_TEST_AGENT_EMAIL=… \
 *     npm run test:live --workspace @e2a/sdk
 *
 * This file plus its e2e-*.test.ts siblings are collectively the ergonomic
 * coverage-gate suite: every method test/coverage/introspect.ts's runtime walk
 * finds on a built E2AClient must be exercised (with an assertion on the
 * RESULT) somewhere across these files, or be in gate.mjs's ALLOWLIST. Run
 * `npm run coverage:gate --workspace @e2a/sdk` after `test:live` to check.
 * This file owns: info, agents.create, agents.delete, agents.list,
 * messages.send, messages.list, messages.get, messages.reply.
 */
import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { E2AClient, E2ANotFoundError } from "../src/v1/index.js";
import { walkErgonomicSurface } from "./coverage/introspect.js";
import { recordSurface, recordCovered, flushCoverage } from "./coverage/recorder.js";

const BASE_URL = process.env.E2A_TEST_BASE_URL || "";
const API_KEY = process.env.E2A_TEST_API_KEY || "";
const AGENT = process.env.E2A_TEST_AGENT_EMAIL || "";
const live = Boolean(BASE_URL && API_KEY && AGENT);

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

describe.skipIf(!live)("ts sdk live e2e", () => {
  let client: E2AClient;

  beforeAll(() => {
    client = new E2AClient({ apiKey: API_KEY, baseUrl: BASE_URL });
    // Feeds the coverage-gate denominator even if every other test in the
    // file somehow short-circuits — mirrors 27-mcp-agents-messages.test.ts's
    // tools/list-in-before() convention.
    recordSurface(walkErgonomicSurface(client));
  });

  afterAll(() => {
    flushCoverage();
  });

  it("info() reports the deployment", async () => {
    const info = await client.info();
    expect(typeof info.version).toBe("string");
    expect(info.version.length).toBeGreaterThan(0);
    expect(info.sharedDomain.length).toBeGreaterThan(0);
    recordCovered("info");
  });

  it("agents.list() pages through a freshly-created agent", async () => {
    // Do NOT assume AGENT (E2A_TEST_AGENT_EMAIL) was ever agents.create()'d —
    // it's only guaranteed to be a valid shared-domain address other tests can
    // self-send through, not a live agent record. Create our own fixture so
    // this test's pass/fail depends only on agents.list(), not seed state.
    const domain = AGENT.split("@")[1];
    const email = `ts-sdk-live-list-${Date.now().toString(36)}@${domain}`;
    const created = await client.agents.create({ email, name: "ts-sdk live agents.list" });
    expect(created.email).toBe(email);
    try {
      const found = await client.agents.list({ limit: 50 }).toArray({ limit: 200 });
      expect(found.some((a) => a.email === email)).toBe(true);
      recordCovered("agents.list");
    } finally {
      await client.agents.delete(email);
    }
  });

  it("agents.create → send → find in inbox → get → reply (self loopback) → delete", async () => {
    // Use a FRESH shared-domain agent (no protection) so the self-send delivers
    // immediately and loops back — the seeded conformance inbox may hold outbound
    // for review, which would never land in the inbox. Same domain as AGENT.
    const domain = AGENT.split("@")[1];
    const bot = `ts-sdk-live-${Date.now().toString(36)}@${domain}`;
    const created = await client.agents.create({ email: bot, name: "ts-sdk live e2e" });
    expect(created.email).toBe(bot);
    recordCovered("agents.create");
    try {
      const subject = `ts-sdk-live ${Date.now()}`;
      const bodyText = "Hello from the TypeScript SDK live e2e";

      const sent = await client.messages.send(bot, { to: [bot], subject, text: bodyText });
      expect(sent.messageId).toBeTruthy();
      expect(["sent", "accepted"]).toContain(sent.status);
      recordCovered("messages.send");

      // A self-send loopback lands an INBOUND copy in the same inbox; poll for it.
      // Filter to inbound so the just-sent outbound copy (same subject) can't match.
      let found: { id: string } | undefined;
      for (let i = 0; i < 12 && !found; i++) {
        const msgs = await client.messages.list(bot, { direction: "inbound", limit: 20 }).toArray({ limit: 20 });
        found = msgs.find((m) => m.subject === subject);
        if (!found) await sleep(1500);
      }
      expect(found, `an inbound message with subject "${subject}" must appear within ~18s`).toBeTruthy();
      recordCovered("messages.list");

      const full = await client.messages.get(bot, found!.id);
      expect(full.id).toBe(found!.id);
      expect(full.subject).toBe(subject);
      // The delivered body is under `parsed` (inbound-extracted MIME), not `body`
      // (the held-outbound draft field, which is null for inbound by design).
      expect(full.parsed?.text ?? "").toContain(bodyText);
      recordCovered("messages.get");

      const reply = await client.messages.reply(bot, found!.id, {
        text: "Reply from the TS SDK live e2e",
      });
      expect(reply.messageId).toBeTruthy();
      // Fresh unprotected inbox → the reply sends immediately (same as the send).
      expect(["sent", "accepted"]).toContain(reply.status);
      recordCovered("messages.reply");
    } finally {
      const deleted = await client.agents.delete(bot);
      expect(deleted.deleted).toBe(true);
      expect(deleted.email).toBe(bot);
      recordCovered("agents.delete");
    }
  }, 40_000);

  it("getMessage on a nonexistent id rejects with E2ANotFoundError", async () => {
    await expect(
      client.messages.get(AGENT, `msg_nonexistent_${Date.now()}`),
    ).rejects.toBeInstanceOf(E2ANotFoundError);
  });
});
