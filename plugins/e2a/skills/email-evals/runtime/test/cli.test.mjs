import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { EvalError } from "../lib/errors.mjs";
import { main, validateReportArtifact } from "../cli.mjs";

const ACTOR = "actor@eval.test";
const TARGET = "target@eval.test";
const CAPABILITIES = [
  "message_action", "visible_recipients", "blind_recipients", "envelope_recipients",
  "thread_headers", "raw_mime", "attachment_hashes", "delivery_lifecycle",
];

function fixtureSuite(overrides = {}) {
  return {
    version: 1,
    name: "fictional-support-smoke",
    digest: "a".repeat(64),
    executionDigest: "b".repeat(64),
    suiteFile: "/resolved/suite.yaml",
    suiteRoot: "/resolved",
    actor: { email: ACTOR },
    target: { email: TARGET },
    transport: { apiKey: "e2a_acct_synthetic", baseUrl: "https://api.example.test", allowedEnvelopeRecipients: [ACTOR, TARGET] },
    defaults: { timeoutMs: 100, settleMs: 0, pollIntervalMs: 1 },
    cases: [{ id: "reply", expect: { action: { kind: "reply", count: 1 } } }],
    ...overrides,
  };
}

function summary(errorClass = null) {
  const assertionFailure = errorClass === "assertion_failure";
  return {
    runId: "run_20260808T120000_0123abcd",
    status: errorClass ? "fail" : "pass",
    complete: true,
    counts: errorClass
      ? { total: 1, passed: 0, failed: assertionFailure ? 1 : 0, errors: assertionFailure ? 0 : 1 }
      : { total: 1, passed: 1, failed: 0, errors: 0 },
    capabilities: CAPABILITIES,
    suite: { name: "fictional-support-smoke", version: 1, digest: "a".repeat(64) },
    cases: errorClass ? [{ id: "reply", status: assertionFailure ? "fail" : "error", primaryError: { class: errorClass, code: "synthetic" } }] : [],
    files: { report: "/results/run_20260808T120000_0123abcd/report.md" },
  };
}

function harness(overrides = {}) {
  const output = [];
  const errors = [];
  const calls = [];
  const suite = overrides.suite ?? fixtureSuite();
  const adapter = overrides.adapter ?? {
    capabilities: CAPABILITIES,
    async preflight() {
      calls.push("preflight");
      return {
        capabilities: CAPABILITIES,
        protectionDigest: "c".repeat(64),
        plan: {
          baseUrl: "https://api.example.test", networkSends: false,
          capabilities: CAPABILITIES, recipientAliases: ["actor", "target"],
          timeouts: { maxRetries: 2, maxElapsedMs: 15000, timeoutMs: 10000 },
          executionBudget: { plannedTimeoutMs: 100, maximumTimeoutMs: 1_500_000 },
          cases: [{
            id: "reply",
            stimulus: { action: "send", sender: "actor", recipients: ["target"], subject: "Synthetic question", text: "Synthetic body" },
            expectedAction: { kind: "reply", count: 1 },
            expectedSender: { from: null, sentAs: null, replyTo: null, displayName: null },
            expectedRecipients: { to: null, cc: null, bcc: null, envelope: null },
            recipientAliases: [],
            assertions: [
              { id: "action.kind", expected: "reply" },
              { id: "action.count", expected: 1 },
              { id: "action.no_duplicates", expected: 1 },
            ],
            evidenceCapabilities: ["message_action"],
            semanticGraders: [],
          }],
        },
      };
    },
    async executeCase() { calls.push("send"); },
  };
  return {
    adapter,
    calls,
    output,
    errors,
    dependencies: {
      cwd: "/cwd",
      environment: {},
      stdout: { write: (value) => output.push(value) },
      stderr: { write: (value) => errors.push(value) },
      loadSuite: async (file) => { calls.push(`load:${file}`); return suite; },
      createAdapter: () => { calls.push("adapter"); return adapter; },
      runSuite: async ({ adapter: passedAdapter, outputRoot }) => {
        calls.push(`run:${outputRoot}`);
        await passedAdapter.preflight(suite);
        return overrides.runSummary ?? summary();
      },
      regradeRun: async ({ runDirectory }) => {
        calls.push(`regrade:${runDirectory}`);
        return overrides.regradeSummary ?? summary();
      },
      // Unit runner results are synthetic; artifact validation has direct
      // filesystem regressions below.
      validateReport: async () => "run_20260808T120000_0123abcd/report.md",
      ...overrides.dependencies,
    },
  };
}

function stdout(h) { return h.output.join(""); }
function stderr(h) { return h.errors.join(""); }

async function approve(h) {
  assert.equal(await main(["validate", "--suite", "suite.yaml", "--json"], h.dependencies), 0);
  const digest = JSON.parse(stdout(h)).plan.approvalDigest;
  assert.match(digest, /^[a-f0-9]{64}$/);
  h.output.length = 0;
  h.errors.length = 0;
  h.calls.length = 0;
  return digest;
}

test("help is local and exact command grammar rejects unknown commands", async () => {
  const help = harness();
  assert.equal(await main(["--help"], help.dependencies), 0);
  assert.match(stdout(help), /email-evals validate --suite <suite.yaml>/);
  assert.deepEqual(help.calls, []);

  const unknown = harness();
  assert.equal(await main(["erase", "--suite", "suite.yaml"], unknown.dependencies), 2);
  assert.equal(stderr(unknown), "Unknown command\n");
  assert.deepEqual(unknown.calls, []);
});

test("strict parsing rejects duplicate unknown missing illegal and positional arguments before loading", async () => {
  for (const [args, expected] of [
    [["run", "--suite", "suite.yaml", "--suite", "again.yaml"], "Duplicate option: --suite"],
    [["run", "--suite", "suite.yaml", "--retain-raw"], "Unknown option: --retain-raw"],
    [["run", "--suite"], "Missing value for: --suite"],
    [["validate", "--suite", "suite.yaml", "--output", "results"], "Option not allowed for validate: --output"],
    [["regrade", "--suite", "suite.yaml", "--run", "run", "extra"], "Unexpected positional argument"],
  ]) {
    const h = harness();
    assert.equal(await main(args, h.dependencies), 2);
    assert.equal(stderr(h), `${expected}\n`);
    assert.deepEqual(h.calls, []);
  }
});

test("validate performs full preflight but never sends and emits only aliases", async () => {
  const h = harness();
  assert.equal(await main(["validate", "--suite", "suite.yaml", "--json"], h.dependencies), 0);
  assert.deepEqual(h.calls, ["load:/cwd/suite.yaml", "adapter", "preflight"]);
  assert.doesNotMatch(stdout(h), /e2a_acct_|actor@eval\.test|target@eval\.test/);
  const plan = JSON.parse(stdout(h)).plan;
  assert.equal(plan.networkSends, false);
  assert.deepEqual(plan.cases[0].stimulus, {
    action: "send", sender: "actor", recipients: ["target"], subject: "Synthetic question", text: "Synthetic body",
  });
  assert.deepEqual(plan.cases[0].assertions.map(({ id }) => id), ["action.kind", "action.count", "action.no_duplicates"]);
  assert.deepEqual(plan.cases[0].semanticGraders, []);
});

test("validate preserves a legitimate open sent-as token in both typed plan representations", async () => {
  const configured = fixtureSuite({
    cases: [{
      id: "reply",
      expect: { action: { kind: "reply", count: 1 }, sender: { sentAs: "e2a_custom" } },
    }],
  });
  const h = harness({ suite: configured });
  const originalPreflight = h.adapter.preflight.bind(h.adapter);
  h.adapter.preflight = async () => {
    const result = await originalPreflight();
    result.plan.cases[0].expectedSender.sentAs = "e2a_custom";
    result.plan.cases[0].assertions.push({ id: "sender.sent_as", expected: "e2a_custom" });
    return result;
  };
  assert.equal(await main(["validate", "--suite", "suite.yaml", "--json"], h.dependencies), 0);
  const planned = JSON.parse(stdout(h)).plan.cases[0];
  assert.equal(planned.expectedSender.sentAs, "e2a_custom");
  assert.equal(planned.assertions.find(({ id }) => id === "sender.sent_as").expected, "e2a_custom");
});

test("run requires and verifies the exact digest of the approved validation plan", async () => {
  const h = harness();
  assert.equal(await main(["validate", "--suite", "suite.yaml", "--json"], h.dependencies), 0);
  const approvalDigest = JSON.parse(stdout(h)).plan.approvalDigest;
  assert.match(approvalDigest, /^[a-f0-9]{64}$/);

  h.output.length = 0;
  h.errors.length = 0;
  h.calls.length = 0;
  assert.equal(await main([
    "run", "--suite", "suite.yaml", "--approval-digest", approvalDigest, "--json",
  ], h.dependencies), 0);
  assert.deepEqual(h.calls, ["load:/cwd/suite.yaml", "adapter", "preflight", "run:/cwd/results"]);

  h.output.length = 0;
  h.errors.length = 0;
  h.calls.length = 0;
  h.dependencies.loadSuite = async (file) => {
    h.calls.push(`load:${file}`);
    return { ...fixtureSuite(), digest: "f".repeat(64) };
  };
  assert.equal(await main([
    "run", "--suite", "suite.yaml", "--approval-digest", approvalDigest, "--json",
  ], h.dependencies), 2);
  assert.deepEqual(h.calls, ["load:/cwd/suite.yaml", "adapter", "preflight"]);
  assert.equal(stdout(h), "");
  assert.equal(stderr(h), "email-evals: configuration_error\n");
});

test("validate rejects a missing requested capability before reporting its plan", async () => {
  const capabilitySuite = fixtureSuite({
    cases: [{ id: "blind-copy", expect: { action: { kind: "reply", count: 1 }, recipients: { bcc: { exactly: [ACTOR] } } } }],
  });
  const h = harness({ adapter: {
    capabilities: ["message_action"],
    async preflight() { return { capabilities: ["message_action"], plan: { networkSends: false } }; },
    async executeCase() { throw new Error("must not execute"); },
  }, suite: capabilitySuite });
  assert.equal(await main(["validate", "--suite", "suite.yaml"], h.dependencies), 2);
  assert.match(stderr(h), /capability_error/);
});

test("validate capability mapping covers every runner assertion family", async () => {
  const allFamilies = fixtureSuite({
    cases: [{
      id: "every-family",
      expect: {
        action: { kind: "reply", count: 1 },
        sender: { exactly: TARGET },
        recipients: { to: { exactly: [ACTOR] }, cc: { exactly: [] }, bcc: { exactly: [] }, envelope: { exactly: [ACTOR] } },
        thread: { inReplyTo: "required" },
        subject: { exact: "Synthetic" },
        body: { plainText: "required" },
        attachments: { exactly: [] },
        lifecycle: { submission: "sent" },
      },
    }],
  });
  for (const missing of CAPABILITIES) {
    const available = CAPABILITIES.filter((entry) => entry !== missing);
    const h = harness({ suite: allFamilies, adapter: {
      capabilities: available,
      async preflight() { return { capabilities: available, plan: { networkSends: false } }; },
      async executeCase() { throw new Error("must not execute"); },
    } });
    assert.equal(await main(["validate", "--suite", "suite.yaml"], h.dependencies), 2, missing);
    assert.match(stderr(h), /capability_error/, missing);
  }
});

test("run caches its preflight for runSuite and maps result and preflight errors", async () => {
  const pass = harness();
  const passApproval = await approve(pass);
  assert.equal(await main(["run", "--suite", "suite.yaml", "--approval-digest", passApproval, "--output", "relative-results"], pass.dependencies), 0);
  assert.deepEqual(pass.calls, ["load:/cwd/suite.yaml", "adapter", "preflight", "run:/cwd/relative-results"]);

  const assertion = harness({ runSummary: summary("assertion_failure") });
  const assertionApproval = await approve(assertion);
  assert.equal(await main(["run", "--suite", "suite.yaml", "--approval-digest", assertionApproval], assertion.dependencies), 1);

  const configuration = harness({ dependencies: { runSuite: async () => { throw new EvalError("configuration_error", "preflight_failed", "synthetic"); } } });
  const configurationApproval = await approve(configuration);
  assert.equal(await main(["run", "--suite", "suite.yaml", "--approval-digest", configurationApproval], configuration.dependencies), 2);
});

test("completed human run and regrade summaries emit their fixed exit diagnostic", async () => {
  for (const command of ["run", "regrade"]) {
    for (const [errorClass, exitCode] of [
      ["assertion_failure", 1],
      ["configuration_error", 2],
      ["capability_error", 2],
      ["transport_error", 3],
      ["target_timeout", 3],
      ["grader_error", 4],
    ]) {
      const h = harness(command === "run"
        ? { runSummary: summary(errorClass) }
        : { regradeSummary: summary(errorClass) });
      const args = command === "run"
        ? ["run", "--suite", "suite.yaml", "--approval-digest", await approve(h)]
        : ["regrade", "--suite", "suite.yaml", "--run", "run"];
      assert.equal(await main(args, h.dependencies), exitCode, `${command}:${errorClass}`);
      assert.match(stdout(h), /^Status: fail; 0\/1 passed\nComplete: yes\nReport: run_[a-zA-Z0-9_]+\/report\.md\n$/);
      assert.equal(stderr(h), `email-evals: ${errorClass}\n`, `${command}:${errorClass}`);
    }
  }
});

test("incomplete run and regrade summaries never publish human results", async () => {
  for (const command of ["run", "regrade"]) {
    const incomplete = summary("transport_error");
    incomplete.complete = false;
    const h = harness(command === "run"
      ? { runSummary: incomplete }
      : { regradeSummary: incomplete });
    const args = command === "run"
      ? ["run", "--suite", "suite.yaml", "--approval-digest", await approve(h)]
      : ["regrade", "--suite", "suite.yaml", "--run", "run"];
    assert.equal(await main(args, h.dependencies), 4, command);
    assert.equal(stdout(h), "", command);
    assert.equal(stderr(h), "email-evals: unexpected runner failure\n", command);
  }
});

test("preflight preserves every recognized error class and makes unknown failure unexpected", async () => {
  for (const [errorClass, exit] of [
    ["assertion_failure", 1], ["configuration_error", 2], ["capability_error", 2],
    ["transport_error", 3], ["target_timeout", 3], ["grader_error", 4],
  ]) {
    const h = harness({ adapter: {
      async preflight() { throw new EvalError(errorClass, "synthetic", "unsafe actor@eval.test"); },
      async executeCase() { throw new Error("must not execute"); },
    } });
    assert.equal(await main(["run", "--suite", "suite.yaml", "--approval-digest", "d".repeat(64)], h.dependencies), exit, errorClass);
    assert.equal(stderr(h), `email-evals: ${errorClass}\n`, errorClass);
  }
  const unknown = harness({ adapter: {
    async preflight() { throw new Error("unsafe actor@eval.test"); },
    async executeCase() { throw new Error("must not execute"); },
  } });
  assert.equal(await main(["run", "--suite", "suite.yaml", "--approval-digest", "d".repeat(64)], unknown.dependencies), 4);
  assert.equal(stderr(unknown), "email-evals: grader_error\n");
});

test("regrade loads the suite but creates no adapter and makes no transport call", async () => {
  const h = harness();
  assert.equal(await main(["regrade", "--suite", "suite.yaml", "--run", "runs/run_20260808T120000_0123abcd", "--json"], h.dependencies), 0);
  assert.deepEqual(h.calls, ["load:/cwd/suite.yaml", "regrade:/cwd/runs/run_20260808T120000_0123abcd"]);
  assert.equal(Object.keys(JSON.parse(stdout(h)))[0], "command");
});

test("JSON stdout is one object and human output carries a safe report path", async () => {
  const json = harness();
  const jsonApproval = await approve(json);
  assert.equal(await main(["run", "--suite", "suite.yaml", "--approval-digest", jsonApproval, "--json"], json.dependencies), 0);
  assert.equal(stdout(json).split("\n").filter(Boolean).length, 1);
  assert.equal(JSON.parse(stdout(json)).command, "run");

  const human = harness();
  const humanApproval = await approve(human);
  assert.equal(await main(["run", "--suite", "suite.yaml", "--approval-digest", humanApproval], human.dependencies), 0);
  assert.match(stdout(human), /Report: run_20260808T120000_0123abcd\/report\.md/);
  assert.doesNotMatch(stdout(human), /actor@eval\.test|target@eval\.test|e2a_acct_/);

  const pathLeak = summary();
  pathLeak.files.report = `/results/${ACTOR}/e2a_acct_synthetic/report.md`;
  const rejectedPath = harness({ runSummary: pathLeak, dependencies: { validateReport: async () => { throw new Error("unsafe path"); } } });
  const rejectedApproval = await approve(rejectedPath);
  assert.equal(await main(["run", "--suite", "suite.yaml", "--approval-digest", rejectedApproval, "--json"], rejectedPath.dependencies), 4);
  assert.equal(stdout(rejectedPath), "");
  assert.equal(stderr(rejectedPath), "email-evals: unexpected runner failure\n");
});

test("report artifacts must be regular expected files in their canonical run directory", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-report-"));
  const runId = "run_20260808T120000_0123abcd";
  const run = path.join(root, runId);
  const report = path.join(run, "report.md");
  await mkdir(run);
  await writeFile(report, "# Synthetic report\n");
  const valid = { runId, files: { report } };
  assert.equal(await validateReportArtifact({ command: "run", summary: valid, outputRoot: root }), `${runId}/report.md`);
  assert.equal(await validateReportArtifact({ command: "regrade", summary: valid, runDirectory: run }), `${runId}/report.md`);

  const aliasContainer = await mkdtemp(path.join(tmpdir(), "email-evals-report-alias-"));
  const canonicalParent = path.join(aliasContainer, "canonical-parent");
  const aliasParent = path.join(aliasContainer, "alias-parent");
  const aliasedRoot = path.join(canonicalParent, "results");
  const aliasedRun = path.join(aliasedRoot, runId);
  const aliasedReport = path.join(aliasedRun, "report.md");
  await mkdir(aliasedRun, { recursive: true });
  await writeFile(aliasedReport, "# Synthetic report\n");
  await symlink(canonicalParent, aliasParent, "dir");
  assert.equal(await validateReportArtifact({
    command: "run",
    summary: { runId, files: { report: aliasedReport } },
    outputRoot: path.join(aliasParent, "results"),
  }), `${runId}/report.md`);

  for (const altered of [
    { runId, files: {} },
    { runId, files: { report: 42 } },
    { runId, files: { report: path.join(root, "elsewhere", "report.md") } },
    { runId, files: { report: path.join(run, "..", "report.md") } },
    { runId: "run_20260808T120000_deadbeef", files: { report } },
  ]) {
    await assert.rejects(validateReportArtifact({ command: "run", summary: altered, outputRoot: root }));
  }

  const linkedRun = path.join(root, "run_20260808T120001_0123abcd");
  await mkdir(linkedRun);
  const linkedReport = path.join(linkedRun, "report.md");
  await symlink(report, linkedReport);
  await assert.rejects(validateReportArtifact({
    command: "run",
    summary: { runId: path.basename(linkedRun), files: { report: linkedReport } },
    outputRoot: root,
  }));
});

test("unexpected errors have stable safe diagnostics", async () => {
  const h = harness({ dependencies: { loadSuite: async () => { throw new Error("e2a_acct_synthetic actor@eval.test"); } } });
  assert.equal(await main(["validate", "--suite", "suite.yaml"], h.dependencies), 4);
  assert.equal(stderr(h), "email-evals: unexpected runner failure\n");
});

function run(command, args, options = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { ...options, stdio: ["ignore", "pipe", "pipe"] });
    let output = "";
    let errors = "";
    child.stdout.on("data", (chunk) => { output += chunk; });
    child.stderr.on("data", (chunk) => { errors += chunk; });
    child.once("error", reject);
    child.once("exit", (code) => resolve({ code, output, errors }));
  });
}

test("launcher routes scaffold locally without depending on a suite runtime", async () => {
  const root = await mkdtemp(path.join("/private/tmp", "email-evals-cli-"));
  const launcher = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../email-evals.sh");
  const scaffoldRoot = path.join(root, "scaffolded");
  const scaffolded = await run("bash", [
    launcher, "scaffold", "--root", scaffoldRoot, "--name", "fictional-support-smoke",
    "--target-env", "E2A_EVAL_TARGET", "--actor-env", "E2A_EVAL_ACTOR",
  ]);
  assert.equal(scaffolded.code, 0, scaffolded.errors);
  assert.match(await readFile(path.join(scaffoldRoot, "suite.yaml"), "utf8"), /fictional-support-smoke/);
});
