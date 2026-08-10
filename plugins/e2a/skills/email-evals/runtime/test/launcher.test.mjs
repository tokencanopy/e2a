import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { mkdtemp, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { main as launch, runRuntimeNode } from "../../launcher.mjs";

const ENVIRONMENT = Object.freeze({
  E2A_EVAL_API_KEY: "e2a_acct_synthetic",
  E2A_EVAL_ACTOR: "actor@eval.test",
  E2A_EVAL_TARGET: "target@eval.test",
});

async function fixture() {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-launcher-"));
  const suite = path.join(root, "suite.yaml");
  await writeFile(suite, "version: 1\n");
  return { root, suite };
}

function io() {
  const output = [];
  const errors = [];
  return {
    output, errors,
    stdout: { write: (value) => output.push(value) },
    stderr: { write: (value) => errors.push(value) },
  };
}

function plan() {
  return {
    baseUrl: "https://api.e2a.dev",
    networkSends: false,
    capabilities: ["message_action"],
    recipientAliases: ["actor", "target"],
    protectionDigest: "a".repeat(64),
    timeouts: { maxRetries: 2, maxElapsedMs: 15000, timeoutMs: 10000 },
    executionBudget: { plannedTimeoutMs: 60000, maximumTimeoutMs: 1500000 },
    cases: [{
      id: "synthetic-case",
      stimulus: { action: "send", sender: "actor", recipients: ["target"], subject: "Synthetic question", text: "Synthetic body" },
      expectedAction: { kind: "none", count: 0 },
      expectedSender: { from: null, sentAs: null, replyTo: null, displayName: null },
      expectedRecipients: { to: null, cc: null, bcc: null, envelope: null },
      recipientAliases: [],
      assertions: [
        { id: "action.kind", expected: "none" },
        { id: "action.count", expected: 0 },
        { id: "action.no_duplicates", expected: 0 },
      ],
      evidenceCapabilities: ["message_action"],
      semanticGraders: [],
      timeoutMs: 60000,
      settleMs: 5000,
      pollIntervalMs: 500,
    }],
    approvalDigest: "b".repeat(64),
  };
}

test("launcher rejects empty and action-inconsistent command results", async () => {
  const { root, suite } = await fixture();
  const mismatchedSentAs = plan();
  mismatchedSentAs.cases[0].expectedSender.sentAs = "e2a_custom";
  mismatchedSentAs.cases[0].assertions.push({ id: "sender.sent_as", expected: "future_route" });
  const invalidPlans = [
    { ...plan(), cases: [] },
    { ...plan(), cases: [{ ...plan().cases[0], expectedAction: { kind: "none", count: 1 } }] },
    mismatchedSentAs,
  ];
  for (const invalid of invalidPlans) {
    const streams = io();
    assert.equal(await launch(["validate", "--suite", suite, "--json"], {
      ...streams, cwd: root, environment: ENVIRONMENT,
      spawnRuntime: async () => result(`${JSON.stringify({ command: "validate", plan: invalid })}\n`),
    }), 4);
    assert.equal(streams.errors.join(""), "email-evals: runtime failure\n");
  }

  const streams = io();
  const empty = completed();
  empty.summary.counts = { total: 0, passed: 0, failed: 0, errors: 0 };
  empty.summary.cases = [];
  assert.equal(await launch(["run", "--suite", suite, "--approval-digest", "b".repeat(64), "--json"], {
    ...streams, cwd: root, environment: ENVIRONMENT,
    spawnRuntime: async () => result(`${JSON.stringify(empty)}\n`),
  }), 4);
  assert.equal(streams.errors.join(""), "email-evals: runtime failure\n");
});

function completed(errorClass = null) {
  const failed = errorClass === "assertion_failure";
  const runId = "run_20260809T120000_0123abcd";
  return {
    command: "run",
    summary: {
      runId,
      status: errorClass === null ? "pass" : "fail",
      complete: true,
      counts: errorClass === null
        ? { total: 1, passed: 1, failed: 0, errors: 0 }
        : { total: 1, passed: 0, failed: failed ? 1 : 0, errors: failed ? 0 : 1 },
      capabilities: ["message_action"],
      cases: errorClass === null
        ? [{ id: "synthetic-case", status: "pass" }]
        : [{ id: "synthetic-case", status: failed ? "fail" : "error", errorClass }],
    },
    report: `${runId}/report.md`,
  };
}

function result(output, code = 0, stderr = "") {
  return {
    code, stdout: output, stderr, truncated: false, finalized: true, termination: "exit",
  };
}

test("help and complete grammar are dependency-free and reject invalid options", async () => {
  const help = io();
  assert.equal(await launch(["--help"], help), 0);
  assert.match(help.output.join(""), /--trusted-origin/);
  for (const args of [
    ["validate", "--suite", "suite.yaml", "--suite", "again.yaml"],
    ["validate", "--suite", "suite.yaml", "--output", "results"],
    ["run", "--suite", "suite.yaml", "extra"],
  ]) {
    const streams = io();
    assert.equal(await launch(args, streams), 2);
    assert.equal(streams.output.join(""), "");
  }
});

test("launcher rejects symlinked suites before invoking the trusted runtime", async () => {
  const { root, suite } = await fixture();
  const linked = path.join(root, "linked.yaml");
  await symlink(suite, linked);
  let invoked = false;
  const streams = io();
  assert.equal(await launch(["validate", "--suite", linked], {
    ...streams, cwd: root, environment: ENVIRONMENT,
    spawnRuntime: async () => { invoked = true; return result(""); },
  }), 2);
  assert.equal(invoked, false);
  assert.equal(streams.errors.join(""), "email-evals: runtime unavailable\n");
});

test("launcher forces JSON from the child and renders a complete alias-only human plan", async () => {
  const { root, suite } = await fixture();
  const streams = io();
  let args;
  const output = { command: "validate", plan: plan() };
  assert.equal(await launch(["validate", "--suite", suite], {
    ...streams, cwd: root, environment: ENVIRONMENT,
    spawnRuntime: async (childArgs) => { args = childArgs; return result(`${JSON.stringify(output)}\n`); },
  }), 0);
  assert.equal(args.at(-1), "--json");
  assert.match(streams.output.join(""), /"protectionDigest"/);
  assert.match(streams.output.join(""), /"expectedAction"/);
  assert.match(streams.output.join(""), /"Synthetic body"/);
  assert.match(streams.output.join(""), /"action.no_duplicates"/);
  assert.doesNotMatch(streams.output.join(""), /@eval\.test|e2a_acct_/);

  const openSentAs = plan();
  openSentAs.cases[0].expectedSender.sentAs = "e2a_custom";
  openSentAs.cases[0].assertions.push({ id: "sender.sent_as", expected: "e2a_custom" });
  const sentAsStreams = io();
  assert.equal(await launch(["validate", "--suite", suite, "--json"], {
    ...sentAsStreams, cwd: root, environment: ENVIRONMENT,
    spawnRuntime: async () => result(`${JSON.stringify({ command: "validate", plan: openSentAs })}\n`),
  }), 0);
  assert.equal(JSON.parse(sentAsStreams.output.join("")).plan.cases[0].expectedSender.sentAs, "e2a_custom");
});

test("exact completed schemas preserve every result class and reject impossible counts", async () => {
  const { root, suite } = await fixture();
  for (const [errorClass, exitCode] of [
    [null, 0], ["assertion_failure", 1], ["configuration_error", 2], ["capability_error", 2],
    ["transport_error", 3], ["target_timeout", 3], ["grader_error", 4],
  ]) {
    const output = completed(errorClass);
    const streams = io();
    assert.equal(await launch(["run", "--suite", suite, "--approval-digest", "b".repeat(64), "--json"], {
      ...streams, cwd: root, environment: ENVIRONMENT,
      spawnRuntime: async () => result(`${JSON.stringify(output)}\n`, exitCode),
    }), exitCode, String(errorClass));
    assert.equal(streams.output.join(""), `${JSON.stringify(output)}\n`);
    assert.equal(streams.errors.join(""), "");
  }

  const impossible = completed("transport_error");
  impossible.summary.counts = { total: 1, passed: 1, failed: 0, errors: 0 };
  const streams = io();
  assert.equal(await launch(["run", "--suite", suite, "--approval-digest", "b".repeat(64), "--json"], {
    ...streams, cwd: root, environment: ENVIRONMENT,
    spawnRuntime: async () => result(`${JSON.stringify(impossible)}\n`, 3),
  }), 4);
  assert.equal(streams.output.join(""), "");
  assert.equal(streams.errors.join(""), "email-evals: runtime failure\n");
});

test("known diagnostic-only failures pass through and all other child data is contained", async () => {
  const { root, suite } = await fixture();
  const known = io();
  assert.equal(await launch(["validate", "--suite", suite], {
    ...known, cwd: root, environment: ENVIRONMENT,
    spawnRuntime: async () => result("", 2, "email-evals: configuration_error\n"),
  }), 2);
  assert.equal(known.errors.join(""), "email-evals: configuration_error\n");

  const hostile = io();
  assert.equal(await launch(["validate", "--suite", suite], {
    ...hostile, cwd: root, environment: ENVIRONMENT,
    spawnRuntime: async () => result("e2a_acct_secret\n", 4, "/private/tmp/secret\n"),
  }), 4);
  assert.equal(hostile.output.join(""), "");
  assert.equal(hostile.errors.join(""), "email-evals: runtime failure\n");
});

function fakeChild() {
  const child = new EventEmitter();
  child.stdout = new EventEmitter();
  child.stderr = new EventEmitter();
  child.stdout.destroy = () => {};
  child.stderr.destroy = () => {};
  child.kill = () => true;
  child.unref = () => {};
  return child;
}

test("runtime capture ignores stdin, waits for both streams, and enforces a hard wall", async () => {
  const child = fakeChild();
  const timers = [];
  let options;
  const running = runRuntimeNode([], {}, {
    spawnChild: (_node, _args, value) => { options = value; return child; },
    setTimer: (callback) => { timers.push(callback); return callback; },
    clearTimer: () => {},
    wallTimeoutMs: 1,
  });
  assert.equal(options.stdio[0], "ignore");
  child.stdout.emit("data", Buffer.from("safe\n"));
  child.emit("exit", 0, null);
  child.stdout.emit("end");
  child.stderr.emit("end");
  assert.deepEqual(await running, {
    code: 0, stdout: "safe\n", stderr: "", truncated: false, finalized: true, termination: "exit",
  });

  const hung = fakeChild();
  let killed = 0;
  hung.kill = () => { killed += 1; return true; };
  const wallTimers = [];
  const wall = runRuntimeNode([], {}, {
    spawnChild: () => hung,
    setTimer: (callback) => { wallTimers.push(callback); return callback; },
    clearTimer: () => {},
    wallTimeoutMs: 1,
  });
  wallTimers[0]();
  assert.equal(killed, 1);
  assert.equal((await wall).termination, "wall_timeout");
});
