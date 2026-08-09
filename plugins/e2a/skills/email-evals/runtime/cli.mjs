import path from "node:path";
import { lstat, realpath } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { loadSuite } from "./lib/contract.mjs";
import { createE2AAdapter } from "./lib/e2a-adapter.mjs";
import { CliUsageError, parseRuntimeArguments, usage } from "./lib/cli-arguments.mjs";
import { EvalError } from "./lib/errors.mjs";
import { regradeRun, runSuite } from "./lib/runner.mjs";


const ERROR_EXIT_CODES = Object.freeze({
  assertion_failure: 1,
  configuration_error: 2,
  capability_error: 2,
  transport_error: 3,
  target_timeout: 3,
  grader_error: 4,
});
const CAPABILITY_NAMES = new Set([
  "message_action", "visible_recipients", "blind_recipients", "envelope_recipients",
  "thread_headers", "raw_mime", "attachment_hashes", "delivery_lifecycle",
]);

// This intentionally mirrors runner.mjs's private requiredCapabilities helper.
// Task 9 cannot alter runner.mjs, while validate must fail before sending if an
// adapter cannot prove an assertion family.
export function requiredCapabilitiesForSuite(suite) {
  const required = new Set();
  for (const testCase of suite.cases) {
    const expectation = testCase.expect ?? {};
    if (expectation.action) required.add("message_action");
    if (expectation.sender || expectation.recipients?.to || expectation.recipients?.cc) required.add("visible_recipients");
    if (expectation.recipients?.bcc) required.add("blind_recipients");
    if (expectation.recipients?.envelope) required.add("envelope_recipients");
    if (expectation.thread) required.add("thread_headers");
    if (expectation.subject || expectation.body) required.add("raw_mime");
    if (expectation.attachments) required.add("attachment_hashes");
    if (expectation.lifecycle) required.add("delivery_lifecycle");
  }
  return [...required].sort();
}

function capabilitiesArray(value) {
  let result;
  try {
    if (Array.isArray(value)) result = [...new Set(value)];
    else if (value && typeof value[Symbol.iterator] === "function") result = [...new Set([...value])];
    else result = [];
  } catch {
    throw new EvalError("capability_error", "invalid_capability_set", "Evaluation adapter returned an invalid capability set");
  }
  if (result.some((entry) => typeof entry !== "string" || !/^[a-z][a-z0-9_]{0,127}$/.test(entry))) {
    throw new EvalError("capability_error", "invalid_capability_set", "Evaluation adapter returned an invalid capability set");
  }
  return result.sort();
}

function checkCapabilities(suite, preflight, adapter) {
  const capabilities = capabilitiesArray(preflight?.capabilities ?? adapter.capabilities);
  const missing = requiredCapabilitiesForSuite(suite).filter((name) => !capabilities.includes(name));
  if (missing.length > 0) {
    throw new EvalError("capability_error", "missing_capability", "Evaluation adapter cannot prove every requested assertion");
  }
  return capabilities;
}

function normalizePreflightError(error) {
  if (error instanceof EvalError && ERROR_EXIT_CODES[error.errorClass] !== undefined) return error;
  return new EvalError("grader_error", "grader_threw", "Evaluation preflight did not complete safely");
}

async function preflightSuite(suite, adapter) {
  let preflight;
  try {
    preflight = await adapter.preflight(suite);
  } catch (error) {
    throw normalizePreflightError(error);
  }
  return { preflight, capabilities: checkCapabilities(suite, preflight, adapter) };
}

function cachePreflight(adapter, preflight) {
  const cached = Object.create(adapter);
  Object.defineProperty(cached, "preflight", {
    value: async () => preflight,
    enumerable: true,
  });
  return cached;
}

function aliases(value) {
  if (!Array.isArray(value)) return [];
  return value.filter((entry) => typeof entry === "string" && /^(?:actor|target|probe:\d+)$/.test(entry));
}

function safePlan(preflight, suite, capabilities) {
  const plan = preflight?.plan ?? {};
  const cases = Array.isArray(plan.cases) ? plan.cases : [];
  return {
    networkSends: false,
    capabilities: capabilities.filter((name) => CAPABILITY_NAMES.has(name)),
    recipientAliases: aliases(plan.recipientAliases),
    cases: suite.cases.map((testCase, index) => ({
      id: testCase.id,
      expectedAction: testCase.expect?.action?.kind ?? "none",
      recipientAliases: aliases(cases[index]?.recipientAliases),
    })),
  };
}

function safeSummary(summary) {
  const cases = Array.isArray(summary?.cases) ? summary.cases.map((record) => ({
    id: typeof record?.id === "string" ? record.id : "unknown",
    status: ["pass", "fail", "error"].includes(record?.status) ? record.status : "error",
    ...(typeof record?.primaryError?.class === "string" && ERROR_EXIT_CODES[record.primaryError.class] !== undefined
      ? { errorClass: record.primaryError.class } : {}),
  })) : [];
  const counts = summary?.counts ?? {};
  return {
    runId: typeof summary?.runId === "string" && /^run_\d{8}T\d{6}_[a-f0-9]{8}$/.test(summary.runId) ? summary.runId : "unknown",
    status: summary?.status === "pass" ? "pass" : "fail",
    complete: summary?.complete === true,
    counts: {
      total: Number.isSafeInteger(counts.total) && counts.total >= 0 ? counts.total : 0,
      passed: Number.isSafeInteger(counts.passed) && counts.passed >= 0 ? counts.passed : 0,
      failed: Number.isSafeInteger(counts.failed) && counts.failed >= 0 ? counts.failed : 0,
      errors: Number.isSafeInteger(counts.errors) && counts.errors >= 0 ? counts.errors : 0,
    },
    capabilities: capabilitiesArray(summary?.capabilities).filter((name) => CAPABILITY_NAMES.has(name)),
    cases,
  };
}

function exitForSummary(summary) {
  if (summary?.status === "pass") return 0;
  const classes = [summary?.primaryError?.class, ...(summary?.cases ?? []).map((record) => record?.primaryError?.class)];
  if (classes.includes("grader_error")) return 4;
  if (classes.includes("transport_error") || classes.includes("target_timeout")) return 3;
  if (classes.includes("configuration_error") || classes.includes("capability_error")) return 2;
  if (classes.includes("assertion_failure")) return 1;
  return 4;
}

const RUN_ID = /^run_\d{8}T\d{6}_[a-f0-9]{8}$/;

function unsafeReport() {
  throw new Error("unsafe report artifact");
}

async function regularDirectory(directory) {
  const state = await lstat(directory);
  if (state.isSymbolicLink() || !state.isDirectory()) unsafeReport();
  return realpath(directory);
}

async function regularFile(file) {
  const state = await lstat(file);
  if (state.isSymbolicLink() || !state.isFile()) unsafeReport();
  return realpath(file);
}

export async function validateReportArtifact({ command, summary, outputRoot, runDirectory }) {
  const runId = typeof summary?.runId === "string" && RUN_ID.test(summary.runId) ? summary.runId : null;
  const files = summary?.files;
  const descriptor = files && typeof files === "object" && Object.getPrototypeOf(files) === Object.prototype
    ? Object.getOwnPropertyDescriptor(files, "report") : null;
  if (!runId || !descriptor || !Object.hasOwn(descriptor, "value") || typeof descriptor.value !== "string") unsafeReport();

  let requestedRun;
  let canonicalRoot;
  if (command === "run") {
    const requestedRoot = path.resolve(outputRoot);
    canonicalRoot = await regularDirectory(requestedRoot);
    requestedRun = path.join(requestedRoot, runId);
  } else if (command === "regrade") {
    requestedRun = path.resolve(runDirectory);
  } else {
    unsafeReport();
  }
  const requestedReport = path.join(requestedRun, "report.md");
  if (path.resolve(descriptor.value) !== requestedReport) unsafeReport();
  const canonicalRun = await regularDirectory(requestedRun);
  if (path.basename(canonicalRun) !== runId) unsafeReport();
  if (canonicalRoot && path.dirname(canonicalRun) !== canonicalRoot) unsafeReport();
  const expected = path.join(canonicalRun, "report.md");
  if ((await regularFile(requestedReport)) !== expected) unsafeReport();
  return `${runId}/report.md`;
}

function writeResult({ command, json, value, report, stdout }) {
  if (json) {
    stdout.write(`${JSON.stringify({ command, ...value })}\n`);
    return;
  }
  if (command === "validate") {
    stdout.write(`Validation passed: ${value.plan.cases.length} case(s); network sends: no\n`);
    return;
  }
  stdout.write(`Status: ${value.summary.status}; ${value.summary.counts.passed}/${value.summary.counts.total} passed\n`);
  if (report !== null) stdout.write(`Report: ${report}\n`);
}

function diagnostic(error, stderr) {
  if (error instanceof CliUsageError) {
    stderr.write(`${error.message}\n`);
    return 2;
  }
  if (error instanceof EvalError && ERROR_EXIT_CODES[error.errorClass] !== undefined) {
    stderr.write(`email-evals: ${error.errorClass}\n`);
    return ERROR_EXIT_CODES[error.errorClass];
  }
  stderr.write("email-evals: unexpected runner failure\n");
  return 4;
}

export async function main(argv, dependencies = {}) {
  const deps = {
    cwd: process.cwd(),
    environment: process.env,
    stdout: process.stdout,
    stderr: process.stderr,
    loadSuite,
    createAdapter: createE2AAdapter,
    runSuite,
    regradeRun,
    validateReport: validateReportArtifact,
    ...dependencies,
  };
  let parsed;
  try {
    parsed = parseRuntimeArguments(argv);
  } catch (error) {
    return diagnostic(error, deps.stderr);
  }
  if (parsed.help) {
    deps.stdout.write(`${usage()}\n`);
    return 0;
  }

  try {
    const suiteFile = path.resolve(deps.cwd, parsed.suite);
    const suite = await deps.loadSuite(suiteFile, { environment: deps.environment });
    if (parsed.command === "regrade") {
      const runDirectory = path.resolve(deps.cwd, parsed.run);
      const result = await deps.regradeRun({ suite, runDirectory });
      const report = await deps.validateReport({ command: parsed.command, summary: result, runDirectory });
      const output = { summary: safeSummary(result) };
      writeResult({ command: parsed.command, json: parsed.json, value: output, report, stdout: deps.stdout });
      return exitForSummary(result);
    }

    const adapter = deps.createAdapter({ apiKey: suite.transport.apiKey, baseUrl: suite.transport.baseUrl });
    if (parsed.command === "validate") {
      const { preflight, capabilities } = await preflightSuite(suite, adapter);
      writeResult({ command: parsed.command, json: parsed.json, value: { plan: safePlan(preflight, suite, capabilities) }, stdout: deps.stdout });
      return 0;
    }

    // The CLI preflight gives run the same no-send validation as validate.
    // runSuite still owns its preflight boundary, so give it this exact cached
    // result rather than repeating the adapter/network operation.
    const { preflight } = await preflightSuite(suite, adapter);
    const outputRoot = path.resolve(deps.cwd, parsed.output ?? "results");
    const result = await deps.runSuite({
      suite,
      adapter: cachePreflight(adapter, preflight),
      outputRoot,
    });
    const report = await deps.validateReport({ command: parsed.command, summary: result, outputRoot });
    const output = { summary: safeSummary(result) };
    writeResult({ command: parsed.command, json: parsed.json, value: output, report, stdout: deps.stdout });
    return exitForSummary(result);
  } catch (error) {
    return diagnostic(error, deps.stderr);
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2)).then((code) => { process.exitCode = code; });
}
