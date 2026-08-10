import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { spawn } from "node:child_process";
import { mkdtemp, mkdir, readFile, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { main as launch, runRuntimeNode } from "../../launcher.mjs";
import { installRuntime } from "../../setup.mjs";
import { loadSuite } from "../lib/contract.mjs";

const ENVIRONMENT = Object.freeze({
  E2A_EVAL_API_KEY: "e2a_acct_synthetic",
  E2A_EVAL_ACTOR: "actor@eval.test",
  E2A_EVAL_TARGET: "target@eval.test",
});

async function writeSuite(root, { apiKey = "${E2A_EVAL_API_KEY}", baseUrl, text = "Synthetic question" } = {}) {
  await writeFile(path.join(root, "case.yaml"), [
    "id: synthetic-case",
    "send:",
    "  subject: Synthetic question",
    `  text: ${JSON.stringify(text)}`,
    "expect:",
    "  action: { kind: none, count: 0 }",
    "  lifecycle: { actor_received: false }",
    "",
  ].join("\n"));
  await writeFile(path.join(root, "suite.yaml"), [
    "version: 1",
    "name: synthetic-suite",
    "target: { email: \"${E2A_EVAL_TARGET}\" }",
    "actor: { email: \"${E2A_EVAL_ACTOR}\" }",
    "transport:",
    "  adapter: e2a",
    `  api_key: ${JSON.stringify(apiKey)}`,
    ...(baseUrl === undefined ? [] : [`  base_url: ${JSON.stringify(baseUrl)}`]),
    "  allowed_envelope_recipients: [\"${E2A_EVAL_ACTOR}\", \"${E2A_EVAL_TARGET}\"]",
    "cases: [case.yaml]",
    "",
  ].join("\n"));
  return path.join(root, "suite.yaml");
}

function validationPlan() {
  return {
    baseUrl: "https://api.e2a.dev",
    networkSends: false,
    capabilities: [],
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

function streams() {
  const output = [];
  const errors = [];
  return {
    output,
    errors,
    stdout: { write: (value) => output.push(value) },
    stderr: { write: (value) => errors.push(value) },
  };
}

test("suite fields cannot select arbitrary inherited credentials or interpolate send content", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-final-security-"));
  const suite = await writeSuite(root, { apiKey: "${AWS_SECRET_ACCESS_KEY}" });
  await assert.rejects(
    loadSuite(suite, { environment: { ...ENVIRONMENT, AWS_SECRET_ACCESS_KEY: "do-not-disclose" } }),
    (error) => error.errorClass === "configuration_error"
      && error.code === "environment_reference_not_allowed"
      && !JSON.stringify(error.toJSON()).includes("do-not-disclose"),
  );

  await writeSuite(root, { text: "${E2A_EVAL_API_KEY}" });
  await assert.rejects(
    loadSuite(suite, { environment: ENVIRONMENT }),
    (error) => error.errorClass === "configuration_error" && error.code === "environment_reference_not_allowed",
  );
});

test("custom origins require an exact operator-controlled opt-in and cleartext stays loopback-only", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-final-origin-"));
  const suite = await writeSuite(root, { baseUrl: "https://api.example.test" });
  await assert.rejects(
    loadSuite(suite, { environment: ENVIRONMENT }),
    (error) => error.errorClass === "configuration_error" && error.code === "untrusted_base_url",
  );
  const allowed = await loadSuite(suite, { environment: ENVIRONMENT, trustedOrigin: "https://api.example.test" });
  assert.equal(allowed.transport.baseUrl, "https://api.example.test");
  await assert.rejects(
    loadSuite(suite, { environment: ENVIRONMENT, trustedOrigin: "https://other.example.test" }),
    (error) => error.errorClass === "configuration_error" && error.code === "untrusted_base_url",
  );

  await writeSuite(root, { baseUrl: "http://api.example.test" });
  await assert.rejects(
    loadSuite(suite, { environment: ENVIRONMENT, trustedOrigin: "http://api.example.test" }),
    (error) => error.errorClass === "configuration_error" && error.code === "insecure_base_url",
  );
});

test("setup installs beside trusted plugin source and never copies executable suite-local runtime", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-final-setup-"));
  const sourceRoot = path.join(root, "trusted-source");
  const suiteRoot = path.join(root, "suite");
  await mkdir(sourceRoot, { recursive: true });
  await mkdir(path.join(suiteRoot, ".eval-runtime"), { recursive: true });
  await writeFile(path.join(sourceRoot, "email-evals-runtime.bundle.mjs"), "export {};\n");
  await writeFile(path.join(sourceRoot, "THIRD_PARTY_NOTICES.md"), "# Synthetic notices\n");
  const malicious = path.join(suiteRoot, ".eval-runtime", "cli.mjs");
  await writeFile(malicious, "throw new Error('suite code executed');\n");
  const result = await installRuntime({ suiteRoot, sourceRoot });
  assert.equal(result.root, sourceRoot);
  assert.equal(result.cli, path.join(sourceRoot, "email-evals-runtime.bundle.mjs"));
  assert.equal(await readFile(malicious, "utf8"), "throw new Error('suite code executed');\n");
});

test("launcher executes only plugin-trusted code with a minimal environment", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-final-launch-"));
  const suite = await writeSuite(root);
  await mkdir(path.join(root, ".eval-runtime"));
  await writeFile(path.join(root, ".eval-runtime", "cli.mjs"), "throw new Error('suite code executed');\n");
  const io = streams();
  let invocation;
  const plan = validationPlan();
  const exit = await launch(["validate", "--suite", suite, "--json"], {
    cwd: root,
    environment: {
      ...ENVIRONMENT,
      E2A_EVAL_PROBE_COPY: "copy@eval.test",
      AWS_SECRET_ACCESS_KEY: "do-not-forward",
      NODE_OPTIONS: "--import=/private/tmp/attacker.mjs",
      PATH: "/synthetic/bin",
    },
    stdout: io.stdout,
    stderr: io.stderr,
    spawnRuntime: async (args, options) => {
      invocation = { args, options };
      return {
        code: 0,
        stdout: `${JSON.stringify({ command: "validate", plan })}\n`,
        stderr: "",
        truncated: false,
        finalized: true,
        termination: "exit",
      };
    },
  });
  assert.equal(exit, 0);
  assert.match(invocation.args[0], /email-evals\/runtime\/email-evals-runtime\.bundle\.mjs$/);
  assert.doesNotMatch(invocation.args[0], new RegExp(root.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
  assert.deepEqual(invocation.options.env, {
    WS_NO_BUFFER_UTIL: "1",
    WS_NO_UTF_8_VALIDATE: "1",
    ...ENVIRONMENT,
    E2A_EVAL_PROBE_COPY: "copy@eval.test",
  });
  assert.equal(io.errors.join(""), "");
});

test("local setup and scaffold children receive no eval credentials", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-final-local-env-"));
  for (const command of ["setup", "scaffold"]) {
    const io = streams();
    let invocation;
    const exit = await launch([command, "--root", root], {
      cwd: root,
      environment: {
        ...ENVIRONMENT,
        E2A_EVAL_PROBE_COPY: "copy@eval.test",
        AWS_SECRET_ACCESS_KEY: "do-not-forward",
        PATH: "/synthetic/bin",
        TMPDIR: "/private/tmp",
      },
      stdout: io.stdout,
      stderr: io.stderr,
      spawnNode: async (args, options) => {
        invocation = { args, options };
        return 0;
      },
    });
    assert.equal(exit, 0);
    assert.match(invocation.args[0], new RegExp(`${command}\\.mjs$`));
    assert.deepEqual(invocation.options.env, { PATH: "/synthetic/bin", TMPDIR: "/private/tmp" });
    assert.equal(io.errors.join(""), "");
  }
});

test("public shell clears Node preload controls before its first Node invocation", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-final-shell-"));
  const marker = path.join(root, "preload-marker");
  const preload = path.join(root, "preload.mjs");
  await writeFile(preload, [
    'import { writeFileSync } from "node:fs";',
    'writeFileSync(process.env.E2A_ATTACK_MARKER, process.env.E2A_EVAL_API_KEY ?? "missing");',
    "",
  ].join("\n"));
  const shell = path.resolve(path.dirname(new URL(import.meta.url).pathname), "../../email-evals.sh");
  const result = await new Promise((resolve, reject) => {
    const child = spawn(shell, ["--help"], {
      env: {
        ...process.env,
        NODE_OPTIONS: `--import=${preload}`,
        NODE_PATH: root,
        E2A_ATTACK_MARKER: marker,
        E2A_EVAL_API_KEY: "e2a_acct_synthetic_collision",
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (chunk) => { stdout += chunk; });
    child.stderr.on("data", (chunk) => { stderr += chunk; });
    child.once("error", reject);
    child.once("exit", (code, signal) => resolve({ code, signal, stdout, stderr }));
  });
  assert.equal(result.code, 0);
  assert.equal(result.signal, null);
  assert.match(result.stdout, /email-evals validate/);
  assert.equal(result.stderr, "");
  await assert.rejects(readFile(marker), (error) => error.code === "ENOENT");
});

test("launcher validates exact command-specific success and impossible human failure shapes", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-final-output-"));
  const suite = await writeSuite(root);
  for (const forged of [
    "arbitrary success\n",
    `${JSON.stringify({ command: "run", summary: { status: "pass" } })}\n`,
  ]) {
    const io = streams();
    const exit = await launch(["run", "--suite", suite, "--approval-digest", "b".repeat(64), "--json"], {
      cwd: root,
      environment: ENVIRONMENT,
      stdout: io.stdout,
      stderr: io.stderr,
      spawnRuntime: async () => ({
        code: 0, stdout: forged, stderr: "", truncated: false, finalized: true, termination: "exit",
      }),
    });
    assert.equal(exit, 4);
    assert.equal(io.output.join(""), "");
    assert.equal(io.errors.join(""), "email-evals: runtime failure\n");
  }

  for (const impossible of [
    "Status: fail; 1/1 passed\nComplete: yes\nReport: run_20260809T120000_0123abcd/report.md\n",
    "Status: fail; 00/01 passed\nComplete: yes\nReport: run_20260809T120000_0123abcd/report.md\n",
  ]) {
    const io = streams();
    const exit = await launch(["run", "--suite", suite, "--approval-digest", "b".repeat(64)], {
      cwd: root,
      environment: ENVIRONMENT,
      stdout: io.stdout,
      stderr: io.stderr,
      spawnRuntime: async () => ({
        code: 3,
        stdout: impossible,
        stderr: "email-evals: transport_error\n",
        truncated: false,
        finalized: true,
        termination: "exit",
      }),
    });
    assert.equal(exit, 4);
    assert.equal(io.output.join(""), "");
  }
});

test("runtime child ignores stdin and is killed at its hard wall deadline", async () => {
  const child = new EventEmitter();
  child.stdout = new EventEmitter();
  child.stderr = new EventEmitter();
  child.stdout.destroy = () => {};
  child.stderr.destroy = () => {};
  let spawnOptions;
  let killed = 0;
  child.kill = () => { killed += 1; return true; };
  const timers = [];
  const running = runRuntimeNode([], {}, {
    spawnChild: (_node, _args, options) => { spawnOptions = options; return child; },
    setTimer: (callback) => { timers.push(callback); return callback; },
    clearTimer: () => {},
    wallTimeoutMs: 1,
  });
  assert.equal(spawnOptions.stdio[0], "ignore");
  timers[0]();
  assert.equal(killed, 1);
  assert.equal((await running).termination, "wall_timeout");
});
