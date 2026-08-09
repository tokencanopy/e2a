import { randomBytes } from "node:crypto";
import { constants as fsConstants } from "node:fs";
import { lstat, open, readFile, realpath } from "node:fs/promises";
import path from "node:path";
import { EvalError } from "./errors.mjs";
import { gradeContent } from "./grade-content.mjs";
import { gradeCore } from "./grade-core.mjs";
import {
  aliasCaseRecord,
  artifactCaseId,
  artifactSuiteName,
  createArtifactWriter,
  reportingError,
  rewriteDerivedArtifacts,
  snapshotJsonData,
  validateRunId,
} from "./report.mjs";

const RUNNER_VERSION = "0.1.0";
const SDK_VERSION = "5.6.0";
const EVIDENCE_VERSION = 1;
const SEMANTIC_ENV_VALUES = Object.freeze([
  "none", "reply", "reply_all", "forward", "new_message", "preserve",
  "required", "forbidden", "equivalent_if_present", "sent", "failed",
  "pending_review", "scheduled", "original", "contains_original", "same",
]);

function instant(now) {
  let value;
  try {
    value = now();
  } catch {
    throw new EvalError("configuration_error", "invalid_clock", "Evaluation clock could not be read");
  }
  const milliseconds = value instanceof Date ? value.getTime()
    : typeof value === "number" ? value
      : typeof value === "string" ? Date.parse(value) : Number.NaN;
  if (!Number.isFinite(milliseconds)) {
    throw new EvalError("configuration_error", "invalid_clock", "Evaluation clock returned an invalid instant");
  }
  try {
    return { milliseconds, iso: new Date(milliseconds).toISOString() };
  } catch {
    throw new EvalError("configuration_error", "invalid_clock", "Evaluation clock returned an invalid instant");
  }
}

function elapsed(start, end) {
  const value = end - start;
  return Number.isFinite(value) ? Math.max(0, value) : 0;
}

function laterInstant(now, fallback) {
  try {
    return { value: instant(now), error: null };
  } catch (error) {
    return { value: fallback, error };
  }
}

function generatedRunId(startedAt) {
  const stamp = startedAt.slice(0, 19).replace(/[-:]/g, "");
  return `run_${stamp}_${randomBytes(4).toString("hex")}`;
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
  if (result.some((entry) => typeof entry !== "string")) {
    throw new EvalError("capability_error", "invalid_capability_set", "Evaluation adapter returned an invalid capability set");
  }
  return result;
}

function requiredCapabilities(suite) {
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

function validateInputs(suite, adapter) {
  if (!suite || typeof suite !== "object" || !Array.isArray(suite.cases)
    || typeof suite.digest !== "string" || !/^[a-f0-9]{64}$/.test(suite.digest)
    || !suite.actor?.email || !suite.target?.email || !suite.defaults) {
    throw new EvalError("configuration_error", "invalid_suite", "Resolved evaluation suite is invalid");
  }
  if (!adapter || typeof adapter.preflight !== "function" || typeof adapter.executeCase !== "function") {
    throw new EvalError("configuration_error", "invalid_adapter", "Evaluation adapter is invalid");
  }
}

function normalizePreflightError(error) {
  if (error instanceof EvalError && ["configuration_error", "capability_error"].includes(error.errorClass)) return error;
  if (error instanceof EvalError) {
    return new EvalError("configuration_error", "preflight_failed", "Evaluation preflight did not complete safely");
  }
  return new EvalError("configuration_error", "preflight_failed", "Evaluation preflight did not complete safely");
}

function primaryError(error, fallbackClass, fallbackCode, fallbackMessage) {
  if (error instanceof EvalError) return error.toJSON();
  return new EvalError(fallbackClass, fallbackCode, fallbackMessage).toJSON();
}

function countsFor(cases) {
  return {
    total: cases.length,
    passed: cases.filter((record) => record.status === "pass").length,
    failed: cases.filter((record) => record.status === "fail").length,
    errors: cases.filter((record) => record.status === "error").length,
  };
}

function runStatus(counts, expectedTotal = counts.total, secondaryErrors = []) {
  const reportingFailed = secondaryErrors.some((error) => ["reporting", "cleanup"].includes(error?.stage));
  return counts.failed === 0 && counts.errors === 0 && counts.total === expectedTotal && !reportingFailed ? "pass" : "fail";
}

function classifyAssertions(assertions) {
  const errors = assertions.filter((assertion) => assertion.status === "error");
  if (errors.length > 0) {
    const missing = errors.every((assertion) => /^(?:missing_|unexpected_actor_receipt$)/.test(assertion.code));
    return missing
      ? { status: "error", error: new EvalError("capability_error", "required_evidence_unavailable", "Required evaluation evidence was unavailable").toJSON() }
      : { status: "error", error: new EvalError("grader_error", "invalid_grader_evidence", "Captured evidence could not be graded safely").toJSON() };
  }
  if (assertions.some((assertion) => assertion.status === "fail")) {
    return { status: "fail", error: new EvalError("assertion_failure", "assertions_failed", "One or more required assertions did not pass").toJSON() };
  }
  return { status: "pass", error: null };
}

function validEvidence(evidence) {
  return evidence && typeof evidence === "object" && !Array.isArray(evidence)
    && evidence.version === EVIDENCE_VERSION && Array.isArray(evidence.candidates)
    && Array.isArray(evidence.capabilities);
}

function summaryDocument({ runId, startedAt, completedAt, suite, capabilities, cases, durations, files, secondaryErrors = [] }) {
  const counts = countsFor(cases);
  return {
    runId,
    status: runStatus(counts, suite.cases.length, secondaryErrors),
    startedAt,
    completedAt,
    counts,
    durations,
    capabilities: [...capabilities].sort(),
    versions: { runner: RUNNER_VERSION, sdk: SDK_VERSION, suite: suite.version, evidence: EVIDENCE_VERSION },
    suite: { name: artifactSuiteName(suite), version: suite.version, digest: suite.digest },
    cases,
    files,
    ...(secondaryErrors.length > 0 ? { secondaryErrors } : {}),
  };
}

function executionContext(suite, testCase, runId, startedAt) {
  return {
    suiteDigest: suite.digest,
    runId,
    actor: suite.actor.email,
    target: suite.target.email,
    startedAt,
    timeoutMs: testCase.timeoutMs ?? suite.defaults.timeoutMs,
    settleMs: testCase.settleMs ?? suite.defaults.settleMs,
    pollIntervalMs: testCase.pollIntervalMs ?? suite.defaults.pollIntervalMs,
  };
}

function freezeDeep(value) {
  if (!value || typeof value !== "object" || Object.isFrozen(value)) return value;
  Object.freeze(value);
  for (const entry of Object.values(value)) freezeDeep(entry);
  return value;
}

async function executeOne({ suite, adapter, testCase, runId, runStartedAt, now }) {
  const caseStart = instant(now);
  let executionMs = 0;
  let gradingMs = 0;
  let evidence = null;
  let assertions = [];
  let status = "pass";
  let error = null;

  const executionStart = caseStart.milliseconds;
  try {
    evidence = await adapter.executeCase(testCase, executionContext(suite, testCase, runId, runStartedAt));
  } catch (caught) {
    error = primaryError(caught, "transport_error", "adapter_threw", "Evaluation transport failed during case execution");
    status = "error";
  }
  if (!error) {
    const captured = snapshotJsonData(evidence);
    if (captured.ok) evidence = captured.value;
    else {
      evidence = null;
      error = new EvalError("transport_error", "invalid_evidence", "Evaluation adapter returned an invalid evidence envelope").toJSON();
      status = "error";
    }
  }
  const executionClock = laterInstant(now, caseStart);
  const executionEnd = executionClock.value;
  executionMs = elapsed(executionStart, executionEnd.milliseconds);

  if (!error && executionClock.error) {
    error = new EvalError("transport_error", "invalid_clock_after_send", "Evaluation clock became invalid after case execution").toJSON();
    status = "error";
  }

  if (!error && !validEvidence(evidence)) {
    error = new EvalError("transport_error", "invalid_evidence", "Evaluation adapter returned an invalid evidence envelope").toJSON();
    status = "error";
  }

  if (!error && testCase.expect.action.kind !== "none" && evidence.candidates.length === 0) {
    error = new EvalError("target_timeout", "no_terminal_response", "Target produced no terminal response").toJSON();
    status = "error";
  }

  if (!error) {
    const gradingStart = laterInstant(now, executionEnd).value;
    try {
      const core = gradeCore(testCase.expect, evidence);
      const content = gradeContent(testCase.expect, evidence);
      assertions = [...core, ...content];
      ({ status, error } = classifyAssertions(assertions));
    } catch {
      error = new EvalError("grader_error", "grader_threw", "A deterministic grader threw while evaluating captured evidence").toJSON();
      status = "error";
    }
    const gradingEnd = laterInstant(now, gradingStart).value;
    gradingMs = elapsed(gradingStart.milliseconds, gradingEnd.milliseconds);
  }

  const caseEnd = laterInstant(now, executionEnd).value;
  return {
    id: testCase.id,
    status,
    startedAt: caseStart.iso,
    completedAt: caseEnd.iso,
    durations: { executionMs, gradingMs, totalMs: elapsed(caseStart.milliseconds, caseEnd.milliseconds) },
    versions: { evidence: EVIDENCE_VERSION },
    suite: { version: suite.version, digest: suite.digest },
    expectation: testCase.expect,
    evidence,
    assertions,
    primaryError: error,
    secondaryErrors: [],
  };
}

/** Execute one preflight and then each case in strict source order. */
export async function runSuite({ suite, adapter, outputRoot, runId, now = () => new Date(), onCase } = {}) {
  validateInputs(suite, adapter);
  const runStart = instant(now);
  const resolvedRunId = validateRunId(runId ?? generatedRunId(runStart.iso));
  const preflightStart = runStart.milliseconds;
  let preflight;
  try {
    preflight = await adapter.preflight(suite);
  } catch (error) {
    throw normalizePreflightError(error);
  }
  const preflightEnd = instant(now);
  const capabilities = capabilitiesArray(preflight?.capabilities ?? adapter.capabilities).sort();
  const missing = requiredCapabilities(suite).filter((name) => !capabilities.includes(name));
  if (missing.length > 0) {
    throw new EvalError("capability_error", "missing_capability", "Evaluation adapter cannot prove every requested assertion", { capabilities: missing });
  }

  // Artifact creation happens after the complete read-only preflight and
  // before the first send. A collision or unsafe path can therefore never
  // leave an unreported mail send behind.
  const writer = await createArtifactWriter({ outputRoot, runId: resolvedRunId });
  const cases = [];
  const runSecondary = [];
  let executionMs = 0;
  let gradingMs = 0;
  let reportingMs = 0;
  let durableReportingFailed = false;

  try {
    // Deliberately plain and sequential. V0 correlation and rate behavior rely
    // on there never being more than one active case.
    for (const testCase of suite.cases) {
      const raw = await executeOne({ suite, adapter, testCase, runId: resolvedRunId, runStartedAt: runStart.iso, now });
      executionMs += raw.durations.executionMs;
      gradingMs += raw.durations.gradingMs;
      const record = aliasCaseRecord(raw, suite);

      cases.push(record);
      let caseLineDurable = false;
      const appendStart = laterInstant(now, runStart).value;
      try {
        const provisional = summaryDocument({
          runId: resolvedRunId,
          startedAt: runStart.iso,
          completedAt: laterInstant(now, appendStart).value.iso,
          suite,
          capabilities,
          cases,
          durations: {
            preflightMs: elapsed(preflightStart, preflightEnd.milliseconds), executionMs, gradingMs,
            reportingMs, totalMs: elapsed(runStart.milliseconds, laterInstant(now, appendStart).value.milliseconds),
          },
          files: writer.files,
          secondaryErrors: runSecondary,
        });
        await writer.appendCase(record, provisional);
        caseLineDurable = true;
      } catch (error) {
        caseLineDurable = error?.lineDurable === true;
        if (!caseLineDurable) cases.pop();
        const secondary = reportingError(error, "reporting", suite, raw);
        runSecondary.push({ caseId: record.id, ...secondary });
        durableReportingFailed = true;
      }
      reportingMs += elapsed(appendStart.milliseconds, laterInstant(now, appendStart).value.milliseconds);

      // Hooks observe only the immutable, already-aliased record and run only
      // after the case line has been appended and fsynced. A throwing or
      // process-terminating hook cannot erase the completed case.
      if (onCase !== undefined && caseLineDurable) {
        const callbackStart = laterInstant(now, appendStart).value;
        try {
          if (typeof onCase !== "function") throw new TypeError("onCase must be a function");
          await onCase(freezeDeep(structuredClone(record)));
        } catch (error) {
          runSecondary.push({ caseId: record.id, ...reportingError(error, "on_case", suite, raw) });
        }
        reportingMs += elapsed(callbackStart.milliseconds, laterInstant(now, callbackStart).value.milliseconds);
      }
      if (durableReportingFailed) break;
    }

    const completed = laterInstant(now, runStart).value;
    let summary = summaryDocument({
      runId: resolvedRunId,
      startedAt: runStart.iso,
      completedAt: completed.iso,
      suite,
      capabilities,
      cases,
      durations: {
        preflightMs: elapsed(preflightStart, preflightEnd.milliseconds), executionMs, gradingMs,
        reportingMs, totalMs: elapsed(runStart.milliseconds, completed.milliseconds),
      },
      files: writer.files,
      secondaryErrors: runSecondary,
    });
    try {
      await writer.finalize(summary);
    } catch (error) {
      const secondary = reportingError(error, "reporting", suite);
      runSecondary.push(secondary);
      summary = { ...summary, secondaryErrors: runSecondary };
      summary.status = "fail";
      await writer.close().catch((closeError) => {
        runSecondary.push(reportingError(closeError, "cleanup", suite));
      });
    }
    // The final artifact and returned value intentionally share this exact
    // immutable timing snapshot. Filesystem latency after the snapshot is not
    // folded into only one representation.
    return summary;
  } finally {
    await writer.close().catch(() => {});
  }
}

function restoreAliasValue(value, parentKey = "", forceAddress = false) {
  const replacements = new Map([["actor", "actor@aliases.invalid"], ["target", "target@aliases.invalid"]]);
  if (typeof value === "string") {
    const restored = value.replace(/\[ENV:[A-Z][A-Z0-9_]*:semantic:(\d+)\]/g, (marker, index) => (
      SEMANTIC_ENV_VALUES[Number(index)] ?? marker
    ));
    if (!forceAddress) return restored;
    if (/^probe:\d+$/.test(restored)) return `probe-${restored.slice(6)}@aliases.invalid`;
    if (/^observed:\d+$/.test(restored)) return `observed-${restored.slice(9)}@aliases.invalid`;
    return replacements.get(restored) ?? restored;
  }
  if (Array.isArray(value)) {
    const childAddress = forceAddress || [
      "to", "cc", "bcc", "replyTo", "envelopeRecipients", "participants", "addresses", "duplicates", "missing", "unexpected", "original",
    ].includes(parentKey);
    return value.map((entry) => restoreAliasValue(entry, parentKey, childAddress));
  }
  if (!value || typeof value !== "object") return value;
  const result = {};
  for (const [key, entry] of Object.entries(value)) {
    const childAddress = ["address", "email", "mailbox", "from", "headerFrom", "sentAs", "recipient", "actor", "target"].includes(key)
      || ["to", "cc", "bcc", "replyTo", "envelopeRecipients", "participants"].includes(key)
      || (key === "exactly" && ["sender", "replyTo", "to", "cc", "bcc", "envelope"].includes(parentKey));
    result[key] = restoreAliasValue(entry, key, childAddress);
  }
  return result;
}

function aliasSuiteFor(record, suite) {
  const probes = new Set();
  const serialized = JSON.stringify(record);
  for (const match of serialized.matchAll(/probe:(\d+)/g)) probes.add(Number(match[1]));
  const aliased = {
    ...suite,
    actor: { email: "actor@aliases.invalid" },
    target: { email: "target@aliases.invalid" },
    transport: {
      allowedEnvelopeRecipients: [
        "actor@aliases.invalid", "target@aliases.invalid",
        ...[...probes].sort((a, b) => a - b).map((index) => `probe-${index}@aliases.invalid`),
      ],
    },
    cases: [{ id: record.id, expect: restoreAliasValue(record.expectation) }],
  };
  for (const symbol of Object.getOwnPropertySymbols(suite)) {
    Object.defineProperty(aliased, symbol, { value: suite[symbol], enumerable: false });
  }
  return aliased;
}

async function readStoredRun(runDirectory) {
  validateRunId(path.basename(runDirectory));
  const state = await lstat(runDirectory);
  if (state.isSymbolicLink() || !state.isDirectory()) throw new EvalError("configuration_error", "invalid_run_directory", "Invalid evaluation run directory");
  const canonical = await realpath(runDirectory);
  if (path.basename(canonical) !== path.basename(runDirectory)) throw new EvalError("configuration_error", "invalid_run_directory", "Invalid evaluation run directory");
  const casesFile = path.join(canonical, "cases.jsonl");
  const casesState = await lstat(casesFile);
  if (casesState.isSymbolicLink() || !casesState.isFile()) throw new EvalError("configuration_error", "invalid_cases_artifact", "Invalid evaluation cases artifact");
  let casesHandle;
  let source;
  try {
    casesHandle = await open(casesFile, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW);
    const opened = await casesHandle.stat();
    if (opened.dev !== casesState.dev || opened.ino !== casesState.ino || !opened.isFile()) {
      throw new EvalError("configuration_error", "artifact_changed", "Evaluation cases artifact changed while opening");
    }
    source = await casesHandle.readFile("utf8");
  } catch (error) {
    if (error instanceof EvalError) throw error;
    throw new EvalError("configuration_error", "invalid_cases_artifact", "Invalid evaluation cases artifact");
  } finally {
    await casesHandle?.close();
  }
  if (!source.endsWith("\n")) throw new EvalError("configuration_error", "interrupted_cases_artifact", "Evaluation cases artifact ends with an incomplete line");
  let records;
  try {
    records = source.length === 0 ? [] : source.trimEnd().split("\n").map((line) => JSON.parse(line));
  } catch {
    throw new EvalError("configuration_error", "invalid_cases_artifact", "Evaluation cases artifact is not valid JSONL");
  }
  return { canonical, records, casesFile };
}

/** Re-run deterministic graders from stored alias-only records; no adapter is accepted or used. */
export async function regradeRun({ suite, runDirectory } = {}) {
  if (!suite || typeof suite.digest !== "string") throw new EvalError("configuration_error", "invalid_suite", "Resolved evaluation suite is invalid");
  const stored = await readStoredRun(runDirectory);
  if (stored.records.some((record) => record?.suite?.digest !== suite.digest)) {
    throw new EvalError("configuration_error", "suite_digest_mismatch", "Stored run does not match the resolved suite digest");
  }
  if (stored.records.some((record) => record?.versions?.evidence !== EVIDENCE_VERSION)) {
    throw new EvalError("configuration_error", "evidence_version_mismatch", "Stored run evidence version is unsupported");
  }
  const expectedIds = suite.cases.map((testCase) => artifactCaseId(suite, testCase.id));
  const storedIds = stored.records.map((record) => record?.id);
  if (storedIds.length !== expectedIds.length || storedIds.some((id, index) => id !== expectedIds[index])
    || new Set(storedIds).size !== storedIds.length) {
    throw new EvalError("configuration_error", "case_set_mismatch", "Stored run cases do not exactly match the resolved suite");
  }

  const cases = stored.records.map((storedRecord) => {
    if (!storedRecord.evidence || storedRecord.evidence.unavailable === "serialization_error"
      || ["transport_error", "target_timeout"].includes(storedRecord.primaryError?.class)) return storedRecord;
    const expectation = restoreAliasValue(storedRecord.expectation);
    const evidence = restoreAliasValue(storedRecord.evidence);
    let assertions = [];
    let status = "pass";
    let error = null;
    try {
      assertions = [...gradeCore(expectation, evidence), ...gradeContent(expectation, evidence)];
      ({ status, error } = classifyAssertions(assertions));
    } catch {
      status = "error";
      error = new EvalError("grader_error", "grader_threw", "A deterministic grader threw while evaluating captured evidence").toJSON();
    }
    return aliasCaseRecord({ ...storedRecord, status, expectation, evidence, assertions, primaryError: error }, aliasSuiteFor(storedRecord, suite));
  });

  let prior = {};
  try {
    const summaryFile = path.join(stored.canonical, "summary.json");
    const summaryState = await lstat(summaryFile);
    if (summaryState.isSymbolicLink() || !summaryState.isFile()) throw new Error("unsafe summary");
    const summaryHandle = await open(summaryFile, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW);
    try {
      const opened = await summaryHandle.stat();
      if (opened.dev !== summaryState.dev || opened.ino !== summaryState.ino || !opened.isFile()) throw new Error("changed summary");
      prior = JSON.parse(await summaryHandle.readFile("utf8"));
    } finally {
      await summaryHandle.close();
    }
  } catch {
    prior = {};
  }
  const counts = countsFor(cases);
  const summary = {
    runId: path.basename(stored.canonical),
    status: runStatus(counts, suite.cases.length),
    startedAt: prior.startedAt ?? null,
    completedAt: prior.completedAt ?? null,
    counts,
    durations: prior.durations ?? { preflightMs: 0, executionMs: 0, gradingMs: 0, reportingMs: 0, totalMs: 0 },
    capabilities: Array.isArray(prior.capabilities) ? [...prior.capabilities].sort() : [],
    versions: { runner: RUNNER_VERSION, sdk: SDK_VERSION, suite: suite.version, evidence: EVIDENCE_VERSION },
    suite: { name: artifactSuiteName(suite), version: suite.version, digest: suite.digest },
    cases,
  };
  return rewriteDerivedArtifacts({ runDirectory: stored.canonical, summary });
}

export const RUNNER_VERSIONS = Object.freeze({ runner: RUNNER_VERSION, sdk: SDK_VERSION, evidence: EVIDENCE_VERSION });
