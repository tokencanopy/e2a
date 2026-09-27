/**
 * High-level client contract tests — the ergonomic `E2AClient` surface against
 * the live contract server, complementing contract.test.ts (which drives the
 * same server over raw HTTP via the scenarios.yaml interpreter and deliberately
 * never touches the ergonomic client).
 *
 * Covers the wrapper-only features that raw-HTTP scenarios cannot reach:
 * `SendOptions.wait`, `agents.delete(email, { permanent: true })`, and
 * caller-supplied `RequestOptions.idempotencyKey` replay.
 *
 * Requires env vars (same as contract.test.ts):
 *   E2A_TEST_BASE_URL  — test server URL
 *   E2A_TEST_API_KEY   — valid API key for the test user
 *   E2A_TEST_RESTRICTED_SDK_API_KEY — optional; key for the contract server's
 *     SDK-only account inside the external-sending-access enforcement cohort.
 *     The account.getSendingAccessRequest / .requestSendingAccess lifecycle
 *     below runs as that account and skips without it (a deployed staging
 *     target has no such account), mirroring the capped/over-cap accounts.
 *
 * Contract-server send topology (cmd/e2a-contract-server): the real River
 * enqueuer is wired but its outbound worker is not started, so external sends
 * can prove accepted/scheduled queue contracts without submitting real mail.
 * The deterministic terminal path is the self-send LOOPBACK, which delivers
 * synchronously — `wait: "sent"` on it observes `status: "sent"` immediately
 * rather than polling to the 15s ceiling.
 */
import { describe, it, expect } from "vitest";
import { E2AClient } from "../../src/v1/client.js";
import { E2AConflictError, E2ANotFoundError } from "../../src/v1/errors.js";

const baseUrl = process.env.E2A_TEST_BASE_URL;
const apiKey = process.env.E2A_TEST_API_KEY;
const restrictedSdkApiKey = process.env.E2A_TEST_RESTRICTED_SDK_API_KEY;

/** Shared-domain slug — must satisfy the server's ^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$
 *  rule (2–40 chars, no underscores). */
function slug(prefix: string): string {
  return `${prefix}-${Math.random().toString(36).slice(2, 10)}`;
}

describe.skipIf(!baseUrl || !apiKey)("E2AClient contract (high-level)", () => {
  const client = new E2AClient({ apiKey: apiKey!, baseUrl: baseUrl! });

  it("messages.send with wait: \"sent\" returns the terminal loopback result", async () => {
    const email = `${slug("sdkc-wait")}@agents.localhost`;
    await client.agents.create({ email });
    try {
      const res = await client.messages.send(
        email,
        { to: [email], subject: "wait contract", text: "self-send loopback" },
        { wait: "sent" },
      );
      expect(res.status).toBe("sent");
      expect(res.messageId).toMatch(/^msg_/);
      expect(res.method).toBe("loopback");
    } finally {
      await client.agents.delete(email, { permanent: true });
    }
  });

  it("messages.getMetrics keeps rates null without outward traffic and numeric with it", async () => {
    const email = `${slug("sdkc-metrics")}@agents.localhost`;
    await client.agents.create({ email });
    try {
      // A brand-new agent has no traffic. Every rate must come back null, not
      // 0 — a zero here would read as total delivery failure on a dashboard.
      const before = await client.messages.getMetrics(email);
      expect(before.agentEmail).toBe(email);
      expect(before.messagesInWindow).toBe(0);
      expect(before.counters).toEqual([]);
      expect(before.summary.accepted).toBe(0);
      expect(before.rates.deliveredRate).toBeNull();
      expect(before.rates.bounceRate).toBeNull();
      expect(before.rates.complaintRate).toBeNull();
      expect(before.rates.suppressionBlockRate).toBeNull();

      // The loopback self-send is the contract server's deterministic terminal
      // path, so the counters below cannot race an async worker.
      await client.messages.send(
        email,
        { to: [email], subject: "metrics contract", text: "self-send loopback" },
        { wait: "sent" },
      );

      const after = await client.messages.getMetrics(email);
      expect(after.messagesInWindow).toBeGreaterThan(0);
      expect(after.messagesWithLifecycle).toBeGreaterThan(0);
      expect(after.summary.accepted).toBeGreaterThan(0);
      expect(after.counters ?? []).not.toHaveLength(0);
      // Loopback traffic remains visible, but it never reaches a recipient
      // server and therefore creates no outward-delivery denominator.
      expect(after.summary.loopback).toBe(1);
      expect(after.rates.deliveredRate).toBeNull();

      // The contract server intentionally does not start its outbound worker.
      // A queued external send is therefore deterministic while still creating
      // the outward denominator needed to prove numeric SDK decoding.
      const queued = await client.messages.send(email, {
        to: ["recipient@example.net"],
        subject: "metrics outward contract",
        text: "queued external send",
      });
      expect(queued.status).toBe("accepted");

      const afterOutward = await client.messages.getMetrics(email);
      expect(afterOutward.summary.accepted).toBe(2);
      expect(afterOutward.summary.loopback).toBe(1);
      expect(afterOutward.rates.deliveredRate).toBe(0);

      // An explicit window is echoed back verbatim, so a caller can tell which
      // cohort a number describes rather than inferring it from wall clock.
      const start = new Date("2026-07-01T00:00:00Z");
      const end = new Date("2026-07-08T00:00:00Z");
      const windowed = await client.messages.getMetrics(email, { start, end });
      expect(windowed.start.toISOString()).toBe(start.toISOString());
      expect(windowed.end.toISOString()).toBe(end.toISOString());
    } finally {
      await client.agents.delete(email, { permanent: true });
    }
  });

  it("account.metrics rolls up every agent and can break down by agent", async () => {
    const email = `${slug("sdkc-acct")}@agents.localhost`;
    await client.agents.create({ email });
    try {
      await client.messages.send(
        email,
        { to: [email], subject: "account metrics", text: "self-send loopback" },
        { wait: "sent" },
      );

      // Absolute counts belong to the whole shared account, so assert the
      // contract instead: totals present, and this agent visible once broken
      // down. Both must hold no matter what else is on the account.
      const totals = await client.account.metrics();
      expect(totals.agentsTruncated).toBe(false);
      expect(totals.messagesInWindow).toBeGreaterThan(0);
      expect(totals.summary.accepted).toBeGreaterThan(0);
      expect(totals.agents ?? []).toHaveLength(0);

      const broken = await client.account.metrics({ groupBy: "agent" });
      const mine = (broken.agents ?? []).find((a) => a.agentEmail === email);
      expect(mine, "this agent must appear in the per-agent breakdown").toBeDefined();
      expect(mine!.summary.accepted).toBeGreaterThan(0);
      // The per-agent slice must never exceed the account it belongs to.
      expect(mine!.summary.accepted).toBeLessThanOrEqual(totals.summary.accepted);
    } finally {
      await client.agents.delete(email, { permanent: true });
    }
  });

  it("agents.delete with permanent: true removes the agent immediately", async () => {
    const email = `${slug("sdkc-del")}@agents.localhost`;
    await client.agents.create({ email });

    const receipt = await client.agents.delete(email, { permanent: true });
    expect(receipt.deleted).toBe(true);

    // No trash window on a permanent delete — the follow-up read is gone for
    // good and must surface as the typed not-found error (404/410 family).
    await expect(client.agents.get(email)).rejects.toBeInstanceOf(E2ANotFoundError);
  });

  it("account.apiKeys.create replays a caller-supplied idempotency key", async () => {
    const idempotencyKey = `contract-${slug("sdkc-idem")}`;
    const body = { name: "contract-idempotency-replay" };

    const first = await client.account.apiKeys.create(body, { idempotencyKey });
    try {
      const replay = await client.account.apiKeys.create(body, { idempotencyKey });

      // Same key + byte-identical body replays the cached response: no second
      // key is minted, and the one-time plaintext comes back unchanged.
      expect(replay.id).toBe(first.id);
      expect(replay.key).toBe(first.key);
    } finally {
      await client.account.apiKeys.delete(first.id);
    }
  });

  it("contacts.update / contacts.setOutreach without an etag are unconditional", async () => {
    // Regression (staging release-pipeline run 30612956986): the generated
    // layer used to emit `If-Match: undefined` when no etag was supplied,
    // turning both calls into conditional requests that always failed 412 —
    // on update the stored validator never matches "undefined", and a
    // conditional request never creates a first enrolment. Only a live server
    // can prove the header truly stays off the wire end to end.
    const email = `${slug("sdkc-ifm")}@agents.localhost`;
    const address = `${slug("sdkc-ifm-c")}@fund.vc`;
    await client.agents.create({ email });
    try {
      await client.contacts.create({ address });
      try {
        const updated = await client.contacts.update(address, { displayName: "Unconditional" });
        expect(updated.displayName).toBe("Unconditional");

        const enrolment = await client.contacts.setOutreach(email, address, { stage: "touch1" });
        expect(enrolment.stage).toBe("touch1");

        // A caller-supplied etag still arrives verbatim: the current validator
        // is accepted, and replaying it after the write proves the header was
        // really sent (412 on the now-stale value).
        const { etag } = await client.contacts.getWithETag(address);
        expect(etag).toBeTruthy();
        const guarded = await client.contacts.update(
          address,
          { displayName: "Guarded" },
          { ifMatch: etag },
        );
        expect(guarded.displayName).toBe("Guarded");
        await expect(
          client.contacts.update(address, { displayName: "Stale" }, { ifMatch: etag }),
        ).rejects.toMatchObject({ status: 412 });
      } finally {
        await client.contacts.delete(address);
      }
    } finally {
      await client.agents.delete(email, { permanent: true });
    }
  });
});

// The external-sending-access request lifecycle runs as the contract server's
// SDK-only RESTRICTED account (E2A_TEST_RESTRICTED_SDK_API_KEY): inside the
// cohort, used by no shared scenario, so it starts with no request and this
// test is its only filer — no race with the raw-HTTP scenario that must be
// the first filer on the scenario restricted account. A filed request is
// private support history with no customer delete, which is why it is kept
// off the shared primary account. Skips without the key (a deployed target
// has no such account), like the capped/over-cap accounts.
describe.skipIf(!baseUrl || !restrictedSdkApiKey)(
  "E2AClient contract (restricted account: external sending access)",
  () => {
    const client = new E2AClient({ apiKey: restrictedSdkApiKey!, baseUrl: baseUrl! });

    it("getSendingAccessRequest → requestSendingAccess → get → idempotent replay", async () => {
      await expect(client.account.getSendingAccessRequest()).rejects.toBeInstanceOf(E2ANotFoundError);

      const created = await client.account.requestSendingAccess({
        useCase: "sdk contract probe",
        recipients: "our own customers who signed up",
        expectedDailyVolume: 250,
      });
      expect(created.state).toBe("pending");
      expect(created.useCase).toBe("sdk contract probe");
      expect(created.expectedDailyVolume).toBe(250);
      expect(created.createdAt).toBeInstanceOf(Date);

      const fetched = await client.account.getSendingAccessRequest();
      expect(fetched.id).toBe(created.id);
      expect(fetched.state).toBe("pending");

      // Idempotent while pending: different fields, SAME request back (200).
      const replay = await client.account.requestSendingAccess({
        useCase: "a different reason",
        recipients: "someone else",
        expectedDailyVolume: 999,
      });
      expect(replay.id).toBe(created.id);
      expect(replay.useCase).toBe("sdk contract probe");
      expect(replay.expectedDailyVolume).toBe(250);
    });
  },
);

// The PRIMARY account is outside the contract cohort (unrestricted): there is
// nothing to request, so the server answers 409 conflict, mapped to
// E2AConflictError, and nothing is filed.
describe.skipIf(!baseUrl || !apiKey || !restrictedSdkApiKey)(
  "E2AClient contract (unrestricted account: external sending access)",
  () => {
    const client = new E2AClient({ apiKey: apiKey!, baseUrl: baseUrl! });

    it("requestSendingAccess on an unrestricted account is E2AConflictError (409 conflict)", async () => {
      const err = await client.account
        .requestSendingAccess({ useCase: "sdk contract probe", recipients: "our own customers", expectedDailyVolume: 1 })
        .then(() => undefined, (e: unknown) => e);
      expect(err).toBeInstanceOf(E2AConflictError);
      expect((err as E2AConflictError).code).toBe("conflict");
      expect((err as E2AConflictError).status).toBe(409);
      await expect(client.account.getSendingAccessRequest()).rejects.toBeInstanceOf(E2ANotFoundError);
    });
  },
);
