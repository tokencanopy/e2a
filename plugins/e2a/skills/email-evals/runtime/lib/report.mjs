import { randomBytes } from "node:crypto";
import { constants as fsConstants } from "node:fs";
import {
  lstat, mkdir, open, realpath, rename, unlink,
} from "node:fs/promises";
import path from "node:path";
import { types as utilTypes } from "node:util";
import { EvalError } from "./errors.mjs";
import { RESOLVED_ENVIRONMENT_SOURCES, RESOLVED_ENVIRONMENT_VALUES } from "./contract.mjs";
import {
  mailboxAddressesInText,
  NormalizationError,
  normalizeMailbox,
  replaceMailboxText,
} from "./normalize.mjs";

const RUN_ID_PATTERN = /^run_\d{8}T\d{6}_[a-f0-9]{8}$/;
const SNAPSHOT_LIMITS = Object.freeze({ depth: 64, nodes: 16_384, keys: 512, array: 2_048 });
export const CASES_ARTIFACT_LIMITS = Object.freeze({ lineBytes: 2 * 1024 * 1024, totalBytes: 16 * 1024 * 1024 });
const ADDRESS_SCALARS = new Set([
  "address", "email", "mailbox", "from", "headerFrom", "sentAs", "recipient", "actor", "target",
]);
const ADDRESS_ARRAYS = new Set([
  "to", "cc", "bcc", "replyTo", "envelopeRecipients", "participants", "allowedEnvelopeRecipients",
  "addresses", "duplicates", "missing", "unexpected", "original",
]);
const UNSAFE_ARTIFACT_KEYS = new Set([
  "apikey", "api_key", "environment", "env", "raw", "rawmime", "raw_mime", "rawmessage", "raw_message",
  "bytes", "contentbytes", "content_bytes", "attachmentbytes", "attachment_bytes",
]);

function reportError(code, message) {
  return new EvalError("configuration_error", code, message);
}

function isMissing(error) {
  return error?.code === "ENOENT";
}

async function pathState(file) {
  try {
    return await lstat(file);
  } catch (error) {
    if (isMissing(error)) return null;
    throw error;
  }
}

function snapshot(value) {
  let nodes = 0;
  const active = new Set();

  function visit(source, depth) {
    nodes += 1;
    if (nodes > SNAPSHOT_LIMITS.nodes || depth > SNAPSHOT_LIMITS.depth) throw new TypeError("serialization bounds exceeded");
    if (source === null || typeof source === "string" || typeof source === "boolean") return source;
    if (typeof source === "number") {
      if (!Number.isFinite(source)) throw new TypeError("non-finite number");
      return source;
    }
    if (typeof source !== "object" || utilTypes.isProxy(source) || active.has(source)) {
      throw new TypeError("unsupported JSON value");
    }
    active.add(source);
    try {
      if (Array.isArray(source)) {
        const length = Object.getOwnPropertyDescriptor(source, "length")?.value;
        if (!Number.isSafeInteger(length) || length < 0 || length > SNAPSHOT_LIMITS.array) throw new TypeError("invalid array");
        const names = Reflect.ownKeys(source);
        if (names.length !== length + 1 || names.some((key) => typeof key === "symbol")) throw new TypeError("extended array");
        const result = new Array(length);
        for (let index = 0; index < length; index += 1) {
          const descriptor = Object.getOwnPropertyDescriptor(source, String(index));
          if (!descriptor?.enumerable || !Object.hasOwn(descriptor, "value")) throw new TypeError("invalid array entry");
          result[index] = visit(descriptor.value, depth + 1);
        }
        return result;
      }
      if (Object.getPrototypeOf(source) !== Object.prototype) throw new TypeError("non-plain object");
      const names = Reflect.ownKeys(source);
      if (names.length > SNAPSHOT_LIMITS.keys || names.some((key) => typeof key === "symbol")) throw new TypeError("invalid object keys");
      const result = {};
      for (const key of names) {
        const descriptor = Object.getOwnPropertyDescriptor(source, key);
        if (!descriptor || !Object.hasOwn(descriptor, "value")) throw new TypeError("invalid object property");
        // JSON and the contract intentionally omit validated non-enumerable
        // caches such as forbiddenPatternRegexes. Never invoke an accessor,
        // but ignore safe hidden data properties.
        if (!descriptor.enumerable) continue;
        result[key] = visit(descriptor.value, depth + 1);
      }
      return result;
    } finally {
      active.delete(source);
    }
  }

  try {
    return { ok: true, value: visit(value, 0) };
  } catch {
    return { ok: false };
  }
}

/** Descriptor-safe JSON snapshot used at the adapter trust boundary. */
export function snapshotJsonData(value) {
  return snapshot(value);
}

function normalizeKnownAddress(value) {
  if (typeof value !== "string") return null;
  try {
    return normalizeMailbox(value).address;
  } catch (error) {
    if (error instanceof NormalizationError) return null;
    throw error;
  }
}

function normalizeKnownMailbox(value) {
  if (typeof value !== "string") return null;
  try {
    return normalizeMailbox(value);
  } catch (error) {
    if (error instanceof NormalizationError) return null;
    throw error;
  }
}

function aliasMapFor(suite) {
  const actor = normalizeKnownAddress(suite?.actor?.email);
  const target = normalizeKnownAddress(suite?.target?.email);
  if (!actor || !target || actor === target) throw reportError("invalid_alias_source", "Evaluation aliases require distinct actor and target mailboxes");
  const allowed = Array.isArray(suite?.transport?.allowedEnvelopeRecipients)
    ? suite.transport.allowedEnvelopeRecipients.map(normalizeKnownAddress) : [];
  if (allowed.some((entry) => entry === null)) throw reportError("invalid_alias_source", "Evaluation allowlist contains an invalid mailbox");
  const probes = [...new Set(allowed.filter((entry) => entry !== actor && entry !== target))].sort();
  const aliases = new Map([[actor, "actor"], [target, "target"]]);
  probes.forEach((probe, index) => aliases.set(probe, `probe:${index + 1}`));
  return aliases;
}

function collectTypedAddresses(value, result, parentKey = "", forceAddress = false) {
  if (typeof value === "string") {
    if (forceAddress) {
      const normalized = normalizeKnownAddress(value);
      if (normalized) result.add(normalized);
    }
    for (const address of mailboxAddressesInText(value)) result.add(address);
    return;
  }
  if (Array.isArray(value)) {
    const childAddress = forceAddress || ADDRESS_ARRAYS.has(parentKey);
    for (const entry of value) collectTypedAddresses(entry, result, parentKey, childAddress);
    return;
  }
  if (!value || typeof value !== "object") return;
  for (const [key, entry] of Object.entries(value)) {
    const childAddress = ADDRESS_SCALARS.has(key) || ADDRESS_ARRAYS.has(key)
      || (key === "exactly" && ["sender", "replyTo", "to", "cc", "bcc", "envelope"].includes(parentKey));
    collectTypedAddresses(entry, result, key, childAddress);
  }
}

function aliasesForRecord(suite, parts) {
  const aliases = aliasMapFor(suite);
  const observed = new Set();
  for (const part of parts) collectTypedAddresses(part, observed);
  const unknown = [...observed].filter((address) => !aliases.has(address)).sort();
  unknown.forEach((address, index) => aliases.set(address, `observed:${index + 1}`));
  return aliases;
}

function aliasAddress(value, aliases) {
  const mailbox = normalizeKnownMailbox(value);
  if (!mailbox || !aliases.has(mailbox.address)) return value;
  const address = aliases.get(mailbox.address);
  return mailbox.displayName ? { address, displayName: aliasText(mailbox.displayName, aliases) } : address;
}

function aliasText(value, aliases) {
  return replaceMailboxText(value, (mailbox) => aliases.get(mailbox.address) ?? "[REDACTED:address]");
}

function transformTypedAddresses(value, aliases, parentKey = "", forceAddress = false) {
  if (typeof value === "string") return forceAddress ? aliasAddress(value, aliases) : aliasText(value, aliases);
  if (Array.isArray(value)) {
    const childAddress = forceAddress || ADDRESS_ARRAYS.has(parentKey);
    return value.map((entry) => transformTypedAddresses(entry, aliases, parentKey, childAddress));
  }
  if (!value || typeof value !== "object") return value;
  const result = {};
  for (const [key, entry] of Object.entries(value)) {
    if (UNSAFE_ARTIFACT_KEYS.has(key.toLowerCase())) continue;
    const childAddress = ADDRESS_SCALARS.has(key) || ADDRESS_ARRAYS.has(key)
      || (key === "exactly" && ["sender", "replyTo", "to", "cc", "bcc", "envelope"].includes(parentKey));
    result[key] = transformTypedAddresses(entry, aliases, key, childAddress);
  }
  return result;
}

function patternsFor(record, suite) {
  const source = record?.expectation?.body?.forbiddenPatterns
    ?? suite?.cases?.find((entry) => entry.id === record?.id)?.expect?.body?.forbiddenPatterns
    ?? [];
  if (!Array.isArray(source)) return [];
  const patterns = [];
  for (const [index, pattern] of source.entries()) {
    if (typeof pattern !== "string" || pattern.length > 512) continue;
    try {
      patterns.push({ index, expression: new RegExp(pattern, "g") });
    } catch {
      // Contract validation normally prevents this. Reporting remains safe if
      // called directly with malformed data.
    }
  }
  return patterns;
}

function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function sensitiveValues(suite, aliases) {
  const values = [];
  for (const [address, alias] of aliases) values.push({ value: address, replacement: alias, mailbox: true });
  if (typeof suite?.transport?.apiKey === "string" && suite.transport.apiKey.length > 0) {
    values.push({ value: suite.transport.apiKey, replacement: "[REDACTED:credential]" });
  }
  values.push(...environmentValues(suite));
  return values.sort((left, right) => right.value.length - left.value.length);
}

function environmentValues(suite) {
  const entries = suite?.[RESOLVED_ENVIRONMENT_VALUES];
  if (!Array.isArray(entries)) return [];
  return entries
    .filter((entry) => entry && typeof entry.name === "string" && typeof entry.value === "string" && entry.value.length > 0)
    .map((entry) => ({ value: entry.value, replacement: `[ENV:${entry.name}]` }))
    .sort((left, right) => right.value.length - left.value.length || left.replacement.localeCompare(right.replacement));
}

const SEMANTIC_ENV_VALUES = Object.freeze([
  "none", "reply", "reply_all", "forward", "new_message", "preserve",
  "required", "forbidden", "equivalent_if_present", "sent", "failed",
  "pending_review", "scheduled", "original", "contains_original", "same",
]);

function environmentMarker(entry, resolved) {
  const semantic = SEMANTIC_ENV_VALUES.indexOf(resolved);
  return semantic === -1 ? entry.replacement : `${entry.replacement.slice(0, -1)}:semantic:${semantic}]`;
}

function tokenizeObservedEnvironment(value, environment) {
  if (typeof value === "string") {
    let result = value;
    for (const entry of environment) {
      const replacement = environmentMarker(entry, entry.value);
      result = result.replace(new RegExp(escapeRegExp(entry.value), "g"), replacement);
    }
    return result;
  }
  if (Array.isArray(value)) return value.map((entry) => tokenizeObservedEnvironment(entry, environment));
  if (!value || typeof value !== "object") return value;
  return Object.fromEntries(Object.entries(value).map(([key, entry]) => [key, tokenizeObservedEnvironment(entry, environment)]));
}

function artifactAlias(value) {
  return typeof value === "string" && /^(?:actor|target|probe:\d+|observed:\d+)$/.test(value);
}

function typedAddressField(key, parentKey) {
  return ADDRESS_SCALARS.has(key) || ADDRESS_ARRAYS.has(key)
    || (key === "exactly" && ["sender", "replyTo", "to", "cc", "bcc", "envelope"].includes(parentKey));
}

function tokenizeResolvedSource(value, source, environment, parentKey = "", forceAddress = false) {
  if (typeof source === "string") {
    if (forceAddress && artifactAlias(value)) return value;
    const match = source.match(/^\$\{([A-Z][A-Z0-9_]*)\}$/);
    if (match) {
      const named = environment.find((candidate) => candidate.replacement === `[ENV:${match[1]}]`);
      const entry = environment.find((candidate) => candidate.value === named?.value) ?? named;
      return entry ? environmentMarker(entry, value) : `[ENV:${match[1]}]`;
    }
    return value;
  }
  if (Array.isArray(value) && Array.isArray(source)) {
    const childAddress = forceAddress || ADDRESS_ARRAYS.has(parentKey);
    return value.map((entry, index) => tokenizeResolvedSource(entry, source[index], environment, parentKey, childAddress));
  }
  if (!value || typeof value !== "object" || !source || typeof source !== "object") return value;
  return Object.fromEntries(Object.entries(value).map(([key, entry]) => [
    key, tokenizeResolvedSource(entry, source[key], environment, key, typedAddressField(key, parentKey)),
  ]));
}

function canonicalCase(suite, caseId) {
  const index = suite?.cases?.findIndex((entry) => entry.id === caseId) ?? -1;
  return index < 0 ? null : suite?.[RESOLVED_ENVIRONMENT_SOURCES]?.cases?.[index] ?? null;
}

export function artifactCaseId(suite, caseId) {
  const environment = environmentValues(suite);
  return tokenizeResolvedSource(caseId, canonicalCase(suite, caseId)?.id, environment);
}

export function artifactSuiteName(suite) {
  return tokenizeResolvedSource(suite?.name, suite?.[RESOLVED_ENVIRONMENT_SOURCES]?.name, environmentValues(suite));
}

function redactString(value, patterns, sensitive) {
  let result = value.replace(/[\u0000-\u001F\u007F]/g, "[REDACTED:control]");
  for (const { index, expression } of patterns) {
    expression.lastIndex = 0;
    result = result.replace(expression, `[REDACTED:${index}]`);
  }
  for (const entry of sensitive) {
    result = entry.mailbox
      ? replaceMailboxText(result, (mailbox, candidate) => mailbox.address === entry.value ? entry.replacement : candidate)
      : result.replace(new RegExp(escapeRegExp(entry.value), "g"), entry.replacement);
  }
  result = replaceMailboxText(result, () => "[REDACTED:address]");
  result = result.replace(/\b(?:sk|e2a)_[A-Za-z0-9_-]+\b/g, "[REDACTED:credential]");
  return result;
}

function redactTree(value, patterns, sensitive) {
  if (typeof value === "string") return redactString(value, patterns, sensitive);
  if (Array.isArray(value)) return value.map((entry) => redactTree(entry, patterns, sensitive));
  if (!value || typeof value !== "object") return value;
  return Object.fromEntries(Object.entries(value).map(([key, entry]) => [key, redactTree(entry, patterns, sensitive)]));
}

function safeError(value, stage, patterns, sensitive) {
  const safe = snapshot(value);
  const source = safe.ok && safe.value && typeof safe.value === "object" ? safe.value : {};
  return redactTree({
    ...(stage ? { stage } : {}),
    ...(typeof source.class === "string" ? { class: source.class } : {}),
    ...(typeof source.origin === "string" ? { origin: source.origin } : {}),
    ...(typeof source.boundary === "string" ? { boundary: source.boundary } : {}),
    code: typeof source.code === "string" ? source.code : "operation_failed",
    message: typeof source.message === "string" ? source.message : "Evaluation operation failed",
    ...(source.details && typeof source.details === "object" ? { details: source.details } : {}),
  }, patterns, sensitive);
}

function errorFromThrown(error, stage, patterns, sensitive) {
  const source = error instanceof EvalError ? error.toJSON() : {
    code: `${stage}_failed`,
    message: error instanceof Error && typeof error.message === "string" ? error.message : "Evaluation operation failed",
  };
  return safeError(source, stage, patterns, sensitive);
}

function aliasAssertions(assertions, aliases, patterns, sensitive) {
  return assertions.map((assertion) => {
    let aliased = assertion;
    if (/^(?:recipients\.|sender\.(?!display_name)|action\.)/.test(assertion.id ?? "")) {
      aliased = transformTypedAddresses(assertion, aliases);
      if (/^recipients\./.test(assertion.id ?? "")) {
        aliased.expected = transformTypedAddresses(aliased.expected, aliases, "recipients", true);
        aliased.actual = transformTypedAddresses(aliased.actual, aliases, "recipients", true);
      } else if (/^sender\.(?!display_name)/.test(assertion.id ?? "")) {
        aliased.expected = transformTypedAddresses(aliased.expected, aliases, "sender", true);
        aliased.actual = transformTypedAddresses(aliased.actual, aliases, "sender", true);
      }
    }
    return { ...aliased, actual: redactTree(aliased.actual, patterns, sensitive) };
  });
}

/** Convert the complete case artifact boundary to JSON-safe alias-only data. */
export function aliasCaseRecord(record, suite) {
  const expectation = snapshot(record?.expectation);
  const evidence = snapshot(record?.evidence);
  const assertions = snapshot(record?.assertions);
  const aliases = aliasesForRecord(suite, [
    expectation.ok ? expectation.value : null,
    evidence.ok ? evidence.value : null,
    assertions.ok ? assertions.value : null,
  ]);
  const patterns = patternsFor(record, suite);
  const sensitive = sensitiveValues(suite, aliases);
  const environment = environmentValues(suite);
  const caseSource = canonicalCase(suite, record?.id);
  const source = {};
  if (record && typeof record === "object" && !utilTypes.isProxy(record)) {
    for (const key of [
      "id", "status", "startedAt", "completedAt", "durations", "versions", "suite", "primaryError", "secondaryErrors",
    ]) {
      const descriptor = Object.getOwnPropertyDescriptor(record, key);
      if (!descriptor || !Object.hasOwn(descriptor, "value")) continue;
      const copied = snapshot(descriptor.value);
      if (copied.ok) source[key] = copied.value;
    }
  }
  const secondary = Array.isArray(source.secondaryErrors)
    ? source.secondaryErrors.map((entry) => safeError(entry, entry?.stage, patterns, sensitive)) : [];

  if (!expectation.ok || !evidence.ok || !assertions.ok) {
    secondary.push({ stage: "reporting", code: "serialization_failed", message: "Case data could not be serialized safely" });
  }

  return {
    id: typeof source.id === "string" ? artifactCaseId(suite, source.id) : "unknown-case",
    status: ["pass", "fail", "error"].includes(source.status) ? source.status : "error",
    ...(typeof source.startedAt === "string" ? { startedAt: source.startedAt } : {}),
    ...(typeof source.completedAt === "string" ? { completedAt: source.completedAt } : {}),
    ...(source.durations && typeof source.durations === "object" ? { durations: source.durations } : {}),
    versions: source.versions && typeof source.versions === "object" ? source.versions : { evidence: 1 },
    ...(source.suite && typeof source.suite === "object" ? { suite: source.suite } : {}),
    expectation: expectation.ok
      ? tokenizeResolvedSource(transformTypedAddresses(expectation.value, aliases), caseSource?.expect, environment)
      : { unavailable: "serialization_error" },
    evidence: evidence.ok
      ? tokenizeObservedEnvironment(transformTypedAddresses(evidence.value, aliases), environment)
      : { unavailable: "serialization_error" },
    assertions: assertions.ok && Array.isArray(assertions.value)
      ? tokenizeObservedEnvironment(transformTypedAddresses(aliasAssertions(assertions.value, aliases, patterns, sensitive), aliases), environment) : [],
    primaryError: source.primaryError ? safeError(source.primaryError, null, patterns, sensitive) : null,
    secondaryErrors: secondary,
  };
}

export function validateRunId(runId) {
  if (typeof runId !== "string" || !RUN_ID_PATTERN.test(runId) || path.basename(runId) !== runId) {
    throw reportError("invalid_run_id", "Invalid evaluation run ID");
  }
  return runId;
}

async function assertDirectory(directory, label) {
  const state = await pathState(directory);
  if (!state) throw reportError("missing_run_directory", `${label} does not exist`);
  if (state.isSymbolicLink()) throw reportError("symlink_path", `${label} must not be a symlink`);
  if (!state.isDirectory()) throw reportError("invalid_run_directory", `${label} must be a directory`);
  return realpath(directory);
}

async function assertNoSymlinkAncestry(target) {
  const absolute = path.resolve(target);
  const parsed = path.parse(absolute);
  let current = parsed.root;
  for (const component of absolute.slice(parsed.root.length).split(path.sep).filter(Boolean)) {
    current = path.join(current, component);
    const state = await pathState(current);
    if (!state) break;
    if (state.isSymbolicLink()) {
      // macOS exposes its stable system temp roots through /var and /tmp
      // aliases. Accept only those exact platform aliases; user-controlled
      // descendant symlinks remain rejected.
      const platformAlias = ["/var", "/tmp"].includes(current)
        && await realpath(current) === `/private${current}`;
      if (!platformAlias) throw reportError("symlink_path", "Evaluation artifact ancestry must not contain symlinks");
    }
  }
}

async function assertRegularOrMissing(file) {
  const state = await pathState(file);
  if (!state) return null;
  if (state.isSymbolicLink()) throw reportError("symlink_path", "Refusing to replace a symlinked evaluation artifact");
  if (!state.isFile()) throw reportError("invalid_artifact", "Evaluation artifact must be a regular file");
  return state;
}

async function syncDirectory(directory) {
  let handle;
  try {
    handle = await open(directory, fsConstants.O_RDONLY);
    await handle.sync();
  } catch (error) {
    if (!["EINVAL", "ENOTSUP", "EISDIR"].includes(error?.code)) throw error;
  } finally {
    await handle?.close();
  }
}

async function atomicWrite(file, content, guard = async () => {}) {
  await guard();
  await assertRegularOrMissing(file);
  const directory = path.dirname(file);
  const temporary = path.join(directory, `.${path.basename(file)}.${process.pid}.${randomBytes(6).toString("hex")}.tmp`);
  let handle;
  try {
    handle = await open(temporary, "wx", 0o600);
    await handle.writeFile(content, "utf8");
    await handle.sync();
    await handle.close();
    handle = null;
    await guard();
    await assertRegularOrMissing(file);
    await rename(temporary, file);
    await syncDirectory(directory);
  } catch (error) {
    await handle?.close().catch(() => {});
    await unlink(temporary).catch(() => {});
    throw error;
  }
}

function diskSummary(summary) {
  return {
    ...summary,
    files: summary.files ? Object.fromEntries(Object.entries(summary.files).map(([key, value]) => [key, path.basename(value)])) : undefined,
  };
}

function markdownCell(value) {
  return String(value ?? "").replace(/\\/g, "\\\\").replace(/\|/g, "\\|").replace(/[\r\n]+/g, " ");
}

/** Render a byte-stable final human report from case records. */
export function renderMarkdown(summary) {
  const lines = [
    `# Email eval report: ${summary.suite?.name ?? "unknown"}`,
    "",
    `- Run: \`${summary.runId ?? "unknown"}\``,
    `- Status: **${summary.status ?? "fail"}**`,
    `- Cases: ${summary.counts?.passed ?? 0} passed, ${summary.counts?.failed ?? 0} failed, ${summary.counts?.errors ?? 0} errors`,
    `- Suite digest: \`${summary.suite?.digest ?? "unknown"}\``,
    "",
    "| Case | Status | Primary result |",
    "| --- | --- | --- |",
  ];
  for (const record of summary.cases ?? []) {
    lines.push(`| ${markdownCell(record.id)} | ${markdownCell(record.status)} | ${markdownCell(record.primaryError?.code ?? "passed")} |`);
  }

  const failures = [];
  const errors = [];
  for (const record of summary.cases ?? []) {
    for (const assertion of record.assertions ?? []) {
      const entry = `- \`${record.id}\` / \`${assertion.id}\`: \`${assertion.code}\``;
      if (assertion.status === "fail") failures.push(entry);
      if (assertion.status === "error") errors.push(entry);
    }
  }
  if (failures.length > 0) lines.push("", "## Failed assertions", "", ...failures);
  if (errors.length > 0) lines.push("", "## Assertion errors", "", ...errors);

  const secondary = (summary.cases ?? []).flatMap((record) => (record.secondaryErrors ?? []).map(
    (error) => `- \`${record.id}\` / \`${error.stage ?? "secondary"}\`: \`${error.code}\``,
  ));
  if (secondary.length > 0) lines.push("", "## Secondary errors", "", ...secondary);
  return `${lines.join("\n")}\n`;
}

/** Create a new, exclusive run directory and its durable append handle. */
export async function createArtifactWriter({ outputRoot, runId }) {
  validateRunId(runId);
  if (typeof outputRoot !== "string" || outputRoot.length === 0) throw reportError("invalid_output_root", "Invalid evaluation output root");
  await assertNoSymlinkAncestry(outputRoot);
  let outputState = await pathState(outputRoot);
  if (!outputState) {
    try {
      await mkdir(outputRoot, { mode: 0o700 });
    } catch (error) {
      if (error?.code !== "EEXIST") throw error;
    }
    outputState = await pathState(outputRoot);
  }
  if (outputState?.isSymbolicLink()) throw reportError("symlink_path", "Evaluation output root must not be a symlink");
  if (!outputState?.isDirectory()) throw reportError("invalid_output_root", "Evaluation output root must be a directory");
  const canonicalRoot = await realpath(outputRoot);
  const runDirectory = path.join(canonicalRoot, runId);
  try {
    await mkdir(runDirectory, { mode: 0o700 });
  } catch (error) {
    if (error?.code === "EEXIST") throw reportError("run_directory_exists", "Evaluation run directory already exists");
    throw error;
  }
  const canonicalRun = await realpath(runDirectory);
  if (path.dirname(canonicalRun) !== canonicalRoot || path.basename(canonicalRun) !== runId) {
    throw reportError("path_outside_output", "Evaluation run directory escaped the output root");
  }
  const files = Object.freeze({
    cases: path.join(canonicalRun, "cases.jsonl"),
    summary: path.join(canonicalRun, "summary.json"),
    report: path.join(canonicalRun, "report.md"),
  });
  const casesHandle = await open(files.cases, "wx", 0o600);
  const identity = await casesHandle.stat();
  const runIdentity = await lstat(canonicalRun);
  let closed = false;
  let casesBytes = 0;

  async function verifyRunAndCases() {
    const directory = await lstat(canonicalRun);
    if (!directory.isDirectory() || directory.isSymbolicLink()
      || directory.dev !== runIdentity.dev || directory.ino !== runIdentity.ino
      || await realpath(canonicalRun) !== canonicalRun) {
      throw reportError("run_directory_changed", "Evaluation run directory changed during reporting");
    }
    const current = await lstat(files.cases);
    if (!current.isFile() || current.isSymbolicLink() || current.dev !== identity.dev || current.ino !== identity.ino) {
      throw reportError("artifact_changed", "Evaluation cases artifact changed during reporting");
    }
  }

  return {
    runDirectory: canonicalRun,
    files,
    async appendCase(record, summary) {
      if (closed) throw reportError("writer_closed", "Evaluation artifact writer is closed");
      let lineDurable = false;
      try {
        await verifyRunAndCases();
        const line = `${JSON.stringify(record)}\n`;
        const lineBytes = Buffer.byteLength(line, "utf8");
        if (lineBytes > CASES_ARTIFACT_LIMITS.lineBytes) {
          throw reportError("case_line_too_large", "Evaluation case record exceeds its size limit");
        }
        if (casesBytes + lineBytes > CASES_ARTIFACT_LIMITS.totalBytes) {
          throw reportError("cases_artifact_limit", "Evaluation cases artifact exceeds its cumulative size limit");
        }
        await casesHandle.writeFile(line, "utf8");
        await casesHandle.sync();
        casesBytes += lineBytes;
        lineDurable = true;
        await atomicWrite(files.summary, `${JSON.stringify(diskSummary(summary), null, 2)}\n`, verifyRunAndCases);
      } catch (error) {
        if (error && (typeof error === "object" || typeof error === "function") && Object.isExtensible(error)) {
          Object.defineProperty(error, "lineDurable", { value: lineDurable });
          throw error;
        }
        const wrapped = reportError("artifact_write_failed", "Evaluation artifact could not be written safely");
        Object.defineProperty(wrapped, "lineDurable", { value: lineDurable });
        throw wrapped;
      }
    },
    async finalize(summary) {
      if (closed) throw reportError("writer_closed", "Evaluation artifact writer is closed");
      await verifyRunAndCases();
      // report.md is prepared before summary.json. The final summary rename is
      // the run's commit marker, so an on-disk pass can never name a missing
      // or failed final report.
      await atomicWrite(files.report, renderMarkdown(summary), verifyRunAndCases);
      await atomicWrite(files.summary, `${JSON.stringify(diskSummary(summary), null, 2)}\n`, verifyRunAndCases);
      await casesHandle.close();
      closed = true;
      await syncDirectory(canonicalRun);
    },
    async commitFailure(summary) {
      if (closed) throw reportError("writer_closed", "Evaluation artifact writer is closed");
      await verifyRunAndCases();
      await atomicWrite(files.summary, `${JSON.stringify(diskSummary(summary), null, 2)}\n`, verifyRunAndCases);
    },
    async close() {
      if (!closed) {
        await casesHandle.close();
        closed = true;
      }
    },
  };
}

/** Rewrite only derived summary/report files for a validated existing run. */
export async function rewriteDerivedArtifacts({ runDirectory, summary }) {
  await assertNoSymlinkAncestry(runDirectory);
  validateRunId(path.basename(runDirectory));
  const canonicalRun = await assertDirectory(runDirectory, "Evaluation run directory");
  const runIdentity = await lstat(canonicalRun);
  if (path.basename(canonicalRun) !== path.basename(runDirectory)) throw reportError("invalid_run_directory", "Invalid evaluation run directory");
  const cases = path.join(canonicalRun, "cases.jsonl");
  const casesState = await assertRegularOrMissing(cases);
  if (!casesState) throw reportError("missing_cases_artifact", "Evaluation cases artifact is missing");
  const files = { cases, summary: path.join(canonicalRun, "summary.json"), report: path.join(canonicalRun, "report.md") };
  const withFiles = { ...summary, files };
  const guard = async () => {
    const directory = await lstat(canonicalRun);
    const currentCases = await lstat(cases);
    if (!directory.isDirectory() || directory.isSymbolicLink()
      || directory.dev !== runIdentity.dev || directory.ino !== runIdentity.ino
      || !currentCases.isFile() || currentCases.isSymbolicLink()
      || currentCases.dev !== casesState.dev || currentCases.ino !== casesState.ino) {
      throw reportError("artifact_changed", "Evaluation artifacts changed during regrading");
    }
  };
  const provisional = { ...withFiles, status: "incomplete", complete: false };
  // Match live-run publication: once regrading starts, the durable marker is
  // explicitly incomplete until both derived artifacts are ready.
  await atomicWrite(files.summary, `${JSON.stringify(diskSummary(provisional), null, 2)}\n`, guard);
  try {
    // Regrade uses the same commit marker as a live run: report first, then a
    // complete summary as the final atomic publication.
    await atomicWrite(files.report, renderMarkdown(withFiles), guard);
    await atomicWrite(files.summary, `${JSON.stringify(diskSummary(withFiles), null, 2)}\n`, guard);
    return withFiles;
  } catch {
    const incomplete = {
      ...withFiles,
      status: "incomplete",
      complete: false,
      secondaryErrors: [
        ...(Array.isArray(withFiles.secondaryErrors) ? withFiles.secondaryErrors : []),
        { stage: "reporting", code: "reporting_failed", message: "Evaluation reporting did not complete safely" },
      ],
    };
    try {
      await atomicWrite(files.summary, `${JSON.stringify(diskSummary(incomplete), null, 2)}\n`, guard);
    } catch {
      // The previous incomplete summary remains the only valid commit marker
      // when even the failure marker cannot be published.
    }
    return incomplete;
  }
}

export function reportingError(error, stage, suite, record = {}) {
  const aliases = aliasMapFor(suite);
  return errorFromThrown(error, stage, patternsFor(record, suite), sensitiveValues(suite, aliases));
}
