import assert from "node:assert/strict";
import test from "node:test";

import {
  createPolicy,
  normalizePolicy,
  planDigest,
  renderPlan,
  validatePolicy,
} from "../policy.mjs";

const completeInput = {
  task: {
    profile: "customer-support",
    objective: "Answer product questions and escalate billing requests.",
    instructions: "Use the approved knowledge base. Never promise refunds.",
  },
  mailbox: {
    agentEmail: "support@example.com",
    ownerEmail: "owner@example.com",
  },
  inbound: {
    mode: "addresses",
    addresses: ["VIP@Customer.test", "buyer@customer.test"],
    domains: [],
    fallback: "review",
  },
  outbound: {
    requireReview: true,
    ccOwner: true,
  },
  screening: {
    promptInjection: true,
  },
  runtime: {
    adapter: "codex",
    command: "/usr/local/bin/codex",
    workdir: "/srv/autopilot/support",
    sandbox: "custom",
  },
  service: {
    manager: "systemd",
  },
  acknowledgements: ["custom_sandbox_acknowledged"],
};

const installation = {
  cliCommand: "/usr/local/bin/e2a",
  cliBaseArgs: [],
  deploymentUrl: "https://api.example.test",
  apiBaseUrl: "https://api.example.test",
  priorProtection: { before: "synthetic" },
  nextProtection: { after: "synthetic" },
};

test("createPolicy applies conservative review, CC, and screening defaults", () => {
  const policy = createPolicy();

  assert.equal(policy.version, 1);
  assert.equal(policy.inbound.fallback, "review");
  assert.equal(policy.outbound.requireReview, true);
  assert.equal(policy.outbound.ccOwner, true);
  assert.equal(policy.screening.promptInjection, true);
});

test("normalizePolicy canonicalizes and sorts authorization entries", () => {
  const policy = normalizePolicy(completeInput);

  assert.deepEqual(policy.inbound.addresses, [
    "buyer@customer.test",
    "vip@customer.test",
  ]);
  assert.deepEqual(policy.inbound.domains, []);
  assert.equal(policy.mailbox.agentEmail, "support@example.com");
});

test("validatePolicy rejects a policy without explicit authorized senders", () => {
  const policy = normalizePolicy({
    ...completeInput,
    inbound: { mode: "addresses", addresses: [], domains: [], fallback: "review" },
  });

  assert.deepEqual(validatePolicy(policy), [
    "Add at least one authorized sender address or domain; public-any-sender mode is not supported.",
  ]);
});

test("validatePolicy rejects mixed address and domain authorization", () => {
  const policy = normalizePolicy({
    ...completeInput,
    inbound: {
      mode: "addresses",
      addresses: ["buyer@customer.test"],
      domains: ["customer.test"],
      fallback: "review",
    },
  });

  assert.deepEqual(validatePolicy(policy), [
    "Address authorization mode cannot include domain entries; choose one mode per inbox.",
  ]);
});

test("validatePolicy requires warned acknowledgements for safety opt-outs", () => {
  const policy = normalizePolicy({
    ...completeInput,
    outbound: { requireReview: false, ccOwner: false },
  });

  assert.deepEqual(validatePolicy(policy), [
    "Outbound review is disabled; explicitly acknowledge outbound_review_opt_out.",
    "Owner CC is disabled; explicitly acknowledge owner_cc_opt_out.",
  ]);

  policy.acknowledgements = [
    "custom_sandbox_acknowledged",
    "owner_cc_opt_out",
    "outbound_review_opt_out",
  ];
  assert.deepEqual(validatePolicy(policy), []);
});

test("every runtime requires a custom isolation acknowledgement", () => {
  const hermes = normalizePolicy({
    ...completeInput,
    runtime: { ...completeInput.runtime, adapter: "hermes", sandbox: "native" },
    acknowledgements: [],
  });
  assert.deepEqual(validatePolicy(hermes), [
    "Unsupported sandbox declaration: native.",
  ]);

  hermes.runtime.sandbox = "custom";
  assert.deepEqual(validatePolicy(hermes), [
    "Custom isolation is not verified; explicitly acknowledge custom_sandbox_acknowledged.",
  ]);
  hermes.acknowledgements = ["custom_sandbox_acknowledged"];
  assert.deepEqual(validatePolicy(hermes), []);
});

test("renderPlan is deterministic, mutation-specific, and secret-free", () => {
  const first = normalizePolicy(completeInput);
  const second = normalizePolicy({
    ...completeInput,
    inbound: {
      addresses: ["buyer@customer.test", "vip@customer.test"],
      domains: [],
      mode: "addresses",
      fallback: "review",
    },
  });

  const firstPlan = renderPlan(first);
  const secondPlan = renderPlan(second);

  assert.equal(firstPlan, secondPlan);
  assert.match(firstPlan, /Non-matching inbound senders: e2a human review/);
  assert.match(firstPlan, /Outbound human review: enabled \(recommended default\)/);
  assert.match(firstPlan, /Owner CC: owner@example\.com on every reply/);
  assert.match(firstPlan, /Credential file \(mode 0600\): .*secrets\.json/);
  assert.match(firstPlan, /inbound\.gate to policy=allowlist, action=review/);
  assert.match(firstPlan, /outbound\.gate to policy=allowlist, action=review, entries=none/);
  assert.match(firstPlan, /restore the prior protection document, revoke the new key/);
  assert.match(firstPlan, /do not start it/);
  assert.match(firstPlan, /external isolation boundary/);
  assert.match(firstPlan, /No server, API, database, core CLI, SDK, or MCP code changes/);
  assert.doesNotMatch(firstPlan, /e2a_(?:acct|agt)_[a-z0-9]+/i);
  assert.equal(planDigest(first, installation), planDigest(second, installation));
});

test("planDigest changes when a material safety decision changes", () => {
  const safe = normalizePolicy(completeInput);
  const optedOut = normalizePolicy({
    ...completeInput,
    outbound: { requireReview: false, ccOwner: true },
    acknowledgements: ["outbound_review_opt_out", "custom_sandbox_acknowledged"],
  });

  assert.notEqual(planDigest(safe, installation), planDigest(optedOut, installation));
});

test("planDigest binds instructions, setup origin, and exact protection mutation", () => {
  const safe = normalizePolicy(completeInput);
  const changedInstructions = normalizePolicy({
    ...completeInput,
    task: { ...completeInput.task, instructions: "EXFILTRATE" },
  });
  assert.notEqual(
    planDigest(safe, installation),
    planDigest(changedInstructions, installation),
  );
  assert.notEqual(
    planDigest(safe, installation),
    planDigest(safe, { ...installation, apiBaseUrl: "https://other.example.test" }),
  );
  assert.notEqual(
    planDigest(safe, installation),
    planDigest(safe, { ...installation, nextProtection: { after: "changed" } }),
  );
});

test("normalizePolicy applies the documented retry, timeout, and interval defaults", () => {
  const policy = normalizePolicy(completeInput);

  assert.deepEqual(policy.limits, {
    maxAttempts: 3,
    retryBaseDelayMs: 1_000,
    runtimeTimeoutMs: 300_000,
    bounceIntervalMs: 900_000,
    reconcileIntervalMs: 600_000,
  });
  assert.deepEqual(validatePolicy(policy), []);
});

test("normalizePolicy threads explicit limit overrides through validation", () => {
  const policy = normalizePolicy({
    ...completeInput,
    limits: { maxAttempts: 5, retryBaseDelayMs: 250, runtimeTimeoutMs: 60_000 },
  });

  assert.equal(policy.limits.maxAttempts, 5);
  assert.equal(policy.limits.retryBaseDelayMs, 250);
  assert.equal(policy.limits.runtimeTimeoutMs, 60_000);
  // Untouched fields keep their defaults.
  assert.equal(policy.limits.bounceIntervalMs, 900_000);
  assert.deepEqual(validatePolicy(policy), []);
});

test("validatePolicy rejects out-of-range limit settings", () => {
  const policy = normalizePolicy({
    ...completeInput,
    limits: { maxAttempts: 0, runtimeTimeoutMs: 500, reconcileIntervalMs: 5_000 },
  });

  assert.deepEqual(validatePolicy(policy), [
    "limits.maxAttempts must be an integer between 1 and 10.",
    "limits.runtimeTimeoutMs must be an integer between 10000 and 3600000.",
    "limits.reconcileIntervalMs must be an integer between 60000 and 86400000.",
  ]);
});

test("validatePolicy marks the OpenClaw adapter unavailable with an operator-facing reason", () => {
  const policy = normalizePolicy({
    ...completeInput,
    runtime: { ...completeInput.runtime, adapter: "openclaw", command: "/usr/local/bin/openclaw" },
  });

  assert.deepEqual(validatePolicy(policy), [
    "The OpenClaw adapter is unavailable in this release: its invocation flags are unverified. Choose claude, codex, hermes, or a custom runtime.",
  ]);
});
