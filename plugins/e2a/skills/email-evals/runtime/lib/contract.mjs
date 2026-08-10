import { createHash } from "node:crypto";
import { constants as fsConstants } from "node:fs";
import { open, realpath, stat } from "node:fs/promises";
import path from "node:path";
import { parseDocument } from "yaml";
import { EvalError } from "./errors.mjs";
import { NormalizationError, normalizeAddressSet, normalizeMailbox, parseDuration } from "./normalize.mjs";
import { EVAL_IDENTIFIER_LIMITS, isEvalLiteralIdentifier } from "./result-contract.mjs";

export { EVAL_IDENTIFIER_LIMITS } from "./result-contract.mjs";

export const RESOLVED_ENVIRONMENT_VALUES = Symbol.for("e2a.email-evals.resolved-environment-values");
export const RESOLVED_ENVIRONMENT_SOURCES = Symbol.for("e2a.email-evals.resolved-environment-sources");

const environmentReference = /^\$\{([A-Z][A-Z0-9_]*)\}$/;
const suiteKeys = new Set(["version", "name", "target", "actor", "transport", "defaults", "cases"]);
const identityKeys = new Set(["email"]);
const transportKeys = new Set(["adapter", "api_key", "base_url", "allowed_envelope_recipients"]);
const defaultsKeys = new Set(["timeout", "settle", "poll_interval"]);
const caseKeys = new Set(["id", "send", "expect"]);
const sendKeys = new Set(["subject", "text"]);
const expectKeys = new Set(["action", "sender", "recipients", "thread", "subject", "body", "attachments", "timing", "lifecycle"]);
const actionKeys = new Set(["kind", "count"]);
const senderKeys = new Set(["exactly", "sent_as", "reply_to", "display_name"]);
const replyToKeys = new Set(["exactly"]);
const recipientsKeys = new Set(["to", "cc", "bcc", "envelope"]);
const recipientSetKeys = new Set(["exactly"]);
const threadKeys = new Set(["message_id", "in_reply_to", "references", "conversation"]);
const subjectKeys = new Set(["exact", "regex", "policy", "required_fragments", "forbidden_fragments"]);
const bodyKeys = new Set(["required_facts", "forbidden_patterns", "plain_text", "html", "max_size"]);
const htmlKeys = new Set(["policy"]);
const attachmentsKeys = new Set(["exactly"]);
const attachmentKeys = new Set(["filename", "content_type", "disposition", "size_bytes", "sha256"]);
const timingKeys = new Set(["reply_within"]);
const lifecycleKeys = new Set(["submission", "actor_received"]);

const actionKinds = new Set(["none", "reply", "reply_all", "forward", "new_message"]);
const subjectPolicies = new Set(["preserve", "forward"]);
const plainTextPolicies = new Set(["required", "forbidden"]);
const htmlPolicies = new Set(["required", "forbidden", "equivalent_if_present"]);
const submissionStates = new Set(["sent", "failed", "pending_review", "scheduled"]);
const mailboxEnvironmentName = /^E2A_EVAL_(?:ACTOR|TARGET|PROBE_[A-Z0-9_]{1,64})$/;
const sentAsToken = /^[a-z][a-z0-9_]{0,63}$/;
const MAX_YAML_FILE_BYTES = 256 * 1024;
const MAX_YAML_TOTAL_BYTES = 4 * 1024 * 1024;
const MAX_CASES = 100;
const MAX_ARRAY_ITEMS = 100;
const MAX_ATTACHMENTS = 32;
const MAX_ACTION_COUNT = 100;
const MAX_STRING_BYTES = 64 * 1024;
const MAX_SEND_TEXT_BYTES = 256 * 1024;
const MAX_SUBJECT_BYTES = 998;
const MAX_MESSAGE_BYTES = 25 * 1024 * 1024;
const MAX_SUITE_EXECUTION_MS = 25 * 60 * 1_000;
export const executionBounds = Object.freeze({ maxSuiteTimeoutMs: MAX_SUITE_EXECUTION_MS });
function configurationError(code, message, pointer) {
  return new EvalError("configuration_error", code, message, pointer ? { path: pointer } : undefined);
}

export function assertEvalIdentifierLength(value, kind, pointer) {
  const limit = EVAL_IDENTIFIER_LIMITS[kind];
  if (typeof value === "string" && Number.isSafeInteger(limit) && Buffer.byteLength(value, "utf8") > limit) {
    throw configurationError("identifier_too_long", "Evaluation identifier exceeds its size limit", pointer);
  }
  return value;
}

export function assertEvalIdentifier(value, kind, pointer) {
  assertEvalIdentifierLength(value, kind, pointer);
  if (!isEvalLiteralIdentifier(value, EVAL_IDENTIFIER_LIMITS[kind])) {
    const suiteName = kind === "suiteNameBytes";
    throw configurationError(
      suiteName ? "invalid_suite_name" : "invalid_case_id",
      suiteName ? "Invalid suite name" : "Invalid case identifier",
      pointer,
    );
  }
  return value;
}

function pathOf(parent, key) {
  return `${parent}/${String(key).replaceAll("~", "~0").replaceAll("/", "~1")}`;
}

function asObject(value, pointer) {
  if (!value || Array.isArray(value) || typeof value !== "object" || Object.getPrototypeOf(value) !== Object.prototype) {
    throw configurationError("invalid_schema", "Expected an object", pointer);
  }
  return value;
}

function allowedObject(value, allowed, pointer, required = []) {
  const object = asObject(value, pointer);
  for (const key of Object.keys(object)) {
    if (!allowed.has(key)) throw configurationError("unknown_key", "Unknown configuration key", pathOf(pointer, key));
  }
  for (const key of required) {
    if (!(key in object)) throw configurationError("missing_required_key", "Missing required configuration key", pathOf(pointer, key));
  }
  return object;
}

function asString(value, pointer, maximumBytes = MAX_STRING_BYTES) {
  if (typeof value !== "string" || value.length === 0) throw configurationError("invalid_schema", "Expected a non-empty string", pointer);
  if (Buffer.byteLength(value, "utf8") > maximumBytes) {
    throw configurationError("string_too_large", "Configuration string exceeds its size limit", pointer);
  }
  return value;
}

function asArray(value, pointer, maximumItems = MAX_ARRAY_ITEMS) {
  if (!Array.isArray(value)) throw configurationError("invalid_schema", "Expected an array", pointer);
  if (value.length > maximumItems) throw configurationError("array_too_large", "Configuration array exceeds its item limit", pointer);
  return value;
}

function asBoolean(value, pointer) {
  if (typeof value !== "boolean") throw configurationError("invalid_schema", "Expected a boolean", pointer);
  return value;
}

function asNonnegativeInteger(value, pointer) {
  if (!Number.isSafeInteger(value) || value < 0) throw configurationError("invalid_schema", "Expected a non-negative integer", pointer);
  return value;
}

function resolveEnvironment(value, environment, pointer, allowedName = () => false, maximumBytes = MAX_STRING_BYTES) {
  const source = asString(value, pointer, maximumBytes);
  if (source.includes("${") && !environmentReference.test(source)) {
    throw configurationError("partial_environment_reference", "Environment references must occupy the complete scalar", pointer);
  }
  const match = source.match(environmentReference);
  if (!match) return { source, value: source };
  if (!allowedName(match[1])) {
    throw configurationError("environment_reference_not_allowed", "Environment reference is not allowed for this field", pointer);
  }
  const resolved = environment?.[match[1]];
  if (typeof resolved !== "string" || resolved.length === 0) {
    throw configurationError("missing_environment", `Missing environment variable ${match[1]}`, pointer);
  }
  if (Buffer.byteLength(resolved, "utf8") > maximumBytes) {
    throw configurationError("string_too_large", "Resolved environment value exceeds its size limit", pointer);
  }
  return { source, value: resolved };
}

function resolveString(value, environment, pointer, maximumBytes = MAX_STRING_BYTES) {
  return resolveEnvironment(value, environment, pointer, () => false, maximumBytes);
}

function normalizeScalarMailbox(value, environment, pointer) {
  const resolved = resolveEnvironment(value, environment, pointer, (name) => mailboxEnvironmentName.test(name));
  try {
    const mailbox = normalizeMailbox(resolved.value);
    return {
      ...resolved,
      mailbox,
      canonical: environmentReference.test(resolved.source)
        ? resolved.source
        : { address: mailbox.address, displayName: mailbox.displayName ?? null },
    };
  } catch (error) {
    if (error instanceof NormalizationError) throw configurationError(error.code, "Invalid mailbox", pointer);
    throw error;
  }
}

function normalizeMailboxSet(value, environment, pointer) {
  const raw = asArray(value, pointer);
  const resolved = raw.map((entry, index) => normalizeScalarMailbox(entry, environment, `${pointer}/${index}`));
  try {
    const addresses = normalizeAddressSet(resolved.map((entry) => entry.value));
    const ordered = [...resolved].sort((left, right) => left.mailbox.address.localeCompare(right.mailbox.address));
    return { source: ordered.map((entry) => entry.canonical), addresses };
  } catch (error) {
    if (error instanceof NormalizationError) throw configurationError(error.code, "Invalid recipient set", pointer);
    throw error;
  }
}

function normalizeDuration(value, environment, pointer) {
  const resolved = resolveString(value, environment, pointer);
  try {
    return { source: resolved.source, milliseconds: parseDuration(resolved.value) };
  } catch (error) {
    if (error instanceof NormalizationError) throw configurationError(error.code, "Invalid duration", pointer);
    throw error;
  }
}

function normalizeSentAs(value, environment, pointer) {
  const resolved = resolveString(value, environment, pointer);
  if (!sentAsToken.test(resolved.value)) {
    throw configurationError("invalid_sent_as", "Sent-as expectation must be a bounded token", pointer);
  }
  return resolved;
}

function normalizeRegexes(value, environment, pointer) {
  return asArray(value, pointer).map((entry, index) => {
    const resolved = resolveString(entry, environment, `${pointer}/${index}`);
    if (environmentReference.test(resolved.source)) {
      throw configurationError(
        "regex_environment_not_supported",
        "Regular expressions must be literal so saved evidence can be regraded without environment secrets",
        `${pointer}/${index}`,
      );
    }
    if (resolved.value.length > 512) throw configurationError("invalid_regex", "Regular expression exceeds the maximum length", `${pointer}/${index}`);
    try {
      return { source: resolved.source, value: resolved.value, regex: new RegExp(resolved.value) };
    } catch {
      throw configurationError("invalid_regex", "Invalid regular expression", `${pointer}/${index}`);
    }
  });
}

function resolveEnum(value, environment, allowed, code, pointer) {
  const resolved = resolveString(value, environment, pointer);
  if (!allowed.has(resolved.value)) throw configurationError(code, "Invalid configuration value", pointer);
  return resolved;
}

function normalizeRecipientSet(value, environment, pointer) {
  const object = allowedObject(value, recipientSetKeys, pointer, ["exactly"]);
  return normalizeMailboxSet(object.exactly, environment, pathOf(pointer, "exactly"));
}

function normalizeCase(rawCase, environment, casePath, casePointer) {
  const item = allowedObject(rawCase, caseKeys, "", ["id", "send", "expect"]);
  const idPointer = pathOf(casePointer, "id");
  const id = resolveString(item.id, environment, idPointer);
  assertEvalIdentifier(id.value, "caseIdBytes", idPointer);
  const sendRaw = allowedObject(item.send, sendKeys, "/send", ["subject", "text"]);
  const send = {
    subject: resolveString(sendRaw.subject, environment, "/send/subject", MAX_SUBJECT_BYTES),
    text: resolveString(sendRaw.text, environment, "/send/text", MAX_SEND_TEXT_BYTES),
  };
  const expectRaw = allowedObject(item.expect, expectKeys, "/expect", ["action"]);
  const actionRaw = allowedObject(expectRaw.action, actionKeys, "/expect/action", ["kind", "count"]);
  const action = {
    kind: resolveEnum(actionRaw.kind, environment, actionKinds, "invalid_action_kind", "/expect/action/kind"),
    count: asNonnegativeInteger(actionRaw.count, "/expect/action/count"),
  };
  if (action.count > MAX_ACTION_COUNT) {
    throw configurationError("invalid_action_count", "Action count exceeds the bounded observation limit", "/expect/action/count");
  }
  if ((action.kind.value === "none") !== (action.count === 0)) {
    throw configurationError("invalid_action_count", "Action kind and count are inconsistent", "/expect/action");
  }

  const normalizedAction = { kind: action.kind.value, count: action.count };
  const expectation = { action: normalizedAction };
  const canonical = { id: id.source, send: { subject: send.subject.source, text: send.text.source }, expect: { action: { kind: action.kind.source, count: action.count } } };
  const sourceRecipients = [];

  if (expectRaw.sender !== undefined) {
    const senderRaw = allowedObject(expectRaw.sender, senderKeys, "/expect/sender");
    const sender = {};
    const senderCanonical = {};
    if (senderRaw.exactly !== undefined) {
      const normalized = normalizeScalarMailbox(senderRaw.exactly, environment, "/expect/sender/exactly");
      sender.exactly = normalized.mailbox.address;
      senderCanonical.exactly = normalized.canonical;
      sourceRecipients.push(normalized.mailbox.address);
    }
    if (senderRaw.sent_as !== undefined) {
      const normalized = normalizeSentAs(senderRaw.sent_as, environment, "/expect/sender/sent_as");
      sender.sentAs = normalized.value;
      senderCanonical.sentAs = normalized.source;
    }
    if (senderRaw.display_name !== undefined) {
      sender.displayName = resolveString(senderRaw.display_name, environment, "/expect/sender/display_name").value;
      senderCanonical.displayName = resolveString(senderRaw.display_name, environment, "/expect/sender/display_name").source;
    }
    if (senderRaw.reply_to !== undefined) {
      const replyRaw = allowedObject(senderRaw.reply_to, replyToKeys, "/expect/sender/reply_to", ["exactly"]);
      const set = normalizeMailboxSet(replyRaw.exactly, environment, "/expect/sender/reply_to/exactly");
      sender.replyTo = { exactly: set.addresses };
      senderCanonical.replyTo = { exactly: set.source };
      sourceRecipients.push(...set.addresses);
    }
    expectation.sender = sender;
    canonical.expect.sender = senderCanonical;
  }

  if (expectRaw.recipients !== undefined) {
    const recipientsRaw = allowedObject(expectRaw.recipients, recipientsKeys, "/expect/recipients");
    const recipients = {};
    const recipientsCanonical = {};
    for (const field of ["to", "cc", "bcc", "envelope"]) {
      if (recipientsRaw[field] === undefined) continue;
      const set = normalizeRecipientSet(recipientsRaw[field], environment, `/expect/recipients/${field}`);
      recipients[field] = { exactly: set.addresses };
      recipientsCanonical[field] = { exactly: set.source };
      sourceRecipients.push(...set.addresses);
    }
    expectation.recipients = recipients;
    canonical.expect.recipients = recipientsCanonical;
  }
  if (normalizedAction.kind !== "none" && !expectation.recipients?.envelope) {
    throw configurationError("missing_envelope_allowlist", "Outbound cases require an exact envelope expectation", "/expect/recipients/envelope");
  }

  if (expectRaw.thread !== undefined) {
    const threadRaw = allowedObject(expectRaw.thread, threadKeys, "/expect/thread");
    const thread = {};
    const threadCanonical = {};
    for (const [rawKey, normalizedKey, allowed] of [["message_id", "messageId", new Set(["required"])], ["in_reply_to", "inReplyTo", new Set(["original"])], ["references", "references", new Set(["contains_original"])], ["conversation", "conversation", new Set(["same"])]]) {
      if (threadRaw[rawKey] !== undefined) {
        const resolved = resolveEnum(threadRaw[rawKey], environment, allowed, "invalid_thread_expectation", `/expect/thread/${rawKey}`);
        thread[normalizedKey] = resolved.value;
        threadCanonical[normalizedKey] = resolved.source;
      }
    }
    expectation.thread = thread;
    canonical.expect.thread = threadCanonical;
  }

  if (expectRaw.subject !== undefined) {
    const subjectRaw = allowedObject(expectRaw.subject, subjectKeys, "/expect/subject");
    const subject = {};
    const subjectCanonical = {};
    for (const key of ["exact", "regex"]) {
      if (subjectRaw[key] !== undefined) {
        const resolved = resolveString(subjectRaw[key], environment, `/expect/subject/${key}`);
        if (key === "regex") normalizeRegexes([subjectRaw.regex], environment, "/expect/subject/regex");
        subject[key] = resolved.value;
        subjectCanonical[key] = resolved.source;
      }
    }
    if (subjectRaw.policy !== undefined) {
      const resolved = resolveEnum(subjectRaw.policy, environment, subjectPolicies, "invalid_subject_policy", "/expect/subject/policy");
      subject.policy = resolved.value;
      subjectCanonical.policy = resolved.source;
    }
    for (const key of ["required_fragments", "forbidden_fragments"]) {
      if (subjectRaw[key] !== undefined) {
        const values = asArray(subjectRaw[key], `/expect/subject/${key}`).map((entry, index) => resolveString(entry, environment, `/expect/subject/${key}/${index}`));
        const normalizedKey = key === "required_fragments" ? "requiredFragments" : "forbiddenFragments";
        subject[normalizedKey] = values.map((entry) => entry.value);
        subjectCanonical[normalizedKey] = values.map((entry) => entry.source);
      }
    }
    expectation.subject = subject;
    canonical.expect.subject = subjectCanonical;
  }

  if (expectRaw.body !== undefined) {
    const bodyRaw = allowedObject(expectRaw.body, bodyKeys, "/expect/body");
    const body = {};
    const bodyCanonical = {};
    for (const [rawKey, normalizedKey] of [["required_facts", "requiredFacts"]]) {
      if (bodyRaw[rawKey] !== undefined) {
        const values = asArray(bodyRaw[rawKey], `/expect/body/${rawKey}`).map((entry, index) => resolveString(entry, environment, `/expect/body/${rawKey}/${index}`));
        body[normalizedKey] = values.map((entry) => entry.value);
        bodyCanonical[normalizedKey] = values.map((entry) => entry.source);
      }
    }
    if (bodyRaw.forbidden_patterns !== undefined) {
      const values = normalizeRegexes(bodyRaw.forbidden_patterns, environment, "/expect/body/forbidden_patterns");
      body.forbiddenPatterns = values.map(({ value }) => value);
      // Compiled forms are intentionally non-enumerable: grading can reuse the
      // validation-time bounded regexes while suite artifacts remain JSON-safe.
      Object.defineProperty(body, "forbiddenPatternRegexes", { value: values.map(({ regex }) => regex) });
      bodyCanonical.forbiddenPatterns = values.map(({ source }) => source);
    }
    if (bodyRaw.plain_text !== undefined) {
      const resolved = resolveEnum(bodyRaw.plain_text, environment, plainTextPolicies, "invalid_plain_text_policy", "/expect/body/plain_text");
      body.plainText = resolved.value;
      bodyCanonical.plainText = resolved.source;
    }
    if (bodyRaw.html !== undefined) {
      const htmlRaw = allowedObject(bodyRaw.html, htmlKeys, "/expect/body/html", ["policy"]);
      const resolved = resolveEnum(htmlRaw.policy, environment, htmlPolicies, "invalid_html_policy", "/expect/body/html/policy");
      body.html = { policy: resolved.value };
      bodyCanonical.html = { policy: resolved.source };
    }
    if (bodyRaw.max_size !== undefined) {
      body.maxSize = asNonnegativeInteger(bodyRaw.max_size, "/expect/body/max_size");
      if (body.maxSize > MAX_MESSAGE_BYTES) {
        throw configurationError("invalid_body_size", "Body size exceeds the evidence limit", "/expect/body/max_size");
      }
      bodyCanonical.maxSize = body.maxSize;
    }
    expectation.body = body;
    canonical.expect.body = bodyCanonical;
  }

  if (expectRaw.attachments !== undefined) {
    const attachmentsRaw = allowedObject(expectRaw.attachments, attachmentsKeys, "/expect/attachments", ["exactly"]);
    const attachments = [];
    const attachmentsCanonical = [];
    for (const [index, attachment] of asArray(attachmentsRaw.exactly, "/expect/attachments/exactly", MAX_ATTACHMENTS).entries()) {
      if (typeof attachment === "string") {
        const resolved = resolveString(attachment, environment, `/expect/attachments/exactly/${index}`);
        attachments.push(resolved.value);
        attachmentsCanonical.push(resolved.source);
        continue;
      }
      const object = allowedObject(attachment, attachmentKeys, `/expect/attachments/exactly/${index}`);
      if (Object.keys(object).length === 0) {
        throw configurationError("invalid_attachment_expectation", "Attachment expectations must name at least one metadata field", `/expect/attachments/exactly/${index}`);
      }
      const normalized = {};
      const normalizedCanonical = {};
      for (const [rawKey, normalizedKey] of [["filename", "filename"], ["content_type", "contentType"], ["disposition", "disposition"], ["sha256", "sha256"]]) {
        if (object[rawKey] !== undefined) {
          const resolved = resolveString(object[rawKey], environment, `/expect/attachments/exactly/${index}/${rawKey}`);
          normalized[normalizedKey] = resolved.value;
          normalizedCanonical[normalizedKey] = resolved.source;
        }
      }
      if (object.size_bytes !== undefined) {
        normalized.sizeBytes = asNonnegativeInteger(object.size_bytes, `/expect/attachments/exactly/${index}/size_bytes`);
        if (normalized.sizeBytes > MAX_MESSAGE_BYTES) {
          throw configurationError("invalid_attachment_expectation", "Attachment size exceeds the evidence limit", `/expect/attachments/exactly/${index}/size_bytes`);
        }
        normalizedCanonical.sizeBytes = normalized.sizeBytes;
      }
      attachments.push(normalized);
      attachmentsCanonical.push(normalizedCanonical);
    }
    expectation.attachments = { exactly: attachments };
    canonical.expect.attachments = { exactly: attachmentsCanonical };
  }

  if (expectRaw.timing !== undefined) {
    const timingRaw = allowedObject(expectRaw.timing, timingKeys, "/expect/timing", ["reply_within"]);
    const duration = normalizeDuration(timingRaw.reply_within, environment, "/expect/timing/reply_within");
    expectation.timing = { replyWithinMs: duration.milliseconds };
    canonical.expect.timing = { replyWithin: duration.source };
  }
  if (expectRaw.lifecycle !== undefined) {
    const lifecycleRaw = allowedObject(expectRaw.lifecycle, lifecycleKeys, "/expect/lifecycle");
    const lifecycle = {};
    const lifecycleCanonical = {};
    if (lifecycleRaw.submission !== undefined) {
      const resolved = resolveEnum(lifecycleRaw.submission, environment, submissionStates, "invalid_submission_state", "/expect/lifecycle/submission");
      lifecycle.submission = resolved.value;
      lifecycleCanonical.submission = resolved.source;
    }
    if (lifecycleRaw.actor_received !== undefined) {
      lifecycle.actorReceived = asBoolean(lifecycleRaw.actor_received, "/expect/lifecycle/actor_received");
      lifecycleCanonical.actorReceived = lifecycle.actorReceived;
    }
    expectation.lifecycle = lifecycle;
    canonical.expect.lifecycle = lifecycleCanonical;
  }

  return { value: { id: id.value, send: { subject: send.subject.value, text: send.text.value }, expect: expectation, caseFile: casePath }, canonical, sourceRecipients };
}

function contained(root, candidate) {
  const relative = path.relative(root, candidate);
  return relative === "" || (!path.isAbsolute(relative) && relative !== ".." && !relative.startsWith(`..${path.sep}`));
}

const FILE_SNAPSHOT_FIELDS = Object.freeze([
  "dev", "ino", "mode", "nlink", "uid", "gid", "rdev", "size", "mtimeNs", "ctimeNs",
]);

function sameFileSnapshot(left, right) {
  return FILE_SNAPSHOT_FIELDS.every((field) => left[field] === right[field]);
}

function stableFileSnapshots(...snapshots) {
  return snapshots.every((snapshot) => snapshot.isFile())
    && snapshots.slice(1).every((snapshot) => sameFileSnapshot(snapshots[0], snapshot));
}

// The optional hooks are internal test seams: callers never receive file handles.
async function readYaml(file, label, {
  root, pointer, openFile = open, beforeRead, byteBudget = { remaining: MAX_YAML_TOTAL_BYTES },
} = {}) {
  let handle;
  try {
    handle = await openFile(file, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW | fsConstants.O_NONBLOCK);
    const handleBefore = await handle.stat({ bigint: true });
    if (!handleBefore.isFile()) throw configurationError("unsafe_file_type", "Configuration path is not a regular file", pointer);
    if (handleBefore.size > BigInt(MAX_YAML_FILE_BYTES) || handleBefore.size > BigInt(byteBudget.remaining)) {
      throw configurationError("yaml_too_large", "Configuration YAML exceeds its size limit", pointer);
    }
    const resolved = await realpath(file);
    if (root && !contained(root, resolved)) throw configurationError("path_outside_suite", "Case path is outside the suite root", pointer);
    const pathBefore = await stat(resolved, { bigint: true });
    if (!stableFileSnapshots(handleBefore, pathBefore)) {
      throw configurationError("file_changed_during_load", "Configuration file changed while loading", pointer);
    }
    await beforeRead?.({ file, label, resolved });
    const buffer = Buffer.allocUnsafe(Math.min(MAX_YAML_FILE_BYTES, byteBudget.remaining) + 1);
    let length = 0;
    while (length < buffer.length) {
      const { bytesRead } = await handle.read(buffer, length, buffer.length - length, length);
      if (bytesRead === 0) break;
      length += bytesRead;
    }
    let handleAfter;
    let resolvedAfter;
    let pathAfter;
    try {
      handleAfter = await handle.stat({ bigint: true });
      resolvedAfter = await realpath(file);
      pathAfter = await stat(resolvedAfter, { bigint: true });
    } catch {
      throw configurationError("file_changed_during_load", "Configuration file changed while loading", pointer);
    }
    if (resolvedAfter !== resolved || !stableFileSnapshots(handleBefore, pathBefore, handleAfter, pathAfter)) {
      throw configurationError("file_changed_during_load", "Configuration file changed while loading", pointer);
    }
    if (length > MAX_YAML_FILE_BYTES || length > byteBudget.remaining) {
      throw configurationError("yaml_too_large", "Configuration YAML exceeds its size limit", pointer);
    }
    byteBudget.remaining -= length;
    let source;
    try { source = new TextDecoder("utf-8", { fatal: true }).decode(buffer.subarray(0, length)); }
    catch { throw configurationError("invalid_yaml", "Suite YAML is invalid", pointer); }
    let document;
    try {
      document = parseDocument(source, { prettyErrors: true, strict: true, uniqueKeys: true, version: "1.2" });
      if (document.errors.length) {
        const duplicate = document.errors.some((error) => /unique|duplicate/i.test(error.message));
        throw configurationError(duplicate ? "duplicate_key" : "invalid_yaml", "Suite YAML is invalid");
      }
      return { value: document.toJS({ maxAliasCount: 50 }), resolved };
    } catch (error) {
      if (error instanceof EvalError) throw error;
      throw configurationError("invalid_yaml", "Suite YAML is invalid", pointer);
    }
  } catch (error) {
    if (error instanceof EvalError) throw error;
    throw configurationError(label === "suite" ? "suite_file_unreadable" : "case_file_unreadable", `Unable to read ${label} YAML`, pointer);
  } finally {
    await handle?.close();
  }
}

function stableJson(value) {
  if (Array.isArray(value)) return `[${value.map(stableJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${stableJson(value[key])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function freezeTree(value) {
  if (!value || typeof value !== "object" || Object.isFrozen(value)) return value;
  for (const entry of Object.values(value)) freezeTree(entry);
  return Object.freeze(value);
}

function environmentRedactions(canonical, environment, extraSources = []) {
  const names = new Set();
  const visit = (value) => {
    if (typeof value === "string") {
      const match = value.match(environmentReference);
      if (match) names.add(match[1]);
      return;
    }
    if (Array.isArray(value)) {
      for (const entry of value) visit(entry);
      return;
    }
    if (value && typeof value === "object") {
      for (const entry of Object.values(value)) visit(entry);
    }
  };
  visit(canonical);
  for (const source of extraSources) visit(source);
  return Object.freeze([...names].sort().map((name) => Object.freeze({ name, value: environment[name] })));
}

function aliasMailboxCanonical(value, aliases) {
  if (typeof value === "string") return value; // An unresolved ${NAME} reference.
  return { ...value, address: aliases.get(value.address) ?? value.address };
}

function aliasMailboxSetCanonical(values, aliases) {
  return values.map((value) => aliasMailboxCanonical(value, aliases));
}

function aliasCaseMailboxCanonical(testCase, aliases) {
  const expectation = { ...testCase.expect };
  if (expectation.sender) {
    expectation.sender = { ...expectation.sender };
    if (expectation.sender.exactly !== undefined) {
      expectation.sender.exactly = aliasMailboxCanonical(expectation.sender.exactly, aliases);
    }
    if (expectation.sender.replyTo) {
      expectation.sender.replyTo = { ...expectation.sender.replyTo, exactly: aliasMailboxSetCanonical(expectation.sender.replyTo.exactly, aliases) };
    }
  }
  if (expectation.recipients) {
    expectation.recipients = { ...expectation.recipients };
    for (const key of ["to", "cc", "bcc", "envelope"]) {
      if (expectation.recipients[key]) expectation.recipients[key] = {
        ...expectation.recipients[key],
        exactly: aliasMailboxSetCanonical(expectation.recipients[key].exactly, aliases),
      };
    }
  }
  return { ...testCase, expect: expectation };
}

export async function loadSuite(suiteFile, {
  environment = process.env, openFile, beforeRead, trustedOrigin,
} = {}) {
  const byteBudget = { remaining: MAX_YAML_TOTAL_BYTES };
  const requestedSuiteFile = path.resolve(suiteFile);
  const suiteDocument = await readYaml(requestedSuiteFile, "suite", { openFile, beforeRead, byteBudget });
  const resolvedSuiteFile = suiteDocument.resolved;
  const suiteRoot = path.dirname(resolvedSuiteFile);
  const rawSuite = suiteDocument.value;
  const suite = allowedObject(rawSuite, suiteKeys, "", ["version", "name", "target", "actor", "transport", "cases"]);
  if (suite.version !== 1) throw configurationError("unsupported_version", "Unsupported suite version", "/version");
  const name = resolveString(suite.name, environment, "/name");
  assertEvalIdentifier(name.value, "suiteNameBytes", "/name");
  const targetRaw = allowedObject(suite.target, identityKeys, "/target", ["email"]);
  const actorRaw = allowedObject(suite.actor, identityKeys, "/actor", ["email"]);
  const target = normalizeScalarMailbox(targetRaw.email, environment, "/target/email");
  const actor = normalizeScalarMailbox(actorRaw.email, environment, "/actor/email");
  if (target.mailbox.address === actor.mailbox.address) throw configurationError("same_actor_target", "Actor and target must differ", "/actor/email");

  const transportRaw = allowedObject(suite.transport, transportKeys, "/transport", ["adapter", "api_key", "allowed_envelope_recipients"]);
  const adapter = resolveString(transportRaw.adapter, environment, "/transport/adapter");
  if (adapter.value !== "e2a") throw configurationError("invalid_adapter", "Unsupported transport adapter", "/transport/adapter");
  const apiKey = resolveEnvironment(
    transportRaw.api_key,
    environment,
    "/transport/api_key",
    (name) => name === "E2A_EVAL_API_KEY",
  );
  if (apiKey.source !== "${E2A_EVAL_API_KEY}") {
    throw configurationError("api_key_environment_required", "API key must use E2A_EVAL_API_KEY", "/transport/api_key");
  }
  const baseUrl = transportRaw.base_url === undefined ? { source: "https://api.e2a.dev", value: "https://api.e2a.dev" } : resolveString(transportRaw.base_url, environment, "/transport/base_url");
  let parsedUrl;
  try { parsedUrl = new URL(baseUrl.value); } catch { throw configurationError("invalid_base_url", "Invalid base URL", "/transport/base_url"); }
  if (!["http:", "https:"].includes(parsedUrl.protocol)) throw configurationError("invalid_base_url", "Invalid base URL", "/transport/base_url");
  if (parsedUrl.username || parsedUrl.password || parsedUrl.search || parsedUrl.hash
    || (parsedUrl.pathname !== "" && parsedUrl.pathname !== "/")) {
    throw configurationError("invalid_base_url", "Base URL must be an origin", "/transport/base_url");
  }
  const origin = parsedUrl.origin;
  const isLoopback = new Set(["localhost", "127.0.0.1", "[::1]"]).has(parsedUrl.hostname);
  if (parsedUrl.protocol === "http:" && !isLoopback) {
    throw configurationError("insecure_base_url", "Cleartext base URLs are limited to loopback", "/transport/base_url");
  }
  if (origin !== "https://api.e2a.dev") {
    let trusted;
    try {
      const parsedTrusted = new URL(trustedOrigin);
      trusted = parsedTrusted.origin === trustedOrigin ? parsedTrusted.origin : null;
    } catch { trusted = null; }
    if (trusted !== origin) {
      throw configurationError("untrusted_base_url", "Custom base URL requires exact operator opt-in", "/transport/base_url");
    }
  }
  const allowedRecipients = normalizeMailboxSet(transportRaw.allowed_envelope_recipients, environment, "/transport/allowed_envelope_recipients");
  const allowedSet = new Set(allowedRecipients.addresses);
  if (!allowedSet.has(target.mailbox.address) || !allowedSet.has(actor.mailbox.address)) {
    throw configurationError("invalid_allowlist", "Allowlist must include actor and target", "/transport/allowed_envelope_recipients");
  }

  const defaultsRaw = suite.defaults === undefined ? {} : allowedObject(suite.defaults, defaultsKeys, "/defaults");
  const timeout = defaultsRaw.timeout === undefined ? { source: "60s", milliseconds: 60_000 } : normalizeDuration(defaultsRaw.timeout, environment, "/defaults/timeout");
  const settle = defaultsRaw.settle === undefined ? { source: "5s", milliseconds: 5_000 } : normalizeDuration(defaultsRaw.settle, environment, "/defaults/settle");
  const pollInterval = defaultsRaw.poll_interval === undefined ? { source: "500ms", milliseconds: 500 } : normalizeDuration(defaultsRaw.poll_interval, environment, "/defaults/poll_interval");
  if (pollInterval.milliseconds > timeout.milliseconds || settle.milliseconds > timeout.milliseconds) throw configurationError("invalid_duration", "Case timing is inconsistent", "/defaults");

  const rawCases = asArray(suite.cases, "/cases", MAX_CASES);
  if (rawCases.length === 0) throw configurationError("missing_cases", "Suite must contain at least one case", "/cases");
  const cases = [];
  const canonicalCases = [];
  const caseIds = new Set();
  for (let index = 0; index < rawCases.length; index += 1) {
    const reference = asString(rawCases[index], `/cases/${index}`, 4096);
    if (reference.includes("${")) throw configurationError("partial_environment_reference", "Case paths cannot contain environment interpolation", `/cases/${index}`);
    const candidate = path.resolve(suiteRoot, reference);
    if (!contained(suiteRoot, candidate)) throw configurationError("path_outside_suite", "Case path is outside the suite root", `/cases/${index}`);
    const caseDocument = await readYaml(candidate, "case", {
      root: suiteRoot, pointer: `/cases/${index}`, openFile, beforeRead, byteBudget,
    });
    const caseFile = caseDocument.resolved;
    const normalized = normalizeCase(caseDocument.value, environment, caseFile, `/cases/${index}`);
    if (normalized.value.expect.sender?.sentAs?.includes(apiKey.value)) {
      throw configurationError(
        "sent_as_conflicts_credential",
        "Sender sent_as must not contain the evaluation credential",
        `/cases/${index}/expect/sender/sent_as`,
      );
    }
    if (caseIds.has(normalized.value.id)) throw configurationError("duplicate_case_id", "Duplicate case identifier", `/cases/${index}`);
    caseIds.add(normalized.value.id);
    for (const recipient of normalized.sourceRecipients) {
      if (!allowedSet.has(recipient)) throw configurationError("recipient_outside_allowlist", "Case recipient is outside the transport allowlist", `/cases/${index}`);
    }
    cases.push(normalized.value);
    canonicalCases.push({ file: path.relative(suiteRoot, caseFile).split(path.sep).join("/"), ...normalized.canonical });
  }
  const plannedTimeoutMs = cases.length * timeout.milliseconds;
  if (!Number.isSafeInteger(plannedTimeoutMs) || plannedTimeoutMs > MAX_SUITE_EXECUTION_MS) {
    throw configurationError(
      "suite_timeout_budget_exceeded",
      "Suite case timeouts exceed the bounded public runtime budget",
      "/defaults/timeout",
    );
  }

  const aliases = new Map([[actor.mailbox.address, "actor"], [target.mailbox.address, "target"]]);
  allowedRecipients.addresses.filter((address) => !aliases.has(address)).forEach((address, index) => aliases.set(address, `probe:${index + 1}`));
  // Bind evidence to the resolved containment identities without serializing
  // addresses into plans or artifacts. Credentials deliberately stay out so a
  // key rotation does not invalidate otherwise identical captured evidence.
  const identityDigest = createHash("sha256").update(stableJson({
    actor: actor.mailbox.address,
    target: target.mailbox.address,
    allowedEnvelopeRecipients: [...allowedRecipients.addresses].sort(),
  })).digest("hex");
  const canonical = {
    version: 1,
    name: name.source,
    target: { email: aliasMailboxCanonical(target.canonical, aliases) },
    actor: { email: aliasMailboxCanonical(actor.canonical, aliases) },
    transport: {
      adapter: adapter.source,
      baseUrl: origin,
      allowedEnvelopeRecipients: aliasMailboxSetCanonical(allowedRecipients.source, aliases),
      identityDigest,
    },
    defaults: { timeout: timeout.source, settle: settle.source, pollInterval: pollInterval.source },
    cases: canonicalCases.map((testCase) => aliasCaseMailboxCanonical(testCase, aliases)),
  };
  const digest = createHash("sha256").update(stableJson(canonical)).digest("hex");
  const executionCanonical = {
    version: canonical.version,
    actor: canonical.actor,
    target: canonical.target,
    transport: canonical.transport,
    defaults: canonical.defaults,
    cases: canonical.cases.map((testCase, index) => ({
      id: testCase.id,
      send: testCase.send,
      expect: {
        action: { kind: testCase.expect.action.kind },
        ...(testCase.expect.lifecycle?.actorReceived === undefined ? {} : {
          lifecycle: { actorReceived: testCase.expect.lifecycle.actorReceived },
        }),
        ...(cases[index].expect.action.kind !== "new_message" || testCase.expect.subject === undefined
          ? {} : { subject: testCase.expect.subject }),
      },
    })),
  };
  const executionDigest = createHash("sha256").update(stableJson(executionCanonical)).digest("hex");

  const resolvedSuite = {
    version: 1,
    name: name.value,
    suiteFile: resolvedSuiteFile,
    suiteRoot,
    digest,
    executionDigest,
    target: { email: target.mailbox.address, displayName: target.mailbox.displayName },
    actor: { email: actor.mailbox.address, displayName: actor.mailbox.displayName },
    transport: {
      adapter: "e2a",
      apiKey: apiKey.value,
      baseUrl: origin,
      allowedEnvelopeRecipients: allowedRecipients.addresses,
    },
    defaults: { timeoutMs: timeout.milliseconds, settleMs: settle.milliseconds, pollIntervalMs: pollInterval.milliseconds },
    cases,
  };
  Object.defineProperty(resolvedSuite, RESOLVED_ENVIRONMENT_VALUES, {
    value: environmentRedactions(canonical, environment, [apiKey.source]),
    enumerable: false,
  });
  Object.defineProperty(resolvedSuite, RESOLVED_ENVIRONMENT_SOURCES, {
    value: freezeTree(canonical),
    enumerable: false,
  });
  return resolvedSuite;
}
