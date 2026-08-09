import { createHash } from "node:crypto";
import { E2AClient, E2AConnectionError } from "@e2a/sdk/v1";
import { EvalError, isStableEvalErrorCode } from "./errors.mjs";
import { normalizeMessageIdToken, parseMimeEvidence } from "./mime.mjs";
import { NormalizationError, normalizeAddressSet, normalizeMailbox } from "./normalize.mjs";

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
  return parsed.toString();
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
  if (!action || typeof action !== "object" || typeof action.kind !== "string" || !Number.isSafeInteger(action.count) || action.count < 0) {
    throw configurationError("invalid_suite", "Resolved case action is invalid");
  }

  const addresses = [];
  const sender = testCase.expect.sender;
  if (sender !== undefined) {
    if (!sender || typeof sender !== "object") throw configurationError("invalid_suite", "Resolved case sender is invalid");
    for (const field of ["exactly", "sentAs"]) {
      if (sender[field] !== undefined) addresses.push(normalizeAddress(sender[field], "invalid_suite"));
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
  if (action.count > 0 && recipients?.envelope === undefined) {
    throw configurationError("missing_envelope_allowlist", "Outbound cases require an exact envelope expectation");
  }
  return { id: testCase.id, action: { kind: action.kind, count: action.count }, addresses: [...new Set(addresses)].sort() };
}

function makePlan({ baseUrl, aliases, allowedAliases, cases }) {
  return {
    baseUrl,
    recipientAliases: allowedAliases,
    capabilities: [...CAPABILITIES],
    timeouts: { ...TIMEOUTS },
    networkSends: false,
    cases: cases.map((testCase) => ({
      id: testCase.id,
      expectedAction: testCase.action,
      recipientAliases: testCase.addresses.map((address) => aliasOf(aliases, address)),
    })),
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

function reconciledIdentity(kind, values, { required = false, authoritative = null } = {}) {
  const observed = values.map(evidenceString).filter(Boolean);
  if (new Set(observed).size > 1) {
    throw transportError("conflicting_evidence", `Evaluation ${kind} references conflict`);
  }
  if (required && observed.length === 0) {
    throw transportError("malformed_event", `Evaluation event omitted its ${kind} reference`);
  }
  return evidenceString(authoritative) ?? observed[0] ?? null;
}

function messageIdOf(event, data, message, { required = false } = {}) {
  return reconciledIdentity("message", [event?.messageId, data?.message_id, message?.id], {
    required,
    authoritative: message?.id,
  });
}

function conversationIdOf(event, data, message) {
  return reconciledIdentity("conversation", [event?.conversationId, data?.conversation_id, message?.conversationId], {
    authoritative: message?.conversationId,
  });
}

function normalizedAgent(value) {
  if (typeof value !== "string") return null;
  try {
    return normalizeMailbox(value).address;
  } catch {
    return null;
  }
}

function eventIsFor(event, expectedAgent, direction) {
  const data = dataOf(event);
  const envelopeAgent = event.agentEmail === undefined ? null : normalizedAgent(event.agentEmail);
  const payloadAgent = normalizedAgent(data.agent_email);
  return payloadAgent === expectedAgent
    && (envelopeAgent === null || envelopeAgent === expectedAgent)
    && data.direction === direction;
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

async function mimeFromMessage(message, { required }) {
  if (typeof message.rawMessage !== "string") {
    if (required) throw transportError("raw_mime_unavailable", "Evaluation message raw MIME is unavailable");
    return null;
  }
  try {
    return await parseMimeEvidence(message.rawMessage);
  } catch {
    throw transportError("mime_observation_failed", "Evaluation message MIME could not be normalized");
  }
}

async function normalizeStimulus(sdk, event, actor, target) {
  const data = dataOf(event);
  if (event.type !== "email.received" || !eventIsFor(event, target, "inbound") || normalizedAgent(data.header_from) !== actor) {
    throw transportError("stimulus_identity_mismatch", "Target stimulus identity could not be verified");
  }
  const eventMessageId = messageIdOf(event, data, null, { required: true });
  const fetchedMessage = await sdk.messages.get(target, eventMessageId);
  const messageId = messageIdOf(event, data, fetchedMessage, { required: true });
  const message = validateMessage(fetchedMessage, messageId, "inbound");
  const conversationId = conversationIdOf(event, data, message);
  const mime = await mimeFromMessage(message, { required: true });
  const to = strings(data.to);
  const cc = strings(data.cc, { optional: true });
  const participants = stableEnvelopeRecipients([data.header_from], to, cc).filter((address) => address !== target);
  const receivedAt = instant(data.received_at ?? message.createdAt ?? event.createdAt, "malformed_event").iso;
  const normalized = {
    ref: event.id,
    messageId,
    conversationId,
    rfcMessageId: mime.messageId,
    subject: typeof data.subject === "string" ? data.subject : message.subject,
    receivedAt,
  };
  normalized.participants = participants;
  return normalized;
}

function stimulusEvents(events, actor, target, subject, lowerBound, upperBound) {
  return uniqueEvents(events).filter((event) => {
    if (event.type !== "email.received") return false;
    const observed = eventInstant(event).milliseconds;
    if (observed < lowerBound || observed > upperBound) return false;
    const data = dataOf(event);
    return eventIsFor(event, target, "inbound")
      && normalizedAgent(data.header_from) === actor
      && data.subject === subject;
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

async function normalizeCandidate(sdk, target, metadata) {
  const { event, data, messageId, message } = metadata;
  const recipients = outboundRecipients(event.type, data);
  let mime = null;
  let transitions = [];
  if (message) {
    mime = await mimeFromMessage(message, { required: event.type !== "email.review_requested" });
    if (mime) mime = { ...mime, messageId: canonicalCandidateMessageId(event, data, mime) };
    transitions = await readLifecycle(sdk, target, messageId);
  }
  const observedAt = eventInstant(event).iso;
  return {
    ref: event.id,
    eventType: event.type,
    direction: "outbound",
    provenance: "target_outbound",
    messageType: typeof data.message_type === "string" ? data.message_type : null,
    from: typeof data.from === "string" ? data.from : message?.headerFrom ?? data.agent_email,
    sentAs: data.agent_email,
    ...(mime ? { replyTo: mime.replyTo } : {}),
    ...recipients,
    conversationId: metadata.conversationId,
    messageId,
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

async function correlatedCandidates(sdk, events, resolvedCase, stimulus, target, lowerBound, upperBound) {
  const stimulusLowerBound = Math.max(lowerBound, instant(stimulus.receivedAt, "malformed_event").milliseconds);
  const eventMetadata = uniqueEvents(events)
    .map((event) => eventCandidateMetadata(event, target, stimulusLowerBound, upperBound))
    .filter(Boolean);
  const metadata = await Promise.all(eventMetadata.map((entry) => hydrateCandidateMetadata(sdk, target, entry)));
  const exact = stimulus.conversationId
    ? metadata.filter((entry) => entry.conversationId === stimulus.conversationId)
    : [];
  if (exact.length > 0) return Promise.all(exact.map((entry) => normalizeCandidate(sdk, target, entry)));

  if (resolvedCase.expect.action.kind === "new_message") {
    const plausible = metadata.filter((entry) => normalizedAgent(entry.data.from ?? entry.data.agent_email) === target
      && subjectMatches(entry.subject, resolvedCase.expect.subject, stimulus.subject));
    if (plausible.length > 1) throw transportError("ambiguous_correlation", "Multiple outbound messages matched the evaluation case");
    return plausible.length === 1 ? [await normalizeCandidate(sdk, target, plausible[0])] : [];
  }

  const normalized = [];
  for (const entry of metadata) {
    const candidate = await normalizeCandidate(sdk, target, entry);
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

async function findActorReceipt(sdk, actorEvents, actorMessages, baseline, candidates, actor, target, lowerBound, upperBound) {
  const receipts = [];
  const candidateSubjects = new Set(candidates.map((candidate) => candidate.mime?.subject).filter((subject) => typeof subject === "string"));
  for (const event of uniqueEvents(actorEvents)) {
    if (event.type !== "email.received") continue;
    const observed = eventInstant(event).milliseconds;
    if (observed < lowerBound || observed > upperBound) continue;
    const data = dataOf(event);
    if (!eventIsFor(event, actor, "inbound") || normalizedAgent(data.header_from) !== target) continue;
    if (candidateSubjects.size > 0 && !candidateSubjects.has(data.subject)) continue;
    const eventMessageId = messageIdOf(event, data, null, { required: true });
    const message = await sdk.messages.get(actor, eventMessageId);
    const receivedMessageId = messageIdOf(event, data, message, { required: true });
    validateMessage(message, receivedMessageId, "inbound");
    conversationIdOf(event, data, message);
    const mime = await mimeFromMessage(message, { required: true });
    const candidate = candidateForMime(candidates, mime);
    if (candidate) receipts.push({
      ref: event.id,
      messageId: candidate.messageId,
      receiptMessageId: receivedMessageId,
      observedAt: instant(data.received_at ?? event.createdAt, "malformed_event").iso,
    });
  }
  if (receipts.length > 1) throw transportError("ambiguous_correlation", "Multiple actor receipts matched the evaluation case");
  if (receipts.length === 1) return receipts[0];

  for (const summary of uniqueMessageSummaries(actorMessages)) {
    if (!summary || typeof summary.id !== "string" || baseline.has(summary.id)) continue;
    if (normalizedAgent(summary.headerFrom) !== target) continue;
    if (candidateSubjects.size > 0 && !candidateSubjects.has(summary.subject)) continue;
    const message = validateMessage(await sdk.messages.get(actor, summary.id), summary.id, "inbound");
    if (normalizedAgent(message.headerFrom) !== target) continue;
    const mime = await mimeFromMessage(message, { required: true });
    const candidate = candidateForMime(candidates, mime);
    if (!candidate) continue;
    receipts.push({
      ref: `message:${summary.id}`,
      messageId: candidate.messageId,
      receiptMessageId: summary.id,
      observedAt: instant(message.createdAt, "malformed_message").iso,
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
  for (const field of ["suiteDigest", "runId", "actor", "target"]) {
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

/**
 * Creates the e2a transport adapter. Its first operation is deliberately a
 * read-only containment preflight; no protection mutation or mail send lives
 * on this path.
 */
export function createE2AAdapter({ apiKey, baseUrl, client, now = () => new Date(), sleep = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds)) }) {
  const sdk = client ?? new E2AClient({ apiKey, baseUrl, ...TIMEOUTS });
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
        plan: makePlan({ baseUrl: safeBaseUrl, aliases, allowedAliases, cases }),
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
      const baselineSince = new Date(caseStart.milliseconds - 2 * context.timeoutMs).toISOString();
      let baselineItems;
      try {
        baselineItems = await boundedItems(
          sdk.messages.list(actor, { direction: "inbound", since: baselineSince, limit: OBSERVATION_LIMIT }),
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
        context.suiteDigest,
        context.runId,
        resolvedCase.id,
        stableJson(stimulusBody),
      ].join("\n")).digest("hex").slice(0, 48)}`;
      const sendOptions = Object.freeze({ idempotencyKey, wait: "sent" });
      let sendResult;
      try {
        sendResult = await sdk.messages.send(actor, stimulusBody, sendOptions);
      } catch (error) {
        if (!isConnectionError(error)) {
          throw transportError("stimulus_send_failed", "Evaluation stimulus could not be submitted");
        }
        try {
          // The SDK already exhausted its keyed retries. This is the only
          // explicit recovery: request the server's idempotent replay with the
          // exact same frozen body object and key.
          sendResult = await sdk.messages.send(actor, stimulusBody, sendOptions);
        } catch {
          throw transportError("send_acceptance_unknown", "Evaluation stimulus acceptance could not be established safely");
        }
      }
      if (!sendResult || !["accepted", "sent"].includes(sendResult.status)
        || typeof sendResult.messageId !== "string" || sendResult.messageId.length === 0) {
        throw transportError("stimulus_not_delivered", "Evaluation stimulus did not enter an observable delivery state");
      }
      const sendAcceptedAt = clockInstant(now).iso;

      let logicalElapsed = Math.max(0, clockInstant(now).milliseconds - caseStart.milliseconds);
      let lastEvidence = { stimulus: null, candidates: [], actorReceipt: null };
      let terminalObservedAt = null;
      let lastReadFailure = null;
      let lastSuccessfulReadAt = -1;

      for (;;) {
        const realElapsed = Math.max(0, clockInstant(now).milliseconds - caseStart.milliseconds);
        const elapsed = Math.max(logicalElapsed, realElapsed);
        if (elapsed > context.timeoutMs) logicalElapsed = context.timeoutMs;

        try {
          const targetEvents = await boundedItems(
            sdk.events.list({ agentEmail: target, since: caseStartedAt, limit: OBSERVATION_LIMIT }),
            "observation_limit_exceeded",
          );
          const actorEvents = await boundedItems(
            sdk.events.list({ agentEmail: actor, since: caseStartedAt, limit: OBSERVATION_LIMIT }),
            "observation_limit_exceeded",
          );
          const actorMessages = await boundedItems(
            sdk.messages.list(actor, { direction: "inbound", since: caseStartedAt, limit: OBSERVATION_LIMIT }),
            "observation_limit_exceeded",
          );

          let stimulus = lastEvidence.stimulus;
          if (!stimulus) {
            const matches = stimulusEvents(targetEvents, actor, target, resolvedCase.send.subject, caseStart.milliseconds, deadline);
            const messageRefs = [...new Set(matches.map((event) => messageIdOf(event, dataOf(event), null, { required: true })))];
            if (messageRefs.length > 1) throw transportError("ambiguous_correlation", "Multiple target messages matched the evaluation stimulus");
            if (matches.length > 0) stimulus = await normalizeStimulus(sdk, matches[0], actor, target);
          }

          let candidates = [];
          let actorReceipt = null;
          if (stimulus) {
            candidates = await correlatedCandidates(sdk, targetEvents, resolvedCase, stimulus, target, caseStart.milliseconds, deadline);
            candidates.sort((left, right) => left.observedAt.localeCompare(right.observedAt) || left.ref.localeCompare(right.ref));
            actorReceipt = await findActorReceipt(
              sdk, actorEvents, actorMessages, baseline, candidates, actor, target, caseStart.milliseconds, deadline,
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
