import assert from "node:assert/strict";
import test from "node:test";

import {
  buildProtectionDocument,
  E2aSetupClient,
  protectionMatchesPolicy,
} from "../setup.mjs";

function policy(overrides = {}) {
  return {
    inbound: {
      mode: "addresses",
      addresses: ["buyer@customer.test"],
      domains: [],
    },
    outbound: { requireReview: true },
    screening: { promptInjection: true },
    ...overrides,
  };
}

const current = {
  inbound: {
    gate: { policy: "open", allowlist: [], action: "flag" },
    scan: { sensitivity: "off" },
  },
  outbound: {
    gate: { policy: "domain", allowlist: [], action: "flag" },
    scan: { sensitivity: "low" },
  },
  holds: {
    ttl_seconds: 86400,
    on_expiry: "approve",
    suppress_notifications: true,
  },
};

test("protection plan allows selected senders and reviews every non-match", () => {
  const next = buildProtectionDocument(current, policy());

  assert.deepEqual(next.inbound, {
    gate: {
      policy: "allowlist",
      allowlist: ["buyer@customer.test"],
      action: "review",
    },
    scan: { sensitivity: "medium" },
  });
  assert.deepEqual(next.outbound, {
    gate: { policy: "allowlist", allowlist: [], action: "review" },
    scan: { sensitivity: "low" },
  });
  assert.deepEqual(next.holds, {
    ttl_seconds: 86400,
    on_expiry: "reject",
    suppress_notifications: false,
  });
  assert.equal(protectionMatchesPolicy(next, policy()), true);
});

test("domain mode and warned opt-outs map to existing protection fields", () => {
  const optedOut = policy({
    inbound: { mode: "domains", addresses: [], domains: ["customer.test"] },
    outbound: { requireReview: false },
    screening: { promptInjection: false },
  });
  const next = buildProtectionDocument(current, optedOut);

  assert.deepEqual(next.inbound, {
    gate: { policy: "domain", allowlist: ["customer.test"], action: "review" },
    scan: { sensitivity: "off" },
  });
  assert.equal(next.outbound.gate.policy, "domain");
  assert.equal(next.outbound.gate.action, "flag");
  assert.equal(next.outbound.scan.sensitivity, "off");
  assert.equal(protectionMatchesPolicy(next, optedOut), true);
});

test("verification rejects security-relevant drift", () => {
  const expected = buildProtectionDocument(current, policy());
  const drifted = structuredClone(expected);
  drifted.inbound.gate.action = "flag";

  assert.equal(protectionMatchesPolicy(drifted, policy()), false);
});

test("setup client uses the existing account CLI session without putting its key in argv", async () => {
  const cliCalls = [];
  const httpCalls = [];
  let remote = structuredClone(current);
  const execFileImpl = (_command, args, _options, callback) => {
    cliCalls.push([...args]);
    const tail = args.join(" ");
    if (tail === "whoami --json") callback(null, '{"scope":"account"}\n');
    else if (tail === "agents get support@example.test --json") callback(null, '{}\n');
    else if (tail === "config get api_url") callback(null, "https://e2a.example.test\n");
    else if (tail === "config get api_key") callback(null, "e2a_acct_synthetic\n");
    else if (tail.includes("keys create")) callback(null, '{"id":"key_1","key":"e2a_agt_synthetic","scope":"agent","agentEmail":"support@example.test"}\n');
    else if (tail === "keys delete key_1") callback(null, "revoked key_1\n");
    else callback(new Error("unexpected synthetic command"), "");
  };
  const fetchImpl = async (url, options) => {
    httpCalls.push({ url, options });
    if (options.method === "PUT") remote = JSON.parse(options.body);
    return { ok: true, status: 200, async json() { return structuredClone(remote); } };
  };
  const client = new E2aSetupClient({
    command: "/opt/e2a/bin/e2a",
    execFileImpl,
    fetchImpl,
  });

  const preflight = await client.preflight("support@example.test");
  const created = await client.createAgentKey("support@example.test");
  const next = buildProtectionDocument(await client.getProtection("support@example.test"), policy());
  await client.replaceProtection("support@example.test", next);
  await client.revokeKey(created.id);

  assert.equal(preflight.deploymentUrl, "https://e2a.example.test");
  assert.equal(created.key, "e2a_agt_synthetic");
  assert.equal(httpCalls[1].options.headers.authorization, "Bearer e2a_acct_synthetic");
  assert.equal(httpCalls[1].options.method, "PUT");
  assert.doesNotMatch(JSON.stringify(cliCalls), /e2a_acct_synthetic/);
  assert.match(httpCalls[1].url, /support%40example\.test\/protection$/);
  client.clearAccountCredential();
  await assert.rejects(client.getProtection("support@example.test"), /Run setup preflight/);
});

test("setup failures redact CLI output and credentials", async () => {
  const client = new E2aSetupClient({
    command: "/opt/e2a/bin/e2a",
    execFileImpl(_command, _args, _options, callback) {
      callback(new Error("e2a_acct_should_not_escape"), "e2a_acct_should_not_escape");
    },
  });

  await assert.rejects(client.preflight("support@example.test"), (error) => {
    assert.doesNotMatch(error.message, /e2a_acct_should_not_escape/);
    return true;
  });
});
