import { createHash } from "node:crypto";
import path from "node:path";
import { lstat, realpath } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { executionBounds, loadSuite } from "./lib/contract.mjs";
import { createE2AAdapter } from "./lib/e2a-adapter.mjs";
import { CliUsageError, parseRuntimeArguments, usage } from "./lib/cli-arguments.mjs";
import { EvalError } from "./lib/errors.mjs";
import { regradeRun, runSuite } from "./lib/runner.mjs";
import { isSafeResultCaseId } from "./lib/result-contract.mjs";


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
const MAX_PLAN_STRING_BYTES = 64 * 1024;
const MAX_PLAN_TEXT_BYTES = 256 * 1024;
const MAX_PLAN_SUBJECT_BYTES = 998;

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

function safeAlias(value) {
  return typeof value === "string" && /^(?:actor|target|probe:(?:[1-9]|[1-8][0-9]|9[0-8]))$/.test(value);
}

function aliases(value, { nullable = false } = {}) {
  if (nullable && value === null) return null;
  if (!Array.isArray(value) || value.some((entry) => !safeAlias(entry)) || new Set(value).size !== value.length) return null;
  return [...value];
}

function exactKeys(value, keys) {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    && Object.getPrototypeOf(value) === Object.prototype
    && Object.keys(value).length === keys.length
    && keys.every((key) => Object.hasOwn(value, key));
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
  if (!exactKeys(value, Object.keys(value)) || Object.keys(value).length > 64) return false;
  return Object.entries(value).every(([key, entry]) => /^[A-Za-z][A-Za-z0-9_]{0,63}$/.test(key)
    && safePlanValue(entry, depth + 1, budget));
}

function safePlanCase(value, testCase, apiKey) {
  if (!exactKeys(value, [
    "id", "stimulus", "expectedAction", "expectedSender", "expectedRecipients", "recipientAliases",
    "assertions", "evidenceCapabilities", "semanticGraders",
  ]) || value.id !== testCase.id) return null;
  const stimulus = value.stimulus;
  if (!exactKeys(stimulus, ["action", "sender", "recipients", "subject", "text"])
    || stimulus.action !== "send" || stimulus.sender !== "actor"
    || JSON.stringify(stimulus.recipients) !== '["target"]'
    || !safePlanString(stimulus.subject, MAX_PLAN_SUBJECT_BYTES)
    || !safePlanString(stimulus.text, MAX_PLAN_TEXT_BYTES)) return null;
  const action = value.expectedAction;
  if (!exactKeys(action, ["kind", "count"])
    || action.kind !== testCase.expect?.action?.kind || action.count !== testCase.expect?.action?.count
    || !Number.isSafeInteger(action.count) || action.count < 0 || action.count > 100) return null;
  const sender = value.expectedSender;
  if (!exactKeys(sender, ["from", "sentAs", "replyTo", "displayName"])
    || !(sender.from === null || safeAlias(sender.from))
    || !(sender.sentAs === null || safePlanSentAs(sender.sentAs, apiKey))
    || aliases(sender.replyTo, { nullable: true }) === null && sender.replyTo !== null
    || !(sender.displayName === null || safePlanString(sender.displayName))) return null;
  const recipients = value.expectedRecipients;
  if (!exactKeys(recipients, ["to", "cc", "bcc", "envelope"])) return null;
  for (const field of ["to", "cc", "bcc", "envelope"]) {
    if (aliases(recipients[field], { nullable: true }) === null && recipients[field] !== null) return null;
  }
  const recipientAliases = aliases(value.recipientAliases);
  if (recipientAliases === null || !Array.isArray(value.assertions) || value.assertions.length < 3 || value.assertions.length > 128) return null;
  const assertionIds = new Set();
  for (const assertion of value.assertions) {
    if (!exactKeys(assertion, ["id", "expected"])
      || typeof assertion.id !== "string" || !/^[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)+$/.test(assertion.id)
      || assertionIds.has(assertion.id)
      || (assertion.id === "sender.sent_as"
        ? !safePlanSentAs(assertion.expected, apiKey) : !safePlanValue(assertion.expected))) return null;
    assertionIds.add(assertion.id);
  }
  const sentAsAssertions = value.assertions.filter((assertion) => assertion.id === "sender.sent_as");
  if (sender.sentAs === null ? sentAsAssertions.length !== 0
    : sentAsAssertions.length !== 1 || sentAsAssertions[0].expected !== sender.sentAs) return null;
  if (!Array.isArray(value.evidenceCapabilities)
    || value.evidenceCapabilities.some((entry) => !CAPABILITY_NAMES.has(entry))
    || new Set(value.evidenceCapabilities).size !== value.evidenceCapabilities.length
    || !Array.isArray(value.semanticGraders) || value.semanticGraders.length !== 0) return null;
  return {
    ...value,
    recipientAliases,
    expectedSender: { ...sender, replyTo: sender.replyTo === null ? null : aliases(sender.replyTo) },
    expectedRecipients: Object.fromEntries(Object.entries(recipients).map(([field, entries]) => [
      field, entries === null ? null : aliases(entries),
    ])),
    evidenceCapabilities: [...value.evidenceCapabilities],
    semanticGraders: [],
  };
}

function safePlan(preflight, suite, capabilities) {
  const plan = preflight?.plan ?? {};
  const cases = Array.isArray(plan.cases) ? plan.cases : [];
  const baseUrl = typeof plan.baseUrl === "string" ? plan.baseUrl : suite.transport.baseUrl;
  const protectionDigest = typeof preflight?.protectionDigest === "string"
    && /^[a-f0-9]{64}$/.test(preflight.protectionDigest) ? preflight.protectionDigest : null;
  const timeouts = plan.timeouts;
  const executionBudget = plan.executionBudget;
  const plannedTimeoutMs = suite.cases.reduce((total, testCase) => (
    total + (testCase.timeoutMs ?? suite.defaults.timeoutMs)
  ), 0);
  if (typeof baseUrl !== "string" || protectionDigest === null
    || !timeouts || !Number.isSafeInteger(timeouts.maxRetries) || timeouts.maxRetries < 0
    || !Number.isSafeInteger(timeouts.maxElapsedMs) || timeouts.maxElapsedMs <= 0
    || !Number.isSafeInteger(timeouts.timeoutMs) || timeouts.timeoutMs <= 0
    || !exactKeys(executionBudget, ["plannedTimeoutMs", "maximumTimeoutMs"])
    || executionBudget.maximumTimeoutMs !== executionBounds.maxSuiteTimeoutMs
    || executionBudget.plannedTimeoutMs !== plannedTimeoutMs
    || !Number.isSafeInteger(plannedTimeoutMs) || plannedTimeoutMs <= 0
    || plannedTimeoutMs > executionBounds.maxSuiteTimeoutMs) {
    throw new EvalError("capability_error", "invalid_preflight_plan", "Evaluation adapter returned an invalid preflight plan");
  }
  const recipientAliases = aliases(plan.recipientAliases);
  if (recipientAliases === null || recipientAliases.length < 2 || recipientAliases.length > 100
    || !recipientAliases.includes("actor") || !recipientAliases.includes("target")
    || cases.length !== suite.cases.length) {
    throw new EvalError("capability_error", "invalid_preflight_plan", "Evaluation adapter returned an invalid preflight plan");
  }
  const plannedCases = suite.cases.map((testCase, index) => (
    safePlanCase(cases[index], testCase, suite.transport.apiKey)
  ));
  if (plannedCases.some((testCase) => testCase === null)) {
    throw new EvalError("capability_error", "invalid_preflight_plan", "Evaluation adapter returned an invalid preflight plan");
  }
  const approvedPlan = {
    baseUrl,
    networkSends: false,
    capabilities: capabilities.filter((name) => CAPABILITY_NAMES.has(name)),
    recipientAliases,
    protectionDigest,
    timeouts: {
      maxRetries: timeouts.maxRetries,
      maxElapsedMs: timeouts.maxElapsedMs,
      timeoutMs: timeouts.timeoutMs,
    },
    executionBudget: { ...executionBudget },
    cases: suite.cases.map((testCase, index) => ({
      ...plannedCases[index],
      timeoutMs: testCase.timeoutMs ?? suite.defaults.timeoutMs,
      settleMs: testCase.settleMs ?? suite.defaults.settleMs,
      pollIntervalMs: testCase.pollIntervalMs ?? suite.defaults.pollIntervalMs,
    })),
  };
  if (!/^[a-f0-9]{64}$/.test(suite.digest) || !/^[a-f0-9]{64}$/.test(suite.executionDigest)) {
    throw new EvalError("configuration_error", "invalid_suite_digest", "Evaluation suite digest is invalid");
  }
  const approvalDigest = createHash("sha256").update(JSON.stringify({
    domain: "email-evals-plan-approval-v1",
    suiteDigest: suite.digest,
    executionDigest: suite.executionDigest,
    plan: approvedPlan,
  })).digest("hex");
  return { ...approvedPlan, approvalDigest };
}

function safeSummary(summary) {
  const cases = Array.isArray(summary?.cases) ? summary.cases.map((record) => ({
    id: isSafeResultCaseId(record?.id) ? record.id : "unknown-case",
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

function failureClassForSummary(summary) {
  if (summary?.status === "pass") return null;
  const classes = [summary?.primaryError?.class, ...(summary?.cases ?? []).map((record) => record?.primaryError?.class)];
  if (classes.includes("grader_error")) return "grader_error";
  if (classes.includes("transport_error")) return "transport_error";
  if (classes.includes("target_timeout")) return "target_timeout";
  if (classes.includes("configuration_error")) return "configuration_error";
  if (classes.includes("capability_error")) return "capability_error";
  if (classes.includes("assertion_failure")) return "assertion_failure";
  return null;
}

function exitForSummary(summary, { json, stderr }) {
  if (summary?.complete !== true) return 4;
  if (summary?.status === "pass") return 0;
  const errorClass = failureClassForSummary(summary);
  if (!json && errorClass !== null) stderr.write(`email-evals: ${errorClass}\n`);
  return errorClass === null ? 4 : ERROR_EXIT_CODES[errorClass];
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
    requestedRun = path.join(canonicalRoot, runId);
  } else if (command === "regrade") {
    requestedRun = path.resolve(runDirectory);
  } else {
    unsafeReport();
  }
  const canonicalRun = await regularDirectory(requestedRun);
  if (path.basename(canonicalRun) !== runId) unsafeReport();
  if (canonicalRoot && path.dirname(canonicalRun) !== canonicalRoot) unsafeReport();
  const expected = path.join(canonicalRun, "report.md");
  if ((await regularFile(path.resolve(descriptor.value))) !== expected) unsafeReport();
  return `${runId}/report.md`;
}

function writeResult({ command, json, value, report, stdout }) {
  if (json) {
    stdout.write(`${JSON.stringify({ command, ...value, ...(command === "validate" ? {} : { report }) })}\n`);
    return;
  }
  if (command === "validate") {
    stdout.write(`Validation passed: ${value.plan.cases.length} case(s); network sends: no\n`);
    return;
  }
  stdout.write(`Status: ${value.summary.status}; ${value.summary.counts.passed}/${value.summary.counts.total} passed\n`);
  stdout.write("Complete: yes\n");
  if (report !== null) stdout.write(`Report: ${report}\n`);
}

function requireCompletedSummary(summary) {
  if (summary?.complete !== true) throw new Error("incomplete evaluation result");
  return summary;
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
    const suite = await deps.loadSuite(suiteFile, {
      environment: deps.environment,
      trustedOrigin: parsed.trustedOrigin,
    });
    if (parsed.command === "regrade") {
      const runDirectory = path.resolve(deps.cwd, parsed.run);
      const result = requireCompletedSummary(await deps.regradeRun({ suite, runDirectory }));
      const report = await deps.validateReport({ command: parsed.command, summary: result, runDirectory });
      const output = { summary: safeSummary(result) };
      writeResult({ command: parsed.command, json: parsed.json, value: output, report, stdout: deps.stdout });
      return exitForSummary(result, { json: parsed.json, stderr: deps.stderr });
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
    const { preflight, capabilities } = await preflightSuite(suite, adapter);
    const plan = safePlan(preflight, suite, capabilities);
    if (parsed.approvalDigest !== plan.approvalDigest) {
      throw new EvalError(
        "configuration_error",
        "approval_digest_mismatch",
        "Run approval does not match the current validated evaluation plan",
      );
    }
    const outputRoot = path.resolve(deps.cwd, parsed.output ?? "results");
    const result = requireCompletedSummary(await deps.runSuite({
      suite,
      adapter: cachePreflight(adapter, preflight),
      outputRoot,
    }));
    const report = await deps.validateReport({ command: parsed.command, summary: result, outputRoot });
    const output = { summary: safeSummary(result) };
    writeResult({ command: parsed.command, json: parsed.json, value: output, report, stdout: deps.stdout });
    return exitForSummary(result, { json: parsed.json, stderr: deps.stderr });
  } catch (error) {
    return diagnostic(error, deps.stderr);
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2)).then((code) => { process.exitCode = code; });
}
