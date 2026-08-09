import { createHash } from "node:crypto";
import { readFile, realpath } from "node:fs/promises";
import path from "node:path";
import { parseDocument } from "yaml";
import { EvalError } from "./errors.mjs";
import { NormalizationError, normalizeAddressSet, normalizeMailbox, parseDuration } from "./normalize.mjs";

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
const threadKeys = new Set(["in_reply_to", "references", "conversation"]);
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

function configurationError(code, message, pointer) {
  return new EvalError("configuration_error", code, message, pointer ? { path: pointer } : undefined);
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

function asString(value, pointer) {
  if (typeof value !== "string" || value.length === 0) throw configurationError("invalid_schema", "Expected a non-empty string", pointer);
  return value;
}

function asArray(value, pointer) {
  if (!Array.isArray(value)) throw configurationError("invalid_schema", "Expected an array", pointer);
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

function resolveEnvironment(value, environment, pointer) {
  const source = asString(value, pointer);
  if (source.includes("${") && !environmentReference.test(source)) {
    throw configurationError("partial_environment_reference", "Environment references must occupy the complete scalar", pointer);
  }
  const match = source.match(environmentReference);
  if (!match) return { source, value: source };
  const resolved = environment?.[match[1]];
  if (typeof resolved !== "string" || resolved.length === 0) {
    throw configurationError("missing_environment", `Missing environment variable ${match[1]}`, pointer);
  }
  return { source, value: resolved };
}

function resolveString(value, environment, pointer) {
  return resolveEnvironment(value, environment, pointer);
}

function normalizeScalarMailbox(value, environment, pointer) {
  const resolved = resolveEnvironment(value, environment, pointer);
  try {
    return { ...resolved, mailbox: normalizeMailbox(resolved.value) };
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
    return { source: resolved.map((entry) => entry.source), addresses };
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

function normalizeRegexes(value, environment, pointer) {
  return asArray(value, pointer).map((entry, index) => {
    const resolved = resolveString(entry, environment, `${pointer}/${index}`);
    if (resolved.value.length > 512) throw configurationError("invalid_regex", "Regular expression exceeds the maximum length", `${pointer}/${index}`);
    try {
      return { source: resolved.source, value: resolved.value, regex: new RegExp(resolved.value) };
    } catch {
      throw configurationError("invalid_regex", "Invalid regular expression", `${pointer}/${index}`);
    }
  });
}

function validateEnum(value, allowed, code, pointer) {
  if (!allowed.has(value)) throw configurationError(code, "Invalid configuration value", pointer);
  return value;
}

function normalizeRecipientSet(value, environment, pointer) {
  const object = allowedObject(value, recipientSetKeys, pointer, ["exactly"]);
  return normalizeMailboxSet(object.exactly, environment, pathOf(pointer, "exactly"));
}

function normalizeCase(rawCase, environment, casePath) {
  const item = allowedObject(rawCase, caseKeys, "", ["id", "send", "expect"]);
  const id = asString(item.id, "/id");
  if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(id)) throw configurationError("invalid_case_id", "Invalid case identifier", "/id");
  const sendRaw = allowedObject(item.send, sendKeys, "/send", ["subject", "text"]);
  const send = {
    subject: resolveString(sendRaw.subject, environment, "/send/subject"),
    text: resolveString(sendRaw.text, environment, "/send/text"),
  };
  const expectRaw = allowedObject(item.expect, expectKeys, "/expect", ["action"]);
  const actionRaw = allowedObject(expectRaw.action, actionKeys, "/expect/action", ["kind", "count"]);
  const action = {
    kind: validateEnum(asString(actionRaw.kind, "/expect/action/kind"), actionKinds, "invalid_action_kind", "/expect/action/kind"),
    count: asNonnegativeInteger(actionRaw.count, "/expect/action/count"),
  };
  if ((action.kind === "none") !== (action.count === 0)) {
    throw configurationError("invalid_action_count", "Action kind and count are inconsistent", "/expect/action");
  }

  const expectation = { action };
  const canonical = { id, send: { subject: send.subject.source, text: send.text.source }, expect: { action } };
  const sourceRecipients = [];

  if (expectRaw.sender !== undefined) {
    const senderRaw = allowedObject(expectRaw.sender, senderKeys, "/expect/sender");
    const sender = {};
    const senderCanonical = {};
    if (senderRaw.exactly !== undefined) {
      const normalized = normalizeScalarMailbox(senderRaw.exactly, environment, "/expect/sender/exactly");
      sender.exactly = normalized.mailbox.address;
      senderCanonical.exactly = normalized.source;
      sourceRecipients.push(normalized.mailbox.address);
    }
    if (senderRaw.sent_as !== undefined) {
      const normalized = normalizeScalarMailbox(senderRaw.sent_as, environment, "/expect/sender/sent_as");
      sender.sentAs = normalized.mailbox.address;
      senderCanonical.sentAs = normalized.source;
      sourceRecipients.push(normalized.mailbox.address);
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
  if (action.count > 0 && !expectation.recipients?.envelope) {
    throw configurationError("missing_envelope_allowlist", "Outbound cases require an exact envelope expectation", "/expect/recipients/envelope");
  }

  if (expectRaw.thread !== undefined) {
    const threadRaw = allowedObject(expectRaw.thread, threadKeys, "/expect/thread");
    const thread = {};
    for (const [rawKey, normalizedKey, allowed] of [["in_reply_to", "inReplyTo", new Set(["original"])], ["references", "references", new Set(["contains_original"])], ["conversation", "conversation", new Set(["same"])]]) {
      if (threadRaw[rawKey] !== undefined) thread[normalizedKey] = validateEnum(asString(threadRaw[rawKey], `/expect/thread/${rawKey}`), allowed, "invalid_thread_expectation", `/expect/thread/${rawKey}`);
    }
    expectation.thread = thread;
    canonical.expect.thread = { ...thread };
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
      subject.policy = validateEnum(asString(subjectRaw.policy, "/expect/subject/policy"), subjectPolicies, "invalid_subject_policy", "/expect/subject/policy");
      subjectCanonical.policy = subject.policy;
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
      bodyCanonical.forbiddenPatterns = values.map(({ source }) => source);
    }
    if (bodyRaw.plain_text !== undefined) {
      body.plainText = validateEnum(asString(bodyRaw.plain_text, "/expect/body/plain_text"), plainTextPolicies, "invalid_plain_text_policy", "/expect/body/plain_text");
      bodyCanonical.plainText = body.plainText;
    }
    if (bodyRaw.html !== undefined) {
      const htmlRaw = allowedObject(bodyRaw.html, htmlKeys, "/expect/body/html", ["policy"]);
      body.html = { policy: validateEnum(asString(htmlRaw.policy, "/expect/body/html/policy"), htmlPolicies, "invalid_html_policy", "/expect/body/html/policy") };
      bodyCanonical.html = { ...body.html };
    }
    if (bodyRaw.max_size !== undefined) {
      body.maxSize = asNonnegativeInteger(bodyRaw.max_size, "/expect/body/max_size");
      bodyCanonical.maxSize = body.maxSize;
    }
    expectation.body = body;
    canonical.expect.body = bodyCanonical;
  }

  if (expectRaw.attachments !== undefined) {
    const attachmentsRaw = allowedObject(expectRaw.attachments, attachmentsKeys, "/expect/attachments", ["exactly"]);
    const attachments = asArray(attachmentsRaw.exactly, "/expect/attachments/exactly").map((attachment, index) => {
      if (typeof attachment === "string") return attachment;
      const object = allowedObject(attachment, attachmentKeys, `/expect/attachments/exactly/${index}`);
      const normalized = {};
      for (const [rawKey, normalizedKey] of [["filename", "filename"], ["content_type", "contentType"], ["disposition", "disposition"], ["sha256", "sha256"]]) {
        if (object[rawKey] !== undefined) normalized[normalizedKey] = asString(object[rawKey], `/expect/attachments/exactly/${index}/${rawKey}`);
      }
      if (object.size_bytes !== undefined) normalized.sizeBytes = asNonnegativeInteger(object.size_bytes, `/expect/attachments/exactly/${index}/size_bytes`);
      return normalized;
    });
    expectation.attachments = { exactly: attachments };
    canonical.expect.attachments = { exactly: attachments };
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
    if (lifecycleRaw.submission !== undefined) lifecycle.submission = validateEnum(asString(lifecycleRaw.submission, "/expect/lifecycle/submission"), submissionStates, "invalid_submission_state", "/expect/lifecycle/submission");
    if (lifecycleRaw.actor_received !== undefined) lifecycle.actorReceived = asBoolean(lifecycleRaw.actor_received, "/expect/lifecycle/actor_received");
    expectation.lifecycle = lifecycle;
    canonical.expect.lifecycle = { ...lifecycle };
  }

  return { value: { id, send: { subject: send.subject.value, text: send.text.value }, expect: expectation, caseFile: casePath }, canonical, sourceRecipients };
}

function contained(root, candidate) {
  const relative = path.relative(root, candidate);
  return relative === "" || (!path.isAbsolute(relative) && relative !== ".." && !relative.startsWith(`..${path.sep}`));
}

async function readYaml(file, label) {
  let source;
  try {
    source = await readFile(file, "utf8");
  } catch {
    throw configurationError("case_file_unreadable", `Unable to read ${label} YAML`);
  }
  const document = parseDocument(source, { prettyErrors: true, strict: true, uniqueKeys: true, version: "1.2" });
  if (document.errors.length) {
    const duplicate = document.errors.some((error) => /unique|duplicate/i.test(error.message));
    throw configurationError(duplicate ? "duplicate_key" : "invalid_yaml", "Suite YAML is invalid");
  }
  return document.toJS();
}

function stableJson(value) {
  if (Array.isArray(value)) return `[${value.map(stableJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${stableJson(value[key])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function aliasCanonical(value, aliases) {
  if (Array.isArray(value)) return value.map((item) => aliasCanonical(item, aliases));
  if (value && typeof value === "object") return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, aliasCanonical(item, aliases)]));
  if (typeof value === "string" && aliases.has(value.toLowerCase())) return aliases.get(value.toLowerCase());
  return value;
}

export async function loadSuite(suiteFile, { environment = process.env } = {}) {
  let resolvedSuiteFile;
  try {
    resolvedSuiteFile = await realpath(path.resolve(suiteFile));
  } catch {
    throw configurationError("suite_file_unreadable", "Unable to read suite YAML");
  }
  const suiteRoot = path.dirname(resolvedSuiteFile);
  const rawSuite = await readYaml(resolvedSuiteFile, "suite");
  const suite = allowedObject(rawSuite, suiteKeys, "", ["version", "name", "target", "actor", "transport", "cases"]);
  if (suite.version !== 1) throw configurationError("unsupported_version", "Unsupported suite version", "/version");
  const name = asString(suite.name, "/name");
  if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(name)) throw configurationError("invalid_suite_name", "Invalid suite name", "/name");
  const targetRaw = allowedObject(suite.target, identityKeys, "/target", ["email"]);
  const actorRaw = allowedObject(suite.actor, identityKeys, "/actor", ["email"]);
  const target = normalizeScalarMailbox(targetRaw.email, environment, "/target/email");
  const actor = normalizeScalarMailbox(actorRaw.email, environment, "/actor/email");
  if (target.mailbox.address === actor.mailbox.address) throw configurationError("same_actor_target", "Actor and target must differ", "/actor/email");

  const transportRaw = allowedObject(suite.transport, transportKeys, "/transport", ["adapter", "api_key", "allowed_envelope_recipients"]);
  if (transportRaw.adapter !== "e2a") throw configurationError("invalid_adapter", "Unsupported transport adapter", "/transport/adapter");
  const apiKey = resolveEnvironment(transportRaw.api_key, environment, "/transport/api_key");
  if (!environmentReference.test(apiKey.source)) throw configurationError("api_key_environment_required", "API key must be an environment reference", "/transport/api_key");
  const baseUrl = transportRaw.base_url === undefined ? { source: "https://api.e2a.dev", value: "https://api.e2a.dev" } : resolveString(transportRaw.base_url, environment, "/transport/base_url");
  let parsedUrl;
  try { parsedUrl = new URL(baseUrl.value); } catch { throw configurationError("invalid_base_url", "Invalid base URL", "/transport/base_url"); }
  if (!["http:", "https:"].includes(parsedUrl.protocol)) throw configurationError("invalid_base_url", "Invalid base URL", "/transport/base_url");
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

  const rawCases = asArray(suite.cases, "/cases");
  const cases = [];
  const canonicalCases = [];
  const caseIds = new Set();
  for (let index = 0; index < rawCases.length; index += 1) {
    const reference = asString(rawCases[index], `/cases/${index}`);
    if (reference.includes("${")) throw configurationError("partial_environment_reference", "Case paths cannot contain environment interpolation", `/cases/${index}`);
    const candidate = path.resolve(suiteRoot, reference);
    if (!contained(suiteRoot, candidate)) throw configurationError("path_outside_suite", "Case path is outside the suite root", `/cases/${index}`);
    let caseFile;
    try { caseFile = await realpath(candidate); } catch { throw configurationError("case_file_unreadable", "Unable to read case YAML", `/cases/${index}`); }
    if (!contained(suiteRoot, caseFile)) throw configurationError("path_outside_suite", "Case path is outside the suite root", `/cases/${index}`);
    const normalized = normalizeCase(await readYaml(caseFile, "case"), environment, caseFile);
    if (caseIds.has(normalized.value.id)) throw configurationError("duplicate_case_id", "Duplicate case identifier", `/cases/${index}`);
    caseIds.add(normalized.value.id);
    for (const recipient of normalized.sourceRecipients) {
      if (!allowedSet.has(recipient)) throw configurationError("recipient_outside_allowlist", "Case recipient is outside the transport allowlist", `/cases/${index}`);
    }
    cases.push(normalized.value);
    canonicalCases.push({ file: path.relative(suiteRoot, caseFile), ...normalized.canonical });
  }

  const aliases = new Map([[actor.mailbox.address, "actor"], [target.mailbox.address, "target"]]);
  allowedRecipients.addresses.filter((address) => !aliases.has(address)).forEach((address, index) => aliases.set(address, `probe:${index + 1}`));
  const canonical = aliasCanonical({
    version: 1,
    name,
    target: { email: target.source },
    actor: { email: actor.source },
    transport: { adapter: "e2a", baseUrl: baseUrl.source, allowedEnvelopeRecipients: allowedRecipients.source },
    defaults: { timeout: timeout.source, settle: settle.source, pollInterval: pollInterval.source },
    cases: canonicalCases,
  }, aliases);
  const digest = createHash("sha256").update(stableJson(canonical)).digest("hex");

  return {
    version: 1,
    name,
    suiteFile: resolvedSuiteFile,
    suiteRoot,
    digest,
    target: { email: target.mailbox.address, displayName: target.mailbox.displayName },
    actor: { email: actor.mailbox.address, displayName: actor.mailbox.displayName },
    transport: {
      adapter: "e2a",
      apiKey: apiKey.value,
      baseUrl: baseUrl.value,
      allowedEnvelopeRecipients: allowedRecipients.addresses,
    },
    defaults: { timeoutMs: timeout.milliseconds, settleMs: settle.milliseconds, pollIntervalMs: pollInterval.milliseconds },
    cases,
  };
}
