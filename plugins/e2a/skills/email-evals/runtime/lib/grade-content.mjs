import { createHash } from "node:crypto";
import { types as utilTypes } from "node:util";

const STATUS_WEIGHT = Object.freeze({ pass: 0, fail: 1, error: 2 });
// Eval evidence is normally fewer than ten levels deep, contains a handful of
// candidates/attachments, and stays well below one thousand scalar fields.
// These deliberately generous limits bound work before grading untrusted
// evidence while leaving substantial room for local diagnostic metadata.
const EVIDENCE_SNAPSHOT_LIMITS = Object.freeze({
  maxDepth: 64,
  maxNodes: 8_192,
  maxObjectKeys: 256,
  maxArrayLength: 1_024,
});
// Positive UTC leap-second insertion instants through IERS Bulletin C 72
// (2026-07-06). Update this table when a later Bulletin C announces one.
const LEAP_SECOND_UTC_INSTANTS = new Set([
  "1972-07-01", "1973-01-01", "1974-01-01", "1975-01-01", "1976-01-01", "1977-01-01", "1978-01-01", "1979-01-01", "1980-01-01",
  "1981-07-01", "1982-07-01", "1983-07-01", "1985-07-01", "1988-01-01", "1990-01-01", "1991-01-01", "1992-07-01", "1993-07-01",
  "1994-07-01", "1996-01-01", "1997-07-01", "1999-01-01", "2006-01-01", "2009-01-01", "2012-07-01", "2015-07-01", "2017-01-01",
].map((date) => Date.parse(`${date}T00:00:00.000Z`)));

function serializable(value) {
  return value === undefined ? null : JSON.parse(JSON.stringify(value));
}

function canonical(value) {
  if (value === null) return "null";
  if (value === undefined) return "undefined";
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (typeof value === "object") return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`).join(",")}}`;
  return JSON.stringify(value);
}

function snapshotJsonSafePlainData(value) {
  let root;
  let nodeCount = 0;
  let currentRootKey = null;
  const active = new Set();
  const stack = [{ kind: "visit", source: value, parent: null, key: null, depth: 0, rootKey: null }];

  const assign = (parent, key, snapshot) => {
    if (parent === null) {
      root = snapshot;
      return;
    }
    Object.defineProperty(parent, key, {
      value: snapshot,
      enumerable: true,
      configurable: true,
      writable: true,
    });
  };

  const invalid = (reason) => ({ ok: false, reason, rootKey: currentRootKey });

  try {
    while (stack.length > 0) {
      const frame = stack.pop();
      currentRootKey = frame.rootKey;
      if (frame.kind === "exit") {
        active.delete(frame.source);
        continue;
      }

      nodeCount += 1;
      if (nodeCount > EVIDENCE_SNAPSHOT_LIMITS.maxNodes) return invalid("node_limit");
      if (frame.depth > EVIDENCE_SNAPSHOT_LIMITS.maxDepth) return invalid("depth_limit");

      const type = typeof frame.source;
      if (frame.source === null || type === "string" || type === "boolean") {
        assign(frame.parent, frame.key, frame.source);
        continue;
      }
      if (type === "number") {
        if (!Number.isFinite(frame.source)) return invalid("non_finite_number");
        assign(frame.parent, frame.key, frame.source);
        continue;
      }
      if (type !== "object") return invalid("unsupported_value");
      // Node exposes Proxy identity without consulting the handler. Reject it
      // before Array.isArray, prototype checks, key enumeration, or descriptor
      // reads so hostile traps cannot execute anywhere in the evidence graph.
      if (utilTypes.isProxy(frame.source)) return invalid("proxy");
      if (active.has(frame.source)) return invalid("cycle");

      let target;
      let children;
      if (Array.isArray(frame.source)) {
        const lengthDescriptor = Object.getOwnPropertyDescriptor(frame.source, "length");
        if (!lengthDescriptor || !Object.hasOwn(lengthDescriptor, "value")
          || !Number.isSafeInteger(lengthDescriptor.value) || lengthDescriptor.value < 0) return invalid("invalid_array_length");
        const length = lengthDescriptor.value;
        // Check length before enumerating keys or allocating the snapshot, so
        // sparse hostile arrays cannot force work proportional to their length.
        if (length > EVIDENCE_SNAPSHOT_LIMITS.maxArrayLength) return invalid("array_length_limit");
        const names = Reflect.ownKeys(frame.source);
        if (names.some((key) => typeof key === "symbol")) return invalid("symbol_key");
        if (names.length !== length + 1) return invalid("sparse_or_extended_array");
        target = new Array(length);
        children = [];
        for (let index = 0; index < length; index += 1) {
          const key = String(index);
          const descriptor = Object.getOwnPropertyDescriptor(frame.source, key);
          if (!descriptor || !descriptor.enumerable || !Object.hasOwn(descriptor, "value")) return invalid("invalid_array_element");
          children.push([key, descriptor.value]);
        }
      } else {
        if (Object.getPrototypeOf(frame.source) !== Object.prototype) return invalid("non_plain_object");
        // For ordinary objects the source already owns its keys. Materialize
        // exactly one key array, enforce its bound immediately, then derive
        // the string/symbol validation from that same enumeration.
        const names = Reflect.ownKeys(frame.source);
        if (names.length > EVIDENCE_SNAPSHOT_LIMITS.maxObjectKeys) return invalid("object_key_limit");
        if (names.some((key) => typeof key === "symbol")) return invalid("symbol_key");
        target = {};
        children = [];
        for (const key of names) {
          if (frame.depth === 0) currentRootKey = key;
          const descriptor = Object.getOwnPropertyDescriptor(frame.source, key);
          if (!descriptor || !descriptor.enumerable || !Object.hasOwn(descriptor, "value")) {
            return invalid("accessor_or_hidden_property");
          }
          children.push([key, descriptor.value]);
        }
      }

      assign(frame.parent, frame.key, target);
      active.add(frame.source);
      stack.push({ kind: "exit", source: frame.source, rootKey: frame.rootKey });
      for (let index = children.length - 1; index >= 0; index -= 1) {
        const [key, child] = children[index];
        stack.push({
          kind: "visit",
          source: child,
          parent: target,
          key,
          depth: frame.depth + 1,
          rootKey: frame.depth === 0 ? key : frame.rootKey,
        });
      }
    }
  } catch {
    return invalid("descriptor_failure");
  }
  return { ok: true, value: root };
}

function references(candidates, extra = []) {
  return [...new Set([...candidates.map((candidate) => candidate?.ref), ...extra].filter((ref) => typeof ref === "string" && ref.length > 0))].sort();
}

function result(id, status, code, expected, actual, candidates = [], extraRefs = []) {
  return { id, status, code, expected: serializable(expected), actual: serializable(actual), evidenceRefs: references(candidates, extraRefs) };
}

function candidatesOf(evidence) {
  return Array.isArray(evidence?.candidates) ? evidence.candidates.filter((candidate) => candidate && typeof candidate === "object") : [];
}

function hasCapability(evidence, capability) {
  return Array.isArray(evidence?.capabilities) && evidence.capabilities.includes(capability);
}

function aggregate(id, expected, candidates, evaluate, extraRefs = []) {
  const byRef = candidates
    .map((candidate) => ({ ref: typeof candidate.ref === "string" ? candidate.ref : null, actual: evaluate(candidate) }))
    .sort((left, right) => String(left.ref ?? "").localeCompare(String(right.ref ?? ""))
      || canonical(left.actual).localeCompare(canonical(right.actual)));
  const highest = Math.max(...byRef.map((entry) => STATUS_WEIGHT[entry.actual.status] ?? 2));
  const selected = byRef
    .filter((entry) => (STATUS_WEIGHT[entry.actual.status] ?? 2) === highest)
    .sort((left, right) => String(left.actual.code).localeCompare(String(right.actual.code))
      || String(left.ref ?? "").localeCompare(String(right.ref ?? ""))
      || canonical(left.actual.actual).localeCompare(canonical(right.actual.actual)))[0];
  return result(id, selected.actual.status, selected.actual.code, expected, { byRef }, candidates, extraRefs);
}

function mimeOf(candidate) {
  return candidate?.mime && typeof candidate.mime === "object" ? candidate.mime : null;
}

function threadToken(value) {
  if (typeof value !== "string" || /[\r\n\u0000-\u001F\u007F]/.test(value)) return null;
  const match = /(?:^|[ \t])<([^<>\s\u0000-\u001F\u007F]+)>(?=$|[ \t])/.exec(value);
  if (match) return match[1];
  return /[<>\s]/.test(value) ? null : value;
}

function stripReplyPrefixes(subject) {
  return subject.replace(/^(?:Re:[ \t]*)+/i, "");
}

function forwardRemainder(subject) {
  const match = subject.match(/^(?:Fwd|Fw):[ \t]*/i);
  return match ? subject.slice(match[0].length) : null;
}

function containsHeaderInjection(value) {
  if (typeof value === "string") return /[\r\n\u0000]/.test(value);
  if (Array.isArray(value)) return value.some(containsHeaderInjection);
  return false;
}

function compileRegex(value) {
  if (value instanceof RegExp) return value;
  if (typeof value !== "string" || value.length > 512) return null;
  try { return new RegExp(value); } catch { return null; }
}

function expectedAttachments(value) {
  if (!Array.isArray(value)) return null;
  return value.map((entry) => {
    if (typeof entry === "string") return entry.length > 0 ? { filename: entry } : null;
    if (!entry || typeof entry !== "object" || Array.isArray(entry) || Object.getPrototypeOf(entry) !== Object.prototype) return null;
    const keys = Object.keys(entry);
    if (keys.length === 0) return null;
    const allowed = new Set(["filename", "contentType", "disposition", "sizeBytes", "sha256"]);
    if (keys.some((key) => !allowed.has(key))) return null;
    for (const key of keys) {
      const field = entry[key];
      if (field === undefined) return null;
      if (key === "sizeBytes") {
        if (!Number.isSafeInteger(field) || field < 0) return null;
      } else if (typeof field !== "string" || field.length === 0) return null;
    }
    return entry;
  });
}

function normalizedAttachments(value) {
  if (!Array.isArray(value)) return null;
  return value.map((entry) => entry && typeof entry === "object" ? {
    filename: entry.filename ?? null,
    contentType: entry.contentType ?? null,
    disposition: entry.disposition ?? null,
    sizeBytes: Number.isSafeInteger(entry.sizeBytes) && entry.sizeBytes >= 0 ? entry.sizeBytes : null,
    sha256: typeof entry.sha256 === "string" ? entry.sha256 : null,
  } : null);
}

function attachmentMatch(expected, actual) {
  if (!Array.isArray(actual) || actual.some((entry) => entry === null)) return { status: "error", code: "missing_attachment_evidence", actual };
  if (expected.length === 0) return { status: actual.length === 0 ? "pass" : "fail", code: actual.length === 0 ? "matched" : "attachment_set_mismatch", actual };
  if (expected.length !== actual.length) return { status: "fail", code: "attachment_set_mismatch", actual };
  const matches = expected.every((entry, index) => entry && Object.entries(entry).every(([key, value]) => actual[index]?.[key] === value));
  return { status: matches ? "pass" : "fail", code: matches ? "matched" : "attachment_set_mismatch", actual };
}

function timestamp(value) {
  if (typeof value !== "string") return { error: "missing_timing_evidence" };
  const match = value.match(/^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})$/);
  if (!match) return { error: "invalid_timestamp" };
  const [year, month, day, hour, minute, second, fraction = "", zone] = match.slice(1);
  // RFC3339 permits arbitrary fractional precision. Arithmetic is millisecond
  // based, so deliberately truncate (never round) beyond three digits.
  const millisecond = fraction.slice(0, 3).padEnd(3, "0");
  const values = [year, month, day, hour, minute, second, millisecond].map(Number);
  if (values.some((entry) => !Number.isSafeInteger(entry))) return { error: "invalid_timestamp" };
  const [yearNumber, monthNumber, dayNumber, hourNumber, minuteNumber, secondNumber, millisecondNumber] = values;
  if (monthNumber < 1 || monthNumber > 12 || dayNumber < 1 || hourNumber > 23 || minuteNumber > 59 || secondNumber > 60 || millisecondNumber > 999) return { error: "invalid_timestamp" };
  const local = new Date(0);
  local.setUTCFullYear(yearNumber, monthNumber - 1, dayNumber);
  // Date normalizes :60, so validate the civil date at :59 then add one
  // second to represent a legal RFC3339 leap second deterministically.
  const civilSecond = secondNumber === 60 ? 59 : secondNumber;
  local.setUTCHours(hourNumber, minuteNumber, civilSecond, millisecondNumber);
  if (local.getUTCFullYear() !== yearNumber || local.getUTCMonth() !== monthNumber - 1 || local.getUTCDate() !== dayNumber
    || local.getUTCHours() !== hourNumber || local.getUTCMinutes() !== minuteNumber || local.getUTCSeconds() !== civilSecond
    || local.getUTCMilliseconds() !== millisecondNumber) return { error: "invalid_timestamp" };
  let offsetMinutes = 0;
  if (zone !== "Z") {
    const sign = zone[0] === "+" ? 1 : -1;
    const offsetHour = Number(zone.slice(1, 3));
    const offsetMinute = Number(zone.slice(4, 6));
    if (offsetHour > 23 || offsetMinute > 59) return { error: "invalid_timestamp" };
    offsetMinutes = sign * (offsetHour * 60 + offsetMinute);
  }
  const milliseconds = local.getTime() + (secondNumber === 60 ? 1_000 : 0) - offsetMinutes * 60_000;
  if (!Number.isSafeInteger(milliseconds) || !Number.isFinite(milliseconds)) return { error: "invalid_timestamp" };
  if (secondNumber === 60 && !LEAP_SECOND_UTC_INSTANTS.has(milliseconds - millisecondNumber)) return { error: "invalid_timestamp" };
  return { milliseconds };
}

function receiptState(evidence, candidates) {
  if (!Object.hasOwn(evidence ?? {}, "actorReceipt")) return { status: "error", code: "missing_actor_receipt_evidence", actual: null, refs: [] };
  const receipt = evidence.actorReceipt;
  if (receipt === null) return { status: "valid", present: false, actual: false, refs: [] };
  if (!receipt || typeof receipt !== "object" || Array.isArray(receipt)
    || typeof receipt.ref !== "string" || receipt.ref.length === 0
    || typeof receipt.messageId !== "string" || receipt.messageId.length === 0) {
    return { status: "error", code: "invalid_actor_receipt_evidence", actual: null, refs: [] };
  }
  if (timestamp(receipt.observedAt).error) return { status: "error", code: "invalid_actor_receipt_evidence", actual: null, refs: [receipt.ref] };
  const matching = [];
  const seenRefs = new Map();
  for (const candidate of candidates) {
    if (candidate.messageId !== receipt.messageId) continue;
    const ref = typeof candidate.ref === "string" && candidate.ref.length > 0 ? candidate.ref : null;
    if (ref === null) {
      matching.push(candidate);
      continue;
    }
    const previous = seenRefs.get(ref);
    if (previous === undefined) {
      seenRefs.set(ref, candidate);
      matching.push(candidate);
    } else if (canonical(previous) !== canonical(candidate)) {
      matching.push(candidate);
    }
  }
  if (matching.length === 0) {
    return { status: "fail", code: "unexpected_actor_receipt", actual: { messageId: receipt.messageId }, refs: [receipt.ref] };
  }
  if (matching.length !== 1) {
    return { status: "fail", code: "ambiguous_actor_receipt", actual: { messageId: receipt.messageId, refs: references(matching) }, refs: [receipt.ref] };
  }
  return { status: "valid", present: true, actual: { messageId: receipt.messageId }, refs: [receipt.ref] };
}

/** Grade normalized MIME/content evidence without retaining raw MIME. */
export function gradeContent(expectation = {}, evidence = {}, { replayRedactions = false } = {}) {
  const snapshot = snapshotJsonSafePlainData(evidence);
  if (!snapshot.ok) {
    // A root Proxy is opaque: classifying it as candidate or actor-receipt
    // evidence would itself require invoking a trap. Ordinary roots retain
    // actorReceipt-specific errors because their top-level path is known.
    const code = snapshot.reason === "proxy" && snapshot.rootKey === null
      ? "malformed_evidence_proxy"
      : snapshot.rootKey === "actorReceipt"
        ? "invalid_actor_receipt_evidence"
        : "malformed_candidate_evidence";
    // Do not touch the expectation on this path. It may alias the same hostile
    // evidence object, and the snapshot error must remain safe to construct.
    return [result("lifecycle.actor_received", "error", code, null, null)];
  }
  evidence = snapshot.value;
  const candidates = candidatesOf(evidence);
  if (candidates.length === 0) return [];
  const results = [];
  const thread = expectation.thread;
  if (thread) {
    if (!hasCapability(evidence, "thread_headers")) {
      for (const id of [["messageId", "thread.message_id"], ["inReplyTo", "thread.in_reply_to"], ["references", "thread.references"], ["conversation", "thread.conversation"]]) {
        if (thread[id[0]] !== undefined) results.push(result(id[1], "error", "missing_thread_headers_evidence", thread[id[0]], null, candidates));
      }
    } else {
      if (thread.messageId !== undefined) results.push(aggregate("thread.message_id", thread.messageId, candidates, (candidate) => {
        const actual = threadToken(mimeOf(candidate)?.messageId);
        return actual
          ? { status: "pass", code: "matched", actual }
          : { status: "error", code: "missing_message_id_evidence", actual: null };
      }));
      if (thread.inReplyTo !== undefined) results.push(aggregate("thread.in_reply_to", thread.inReplyTo, candidates, (candidate) => {
        const needed = threadToken(evidence?.stimulus?.rfcMessageId);
        const found = threadToken(mimeOf(candidate)?.inReplyTo);
        if (!needed || !found) return { status: "error", code: "missing_thread_header_evidence", actual: { expected: needed, actual: found } };
        return { status: needed === found ? "pass" : "fail", code: needed === found ? "matched" : "wrong_in_reply_to", actual: found };
      }));
      if (thread.references !== undefined) results.push(aggregate("thread.references", thread.references, candidates, (candidate) => {
        const needed = threadToken(evidence?.stimulus?.rfcMessageId);
        const actual = Array.isArray(mimeOf(candidate)?.references) ? mimeOf(candidate).references.map(threadToken).filter(Boolean) : null;
        if (!needed || actual === null) return { status: "error", code: "missing_thread_header_evidence", actual };
        return { status: actual.includes(needed) ? "pass" : "fail", code: actual.includes(needed) ? "matched" : "missing_original_reference", actual };
      }));
      if (thread.conversation !== undefined) results.push(aggregate("thread.conversation", thread.conversation, candidates, (candidate) => {
        const original = evidence?.stimulus?.conversationId;
        const actual = candidate.conversationId;
        if (typeof original !== "string" || typeof actual !== "string") return { status: "error", code: "missing_conversation_evidence", actual };
        return { status: original === actual ? "pass" : "fail", code: original === actual ? "matched" : "wrong_conversation", actual };
      }));
    }
  }

  const subject = expectation.subject;
  if (subject) {
    if (!hasCapability(evidence, "raw_mime")) {
      for (const id of ["subject.exact", "subject.regex", "subject.policy", "subject.required_fragments", "subject.forbidden_fragments", "subject.no_header_injection"]) {
        const key = id === "subject.exact" ? "exact" : id === "subject.required_fragments" ? "requiredFragments" : id === "subject.forbidden_fragments" ? "forbiddenFragments" : id.split(".")[1];
        if (key === "no_header_injection" || subject[key] !== undefined) results.push(result(id, "error", "missing_raw_mime_evidence", subject[key] ?? "safe headers", null, candidates));
      }
    } else {
      const withSubject = (candidate) => {
        const value = mimeOf(candidate)?.subject;
        return typeof value === "string" ? { value } : { error: { status: "error", code: "missing_subject_evidence", actual: null } };
      };
      if (subject.exact !== undefined) results.push(aggregate("subject.exact", subject.exact, candidates, (candidate) => {
        const found = withSubject(candidate); if (found.error) return found.error;
        return { status: found.value === subject.exact ? "pass" : "fail", code: found.value === subject.exact ? "matched" : "subject_mismatch", actual: found.value };
      }));
      if (subject.regex !== undefined) results.push(aggregate("subject.regex", subject.regex, candidates, (candidate) => {
        const found = withSubject(candidate); if (found.error) return found.error;
        const regex = compileRegex(subject.regex);
        if (!regex) return { status: "error", code: "invalid_subject_regex", actual: null };
        const matches = regex.test(found.value);
        return { status: matches ? "pass" : "fail", code: matches ? "matched" : "subject_regex_mismatch", actual: found.value };
      }));
      if (subject.policy !== undefined) results.push(aggregate("subject.policy", subject.policy, candidates, (candidate) => {
        const found = withSubject(candidate); if (found.error) return found.error;
        const original = evidence?.stimulus?.subject;
        if (typeof original !== "string") return { status: "error", code: "missing_original_subject", actual: null };
        if (subject.policy === "preserve") {
          const matches = stripReplyPrefixes(original) === stripReplyPrefixes(found.value);
          return { status: matches ? "pass" : "fail", code: matches ? "matched" : "subject_policy_mismatch", actual: found.value };
        }
        const remainder = forwardRemainder(found.value);
        if (remainder === null) return { status: "fail", code: "missing_forward_prefix", actual: found.value };
        const matches = remainder === original;
        return { status: matches ? "pass" : "fail", code: matches ? "matched" : "subject_policy_mismatch", actual: found.value };
      }));
      for (const [key, id, forbidden] of [["requiredFragments", "subject.required_fragments", false], ["forbiddenFragments", "subject.forbidden_fragments", true]]) {
        if (subject[key] === undefined) continue;
        results.push(aggregate(id, subject[key], candidates, (candidate) => {
          const found = withSubject(candidate); if (found.error) return found.error;
          const matched = subject[key].filter((fragment) => found.value.includes(fragment));
          const passes = forbidden ? matched.length === 0 : matched.length === subject[key].length;
          return { status: passes ? "pass" : "fail", code: passes ? "matched" : forbidden ? "forbidden_fragment_matched" : "required_fragment_missing", actual: { subject: found.value, matched } };
        }));
      }
      results.push(aggregate("subject.no_header_injection", "safe headers", candidates, (candidate) => {
        const mime = mimeOf(candidate);
        if (!mime) return { status: "error", code: "missing_mime_evidence", actual: null };
        const injected = [mime.subject, mime.messageId, mime.inReplyTo, mime.references].some(containsHeaderInjection);
        return { status: injected ? "fail" : "pass", code: injected ? "header_injection_detected" : "matched", actual: injected };
      }));
    }
  }

  const body = expectation.body;
  if (body) {
    if (!hasCapability(evidence, "raw_mime")) {
      for (const [key, id] of [["requiredFacts", "body.required_facts"], ["forbiddenPatterns", "body.forbidden_patterns"], ["plainText", "body.plain_text"], ["maxSize", "body.max_size"]]) {
        if (body[key] !== undefined) results.push(result(id, "error", "missing_raw_mime_evidence", body[key], null, candidates));
      }
    } else {
      const text = (candidate) => {
        const mime = mimeOf(candidate);
        if (!mime || !Object.hasOwn(mime, "text")) return { error: { status: "error", code: "missing_plain_text_evidence", actual: null } };
        if (mime.text !== null && typeof mime.text !== "string") return { error: { status: "error", code: "invalid_plain_text_evidence", actual: null } };
        return { value: mime.text };
      };
      if (body.requiredFacts !== undefined) results.push(aggregate("body.required_facts", body.requiredFacts, candidates, (candidate) => {
        const found = text(candidate); if (found.error) return found.error;
        const missing = found.value === null ? [...body.requiredFacts] : body.requiredFacts.filter((fact) => !found.value.includes(fact));
        return { status: missing.length === 0 ? "pass" : "fail", code: missing.length === 0 ? "matched" : "required_fact_missing", actual: { missing } };
      }));
      if (body.forbiddenPatterns !== undefined) results.push(aggregate("body.forbidden_patterns", body.forbiddenPatterns, candidates, (candidate) => {
        const found = text(candidate); if (found.error) return found.error;
        const patterns = body.forbiddenPatternRegexes ?? body.forbiddenPatterns.map((pattern) => compileRegex(pattern));
        if (patterns.some((pattern) => !pattern)) return { status: "error", code: "invalid_forbidden_pattern", actual: null };
        const replayDigests = replayRedactions && Array.isArray(mimeOf(candidate)?.textRedactions?.forbiddenPatternDigests)
          ? new Set(mimeOf(candidate).textRedactions.forbiddenPatternDigests) : new Set();
        const matched = found.value === null ? [] : patterns.map((pattern, index) => ({
          pattern: body.forbiddenPatterns[index],
          matched: pattern.test(found.value) || replayDigests.has(
            createHash("sha256").update(body.forbiddenPatterns[index]).digest("hex"),
          ),
        })).filter((entry) => entry.matched).map((entry) => entry.pattern);
        return { status: matched.length === 0 ? "pass" : "fail", code: matched.length === 0 ? "matched" : "forbidden_pattern_matched", actual: { matched } };
      }));
      if (body.plainText !== undefined) results.push(aggregate("body.plain_text", body.plainText, candidates, (candidate) => {
        const found = text(candidate); if (found.error) return found.error;
        const present = typeof found.value === "string" && found.value.length > 0;
        const passes = body.plainText === "required" ? present : !present;
        return { status: passes ? "pass" : "fail", code: passes ? "matched" : body.plainText === "required" ? "plain_text_required" : "plain_text_forbidden", actual: present };
      }));
      if (body.maxSize !== undefined) results.push(aggregate("body.max_size", body.maxSize, candidates, (candidate) => {
        const sizeBytes = mimeOf(candidate)?.sizeBytes;
        if (!Number.isSafeInteger(sizeBytes) || sizeBytes < 0) return { status: "error", code: "missing_size_evidence", actual: sizeBytes ?? null };
        return { status: sizeBytes <= body.maxSize ? "pass" : "fail", code: sizeBytes <= body.maxSize ? "matched" : "body_too_large", actual: sizeBytes };
      }));
    }
  }

  if (expectation.attachments?.exactly !== undefined) {
    if (!hasCapability(evidence, "attachment_hashes")) results.push(result("attachments.exactly", "error", "missing_attachment_hash_evidence", expectation.attachments.exactly, null, candidates));
    else {
      const expected = expectedAttachments(expectation.attachments.exactly);
      results.push(aggregate("attachments.exactly", expectation.attachments.exactly, candidates, (candidate) => {
        if (!expected || expected.some((entry) => entry === null)) return { status: "error", code: "invalid_attachment_expectation", actual: null };
        return attachmentMatch(expected, normalizedAttachments(mimeOf(candidate)?.attachments));
      }));
    }
  }

  if (expectation.timing?.replyWithinMs !== undefined) {
    results.push(aggregate("timing.reply_within", expectation.timing.replyWithinMs, candidates, (candidate) => {
      const start = timestamp(evidence?.stimulus?.receivedAt ?? evidence?.timings?.targetReceivedAt);
      const end = timestamp(candidate.observedAt ?? candidate.sentAt);
      if (start.error || end.error) return { status: "error", code: start.error === "invalid_timestamp" || end.error === "invalid_timestamp" ? "invalid_timestamp" : "missing_timing_evidence", actual: { start: evidence?.stimulus?.receivedAt ?? null, end: candidate.observedAt ?? candidate.sentAt ?? null } };
      const elapsedMs = end.milliseconds - start.milliseconds;
      return { status: elapsedMs <= expectation.timing.replyWithinMs && elapsedMs >= 0 ? "pass" : "fail", code: elapsedMs <= expectation.timing.replyWithinMs && elapsedMs >= 0 ? "matched" : "reply_too_late", actual: { elapsedMs } };
    }));
  }

  const lifecycle = expectation.lifecycle;
  if (lifecycle) {
    if (!hasCapability(evidence, "delivery_lifecycle")) {
      if (lifecycle.submission !== undefined) results.push(result("lifecycle.submission", "error", "missing_delivery_lifecycle_evidence", lifecycle.submission, null, candidates));
      if (lifecycle.actorReceived !== undefined) results.push(result("lifecycle.actor_received", "error", "missing_delivery_lifecycle_evidence", lifecycle.actorReceived, null, candidates));
    } else {
      if (lifecycle.submission !== undefined) results.push(aggregate("lifecycle.submission", lifecycle.submission, candidates, (candidate) => {
        if (!candidate.lifecycle || typeof candidate.lifecycle !== "object" || Array.isArray(candidate.lifecycle) || !Object.hasOwn(candidate.lifecycle, "submission")) {
          return { status: "error", code: "missing_lifecycle_evidence", actual: null };
        }
        const actual = candidate.lifecycle.submission;
        if (typeof actual !== "string") return { status: "error", code: "invalid_lifecycle_evidence", actual: null };
        return { status: actual === lifecycle.submission ? "pass" : "fail", code: actual === lifecycle.submission ? "matched" : "submission_state_mismatch", actual };
      }));
      if (lifecycle.actorReceived !== undefined) {
        const state = receiptState(evidence, candidates);
        let status;
        let code;
        if (state.status === "error") ({ status, code } = state);
        else if (state.status === "fail") ({ status, code } = state);
        else if (state.present === lifecycle.actorReceived) ({ status, code } = { status: "pass", code: "matched" });
        else ({ status, code } = { status: "fail", code: lifecycle.actorReceived ? "actor_receipt_missing" : "unexpected_actor_receipt" });
        results.push(result("lifecycle.actor_received", status, code, lifecycle.actorReceived, state.actual, candidates, state.refs));
      }
    }
  }
  return results;
}
