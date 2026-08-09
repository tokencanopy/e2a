import { randomBytes } from "node:crypto";
import { constants as fsConstants } from "node:fs";
import { lstat, open, readFile, realpath } from "node:fs/promises";
import path from "node:path";
import { parseDocument } from "yaml";
import { assertEvalIdentifier, RESOLVED_ENVIRONMENT_VALUES } from "./contract.mjs";
import {
  EVAL_ERROR_CODE_REGISTRY,
  EvalError,
  isStableEvalErrorCode,
  isStableEvalErrorOrigin,
} from "./errors.mjs";
import { gradeContent } from "./grade-content.mjs";
import { gradeCore } from "./grade-core.mjs";
import { containsMailboxText, NormalizationError, normalizeMailbox } from "./normalize.mjs";
import {
  aliasCaseRecord,
  artifactCaseId,
  artifactSuiteName,
  CASES_ARTIFACT_LIMITS,
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
const MAX_EVIDENCE_STRING = 1_048_576;
const MAX_PROJECTED_EVIDENCE_BYTES = 1024 * 1024;
const ARTIFACT_LIMIT_MESSAGE = "Evaluation artifact reached its cumulative size limit";
const DANGEROUS_OBJECT_KEYS = new Set(["__proto__", "constructor", "prototype"]);
const SECONDARY_STAGES = new Set(["reporting", "on_case", "cleanup"]);
const SECONDARY_CODES = new Set([
  "artifact_changed", "artifact_write_failed", "case_line_too_large", "cases_artifact_limit", "cleanup_failed", "invalid_alias_source", "invalid_artifact",
  "invalid_output_root", "invalid_run_directory", "invalid_run_id", "missing_cases_artifact", "missing_run_directory",
  "on_case_failed", "path_outside_output", "reporting_failed", "run_directory_changed", "run_directory_exists",
  "serialization_failed", "symlink_path", "writer_closed",
]);
const GRADER_BOUNDARIES = new Set(["core", "content"]);

function invalidEvidence() {
  throw new TypeError("invalid evidence");
}

function exactObject(value, allowed) {
  if (!value || Array.isArray(value) || typeof value !== "object" || Object.getPrototypeOf(value) !== Object.prototype) invalidEvidence();
  if (Object.keys(value).some((key) => DANGEROUS_OBJECT_KEYS.has(key) || !allowed.has(key))) invalidEvidence();
  return value;
}

function textValue(value, { nullable = false } = {}) {
  if (nullable && value === null) return null;
  if (typeof value !== "string" || value.length > MAX_EVIDENCE_STRING) invalidEvidence();
  return value;
}

function tokenValue(value, { nullable = false } = {}) {
  if (nullable && value === null) return null;
  const result = textValue(value);
  const grammarValue = result.replace(/\[ENV:[A-Z][A-Z0-9_]*(?::semantic:\d+)?\]/g, "ENV");
  if (result.length > 4_096 || /[\u0000-\u001F\u007F@]/.test(result)
    || !/^[A-Za-z0-9][A-Za-z0-9._:/+-]*$/.test(grammarValue)) invalidEvidence();
  return result;
}

function headerTokenValue(value, { nullable = false } = {}) {
  if (nullable && value === null) return null;
  const result = textValue(value);
  if (result.length > 4_096 || /[\u0000-\u001F\u007F]/.test(result)) invalidEvidence();
  return result;
}

function capabilityValue(value) {
  const result = textValue(value);
  if (!/^[a-z][a-z0-9_]{0,127}$/.test(result)) invalidEvidence();
  return result;
}

function booleanValue(value) {
  if (typeof value !== "boolean") invalidEvidence();
  return value;
}

function integerValue(value, { nullable = false } = {}) {
  if (nullable && value === null) return null;
  if (!Number.isSafeInteger(value) || value < 0) invalidEvidence();
  return value;
}

function stringArray(value, mapper = textValue) {
  if (!Array.isArray(value)) invalidEvidence();
  return value.map((entry) => mapper(entry));
}

function mailboxValue(value, stored, { nullable = false } = {}) {
  if (nullable && value === null) return null;
  if (stored && value && typeof value === "object" && !Array.isArray(value)) {
    const source = exactObject(value, new Set(["address", "displayName"]));
    if (!Object.hasOwn(source, "address") || !Object.hasOwn(source, "displayName")) invalidEvidence();
    const address = mailboxValue(source.address, true);
    const displayName = textValue(source.displayName);
    if (/[\r\n\u0000]/.test(displayName)) invalidEvidence();
    return { address, displayName };
  }
  const text = textValue(value);
  if (stored) {
    if (!/^(?:actor|target|probe:\d+|observed:\d+)$/.test(text)) invalidEvidence();
    return text;
  }
  try {
    normalizeMailbox(text);
    return text;
  } catch (error) {
    if (error instanceof NormalizationError) invalidEvidence();
    throw error;
  }
}

function copyOptional(source, target, key, project) {
  if (Object.hasOwn(source, key)) target[key] = project(source[key]);
}

function projectAttachment(value) {
  const source = exactObject(value, new Set(["filename", "contentType", "disposition", "sizeBytes", "sha256"]));
  const result = {};
  for (const key of ["filename", "contentType", "disposition", "sha256"]) copyOptional(source, result, key, (entry) => textValue(entry, { nullable: true }));
  copyOptional(source, result, "sizeBytes", (entry) => integerValue(entry, { nullable: true }));
  return result;
}

function projectMime(value, stored) {
  if (value === null) return null;
  const source = exactObject(value, new Set([
    "messageId", "inReplyTo", "references", "subject", "from", "replyTo", "text", "htmlPresent", "sizeBytes", "attachments",
  ]));
  const result = {};
  for (const key of ["messageId", "inReplyTo"]) copyOptional(source, result, key, (entry) => headerTokenValue(entry, { nullable: true }));
  for (const key of ["subject", "text"]) copyOptional(source, result, key, (entry) => textValue(entry, { nullable: true }));
  copyOptional(source, result, "references", (entry) => stringArray(entry, headerTokenValue));
  copyOptional(source, result, "from", (entry) => mailboxValue(entry, stored, { nullable: true }));
  copyOptional(source, result, "replyTo", (entry) => stringArray(entry, (address) => mailboxValue(address, stored)));
  copyOptional(source, result, "htmlPresent", booleanValue);
  copyOptional(source, result, "sizeBytes", integerValue);
  copyOptional(source, result, "attachments", (entry) => {
    if (!Array.isArray(entry)) invalidEvidence();
    return entry.map(projectAttachment);
  });
  return result;
}

function projectTransition(value, stored) {
  const source = exactObject(value, new Set([
    "id", "messageId", "direction", "stage", "outcome", "reasonCode", "retryable", "reconstructed", "recipient", "occurredAt",
  ]));
  const result = {};
  for (const key of ["id", "messageId", "direction", "stage", "outcome", "reasonCode", "occurredAt"]) {
    copyOptional(source, result, key, (entry) => tokenValue(entry, { nullable: true }));
  }
  for (const key of ["retryable", "reconstructed"]) copyOptional(source, result, key, booleanValue);
  copyOptional(source, result, "recipient", (entry) => mailboxValue(entry, stored, { nullable: true }));
  return result;
}

function projectCandidateLifecycle(value, stored) {
  const source = exactObject(value, new Set(["submission", "transitions"]));
  const result = {};
  copyOptional(source, result, "submission", (entry) => tokenValue(entry, { nullable: true }));
  copyOptional(source, result, "transitions", (entry) => {
    if (!Array.isArray(entry)) invalidEvidence();
    return entry.map((transition) => projectTransition(transition, stored));
  });
  return result;
}

function projectCandidate(value, stored) {
  const source = exactObject(value, new Set([
    "ref", "eventType", "direction", "provenance", "messageType", "from", "sentAs", "replyTo", "to", "cc", "bcc",
    "envelopeRecipients", "conversationId", "messageId", "observedAt", "sentAt", "mime", "lifecycle",
  ]));
  const result = {};
  for (const key of ["ref", "eventType", "direction", "provenance", "messageType", "conversationId", "messageId", "observedAt", "sentAt"]) {
    copyOptional(source, result, key, (entry) => tokenValue(entry, { nullable: true }));
  }
  for (const key of ["from", "sentAs"]) copyOptional(source, result, key, (entry) => mailboxValue(entry, stored, { nullable: true }));
  for (const key of ["replyTo", "to", "cc", "bcc", "envelopeRecipients"]) {
    copyOptional(source, result, key, (entry) => stringArray(entry, (address) => mailboxValue(address, stored)));
  }
  copyOptional(source, result, "mime", (entry) => projectMime(entry, stored));
  copyOptional(source, result, "lifecycle", (entry) => projectCandidateLifecycle(entry, stored));
  return result;
}

function projectStimulus(value, stored) {
  const source = exactObject(value, new Set([
    "ref", "messageId", "outboundMessageId", "conversationId", "rfcMessageId", "subject", "receivedAt", "participants",
  ]));
  const result = {};
  for (const key of ["ref", "messageId", "outboundMessageId", "conversationId", "receivedAt"]) {
    copyOptional(source, result, key, (entry) => tokenValue(entry, { nullable: true }));
  }
  copyOptional(source, result, "rfcMessageId", (entry) => headerTokenValue(entry, { nullable: true }));
  copyOptional(source, result, "subject", (entry) => textValue(entry, { nullable: true }));
  copyOptional(source, result, "participants", (entry) => stringArray(entry, (address) => mailboxValue(address, stored)));
  return result;
}

function projectActorReceipt(value) {
  if (value === null) return null;
  const source = exactObject(value, new Set(["ref", "messageId", "receiptMessageId", "observedAt"]));
  const result = {};
  for (const key of ["ref", "messageId", "receiptMessageId", "observedAt"]) copyOptional(source, result, key, (entry) => tokenValue(entry, { nullable: true }));
  return result;
}

function projectEvidenceLifecycle(value, stored) {
  const source = exactObject(value, new Set(["stimulus", "candidates", "actorReceived"]));
  const result = {};
  copyOptional(source, result, "stimulus", (entry) => tokenValue(entry, { nullable: true }));
  copyOptional(source, result, "candidates", (entry) => {
    if (!Array.isArray(entry)) invalidEvidence();
    return entry.map((candidate) => {
      const item = exactObject(candidate, new Set(["ref", "submission", "transitions"]));
      const projected = {};
      copyOptional(item, projected, "ref", (field) => tokenValue(field, { nullable: true }));
      copyOptional(item, projected, "submission", (field) => tokenValue(field, { nullable: true }));
      copyOptional(item, projected, "transitions", (field) => {
        if (!Array.isArray(field)) invalidEvidence();
        return field.map((transition) => projectTransition(transition, stored));
      });
      return projected;
    });
  });
  copyOptional(source, result, "actorReceived", booleanValue);
  return result;
}

function projectTimings(value) {
  const source = exactObject(value, new Set([
    "runStartedAt", "caseStartedAt", "sendAcceptedAt", "targetReceivedAt", "firstCandidateAt", "completedAt",
    "timeoutMs", "settleMs", "pollIntervalMs",
  ]));
  const result = {};
  for (const key of ["runStartedAt", "caseStartedAt", "sendAcceptedAt", "targetReceivedAt", "firstCandidateAt", "completedAt"]) {
    copyOptional(source, result, key, (entry) => tokenValue(entry, { nullable: true }));
  }
  for (const key of ["timeoutMs", "settleMs", "pollIntervalMs"]) copyOptional(source, result, key, integerValue);
  return result;
}

function projectRefs(value) {
  const source = exactObject(value, new Set(["stimulus", "candidates", "actorReceipt"]));
  const result = {};
  copyOptional(source, result, "stimulus", (entry) => {
    const item = exactObject(entry, new Set(["event", "message", "outboundMessage"]));
    const projected = {};
    for (const key of ["event", "message", "outboundMessage"]) copyOptional(item, projected, key, (field) => tokenValue(field, { nullable: true }));
    return projected;
  });
  copyOptional(source, result, "candidates", (entry) => stringArray(entry, tokenValue));
  copyOptional(source, result, "actorReceipt", (entry) => tokenValue(entry, { nullable: true }));
  return result;
}

function projectEvidence(value, { stored = false } = {}) {
  const source = exactObject(value, new Set([
    "version", "capabilities", "target", "stimulus", "candidates", "actorReceipt", "lifecycle", "timings", "refs",
  ]));
  if (source.version !== EVIDENCE_VERSION || !Array.isArray(source.capabilities) || !Array.isArray(source.candidates)) invalidEvidence();
  const result = {
    version: EVIDENCE_VERSION,
    capabilities: stringArray(source.capabilities, capabilityValue),
    candidates: source.candidates.map((candidate) => projectCandidate(candidate, stored)),
  };
  copyOptional(source, result, "target", (entry) => {
    const item = exactObject(entry, new Set(["email"]));
    if (!Object.hasOwn(item, "email")) invalidEvidence();
    return { email: mailboxValue(item.email, stored) };
  });
  copyOptional(source, result, "stimulus", (entry) => projectStimulus(entry, stored));
  copyOptional(source, result, "actorReceipt", projectActorReceipt);
  copyOptional(source, result, "lifecycle", (entry) => projectEvidenceLifecycle(entry, stored));
  copyOptional(source, result, "timings", projectTimings);
  copyOptional(source, result, "refs", projectRefs);
  if (Buffer.byteLength(JSON.stringify(result), "utf8") > MAX_PROJECTED_EVIDENCE_BYTES) invalidEvidence();
  return result;
}

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
  if (result.some((entry) => typeof entry !== "string" || !/^[a-z][a-z0-9_]{0,127}$/.test(entry))) {
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

function validateSuite(suite) {
  if (!suite || typeof suite !== "object" || !Array.isArray(suite.cases)
    || typeof suite.digest !== "string" || !/^[a-f0-9]{64}$/.test(suite.digest)
    || typeof suite.name !== "string" || !suite.actor?.email || !suite.target?.email || !suite.defaults
    || suite.cases.some((testCase) => !testCase || typeof testCase !== "object" || typeof testCase.id !== "string")) {
    throw new EvalError("configuration_error", "invalid_suite", "Resolved evaluation suite is invalid");
  }
  assertEvalIdentifier(suite.name, "suiteNameBytes", "/name");
  suite.cases.forEach((testCase, index) => {
    assertEvalIdentifier(testCase.id, "caseIdBytes", `/cases/${index}/id`);
  });
}

function validateInputs(suite, adapter) {
  validateSuite(suite);
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

function primaryEvalError(errorClass, code, message, origin, boundary) {
  if (!isStableEvalErrorOrigin(errorClass, code, origin)) {
    throw new TypeError("Invalid primary evaluation error contract");
  }
  if (boundary !== undefined && !GRADER_BOUNDARIES.has(boundary)) {
    throw new TypeError("Invalid grader boundary contract");
  }
  const serialized = new EvalError(errorClass, code, message).toJSON();
  return {
    class: serialized.class, code: serialized.code, origin,
    ...(boundary === undefined ? {} : { boundary }),
    message: serialized.message,
  };
}

function runGraders(expectation, evidence) {
  let core;
  try {
    core = gradeCore(expectation, evidence);
  } catch {
    return { assertions: [], boundary: "core" };
  }
  try {
    return { assertions: [...core, ...gradeContent(expectation, evidence)], boundary: null };
  } catch {
    return { assertions: [], boundary: "content" };
  }
}

function adapterExecutionError(error) {
  if (error instanceof EvalError && error.errorClass === "transport_error"
    && isStableEvalErrorOrigin("transport_error", error.code, "adapter")) {
    return primaryEvalError("transport_error", error.code, error.message, "adapter");
  }
  return primaryEvalError("transport_error", "adapter_failed", "Evaluation adapter failed during case execution", "adapter_boundary");
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
    const missing = errors.every((assertion) => /^missing_/.test(assertion.code));
    return missing
      ? { status: "error", error: primaryEvalError("capability_error", "required_evidence_unavailable", "Required evaluation evidence was unavailable", "grader") }
      : { status: "error", error: primaryEvalError("transport_error", "invalid_evidence", "Captured evaluation evidence was malformed", "grader") };
  }
  if (assertions.some((assertion) => assertion.status === "fail")) {
    return { status: "fail", error: primaryEvalError("assertion_failure", "assertions_failed", "One or more required assertions did not pass", "grader") };
  }
  return { status: "pass", error: null };
}

function validEvidence(evidence) {
  return evidence && typeof evidence === "object" && !Array.isArray(evidence)
    && evidence.version === EVIDENCE_VERSION && Array.isArray(evidence.candidates)
    && Array.isArray(evidence.capabilities);
}

function summaryDocument({ runId, startedAt, completedAt, suite, capabilities, cases, durations, files, secondaryErrors = [], complete = false }) {
  const counts = countsFor(cases);
  return {
    runId,
    status: complete ? runStatus(counts, suite.cases.length, secondaryErrors) : "incomplete",
    complete,
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
    error = adapterExecutionError(caught);
    status = "error";
  }
  if (!error) {
    const captured = snapshotJsonData(evidence);
    try {
      evidence = captured.ok ? projectEvidence(captured.value) : invalidEvidence();
    } catch {
      evidence = null;
      error = primaryEvalError("transport_error", "invalid_evidence", "Evaluation adapter returned an invalid evidence envelope", "runner");
      status = "error";
    }
  }
  const executionClock = laterInstant(now, caseStart);
  const executionEnd = executionClock.value;
  executionMs = elapsed(executionStart, executionEnd.milliseconds);

  if (!error && executionClock.error) {
    error = primaryEvalError("transport_error", "invalid_clock_after_send", "Evaluation clock became invalid after case execution", "runner");
    status = "error";
  }

  if (!error && !validEvidence(evidence)) {
    error = primaryEvalError("transport_error", "invalid_evidence", "Evaluation adapter returned an invalid evidence envelope", "runner");
    status = "error";
  }

  if (!error && testCase.expect.action.kind !== "none" && evidence.candidates.length === 0) {
    error = primaryEvalError("target_timeout", "no_terminal_response", "Target produced no terminal response", "runner");
    status = "error";
  }

  if (!error) {
    const gradingStart = laterInstant(now, executionEnd).value;
    const graded = runGraders(testCase.expect, evidence);
    if (graded.boundary === null) {
      assertions = graded.assertions;
      ({ status, error } = classifyAssertions(assertions));
    } else {
      error = primaryEvalError(
        "grader_error", "grader_threw",
        "A deterministic grader threw while evaluating captured evidence", "grader", graded.boundary,
      );
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

function artifactLimitRecord(suite, testCase) {
  return {
    id: artifactCaseId(suite, testCase.id),
    status: "error",
    versions: { evidence: EVIDENCE_VERSION },
    suite: { version: suite.version, digest: suite.digest },
    evidence: null,
    assertions: [],
    primaryError: primaryEvalError(
      "transport_error", "cases_artifact_limit", ARTIFACT_LIMIT_MESSAGE, "runner",
    ),
    secondaryErrors: [],
  };
}

function caseLineBytes(record) {
  return Buffer.byteLength(`${JSON.stringify(record)}\n`, "utf8");
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

  const artifactLimitRecords = suite.cases.map((testCase) => artifactLimitRecord(suite, testCase));
  const artifactLimitLineBytes = artifactLimitRecords.map(caseLineBytes);
  const artifactLimitSuffixBytes = new Array(artifactLimitRecords.length + 1).fill(0);
  for (let index = artifactLimitRecords.length - 1; index >= 0; index -= 1) {
    if (artifactLimitLineBytes[index] > CASES_ARTIFACT_LIMITS.lineBytes) {
      throw new EvalError("configuration_error", "cases_artifact_limit", "Evaluation case identifiers cannot fit bounded artifact records");
    }
    artifactLimitSuffixBytes[index] = artifactLimitLineBytes[index] + artifactLimitSuffixBytes[index + 1];
  }
  if (artifactLimitSuffixBytes[0] > CASES_ARTIFACT_LIMITS.totalBytes) {
    throw new EvalError("configuration_error", "cases_artifact_limit", "Evaluation case set cannot fit bounded artifact records");
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
  let artifactLimitReached = false;
  let casesBytes = 0;
  let projectedNormalLineBytes = null;

  try {
    // Deliberately plain and sequential. V0 correlation and rate behavior rely
    // on there never being more than one active case.
    for (let caseIndex = 0; caseIndex < suite.cases.length; caseIndex += 1) {
      const testCase = suite.cases[caseIndex];
      // The exact compact suffix is always reserved before another side effect.
      // Once a previous normal line gives us a deterministic projection, stop
      // sending before that projection would consume the suffix reservation.
      if (!artifactLimitReached && projectedNormalLineBytes !== null
        && casesBytes + projectedNormalLineBytes + artifactLimitSuffixBytes[caseIndex + 1] > CASES_ARTIFACT_LIMITS.totalBytes) {
        artifactLimitReached = true;
      }

      let raw = null;
      let record;
      if (artifactLimitReached) {
        record = artifactLimitRecords[caseIndex];
        raw = record;
      } else {
        raw = await executeOne({ suite, adapter, testCase, runId: resolvedRunId, runStartedAt: runStart.iso, now });
        executionMs += raw.durations.executionMs;
        gradingMs += raw.durations.gradingMs;
        record = aliasCaseRecord(raw, suite);
      }
      if (caseLineBytes(record) > CASES_ARTIFACT_LIMITS.lineBytes) {
        record = aliasCaseRecord({
          ...raw,
          status: "error",
          evidence: null,
          assertions: [],
          primaryError: primaryEvalError("transport_error", "invalid_evidence", "Evaluation evidence exceeded the artifact size limit", "runner"),
        }, suite);
      }

      let recordBytes = caseLineBytes(record);
      if (!artifactLimitReached && recordBytes > CASES_ARTIFACT_LIMITS.lineBytes) {
        artifactLimitReached = true;
        record = artifactLimitRecords[caseIndex];
        raw = record;
        recordBytes = artifactLimitLineBytes[caseIndex];
      }
      if (!artifactLimitReached
        && casesBytes + recordBytes + artifactLimitSuffixBytes[caseIndex + 1] > CASES_ARTIFACT_LIMITS.totalBytes) {
        artifactLimitReached = true;
        record = artifactLimitRecords[caseIndex];
        raw = record;
        recordBytes = artifactLimitLineBytes[caseIndex];
      } else if (!artifactLimitReached) {
        projectedNormalLineBytes = Math.max(projectedNormalLineBytes ?? 0, recordBytes);
      }

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
        if (!caseLineDurable && error?.code === "cases_artifact_limit" && !artifactLimitReached) {
          cases.pop();
          artifactLimitReached = true;
          raw = artifactLimitRecords[caseIndex];
          record = raw;
          recordBytes = artifactLimitLineBytes[caseIndex];
          cases.push(record);
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
          } catch (fallbackError) {
            caseLineDurable = fallbackError?.lineDurable === true;
            if (!caseLineDurable) cases.pop();
            const secondary = reportingError(fallbackError, "reporting", suite, raw);
            runSecondary.push({ caseId: record.id, ...secondary });
            durableReportingFailed = true;
          }
        } else {
          if (!caseLineDurable) cases.pop();
          const secondary = reportingError(error, "reporting", suite, raw);
          runSecondary.push({ caseId: record.id, ...secondary });
          durableReportingFailed = true;
        }
      }
      if (caseLineDurable) casesBytes += recordBytes;
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
      complete: true,
    });
    try {
      await writer.finalize(summary);
    } catch (error) {
      const secondary = reportingError(error, "reporting", suite);
      runSecondary.push(secondary);
      summary = { ...summary, status: "fail", complete: false, secondaryErrors: [...runSecondary] };
      try {
        await writer.commitFailure(summary);
      } catch (commitError) {
        runSecondary.push(reportingError(commitError, "cleanup", suite));
        summary = { ...summary, secondaryErrors: [...runSecondary] };
      }
      await writer.close().catch((closeError) => {
        runSecondary.push(reportingError(closeError, "cleanup", suite));
        summary = { ...summary, secondaryErrors: [...runSecondary] };
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
  if (forceAddress && Object.keys(value).length === 2
    && typeof value.address === "string" && typeof value.displayName === "string") {
    const address = restoreAliasValue(value.address, "address", true);
    return `${JSON.stringify(value.displayName)} <${address}>`;
  }
  const result = {};
  for (const [key, entry] of Object.entries(value)) {
    const childAddress = ["address", "email", "mailbox", "from", "headerFrom", "sentAs", "recipient", "actor", "target"].includes(key)
      || ["to", "cc", "bcc", "replyTo", "envelopeRecipients", "participants"].includes(key)
      || (key === "exactly" && ["sender", "replyTo", "to", "cc", "bcc", "envelope"].includes(parentKey));
    result[key] = restoreAliasValue(entry, key, childAddress);
  }
  return result;
}

function trustedReplayExpectation(testCase, storedExpectation) {
  const rebuilt = restoreAliasValue(storedExpectation);
  const sourceBody = testCase?.expect?.body;
  const targetBody = rebuilt?.body;
  if (!sourceBody || !targetBody || !Array.isArray(targetBody.forbiddenPatterns)) return rebuilt;
  const descriptor = Object.getOwnPropertyDescriptor(sourceBody, "forbiddenPatternRegexes");
  if (descriptor && !descriptor.enumerable && Object.hasOwn(descriptor, "value")
    && Array.isArray(descriptor.value) && descriptor.value.length === targetBody.forbiddenPatterns.length) {
    // This state comes only from the caller-provided trusted resolved suite.
    // Stored artifacts can neither supply nor override executable grader state.
    Object.defineProperty(targetBody, "forbiddenPatternRegexes", { value: descriptor.value });
  }
  return rebuilt;
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

function invalidCaseArtifact() {
  throw new EvalError("configuration_error", "invalid_case_artifact", "Stored evaluation case is invalid");
}

function exactCaseObject(value, allowed) {
  try {
    return exactObject(value, allowed);
  } catch {
    invalidCaseArtifact();
  }
}

function safeStoredString(value, { nullable = false } = {}) {
  if (nullable && value === null) return null;
  if (typeof value !== "string" || value.length > MAX_EVIDENCE_STRING) invalidCaseArtifact();
  if (containsMailboxText(value)) invalidCaseArtifact();
  if (/\b(?:sk|e2a)_[A-Za-z0-9_-]+\b/.test(value)) invalidCaseArtifact();
  return value;
}

function safeStoredControlFreeString(value) {
  const result = safeStoredString(value);
  if (/[\u0000-\u001F\u007F]/.test(result)) invalidCaseArtifact();
  return result;
}

function safeStoredIdentifier(value) {
  const result = safeStoredString(value);
  if (!/^[a-z][a-z0-9_.:-]{0,127}$/.test(result)) invalidCaseArtifact();
  return result;
}

function storedArtifactContainsSecret(value, suite) {
  const secrets = [suite?.transport?.apiKey, ...(suite?.[RESOLVED_ENVIRONMENT_VALUES] ?? []).map((entry) => entry?.value)]
    .filter((entry) => typeof entry === "string" && entry.length > 0 && !SEMANTIC_ENV_VALUES.includes(entry));
  let unsafe = false;
  const visit = (entry) => {
    if (unsafe) return;
    if (typeof entry === "string") {
      unsafe = containsMailboxText(entry)
        || /\b(?:sk|e2a)_[A-Za-z0-9_-]+\b/.test(entry)
        || secrets.some((secret) => entry.includes(secret));
      return;
    }
    if (Array.isArray(entry)) {
      for (const item of entry) visit(item);
      return;
    }
    if (entry && typeof entry === "object") {
      for (const [key, item] of Object.entries(entry)) {
        if (DANGEROUS_OBJECT_KEYS.has(key) || /[\u0000-\u001F\u007F]/.test(key)) { unsafe = true; return; }
        visit(item);
      }
    }
  };
  visit(value);
  return unsafe;
}

function projectStoredDiagnostic(value, depth = 0) {
  if (depth > 64) invalidCaseArtifact();
  if (value === null || typeof value === "boolean") return value;
  if (typeof value === "string") return safeStoredString(value);
  if (typeof value === "number") {
    if (!Number.isFinite(value)) invalidCaseArtifact();
    return value;
  }
  if (Array.isArray(value)) return value.map((entry) => projectStoredDiagnostic(entry, depth + 1));
  const source = exactCaseObject(value, new Set(Object.keys(value ?? {})));
  const result = {};
  for (const [key, entry] of Object.entries(source)) {
    if (/^(?:raw(?:mime|message)?(?:base64)?|attachmentbytes|contentbytes|bytes|content)$/i.test(key)) invalidCaseArtifact();
    result[key] = projectStoredDiagnostic(entry, depth + 1);
  }
  return result;
}

function projectStoredError(value, { secondary = false } = {}) {
  if (value === null && !secondary) return null;
  const allowed = secondary
    ? new Set(["stage", "class", "code", "message"])
    : new Set(["class", "code", "origin", "boundary", "message"]);
  const source = exactCaseObject(value, allowed);
  const result = {};
  if (secondary) {
    if (!Object.hasOwn(source, "stage")) invalidCaseArtifact();
    result.stage = safeStoredIdentifier(source.stage);
  }
  if (Object.hasOwn(source, "class")) result.class = safeStoredIdentifier(source.class);
  if (!secondary) {
    if (!Object.hasOwn(source, "origin")) invalidCaseArtifact();
    result.origin = safeStoredIdentifier(source.origin);
    if (Object.hasOwn(source, "boundary")) result.boundary = safeStoredIdentifier(source.boundary);
  }
  if (!Object.hasOwn(source, "code") || !Object.hasOwn(source, "message")) invalidCaseArtifact();
  result.code = safeStoredIdentifier(source.code);
  result.message = safeStoredControlFreeString(source.message);
  return result;
}

function projectStoredAssertion(value) {
  const source = exactCaseObject(value, new Set(["id", "status", "code", "expected", "actual", "evidenceRefs"]));
  if (!["pass", "fail", "error"].includes(source.status)
    || !Object.hasOwn(source, "id") || !Object.hasOwn(source, "code")
    || !Object.hasOwn(source, "expected") || !Object.hasOwn(source, "actual")
    || !Array.isArray(source.evidenceRefs)) invalidCaseArtifact();
  return {
    id: safeStoredIdentifier(source.id),
    status: source.status,
    code: safeStoredIdentifier(source.code),
    expected: projectStoredDiagnostic(source.expected),
    actual: projectStoredDiagnostic(source.actual),
    evidenceRefs: source.evidenceRefs.map((entry) => safeStoredControlFreeString(entry)),
  };
}

function expectedArtifactExpectation(suite, testCase) {
  return aliasCaseRecord({
    id: testCase.id,
    status: "pass",
    versions: { evidence: EVIDENCE_VERSION },
    suite: { version: suite.version, digest: suite.digest },
    expectation: testCase.expect,
    evidence: { version: EVIDENCE_VERSION, capabilities: [], candidates: [] },
    assertions: [],
    primaryError: null,
    secondaryErrors: [],
  }, suite).expectation;
}

function canonicalObservedAliases(value) {
  const aliases = new Map();
  let next = 1;
  const visit = (entry) => {
    if (typeof entry === "string") {
      return entry.replace(/observed:\d+/g, (alias) => {
        if (!aliases.has(alias)) aliases.set(alias, `observed:${next++}`);
        return aliases.get(alias);
      });
    }
    if (Array.isArray(entry)) return entry.map(visit);
    if (entry && typeof entry === "object") {
      return Object.fromEntries(Object.entries(entry).map(([key, item]) => [key, visit(item)]));
    }
    return entry;
  };
  return visit(value);
}

function validateStoredArtifactLimitRecord(record, suite) {
  const source = exactCaseObject(record, new Set([
    "id", "status", "versions", "suite", "evidence", "assertions", "primaryError", "secondaryErrors",
  ]));
  const versions = exactCaseObject(source.versions, new Set(["evidence"]));
  const suiteRef = exactCaseObject(source.suite, new Set(["version", "digest"]));
  const primaryError = projectStoredError(source.primaryError);
  if (source.status !== "error" || versions.evidence !== EVIDENCE_VERSION
    || suiteRef.version !== suite.version || suiteRef.digest !== suite.digest
    || source.evidence !== null || !Array.isArray(source.assertions) || source.assertions.length !== 0
    || !Array.isArray(source.secondaryErrors) || source.secondaryErrors.length !== 0
    || primaryError.class !== "transport_error" || primaryError.code !== "cases_artifact_limit"
    || primaryError.origin !== "runner") invalidCaseArtifact();
  return {
    id: safeStoredControlFreeString(source.id),
    status: "error",
    versions: { evidence: EVIDENCE_VERSION },
    suite: { version: suite.version, digest: suite.digest },
    evidence: null,
    assertions: [],
    primaryError: { ...primaryError, message: ARTIFACT_LIMIT_MESSAGE },
    secondaryErrors: [],
  };
}

function validateStoredRecord(record, suite, testCase) {
  if (storedArtifactContainsSecret(record, suite)) invalidCaseArtifact();
  if (record?.primaryError?.class === "transport_error"
    && record?.primaryError?.code === "cases_artifact_limit") {
    return validateStoredArtifactLimitRecord(record, suite);
  }
  const source = exactCaseObject(record, new Set([
    "id", "status", "startedAt", "completedAt", "durations", "versions", "suite", "expectation", "evidence",
    "assertions", "primaryError", "secondaryErrors",
  ]));
  if (!["pass", "fail", "error"].includes(source.status) || !Array.isArray(source.assertions)
    || !Array.isArray(source.secondaryErrors)) invalidCaseArtifact();
  const versions = exactCaseObject(source.versions, new Set(["evidence"]));
  const suiteRef = exactCaseObject(source.suite, new Set(["version", "digest"]));
  if (versions.evidence !== EVIDENCE_VERSION || suiteRef.version !== suite.version || suiteRef.digest !== suite.digest) invalidCaseArtifact();
  const expected = expectedArtifactExpectation(suite, testCase);
  if (JSON.stringify(canonicalObservedAliases(source.expectation))
    !== JSON.stringify(canonicalObservedAliases(expected))) invalidCaseArtifact();

  let evidence = null;
  if (source.evidence !== null) {
    try { evidence = projectEvidence(source.evidence, { stored: true }); } catch { invalidCaseArtifact(); }
  }
  const assertions = source.assertions.map(projectStoredAssertion);
  const primaryError = projectStoredError(source.primaryError);
  const secondaryErrors = source.secondaryErrors.map((entry) => projectStoredError(entry, { secondary: true }));
  const allowedClasses = new Set(Object.keys(EVAL_ERROR_CODE_REGISTRY).filter((entry) => entry !== "configuration_error"));
  if (primaryError && !allowedClasses.has(primaryError.class)) invalidCaseArtifact();
  if (primaryError && (!isStableEvalErrorCode(primaryError.class, primaryError.code)
    || !isStableEvalErrorOrigin(primaryError.class, primaryError.code, primaryError.origin))) invalidCaseArtifact();
  if (primaryError?.class === "grader_error") {
    if (!GRADER_BOUNDARIES.has(primaryError.boundary)) invalidCaseArtifact();
  } else if (primaryError && Object.hasOwn(primaryError, "boundary")) invalidCaseArtifact();
  const allAssertionsPass = assertions.length > 0 && assertions.every((assertion) => assertion.status === "pass");
  if (evidence === null && assertions.length > 0) invalidCaseArtifact();
  if (primaryError === null) {
    if (source.status !== "pass" || evidence === null || !allAssertionsPass) invalidCaseArtifact();
  } else if (primaryError.origin === "adapter" || primaryError.origin === "adapter_boundary") {
    if (source.status !== "error" || evidence !== null || assertions.length !== 0
      || primaryError.class !== "transport_error") invalidCaseArtifact();
  } else if (primaryError.origin === "runner") {
    if (source.status !== "error" || assertions.length !== 0) invalidCaseArtifact();
    if (primaryError.class === "target_timeout") {
      if (evidence === null || evidence.candidates.length !== 0) invalidCaseArtifact();
    } else if (primaryError.class === "transport_error" && primaryError.code === "invalid_clock_after_send") {
      if (evidence === null) invalidCaseArtifact();
    } else if (primaryError.class === "transport_error" && ["invalid_evidence", "artifact_limit_exceeded"].includes(primaryError.code)) {
      if (evidence !== null) invalidCaseArtifact();
    } else {
      invalidCaseArtifact();
    }
  } else if (primaryError.origin === "grader") {
    if (evidence === null) invalidCaseArtifact();
    if (primaryError.class === "grader_error") {
      if (source.status !== "error" || assertions.length !== 0) invalidCaseArtifact();
      // A stored grader failure is only evidence that the historical runner
      // crossed this boundary. Rebuild exclusively from the trusted suite
      // expectation and the closed evidence projection, then require the same
      // deterministic boundary to throw now. The stored message is never used.
      const rebuiltExpectation = trustedReplayExpectation(testCase, expected);
      const rebuiltEvidence = restoreAliasValue(evidence);
      const graded = runGraders(rebuiltExpectation, rebuiltEvidence);
      if (graded.boundary !== primaryError.boundary) invalidCaseArtifact();
    } else {
      const classified = classifyAssertions(assertions);
      if (classified.status !== source.status || classified.error?.class !== primaryError.class
        || classified.error?.code !== primaryError.code || classified.error?.origin !== primaryError.origin) invalidCaseArtifact();
    }
  } else {
    invalidCaseArtifact();
  }
  for (const secondary of secondaryErrors) {
    if (!SECONDARY_STAGES.has(secondary.stage) || !SECONDARY_CODES.has(secondary.code)) invalidCaseArtifact();
    if (secondary.class && !new Set([...allowedClasses, "configuration_error"]).has(secondary.class)) invalidCaseArtifact();
  }

  const canonicalPrimary = primaryError ? {
    ...primaryError,
    message: {
      assertion_failure: "One or more required assertions did not pass",
      capability_error: "Required evaluation evidence was unavailable",
      grader_error: "A deterministic grader threw while evaluating captured evidence",
      target_timeout: "Target produced no terminal response",
      transport_error: "Evaluation transport did not complete safely",
    }[primaryError.class],
  } : null;
  const canonicalSecondary = secondaryErrors.map((entry) => ({
    ...entry,
    message: "Evaluation reporting did not complete safely",
  }));

  const result = {
    id: safeStoredControlFreeString(source.id),
    status: source.status,
    versions: { evidence: EVIDENCE_VERSION },
    suite: { version: suite.version, digest: suite.digest },
    expectation: source.expectation,
    evidence,
    assertions,
    primaryError: canonicalPrimary,
    secondaryErrors: canonicalSecondary,
  };
  for (const key of ["startedAt", "completedAt"]) copyOptional(source, result, key, (entry) => safeStoredControlFreeString(entry));
  if (Object.hasOwn(source, "durations")) {
    const durations = exactCaseObject(source.durations, new Set(["executionMs", "gradingMs", "totalMs"]));
    result.durations = {};
    for (const key of ["executionMs", "gradingMs", "totalMs"]) {
      if (!Number.isFinite(durations[key]) || durations[key] < 0) invalidCaseArtifact();
      result.durations[key] = durations[key];
    }
  }
  return result;
}

function priorRunMetadata(value) {
  const defaults = {
    startedAt: null,
    completedAt: null,
    durations: { preflightMs: 0, executionMs: 0, gradingMs: 0, reportingMs: 0, totalMs: 0 },
    capabilities: [],
  };
  const safe = snapshotJsonData(value);
  if (!safe.ok || !safe.value || typeof safe.value !== "object" || Array.isArray(safe.value)) return defaults;
  const source = safe.value;
  const timestamp = (entry) => typeof entry === "string" && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/.test(entry)
    && Number.isFinite(Date.parse(entry)) ? entry : null;
  const capabilities = Array.isArray(source.capabilities)
    && source.capabilities.every((entry) => typeof entry === "string" && /^[a-z][a-z0-9_]{0,127}$/.test(entry))
    ? [...new Set(source.capabilities)].sort() : [];
  let durations = defaults.durations;
  if (source.durations && typeof source.durations === "object" && !Array.isArray(source.durations)) {
    const keys = ["preflightMs", "executionMs", "gradingMs", "reportingMs", "totalMs"];
    if (Object.keys(source.durations).length === keys.length
      && keys.every((key) => Number.isFinite(source.durations[key]) && source.durations[key] >= 0)) {
      durations = Object.fromEntries(keys.map((key) => [key, source.durations[key]]));
    }
  }
  return { startedAt: timestamp(source.startedAt), completedAt: timestamp(source.completedAt), durations, capabilities };
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
  if (casesState.size > CASES_ARTIFACT_LIMITS.totalBytes) throw new EvalError("configuration_error", "invalid_cases_artifact", "Evaluation cases artifact exceeds its size limit");
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
    records = source.length === 0 ? [] : source.trimEnd().split("\n").map((line) => {
      if (Buffer.byteLength(line, "utf8") + 1 > CASES_ARTIFACT_LIMITS.lineBytes) throw new Error("oversized case line");
      const parsed = JSON.parse(line);
      const yaml = parseDocument(line, { strict: true, uniqueKeys: true });
      if (yaml.errors.length > 0) throw new Error("duplicate JSON key");
      const safe = snapshotJsonData(parsed);
      if (!safe.ok) throw new Error("unsafe case record");
      return safe.value;
    });
  } catch {
    throw new EvalError("configuration_error", "invalid_cases_artifact", "Evaluation cases artifact is not valid JSONL");
  }
  return { canonical, records, casesFile };
}

/** Re-run deterministic graders from stored alias-only records; no adapter is accepted or used. */
export async function regradeRun({ suite, runDirectory } = {}) {
  validateSuite(suite);
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
  const validatedRecords = stored.records.map((record, index) => validateStoredRecord(record, suite, suite.cases[index]));
  let artifactLimitSuffix = false;
  for (const record of validatedRecords) {
    const compact = record.primaryError?.code === "cases_artifact_limit";
    if (artifactLimitSuffix && !compact) invalidCaseArtifact();
    if (compact) artifactLimitSuffix = true;
  }

  const cases = validatedRecords.map((storedRecord, index) => {
    if (!storedRecord.evidence || storedRecord.evidence.unavailable === "serialization_error"
      || storedRecord.primaryError?.class === "target_timeout"
      || storedRecord.primaryError?.class === "grader_error"
      || (storedRecord.primaryError?.class === "transport_error" && storedRecord.primaryError.origin !== "grader")) return storedRecord;
    const expectation = trustedReplayExpectation(suite.cases[index], storedRecord.expectation);
    const evidence = restoreAliasValue(storedRecord.evidence);
    let assertions = [];
    let status = "pass";
    let error = null;
    const graded = runGraders(expectation, evidence);
    if (graded.boundary === null) {
      assertions = graded.assertions;
      ({ status, error } = classifyAssertions(assertions));
    } else {
      status = "error";
      error = primaryEvalError(
        "grader_error", "grader_threw",
        "A deterministic grader threw while evaluating captured evidence", "grader", graded.boundary,
      );
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
  prior = priorRunMetadata(prior);
  const counts = countsFor(cases);
  const summary = {
    runId: path.basename(stored.canonical),
    status: runStatus(counts, suite.cases.length),
    complete: true,
    startedAt: prior.startedAt,
    completedAt: prior.completedAt,
    counts,
    durations: prior.durations,
    capabilities: prior.capabilities,
    versions: { runner: RUNNER_VERSION, sdk: SDK_VERSION, suite: suite.version, evidence: EVIDENCE_VERSION },
    suite: { name: artifactSuiteName(suite), version: suite.version, digest: suite.digest },
    cases,
  };
  return rewriteDerivedArtifacts({ runDirectory: stored.canonical, summary });
}

export const RUNNER_VERSIONS = Object.freeze({ runner: RUNNER_VERSION, sdk: SDK_VERSION, evidence: EVIDENCE_VERSION });
