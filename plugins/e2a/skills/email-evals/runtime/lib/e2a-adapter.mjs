import { createHash } from "node:crypto";
import { E2AClient, E2AConnectionError } from "@e2a/sdk/v1";
import { executionBounds } from "./contract.mjs";
import { EvalError, isStableEvalErrorCode } from "./errors.mjs";
import { normalizeMessageIdToken, parseMimeEvidence } from "./mime.mjs";
import {
  NormalizationError, normalizeAddressSet, normalizeMailbox, normalizeMailboxHeader, replaceMailboxText,
} from "./normalize.mjs";

const CAPABILITIES = Object.freeze([
  "message_action",
  "visible_recipients",
  "blind_recipients",
  "envelope_recipients",
  "thread_headers",
  "raw_mime",
  "attachment_hashes",
  "delivery_lifecycle",
]);

const TIMEOUTS = Object.freeze({ maxRetries: 2, maxElapsedMs: 15_000, timeoutMs: 10_000 });
const MIME_CASE_BUDGET_BYTES = 25 * 1024 * 1024;
const SENT_AS_TOKEN = /^[a-z][a-z0-9_]{0,63}$/;
const RELAY_DOMAIN = /^(?=.{1,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
const MAX_RELAY_FROM_BYTES = 998;

class ReadonlyCapabilitySet {
  #values;
  #members;

  constructor(values) {
    this.#values = Object.freeze([...values]);
    this.#members = new Set(this.#values);
    Object.freeze(this);
  }

  get size() {
    return this.#values.length;
  }

  has(value) {
    return this.#members.has(value);
  }

  values() {
    return this.#values.values();
  }

  keys() {
    return this.values();
  }

  entries() {
    return this.#values.map((value) => [value, value]).values();
  }

  forEach(callback, thisArg) {
    for (const value of this.#values) callback.call(thisArg, value, value, this);
  }

  [Symbol.iterator]() {
    return this.values();
  }

  toArray() {
    return [...this.#values];
  }

  toJSON() {
    return this.toArray();
  }
}

// Instances intentionally share this tiny read-only surface. Freeze both the
// prototype and constructor so an untrusted caller cannot replace a method on
// the shared prototype and alter existing or future preflight results.
Object.freeze(ReadonlyCapabilitySet.prototype);
Object.freeze(ReadonlyCapabilitySet);

function configurationError(code, message) {
  return new EvalError("configuration_error", code, message);
}

function transportError(code, message) {
  if (!isStableEvalErrorCode("transport_error", code)) {
    throw new TypeError(`Unregistered evaluation transport error code: ${code}`);
  }
  return new EvalError("transport_error", code, message);
}

function stableJson(value) {
  if (Array.isArray(value)) return `[${value.map(stableJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${stableJson(value[key])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function serializableBaseUrl(value) {
  let parsed;
  try {
    parsed = new URL(value ?? "https://api.e2a.dev");
  } catch {
    throw configurationError("invalid_base_url", "Invalid evaluation API base URL");
  }
  if (!['http:', 'https:'].includes(parsed.protocol)) {
    throw configurationError("invalid_base_url", "Invalid evaluation API base URL");
  }
  // The SDK receives the original configured endpoint. Only the copy rendered
  // in dry-run artifacts drops components that commonly contain credentials.
  parsed.username = "";
  parsed.password = "";
  parsed.search = "";
  parsed.hash = "";
  return parsed.origin;
}

function normalizeAddress(value, code = "invalid_mailbox") {
  try {
    return normalizeMailbox(value).address;
  } catch (error) {
    if (error instanceof NormalizationError) throw configurationError(code, "Invalid evaluation mailbox");
    throw error;
  }
}

function normalizeAddressList(value, code) {
  try {
    return normalizeAddressSet(value);
  } catch (error) {
    if (error instanceof NormalizationError) throw configurationError(code, "Invalid evaluation address set");
    throw error;
  }
}

function isNotFound(error) {
  return error?.status === 404 || error?.code === "not_found" || error?.code === "agent_not_found";
}

async function readOwnedAgent(client, email) {
  try {
    const agent = await client.agents.get(email);
    if (!agent || normalizeAddress(agent.email, "agent_identity_mismatch") !== email) {
      throw configurationError("agent_identity_mismatch", "Evaluation agent identity did not match");
    }
    return agent;
  } catch (error) {
    if (error instanceof EvalError) throw error;
    if (isNotFound(error)) throw configurationError("agent_not_found", "A dedicated evaluation agent is unavailable");
    throw transportError("agent_lookup_failed", "Unable to read dedicated evaluation agent");
  }
}

async function readProtection(client, email) {
  try {
    return await client.agents.getProtection(email);
  } catch {
    // The endpoint is account-scope only. Do not disclose the address or the
    // SDK/server error, either of which could distinguish account state.
    throw configurationError("account_scope_required", "Account-scoped access is required to read evaluation protection");
  }
}

function normalizeGate(document, gateCode) {
  const gate = document?.outbound?.gate;
  if (!gate || typeof gate !== "object") throw configurationError(gateCode, "Outbound recipient gate is not configured exactly");
  if (gate.policy !== "allowlist") throw configurationError(gateCode, "Outbound recipient gate is not configured exactly");
  if (gate.action !== "block") throw configurationError(gateCode.replace("not_exact", "not_blocking"), "Outbound recipient gate must block non-matches");
  return {
    policy: gate.policy,
    action: gate.action,
    allowlist: normalizeAddressList(gate.allowlist, gateCode),
  };
}

function sameSet(left, right) {
  return left.length === right.length && left.every((entry, index) => entry === right[index]);
}

function aliasesFor(actor, target, probes) {
  const aliases = new Map([[actor, "actor"], [target, "target"]]);
  probes.forEach((probe, index) => aliases.set(probe, `probe:${index + 1}`));
  return aliases;
}

function aliasOf(aliases, address) {
  const alias = aliases.get(address);
  if (!alias) throw configurationError("recipient_outside_allowlist", "Resolved recipient is outside the transport allowlist");
  return alias;
}

function exactlyAddresses(value, code = "invalid_suite") {
  if (!value || typeof value !== "object" || !Array.isArray(value.exactly)) {
    throw configurationError(code, "Expected exact recipient set");
  }
  return normalizeAddressList(value.exactly, code);
}

function collectCaseAddresses(testCase) {
  if (!testCase || typeof testCase !== "object" || typeof testCase.id !== "string" || !testCase.expect || typeof testCase.expect !== "object") {
    throw configurationError("invalid_suite", "Resolved evaluation case is invalid");
  }
  const action = testCase.expect.action;
  if (!action || typeof action !== "object" || typeof action.kind !== "string"
    || !Number.isSafeInteger(action.count) || action.count < 0 || action.count > OBSERVATION_LIMIT) {
    throw configurationError("invalid_suite", "Resolved case action is invalid");
  }

  const addresses = [];
  const sender = testCase.expect.sender;
  if (sender !== undefined) {
    if (!sender || typeof sender !== "object") throw configurationError("invalid_suite", "Resolved case sender is invalid");
    if (sender.exactly !== undefined) addresses.push(normalizeAddress(sender.exactly, "invalid_suite"));
    if (sender.sentAs !== undefined && !SENT_AS_TOKEN.test(sender.sentAs)) {
      throw configurationError("invalid_suite", "Resolved sent-as expectation is invalid");
    }
    if (sender.replyTo !== undefined) addresses.push(...exactlyAddresses(sender.replyTo));
  }
  const recipients = testCase.expect.recipients;
  if (recipients !== undefined) {
    if (!recipients || typeof recipients !== "object") throw configurationError("invalid_suite", "Resolved case recipients are invalid");
    for (const field of ["to", "cc", "bcc", "envelope"]) {
      if (recipients[field] !== undefined) addresses.push(...exactlyAddresses(recipients[field]));
    }
  }
  if (action.kind !== "none" && recipients?.envelope === undefined) {
    throw configurationError("missing_envelope_allowlist", "Outbound cases require an exact envelope expectation");
  }
  return {
    id: testCase.id,
    action: { kind: action.kind, count: action.count },
    addresses: [...new Set(addresses)].sort(),
    source: testCase,
  };
}

function planString(value, aliases, apiKey) {
  let result = replaceMailboxText(value, (mailbox) => aliases.get(mailbox.address) ?? "[REDACTED:address]");
  if (typeof apiKey === "string" && apiKey.length > 0) {
    result = result.replaceAll(apiKey, "[REDACTED:credential]");
  }
  return result
    .replace(/[\u0000-\u001f\u007f]/g, "[REDACTED:control]")
    .replace(/\b(?:sk|e2a)_[A-Za-z0-9_-]+\b/g, "[REDACTED:credential]")
    .replace(/@/g, "[REDACTED:address]");
}

function planSentAs(value, apiKey) {
  if (typeof apiKey === "string" && apiKey.length > 0 && value.includes(apiKey)) {
    return "[REDACTED:credential]";
  }
  return SENT_AS_TOKEN.test(value) ? value : "[REDACTED:credential]";
}

function planValue(value, aliases, apiKey) {
  if (typeof value === "string") return planString(value, aliases, apiKey);
  if (Array.isArray(value)) return value.map((entry) => planValue(entry, aliases, apiKey));
  if (!value || typeof value !== "object") return value;
  return Object.fromEntries(Object.entries(value).map(([key, entry]) => [key, planValue(entry, aliases, apiKey)]));
}

function planAddresses(value, aliases) {
  if (value === undefined) return null;
  return exactlyAddresses(value).map((address) => aliasOf(aliases, address));
}

function caseAssertionPlan(testCase, aliases, apiKey) {
  const expectation = testCase.expect;
  const assertions = [];
  const add = (id, expected) => assertions.push({ id, expected: planValue(expected, aliases, apiKey) });
  add("action.kind", expectation.action.kind);
  add("action.count", expectation.action.count);
  add("action.no_duplicates", expectation.action.count);

  if (expectation.sender?.exactly !== undefined) {
    add("sender.from", aliasOf(aliases, normalizeAddress(expectation.sender.exactly, "invalid_suite")));
  }
  if (expectation.sender?.sentAs !== undefined) {
    assertions.push({ id: "sender.sent_as", expected: planSentAs(expectation.sender.sentAs, apiKey) });
  }
  if (expectation.sender?.replyTo !== undefined) add("sender.reply_to", planAddresses(expectation.sender.replyTo, aliases));
  if (expectation.sender?.displayName !== undefined) add("sender.display_name", expectation.sender.displayName);

  if (expectation.recipients !== undefined) {
    for (const field of ["to", "cc", "bcc", "envelope"]) {
      if (expectation.recipients[field] !== undefined) {
        add(`recipients.${field}`, planAddresses(expectation.recipients[field], aliases));
      }
    }
    add("recipients.cross_field", "same recipient fields");
    add("recipients.no_target_self", "target");
  }

  for (const [field, id] of [
    ["messageId", "thread.message_id"], ["inReplyTo", "thread.in_reply_to"],
    ["references", "thread.references"], ["conversation", "thread.conversation"],
  ]) {
    if (expectation.thread?.[field] !== undefined) add(id, expectation.thread[field]);
  }
  for (const [field, id] of [
    ["exact", "subject.exact"], ["regex", "subject.regex"], ["policy", "subject.policy"],
    ["requiredFragments", "subject.required_fragments"], ["forbiddenFragments", "subject.forbidden_fragments"],
  ]) {
    if (expectation.subject?.[field] !== undefined) add(id, expectation.subject[field]);
  }
  if (expectation.subject !== undefined) add("subject.no_header_injection", "safe headers");
  for (const [field, id] of [
    ["requiredFacts", "body.required_facts"], ["forbiddenPatterns", "body.forbidden_patterns"],
    ["plainText", "body.plain_text"], ["maxSize", "body.max_size"],
  ]) {
    if (expectation.body?.[field] !== undefined) add(id, expectation.body[field]);
  }
  if (expectation.attachments?.exactly !== undefined) add("attachments.exactly", expectation.attachments.exactly);
  if (expectation.timing?.replyWithinMs !== undefined) add("timing.reply_within", expectation.timing.replyWithinMs);
  if (expectation.lifecycle?.submission !== undefined) add("lifecycle.submission", expectation.lifecycle.submission);
  if (expectation.lifecycle?.actorReceived !== undefined) add("lifecycle.actor_received", expectation.lifecycle.actorReceived);
  return assertions;
}

function caseCapabilities(testCase) {
  const expectation = testCase.expect;
  const required = new Set();
  if (expectation.action) required.add("message_action");
  if (expectation.sender || expectation.recipients?.to || expectation.recipients?.cc) required.add("visible_recipients");
  if (expectation.recipients?.bcc) required.add("blind_recipients");
  if (expectation.recipients?.envelope) required.add("envelope_recipients");
  if (expectation.thread) required.add("thread_headers");
  if (expectation.subject || expectation.body) required.add("raw_mime");
  if (expectation.attachments) required.add("attachment_hashes");
  if (expectation.lifecycle) required.add("delivery_lifecycle");
  return CAPABILITIES.filter((capability) => required.has(capability));
}

function makePlan({ baseUrl, aliases, allowedAliases, cases, apiKey, defaults }) {
  const plannedTimeoutMs = cases.reduce((total, testCase) => (
    total + (testCase.source.timeoutMs ?? defaults?.timeoutMs ?? 0)
  ), 0);
  if (!Number.isSafeInteger(plannedTimeoutMs) || plannedTimeoutMs <= 0
    || plannedTimeoutMs > executionBounds.maxSuiteTimeoutMs) {
    throw configurationError("suite_timeout_budget_exceeded", "Suite case timeouts exceed the bounded public runtime budget");
  }
  return {
    baseUrl,
    recipientAliases: allowedAliases,
    capabilities: [...CAPABILITIES],
    timeouts: { ...TIMEOUTS },
    executionBudget: {
      plannedTimeoutMs,
      maximumTimeoutMs: executionBounds.maxSuiteTimeoutMs,
    },
    networkSends: false,
    cases: cases.map((collected) => {
      const testCase = collected.source;
      const sender = testCase.expect.sender;
      const recipients = testCase.expect.recipients;
      return {
        id: collected.id,
        stimulus: {
          action: "send",
          sender: "actor",
          recipients: ["target"],
          subject: planString(testCase.send.subject, aliases, apiKey),
          text: planString(testCase.send.text, aliases, apiKey),
        },
        expectedAction: collected.action,
        expectedSender: {
          from: sender?.exactly === undefined ? null : aliasOf(aliases, normalizeAddress(sender.exactly, "invalid_suite")),
          sentAs: sender?.sentAs === undefined ? null : planSentAs(sender.sentAs, apiKey),
          replyTo: sender?.replyTo === undefined ? null : planAddresses(sender.replyTo, aliases),
          displayName: sender?.displayName === undefined ? null : planString(sender.displayName, aliases, apiKey),
        },
        expectedRecipients: Object.fromEntries(["to", "cc", "bcc", "envelope"].map((field) => [
          field, recipients?.[field] === undefined ? null : planAddresses(recipients[field], aliases),
        ])),
        recipientAliases: collected.addresses.map((address) => aliasOf(aliases, address)),
        assertions: caseAssertionPlan(testCase, aliases, apiKey),
        evidenceCapabilities: caseCapabilities(testCase),
        semanticGraders: [],
      };
    }),
  };
}

const OBSERVATION_LIMIT = 100;
const OUTBOUND_TERMINAL_EVENTS = new Set([
  "email.sent",
  "email.failed",
  "email.blocked",
  "email.review_requested",
]);

function instant(value, code = "invalid_timestamp") {
  const milliseconds = value instanceof Date ? value.getTime()
    : typeof value === "number" ? value
      : typeof value === "string" ? Date.parse(value) : Number.NaN;
  if (!Number.isFinite(milliseconds)) throw transportError(code, "Evaluation observation contained an invalid timestamp");
  return { milliseconds, iso: new Date(milliseconds).toISOString() };
}

function clockInstant(now) {
  try {
    return instant(now(), "invalid_clock");
  } catch (error) {
    if (error instanceof EvalError) throw error;
    throw transportError("invalid_clock", "Evaluation clock returned an invalid instant");
  }
}

function strings(value, { optional = false } = {}) {
  if (value === undefined && optional) return [];
  if (!Array.isArray(value) || value.some((entry) => typeof entry !== "string")) {
    throw transportError("malformed_event", "Evaluation event recipient evidence is malformed");
  }
  return [...value];
}

function dataOf(event) {
  if (!event?.data || typeof event.data !== "object" || Array.isArray(event.data)) {
    throw transportError("malformed_event", "Evaluation event payload is malformed");
  }
  return event.data;
}

function eventInstant(event) {
  return instant(event?.createdAt, "malformed_event");
}

function eventIdentity(event) {
  if (typeof event?.id !== "string" || event.id.length === 0 || typeof event?.type !== "string") {
    throw transportError("malformed_event", "Evaluation event envelope is malformed");
  }
  return event.id;
}

function sameCanonical(left, right) {
  const canonicalEvent = (event) => ({
    id: event.id,
    type: event.type,
    schemaVersion: event.schemaVersion,
    status: event.status,
    createdAt: eventInstant(event).iso,
    agentEmail: event.agentEmail,
    conversationId: event.conversationId,
    messageId: event.messageId,
    data: event.data,
  });
  return stableJson(canonicalEvent(left)) === stableJson(canonicalEvent(right));
}

function uniqueEvents(events) {
  const byRef = new Map();
  for (const event of events) {
    const ref = eventIdentity(event);
    const prior = byRef.get(ref);
    if (prior && !sameCanonical(prior, event)) {
      throw transportError("conflicting_event_ref", "Evaluation event reference carried conflicting evidence");
    }
    if (!prior) byRef.set(ref, event);
  }
  return [...byRef.values()].sort((left, right) => eventInstant(left).iso.localeCompare(eventInstant(right).iso)
    || left.id.localeCompare(right.id));
}

async function boundedItems(source, code) {
  let resolved;
  try {
    resolved = await source;
  } catch (error) {
    throw error;
  }
  let items;
  if (Array.isArray(resolved)) {
    items = resolved.slice(0, OBSERVATION_LIMIT + 1);
  } else if (resolved && typeof resolved.toArray === "function") {
    items = await resolved.toArray({ limit: OBSERVATION_LIMIT + 1 });
  } else if (resolved && typeof resolved[Symbol.asyncIterator] === "function") {
    items = [];
    for await (const item of resolved) {
      items.push(item);
      if (items.length > OBSERVATION_LIMIT) break;
    }
  } else {
    throw transportError("malformed_page", "Evaluation list response is not iterable");
  }
  if (!Array.isArray(items)) throw transportError("malformed_page", "Evaluation list response is malformed");
  if (items.length > OBSERVATION_LIMIT) throw transportError(code, "Evaluation observation exceeded its bounded result limit");
  return items;
}

function evidenceString(value) {
  return typeof value === "string" && value.length > 0 ? value : null;
}

const ABSENT_IDENTITY = Symbol("absent-identity");

function identityField(source, key) {
  return source !== null && typeof source === "object" && Object.hasOwn(source, key)
    ? source[key] : ABSENT_IDENTITY;
}

function reconciledIdentity(kind, values, { required = false, authoritative = null } = {}) {
  const observed = [];
  for (const value of values) {
    // Generated SDK views materialize omitted optional message/conversation
    // class fields as own undefined values, while the API also uses an empty
    // pre-assignment string. Those exact states are absent. Explicit null and
    // every other defined value must satisfy the strict string contract.
    if (value === ABSENT_IDENTITY || value === undefined || value === "") continue;
    if (typeof value !== "string") {
      throw transportError("malformed_event", `Evaluation ${kind} reference is malformed`);
    }
    observed.push(value);
  }
  if (new Set(observed).size > 1) {
    throw transportError("conflicting_evidence", `Evaluation ${kind} references conflict`);
  }
  if (required && observed.length === 0) {
    throw transportError("malformed_event", `Evaluation event omitted its ${kind} reference`);
  }
  return authoritative === ABSENT_IDENTITY || authoritative === undefined || authoritative === ""
    ? observed[0] ?? null : authoritative;
}

function messageIdOf(event, data, message, { required = false } = {}) {
  return reconciledIdentity("message", [
    identityField(event, "messageId"), identityField(data, "message_id"), identityField(message, "id"),
  ], {
    required,
    authoritative: identityField(message, "id"),
  });
}

function conversationIdOf(event, data, message) {
  return reconciledIdentity("conversation", [
    identityField(event, "conversationId"), identityField(data, "conversation_id"),
    identityField(message, "conversationId"),
  ], {
    authoritative: identityField(message, "conversationId"),
  });
}

function normalizedAgent(value) {
  if (typeof value !== "string" || value.length === 0) {
    throw transportError("malformed_event", "Evaluation mailbox identity is malformed");
  }
  try {
    return normalizeMailbox(value).address;
  } catch {
    throw transportError("malformed_event", "Evaluation mailbox identity is malformed");
  }
}

function eventIsFor(event, expectedAgent, direction) {
  const data = dataOf(event);
  const envelopeIdentity = identityField(event, "agentEmail");
  const payloadIdentity = identityField(data, "agent_email");
  const envelopeAgent = envelopeIdentity === ABSENT_IDENTITY ? null : normalizedAgent(envelopeIdentity);
  if (payloadIdentity === ABSENT_IDENTITY) {
    throw transportError("malformed_event", "Evaluation event omitted its mailbox identity");
  }
  const payloadAgent = normalizedAgent(payloadIdentity);
  if (envelopeAgent !== null && envelopeAgent !== payloadAgent) {
    throw transportError("conflicting_evidence", "Evaluation event mailbox identities conflict");
  }
  if (typeof data.direction !== "string" || !["inbound", "outbound"].includes(data.direction)) {
    throw transportError("malformed_event", "Evaluation event direction is malformed");
  }
  if (payloadAgent !== expectedAgent) return false;
  if (data.direction !== direction) {
    throw transportError("conflicting_evidence", "Evaluation event direction conflicts with its event type");
  }
  return true;
}

function stableEnvelopeRecipients(to, cc, bcc) {
  const seen = new Set();
  const result = [];
  for (const value of [...to, ...cc, ...bcc]) {
    let key = value;
    try { key = normalizeMailbox(value).address; } catch { /* preserve malformed evidence for fail-closed grading */ }
    if (seen.has(key)) continue;
    seen.add(key);
    result.push(key);
  }
  return result;
}

function safeTransition(transition) {
  if (!transition || typeof transition !== "object" || Array.isArray(transition)) {
    throw transportError("malformed_lifecycle", "Evaluation lifecycle evidence is malformed");
  }
  const occurredAt = instant(transition.occurredAt, "malformed_lifecycle").iso;
  return {
    id: typeof transition.id === "string" ? transition.id : null,
    messageId: typeof transition.messageId === "string" ? transition.messageId : null,
    direction: typeof transition.direction === "string" ? transition.direction : null,
    stage: typeof transition.stage === "string" ? transition.stage : null,
    outcome: typeof transition.outcome === "string" ? transition.outcome : null,
    reasonCode: typeof transition.reasonCode === "string" ? transition.reasonCode : null,
    retryable: transition.retryable === true,
    reconstructed: transition.reconstructed === true,
    recipient: typeof transition.recipient === "string" ? transition.recipient : null,
    occurredAt,
  };
}

function submissionFor(eventType) {
  switch (eventType) {
    case "email.sent": return "sent";
    case "email.failed": return "failed";
    case "email.blocked": return "blocked";
    case "email.review_requested": return "pending_review";
    default: return null;
  }
}

async function readLifecycle(sdk, target, messageId) {
  const page = await sdk.messages.getLifecycle(target, messageId, { limit: OBSERVATION_LIMIT });
  if (!page || !Array.isArray(page.items)) throw transportError("malformed_lifecycle", "Evaluation lifecycle response is malformed");
  if (page.items.length > OBSERVATION_LIMIT || page.nextCursor) {
    throw transportError("observation_limit_exceeded", "Evaluation lifecycle exceeded its bounded result limit");
  }
  return page.items.map(safeTransition);
}

function validateMessage(message, messageId, direction) {
  if (!message || typeof message !== "object" || message.id !== messageId || message.direction !== direction) {
    throw transportError("message_identity_mismatch", "Evaluation message did not match its durable event reference");
  }
  return message;
}

function reconcileText(kind, values, { required = false } = {}) {
  const observed = [];
  for (const value of values) {
    if (value === undefined || value === null) continue;
    if (typeof value !== "string") throw transportError("malformed_message", `Evaluation ${kind} evidence is malformed`);
    observed.push(value);
  }
  if (required && observed.length === 0) throw transportError("malformed_message", `Evaluation ${kind} evidence is missing`);
  if (new Set(observed).size > 1) throw transportError("conflicting_evidence", `Evaluation ${kind} representations conflict`);
  return observed[0] ?? null;
}

function reconcileMailbox(kind, values, { required = false, preferred = null } = {}) {
  const observed = [];
  for (const value of values) {
    if (value === undefined || value === null) continue;
    observed.push({ source: value, address: normalizedAgent(value) });
  }
  if (required && observed.length === 0) throw transportError("malformed_message", `Evaluation ${kind} evidence is missing`);
  if (new Set(observed.map((entry) => entry.address)).size > 1) {
    throw transportError("conflicting_evidence", `Evaluation ${kind} representations conflict`);
  }
  return preferred ?? observed[0]?.source ?? null;
}

function normalizedEvidenceAddresses(kind, value, { optional = false } = {}) {
  if (value === undefined || value === null) {
    if (optional) return null;
    throw transportError("malformed_message", `Evaluation ${kind} evidence is missing`);
  }
  if (!Array.isArray(value)) throw transportError("malformed_message", `Evaluation ${kind} evidence is malformed`);
  return value.map((entry) => normalizedAgent(entry)).sort();
}

function reconcileAddressSources(kind, primary, redundant) {
  const left = normalizedEvidenceAddresses(kind, primary);
  if (redundant === undefined || redundant === null) return primary;
  const right = normalizedEvidenceAddresses(kind, redundant);
  if (!sameSet(left, right)) throw transportError("conflicting_evidence", `Evaluation ${kind} representations conflict`);
  return primary;
}

function reconcileSentAs(values) {
  const observed = [];
  for (const value of values) {
    if (value === undefined || value === null) continue;
    if (typeof value !== "string" || !SENT_AS_TOKEN.test(value)) {
      throw transportError("malformed_message", "Evaluation sent-as evidence is malformed");
    }
    observed.push(value);
  }
  if (new Set(observed).size > 1) throw transportError("conflicting_evidence", "Evaluation sent-as representations conflict");
  return observed[0] ?? null;
}

function normalizedRelayPhysicalFrom(value) {
  if (typeof value !== "string" || Buffer.byteLength(value, "utf8") > MAX_RELAY_FROM_BYTES) {
    throw transportError("malformed_message", "Evaluation relay sender evidence is malformed");
  }
  let mailbox;
  try {
    mailbox = normalizeMailboxHeader(value);
  } catch {
    throw transportError("malformed_message", "Evaluation relay sender evidence is malformed");
  }
  const separator = mailbox.address.lastIndexOf("@");
  const localPart = mailbox.address.slice(0, separator);
  const domain = mailbox.address.slice(separator + 1);
  const suffix = " via e2a";
  const displayName = mailbox.displayName;
  if (localPart !== "agent" || !RELAY_DOMAIN.test(domain)
    || typeof displayName !== "string" || !displayName.endsWith(suffix)
    || displayName.length === suffix.length) {
    throw transportError("malformed_message", "Evaluation relay sender evidence is malformed");
  }
  return { source: value.replace(/^[ \t]+|[ \t]+$/g, ""), address: mailbox.address, displayName };
}

function relayEnvelopeAddress(value) {
  const address = normalizedAgent(value);
  const separator = address.lastIndexOf("@");
  const localPart = address.slice(0, separator);
  const domain = address.slice(separator + 1);
  if (localPart !== "agent" || !RELAY_DOMAIN.test(domain)) {
    throw transportError("malformed_message", "Evaluation relay envelope sender evidence is malformed");
  }
  return address;
}

function reconcileRelayDelivery(kind, {
  logicalFrom, method, physicalFroms, mimeFroms, envelopeFroms, replyTos, expectedPhysicalFrom = null,
}) {
  const from = reconcileMailbox(`${kind} logical sender`, [logicalFrom], { required: true, preferred: logicalFrom });
  if (method !== "smtp") {
    throw transportError("conflicting_evidence", `Evaluation ${kind} relay sender provenance is inconsistent`);
  }
  const physical = mimeFroms
    .filter((value) => value !== undefined && value !== null)
    .map(normalizedRelayPhysicalFrom);
  if (physical.length === 0) {
    throw transportError("malformed_message", `Evaluation ${kind} relay sender evidence is missing`);
  }
  if (new Set(physical.map(({ address, displayName }) => `${displayName}\n${address}`)).size > 1) {
    throw transportError("conflicting_evidence", `Evaluation ${kind} relay sender representations conflict`);
  }
  const envelopes = envelopeFroms
    .filter((value) => value !== undefined && value !== null)
    .map(relayEnvelopeAddress);
  if (envelopes.length === 0) {
    throw transportError("malformed_message", `Evaluation ${kind} relay envelope sender evidence is missing`);
  }
  if (new Set(envelopes).size > 1 || physical.some(({ address }) => address !== envelopes[0])) {
    throw transportError("conflicting_evidence", `Evaluation ${kind} relay envelope sender representations conflict`);
  }
  const headerAddresses = physicalFroms
    .filter((value) => value !== undefined && value !== null)
    .map(normalizedAgent);
  if (headerAddresses.some((address) => address !== envelopes[0])) {
    throw transportError("conflicting_evidence", `Evaluation ${kind} relay sender representations conflict`);
  }
  const logicalAddress = normalizedAgent(from);
  let replyToObserved = false;
  for (const value of replyTos) {
    if (value === undefined || value === null) continue;
    const addresses = normalizedEvidenceAddresses(`${kind} Reply-To`, value);
    replyToObserved = true;
    if (!sameSet(addresses, [logicalAddress])) {
      throw transportError("conflicting_evidence", `Evaluation ${kind} relay Reply-To representations conflict`);
    }
  }
  if (!replyToObserved) {
    throw transportError("malformed_message", `Evaluation ${kind} relay Reply-To evidence is missing`);
  }
  if (expectedPhysicalFrom !== null) {
    const expected = normalizedRelayPhysicalFrom(expectedPhysicalFrom);
    if (expected.address !== physical[0].address || expected.displayName !== physical[0].displayName) {
      throw transportError("conflicting_evidence", `Evaluation ${kind} relay sender representations conflict`);
    }
  }
  return { from, physicalFrom: physical[0].source, sentAs: "relay" };
}

function reconcileDeliveredSender(kind, {
  logicalFrom, sentAs, method, message, mime, physicalFroms = [], envelopeFroms = [], replyTos = [],
  expectedPhysicalFrom = null,
}) {
  const from = reconcileMailbox(`${kind} logical sender`, [logicalFrom], { required: true, preferred: logicalFrom });
  if (sentAs === "relay") {
    return reconcileRelayDelivery(kind, {
      logicalFrom: from,
      method,
      physicalFroms,
      mimeFroms: [mime?.from],
      envelopeFroms: [...envelopeFroms, message?.envelopeFrom],
      replyTos: [...replyTos, mime?.replyTo],
      expectedPhysicalFrom,
    });
  }
  reconcileMailbox(`${kind} sender`, [from, ...physicalFroms, message?.headerFrom, mime?.from], { required: true });
  return { from, ...(sentAs === null ? {} : { sentAs }) };
}

async function mimeFromMessage(message, {
  required, budget, label = "message", requireMessageId = true,
}) {
  if (typeof message.rawMessage !== "string") {
    if (required) throw transportError("raw_mime_unavailable", "Evaluation message raw MIME is unavailable");
    return null;
  }
  const key = typeof message.id === "string" ? message.id : null;
  const fingerprint = createHash("sha256").update(message.rawMessage).digest("hex");
  const cached = key === null ? null : budget.cache.get(key);
  if (cached) {
    if (cached.fingerprint !== fingerprint) throw transportError("conflicting_evidence", "Evaluation raw MIME changed during observation");
    return cached.mime;
  }
  try {
    const mime = await parseMimeEvidence(message.rawMessage, { maxBytes: budget.remaining, requireMessageId });
    budget.remaining -= mime.sizeBytes;
    if (key !== null) budget.cache.set(key, { fingerprint, mime });
    return mime;
  } catch (error) {
    if (error instanceof EvalError && error.errorClass === "transport_error") throw error;
    const reason = error instanceof EvalError && /^[a-z][a-z0-9_]{0,127}$/.test(error.code)
      ? ` (${error.code})` : "";
    throw transportError("mime_observation_failed", `Evaluation ${label} MIME could not be normalized${reason}`);
  }
}

async function relayStimulusSenderContract(sdk, actor, sendResult, budget) {
  const fetched = await sdk.messages.get(actor, sendResult.messageId);
  const message = validateMessage(fetched, sendResult.messageId, "outbound");
  const sentAs = reconcileSentAs([sendResult.sentAs, message.sentAs]);
  if (sentAs !== "relay") {
    throw transportError("conflicting_evidence", "Evaluation stimulus relay mode changed after submission");
  }
  const from = reconcileMailbox("stimulus outbound logical sender", [actor, message.headerFrom], {
    required: true,
    preferred: actor,
  });
  const mime = await mimeFromMessage(message, {
    required: true, budget, label: "stimulus outbound", requireMessageId: false,
  });
  const providerIdentityAbsent = sendResult.providerMessageId === undefined
    || sendResult.providerMessageId === null || sendResult.providerMessageId === "";
  const providerMessageId = providerIdentityAbsent ? null : normalizeMessageIdToken(sendResult.providerMessageId);
  if (!providerIdentityAbsent && providerMessageId === null) {
    throw transportError("malformed_event", "Evaluation stimulus carried a malformed provider message identity");
  }
  if (mime.messageId !== null && providerMessageId !== null && mime.messageId !== providerMessageId) {
    throw transportError("conflicting_evidence", "Evaluation stimulus MIME and provider message identities conflict");
  }
  const rfcMessageId = mime.messageId ?? providerMessageId;
  if (rfcMessageId === null) {
    throw transportError("malformed_event", "Evaluation stimulus omitted its provider message identity");
  }
  const sender = reconcileDeliveredSender("stimulus outbound", {
    logicalFrom: from,
    sentAs,
    method: sendResult.method,
    message,
    mime,
  });
  return { ...sender, rfcMessageId };
}

async function normalizeStimulus(sdk, event, actor, target, sendResult, senderContract, budget) {
  const data = dataOf(event);
  if (event.type !== "email.received" || !eventIsFor(event, target, "inbound")) {
    throw transportError("stimulus_identity_mismatch", "Target stimulus identity could not be verified");
  }
  const eventMessageId = messageIdOf(event, data, null, { required: true });
  const fetchedMessage = await sdk.messages.get(target, eventMessageId);
  const messageId = messageIdOf(event, data, fetchedMessage, { required: true });
  const message = validateMessage(fetchedMessage, messageId, "inbound");
  const conversationId = conversationIdOf(event, data, message);
  const mime = await mimeFromMessage(message, { required: true, budget, label: "stimulus" });
  const sentAs = reconcileSentAs([sendResult?.sentAs, message?.sentAs]);
  const sender = reconcileDeliveredSender("stimulus", {
    logicalFrom: actor,
    sentAs,
    method: sentAs === "relay" ? "smtp" : sendResult?.method,
    message,
    mime,
    physicalFroms: [data.header_from, message.headerFrom],
    envelopeFroms: [data.envelope_from],
    replyTos: [data.reply_to, message.replyTo],
    expectedPhysicalFrom: senderContract?.physicalFrom ?? null,
  });
  if (sentAs === "relay") {
    if (!senderContract || senderContract.sentAs !== "relay") {
      throw transportError("conflicting_evidence", "Evaluation stimulus relay contract is unavailable");
    }
    reconcileText("stimulus RFC message", [senderContract.rfcMessageId, mime.messageId], { required: true });
  }
  const subject = reconcileText("stimulus subject", [data.subject, message.subject, mime.subject], { required: true });
  const to = strings(data.to);
  const cc = strings(data.cc, { optional: true });
  reconcileAddressSources("stimulus To", to, message.to ?? mime.to);
  reconcileAddressSources("stimulus To", to, mime.to);
  reconcileAddressSources("stimulus Cc", cc, message.cc ?? mime.cc);
  reconcileAddressSources("stimulus Cc", cc, mime.cc);
  const participants = stableEnvelopeRecipients([actor], to, cc).filter((address) => address !== target);
  const receivedAt = instant(data.received_at ?? message.createdAt ?? event.createdAt, "malformed_event").iso;
  const normalized = {
    ref: event.id,
    messageId,
    conversationId,
    rfcMessageId: mime.messageId,
    subject,
    receivedAt,
    ...sender,
  };
  normalized.participants = participants;
  return normalized;
}

async function refreshStimulusConversation(sdk, stimulus, target) {
  if (evidenceString(stimulus?.conversationId)) return stimulus;
  const fetchedMessage = await sdk.messages.get(target, stimulus.messageId);
  const message = validateMessage(fetchedMessage, stimulus.messageId, "inbound");
  const conversationId = conversationIdOf(null, null, message);
  return conversationId === null ? stimulus : { ...stimulus, conversationId };
}

function bindCorrelatedConversation(stimulus, candidates) {
  if (evidenceString(stimulus?.conversationId)) return stimulus;
  const conversations = [...new Set(candidates.map((candidate) => evidenceString(candidate.conversationId)).filter(Boolean))];
  if (conversations.length > 1) {
    throw transportError("conflicting_evidence", "Correlated replies carried conflicting conversation references");
  }
  return conversations.length === 1 ? { ...stimulus, conversationId: conversations[0] } : stimulus;
}

function stimulusEvents(events, actor, target, subject, lowerBound, upperBound, senderContract) {
  return uniqueEvents(events).filter((event) => {
    if (event.type !== "email.received") return false;
    const observed = eventInstant(event).milliseconds;
    if (observed < lowerBound || observed > upperBound) return false;
    const data = dataOf(event);
    if (!eventIsFor(event, target, "inbound") || data.subject !== subject) return false;
    if (senderContract?.sentAs !== "relay") return normalizedAgent(data.header_from) === actor;
    const expected = normalizedRelayPhysicalFrom(senderContract.physicalFrom);
    const physicalAddress = normalizedAgent(data.header_from);
    const envelope = relayEnvelopeAddress(data.envelope_from);
    const replyTo = normalizedEvidenceAddresses("stimulus Reply-To", data.reply_to);
    return physicalAddress === expected.address && envelope === expected.address && sameSet(replyTo, [actor]);
  });
}

function subjectMatches(subject, expectation, stimulusSubject) {
  if (typeof subject !== "string") return false;
  const specification = expectation ?? {};
  if (typeof specification.exact === "string" && subject !== specification.exact) return false;
  if (typeof specification.regex === "string") {
    try { if (!new RegExp(specification.regex).test(subject)) return false; } catch { return false; }
  }
  if (Array.isArray(specification.requiredFragments) && specification.requiredFragments.some((fragment) => !subject.includes(fragment))) return false;
  if (Array.isArray(specification.forbiddenFragments) && specification.forbiddenFragments.some((fragment) => subject.includes(fragment))) return false;
  if (specification.policy === "preserve") {
    const stripped = subject.replace(/^(?:Re:[ \t]*)+/i, "");
    if (stripped !== stimulusSubject) return false;
  }
  if (specification.policy === "forward") {
    const match = subject.match(/^(?:Fwd|Fw):[ \t]*/i);
    if (!match || subject.slice(match[0].length) !== stimulusSubject) return false;
  }
  const hasConstraint = Object.keys(specification).length > 0;
  return hasConstraint || subject === stimulusSubject;
}

function eventCandidateMetadata(event, target, lowerBound, upperBound) {
  if (!OUTBOUND_TERMINAL_EVENTS.has(event.type)) return null;
  const observed = eventInstant(event).milliseconds;
  if (observed < lowerBound || observed > upperBound) return null;
  const data = dataOf(event);
  if (!eventIsFor(event, target, "outbound")) return null;
  const messageId = messageIdOf(event, data, null, { required: true });
  return {
    event,
    data,
    messageId,
    conversationId: conversationIdOf(event, data),
    subject: data.subject,
  };
}

function outboundRecipients(eventType, data) {
  const stable = eventType === "email.sent" || eventType === "email.failed";
  const to = strings(data.to);
  const optional = (field) => data[field] === undefined
    ? (stable ? [] : undefined)
    : strings(data[field]);
  const cc = optional("cc");
  const bcc = optional("bcc");
  return {
    to,
    ...(cc === undefined ? {} : { cc }),
    ...(bcc === undefined ? {} : { bcc }),
    ...(cc === undefined || bcc === undefined ? {} : { envelopeRecipients: stableEnvelopeRecipients(to, cc, bcc) }),
  };
}

function isRowBackedCandidate(metadata) {
  return !(metadata.event.type === "email.blocked" && typeof metadata.event.messageId !== "string");
}

function canonicalCandidateMessageId(event, data, mime) {
  const rawMessageId = mime?.messageId ?? null;
  const providerValue = data.provider_message_id;
  if (providerValue === undefined || providerValue === null || providerValue === "") {
    if (event.type === "email.sent" && rawMessageId === null) {
      throw transportError("malformed_event", "Sent evaluation event omitted its provider message identity");
    }
    return rawMessageId;
  }
  const providerMessageId = normalizeMessageIdToken(providerValue);
  if (providerMessageId === null) {
    throw transportError("malformed_event", "Sent evaluation event carried a malformed provider message identity");
  }
  if (rawMessageId !== null && rawMessageId !== providerMessageId) {
    throw transportError("conflicting_evidence", "Sender MIME and provider message identities conflict");
  }
  return rawMessageId ?? providerMessageId;
}

async function hydrateCandidateMetadata(sdk, target, metadata) {
  if (!isRowBackedCandidate(metadata)) return { ...metadata, message: null };
  const fetchedMessage = await sdk.messages.get(target, metadata.messageId);
  messageIdOf(metadata.event, metadata.data, fetchedMessage, { required: true });
  const message = validateMessage(fetchedMessage, metadata.messageId, "outbound");
  return {
    ...metadata,
    message,
    conversationId: conversationIdOf(metadata.event, metadata.data, message),
  };
}

async function normalizeCandidate(sdk, target, metadata, budget) {
  const { event, data, messageId, message } = metadata;
  const recipients = outboundRecipients(event.type, data);
  let mime = null;
  let transitions = [];
  if (message) {
    mime = await mimeFromMessage(message, {
      required: event.type !== "email.review_requested", budget, label: "candidate", requireMessageId: false,
    });
    if (mime) mime = { ...mime, messageId: canonicalCandidateMessageId(event, data, mime) };
    transitions = await readLifecycle(sdk, target, messageId);
  }
  const sentAs = reconcileSentAs([data.sent_as, message?.sentAs]);
  const from = reconcileMailbox("candidate sender", [data.from, data.agent_email, message?.headerFrom], {
    required: true,
    preferred: data.from ?? data.agent_email,
  });
  const sender = reconcileDeliveredSender("candidate", {
    logicalFrom: from,
    sentAs,
    method: data.method,
    message,
    mime,
  });
  const subject = reconcileText("candidate subject", [data.subject, message?.subject, mime?.subject], {
    required: event.type !== "email.review_requested",
  });
  if (mime) {
    if (message?.to !== undefined) reconcileAddressSources("candidate To", recipients.to, message.to);
    reconcileAddressSources("candidate To", recipients.to, mime.to);
    if (recipients.cc !== undefined) {
      if (message?.cc !== undefined) reconcileAddressSources("candidate Cc", recipients.cc, message.cc);
      reconcileAddressSources("candidate Cc", recipients.cc, mime.cc);
    }
    else if (mime.cc.length > 0) throw transportError("conflicting_evidence", "Evaluation candidate Cc representations conflict");
    // MessageView.replyTo is deliberately inbound-only and always empty for
    // outbound rows. It is not a redundant representation of the header the
    // sender requested, so outbound Reply-To evidence comes from strict MIME.
  }
  const observedAt = eventInstant(event).iso;
  return {
    ref: event.id,
    eventType: event.type,
    direction: "outbound",
    provenance: "target_outbound",
    messageType: typeof data.message_type === "string" ? data.message_type : null,
    ...sender,
    ...(sentAs === null ? { sentAs: null } : {}),
    ...(mime ? { replyTo: mime.replyTo } : {}),
    ...recipients,
    conversationId: metadata.conversationId,
    messageId,
    subject,
    observedAt,
    sentAt: observedAt,
    mime,
    lifecycle: { submission: submissionFor(event.type), transitions },
  };
}

function relatedByMime(candidate, stimulus) {
  if (!candidate.mime || typeof stimulus.rfcMessageId !== "string") return false;
  return candidate.mime.inReplyTo === stimulus.rfcMessageId
    || candidate.mime.references?.includes(stimulus.rfcMessageId);
}

async function correlatedCandidates(sdk, events, resolvedCase, stimulus, target, lowerBound, upperBound, budget) {
  const stimulusLowerBound = Math.max(lowerBound, instant(stimulus.receivedAt, "malformed_event").milliseconds);
  const eventMetadata = uniqueEvents(events)
    .map((event) => eventCandidateMetadata(event, target, stimulusLowerBound, upperBound))
    .filter(Boolean);
  if (resolvedCase.expect.action.kind === "new_message") {
    const plausible = eventMetadata.filter((entry) => normalizedAgent(entry.data.from ?? entry.data.agent_email) === target
      && subjectMatches(entry.subject, resolvedCase.expect.subject, stimulus.subject));
    if (plausible.length === 0) return [];
    // A provider event can omit the authoritative row conversation. Hydrate
    // and immediately normalize one candidate at a time, then use the durable
    // conversation to disambiguate without ever retaining a page of raw MIME.
    const normalized = [];
    for (const entry of plausible) {
      const hydrated = await hydrateCandidateMetadata(sdk, target, entry);
      normalized.push(await normalizeCandidate(sdk, target, hydrated, budget));
    }
    if (stimulus.conversationId) {
      const exact = normalized.filter((candidate) => candidate.conversationId === stimulus.conversationId);
      if (exact.length === 1) return exact;
      if (exact.length > 1) throw transportError("ambiguous_correlation", "Multiple outbound messages matched the evaluation case");
    }
    if (normalized.length > 1) throw transportError("ambiguous_correlation", "Multiple outbound messages matched the evaluation case");
    return normalized;
  }

  // Never retain a page of MessageViews containing raw MIME. When a durable
  // conversation is available, inspect one row at a time and immediately
  // normalize only exact matches. If no exact match exists, refetch rows one at
  // a time for the MIME fallback; the extra bounded reads trade latency for a
  // strict O(one message) raw-byte footprint.
  if (stimulus.conversationId) {
    const exact = [];
    for (const entry of eventMetadata) {
      const hydrated = await hydrateCandidateMetadata(sdk, target, entry);
      if (hydrated.conversationId === stimulus.conversationId) {
        exact.push(await normalizeCandidate(sdk, target, hydrated, budget));
      }
    }
    if (exact.length > 0) return exact;
  }

  const normalized = [];
  for (const entry of eventMetadata) {
    const hydrated = await hydrateCandidateMetadata(sdk, target, entry);
    const candidate = await normalizeCandidate(sdk, target, hydrated, budget);
    if (relatedByMime(candidate, stimulus)) normalized.push(candidate);
  }
  return normalized;
}

function candidateForMime(candidates, mime) {
  const matches = candidates.filter((candidate) => candidate.mime?.messageId && candidate.mime.messageId === mime.messageId);
  if (matches.length > 1) {
    throw transportError("ambiguous_correlation", "Actor receipt matched multiple outbound candidates");
  }
  return matches[0] ?? null;
}

function canonicalMessageSummary(summary) {
  const comparable = (value) => {
    if (value instanceof Date) return value.toISOString();
    if (Array.isArray(value)) return value.map(comparable);
    if (value && typeof value === "object") {
      return Object.fromEntries(Object.entries(value).map(([key, entry]) => [key, comparable(entry)]));
    }
    return value;
  };
  return stableJson(comparable(summary));
}

function uniqueMessageSummaries(summaries) {
  const byId = new Map();
  for (const summary of summaries) {
    if (!summary || typeof summary.id !== "string" || summary.id.length === 0) {
      throw transportError("malformed_page", "Actor inbox contained a malformed message reference");
    }
    const prior = byId.get(summary.id);
    if (prior && canonicalMessageSummary(prior) !== canonicalMessageSummary(summary)) {
      throw transportError("conflicting_evidence", "Actor inbox message reference carried conflicting evidence");
    }
    if (!prior) byId.set(summary.id, summary);
  }
  return [...byId.values()];
}

async function findActorReceipt(sdk, actorEvents, actorMessages, baseline, candidates, actor, target, lowerBound, upperBound, budget) {
  const receipts = [];
  const candidateSubjects = new Set(candidates.map((candidate) => candidate.mime?.subject).filter((subject) => typeof subject === "string"));
  const relayCandidatePresent = candidates.some((candidate) => candidate.sentAs === "relay");
  for (const event of uniqueEvents(actorEvents)) {
    if (event.type !== "email.received") continue;
    const observed = eventInstant(event).milliseconds;
    if (observed < lowerBound || observed > upperBound) continue;
    const data = dataOf(event);
    if (!eventIsFor(event, actor, "inbound")) continue;
    if (!relayCandidatePresent && normalizedAgent(data.header_from) !== target) continue;
    if (candidateSubjects.size > 0 && !candidateSubjects.has(data.subject)) continue;
    const eventMessageId = messageIdOf(event, data, null, { required: true });
    const message = await sdk.messages.get(actor, eventMessageId);
    const receivedMessageId = messageIdOf(event, data, message, { required: true });
    validateMessage(message, receivedMessageId, "inbound");
    conversationIdOf(event, data, message);
    const mime = await mimeFromMessage(message, { required: true, budget, label: "actor receipt" });
    const candidate = candidateForMime(candidates, mime);
    if (candidate) {
      const sender = reconcileDeliveredSender("actor receipt", {
        logicalFrom: candidate.from,
        sentAs: candidate.sentAs,
        method: candidate.sentAs === "relay" ? "smtp" : null,
        message,
        mime,
        physicalFroms: [data.header_from, message.headerFrom],
        envelopeFroms: [data.envelope_from],
        replyTos: [data.reply_to, message.replyTo],
        expectedPhysicalFrom: candidate.physicalFrom ?? null,
      });
      reconcileText("actor receipt subject", [data.subject, message.subject, mime.subject], { required: true });
      receipts.push({
      ref: event.id,
      messageId: candidate.messageId,
      receiptMessageId: receivedMessageId,
      observedAt: instant(data.received_at ?? event.createdAt, "malformed_event").iso,
        ...sender,
      });
    }
  }
  if (receipts.length > 1) throw transportError("ambiguous_correlation", "Multiple actor receipts matched the evaluation case");
  if (receipts.length === 1) return receipts[0];

  for (const summary of uniqueMessageSummaries(actorMessages)) {
    if (!summary || typeof summary.id !== "string" || baseline.has(summary.id)) continue;
    if (!relayCandidatePresent && normalizedAgent(summary.headerFrom) !== target) continue;
    if (candidateSubjects.size > 0 && !candidateSubjects.has(summary.subject)) continue;
    const message = validateMessage(await sdk.messages.get(actor, summary.id), summary.id, "inbound");
    const mime = await mimeFromMessage(message, { required: true, budget, label: "actor receipt" });
    const candidate = candidateForMime(candidates, mime);
    if (!candidate) continue;
    const sender = reconcileDeliveredSender("actor receipt", {
      logicalFrom: candidate.from,
      sentAs: candidate.sentAs,
      method: candidate.sentAs === "relay" ? "smtp" : null,
      message,
      mime,
      physicalFroms: [summary.headerFrom, message.headerFrom],
      envelopeFroms: [summary.envelopeFrom],
      replyTos: [summary.replyTo, message.replyTo],
      expectedPhysicalFrom: candidate.physicalFrom ?? null,
    });
    reconcileText("actor receipt subject", [summary.subject, message.subject, mime.subject], { required: true });
    receipts.push({
      ref: `message:${summary.id}`,
      messageId: candidate.messageId,
      receiptMessageId: summary.id,
      observedAt: instant(message.createdAt, "malformed_message").iso,
      ...sender,
    });
  }
  if (receipts.length > 1) throw transportError("ambiguous_correlation", "Multiple actor receipts matched the evaluation case");
  return receipts[0] ?? null;
}

function isConnectionError(error) {
  return error instanceof E2AConnectionError;
}

function readBackoff(error, pollIntervalMs, remainingMs) {
  const hinted = typeof error?.retryAfterSeconds === "number" && Number.isFinite(error.retryAfterSeconds)
    ? Math.max(0, error.retryAfterSeconds * 1_000) : pollIntervalMs;
  return Math.min(Math.max(pollIntervalMs, hinted), remainingMs);
}

function validateExecution(resolvedCase, context) {
  if (!resolvedCase || typeof resolvedCase.id !== "string" || !resolvedCase.send
    || typeof resolvedCase.send.subject !== "string" || typeof resolvedCase.send.text !== "string"
    || !resolvedCase.expect?.action || typeof resolvedCase.expect.action.kind !== "string") {
    throw configurationError("invalid_case", "Resolved evaluation case is invalid");
  }
  for (const [field, minimum] of [["timeoutMs", 1], ["settleMs", 0], ["pollIntervalMs", 1]]) {
    if (!Number.isSafeInteger(context?.[field]) || context[field] < minimum) {
      throw configurationError("invalid_execution_context", "Evaluation execution timing is invalid");
    }
  }
  for (const field of ["executionDigest", "runId", "actor", "target"]) {
    if (typeof context?.[field] !== "string" || context[field].length === 0) {
      throw configurationError("invalid_execution_context", "Evaluation execution context is invalid");
    }
  }
}

function evidenceDocument({ capabilities, context, caseStartedAt, sendAcceptedAt, sendResult, stimulus, candidates, actorReceipt, completedAt }) {
  const refs = {
    stimulus: { event: stimulus?.ref ?? null, message: stimulus?.messageId ?? null, outboundMessage: sendResult.messageId ?? null },
    candidates: candidates.map((candidate) => candidate.ref),
    actorReceipt: actorReceipt?.ref ?? null,
  };
  return {
    version: 1,
    capabilities: capabilities.toArray(),
    target: { email: context.target },
    stimulus: stimulus ? { ...stimulus, outboundMessageId: sendResult.messageId ?? null } : {
      ref: null, messageId: null, outboundMessageId: sendResult.messageId ?? null,
      conversationId: null, rfcMessageId: null, subject: null, receivedAt: null,
    },
    candidates,
    actorReceipt,
    lifecycle: {
      stimulus: sendResult.status,
      candidates: candidates.map((candidate) => ({ ref: candidate.ref, ...candidate.lifecycle })),
      actorReceived: actorReceipt !== null,
    },
    timings: {
      runStartedAt: typeof context.startedAt === "string" ? context.startedAt : null,
      caseStartedAt,
      sendAcceptedAt,
      targetReceivedAt: stimulus?.receivedAt ?? null,
      firstCandidateAt: candidates[0]?.observedAt ?? null,
      completedAt,
      timeoutMs: context.timeoutMs,
      settleMs: context.settleMs,
      pollIntervalMs: context.pollIntervalMs,
    },
    refs,
  };
}

function deadlineBoundClient({
  client, clientFactory, apiKey, baseUrl, deadline, now,
  setTimer = setTimeout, clearTimer = clearTimeout,
}) {
  const remainingMilliseconds = () => Math.max(0, deadline - clockInstant(now).milliseconds);

  async function call(operation, code, message) {
    const remaining = remainingMilliseconds();
    if (remaining <= 0) throw transportError(code, message);
    const active = client ?? clientFactory({
      apiKey,
      baseUrl,
      maxRetries: 0,
      maxElapsedMs: remaining,
      timeoutMs: Math.max(1, Math.min(TIMEOUTS.timeoutMs, remaining)),
    });
    return new Promise((resolve, reject) => {
      let settled = false;
      const timer = setTimer(() => {
        if (settled) return;
        settled = true;
        reject(transportError(code, message));
      }, remaining);
      Promise.resolve().then(() => operation(active)).then(
        (value) => {
          if (settled) return;
          settled = true;
          clearTimer(timer);
          resolve(value);
        },
        (error) => {
          if (settled) return;
          settled = true;
          clearTimer(timer);
          reject(error);
        },
      );
    });
  }

  function list(resource, args) {
    return {
      async toArray({ limit }) {
        if (client) {
          return call(
            async (active) => active[resource].list(...args).toArray({ limit }),
            "observation_failed",
            "Evaluation evidence read exceeded the case deadline",
          );
        }
        const items = [];
        const cursors = new Set();
        let cursor;
        while (items.length < limit) {
          const page = await call(
            async (active) => active[resource].list(...args).page(cursor),
            "observation_failed",
            "Evaluation evidence read exceeded the case deadline",
          );
          if (!page || !Array.isArray(page.items)) throw transportError("malformed_page", "Evaluation list response is malformed");
          for (const item of page.items) {
            items.push(item);
            if (items.length >= limit) return items;
          }
          const next = page.next_cursor;
          if (!next) return items;
          if (typeof next !== "string" || cursors.has(next)) {
            throw transportError("malformed_page", "Evaluation list cursor did not advance safely");
          }
          cursors.add(next);
          cursor = next;
          if (cursors.size > OBSERVATION_LIMIT + 1) {
            throw transportError("observation_limit_exceeded", "Evaluation observation exceeded its bounded result limit");
          }
        }
        return items;
      },
    };
  }

  return {
    messages: {
      list: (...args) => list("messages", args),
      get: (...args) => call(
        (active) => active.messages.get(...args),
        "observation_failed",
        "Evaluation message read exceeded the case deadline",
      ),
      getLifecycle: (...args) => call(
        (active) => active.messages.getLifecycle(...args),
        "observation_failed",
        "Evaluation lifecycle read exceeded the case deadline",
      ),
      send: (...args) => call(
        (active) => active.messages.send(...args),
        "stimulus_send_failed",
        "Evaluation case deadline elapsed during stimulus submission",
      ),
    },
    events: {
      list: (...args) => list("events", args),
    },
  };
}

/**
 * Creates the e2a transport adapter. Its first operation is deliberately a
 * read-only containment preflight; no protection mutation or mail send lives
 * on this path.
 */
export function createE2AAdapter({
  apiKey, baseUrl, client, now = () => new Date(),
  sleep = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds)),
  mimeBudgetBytes = MIME_CASE_BUDGET_BYTES,
  clientFactory = (options) => new E2AClient(options),
  setTimer = setTimeout,
  clearTimer = clearTimeout,
}) {
  if (!Number.isSafeInteger(mimeBudgetBytes) || mimeBudgetBytes <= 0 || mimeBudgetBytes > MIME_CASE_BUDGET_BYTES) {
    throw configurationError("invalid_suite", "Evaluation MIME budget is invalid");
  }
  if (typeof clientFactory !== "function" || typeof setTimer !== "function" || typeof clearTimer !== "function") {
    throw configurationError("invalid_suite", "Evaluation network boundary is invalid");
  }
  const sdk = client ?? clientFactory({ apiKey, baseUrl, ...TIMEOUTS });
  const capabilities = new ReadonlyCapabilitySet(CAPABILITIES);

  return Object.freeze({
    capabilities,
    async preflight(resolvedSuite) {
      const safeBaseUrl = serializableBaseUrl(baseUrl);
      const actor = normalizeAddress(resolvedSuite?.actor?.email);
      const target = normalizeAddress(resolvedSuite?.target?.email);
      if (actor === target) throw configurationError("same_actor_target", "Actor and target must differ");

      const allowed = normalizeAddressList(resolvedSuite?.transport?.allowedEnvelopeRecipients, "invalid_allowlist");
      const allowedSet = new Set(allowed);
      if (!allowedSet.has(actor) || !allowedSet.has(target)) {
        throw configurationError("invalid_allowlist", "Allowlist must include actor and target");
      }
      const probes = allowed.filter((address) => address !== actor && address !== target);
      const aliases = aliasesFor(actor, target, probes);
      const cases = Array.isArray(resolvedSuite?.cases) ? resolvedSuite.cases.map(collectCaseAddresses) : (() => {
        throw configurationError("invalid_suite", "Resolved evaluation cases are invalid");
      })();
      for (const testCase of cases) {
        for (const address of testCase.addresses) aliasOf(aliases, address);
      }

      await readOwnedAgent(sdk, actor);
      await readOwnedAgent(sdk, target);
      const actorProtection = normalizeGate(await readProtection(sdk, actor), "actor_gate_not_exact");
      const targetProtection = normalizeGate(await readProtection(sdk, target), "target_gate_not_exact");
      if (!sameSet(actorProtection.allowlist, [target])) {
        throw configurationError("actor_gate_not_exact", "Actor outbound gate must contain only the target");
      }
      const expectedTargetGate = [actor, ...probes].sort();
      if (!sameSet(targetProtection.allowlist, expectedTargetGate)) {
        throw configurationError("target_gate_not_exact", "Target outbound gate must match the evaluation allowlist");
      }
      for (const probe of probes) await readOwnedAgent(sdk, probe);

      const aliasedActorGate = actorProtection.allowlist.map((address) => aliasOf(aliases, address));
      const aliasedTargetGate = targetProtection.allowlist.map((address) => aliasOf(aliases, address));
      const protectionDigest = createHash("sha256").update(stableJson({
        actor: { policy: actorProtection.policy, action: actorProtection.action, allowlist: aliasedActorGate },
        target: { policy: targetProtection.policy, action: targetProtection.action, allowlist: aliasedTargetGate },
      })).digest("hex");
      const allowedAliases = allowed.map((address) => aliasOf(aliases, address));

      return {
        capabilities,
        actor: { email: "actor" },
        target: { email: "target" },
        probes: probes.map((address) => aliasOf(aliases, address)),
        protectionDigest,
        plan: makePlan({
          baseUrl: safeBaseUrl, aliases, allowedAliases, cases, apiKey, defaults: resolvedSuite.defaults,
        }),
      };
    },
    async executeCase(resolvedCase, context) {
      validateExecution(resolvedCase, context);
      const actor = normalizeAddress(context.actor);
      const target = normalizeAddress(context.target);
      if (actor === target) throw configurationError("same_actor_target", "Actor and target must differ");

      const caseStart = clockInstant(now);
      const caseStartedAt = caseStart.iso;
      const deadline = caseStart.milliseconds + context.timeoutMs;
      const caseSdk = deadlineBoundClient({
        client, clientFactory, apiKey, baseUrl, deadline, now, setTimer, clearTimer,
      });
      const mimeBudget = { remaining: mimeBudgetBytes, cache: new Map() };
      const requireSendWindow = () => {
        if (clockInstant(now).milliseconds >= deadline) {
          throw transportError("stimulus_send_failed", "Evaluation case deadline elapsed before stimulus submission");
        }
      };
      const baselineSince = new Date(caseStart.milliseconds - 2 * context.timeoutMs).toISOString();
      let baselineItems;
      try {
        baselineItems = await boundedItems(
          caseSdk.messages.list(actor, { direction: "inbound", since: baselineSince, limit: OBSERVATION_LIMIT }),
          "baseline_limit_exceeded",
        );
      } catch (error) {
        if (error instanceof EvalError) throw error;
        throw transportError("baseline_read_failed", "Unable to record the bounded actor inbox baseline");
      }
      const baseline = new Set(uniqueMessageSummaries(baselineItems).map((item) => item.id));

      const stimulusBody = Object.freeze({
        to: Object.freeze([target]),
        subject: resolvedCase.send.subject,
        text: resolvedCase.send.text,
      });
      const idempotencyKey = `eev1_${createHash("sha256").update([
        context.executionDigest,
        context.runId,
        resolvedCase.id,
        stableJson(stimulusBody),
      ].join("\n")).digest("hex").slice(0, 48)}`;
      const sendOptions = Object.freeze({ idempotencyKey, wait: "sent" });
      let sendResult;
      try {
        requireSendWindow();
        sendResult = await caseSdk.messages.send(actor, stimulusBody, sendOptions);
      } catch (error) {
        if (!isConnectionError(error)) {
          throw transportError("stimulus_send_failed", "Evaluation stimulus could not be submitted");
        }
        try {
          // The SDK already exhausted its keyed retries. This is the only
          // explicit recovery: request the server's idempotent replay with the
          // exact same frozen body object and key.
          requireSendWindow();
          sendResult = await caseSdk.messages.send(actor, stimulusBody, sendOptions);
        } catch {
          throw transportError("send_acceptance_unknown", "Evaluation stimulus acceptance could not be established safely");
        }
      }
      if (!sendResult || !["accepted", "sent"].includes(sendResult.status)
        || typeof sendResult.messageId !== "string" || sendResult.messageId.length === 0) {
        throw transportError("stimulus_not_delivered", "Evaluation stimulus did not enter an observable delivery state");
      }
      const sendAcceptedAt = clockInstant(now).iso;
      const submittedSentAs = reconcileSentAs([sendResult.sentAs]);
      const stimulusSenderContract = submittedSentAs === "relay"
        ? await relayStimulusSenderContract(caseSdk, actor, sendResult, mimeBudget)
        : null;

      let logicalElapsed = Math.max(0, clockInstant(now).milliseconds - caseStart.milliseconds);
      let lastEvidence = { stimulus: null, candidates: [], actorReceipt: null };
      let terminalObservedAt = null;
      let lastReadFailure = null;
      let lastSuccessfulReadAt = -1;

      for (;;) {
        const realElapsed = Math.max(0, clockInstant(now).milliseconds - caseStart.milliseconds);
        const elapsed = Math.max(logicalElapsed, realElapsed);
        if (elapsed > context.timeoutMs) logicalElapsed = context.timeoutMs;

        // Do not begin a fresh network operation once the case or settle
        // boundary has already been reached. The previous complete read is
        // authoritative at that point; attempting one more bounded read would
        // manufacture an observation failure exactly at the deadline.
        const boundaryReached = elapsed >= context.timeoutMs
          || (resolvedCase.expect.action.kind !== "none"
            && terminalObservedAt !== null
            && elapsed >= Math.min(context.timeoutMs, terminalObservedAt + context.settleMs));
        if (boundaryReached) {
          if (lastReadFailure && lastReadFailure.elapsed >= lastSuccessfulReadAt) {
            throw transportError("observation_failed", "Evaluation evidence could not be read completely");
          }
          if (!lastEvidence.stimulus) {
            throw transportError("stimulus_not_observed", "Evaluation stimulus receipt was not observed");
          }
          return evidenceDocument({
            capabilities,
            context: { ...context, actor, target },
            caseStartedAt,
            sendAcceptedAt,
            sendResult,
            ...lastEvidence,
            completedAt: clockInstant(now).iso,
          });
        }

        try {
          const targetEvents = await boundedItems(
            caseSdk.events.list({ agentEmail: target, since: caseStartedAt, limit: OBSERVATION_LIMIT }),
            "observation_limit_exceeded",
          );
          const actorEvents = await boundedItems(
            caseSdk.events.list({ agentEmail: actor, since: caseStartedAt, limit: OBSERVATION_LIMIT }),
            "observation_limit_exceeded",
          );
          const actorMessages = await boundedItems(
            caseSdk.messages.list(actor, { direction: "inbound", since: caseStartedAt, limit: OBSERVATION_LIMIT }),
            "observation_limit_exceeded",
          );

          let stimulus = lastEvidence.stimulus;
          if (!stimulus) {
            const matches = stimulusEvents(
              targetEvents, actor, target, resolvedCase.send.subject,
              caseStart.milliseconds, deadline, stimulusSenderContract,
            );
            const messageRefs = [...new Set(matches.map((event) => messageIdOf(event, dataOf(event), null, { required: true })))];
            if (messageRefs.length > 1) throw transportError("ambiguous_correlation", "Multiple target messages matched the evaluation stimulus");
            if (matches.length > 0) {
              stimulus = await normalizeStimulus(
                caseSdk, matches[0], actor, target, sendResult, stimulusSenderContract, mimeBudget,
              );
            }
          }

          let candidates = [];
          let actorReceipt = null;
          if (stimulus) {
            candidates = await correlatedCandidates(
              caseSdk, targetEvents, resolvedCase, stimulus, target, caseStart.milliseconds, deadline, mimeBudget,
            );
            candidates.sort((left, right) => left.observedAt.localeCompare(right.observedAt) || left.ref.localeCompare(right.ref));
            if (candidates.length > 0 && !evidenceString(stimulus.conversationId)) {
              // An inbound row can receive its agent-local conversation ID only
              // when the target creates the reply. Refresh that structured row
              // after MIME correlation so conversation grading remains an
              // independent durable-source check.
              stimulus = await refreshStimulusConversation(caseSdk, stimulus, target);
              if (!evidenceString(stimulus.conversationId) && resolvedCase.expect.action.kind !== "new_message") {
                // Some inbound APIs assign no target-local conversation until
                // reply creation and do not backfill MessageView. A uniquely
                // MIME-correlated reply supplies that same durable thread ID.
                stimulus = bindCorrelatedConversation(stimulus, candidates);
              }
            }
            actorReceipt = await findActorReceipt(
              caseSdk, actorEvents, actorMessages, baseline, candidates, actor, target, caseStart.milliseconds, deadline, mimeBudget,
            );
          }
          lastEvidence = { stimulus, candidates, actorReceipt };
          lastSuccessfulReadAt = elapsed;
          lastReadFailure = null;
          if (resolvedCase.expect.action.kind !== "none" && candidates.length > 0 && terminalObservedAt === null) {
            terminalObservedAt = elapsed;
          }
        } catch (error) {
          if (error instanceof EvalError && [
            "observation_limit_exceeded", "ambiguous_correlation", "conflicting_event_ref", "conflicting_evidence",
            "malformed_event", "malformed_message", "mime_observation_failed", "stimulus_identity_mismatch",
          ].includes(error.code)) throw error;
          lastReadFailure = { error, elapsed };
        }

        const afterReadRealElapsed = Math.max(0, clockInstant(now).milliseconds - caseStart.milliseconds);
        const afterReadElapsed = Math.max(logicalElapsed, afterReadRealElapsed);
        const fullTimeout = afterReadElapsed >= context.timeoutMs;
        const settled = resolvedCase.expect.action.kind !== "none"
          && terminalObservedAt !== null
          && afterReadElapsed >= Math.min(context.timeoutMs, terminalObservedAt + context.settleMs);
        if (fullTimeout || settled) {
          if (lastReadFailure && lastReadFailure.elapsed >= lastSuccessfulReadAt) {
            throw transportError("observation_failed", "Evaluation evidence could not be read completely");
          }
          if (!lastEvidence.stimulus) {
            throw transportError("stimulus_not_observed", "Evaluation stimulus receipt was not observed");
          }
          const completedAt = clockInstant(now).iso;
          return evidenceDocument({
            capabilities,
            context: { ...context, actor, target },
            caseStartedAt,
            sendAcceptedAt,
            sendResult,
            ...lastEvidence,
            completedAt,
          });
        }

        const nextBoundary = terminalObservedAt === null || resolvedCase.expect.action.kind === "none"
          ? context.timeoutMs
          : Math.min(context.timeoutMs, terminalObservedAt + context.settleMs);
        const remaining = Math.max(0, nextBoundary - afterReadElapsed);
        const waitMs = lastReadFailure
          ? readBackoff(lastReadFailure.error, context.pollIntervalMs, remaining)
          : Math.min(context.pollIntervalMs, remaining);
        if (waitMs <= 0) {
          logicalElapsed = nextBoundary;
          continue;
        }
        await sleep(waitMs);
        logicalElapsed = Math.min(context.timeoutMs, afterReadElapsed + waitMs);
      }
    },
  });
}

export const E2A_ADAPTER_CAPABILITIES = Object.freeze([...CAPABILITIES]);
