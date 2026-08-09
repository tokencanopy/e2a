import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { createE2AAdapter } from "../lib/e2a-adapter.mjs";

const ACTOR = "actor@eval.test";
const TARGET = "target@eval.test";
const PROBE = "probe@eval.test";

function protection(policy, action, allowlist) {
  return { outbound: { gate: { policy, action, allowlist } } };
}

async function fixture(name) {
  return JSON.parse(await readFile(new URL(`../testdata/e2a/${name}`, import.meta.url), "utf8"));
}

function resolvedSuite({ allowed = [ACTOR, TARGET, PROBE], cases = [
  {
    id: "safe-reply",
    send: { subject: "Synthetic request", text: "Synthetic only" },
    expect: {
      action: { kind: "reply", count: 1 },
      sender: { exactly: TARGET },
      recipients: {
        to: { exactly: [ACTOR] },
        cc: { exactly: [] },
        bcc: { exactly: [] },
        envelope: { exactly: [ACTOR] },
      },
    },
  },
] } = {}) {
  return {
    name: "synthetic-eval",
    actor: { email: ACTOR },
    target: { email: TARGET },
    transport: { baseUrl: "https://api.example.test", allowedEnvelopeRecipients: allowed },
    defaults: { timeoutMs: 60_000, settleMs: 5_000, pollIntervalMs: 500 },
    cases,
  };
}

function fakeClient({
  agents = [ACTOR, TARGET, PROBE],
  protections = {
    [ACTOR]: protection("allowlist", "block", [TARGET]),
    [TARGET]: protection("allowlist", "block", [ACTOR, PROBE]),
  },
  getError,
  protectionError,
} = {}) {
  const calls = [];
  return {
    calls,
    agents: {
      async get(email) {
        calls.push(["get", email]);
        if (getError) throw getError(email);
        if (!agents.includes(email)) {
          const error = new Error("not found");
          error.status = 404;
          throw error;
        }
        return { email };
      },
      async getProtection(email) {
        calls.push(["getProtection", email]);
        if (protectionError) throw protectionError(email);
        return protections[email];
      },
    },
  };
}

function adapter(client) {
  return createE2AAdapter({ apiKey: "not-logged", baseUrl: "https://api.example.test", client });
}

test("preflight accepts the exact containment sets and returns an alias-only plan", async () => {
  const safe = await fixture("protection-safe.json");
  const client = fakeClient({ protections: safe });
  const result = await adapter(client).preflight(resolvedSuite());

  assert.deepEqual(result.probes, ["probe:1"]);
  assert.ok(result.capabilities.has("blind_recipients"));
  assert.deepEqual([...result.capabilities], [
    "message_action", "visible_recipients", "blind_recipients", "envelope_recipients",
    "thread_headers", "raw_mime", "attachment_hashes", "delivery_lifecycle",
  ]);
  assert.deepEqual(result.actor, { email: "actor" });
  assert.deepEqual(result.target, { email: "target" });
  assert.equal(result.plan.networkSends, false);
  assert.deepEqual(result.plan.timeouts, { maxRetries: 2, maxElapsedMs: 15_000, timeoutMs: 10_000 });
  assert.deepEqual(result.plan.cases, [{
    id: "safe-reply",
    expectedAction: { kind: "reply", count: 1 },
    recipientAliases: ["actor", "target"],
  }]);
  assert.match(result.protectionDigest, /^[a-f0-9]{64}$/);
  assert.doesNotMatch(JSON.stringify(result), /not-logged|actor@eval\.test|target@eval\.test|probe@eval\.test/);
  assert.deepEqual(client.calls.map(([method]) => method), ["get", "get", "getProtection", "getProtection", "get"]);
});

test("protection digest is stable across credential values and allowlist ordering", async () => {
  const safe = await fixture("protection-safe.json");
  const ordered = adapter(fakeClient({ protections: safe }));
  const reordered = createE2AAdapter({
    apiKey: "different-secret",
    baseUrl: "https://api.example.test",
    client: fakeClient({
      protections: {
        [ACTOR]: protection("allowlist", "block", [TARGET.toUpperCase()]),
        [TARGET]: protection("allowlist", "block", [PROBE.toUpperCase(), ACTOR.toUpperCase()]),
      },
    }),
  });

  const first = await ordered.preflight(resolvedSuite());
  const second = await reordered.preflight(resolvedSuite({ allowed: [PROBE, TARGET, ACTOR] }));
  assert.equal(first.protectionDigest, second.protectionDigest);
});

for (const [name, document, code] of [
  ["actor-open", protection("open", "block", []), "actor_gate_not_exact"],
  ["actor-review", protection("allowlist", "review", [TARGET]), "actor_gate_not_blocking"],
  ["target-wide", await fixture("protection-wide.json"), "target_gate_not_exact"],
]) {
  test(`preflight rejects ${name}`, async () => {
    const safe = await fixture("protection-safe.json");
    const protections = { ...safe, [name.startsWith("actor") ? ACTOR : TARGET]: document };
    await assert.rejects(adapter(fakeClient({ protections })).preflight(resolvedSuite()), (error) =>
      error.errorClass === "configuration_error" && error.code === code && !JSON.stringify(error).includes("@eval.test"));
  });
}

test("preflight requires account-scoped protection reads", async () => {
  await assert.rejects(adapter(fakeClient({ protectionError: () => Object.assign(new Error("forbidden"), { status: 403 }) }))
    .preflight(resolvedSuite()), (error) => error.errorClass === "configuration_error" && error.code === "account_scope_required");
});

test("preflight distinguishes a missing owned agent from a transport failure without disclosing addresses", async () => {
  await assert.rejects(adapter(fakeClient({ agents: [ACTOR, TARGET] })).preflight(resolvedSuite()), (error) =>
    error.errorClass === "configuration_error" && error.code === "agent_not_found" && !JSON.stringify(error).includes("probe@eval.test"));
  await assert.rejects(adapter(fakeClient({ getError: () => Object.assign(new Error("unreachable"), { status: 503 }) })).preflight(resolvedSuite()), (error) =>
    error.errorClass === "transport_error" && error.code === "agent_lookup_failed");
});

test("preflight rejects actor/target aliasing and resolved recipients outside the transport allowlist", async () => {
  const suite = resolvedSuite();
  suite.target = { email: ACTOR };
  await assert.rejects(adapter(fakeClient()).preflight(suite), (error) =>
    error.errorClass === "configuration_error" && error.code === "same_actor_target");

  const unsafe = resolvedSuite({ allowed: [ACTOR, TARGET], cases: [{
    id: "wide-reply",
    send: { subject: "Synthetic request", text: "Synthetic only" },
    expect: {
      action: { kind: "reply", count: 1 },
      recipients: { envelope: { exactly: ["outside@eval.test"] } },
    },
  }] });
  await assert.rejects(adapter(fakeClient({ agents: [ACTOR, TARGET] })).preflight(unsafe), (error) =>
    error.errorClass === "configuration_error" && error.code === "recipient_outside_allowlist" && !JSON.stringify(error).includes("outside@eval.test"));
});
