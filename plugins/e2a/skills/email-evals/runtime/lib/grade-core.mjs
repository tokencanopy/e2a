import { NormalizationError, normalizeMailbox } from "./normalize.mjs";

const ATTEMPT_EVENTS = new Set(["email.sent", "email.failed", "email.blocked", "email.review_requested"]);
const RECIPIENT_FIELDS = ["to", "cc", "bcc"];
const MESSAGE_TYPES = new Map([
  ["send", "new_message"],
  ["reply", "reply"],
  ["forward", "forward"],
]);

function serializable(value) {
  if (value === undefined) return null;
  return JSON.parse(JSON.stringify(value));
}

function result(id, status, code, expected, actual, candidates = []) {
  return {
    id,
    status,
    code,
    expected: serializable(expected),
    actual: serializable(actual),
    evidenceRefs: [...new Set(candidates.map((candidate) => candidate?.ref).filter((ref) => typeof ref === "string"))].sort(),
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

function attemptCandidate(candidate) {
  if (!candidate || typeof candidate !== "object" || !ATTEMPT_EVENTS.has(candidate.eventType)) return false;
  return candidate.eventType !== "email.review_requested" || (candidate.direction !== "inbound" && candidate.outbound !== false);
}

function orderedCandidates(evidence) {
  const candidates = Array.isArray(evidence?.candidates) ? evidence.candidates : [];
  return candidates
    .filter(attemptCandidate)
    .map((candidate, index) => ({ candidate, index }))
    .sort((left, right) => {
      const leftTime = typeof left.candidate.observedAt === "string" ? left.candidate.observedAt : "";
      const rightTime = typeof right.candidate.observedAt === "string" ? right.candidate.observedAt : "";
      const byTime = leftTime.localeCompare(rightTime);
      if (byTime !== 0) return byTime;
      const leftRef = typeof left.candidate.ref === "string" ? left.candidate.ref : "";
      const rightRef = typeof right.candidate.ref === "string" ? right.candidate.ref : "";
      return leftRef.localeCompare(rightRef) || left.index - right.index;
    })
    .map(({ candidate }) => candidate);
}

function expectedAddresses(specification) {
  return addressSet(specification?.exactly ?? []);
}

function recipientAddresses(candidate) {
  return RECIPIENT_FIELDS.flatMap((field) => addressList(candidate[field]).addresses);
}

function participantSet(evidence) {
  const participants = evidence?.stimulus?.participants;
  return addressSet(Array.isArray(participants) ? participants : []);
}

function actionKind(candidate, participants) {
  const kind = MESSAGE_TYPES.get(candidate.messageType);
  if (kind !== "reply") return kind ?? null;
  const recipients = new Set(recipientAddresses(candidate));
  return participants.length > 1 && participants.every((participant) => recipients.has(participant)) ? "reply_all" : "reply";
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

function targetAddress(evidence) {
  return mailbox(evidence?.target?.email) ?? mailbox(evidence?.targetEmail) ?? mailbox(evidence?.target);
}

/**
 * Grade normalized outbound evidence without mutating it. Every returned value is a
 * plain JSON value so it can be written directly to replay artifacts.
 */
export function gradeCore(expectation = {}, evidence = {}) {
  const attempts = orderedCandidates(evidence);
  const candidate = attempts[0] ?? {};
  const results = [];
  const expectedAction = expectation.action ?? { kind: "none", count: 0 };
  const expectedCount = Number.isSafeInteger(expectedAction.count) && expectedAction.count >= 0 ? expectedAction.count : 0;
  const participants = participantSet(evidence);
  const actualKinds = attempts.map((entry) => actionKind(entry, participants));

  let kindStatus = "pass";
  let kindCode = "matched";
  if (expectedAction.kind === "reply_all" && attempts.some((entry) => entry.messageType === "reply") && actualKinds.includes("reply")) {
    kindStatus = "fail";
    kindCode = "reply_all_participants_missing";
  } else if (expectedAction.kind !== "none" && (actualKinds.length === 0 || actualKinds.some((kind) => kind !== expectedAction.kind))) {
    kindStatus = "fail";
    kindCode = "action_kind_mismatch";
  } else if (expectedAction.kind === "none" && actualKinds.length > 0) {
    kindStatus = "fail";
    kindCode = "unexpected_outbound_attempt";
  }
  results.push(result("action.kind", kindStatus, kindCode, expectedAction.kind, actualKinds, attempts));

  const countMatches = attempts.length === expectedCount;
  results.push(result(
    "action.count",
    countMatches ? "pass" : "fail",
    countMatches ? "matched" : expectedCount === 0 && attempts.length > 0 ? "unexpected_outbound_attempt" : "action_count_mismatch",
    expectedCount,
    attempts.length,
    attempts,
  ));
  const duplicate = expectedCount > 0 && attempts.length > expectedCount;
  results.push(result("action.no_duplicates", duplicate ? "fail" : "pass", duplicate ? "duplicate_outbound_attempt" : "matched", expectedCount, attempts.length, attempts));

  if (expectation.sender?.exactly !== undefined) {
    const expected = mailbox(expectation.sender.exactly);
    const actual = mailbox(candidate.from);
    results.push(result("sender.from", expected === actual ? "pass" : "fail", expected === actual ? "matched" : "sender_mismatch", expected, actual, attempts));
  }
  if (expectation.sender?.sentAs !== undefined) {
    const expected = mailbox(expectation.sender.sentAs);
    const actual = mailbox(candidate.sentAs);
    results.push(result("sender.sent_as", expected === actual ? "pass" : "fail", expected === actual ? "matched" : "sent_as_mismatch", expected, actual, attempts));
  }
  if (expectation.sender?.replyTo !== undefined) {
    const expected = expectedAddresses(expectation.sender.replyTo);
    const actual = addressSet(candidate.replyTo ?? []);
    results.push(result("sender.reply_to", sameSet(expected, actual) ? "pass" : "fail", sameSet(expected, actual) ? "matched" : "reply_to_mismatch", expected, actual, attempts));
  }
  if (expectation.sender?.displayName !== undefined) {
    const actual = mailboxWithDisplayName(candidate.from)?.displayName ?? null;
    const expected = expectation.sender.displayName;
    results.push(result("sender.display_name", expected === actual ? "pass" : "fail", expected === actual ? "matched" : "display_name_mismatch", expected, actual, attempts));
  }

  if (expectation.recipients) {
    const movements = fieldPlacement(expectation, candidate);
    for (const field of RECIPIENT_FIELDS) {
      if (expectation.recipients[field] === undefined) continue;
      if (field === "bcc" && !evidence.capabilities?.includes("blind_recipients")) {
        results.push(result("recipients.bcc", "error", "missing_blind_recipient_evidence", expectedAddresses(expectation.recipients.bcc), null, attempts));
        continue;
      }
      const assertion = recipientAssertion(field, expectation.recipients[field], candidate, movements);
      results.push(result(`recipients.${field}`, assertion.status, assertion.code, expectedAddresses(expectation.recipients[field]), assertion.actual, attempts));
    }
    if (expectation.recipients.envelope !== undefined) {
      if (!evidence.capabilities?.includes("envelope_recipients")) {
        results.push(result("recipients.envelope", "error", "missing_envelope_recipient_evidence", expectedAddresses(expectation.recipients.envelope), null, attempts));
      } else {
        const observed = addressList(candidate.envelopeRecipients);
        const expected = expectedAddresses(expectation.recipients.envelope);
        const actual = [...new Set(observed.addresses)].sort();
        const missing = expected.filter((address) => !actual.includes(address));
        const unexpected = actual.filter((address) => !expected.includes(address));
        const code = observed.invalid.length > 0 || (missing.length > 0 && unexpected.length > 0) ? "recipient_set_mismatch"
          : missing.length > 0 ? "missing_recipient"
            : unexpected.length > 0 ? "unexpected_recipient" : "matched";
        const original = Array.isArray(candidate.envelopeRecipients) ? [...candidate.envelopeRecipients] : [];
        results.push(result("recipients.envelope", code === "matched" ? "pass" : "fail", code, expected, { original, addresses: actual, invalid: observed.invalid, missing, unexpected }, attempts));
      }
    }
    results.push(result("recipients.cross_field", movements.length === 0 ? "pass" : "fail", movements.length === 0 ? "matched" : "recipient_cross_field", "same recipient fields", movements, attempts));

    const target = targetAddress(evidence);
    const selfRecipients = target === null ? [] : [...new Set([
      ...recipientAddresses(candidate),
      ...addressList(candidate.envelopeRecipients).addresses,
    ].filter((address) => address === target))].sort();
    results.push(result(
      "recipients.no_target_self",
      target === null ? "error" : selfRecipients.length === 0 ? "pass" : "fail",
      target === null ? "missing_target_identity_evidence" : selfRecipients.length === 0 ? "matched" : "target_self_recipient",
      target,
      selfRecipients,
      attempts,
    ));
  }
  return results;
}
