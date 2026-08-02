import assert from "node:assert/strict";
import test from "node:test";

import {
  answerQuestion,
  buildPolicyFromInterview,
  createInterview,
  nextQuestion,
} from "../interview.mjs";

function answer(state, expectedId, value) {
  const question = nextQuestion(state);
  assert.equal(question?.id, expectedId);
  return answerQuestion(state, value);
}

function completeSafeSupportInterview() {
  let state = createInterview({ platform: "linux" });
  state = answer(state, "objective", "Resolve routine customer questions.");
  state = answer(state, "profile", "");
  state = answer(state, "support_scope", "Setup, usage, and troubleshooting.");
  state = answer(state, "support_exclusions", "Refunds and legal requests.");
  state = answer(state, "knowledge_sources", "The approved product handbook.");
  state = answer(state, "escalation_rules", "Escalate billing, security, and uncertainty.");
  state = answer(state, "tone", "");
  state = answer(state, "signature", "Token Canopy Support");
  state = answer(state, "agent_email", "support@example.com");
  state = answer(state, "owner_email", "owner@example.com");
  state = answer(
    state,
    "authorized_senders",
    "vip@customer.test, Example.ORG",
  );
  state = answer(state, "outbound_review", "");
  state = answer(state, "owner_cc", "");
  state = answer(state, "screening", "");
  state = answer(state, "runtime", "codex");
  state = answer(state, "runtime_command", "/usr/local/bin/codex");
  state = answer(state, "workdir", "/srv/autopilot/support");
  state = answer(state, "sandbox", "native");
  state = answer(state, "service", "");
  assert.equal(nextQuestion(state), null);
  return state;
}

test("customer-support is the default and asks support-specific questions", () => {
  let state = createInterview({ platform: "darwin" });
  state = answer(state, "objective", "Help customers use the product.");
  const profile = nextQuestion(state);

  assert.equal(profile.default, "customer-support");
  state = answerQuestion(state, "");
  assert.equal(nextQuestion(state).id, "support_scope");
});

test("custom profile skips support-only questions", () => {
  let state = createInterview({ platform: "darwin" });
  state = answer(state, "objective", "Route partnership requests.");
  state = answer(state, "profile", "custom");

  assert.equal(nextQuestion(state).id, "custom_instructions");
  state = answer(state, "custom_instructions", "Escalate requests with no company name.");
  assert.equal(nextQuestion(state).id, "agent_email");
});

test("turning off outbound review inserts a separate warned acknowledgement", () => {
  let state = createInterview({ platform: "linux" });
  const answers = [
    ["objective", "Answer support requests."],
    ["profile", ""],
    ["support_scope", "Product usage."],
    ["support_exclusions", "Billing."],
    ["knowledge_sources", "Approved docs."],
    ["escalation_rules", "Escalate uncertainty."],
    ["tone", ""],
    ["signature", "Support"],
    ["agent_email", "support@example.com"],
    ["owner_email", "owner@example.com"],
    ["authorized_senders", "customer.test"],
  ];
  for (const [id, value] of answers) state = answer(state, id, value);
  state = answer(state, "outbound_review", "no");

  const warning = nextQuestion(state);
  assert.equal(warning.id, "outbound_review_ack");
  assert.match(warning.prompt, /may send email without human approval/i);
  assert.throws(
    () => answerQuestion(state, "yes"),
    /Type I understand to continue/,
  );

  state = answerQuestion(state, "I understand");
  assert.equal(nextQuestion(state).id, "owner_cc");
});

test("authorized senders require at least one valid exact address or domain", () => {
  let state = createInterview({ platform: "linux" });
  state.answers = {
    objective: "Answer support requests.",
    profile: "custom",
    custom_instructions: "Use approved sources.",
    agent_email: "support@example.com",
    owner_email: "owner@example.com",
  };

  assert.equal(nextQuestion(state).id, "authorized_senders");
  assert.throws(() => answerQuestion(state, ""), /at least one/i);
  assert.throws(
    () => answerQuestion(state, "not an address"),
    /valid email address or domain/i,
  );
});

test("a completed support interview produces a valid conservative policy", () => {
  const policy = buildPolicyFromInterview(completeSafeSupportInterview());

  assert.equal(policy.task.profile, "customer-support");
  assert.match(policy.task.instructions, /Setup, usage, and troubleshooting/);
  assert.match(policy.task.instructions, /Never handle: Refunds and legal requests/);
  assert.deepEqual(policy.inbound.addresses, ["vip@customer.test"]);
  assert.deepEqual(policy.inbound.domains, ["example.org"]);
  assert.equal(policy.inbound.fallback, "review");
  assert.equal(policy.outbound.requireReview, true);
  assert.equal(policy.outbound.ccOwner, true);
  assert.equal(policy.screening.promptInjection, true);
  assert.equal(policy.runtime.adapter, "codex");
  assert.equal(policy.service.manager, "systemd");
  assert.deepEqual(policy.acknowledgements, []);
});

test("interview state can be serialized and resumed without changing the next question", () => {
  let state = createInterview({ platform: "darwin" });
  state = answer(state, "objective", "Answer customer questions.");
  state = answer(state, "profile", "customer-support");
  state = answer(state, "support_scope", "Product use.");

  const resumed = JSON.parse(JSON.stringify(state));
  assert.equal(nextQuestion(resumed).id, "support_exclusions");
});
