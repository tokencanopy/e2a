import assert from "node:assert/strict";
import { chmod, mkdtemp, mkdir, readFile, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { EvalError } from "../lib/errors.mjs";
import { main } from "../cli.mjs";

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
  return {
    runId: "run_20260808T120000_0123abcd",
    status: errorClass ? "fail" : "pass",
    complete: true,
    counts: errorClass ? { total: 1, passed: 0, failed: 1, errors: 0 } : { total: 1, passed: 1, failed: 0, errors: 0 },
    capabilities: CAPABILITIES,
    suite: { name: "fictional-support-smoke", version: 1, digest: "a".repeat(64) },
    cases: errorClass ? [{ id: "reply", status: "fail", primaryError: { class: errorClass, code: "assertions_failed" } }] : [],
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
      return { capabilities: CAPABILITIES, plan: { networkSends: false, recipientAliases: ["actor", "target"], cases: [{ id: "reply" }] } };
    },
    async executeCase() { calls.push("send"); },
  };
  return {
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
      ...overrides.dependencies,
    },
  };
}

function stdout(h) { return h.output.join(""); }
function stderr(h) { return h.errors.join(""); }

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
  assert.doesNotMatch(stdout(h), /e2a_acct_|actor@eval\.test|target@eval\.test|api\.example\.test/);
  assert.equal(JSON.parse(stdout(h)).plan.networkSends, false);
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
  assert.equal(await main(["run", "--suite", "suite.yaml", "--output", "relative-results"], pass.dependencies), 0);
  assert.deepEqual(pass.calls, ["load:/cwd/suite.yaml", "adapter", "preflight", "run:/cwd/relative-results"]);

  const assertion = harness({ runSummary: summary("assertion_failure") });
  assert.equal(await main(["run", "--suite", "suite.yaml"], assertion.dependencies), 1);

  const configuration = harness({ dependencies: { runSuite: async () => { throw new EvalError("configuration_error", "preflight_failed", "synthetic"); } } });
  assert.equal(await main(["run", "--suite", "suite.yaml"], configuration.dependencies), 2);
});

test("regrade loads the suite but creates no adapter and makes no transport call", async () => {
  const h = harness();
  assert.equal(await main(["regrade", "--suite", "suite.yaml", "--run", "runs/run_20260808T120000_0123abcd", "--json"], h.dependencies), 0);
  assert.deepEqual(h.calls, ["load:/cwd/suite.yaml", "regrade:/cwd/runs/run_20260808T120000_0123abcd"]);
  assert.equal(Object.keys(JSON.parse(stdout(h)))[0], "command");
});

test("JSON stdout is one object and human output carries a safe report path", async () => {
  const json = harness();
  assert.equal(await main(["run", "--suite", "suite.yaml", "--json"], json.dependencies), 0);
  assert.equal(stdout(json).split("\n").filter(Boolean).length, 1);
  assert.equal(JSON.parse(stdout(json)).command, "run");

  const human = harness();
  assert.equal(await main(["run", "--suite", "suite.yaml"], human.dependencies), 0);
  assert.match(stdout(human), /Report: \.\.\/results\/run_20260808T120000_0123abcd\/report\.md/);
  assert.doesNotMatch(stdout(human), /actor@eval\.test|target@eval\.test|e2a_acct_/);

  const pathLeak = summary();
  pathLeak.files.report = `/results/${ACTOR}/e2a_acct_synthetic/report.md`;
  const redactedPath = harness({ runSummary: pathLeak });
  assert.equal(await main(["run", "--suite", "suite.yaml"], redactedPath.dependencies), 0);
  assert.doesNotMatch(stdout(redactedPath), /actor@eval\.test|e2a_acct_/);
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

test("launcher routes scaffold locally and runtime commands through the suite runtime", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-cli-"));
  const suite = path.join(root, "suite.yaml");
  const runtime = path.join(root, ".eval-runtime");
  const captured = path.join(root, "captured.json");
  await mkdir(runtime);
  await writeFile(suite, "version: 1\n");
  await writeFile(path.join(runtime, "cli.mjs"), `import { writeFile } from "node:fs/promises"; await writeFile(${JSON.stringify(captured)}, JSON.stringify(process.argv.slice(2)));`);
  await chmod(path.join(runtime, "cli.mjs"), 0o700);
  const launcher = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../email-evals.sh");
  const invoked = await run("bash", [launcher, "validate", "--suite", suite, "--json"]);
  assert.equal(invoked.code, 0);
  assert.deepEqual(JSON.parse(await readFile(captured, "utf8")), ["validate", "--suite", suite, "--json"]);

  const scaffoldRoot = path.join(root, "scaffolded");
  const scaffolded = await run("bash", [
    launcher, "scaffold", "--root", scaffoldRoot, "--name", "fictional-support-smoke",
    "--target-env", "E2A_EVAL_TARGET", "--actor-env", "E2A_EVAL_ACTOR", "--api-key-env", "E2A_EVAL_API_KEY",
  ]);
  assert.equal(scaffolded.code, 0);
  assert.match(await readFile(path.join(scaffoldRoot, "suite.yaml"), "utf8"), /fictional-support-smoke/);
});
