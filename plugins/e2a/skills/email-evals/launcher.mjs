import { constants as fsConstants } from "node:fs";
import { lstat, open, realpath } from "node:fs/promises";
import path from "node:path";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { CliUsageError, parseRuntimeArguments, usage } from "./runtime/lib/cli-arguments.mjs";

const launcherDirectory = path.dirname(fileURLToPath(import.meta.url));
// Match the runtime's maximum retained cases artifact while keeping child output bounded.
const RUNTIME_STDOUT_LIMIT = 16 * 1024 * 1024;
const RUNTIME_STDERR_LIMIT = 8192;
const RUNTIME_STDERR_GRACE_MS = 100;
const RESULT_CLASSES = new Set([
  "assertion_failure", "configuration_error", "capability_error",
  "transport_error", "target_timeout", "grader_error",
]);
const RESULT_CAPABILITIES = new Set([
  "message_action", "visible_recipients", "blind_recipients", "envelope_recipients",
  "thread_headers", "raw_mime", "attachment_hashes", "delivery_lifecycle",
]);
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
  } = dependencies;
  return new Promise((resolve) => {
    const stdoutCapture = { chunks: [], size: 0, truncated: false, finalized: false };
    const stderrCapture = { chunks: [], size: 0, truncated: false, finalized: false };
    let truncated = false;
    let settled = false;
    let exited = false;
    let exitCode = 4;
    let exitSignal = null;
    let graceTimer = null;
    let timedOut = false;
    let child;

    function finish(result) {
      if (settled) return;
      settled = true;
      if (graceTimer !== null) clearTimer(graceTimer);
      resolve(result);
    }

    function fixedFailure() {
      finish({ code: 4, stdout: "", stderr: "", truncated: true, finalized: false });
    }

    function finishFromExit() {
      if (!exited || !stdoutCapture.finalized || !stderrCapture.finalized) return;
      finish({
        code: exitSignal || !Number.isInteger(exitCode) ? 4 : exitCode,
        stdout: Buffer.concat(stdoutCapture.chunks).toString("utf8"),
        stderr: Buffer.concat(stderrCapture.chunks).toString("utf8"),
        truncated,
        finalized: true,
      });
    }

    function captureStream(stream, capture, limit) {
      stream.on("data", (chunk) => {
        if (settled) return;
        const data = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
        if (capture.size >= limit) {
          capture.truncated = true;
          truncated = true;
          return;
        }
        const remaining = limit - capture.size;
        const retained = data.length > remaining ? data.subarray(0, remaining) : data;
        capture.chunks.push(retained);
        capture.size += retained.length;
        if (data.length > remaining) {
          capture.truncated = true;
          truncated = true;
        }
      });
      const finalize = () => {
        if (timedOut || capture.finalized || settled) return;
        capture.finalized = true;
        finishFromExit();
      };
      stream.once("end", finalize);
      stream.once("close", finalize);
    }

    try {
      child = spawnChild(process.execPath, args, { ...options, stdio: ["inherit", "pipe", "pipe"] });
    } catch {
      fixedFailure();
      return;
    }
    if (!child?.stdout || typeof child.stdout.on !== "function"
      || !child?.stderr || typeof child.stderr.on !== "function" || typeof child.once !== "function") {
      fixedFailure();
      return;
    }
    captureStream(child.stdout, stdoutCapture, RUNTIME_STDOUT_LIMIT);
    captureStream(child.stderr, stderrCapture, RUNTIME_STDERR_LIMIT);
    child.once("error", fixedFailure);
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
        timedOut = true;
        truncated = true;
        try { child.stdout.destroy?.(); } catch {}
        try { child.stderr.destroy?.(); } catch {}
        try { child.unref?.(); } catch {}
        fixedFailure();
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

function completedJSONExit(stdout, command) {
  if ((command !== "run" && command !== "regrade") || !stdout.endsWith("\n") || stdout.slice(0, -1).includes("\n")) return null;
  let output;
  try {
    output = JSON.parse(stdout.slice(0, -1));
  } catch {
    return null;
  }
  if (JSON.stringify(output) + "\n" !== stdout || !exactKeys(output, ["command", "summary"]) || output.command !== command) return null;
  const summary = output.summary;
  if (!exactKeys(summary, ["runId", "status", "complete", "counts", "capabilities", "cases"])
    || !/^run_\d{8}T\d{6}_[a-f0-9]{8}$/.test(summary.runId)
    || summary.status !== "fail" || summary.complete !== true
    || !exactKeys(summary.counts, ["total", "passed", "failed", "errors"])
    || !Object.values(summary.counts).every(nonnegativeInteger)
    || !Array.isArray(summary.capabilities)
    || summary.capabilities.some((entry) => !RESULT_CAPABILITIES.has(entry))
    || new Set(summary.capabilities).size !== summary.capabilities.length
    || !Array.isArray(summary.cases) || summary.cases.length !== summary.counts.total) return null;

  const observedCounts = { passed: 0, failed: 0, errors: 0 };
  const classes = [];
  const caseIds = new Set();
  for (const result of summary.cases) {
    const keys = result?.errorClass === undefined ? ["id", "status"] : ["id", "status", "errorClass"];
    if (!exactKeys(result, keys) || typeof result.id !== "string" || result.id.length < 1 || result.id.length > 128
      || !/^[a-z0-9]+(?:[-_][a-z0-9]+)*$/.test(result.id)
      || !["pass", "fail", "error"].includes(result.status)) return null;
    if (caseIds.has(result.id)) return null;
    caseIds.add(result.id);
    if (result.status === "pass") {
      if (result.errorClass !== undefined) return null;
      observedCounts.passed++;
      continue;
    }
    if (!RESULT_CLASSES.has(result.errorClass)
      || (result.errorClass === "assertion_failure") !== (result.status === "fail")) return null;
    observedCounts[result.status === "fail" ? "failed" : "errors"]++;
    classes.push(result.errorClass);
  }
  if (summary.counts.passed !== observedCounts.passed || summary.counts.failed !== observedCounts.failed
    || summary.counts.errors !== observedCounts.errors
    || summary.counts.total !== summary.counts.passed + summary.counts.failed + summary.counts.errors) return null;
  if (classes.includes("grader_error")) return 4;
  if (classes.includes("transport_error") || classes.includes("target_timeout")) return 3;
  if (classes.includes("configuration_error") || classes.includes("capability_error")) return 2;
  if (classes.includes("assertion_failure")) return 1;
  return null;
}

function completedHumanOutput(stdout, command) {
  if (command !== "run" && command !== "regrade") return false;
  const match = /^Status: fail; (\d+)\/(\d+) passed\nReport: (run_\d{8}T\d{6}_[a-f0-9]{8})\/report\.md\n$/.exec(stdout);
  if (!match) return false;
  const passed = Number(match[1]);
  const total = Number(match[2]);
  return Number.isSafeInteger(passed) && Number.isSafeInteger(total) && passed >= 0 && passed <= total;
}

function safeRuntimeExit(result, parsed, stdout, stderr) {
  if (result.finalized !== true || result.truncated || typeof result.stdout !== "string" || typeof result.stderr !== "string") {
    stderr.write("email-evals: runtime failure\n");
    return 4;
  }
  const completedExit = parsed.json ? completedJSONExit(result.stdout, parsed.command) : null;
  const completedHuman = !parsed.json && completedHumanOutput(result.stdout, parsed.command);
  const diagnosticExit = SAFE_RUNTIME_DIAGNOSTICS.get(result.stderr);
  if (result.code === 0 && result.stderr === "") {
    stdout.write(result.stdout);
    return 0;
  }
  if (result.code === completedExit && (result.stderr === "" || diagnosticExit === completedExit)) {
    stdout.write(result.stdout);
    if (result.stderr !== "") stderr.write(result.stderr);
    return result.code;
  }
  if (completedHuman && diagnosticExit !== undefined && result.code === diagnosticExit) {
    stdout.write(result.stdout);
    stderr.write(result.stderr);
    return result.code;
  }
  if (result.stdout === "" && diagnosticExit !== undefined && result.code === diagnosticExit) {
    stderr.write(result.stderr);
    return result.code;
  }
  stderr.write("email-evals: runtime failure\n");
  return 4;
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

async function launchLocal(command, args, dependencies) {
  const script = command === "scaffold" ? "scaffold.mjs" : "setup.mjs";
  try {
    return await dependencies.spawnNode([path.join(launcherDirectory, script), ...args], {
      cwd: dependencies.cwd,
      env: dependencies.environment,
      stdio: "inherit",
    });
  } catch {
    dependencies.stderr.write("email-evals: launcher failure\n");
    return 4;
  }
}

async function launchRuntime(argv, parsed, dependencies) {
  let handle;
  try {
    const requestedSuite = path.resolve(dependencies.cwd, parsed.suite);
    const requestedParent = path.dirname(requestedSuite);
    const suiteName = path.basename(requestedSuite);
    const suiteParent = await directoryNoLink(requestedParent);
    const suite = path.join(suiteParent, suiteName);
    const suiteState = await regularNoLink(suite);
    const resolvedSuite = await realpath(suite);
    if (path.dirname(resolvedSuite) !== suiteParent || path.basename(resolvedSuite) !== suiteName) throw new Error("suite escaped root");

    const runtime = path.join(suiteParent, ".eval-runtime");
    const resolvedRuntime = await directoryNoLink(runtime);
    if (path.dirname(resolvedRuntime) !== suiteParent || path.basename(resolvedRuntime) !== ".eval-runtime") throw new Error("runtime escaped root");
    const cli = path.join(resolvedRuntime, "cli.mjs");
    const beforeOpen = await regularNoLink(cli);
    const resolvedCli = await realpath(cli);
    if (path.dirname(resolvedCli) !== resolvedRuntime || path.basename(resolvedCli) !== "cli.mjs") throw new Error("cli escaped runtime");
    handle = await open(resolvedCli, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW);
    if (!sameIdentity(beforeOpen, await handle.stat())) throw new Error("cli changed while opening");
    await dependencies.beforeExec?.();
    const immediatelyBeforeExec = await regularNoLink(resolvedCli);
    if (!sameIdentity(beforeOpen, immediatelyBeforeExec) || await realpath(resolvedCli) !== resolvedCli) throw new Error("cli changed before execution");
    try {
      const result = await dependencies.spawnRuntime([resolvedCli, ...argv], {
        cwd: dependencies.cwd,
        env: dependencies.environment,
      });
      return safeRuntimeExit(result, parsed, dependencies.stdout, dependencies.stderr);
    } catch {
      return runtimeFailure(dependencies.stderr);
    }
  } catch {
    return unavailable(dependencies.stderr);
  } finally {
    await handle?.close().catch(() => {});
  }
}

function runtimeFailure(stderr) {
  stderr.write("email-evals: runtime failure\n");
  return 4;
}

export async function main(argv, supplied = {}) {
  const dependencies = {
    cwd: process.cwd(),
    environment: process.env,
    stdout: process.stdout,
    stderr: process.stderr,
    spawnNode: runNode,
    spawnRuntime: null,
    ...supplied,
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
