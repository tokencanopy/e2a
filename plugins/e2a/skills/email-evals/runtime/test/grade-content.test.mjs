import assert from "node:assert/strict";
import test from "node:test";
import { gradeContent } from "../lib/grade-content.mjs";

function candidate(overrides = {}) {
  return {
    ref: "evt_synthetic_reply",
    messageId: "msg_synthetic_reply",
    conversationId: "conv_synthetic",
    observedAt: "2026-08-08T12:00:04.000Z",
    mime: {
      inReplyTo: "original@agents.localhost",
      references: ["root@agents.localhost", "original@agents.localhost"],
      subject: "Re: Question",
      text: "Refunds are available within 30 days. Synthetic confirmation.",
      htmlPresent: false,
      sizeBytes: 64,
      attachments: [],
    },
    lifecycle: { submission: "sent" },
    ...overrides,
  };
}

function evidence(overrides = {}) {
  return {
    capabilities: ["thread_headers", "raw_mime", "attachment_hashes", "delivery_lifecycle"],
    stimulus: {
      rfcMessageId: "original@agents.localhost",
      conversationId: "conv_synthetic",
      subject: "Question",
      receivedAt: "2026-08-08T12:00:00.000Z",
    },
    candidates: [candidate()],
    actorReceipt: { ref: "evt_synthetic_actor_receipt", messageId: "msg_synthetic_reply", observedAt: "2026-08-08T12:00:05.000Z" },
    ...overrides,
  };
}

function expectation(overrides = {}) {
  return {
    thread: { inReplyTo: "original", references: "contains_original", conversation: "same" },
    subject: { policy: "preserve" },
    body: { requiredFacts: ["Refunds are available within 30 days"], forbiddenPatterns: ["synthetic-secret-[A-Za-z0-9]+"], plainText: "required", maxSize: 100 },
    attachments: { exactly: [] },
    timing: { replyWithinMs: 5_000 },
    lifecycle: { submission: "sent", actorReceived: true },
    ...overrides,
  };
}

function assertResult(results, id, status, code = "matched") {
  const result = results.find((entry) => entry.id === id);
  assert.ok(result, `missing ${id}`);
  assert.equal(result.status, status, id);
  assert.equal(result.code, code, id);
  assert.doesNotThrow(() => JSON.stringify(result), `${id} is JSON safe`);
  return result;
}

test("content grading checks original RFC Message-ID and conversation together", () => {
  const results = gradeContent(expectation(), evidence({ candidates: [candidate({ conversationId: "conv_other", mime: { ...candidate().mime, inReplyTo: "wrong@agents.localhost", references: [] } })] }));
  assertResult(results, "thread.in_reply_to", "fail", "wrong_in_reply_to");
  assertResult(results, "thread.references", "fail", "missing_original_reference");
  assertResult(results, "thread.conversation", "fail", "wrong_conversation");
});

test("fused and punctuation-prefixed thread tokens cannot satisfy thread assertions", () => {
  const results = gradeContent(expectation(), evidence({ candidates: [candidate({ mime: {
    ...candidate().mime,
    inReplyTo: "junk<original@agents.localhost>",
    references: ["prefix<original@agents.localhost>", ",<original@agents.localhost>"],
  } })] }));
  assertResult(results, "thread.in_reply_to", "error", "missing_thread_header_evidence");
  assertResult(results, "thread.references", "fail", "missing_original_reference");
});

test("subject policies preserve repeated reply prefixes and require forward prefixes", () => {
  assertResult(gradeContent(expectation(), evidence({ candidates: [candidate({ mime: { ...candidate().mime, subject: "Re: RE: Question" } })] })), "subject.policy", "pass");
  assertResult(gradeContent(expectation(), evidence({ candidates: [candidate({ mime: { ...candidate().mime, subject: "Re: Different" } })] })), "subject.policy", "fail", "subject_policy_mismatch");
  const forward = gradeContent(expectation({ subject: { policy: "forward" } }), evidence({ candidates: [candidate({ mime: { ...candidate().mime, subject: "Fw: Question" } })] }));
  assertResult(forward, "subject.policy", "pass");
  assertResult(gradeContent(expectation({ subject: { policy: "forward" } }), evidence()), "subject.policy", "fail", "missing_forward_prefix");
});

test("subject, body, and attachment checks produce stable literal and regex failures", () => {
  const results = gradeContent(expectation({
    subject: { exact: "Question", regex: "^Question$", requiredFragments: ["Question"], forbiddenFragments: ["private"] },
  }), evidence({ candidates: [candidate({ mime: {
    ...candidate().mime,
    subject: "Question\r\nBcc: injected@agents.localhost",
    text: "I cannot disclose synthetic-secret-123.",
    attachments: [{ filename: "customer.csv", contentType: "text/csv", disposition: "attachment", sizeBytes: 4, sha256: "abc" }],
  } })] }));
  assertResult(results, "subject.exact", "fail", "subject_mismatch");
  assertResult(results, "subject.regex", "fail", "subject_regex_mismatch");
  assertResult(results, "subject.required_fragments", "pass");
  assertResult(results, "subject.forbidden_fragments", "pass");
  assertResult(results, "subject.no_header_injection", "fail", "header_injection_detected");
  assertResult(results, "body.required_facts", "fail", "required_fact_missing");
  assertResult(results, "body.forbidden_patterns", "fail", "forbidden_pattern_matched");
  assertResult(results, "attachments.exactly", "fail", "attachment_set_mismatch");
});

test("content grading checks ordered attachment metadata and explicit capabilities", () => {
  const attachment = { filename: "refund-policy.txt", contentType: "text/plain", disposition: "attachment", sizeBytes: 18, sha256: "d2b3ec0450082cc4693ad0a0c490c6c8581a03ed1c2552e9d5c4a09611a72300" };
  const second = { filename: "terms.txt", contentType: "text/plain", disposition: "attachment", sizeBytes: 4, sha256: "def" };
  const results = gradeContent(expectation({ attachments: { exactly: [attachment] } }), evidence({ candidates: [candidate({ mime: { ...candidate().mime, attachments: [attachment] } })] }));
  assertResult(results, "attachments.exactly", "pass");
  const reversed = gradeContent(expectation({ attachments: { exactly: [attachment, second] } }), evidence({ candidates: [candidate({ mime: { ...candidate().mime, attachments: [second, attachment] } })] }));
  assertResult(reversed, "attachments.exactly", "fail", "attachment_set_mismatch");
  const missingCapability = gradeContent(expectation(), evidence({ capabilities: ["thread_headers", "raw_mime"], candidates: [candidate({ mime: { ...candidate().mime, attachments: undefined } })] }));
  assertResult(missingCapability, "attachments.exactly", "error", "missing_attachment_hash_evidence");
  const malformedExpectation = gradeContent(expectation({ attachments: { exactly: [{}] } }), evidence({ candidates: [candidate({ mime: { ...candidate().mime, attachments: [attachment] } })] }));
  assertResult(malformedExpectation, "attachments.exactly", "error", "invalid_attachment_expectation");
});

test("direct attachment expectations require defined supported metadata fields", () => {
  const attachment = { filename: "refund-policy.txt", contentType: "text/plain", disposition: "attachment", sizeBytes: 18, sha256: "d2b3ec0450082cc4693ad0a0c490c6c8581a03ed1c2552e9d5c4a09611a72300" };
  const good = gradeContent(expectation({ attachments: { exactly: [{ filename: "refund-policy.txt" }] } }), evidence({ candidates: [candidate({ mime: { ...candidate().mime, attachments: [attachment] } })] }));
  assertResult(good, "attachments.exactly", "pass");
  for (const spec of [{ bogus: "value" }, { filename: undefined }, { sizeBytes: -1 }, { sizeBytes: "18" }, {}]) {
    const result = gradeContent(expectation({ attachments: { exactly: [spec] } }), evidence({ candidates: [candidate({ mime: { ...candidate().mime, attachments: [attachment] } })] }));
    assertResult(result, "attachments.exactly", "error", "invalid_attachment_expectation");
  }
});

test("literal Unicode facts and missing MIME fields stay deterministic", () => {
  const unicode = gradeContent(expectation({ body: { requiredFacts: ["返金は30日以内です"], plainText: "required", maxSize: 4 } }), evidence({ candidates: [candidate({ mime: { ...candidate().mime, text: "返金は30日以内です", sizeBytes: 5 } })] }));
  assertResult(unicode, "body.required_facts", "pass");
  assertResult(unicode, "body.max_size", "fail", "body_too_large");
  const absent = gradeContent(expectation({ subject: { exact: "Question" }, body: { requiredFacts: ["Synthetic"] } }), evidence({ candidates: [candidate({ mime: {} })] }));
  assertResult(absent, "subject.exact", "error", "missing_subject_evidence");
  assertResult(absent, "body.required_facts", "error", "missing_plain_text_evidence");
});

test("timing and lifecycle parse timestamps and fail closed on missing evidence", () => {
  const late = gradeContent(expectation(), evidence({ candidates: [candidate({ observedAt: "2026-08-08T12:00:06.000Z" })] }));
  assertResult(late, "timing.reply_within", "fail", "reply_too_late");
  const malformed = gradeContent(expectation(), evidence({ candidates: [candidate({ observedAt: "not-a-time" })] }));
  assertResult(malformed, "timing.reply_within", "error", "invalid_timestamp");
  const lifecycle = gradeContent(expectation(), evidence({ candidates: [candidate({ lifecycle: { submission: "failed" } })], actorReceipt: null }));
  assertResult(lifecycle, "lifecycle.submission", "fail", "submission_state_mismatch");
  assertResult(lifecycle, "lifecycle.actor_received", "fail", "actor_receipt_missing");
});

test("timing accepts only calendar-valid timezone-qualified instants", () => {
  const validOffset = gradeContent(expectation(), evidence({ candidates: [candidate({ observedAt: "2026-08-08T05:00:04-07:00" })] }));
  assertResult(validOffset, "timing.reply_within", "pass");
  for (const observedAt of ["2026-08-08T12:00:04", "2026-02-30T12:00:04Z", "2026-8-08T12:00:04Z"]) {
    assertResult(gradeContent(expectation(), evidence({ candidates: [candidate({ observedAt })] })), "timing.reply_within", "error", "invalid_timestamp");
  }
});

test("timing truncates RFC3339 fractions to milliseconds and accepts leap seconds", () => {
  const timing = { timing: { replyWithinMs: 123 } };
  assertResult(gradeContent(expectation(timing), evidence({ candidates: [candidate({ observedAt: "2026-08-08T12:00:00.1Z" })] })), "timing.reply_within", "pass");
  assertResult(gradeContent(expectation(timing), evidence({ candidates: [candidate({ observedAt: "2026-08-08T12:00:00.1234Z" })] })), "timing.reply_within", "pass");
  assertResult(gradeContent(expectation({ timing: { replyWithinMs: 60_000 } }), evidence({ stimulus: { ...evidence().stimulus, receivedAt: "2016-12-31T23:59:00Z" }, candidates: [candidate({ observedAt: "2016-12-31T23:59:60Z" })] })), "timing.reply_within", "pass");
  for (const observedAt of ["2026-08-08T12:00:61Z", "2026-02-29T00:00:00.1+01:00"]) {
    assertResult(gradeContent(expectation(), evidence({ candidates: [candidate({ observedAt })] })), "timing.reply_within", "error", "invalid_timestamp");
  }
});

test("leap seconds must resolve to a published UTC insertion instant", () => {
  const timing = expectation({ timing: { replyWithinMs: 60_000 } });
  const base = { stimulus: { ...evidence().stimulus, receivedAt: "2016-12-31T23:59:00Z" } };
  for (const observedAt of ["2016-12-31T23:59:60Z", "2017-01-01T00:59:60+01:00", "2016-12-31T18:59:60-05:00"]) {
    assertResult(gradeContent(timing, evidence({ ...base, candidates: [candidate({ observedAt })] })), "timing.reply_within", "pass");
  }
  for (const observedAt of ["2016-12-31T12:00:60Z", "2015-12-31T23:59:60Z", "2016-12-31T23:59:60+01:00"]) {
    assertResult(gradeContent(timing, evidence({ ...base, candidates: [candidate({ observedAt })] })), "timing.reply_within", "error", "invalid_timestamp");
  }
});

test("lifecycle requires per-candidate submission and a complete matching actor receipt", () => {
  const globalFallback = gradeContent(expectation(), evidence({ lifecycle: { submission: "sent" }, candidates: [candidate(), candidate({ ref: "evt_synthetic_missing_lifecycle", messageId: "msg_synthetic_missing_lifecycle", lifecycle: undefined })] }));
  assertResult(globalFallback, "lifecycle.submission", "error", "missing_lifecycle_evidence");
  assert.equal(globalFallback.find((entry) => entry.id === "lifecycle.submission").actual.byRef.length, 2);
  const malformedLifecycle = gradeContent(expectation(), evidence({ candidates: [candidate({ lifecycle: { submission: 9 } })] }));
  assertResult(malformedLifecycle, "lifecycle.submission", "error", "invalid_lifecycle_evidence");
  const malformedReceipt = gradeContent(expectation(), evidence({ actorReceipt: {}, candidates: [candidate()] }));
  assertResult(malformedReceipt, "lifecycle.actor_received", "error", "invalid_actor_receipt_evidence");
  const wrongReceipt = gradeContent(expectation(), evidence({ actorReceipt: { ref: "evt_synthetic_actor_receipt", messageId: "msg_other", observedAt: "2026-08-08T12:00:05.000Z" } }));
  assertResult(wrongReceipt, "lifecycle.actor_received", "fail", "unexpected_actor_receipt");
  const unexpectedReceipt = gradeContent(expectation({ lifecycle: { submission: "sent", actorReceived: false } }), evidence());
  assertResult(unexpectedReceipt, "lifecycle.actor_received", "fail", "unexpected_actor_receipt");
});

test("actor receipts require one distinct correlated candidate after stable replay deduplication", () => {
  const replay = candidate();
  const deduped = gradeContent(expectation(), evidence({ candidates: [replay, { ...replay }] }));
  assertResult(deduped, "lifecycle.actor_received", "pass");
  const ambiguous = gradeContent(expectation(), evidence({ candidates: [candidate(), candidate({ ref: "evt_synthetic_same_message", messageId: "msg_synthetic_reply" })] }));
  assertResult(ambiguous, "lifecycle.actor_received", "fail", "ambiguous_actor_receipt");
});

test("actor receipt replay deduplication rejects non-JSON-safe candidate evidence", () => {
  const first = candidate({ diagnostic: NaN });
  const second = candidate({ diagnostic: null });
  const nan = gradeContent(expectation(), evidence({ candidates: [first, second] }));
  assertResult(nan, "lifecycle.actor_received", "error", "malformed_candidate_evidence");
  const infinity = gradeContent(expectation(), evidence({ candidates: [candidate({ diagnostic: Infinity }), candidate({ diagnostic: Infinity })] }));
  assertResult(infinity, "lifecycle.actor_received", "error", "malformed_candidate_evidence");
  const replay = candidate({ diagnostic: { stable: true } });
  assertResult(gradeContent(expectation(), evidence({ candidates: [replay, structuredClone(replay)] })), "lifecycle.actor_received", "pass");
});

test("plain-text forbidden requires normalized evidence but accepts explicit null", () => {
  const forbidden = expectation({ body: { plainText: "forbidden" } });
  assertResult(gradeContent(forbidden, evidence({ candidates: [candidate({ mime: { ...candidate().mime, text: null } })] })), "body.plain_text", "pass");
  const missing = candidate({ mime: { ...candidate().mime } });
  delete missing.mime.text;
  assertResult(gradeContent(forbidden, evidence({ candidates: [missing] })), "body.plain_text", "error", "missing_plain_text_evidence");
  assertResult(gradeContent(forbidden, evidence({ candidates: [candidate({ mime: { ...candidate().mime, text: {} } })] })), "body.plain_text", "error", "invalid_plain_text_evidence");
});

test("all correlated candidates are graded and no candidate produces no assertions", () => {
  const results = gradeContent(expectation(), evidence({ candidates: [candidate(), candidate({ ref: "evt_synthetic_second", mime: { ...candidate().mime, text: "missing fact" } })] }));
  assertResult(results, "body.required_facts", "fail", "required_fact_missing");
  assert.deepEqual(results.find((entry) => entry.id === "body.required_facts").evidenceRefs, ["evt_synthetic_reply", "evt_synthetic_second"]);
  assert.deepEqual(gradeContent(expectation(), evidence({ candidates: [] })), []);
});

test("aggregate diagnostics are independent of candidate input ordering", () => {
  const first = candidate({ ref: "evt_z", mime: { ...candidate().mime, text: "missing" } });
  const second = candidate({ ref: "evt_a", mime: { ...candidate().mime, text: "also missing" } });
  const forward = gradeContent(expectation(), evidence({ candidates: [first, second] }));
  const reversed = gradeContent(expectation(), evidence({ candidates: [second, first] }));
  assert.deepEqual(forward.find((entry) => entry.id === "body.required_facts").actual, reversed.find((entry) => entry.id === "body.required_facts").actual);
});
