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
    sandbox: "native",
  },
  service: {
    manager: "systemd",
  },
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
    "owner_cc_opt_out",
    "outbound_review_opt_out",
  ];
  assert.deepEqual(validatePolicy(policy), []);
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
  assert.match(firstPlan, /No server, API, database, core CLI, SDK, or MCP code changes/);
  assert.doesNotMatch(firstPlan, /e2a_(?:acct|agt)_[a-z0-9]+/i);
  assert.equal(planDigest(first), planDigest(second));
});

test("planDigest changes when a material safety decision changes", () => {
  const safe = normalizePolicy(completeInput);
  const optedOut = normalizePolicy({
    ...completeInput,
    outbound: { requireReview: false, ccOwner: true },
    acknowledgements: ["outbound_review_opt_out"],
  });

  assert.notEqual(planDigest(safe), planDigest(optedOut));
});
