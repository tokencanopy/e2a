import { lstat, realpath } from "node:fs/promises";
import path from "node:path";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { CliUsageError, parseRuntimeArguments, usage } from "./runtime/lib/cli-arguments.mjs";
import { isSafeResultCaseId } from "./runtime/lib/result-contract.mjs";

const launcherDirectory = path.dirname(fileURLToPath(import.meta.url));
const trustedRuntimeDirectory = path.join(launcherDirectory, "runtime");
const trustedRuntimeCli = path.join(trustedRuntimeDirectory, "email-evals-runtime.bundle.mjs");
const RUNTIME_STDOUT_LIMIT = 16 * 1024 * 1024;
const RUNTIME_STDERR_LIMIT = 8192;
const RUNTIME_STDERR_GRACE_MS = 100;
const RUNTIME_WALL_TIMEOUT_MS = 30 * 60 * 1000;
const MAX_SUITE_EXECUTION_MS = 25 * 60 * 1000;
const MAX_PLAN_STRING_BYTES = 64 * 1024;
const MAX_PLAN_TEXT_BYTES = 256 * 1024;
const MAX_PLAN_SUBJECT_BYTES = 998;
const RESULT_CLASSES = new Set([
  "assertion_failure", "configuration_error", "capability_error",
  "transport_error", "target_timeout", "grader_error",
]);
const RESULT_CAPABILITIES = new Set([
  "message_action", "visible_recipients", "blind_recipients", "envelope_recipients",
  "thread_headers", "raw_mime", "attachment_hashes", "delivery_lifecycle",
]);
const ACTION_KINDS = new Set(["none", "reply", "reply_all", "forward", "new_message"]);
const SAFE_RUNTIME_DIAGNOSTICS = new Map([
  ["email-evals: assertion_failure\n", 1],
  ["email-evals: configuration_error\n", 2],
  ["email-evals: capability_error\n", 2],
  ["email-evals: transport_error\n", 3],
  ["email-evals: target_timeout\n", 3],
  ["email-evals: grader_error\n", 4],
  ["email-evals: unexpected runner failure\n", 4],
]);

function unavailable(stderr) {
  stderr.write("email-evals: runtime unavailable\n");
  return 2;
}

function runtimeFailure(stderr) {
  stderr.write("email-evals: runtime failure\n");
  return 4;
}

function runNode(args, options) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, args, options);
    child.once("error", reject);
    child.once("exit", (code, signal) => resolve(code ?? (signal ? 4 : 4)));
  });
}

export function runRuntimeNode(args, options, dependencies = {}) {
  const {
    spawnChild = spawn,
    setTimer = setTimeout,
    clearTimer = clearTimeout,
    graceMs = RUNTIME_STDERR_GRACE_MS,
    wallTimeoutMs = RUNTIME_WALL_TIMEOUT_MS,
  } = dependencies;
  return new Promise((resolve) => {
    const stdoutCapture = { chunks: [], size: 0, finalized: false };
    const stderrCapture = { chunks: [], size: 0, finalized: false };
    let truncated = false;
    let settled = false;
    let exited = false;
    let exitCode = 4;
    let exitSignal = null;
    let graceTimer = null;
    let wallTimer = null;
    let child;

    function finish(result) {
      if (settled) return;
      settled = true;
      if (graceTimer !== null) clearTimer(graceTimer);
      if (wallTimer !== null) clearTimer(wallTimer);
      resolve(result);
    }

    function fixedFailure(termination = "failure") {
      finish({ code: 4, stdout: "", stderr: "", truncated: true, finalized: false, termination });
    }

    function finishFromExit() {
      if (!exited || !stdoutCapture.finalized || !stderrCapture.finalized) return;
      finish({
        code: exitSignal || !Number.isInteger(exitCode) ? 4 : exitCode,
        stdout: Buffer.concat(stdoutCapture.chunks).toString("utf8"),
        stderr: Buffer.concat(stderrCapture.chunks).toString("utf8"),
        truncated,
        finalized: true,
        termination: exitSignal ? "signal" : "exit",
      });
    }

    function captureStream(stream, capture, limit) {
      stream.on("data", (chunk) => {
        if (settled) return;
        const data = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
        if (capture.size >= limit) {
          truncated = true;
          return;
        }
        const remaining = limit - capture.size;
        capture.chunks.push(data.length > remaining ? data.subarray(0, remaining) : data);
        capture.size += Math.min(data.length, remaining);
        if (data.length > remaining) truncated = true;
      });
      const finalize = () => {
        if (capture.finalized || settled) return;
        capture.finalized = true;
        finishFromExit();
      };
      stream.once("end", finalize);
      stream.once("close", finalize);
      stream.once("error", () => fixedFailure("stream_error"));
    }

    try {
      child = spawnChild(process.execPath, args, { ...options, stdio: ["ignore", "pipe", "pipe"] });
    } catch {
      fixedFailure("spawn_error");
      return;
    }
    if (!child?.stdout || typeof child.stdout.on !== "function"
      || !child?.stderr || typeof child.stderr.on !== "function" || typeof child.once !== "function") {
      fixedFailure("spawn_error");
      return;
    }
    captureStream(child.stdout, stdoutCapture, RUNTIME_STDOUT_LIMIT);
    captureStream(child.stderr, stderrCapture, RUNTIME_STDERR_LIMIT);
    wallTimer = setTimer(() => {
      if (settled) return;
      try { child.kill?.("SIGKILL"); } catch {}
      try { child.stdout.destroy?.(); } catch {}
      try { child.stderr.destroy?.(); } catch {}
      fixedFailure("wall_timeout");
    }, wallTimeoutMs);
    child.once("error", () => fixedFailure("spawn_error"));
    child.once("exit", (code, signal) => {
      if (settled) return;
      exited = true;
      exitCode = code;
      exitSignal = signal;
      if (stdoutCapture.finalized && stderrCapture.finalized) {
        finishFromExit();
        return;
      }
      graceTimer = setTimer(() => {
        if ((stdoutCapture.finalized && stderrCapture.finalized) || settled) return;
        truncated = true;
        try { child.stdout.destroy?.(); } catch {}
        try { child.stderr.destroy?.(); } catch {}
        try { child.unref?.(); } catch {}
        fixedFailure("stream_timeout");
      }, graceMs);
    });
  });
}

function plainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    && Object.getPrototypeOf(value) === Object.prototype;
}

function exactKeys(value, keys) {
  if (!plainObject(value)) return false;
  const actual = Object.keys(value);
  return actual.length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}

function nonnegativeInteger(value) {
  return Number.isSafeInteger(value) && value >= 0;
}

function positiveInteger(value, maximum = Number.MAX_SAFE_INTEGER) {
  return Number.isSafeInteger(value) && value > 0 && value <= maximum;
}

function safeAlias(value) {
  return typeof value === "string" && /^(?:actor|target|probe:(?:[1-9]|[1-8][0-9]|9[0-8]))$/.test(value);
}

function safeUniqueArray(value, predicate) {
  return Array.isArray(value) && value.every(predicate) && new Set(value).size === value.length;
}

function safeCapabilities(value) {
  return safeUniqueArray(value, (entry) => RESULT_CAPABILITIES.has(entry));
}

function safePlanString(value, maximumBytes = MAX_PLAN_STRING_BYTES) {
  return typeof value === "string" && Buffer.byteLength(value, "utf8") <= maximumBytes
    && !/[\u0000-\u001f\u007f@]/.test(value)
    && !/\b(?:sk|e2a)_[A-Za-z0-9_-]+\b/.test(value);
}

function safePlanSentAs(value, apiKey) {
  return value === "[REDACTED:credential]"
    || (typeof value === "string" && /^[a-z][a-z0-9_]{0,63}$/.test(value)
      && !(typeof apiKey === "string" && apiKey.length > 0 && value.includes(apiKey)));
}

function safePlanValue(value, depth = 0, budget = { nodes: 0 }) {
  budget.nodes += 1;
  if (budget.nodes > 2_048 || depth > 12) return false;
  if (value === null || typeof value === "boolean") return true;
  if (typeof value === "number") return Number.isFinite(value);
  if (typeof value === "string") return safePlanString(value);
  if (Array.isArray(value)) return value.length <= 256
    && value.every((entry) => safePlanValue(entry, depth + 1, budget));
  if (!plainObject(value) || Object.keys(value).length > 64) return false;
  return Object.entries(value).every(([key, entry]) => /^[A-Za-z][A-Za-z0-9_]{0,63}$/.test(key)
    && safePlanValue(entry, depth + 1, budget));
}

function nullableAliasArray(value) {
  return value === null || safeUniqueArray(value, safeAlias);
}

function safeBaseUrl(value) {
  if (typeof value !== "string" || value.length > 2048 || /[\u0000-\u001f\u007f]/.test(value)) return false;
  try {
    const parsed = new URL(value);
    const loopback = new Set(["localhost", "127.0.0.1", "[::1]"]).has(parsed.hostname);
    return (parsed.protocol === "https:" || (parsed.protocol === "http:" && loopback))
      && parsed.username === "" && parsed.password === "" && parsed.search === "" && parsed.hash === ""
      && (parsed.pathname === "" || parsed.pathname === "/") && parsed.origin === value.replace(/\/$/, "");
  } catch {
    return false;
  }
}

function validationOutput(stdout, apiKey) {
  if (!stdout.endsWith("\n") || stdout.slice(0, -1).includes("\n")) return null;
  let output;
  try { output = JSON.parse(stdout.slice(0, -1)); } catch { return null; }
  if (JSON.stringify(output) + "\n" !== stdout || !exactKeys(output, ["command", "plan"])
    || output.command !== "validate") return null;
  const plan = output.plan;
  if (!exactKeys(plan, ["baseUrl", "networkSends", "capabilities", "recipientAliases", "protectionDigest", "timeouts", "executionBudget", "cases", "approvalDigest"])
    || !safeBaseUrl(plan.baseUrl) || plan.networkSends !== false
    || !safeCapabilities(plan.capabilities) || !safeUniqueArray(plan.recipientAliases, safeAlias)
    || plan.recipientAliases.length < 2 || plan.recipientAliases.length > 100
    || !plan.recipientAliases.includes("actor") || !plan.recipientAliases.includes("target")
    || typeof plan.protectionDigest !== "string" || !/^[a-f0-9]{64}$/.test(plan.protectionDigest)
    || typeof plan.approvalDigest !== "string" || !/^[a-f0-9]{64}$/.test(plan.approvalDigest)
    || !exactKeys(plan.timeouts, ["maxRetries", "maxElapsedMs", "timeoutMs"])
    || !nonnegativeInteger(plan.timeouts.maxRetries)
    || !positiveInteger(plan.timeouts.maxElapsedMs, 10 * 60 * 1000)
    || !positiveInteger(plan.timeouts.timeoutMs, 10 * 60 * 1000)
    || !exactKeys(plan.executionBudget, ["plannedTimeoutMs", "maximumTimeoutMs"])
    || plan.executionBudget.maximumTimeoutMs !== MAX_SUITE_EXECUTION_MS
    || !positiveInteger(plan.executionBudget.plannedTimeoutMs, MAX_SUITE_EXECUTION_MS)
    || !Array.isArray(plan.cases) || plan.cases.length < 1 || plan.cases.length > 100) return null;
  const caseIds = new Set();
  let plannedTimeoutMs = 0;
  for (const testCase of plan.cases) {
    if (!exactKeys(testCase, [
      "id", "stimulus", "expectedAction", "expectedSender", "expectedRecipients", "recipientAliases",
      "assertions", "evidenceCapabilities", "semanticGraders", "timeoutMs", "settleMs", "pollIntervalMs",
    ])
      || !isSafeResultCaseId(testCase.id) || caseIds.has(testCase.id)
      || !exactKeys(testCase.stimulus, ["action", "sender", "recipients", "subject", "text"])
      || testCase.stimulus.action !== "send" || testCase.stimulus.sender !== "actor"
      || JSON.stringify(testCase.stimulus.recipients) !== '["target"]'
      || !safePlanString(testCase.stimulus.subject, MAX_PLAN_SUBJECT_BYTES)
      || !safePlanString(testCase.stimulus.text, MAX_PLAN_TEXT_BYTES)
      || !exactKeys(testCase.expectedAction, ["kind", "count"])
      || !ACTION_KINDS.has(testCase.expectedAction.kind) || !nonnegativeInteger(testCase.expectedAction.count)
      || testCase.expectedAction.count > 100
      || (testCase.expectedAction.kind === "none") !== (testCase.expectedAction.count === 0)
      || !exactKeys(testCase.expectedSender, ["from", "sentAs", "replyTo", "displayName"])
      || !(testCase.expectedSender.from === null || safeAlias(testCase.expectedSender.from))
      || !(testCase.expectedSender.sentAs === null || safePlanSentAs(testCase.expectedSender.sentAs, apiKey))
      || !nullableAliasArray(testCase.expectedSender.replyTo)
      || !(testCase.expectedSender.displayName === null || safePlanString(testCase.expectedSender.displayName))
      || !exactKeys(testCase.expectedRecipients, ["to", "cc", "bcc", "envelope"])
      || !["to", "cc", "bcc", "envelope"].every((field) => nullableAliasArray(testCase.expectedRecipients[field]))
      || !safeUniqueArray(testCase.recipientAliases, safeAlias)
      || testCase.recipientAliases.length > 100
      || !testCase.recipientAliases.every((alias) => plan.recipientAliases.includes(alias))
      || !Array.isArray(testCase.assertions) || testCase.assertions.length < 3 || testCase.assertions.length > 128
      || !safeCapabilities(testCase.evidenceCapabilities)
      || !Array.isArray(testCase.semanticGraders) || testCase.semanticGraders.length !== 0
      || !positiveInteger(testCase.timeoutMs, MAX_SUITE_EXECUTION_MS)
      || !nonnegativeInteger(testCase.settleMs) || testCase.settleMs > testCase.timeoutMs
      || !positiveInteger(testCase.pollIntervalMs, testCase.timeoutMs)) return null;
    const assertionIds = new Set();
    for (const assertion of testCase.assertions) {
      if (!exactKeys(assertion, ["id", "expected"])
        || typeof assertion.id !== "string" || !/^[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)+$/.test(assertion.id)
        || assertionIds.has(assertion.id)
        || (assertion.id === "sender.sent_as"
          ? !safePlanSentAs(assertion.expected, apiKey) : !safePlanValue(assertion.expected))) return null;
      assertionIds.add(assertion.id);
    }
    const sentAsAssertions = testCase.assertions.filter((assertion) => assertion.id === "sender.sent_as");
    if (testCase.expectedSender.sentAs === null ? sentAsAssertions.length !== 0
      : sentAsAssertions.length !== 1
        || sentAsAssertions[0].expected !== testCase.expectedSender.sentAs) return null;
    plannedTimeoutMs += testCase.timeoutMs;
    caseIds.add(testCase.id);
  }
  if (plannedTimeoutMs !== plan.executionBudget.plannedTimeoutMs) return null;
  return output;
}

function summaryExit(summary) {
  if (summary.status === "pass") return 0;
  const classes = summary.cases.filter((entry) => entry.status !== "pass").map((entry) => entry.errorClass);
  if (classes.includes("grader_error")) return 4;
  if (classes.includes("transport_error") || classes.includes("target_timeout")) return 3;
  if (classes.includes("configuration_error") || classes.includes("capability_error")) return 2;
  if (classes.includes("assertion_failure")) return 1;
  return null;
}

function completedOutput(stdout, command) {
  if ((command !== "run" && command !== "regrade") || !stdout.endsWith("\n")
    || stdout.slice(0, -1).includes("\n")) return null;
  let output;
  try { output = JSON.parse(stdout.slice(0, -1)); } catch { return null; }
  if (JSON.stringify(output) + "\n" !== stdout || !exactKeys(output, ["command", "summary", "report"])
    || output.command !== command) return null;
  const summary = output.summary;
  if (!exactKeys(summary, ["runId", "status", "complete", "counts", "capabilities", "cases"])
    || !/^run_\d{8}T\d{6}_[a-f0-9]{8}$/.test(summary.runId)
    || !["pass", "fail"].includes(summary.status) || summary.complete !== true
    || output.report !== `${summary.runId}/report.md`
    || !exactKeys(summary.counts, ["total", "passed", "failed", "errors"])
    || !Object.values(summary.counts).every(nonnegativeInteger)
    || !positiveInteger(summary.counts.total, 100)
    || summary.counts.total !== summary.counts.passed + summary.counts.failed + summary.counts.errors
    || !safeCapabilities(summary.capabilities)
    || !Array.isArray(summary.cases) || summary.cases.length !== summary.counts.total) return null;
  const observed = { passed: 0, failed: 0, errors: 0 };
  const caseIds = new Set();
  for (const result of summary.cases) {
    const keys = result?.status === "pass" ? ["id", "status"] : ["id", "status", "errorClass"];
    if (!exactKeys(result, keys) || !isSafeResultCaseId(result.id) || caseIds.has(result.id)
      || !["pass", "fail", "error"].includes(result.status)) return null;
    caseIds.add(result.id);
    if (result.status === "pass") observed.passed += 1;
    else if (!RESULT_CLASSES.has(result.errorClass)
      || (result.errorClass === "assertion_failure") !== (result.status === "fail")) return null;
    else observed[result.status === "fail" ? "failed" : "errors"] += 1;
  }
  if (summary.counts.passed !== observed.passed || summary.counts.failed !== observed.failed
    || summary.counts.errors !== observed.errors
    || (summary.status === "pass") !== (observed.failed === 0 && observed.errors === 0)) return null;
  const exit = summaryExit(summary);
  return exit === null ? null : { output, exit };
}

function renderValidation(output) {
  return `Validation passed: ${output.plan.cases.length} case(s); network sends: no\nPlan:\n${JSON.stringify(output.plan, null, 2)}\n`;
}

function renderCompleted(output) {
  return `Status: ${output.summary.status}; ${output.summary.counts.passed}/${output.summary.counts.total} passed\nComplete: yes\nReport: ${output.report}\n`;
}

function safeRuntimeExit(result, parsed, stdout, stderr, apiKey) {
  if (result.termination !== "exit" || result.finalized !== true || result.truncated
    || typeof result.stdout !== "string" || typeof result.stderr !== "string") return runtimeFailure(stderr);
  const diagnosticExit = SAFE_RUNTIME_DIAGNOSTICS.get(result.stderr);
  if (result.stdout === "" && diagnosticExit !== undefined && result.code === diagnosticExit) {
    stderr.write(result.stderr);
    return result.code;
  }
  if (result.stderr !== "") return runtimeFailure(stderr);
  if (parsed.command === "validate") {
    const output = validationOutput(result.stdout, apiKey);
    if (result.code !== 0 || output === null) return runtimeFailure(stderr);
    stdout.write(parsed.json ? `${JSON.stringify(output)}\n` : renderValidation(output));
    return 0;
  }
  const completed = completedOutput(result.stdout, parsed.command);
  if (completed === null || result.code !== completed.exit) return runtimeFailure(stderr);
  stdout.write(parsed.json ? `${JSON.stringify(completed.output)}\n` : renderCompleted(completed.output));
  if (!parsed.json && completed.exit !== 0) {
    const errorClass = completed.output.summary.cases.find((entry) => entry.status !== "pass")?.errorClass;
    if (typeof errorClass !== "string") return runtimeFailure(stderr);
    stderr.write(`email-evals: ${errorClass}\n`);
  }
  return completed.exit;
}

async function regularNoLink(file) {
  const state = await lstat(file);
  if (state.isSymbolicLink() || !state.isFile()) throw new Error("unsafe runtime path");
  return state;
}

async function directoryNoLink(directory) {
  const state = await lstat(directory);
  if (state.isSymbolicLink() || !state.isDirectory()) throw new Error("unsafe runtime path");
  return realpath(directory);
}

function sameIdentity(left, right) {
  return left.dev === right.dev && left.ino === right.ino && left.isFile() && right.isFile();
}

function minimalEnvironment(environment) {
  // The bundled WebSocket dependency has optional native accelerators. Keep
  // resolution entirely inside the authenticated single-file bundle instead
  // of allowing Node to walk ancestor node_modules directories.
  const safe = { WS_NO_BUFFER_UTIL: "1", WS_NO_UTF_8_VALIDATE: "1" };
  for (const [name, value] of Object.entries(environment ?? {})) {
    if ((name === "E2A_EVAL_API_KEY" || name === "E2A_EVAL_ACTOR" || name === "E2A_EVAL_TARGET"
      || /^E2A_EVAL_PROBE_[A-Z0-9_]{1,64}$/.test(name)) && typeof value === "string") safe[name] = value;
  }
  return safe;
}

function localToolEnvironment(environment) {
  const safe = {};
  if (typeof environment?.PATH === "string") safe.PATH = environment.PATH;
  if (typeof environment?.TMPDIR === "string") safe.TMPDIR = environment.TMPDIR;
  return safe;
}

async function launchLocal(command, args, dependencies) {
  const script = command === "scaffold" ? "scaffold.mjs" : "setup.mjs";
  try {
    return await dependencies.spawnNode([path.join(launcherDirectory, script), ...args], {
      cwd: dependencies.cwd,
      env: localToolEnvironment(dependencies.environment),
      stdio: "inherit",
    });
  } catch {
    dependencies.stderr.write("email-evals: launcher failure\n");
    return 4;
  }
}

async function launchRuntime(argv, parsed, dependencies) {
  try {
    const requestedSuite = path.resolve(dependencies.cwd, parsed.suite);
    const parent = await directoryNoLink(path.dirname(requestedSuite));
    const suite = path.join(parent, path.basename(requestedSuite));
    const suiteState = await regularNoLink(suite);
    const resolvedSuite = await realpath(suite);
    if (resolvedSuite !== suite || !sameIdentity(suiteState, await regularNoLink(resolvedSuite))) throw new Error("unsafe suite path");

    const runtime = await directoryNoLink(trustedRuntimeDirectory);
    if (runtime !== trustedRuntimeDirectory) throw new Error("unsafe runtime path");
    const cliState = await regularNoLink(trustedRuntimeCli);
    if (await realpath(trustedRuntimeCli) !== trustedRuntimeCli) throw new Error("unsafe runtime path");
    await dependencies.beforeExec?.();
    if (!sameIdentity(cliState, await regularNoLink(trustedRuntimeCli))) throw new Error("runtime changed");

    const childArgv = parsed.json ? argv : [...argv, "--json"];
    const runtimeEnvironment = minimalEnvironment(dependencies.environment);
    const result = await dependencies.spawnRuntime([trustedRuntimeCli, ...childArgv], {
      cwd: dependencies.cwd,
      env: runtimeEnvironment,
    });
    return safeRuntimeExit(
      result, parsed, dependencies.stdout, dependencies.stderr, runtimeEnvironment.E2A_EVAL_API_KEY,
    );
  } catch {
    return unavailable(dependencies.stderr);
  }
}

export async function main(argv, supplied = {}) {
  const dependencies = {
    cwd: process.cwd(), environment: process.env, stdout: process.stdout, stderr: process.stderr,
    spawnNode: runNode, spawnRuntime: null, ...supplied,
  };
  if (dependencies.spawnRuntime === null) {
    dependencies.spawnRuntime = (args, options) => runRuntimeNode(args, options, dependencies);
  }
  if (argv[0] === "scaffold" || argv[0] === "setup") return launchLocal(argv[0], argv.slice(1), dependencies);
  let parsed;
  try {
    parsed = parseRuntimeArguments(argv);
  } catch (error) {
    if (error instanceof CliUsageError) {
      dependencies.stderr.write(`${error.message}\n`);
      return 2;
    }
    dependencies.stderr.write("email-evals: launcher failure\n");
    return 4;
  }
  if (parsed.help) {
    dependencies.stdout.write(`${usage()}\n`);
    return 0;
  }
  return launchRuntime(argv, parsed, dependencies);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2)).then((code) => { process.exitCode = code; });
}
