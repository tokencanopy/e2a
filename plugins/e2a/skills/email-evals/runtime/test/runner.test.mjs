import assert from "node:assert/strict";
import { chmod, cp, mkdir, mkdtemp, readFile, readdir, stat, symlink, unlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { loadSuite } from "../lib/contract.mjs";
import * as errorContract from "../lib/errors.mjs";
import { EvalError } from "../lib/errors.mjs";
import { regradeRun, runSuite } from "../lib/runner.mjs";

const ACTOR = "actor@eval.test";
const TARGET = "target@eval.test";
const CAPABILITIES = [
  "message_action", "visible_recipients", "blind_recipients", "envelope_recipients",
  "thread_headers", "raw_mime", "attachment_hashes", "delivery_lifecycle",
];
const RUN_ID = "run_20260808T120000_0123abcd";

function expectation(kind = "reply") {
  const count = kind === "none" ? 0 : 1;
  return {
    action: { kind, count },
    ...(kind === "none" ? {} : {
      sender: { exactly: TARGET, replyTo: { exactly: [] } },
      recipients: {
        to: { exactly: [ACTOR] }, cc: { exactly: [] }, bcc: { exactly: [] }, envelope: { exactly: [ACTOR] },
      },
      body: { requiredFacts: ["Synthetic answer"] },
    }),
  };
}

function suite(cases = [
  { id: "first", send: { subject: "First", text: "Synthetic question" }, expect: expectation() },
  { id: "second", send: { subject: "Second", text: "Synthetic question" }, expect: expectation() },
  { id: "unsafe-request", send: { subject: "Unsafe", text: "Synthetic request" }, expect: expectation("none") },
]) {
  return {
    version: 1,
    name: "synthetic-suite",
    digest: "a".repeat(64),
    actor: { email: ACTOR },
    target: { email: TARGET },
    transport: {
      apiKey: "synthetic-credential",
      allowedEnvelopeRecipients: [TARGET, ACTOR],
    },
    defaults: { timeoutMs: 50, settleMs: 0, pollIntervalMs: 1 },
    cases,
  };
}

function evidence(testCase, overrides = {}) {
  const none = testCase.expect.action.kind === "none";
  const candidate = {
    ref: `event_${testCase.id}`,
    eventType: "email.sent",
    direction: "outbound",
    provenance: "target_outbound",
    messageType: "reply",
    from: TARGET,
    sentAs: TARGET,
    replyTo: [],
    to: [ACTOR],
    cc: [],
    bcc: [],
    envelopeRecipients: [ACTOR],
    conversationId: "conv_synthetic",
    messageId: `msg_${testCase.id}`,
    observedAt: "2026-08-08T12:00:02.000Z",
    lifecycle: { submission: "sent" },
    mime: { text: "Synthetic answer", subject: `Re: ${testCase.send.subject}`, attachments: [], sizeBytes: 16 },
  };
  return {
    version: 1,
    capabilities: CAPABILITIES,
    target: { email: TARGET },
    stimulus: {
      ref: `stimulus_${testCase.id}`,
      messageId: `stimulus_${testCase.id}`,
      conversationId: "conv_synthetic",
      rfcMessageId: `original-${testCase.id}@agents.localhost`,
      subject: testCase.send.subject,
      receivedAt: "2026-08-08T12:00:01.000Z",
      participants: [ACTOR],
    },
    candidates: none ? [] : [candidate],
    actorReceipt: none ? null : {
      ref: `receipt_${testCase.id}`,
      messageId: candidate.messageId,
      observedAt: "2026-08-08T12:00:03.000Z",
    },
    timings: { completedAt: "2026-08-08T12:00:03.000Z" },
    ...overrides,
  };
}

function adapter(execute, preflight) {
  const calls = { preflight: 0, execute: [] };
  return {
    calls,
    async preflight(resolvedSuite) {
      calls.preflight += 1;
      if (preflight) return preflight(resolvedSuite);
      return { capabilities: CAPABILITIES, plan: { networkSends: false } };
    },
    async executeCase(testCase, context) {
      calls.execute.push({ id: testCase.id, context });
      return execute ? execute(testCase, context) : evidence(testCase);
    },
  };
}

async function root() {
  return mkdtemp(path.join(tmpdir(), "email-evals-runner-"));
}

test("runSuite executes cases with a plain sequential boundary and persists every outcome", async () => {
  let active = 0;
  const observed = [];
  const fake = adapter(async (testCase) => {
    active += 1;
    assert.equal(active, 1);
    observed.push(testCase.id);
    await Promise.resolve();
    active -= 1;
    if (testCase.id === "unsafe-request") throw new EvalError("transport_error", "poll_failed", "Synthetic poll failure");
    return evidence(testCase);
  });
  const summary = await runSuite({ suite: suite(), adapter: fake, outputRoot: await root(), runId: RUN_ID });
  assert.deepEqual(observed, ["first", "second", "unsafe-request"]);
  assert.equal(fake.calls.preflight, 1);
  assert.deepEqual(summary.counts, { total: 3, passed: 2, failed: 0, errors: 1 });
  assert.equal(summary.complete, true);
  assert.equal(JSON.parse(await readFile(summary.files.summary, "utf8")).complete, true);
  const lines = (await readFile(summary.files.cases, "utf8")).trim().split("\n").map(JSON.parse);
  assert.equal(lines.length, 3);
  assert.equal(lines[2].primaryError.class, "transport_error");
  assert.equal((await stat(path.dirname(summary.files.cases))).mode & 0o777, 0o700);
});

test("preflight runs once and configuration or capability failure sends no cases", async () => {
  for (const errorClass of ["configuration_error", "capability_error"]) {
    const fake = adapter(null, async () => { throw new EvalError(errorClass, "preflight_failed", "Synthetic preflight failure"); });
    await assert.rejects(
      runSuite({ suite: suite(), adapter: fake, outputRoot: await root(), runId: RUN_ID }),
      (error) => error.errorClass === errorClass,
    );
    assert.equal(fake.calls.preflight, 1);
    assert.equal(fake.calls.execute.length, 0);
  }
});

test("missing preflight capability fails before every send", async () => {
  const fake = adapter(null, async () => ({ capabilities: ["message_action"] }));
  await assert.rejects(
    runSuite({ suite: suite(), adapter: fake, outputRoot: await root(), runId: RUN_ID }),
    (error) => error.errorClass === "capability_error" && error.code === "missing_capability",
  );
  assert.equal(fake.calls.execute.length, 0);
});

test("a throwing capability iterator is a stable pre-send capability error", async () => {
  const fake = adapter(null, async () => ({ capabilities: { [Symbol.iterator]() { throw new Error("Synthetic iterator failure"); } } }));
  await assert.rejects(
    runSuite({ suite: suite(), adapter: fake, outputRoot: await root(), runId: RUN_ID }),
    (error) => error.errorClass === "capability_error" && error.code === "invalid_capability_set",
  );
  assert.equal(fake.calls.execute.length, 0);
});

test("capability names reject control characters before every send", async () => {
  const fake = adapter(null, async () => ({ capabilities: [...CAPABILITIES, "raw_mime\nforged"] }));
  await assert.rejects(
    runSuite({ suite: suite(), adapter: fake, outputRoot: await root(), runId: RUN_ID }),
    (error) => error.errorClass === "capability_error" && error.code === "invalid_capability_set",
  );
  assert.equal(fake.calls.execute.length, 0);
});

test("a response case with zero candidates is target_timeout", async () => {
  const one = suite([{ id: "timeout", send: { subject: "Timeout", text: "Synthetic" }, expect: expectation() }]);
  const summary = await runSuite({
    suite: one,
    adapter: adapter((testCase) => evidence(testCase, { candidates: [], actorReceipt: null })),
    outputRoot: await root(),
    runId: RUN_ID,
  });
  assert.equal(summary.cases[0].primaryError.class, "target_timeout");
  assert.equal(summary.cases[0].primaryError.code, "no_terminal_response");
});

test("failed assertions are assertion_failure and a thrown grader boundary is grader_error", async () => {
  const mismatch = suite([{ id: "mismatch", send: { subject: "Mismatch", text: "Synthetic" }, expect: expectation() }]);
  const failed = await runSuite({
    suite: mismatch,
    adapter: adapter((testCase) => evidence(testCase, { candidates: [{ ...evidence(testCase).candidates[0], to: [] }] })),
    outputRoot: await root(), runId: RUN_ID,
  });
  assert.equal(failed.cases[0].status, "fail");
  assert.equal(failed.cases[0].primaryError.class, "assertion_failure");

  const hostileExpectation = expectation();
  Object.defineProperty(hostileExpectation.sender, "exactly", { enumerable: true, get() { throw new Error("synthetic grader crash"); } });
  const thrown = await runSuite({
    suite: suite([{ id: "grader", send: { subject: "Grader", text: "Synthetic" }, expect: hostileExpectation }]),
    adapter: adapter((testCase) => evidence({ ...testCase, expect: expectation() })),
    outputRoot: await root(), runId: RUN_ID,
  });
  assert.equal(thrown.cases[0].primaryError.class, "grader_error");
  assert.equal(thrown.cases[0].primaryError.code, "grader_threw");
  assert.equal(thrown.cases[0].primaryError.origin, "grader");
  assert.ok(thrown.cases[0].evidence);
  assert.deepEqual(thrown.cases[0].assertions, []);
  const regraded = await regradeRun({ suite: suite([{ id: "grader", send: { subject: "Grader", text: "Synthetic" }, expect: hostileExpectation }]), runDirectory: path.dirname(thrown.files.cases) });
  assert.equal(regraded.status, thrown.status);
  assert.deepEqual(regraded.cases[0].evidence, thrown.cases[0].evidence);
  assert.deepEqual(regraded.cases[0].assertions, []);
  assert.deepEqual(regraded.cases[0].primaryError, thrown.cases[0].primaryError);
});

test("adapter transport error registry covers every literal adapter code and baseline overflow regrades", async () => {
  const registry = errorContract.EVAL_ERROR_CODE_REGISTRY;
  assert.ok(registry);
  const source = await readFile(new URL("../lib/e2a-adapter.mjs", import.meta.url), "utf8");
  const direct = [
    ...source.matchAll(/transportError\("([a-z][a-z0-9_]+)"/g),
    ...source.matchAll(/new EvalError\("transport_error",\s*"([a-z][a-z0-9_]+)"/g),
  ].map((match) => match[1]);
  for (const code of direct) assert.ok(registry.transport_error.includes(code), `missing adapter transport code ${code}`);
  for (const code of ["baseline_limit_exceeded", "observation_limit_exceeded", "malformed_message"]) {
    assert.ok(source.includes(`"${code}"`));
    assert.ok(registry.transport_error.includes(code), `missing parameterized adapter transport code ${code}`);
  }

  const one = suite([{ id: "baseline-limit", send: { subject: "Baseline", text: "Synthetic" }, expect: expectation() }]);
  const original = await runSuite({
    suite: one,
    adapter: adapter(async () => { throw new EvalError("transport_error", "baseline_limit_exceeded", "Synthetic baseline overflow"); }),
    outputRoot: await root(), runId: RUN_ID,
  });
  assert.equal(original.cases[0].primaryError.code, "baseline_limit_exceeded");
  assert.equal(original.cases[0].evidence, null);
  assert.deepEqual(original.cases[0].assertions, []);
  const regraded = await regradeRun({ suite: one, runDirectory: path.dirname(original.files.cases) });
  assert.equal(regraded.cases[0].primaryError.code, "baseline_limit_exceeded");
  assert.equal(regraded.status, original.status);

  const boundary = await runSuite({
    suite: one,
    adapter: adapter(async () => { throw new EvalError("transport_error", "invalid_evidence", "Forged runner code"); }),
    outputRoot: await root(), runId: RUN_ID,
  });
  assert.equal(boundary.cases[0].primaryError.code, "adapter_failed");
  assert.equal(boundary.cases[0].primaryError.origin, "adapter_boundary");
  assert.equal(boundary.cases[0].evidence, null);
  assert.deepEqual(boundary.cases[0].assertions, []);
  const boundaryRegrade = await regradeRun({ suite: one, runDirectory: path.dirname(boundary.files.cases) });
  assert.equal(boundaryRegrade.cases[0].primaryError.code, "adapter_failed");
});

test("missing required evidence is capability_error in both run and regrade", async () => {
  const one = suite([{ id: "missing-bcc", send: { subject: "Missing", text: "Synthetic" }, expect: expectation() }]);
  const outputRoot = await root();
  const summary = await runSuite({
    suite: one,
    adapter: adapter((testCase) => {
      const captured = evidence(testCase);
      delete captured.candidates[0].bcc;
      return captured;
    }),
    outputRoot, runId: RUN_ID,
  });
  assert.equal(summary.cases[0].status, "error");
  assert.equal(summary.cases[0].primaryError.class, "capability_error");
  assert.ok(summary.cases[0].assertions.some((assertion) => assertion.status === "error"));
  const regraded = await regradeRun({ suite: one, runDirectory: path.dirname(summary.files.cases) });
  assert.equal(regraded.cases[0].primaryError.class, "capability_error");

  const forged = (await readFile(summary.files.cases, "utf8")).trimEnd().split("\n").map(JSON.parse);
  forged[0].primaryError = {
    class: "transport_error", code: "invalid_evidence", origin: "grader", message: "Synthetic",
  };
  await writeFile(summary.files.cases, `${forged.map(JSON.stringify).join("\n")}\n`);
  await assert.rejects(
    regradeRun({ suite: one, runDirectory: path.dirname(summary.files.cases) }),
    (error) => error.errorClass === "configuration_error" && error.code === "invalid_case_artifact",
  );
});

test("a no-action case still requires a versioned evidence envelope", async () => {
  const one = suite([{ id: "none", send: { subject: "None", text: "Synthetic" }, expect: expectation("none") }]);
  const summary = await runSuite({ suite: one, adapter: adapter(async () => null), outputRoot: await root(), runId: RUN_ID });
  assert.equal(summary.cases[0].primaryError.class, "transport_error");
  assert.equal(summary.cases[0].primaryError.code, "invalid_evidence");
});

test("getter-backed adapter evidence becomes a durable invalid-evidence result without invoking the getter", async () => {
  const one = suite([{ id: "getter-evidence", send: { subject: "Getter", text: "Synthetic" }, expect: expectation("none") }]);
  let invoked = false;
  const hostile = {};
  Object.defineProperty(hostile, "version", { enumerable: true, get() { invoked = true; throw new Error("must not run"); } });
  const summary = await runSuite({ suite: one, adapter: adapter(async () => hostile), outputRoot: await root(), runId: RUN_ID });
  assert.equal(invoked, false);
  assert.equal(summary.status, "fail");
  assert.equal(summary.cases[0].primaryError.code, "invalid_evidence");
  assert.equal((await readFile(summary.files.cases, "utf8")).trim().split("\n").length, 1);
});

test("the first primary failure wins and onCase failures are redacted secondary errors", async () => {
  const fake = adapter(async () => {
    throw new EvalError("transport_error", "poll_failed", `failed for ${ACTOR} using synthetic-credential`);
  });
  const summary = await runSuite({
    suite: suite().constructor === Object ? suite() : suite(),
    adapter: fake,
    outputRoot: await root(),
    runId: RUN_ID,
    onCase: async () => { throw new Error(`callback leaked ${TARGET} synthetic-credential`); },
  });
  const first = summary.cases[0];
  assert.equal(first.primaryError.class, "transport_error");
  assert.equal(summary.secondaryErrors[0].stage, "on_case");
  assert.doesNotMatch(JSON.stringify(summary), /@eval\.test|synthetic-credential/);
  assert.equal(summary.counts.errors, 3);
});

test("onCase receives frozen aliased data only after its complete JSONL line is durable", async () => {
  const outputRoot = await root();
  let callbacks = 0;
  const summary = await runSuite({
    suite: suite(), adapter: adapter(), outputRoot, runId: RUN_ID,
    onCase: async (record) => {
      callbacks += 1;
      assert.ok(Object.isFrozen(record));
      assert.throws(() => { record.status = "error"; }, TypeError);
      assert.doesNotMatch(JSON.stringify(record), /@eval\.test|synthetic-credential/);
      const lines = (await readFile(path.join(outputRoot, RUN_ID, "cases.jsonl"), "utf8")).trim().split("\n");
      assert.equal(lines.length, callbacks);
      assert.equal(JSON.parse(lines.at(-1)).id, record.id);
    },
  });
  assert.equal(callbacks, 3);
  assert.equal(summary.status, "pass");
});

test("run metadata has exact versions, sorted capabilities, safe timestamps, and finite durations", async () => {
  const instants = [
    "2026-08-08T12:00:00.000Z", "2026-08-08T12:00:00.010Z", "2026-08-08T12:00:00.020Z",
    "2026-08-08T12:00:00.030Z", "2026-08-08T12:00:00.040Z", "2026-08-08T12:00:00.050Z",
    "2026-08-08T12:00:00.060Z", "2026-08-08T12:00:00.070Z", "2026-08-08T12:00:00.080Z",
    "2026-08-08T12:00:00.090Z", "2026-08-08T12:00:00.100Z", "2026-08-08T12:00:00.110Z",
  ].map((value) => new Date(value));
  let index = 0;
  const now = () => instants[Math.min(index++, instants.length - 1)];
  const summary = await runSuite({ suite: suite(), adapter: adapter(), outputRoot: await root(), runId: RUN_ID, now });
  assert.deepEqual(summary.versions, { runner: "0.1.0", sdk: "5.6.0", suite: 1, evidence: 1 });
  assert.deepEqual(summary.capabilities, [...CAPABILITIES].sort());
  assert.equal(summary.suite.digest, "a".repeat(64));
  assert.ok(Object.values(summary.durations).every((value) => Number.isFinite(value) && value >= 0));
  assert.doesNotMatch(JSON.stringify(summary), /NaN|Infinity/);
  const stored = JSON.parse(await readFile(summary.files.summary, "utf8"));
  assert.deepEqual(stored.durations, summary.durations);
  assert.deepEqual(stored.counts, summary.counts);
  const artifacts = await Promise.all(Object.values(summary.files).map((file) => readFile(file, "utf8")));
  assert.match(artifacts.join("\n"), /actor|target/);
  assert.doesNotMatch(artifacts.join("\n"), /actor@eval\.test|target@eval\.test|synthetic-credential/);
});

test("generated run IDs use the exact UTC timestamp and lowercase random suffix", async () => {
  const outputRoot = await root();
  const now = () => new Date("2026-08-08T12:34:56.000Z");
  const summary = await runSuite({ suite: suite(), adapter: adapter(), outputRoot, now });
  assert.match(summary.runId, /^run_20260808T123456_[a-f0-9]{8}$/);
  assert.deepEqual(await readdir(outputRoot), [summary.runId]);
});

test("invalid clocks fail before preflight or sends", async () => {
  const fake = adapter();
  await assert.rejects(
    runSuite({ suite: suite(), adapter: fake, outputRoot: await root(), runId: RUN_ID, now: () => new Date(Number.NaN) }),
    (error) => error.errorClass === "configuration_error" && error.code === "invalid_clock",
  );
  assert.equal(fake.calls.preflight, 0);
  assert.equal(fake.calls.execute.length, 0);
  await assert.rejects(
    runSuite({ suite: suite(), adapter: fake, outputRoot: await root(), runId: RUN_ID, now: () => 1e20 }),
    (error) => error.errorClass === "configuration_error" && error.code === "invalid_clock",
  );
});

test("a clock that becomes invalid after execution still yields a durable transport error", async () => {
  let reads = 0;
  const now = () => {
    reads += 1;
    return reads <= 3 ? new Date("2026-08-08T12:00:00.000Z") : new Date(Number.NaN);
  };
  const one = suite([{ id: "clock", send: { subject: "Clock", text: "Synthetic" }, expect: expectation() }]);
  const summary = await runSuite({ suite: one, adapter: adapter(), outputRoot: await root(), runId: RUN_ID, now });
  assert.equal(summary.cases[0].primaryError.class, "transport_error");
  assert.equal(summary.cases[0].primaryError.code, "invalid_clock_after_send");
  assert.equal((await readFile(summary.files.cases, "utf8")).trim().split("\n").length, 1);
});

test("concurrent run ID collision executes only one suite and never overwrites it", async () => {
  const outputRoot = await root();
  const first = adapter();
  const second = adapter();
  const settled = await Promise.allSettled([
    runSuite({ suite: suite(), adapter: first, outputRoot, runId: RUN_ID }),
    runSuite({ suite: suite(), adapter: second, outputRoot, runId: RUN_ID }),
  ]);
  assert.deepEqual(settled.map((entry) => entry.status).sort(), ["fulfilled", "rejected"]);
  assert.equal(first.calls.execute.length + second.calls.execute.length, 3);
  assert.equal((await readFile(path.join(outputRoot, RUN_ID, "cases.jsonl"), "utf8")).trim().split("\n").length, 3);
});

test("a case primary error survives later incremental-summary and final-report failures", async () => {
  const outputRoot = await root();
  const outside = await root();
  const sentinel = path.join(outside, "sentinel.json");
  await writeFile(sentinel, "sentinel\n");
  const fake = adapter(async () => { throw new EvalError("transport_error", "poll_failed", "Synthetic poll failure"); });
  let linked = false;
  const summary = await runSuite({
    suite: suite(), adapter: fake, outputRoot, runId: RUN_ID,
    onCase: async () => {
      if (!linked) {
        linked = true;
        await unlink(path.join(outputRoot, RUN_ID, "summary.json"));
        await symlink(sentinel, path.join(outputRoot, RUN_ID, "summary.json"));
      }
      throw new Error("Synthetic reporting hook failure");
    },
  });
  assert.equal(summary.cases[0].primaryError.class, "transport_error");
  assert.ok(summary.secondaryErrors.some((error) => error.stage === "on_case"));
  assert.ok(summary.secondaryErrors.some((error) => error.stage === "reporting"));
  assert.equal(fake.calls.execute.length, 2);
  assert.equal((await readFile(summary.files.cases, "utf8")).trim().split("\n").length, 2);
  assert.equal(await readFile(sentinel, "utf8"), "sentinel\n");
});

test("a durable reporting failure makes an otherwise passing incomplete run fail", async () => {
  const outputRoot = await root();
  const outside = await root();
  const sentinel = path.join(outside, "sentinel.json");
  await writeFile(sentinel, "sentinel\n");
  let linked = false;
  const fake = adapter();
  const summary = await runSuite({
    suite: suite(), adapter: fake, outputRoot, runId: RUN_ID,
    onCase: async () => {
      if (!linked) {
        linked = true;
        await unlink(path.join(outputRoot, RUN_ID, "summary.json"));
        await symlink(sentinel, path.join(outputRoot, RUN_ID, "summary.json"));
      }
    },
  });
  assert.equal(summary.status, "fail");
  assert.equal(summary.counts.total, 2);
  assert.equal(fake.calls.execute.length, 2);
  assert.ok(summary.secondaryErrors.some((error) => error.stage === "reporting"));
  assert.equal(await readFile(sentinel, "utf8"), "sentinel\n");
});

test("environment-backed suite names, case IDs, and action enums redact without changing regrade semantics", async () => {
  const fixtureRoot = await root();
  await mkdir(path.join(fixtureRoot, "cases"));
  await writeFile(path.join(fixtureRoot, "suite.yaml"), `
version: 1
name: "\${E2A_SUITE_NAME}"
target: { email: "\${E2A_EVAL_TARGET}" }
actor: { email: "\${E2A_EVAL_ACTOR}" }
transport:
  adapter: e2a
  api_key: "\${E2A_EVAL_API_KEY}"
  base_url: https://api.example.test
  allowed_envelope_recipients: ["\${E2A_EVAL_ACTOR}", "\${E2A_EVAL_TARGET}"]
cases: [cases/environment.yaml]
`);
  await writeFile(path.join(fixtureRoot, "cases/environment.yaml"), `
id: "\${E2A_CASE_ID}"
send: { subject: Synthetic, text: Synthetic }
expect: { action: { kind: "\${E2A_ACTION}", count: 0 } }
`);
  const loaded = await loadSuite(path.join(fixtureRoot, "suite.yaml"), { environment: {
    E2A_SUITE_NAME: "private-suite-name",
    E2A_CASE_ID: "private-case-id",
    E2A_ACTION: "none",
    E2A_EVAL_TARGET: TARGET,
    E2A_EVAL_ACTOR: ACTOR,
    E2A_EVAL_API_KEY: "synthetic-credential",
  } });
  const summary = await runSuite({ suite: loaded, adapter: adapter(), outputRoot: await root(), runId: RUN_ID });
  const artifacts = await Promise.all(Object.values(summary.files).map((file) => readFile(file, "utf8")));
  assert.equal(summary.status, "pass");
  assert.equal(summary.suite.name, "[ENV:E2A_SUITE_NAME]");
  assert.equal(summary.cases[0].id, "[ENV:E2A_CASE_ID]");
  assert.equal(summary.cases[0].expectation.action.kind, "[ENV:E2A_ACTION:semantic:0]");
  assert.doesNotMatch(artifacts.join("\n"), /private-suite-name|private-case-id|synthetic-credential/);
  const regraded = await regradeRun({ suite: loaded, runDirectory: path.dirname(summary.files.cases) });
  assert.equal(regraded.status, "pass");
  assert.equal(regraded.cases[0].status, "pass");
});

test("environment-backed mailbox fields retain typed aliases through run and regrade", async () => {
  const fixtureRoot = await root();
  await mkdir(path.join(fixtureRoot, "cases"));
  await writeFile(path.join(fixtureRoot, "suite.yaml"), `
version: 1
name: mailbox-parity
target: { email: "\${E2A_EVAL_TARGET}" }
actor: { email: "\${E2A_EVAL_ACTOR}" }
transport:
  adapter: e2a
  api_key: "\${E2A_EVAL_API_KEY}"
  base_url: https://api.example.test
  allowed_envelope_recipients: ["\${E2A_EVAL_TARGET}", "\${E2A_EVAL_ACTOR}", "\${E2A_PROBE_CC}", "\${E2A_PROBE_BCC}"]
cases: [cases/mailboxes.yaml]
`);
  await writeFile(path.join(fixtureRoot, "cases/mailboxes.yaml"), `
id: mailbox-parity
send: { subject: Synthetic, text: Synthetic }
expect:
  action: { kind: reply_all, count: 1 }
  sender:
    exactly: "\${E2A_EVAL_TARGET}"
    sent_as: "\${E2A_EVAL_TARGET}"
    display_name: Synthetic Target
    reply_to: { exactly: ["\${E2A_PROBE_CC}"] }
  recipients:
    to: { exactly: ["\${E2A_EVAL_ACTOR}"] }
    cc: { exactly: ["\${E2A_PROBE_CC}"] }
    bcc: { exactly: ["\${E2A_PROBE_BCC}"] }
    envelope: { exactly: ["\${E2A_EVAL_ACTOR}", "\${E2A_PROBE_CC}", "\${E2A_PROBE_BCC}"] }
`);
  const probeCc = "cc-probe@eval.test";
  const probeBcc = "bcc-probe@eval.test";
  const loaded = await loadSuite(path.join(fixtureRoot, "suite.yaml"), { environment: {
    E2A_EVAL_TARGET: TARGET, E2A_EVAL_ACTOR: ACTOR, E2A_EVAL_API_KEY: "synthetic-credential",
    E2A_PROBE_CC: probeCc, E2A_PROBE_BCC: probeBcc,
  } });
  const summary = await runSuite({
    suite: loaded,
    adapter: adapter((testCase) => {
      const captured = evidence(testCase);
      captured.stimulus.participants = [`Synthetic Actor <${ACTOR}>`, `CC Probe <${probeCc}>`];
      Object.assign(captured.candidates[0], {
        from: `Synthetic Target <${TARGET}>`, sentAs: `Synthetic Target <${TARGET}>`, replyTo: [`CC Probe <${probeCc}>`],
        to: [`Synthetic Actor <${ACTOR}>`], cc: [`CC Probe <${probeCc}>`], bcc: [`BCC Probe <${probeBcc}>`],
        envelopeRecipients: [`Synthetic Actor <${ACTOR}>`, `CC Probe <${probeCc}>`, `BCC Probe <${probeBcc}>`],
      });
      return captured;
    }),
    outputRoot: await root(), runId: RUN_ID,
  });
  assert.equal(summary.status, "pass");
  assert.deepEqual(summary.cases[0].expectation.sender, {
    exactly: "target", sentAs: "target", displayName: "Synthetic Target", replyTo: { exactly: ["probe:2"] },
  });
  assert.deepEqual(summary.cases[0].expectation.recipients, {
    to: { exactly: ["actor"] }, cc: { exactly: ["probe:2"] }, bcc: { exactly: ["probe:1"] },
    envelope: { exactly: ["actor", "probe:1", "probe:2"] },
  });
  assert.deepEqual(summary.cases[0].evidence.candidates[0].from, { address: "target", displayName: "Synthetic Target" });
  assert.deepEqual(summary.cases[0].evidence.candidates[0].sentAs, { address: "target", displayName: "Synthetic Target" });
  assert.deepEqual(summary.cases[0].evidence.candidates[0].replyTo, [{ address: "probe:2", displayName: "CC Probe" }]);
  assert.deepEqual(summary.cases[0].evidence.candidates[0].to, [{ address: "actor", displayName: "Synthetic Actor" }]);
  const regraded = await regradeRun({ suite: loaded, runDirectory: path.dirname(summary.files.cases) });
  assert.equal(regraded.status, "pass");
  assert.deepEqual(regraded.cases[0].expectation, summary.cases[0].expectation);
});

test("adapter evidence is a strict projection and unknown nested data never reaches artifacts", async () => {
  const mutations = [
    (captured) => { captured.rawMimeBase64 = "secret-root outside@example.com"; },
    (captured) => { captured.candidates[0].mailboxMap = { "outside@example.com": "secret-map" }; },
    (captured) => { captured.candidates[0].mime.attachments.push({ filename: "x", contentType: "text/plain", disposition: "attachment", sizeBytes: 1, sha256: "a".repeat(64), content: "secret-bytes" }); },
    (captured) => { captured.candidates[0].diagnostic = "secret-diagnostic outside@example.com"; },
  ];
  for (const [index, mutate] of mutations.entries()) {
    const one = suite([{ id: `strict-${index}`, send: { subject: "Strict", text: "Synthetic" }, expect: expectation() }]);
    const summary = await runSuite({
      suite: one,
      adapter: adapter((testCase) => { const captured = evidence(testCase); mutate(captured); return captured; }),
      outputRoot: await root(), runId: RUN_ID,
    });
    assert.equal(summary.cases[0].status, "error");
    assert.equal(summary.cases[0].primaryError.class, "transport_error");
    assert.equal(summary.cases[0].primaryError.code, "invalid_evidence");
    const artifacts = (await Promise.all(Object.values(summary.files).map((file) => readFile(file, "utf8")))).join("\n");
    assert.doesNotMatch(artifacts, /secret-root|secret-map|secret-bytes|secret-diagnostic|outside@example\.com/);
  }
});

test("evidence token fields and aggregate CaseRecord size fail closed before append", async () => {
  const scenarios = [
    (captured) => { captured.candidates[0].ref = "outside@example.com"; },
    (captured) => { captured.candidates[0].ref = "event\nforged"; },
    (captured) => {
      captured.candidates[0].mime.subject = "S".repeat(1_048_000);
      captured.candidates[0].mime.text = "T".repeat(1_048_000);
    },
  ];
  for (const [index, mutate] of scenarios.entries()) {
    const one = suite([{ id: `bounded-${index}`, send: { subject: "Bounded", text: "Synthetic" }, expect: expectation() }]);
    const summary = await runSuite({
      suite: one,
      adapter: adapter((testCase) => { const captured = evidence(testCase); mutate(captured); return captured; }),
      outputRoot: await root(), runId: RUN_ID,
    });
    assert.equal(summary.cases[0].primaryError.class, "transport_error");
    assert.equal(summary.cases[0].primaryError.code, "invalid_evidence");
    assert.ok((await stat(summary.files.cases)).size < 2 * 1024 * 1024);
    assert.doesNotMatch(await readFile(summary.files.cases, "utf8"), /outside@example\.com/);
  }
});

test("body mailbox text is artifact-safe and replay-equivalent", async () => {
  const addressFact = "Contact outside@example.com for the synthetic answer";
  const expected = expectation();
  expected.body.requiredFacts = [addressFact];
  const one = suite([{ id: "body-address", send: { subject: "Body", text: "Synthetic" }, expect: expected }]);
  const summary = await runSuite({
    suite: one,
    adapter: adapter((testCase) => evidence(testCase, {
      candidates: [{ ...evidence(testCase).candidates[0], mime: { ...evidence(testCase).candidates[0].mime, text: addressFact } }],
    })),
    outputRoot: await root(), runId: RUN_ID,
  });
  assert.equal(summary.status, "pass");
  assert.doesNotMatch(await readFile(summary.files.cases, "utf8"), /outside@example\.com/);
  const regraded = await regradeRun({ suite: one, runDirectory: path.dirname(summary.files.cases) });
  assert.equal(regraded.status, "pass");
});

test("nonthrowing malformed observations are transport errors while thrown graders remain grader errors", async () => {
  const scenarios = [
    (captured) => { captured.actorReceipt = {}; },
    (captured) => { captured.candidates[0].observedAt = "not-a-timestamp"; },
  ];
  for (const [index, mutate] of scenarios.entries()) {
    const expected = { ...expectation(), timing: { replyWithinMs: 5_000 }, lifecycle: { actorReceived: true } };
    const one = suite([{ id: `malformed-${index}`, send: { subject: "Malformed", text: "Synthetic" }, expect: expected }]);
    const summary = await runSuite({
      suite: one,
      adapter: adapter((testCase) => { const captured = evidence(testCase); mutate(captured); return captured; }),
      outputRoot: await root(), runId: RUN_ID,
    });
    assert.equal(summary.cases[0].status, "error");
    assert.equal(summary.cases[0].primaryError.class, "transport_error");
    assert.equal(summary.cases[0].primaryError.code, "invalid_evidence");
    const forged = (await readFile(summary.files.cases, "utf8")).trimEnd().split("\n").map(JSON.parse);
    const assertionError = forged[0].assertions.find((assertion) => assertion.status === "error");
    assertionError.code = "missing_synthetic_evidence";
    await writeFile(summary.files.cases, `${forged.map(JSON.stringify).join("\n")}\n`);
    await assert.rejects(
      regradeRun({ suite: one, runDirectory: path.dirname(summary.files.cases) }),
      (error) => error.errorClass === "configuration_error" && error.code === "invalid_case_artifact",
    );
  }
});

test("missing original subject is capability_error and adapter-thrown grader_error is remapped", async () => {
  const expected = { ...expectation(), subject: { policy: "preserve" } };
  const one = suite([{ id: "missing-subject", send: { subject: "Subject", text: "Synthetic" }, expect: expected }]);
  const missing = await runSuite({
    suite: one,
    adapter: adapter((testCase) => { const captured = evidence(testCase); captured.stimulus.subject = null; return captured; }),
    outputRoot: await root(), runId: RUN_ID,
  });
  assert.equal(missing.cases[0].primaryError.class, "capability_error");
  assert.equal(missing.cases[0].primaryError.code, "required_evidence_unavailable");

  const external = await runSuite({
    suite: one,
    adapter: adapter(async () => { throw new EvalError("grader_error", "grader_threw", "External grader impersonation"); }),
    outputRoot: await root(), runId: RUN_ID,
  });
  assert.equal(external.cases[0].primaryError.class, "transport_error");
  assert.equal(external.cases[0].primaryError.code, "adapter_failed");
});

test("regrade rejects untrusted CaseRecords before copying any stored fields", async () => {
  const mutations = [
    (record) => { record.status = "pass"; record.evidence = null; record.primaryError = { class: "transport_error", code: "adapter_threw", message: "Synthetic" }; },
    (record) => { record.rawMimeBase64 = "secret-root"; },
    (record) => { record.evidence.candidates[0].from = "outside@example.com"; },
    (record) => { record.assertions[0].status = "unknown"; },
    (record) => { record.secondaryErrors.push({ stage: "reporting", code: "synthetic", message: "e2a_secret_must_not_escape" }); },
    (record) => {
      record.status = "error";
      record.evidence.candidates = [];
      record.assertions = [];
      record.primaryError = { class: "target_timeout", code: "adapter_threw", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.evidence = null;
      record.assertions = [];
      record.primaryError = { class: "transport_error", code: "poll_failed", message: "synthetic-credential" };
    },
    (record) => { record.secondaryErrors.push({ stage: "arbitrary", code: "synthetic", message: "Synthetic" }); },
    (record) => {
      record.status = "error";
      record.evidence = null;
      record.assertions = [];
      record.primaryError = { class: "grader_error", code: "grader_threw", origin: "grader", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.evidence.candidates = [];
      record.assertions = [];
      record.primaryError = { class: "target_timeout", code: "no_terminal_response", origin: "adapter", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.evidence = null;
      record.assertions = [];
      record.primaryError = { class: "capability_error", code: "required_evidence_unavailable", origin: "grader", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.primaryError = { class: "capability_error", code: "invalid_capability_set", origin: "grader", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.evidence = null;
      record.assertions = [];
      record.primaryError = { class: "configuration_error", code: "invalid_suite", origin: "runner", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.evidence = null;
      record.assertions = [];
      record.primaryError = { class: "transport_error", code: "poll_failed", origin: "grader", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.evidence = null;
      record.assertions = [];
      record.primaryError = { class: "transport_error", code: "poll_failed", origin: "adapter", message: "Synthetic\nforged" };
    },
    (record) => {
      record.status = "error";
      record.primaryError = { class: "transport_error", code: "poll_failed", origin: "adapter", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.primaryError = { class: "transport_error", code: "adapter_failed", origin: "adapter_boundary", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.primaryError = { class: "transport_error", code: "invalid_evidence", origin: "runner", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.primaryError = { class: "transport_error", code: "invalid_clock_after_send", origin: "runner", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.primaryError = { class: "target_timeout", code: "no_terminal_response", origin: "runner", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.primaryError = { class: "grader_error", code: "grader_threw", origin: "grader", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.primaryError = { class: "capability_error", code: "required_evidence_unavailable", origin: "grader", message: "Synthetic" };
    },
    (record) => {
      record.status = "error";
      record.primaryError = { class: "transport_error", code: "invalid_evidence", origin: "grader", message: "Synthetic" };
    },
    (record) => {
      record.status = "fail";
      record.primaryError = { class: "assertion_failure", code: "assertions_failed", origin: "grader", message: "Synthetic" };
    },
  ];
  for (const mutate of mutations) {
    const original = await runSuite({ suite: suite(), adapter: adapter(), outputRoot: await root(), runId: RUN_ID });
    const records = (await readFile(original.files.cases, "utf8")).trim().split("\n").map(JSON.parse);
    mutate(records[0]);
    await writeFile(original.files.cases, `${records.map(JSON.stringify).join("\n")}\n`);
    await assert.rejects(
      regradeRun({ suite: suite(), runDirectory: path.dirname(original.files.cases) }),
      (error) => error.errorClass === "configuration_error" && error.code === "invalid_case_artifact",
    );
  }

  const duplicateKey = await runSuite({ suite: suite(), adapter: adapter(), outputRoot: await root(), runId: RUN_ID });
  const source = await readFile(duplicateKey.files.cases, "utf8");
  await writeFile(duplicateKey.files.cases, source.replace('{"id":"first"', '{"id":"first","status":"pass"'));
  await assert.rejects(
    regradeRun({ suite: suite(), runDirectory: path.dirname(duplicateKey.files.cases) }),
    (error) => error.errorClass === "configuration_error" && error.code === "invalid_cases_artifact",
  );

  for (const key of ["__proto__", "constructor", "prototype"]) {
    const poisoned = await runSuite({ suite: suite(), adapter: adapter(), outputRoot: await root(), runId: RUN_ID });
    const beforeSummary = await readFile(poisoned.files.summary, "utf8");
    const beforeReport = await readFile(poisoned.files.report, "utf8");
    const lines = (await readFile(poisoned.files.cases, "utf8")).trimEnd().split("\n");
    lines[0] = lines[0].replace("{", `{${JSON.stringify(key)}:{"polluted":true},`);
    await writeFile(poisoned.files.cases, `${lines.join("\n")}\n`);
    await assert.rejects(
      regradeRun({ suite: suite(), runDirectory: path.dirname(poisoned.files.cases) }),
      (error) => error.errorClass === "configuration_error" && error.code === "invalid_case_artifact",
    );
    assert.equal({}.polluted, undefined);
    assert.equal(await readFile(poisoned.files.summary, "utf8"), beforeSummary);
    assert.equal(await readFile(poisoned.files.report, "utf8"), beforeReport);
  }
});

test("regrade rebuilds summary status and counts and projects prior metadata", async () => {
  const original = await runSuite({ suite: suite(), adapter: adapter(), outputRoot: await root(), runId: RUN_ID });
  const stored = JSON.parse(await readFile(original.files.summary, "utf8"));
  Object.assign(stored, {
    status: "fail",
    counts: { total: 999, passed: 0, failed: 999, errors: 0 },
    primaryError: { message: "secret-summary outside@example.com" },
    secondaryErrors: [{ message: "e2a_secret_summary" }],
    capabilities: ["message_action", "outside@example.com"],
    durations: { arbitrary: "secret-duration" },
  });
  await writeFile(original.files.summary, `${JSON.stringify(stored)}\n`);
  const regraded = await regradeRun({ suite: suite(), runDirectory: path.dirname(original.files.cases) });
  assert.equal(regraded.status, "pass");
  assert.deepEqual(regraded.counts, { total: 3, passed: 3, failed: 0, errors: 0 });
  assert.deepEqual(regraded.capabilities, []);
  assert.deepEqual(regraded.durations, { preflightMs: 0, executionMs: 0, gradingMs: 0, reportingMs: 0, totalMs: 0 });
  assert.doesNotMatch(await readFile(original.files.summary, "utf8"), /secret-summary|outside@example\.com|e2a_secret_summary|secret-duration/);
});

test("regrade publishes report before the complete summary commit marker", async () => {
  const original = await runSuite({ suite: suite(), adapter: adapter(), outputRoot: await root(), runId: RUN_ID });
  const prior = JSON.parse(await readFile(original.files.summary, "utf8"));
  await writeFile(original.files.summary, `${JSON.stringify({ ...prior, status: "incomplete", complete: false })}\n`);
  await unlink(original.files.report);
  await mkdir(original.files.report);
  const regraded = await regradeRun({ suite: suite(), runDirectory: path.dirname(original.files.cases) });
  const stored = JSON.parse(await readFile(original.files.summary, "utf8"));
  assert.equal(regraded.status, "incomplete");
  assert.equal(regraded.complete, false);
  assert.equal(stored.status, "incomplete");
  assert.equal(stored.complete, false);
});

test("cumulative cases artifact bounds stop later sends and remain regradable", async () => {
  const cases = Array.from({ length: 18 }, (_, index) => ({
    id: `large-${index + 1}`,
    send: { subject: `Large ${index + 1}`, text: "Synthetic" },
    expect: expectation(),
  }));
  const largeSuite = suite(cases);
  const fake = adapter((testCase) => {
    const captured = evidence(testCase);
    captured.candidates[0].mime.text = `Synthetic answer ${"X".repeat(990_000)}`;
    return captured;
  });
  const original = await runSuite({ suite: largeSuite, adapter: fake, outputRoot: await root(), runId: RUN_ID });
  assert.ok((await stat(original.files.cases)).size <= 16_777_216);
  assert.equal(fake.calls.execute.some((call) => call.id === "large-17"), false);
  assert.equal(fake.calls.execute.some((call) => call.id === "large-18"), false);
  assert.notEqual(original.status, "pass");
  assert.equal(original.complete, true);
  assert.deepEqual(original.cases.map((record) => record.id), cases.map((entry) => entry.id));
  const bounded = original.cases.filter((record) => record.primaryError?.code === "cases_artifact_limit");
  assert.ok(bounded.length >= 2);
  for (const record of bounded) {
    assert.deepEqual(Object.keys(record).sort(), [
      "assertions", "evidence", "id", "primaryError", "secondaryErrors", "status", "suite", "versions",
    ]);
    assert.equal(record.status, "error");
    assert.equal(record.evidence, null);
    assert.deepEqual(record.assertions, []);
    assert.deepEqual(record.suite, { version: largeSuite.version, digest: largeSuite.digest });
    assert.deepEqual(record.versions, { evidence: 1 });
    assert.equal(record.primaryError.origin, "runner");
  }
  const regraded = await regradeRun({ suite: largeSuite, runDirectory: path.dirname(original.files.cases) });
  assert.equal(regraded.status, original.status);
  assert.equal(regraded.counts.total, cases.length);
  assert.equal(regraded.complete, true);
  assert.deepEqual(regraded.cases, original.cases);

  const forged = (await readFile(original.files.cases, "utf8")).trimEnd().split("\n").map(JSON.parse);
  const firstCompact = forged.findIndex((record) => record.primaryError?.code === "cases_artifact_limit");
  assert.ok(firstCompact > 0);
  const priorId = forged[firstCompact - 1].id;
  const compactId = forged[firstCompact].id;
  const prior = forged[firstCompact - 1];
  forged[firstCompact - 1] = { ...forged[firstCompact], id: priorId };
  forged[firstCompact] = { ...prior, id: compactId };
  await writeFile(original.files.cases, `${forged.map(JSON.stringify).join("\n")}\n`);
  await assert.rejects(
    regradeRun({ suite: largeSuite, runDirectory: path.dirname(original.files.cases) }),
    (error) => error.errorClass === "configuration_error" && error.code === "invalid_case_artifact",
  );
});

test("a report-only finalize failure durably marks both returned and on-disk summary failed", async () => {
  const outputRoot = await root();
  const outside = await root();
  const sentinel = path.join(outside, "report.md");
  await writeFile(sentinel, "sentinel\n");
  let callbacks = 0;
  const summary = await runSuite({
    suite: suite(), adapter: adapter(), outputRoot, runId: RUN_ID,
    onCase: async () => {
      callbacks += 1;
      if (callbacks === 3) await symlink(sentinel, path.join(outputRoot, RUN_ID, "report.md"));
    },
  });
  const stored = JSON.parse(await readFile(summary.files.summary, "utf8"));
  assert.equal(summary.status, "fail");
  assert.equal(summary.complete, false);
  assert.equal(stored.status, "fail");
  assert.equal(stored.complete, false);
  assert.deepEqual(stored.secondaryErrors, summary.secondaryErrors);
  assert.ok(stored.secondaryErrors.some((error) => error.stage === "reporting"));
  assert.equal(await readFile(sentinel, "utf8"), "sentinel\n");
});

test("incremental summary remains explicitly incomplete when report and failure-summary publication both fail", async () => {
  const outputRoot = await root();
  const runDirectory = path.join(outputRoot, RUN_ID);
  let callbacks = 0;
  const summary = await runSuite({
    suite: suite(), adapter: adapter(), outputRoot, runId: RUN_ID,
    onCase: async () => {
      callbacks += 1;
      if (callbacks === 3) await chmod(runDirectory, 0o500);
    },
  });
  await chmod(runDirectory, 0o700);
  const stored = JSON.parse(await readFile(path.join(runDirectory, "summary.json"), "utf8"));
  assert.equal(summary.status, "fail");
  assert.equal(summary.complete, false);
  assert.equal(stored.status, "incomplete");
  assert.equal(stored.complete, false);
  assert.equal(await readFile(path.join(runDirectory, "cases.jsonl"), "utf8").then((value) => value.trim().split("\n").length), 3);
});

test("regradeRun uses stored aliased evidence, makes no adapter call, and never rewrites cases.jsonl", async () => {
  const outputRoot = await root();
  const original = await runSuite({ suite: suite(), adapter: adapter(), outputRoot, runId: RUN_ID });
  const before = await readFile(original.files.cases);
  const beforeStat = await stat(original.files.cases);
  const summary = await regradeRun({ suite: suite(), runDirectory: path.dirname(original.files.cases) });
  const after = await readFile(original.files.cases);
  const afterStat = await stat(original.files.cases);
  assert.equal(summary.status, "pass");
  assert.deepEqual(after, before);
  assert.equal(afterStat.ino, beforeStat.ino);
});

test("regradeRun deterministically regrades the committed alias-only pass golden", async () => {
  const outputRoot = await root();
  const runDirectory = path.join(outputRoot, RUN_ID);
  await cp(new URL("../testdata/reports/pass/", import.meta.url), runDirectory, { recursive: true });
  const goldenSuite = suite([{ id: "no-action", send: { subject: "Synthetic", text: "Synthetic" }, expect: expectation("none") }]);
  const before = await readFile(path.join(runDirectory, "cases.jsonl"));
  const summary = await regradeRun({ suite: goldenSuite, runDirectory });
  assert.equal(summary.status, "pass");
  assert.equal(summary.counts.passed, 1);
  assert.deepEqual(await readFile(path.join(runDirectory, "cases.jsonl")), before);
  assert.equal(await readFile(path.join(runDirectory, "report.md"), "utf8"), await readFile(new URL("../testdata/reports/pass/report.md", import.meta.url), "utf8"));
});

test("regradeRun rejects a changed suite digest and evidence version", async () => {
  const outputRoot = await root();
  const original = await runSuite({ suite: suite(), adapter: adapter(), outputRoot, runId: RUN_ID });
  await assert.rejects(
    regradeRun({ suite: { ...suite(), digest: "b".repeat(64) }, runDirectory: path.dirname(original.files.cases) }),
    (error) => error.errorClass === "configuration_error" && error.code === "suite_digest_mismatch",
  );

  const secondRoot = await root();
  const versioned = await runSuite({ suite: suite(), adapter: adapter(), outputRoot: secondRoot, runId: RUN_ID });
  const records = (await readFile(versioned.files.cases, "utf8")).trim().split("\n").map(JSON.parse);
  records[0].versions.evidence = 2;
  await writeFile(versioned.files.cases, `${records.map(JSON.stringify).join("\n")}\n`);
  await assert.rejects(
    regradeRun({ suite: suite(), runDirectory: path.dirname(versioned.files.cases) }),
    (error) => error.errorClass === "configuration_error" && error.code === "evidence_version_mismatch",
  );
});

test("regrade rejects an interrupted trailing JSONL fragment without rewriting it", async () => {
  const original = await runSuite({ suite: suite(), adapter: adapter(), outputRoot: await root(), runId: RUN_ID });
  const complete = await readFile(original.files.cases, "utf8");
  const interrupted = `${complete}{\"id\":`;
  await writeFile(original.files.cases, interrupted);
  await assert.rejects(
    regradeRun({ suite: suite(), runDirectory: path.dirname(original.files.cases) }),
    (error) => error.errorClass === "configuration_error" && error.code === "interrupted_cases_artifact",
  );
  assert.equal(await readFile(original.files.cases, "utf8"), interrupted);
});

test("regrade rejects newline-aligned omissions and duplicates", async () => {
  for (const mutation of ["omit", "duplicate"]) {
    const original = await runSuite({ suite: suite(), adapter: adapter(), outputRoot: await root(), runId: RUN_ID });
    const lines = (await readFile(original.files.cases, "utf8")).trim().split("\n");
    const changed = mutation === "omit" ? lines.slice(0, -1) : [...lines, lines[0]];
    await writeFile(original.files.cases, `${changed.join("\n")}\n`);
    await assert.rejects(
      regradeRun({ suite: suite(), runDirectory: path.dirname(original.files.cases) }),
      (error) => error.errorClass === "configuration_error" && error.code === "case_set_mismatch",
    );
  }
});
