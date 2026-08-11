import { NormalizationError, normalizeMailbox } from "./normalize.mjs";

const ATTEMPT_EVENTS = new Set(["email.sent", "email.failed", "email.blocked", "email.review_requested"]);
const RECIPIENT_FIELDS = ["to", "cc", "bcc"];
const MESSAGE_TYPES = new Map([["send", "new_message"], ["reply", "reply"], ["forward", "forward"]]);

function serializable(value) {
  return value === undefined ? null : JSON.parse(JSON.stringify(value));
}

function result(id, status, code, expected, actual, candidates = []) {
  return {
    id,
    status,
    code,
    expected: serializable(expected),
    actual: serializable(actual),
    evidenceRefs: [...new Set(candidates.map((candidate) => candidate?.ref).filter((ref) => typeof ref === "string" && ref.length > 0))].sort(),
  };
}

function mailbox(value) {
  try {
    return normalizeMailbox(value).address;
  } catch (error) {
    if (error instanceof NormalizationError) return null;
    throw error;
  }
}

function mailboxWithDisplayName(value) {
  try {
    return normalizeMailbox(value);
  } catch (error) {
    if (error instanceof NormalizationError) return null;
    throw error;
  }
}

function addressList(values) {
  if (!Array.isArray(values)) return { addresses: [], invalid: values === undefined ? [] : [values] };
  const addresses = [];
  const invalid = [];
  for (const value of values) {
    const address = mailbox(value);
    if (address === null) invalid.push(value);
    else addresses.push(address);
  }
  return { addresses, invalid };
}

function addressSet(values) {
  return [...new Set(addressList(values).addresses)].sort();
}

function sameSet(left, right) {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

function hasVerifiedOutboundProvenance(candidate) {
  return candidate.direction === "outbound" && candidate.provenance === "target_outbound";
}

function boundedToken(value) {
  return typeof value === "string" && /^[a-z][a-z0-9_]{0,63}$/.test(value) ? value : null;
}

function canonical(value) {
  if (value === null) return "null";
  if (value === undefined) return "undefined";
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (typeof value === "object") return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`).join(",")}}`;
  return JSON.stringify(value);
}

function ordered(candidates) {
  return candidates
    .map((candidate, index) => ({ candidate, index }))
    .sort((left, right) => {
      const leftTime = typeof left.candidate.observedAt === "string" ? left.candidate.observedAt : "";
      const rightTime = typeof right.candidate.observedAt === "string" ? right.candidate.observedAt : "";
      return leftTime.localeCompare(rightTime)
        || String(left.candidate.ref ?? "").localeCompare(String(right.candidate.ref ?? ""))
        || left.index - right.index;
    })
    .map(({ candidate }) => candidate);
}

function observations(evidence) {
  const outbound = [];
  const unverified = [];
  for (const candidate of Array.isArray(evidence?.candidates) ? evidence.candidates : []) {
    if (!candidate || typeof candidate !== "object" || !ATTEMPT_EVENTS.has(candidate.eventType)) continue;
    if (hasVerifiedOutboundProvenance(candidate)) outbound.push(candidate);
    else unverified.push(candidate);
  }
  const byRef = new Map();
  const attempts = [];
  const conflicts = [];
  for (const candidate of ordered(outbound)) {
    if (typeof candidate.ref !== "string" || candidate.ref.length === 0) {
      attempts.push(candidate); // Ref-less observations remain distinct attempts.
      continue;
    }
    const prior = byRef.get(candidate.ref);
    if (!prior) {
      byRef.set(candidate.ref, candidate);
      attempts.push(candidate);
    } else if (canonical(prior) !== canonical(candidate)) {
      conflicts.push(candidate.ref);
    }
  }
  return { attempts: ordered(attempts), unverified: ordered(unverified), conflicts: [...new Set(conflicts)].sort() };
}

function expectedAddresses(specification) {
  return addressSet(specification?.exactly ?? []);
}

function recipientAddresses(candidate) {
  return RECIPIENT_FIELDS.flatMap((field) => addressList(candidate[field]).addresses);
}

function participantSet(evidence) {
  if (!Array.isArray(evidence?.stimulus?.participants)) return { available: false, addresses: [] };
  return { available: true, addresses: addressSet(evidence.stimulus.participants) };
}

function actionKind(candidate, participants) {
  const kind = MESSAGE_TYPES.get(candidate.messageType);
  if (kind !== "reply") return kind ?? null;
  if (!participants.available) return "reply";
  if (participants.addresses.length <= 1) return "reply";
  const recipients = new Set(recipientAddresses(candidate));
  return participants.addresses.every((participant) => recipients.has(participant)) ? "reply_all" : "reply";
}

function fieldPlacement(expectation, candidate) {
  const expectedByAddress = new Map();
  for (const field of RECIPIENT_FIELDS) {
    for (const address of expectedAddresses(expectation?.recipients?.[field])) {
      if (!expectedByAddress.has(address)) expectedByAddress.set(address, field);
    }
  }
  const movements = [];
  for (const field of RECIPIENT_FIELDS) {
    for (const address of addressList(candidate[field]).addresses) {
      const expectedField = expectedByAddress.get(address);
      if (expectedField && expectedField !== field) movements.push({ address, expectedField, actualField: field });
    }
  }
  return movements.sort((left, right) => left.address.localeCompare(right.address) || left.actualField.localeCompare(right.actualField));
}

function recipientAssertion(field, expected, candidate, movements) {
  const observed = addressList(candidate[field]);
  const actual = [...new Set(observed.addresses)].sort();
  const duplicates = [...new Set(observed.addresses.filter((address, index) => observed.addresses.indexOf(address) !== index))].sort();
  const expectedSet = expectedAddresses(expected);
  const fieldMovements = movements.filter((movement) => movement.actualField === field);
  const original = Array.isArray(candidate[field]) ? [...candidate[field]] : [];
  const detail = { original, addresses: actual, duplicates, invalid: observed.invalid, movements: fieldMovements };
  if (fieldMovements.length > 0) return { status: "fail", code: "recipient_cross_field", actual: detail };
  if (duplicates.length > 0) return { status: "fail", code: "duplicate_recipient", actual: detail };
  const missing = expectedSet.filter((address) => !actual.includes(address));
  const unexpected = actual.filter((address) => !expectedSet.includes(address));
  if (missing.length > 0 && unexpected.length === 0) return { status: "fail", code: "missing_recipient", actual: { ...detail, missing } };
  if (unexpected.length > 0 && missing.length === 0 && expectedSet.length === 0) return { status: "fail", code: "unexpected_recipient", actual: { ...detail, unexpected } };
  if (!sameSet(expectedSet, actual) || observed.invalid.length > 0) return { status: "fail", code: "recipient_set_mismatch", actual: { ...detail, missing, unexpected } };
  return { status: "pass", code: "matched", actual: detail };
}

function envelopeAssertion(expected, candidate) {
  const assertion = recipientAssertion("envelopeRecipients", expected, { envelopeRecipients: candidate.envelopeRecipients }, []);
  if (assertion.code === "recipient_set_mismatch" && assertion.actual.missing?.length === 0 && assertion.actual.unexpected?.length > 0) {
    return { ...assertion, code: "unexpected_recipient" };
  }
  return assertion;
}

function hasRecipientField(candidate, field) {
  return Object.hasOwn(candidate, field) && Array.isArray(candidate[field]);
}

function requiredRecipientAssertion(field, expected, candidate, movements) {
  if (!hasRecipientField(candidate, field)) return { status: "error", code: "missing_recipient_evidence", actual: null };
  return recipientAssertion(field, expected, candidate, movements);
}

function requiredEnvelopeAssertion(expected, candidate) {
  if (!hasRecipientField(candidate, "envelopeRecipients")) return { status: "error", code: "missing_recipient_evidence", actual: null };
  return envelopeAssertion(expected, candidate);
}

function crossFieldAssertion(expectation, evidence, candidate) {
  if (!evidence.capabilities?.includes("visible_recipients")) return { status: "error", code: "missing_recipient_evidence", actual: null };
  if (!evidence.capabilities?.includes("blind_recipients")) return { status: "error", code: "missing_blind_recipient_evidence", actual: null };
  if (RECIPIENT_FIELDS.some((field) => !hasRecipientField(candidate, field))) return { status: "error", code: "missing_recipient_evidence", actual: null };
  const actual = fieldPlacement(expectation, candidate);
  return { status: actual.length === 0 ? "pass" : "fail", code: actual.length === 0 ? "matched" : "recipient_cross_field", actual };
}

function targetAddress(evidence) {
  return mailbox(evidence?.target?.email) ?? mailbox(evidence?.targetEmail) ?? mailbox(evidence?.target);
}

function diagnostic(candidate, actual) {
  return { ref: typeof candidate.ref === "string" && candidate.ref.length > 0 ? candidate.ref : null, actual };
}

function aggregate(id, expected, attempts, evaluate) {
  const byRef = attempts.map((candidate) => diagnostic(candidate, evaluate(candidate)));
  const severity = { pass: 0, fail: 1, error: 2 };
  const highest = Math.max(0, ...byRef.map((entry) => severity[entry.actual.status] ?? 2));
  const selected = byRef
    .filter((entry) => (severity[entry.actual.status] ?? 2) === highest)
    .sort((left, right) => String(left.actual.code).localeCompare(String(right.actual.code))
      || String(left.ref ?? "").localeCompare(String(right.ref ?? ""))
      || canonical(left.actual.actual).localeCompare(canonical(right.actual.actual)))[0];
  return result(id, selected?.actual.status ?? "pass", selected?.actual.code ?? "matched", expected, { byRef }, attempts);
}

function blockedAssertions(expectation, evidence, attempts, code, actual) {
  const results = [];
  for (const id of ["action.kind", "action.count", "action.no_duplicates"]) {
    results.push(result(id, "error", code, expectation.action ?? { kind: "none", count: 0 }, actual, attempts));
  }
  if (expectation.sender?.exactly !== undefined) results.push(result("sender.from", "error", code, expectation.sender.exactly, actual, attempts));
  if (expectation.sender?.sentAs !== undefined) results.push(result("sender.sent_as", "error", code, expectation.sender.sentAs, actual, attempts));
  if (expectation.sender?.replyTo !== undefined) results.push(result("sender.reply_to", "error", code, expectedAddresses(expectation.sender.replyTo), actual, attempts));
  if (expectation.sender?.displayName !== undefined) results.push(result("sender.display_name", "error", code, expectation.sender.displayName, actual, attempts));
  if (expectation.recipients) {
    for (const field of ["to", "cc", "bcc", "envelope"]) {
      if (expectation.recipients[field] !== undefined) results.push(result(`recipients.${field}`, "error", code, expectedAddresses(expectation.recipients[field]), actual, attempts));
    }
    results.push(result("recipients.cross_field", "error", code, "same recipient fields", actual, attempts));
    results.push(result("recipients.no_target_self", "error", code, targetAddress(evidence), actual, attempts));
  }
  return results;
}

/** Grade normalized outbound evidence without mutating it. */
export function gradeCore(expectation = {}, evidence = {}) {
  const { attempts, unverified, conflicts } = observations(evidence);
  if (conflicts.length > 0) return blockedAssertions(expectation, evidence, attempts, "conflicting_evidence_ref", { refs: conflicts });
  if (unverified.length > 0) return blockedAssertions(expectation, evidence, attempts, "missing_outbound_provenance", { count: attempts.length, refs: unverified.map((candidate) => candidate.ref ?? null) });

  const results = [];
  const expectedAction = expectation.action ?? { kind: "none", count: 0 };
  const expectedCount = Number.isSafeInteger(expectedAction.count) && expectedAction.count >= 0 ? expectedAction.count : 0;
  const participants = participantSet(evidence);
  const actualKinds = attempts.map((candidate) => actionKind(candidate, participants));
  const replyIsIndistinguishable = participants.available && participants.addresses.length <= 1;
  const kindMatches = (kind) => kind === expectedAction.kind
    || (expectedAction.kind === "reply_all" && replyIsIndistinguishable && kind === "reply");
  let kindStatus = "pass";
  let kindCode = "matched";
  if (expectedAction.kind === "reply_all" && !participants.available) {
    kindStatus = "error";
    kindCode = "missing_reply_all_participant_evidence";
  } else if (expectedAction.kind === "reply_all" && !replyIsIndistinguishable && actualKinds.includes("reply")) {
    kindStatus = "fail";
    kindCode = "reply_all_participants_missing";
  } else if (expectedAction.kind !== "none" && (actualKinds.length === 0 || actualKinds.some((kind) => !kindMatches(kind)))) {
    kindStatus = "fail";
    kindCode = "action_kind_mismatch";
  } else if (expectedAction.kind === "none" && actualKinds.length > 0) {
    kindStatus = "fail";
    kindCode = "unexpected_outbound_attempt";
  }
  results.push(result("action.kind", kindStatus, kindCode, expectedAction.kind, actualKinds, attempts));
  const countMatches = attempts.length === expectedCount;
  results.push(result("action.count", countMatches ? "pass" : "fail", countMatches ? "matched" : expectedCount === 0 && attempts.length > 0 ? "unexpected_outbound_attempt" : "action_count_mismatch", expectedCount, attempts.length, attempts));
  const duplicate = expectedCount > 0 && attempts.length > expectedCount;
  results.push(result("action.no_duplicates", duplicate ? "fail" : "pass", duplicate ? "duplicate_outbound_attempt" : "matched", expectedCount, attempts.length, attempts));

  if (expectation.sender?.exactly !== undefined) results.push(aggregate("sender.from", mailbox(expectation.sender.exactly), attempts, (candidate) => {
    const actual = mailbox(candidate.from); return { status: actual === mailbox(expectation.sender.exactly) ? "pass" : "fail", code: actual === mailbox(expectation.sender.exactly) ? "matched" : "sender_mismatch", actual };
  }));
  if (expectation.sender?.sentAs !== undefined) results.push(aggregate("sender.sent_as", expectation.sender.sentAs, attempts, (candidate) => {
    if (!Object.hasOwn(candidate, "sentAs") || candidate.sentAs === null || candidate.sentAs === undefined) {
      return { status: "error", code: "missing_sent_as_evidence", actual: null };
    }
    const actual = boundedToken(candidate.sentAs);
    if (actual === null) return { status: "error", code: "invalid_sent_as_evidence", actual: null };
    return {
      status: actual === expectation.sender.sentAs ? "pass" : "fail",
      code: actual === expectation.sender.sentAs ? "matched" : "sent_as_mismatch",
      actual,
    };
  }));
  if (expectation.sender?.replyTo !== undefined) results.push(aggregate("sender.reply_to", expectedAddresses(expectation.sender.replyTo), attempts, (candidate) => {
    if (!Object.hasOwn(candidate, "replyTo") || !Array.isArray(candidate.replyTo)) {
      return { status: "error", code: "missing_reply_to_evidence", actual: null };
    }
    const actual = addressSet(candidate.replyTo); return { status: sameSet(expectedAddresses(expectation.sender.replyTo), actual) ? "pass" : "fail", code: sameSet(expectedAddresses(expectation.sender.replyTo), actual) ? "matched" : "reply_to_mismatch", actual };
  }));
  if (expectation.sender?.displayName !== undefined) results.push(aggregate("sender.display_name", expectation.sender.displayName, attempts, (candidate) => {
    const actual = mailboxWithDisplayName(candidate.from)?.displayName ?? null; return { status: actual === expectation.sender.displayName ? "pass" : "fail", code: actual === expectation.sender.displayName ? "matched" : "display_name_mismatch", actual };
  }));

  if (expectation.recipients) {
    for (const field of RECIPIENT_FIELDS) {
      if (expectation.recipients[field] === undefined) continue;
      if (["to", "cc"].includes(field) && !evidence.capabilities?.includes("visible_recipients")) {
        results.push(result(`recipients.${field}`, "error", "missing_recipient_evidence", expectedAddresses(expectation.recipients[field]), null, attempts));
      } else if (field === "bcc" && !evidence.capabilities?.includes("blind_recipients")) {
        results.push(result("recipients.bcc", "error", "missing_blind_recipient_evidence", expectedAddresses(expectation.recipients.bcc), null, attempts));
      } else {
        results.push(aggregate(`recipients.${field}`, expectedAddresses(expectation.recipients[field]), attempts, (candidate) => requiredRecipientAssertion(field, expectation.recipients[field], candidate, fieldPlacement(expectation, candidate))));
      }
    }
    if (expectation.recipients.envelope !== undefined) {
      if (!evidence.capabilities?.includes("envelope_recipients")) {
        results.push(result("recipients.envelope", "error", "missing_envelope_recipient_evidence", expectedAddresses(expectation.recipients.envelope), null, attempts));
      } else {
        results.push(aggregate("recipients.envelope", expectedAddresses(expectation.recipients.envelope), attempts, (candidate) => requiredEnvelopeAssertion(expectation.recipients.envelope, candidate)));
      }
    }
    results.push(aggregate("recipients.cross_field", "same recipient fields", attempts, (candidate) => crossFieldAssertion(expectation, evidence, candidate)));
    const target = targetAddress(evidence);
    results.push(aggregate("recipients.no_target_self", target, attempts, (candidate) => {
      if (target === null) return { status: "error", code: "missing_target_identity_evidence", actual: [] };
      if (!evidence.capabilities?.includes("visible_recipients")) return { status: "error", code: "missing_recipient_evidence", actual: null };
      if (!evidence.capabilities?.includes("blind_recipients")) return { status: "error", code: "missing_blind_recipient_evidence", actual: null };
      if (!evidence.capabilities?.includes("envelope_recipients")) return { status: "error", code: "missing_envelope_recipient_evidence", actual: null };
      if (!hasRecipientField(candidate, "to") || !hasRecipientField(candidate, "cc")
        || !hasRecipientField(candidate, "bcc") || !hasRecipientField(candidate, "envelopeRecipients")) {
        return { status: "error", code: "missing_recipient_evidence", actual: null };
      }
      const recipients = [...new Set([...recipientAddresses(candidate), ...addressList(candidate.envelopeRecipients).addresses].filter((address) => address === target))].sort();
      return { status: recipients.length === 0 ? "pass" : "fail", code: recipients.length === 0 ? "matched" : "target_self_recipient", actual: { recipients } };
    }));
  }
  return results;
}
