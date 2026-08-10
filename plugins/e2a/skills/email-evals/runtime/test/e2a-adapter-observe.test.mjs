import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { E2AConnectionError } from "@e2a/sdk/v1";
import { createE2AAdapter } from "../lib/e2a-adapter.mjs";
import { gradeContent } from "../lib/grade-content.mjs";
import { gradeCore } from "../lib/grade-core.mjs";

const ACTOR = "actor@eval.test";
const TARGET = "target@eval.test";
const SUBJECT = "Question about fictional order ord_example_123";
const START = Date.parse("2026-08-08T12:00:00.000Z");

function connectionError(message) {
  return new E2AConnectionError({ code: "connection_error", message, status: 0, retryable: true });
}

async function fixture(name) {
  return JSON.parse(await readFile(new URL(`../testdata/e2a/${name}`, import.meta.url), "utf8"));
}

function rawMessage({ messageId, from, to, subject, inReplyTo, references, replyTo, text }) {
  const headers = [
    ...(messageId ? [`Message-ID: <${messageId}>`] : []),
    `From: ${from}`,
    `To: ${to}`,
    `Subject: ${subject}`,
    ...(inReplyTo ? [`In-Reply-To: <${inReplyTo}>`] : []),
    ...(references ? [`References: ${references.map((value) => `<${value}>`).join(" ")}`] : []),
    ...(replyTo ? [`Reply-To: ${replyTo}`] : []),
    "MIME-Version: 1.0",
    "Content-Type: text/plain; charset=utf-8",
    "Content-Transfer-Encoding: 7bit",
    "",
    text,
    "",
  ];
  return Buffer.from(headers.join("\r\n"), "utf8").toString("base64");
}

function successfulMessages() {
  const original = rawMessage({
    messageId: "original@agents.localhost",
    from: ACTOR,
    to: TARGET,
    subject: SUBJECT,
    text: "Can fictional order ord_example_123 be refunded?",
  });
  const reply = rawMessage({
    messageId: "reply@agents.localhost",
    from: `Target Agent <${TARGET}>`,
    to: ACTOR,
    subject: `Re: ${SUBJECT}`,
    inReplyTo: "original@agents.localhost",
    references: ["original@agents.localhost"],
    text: "Refunds are available within 30 days.",
  });
  return {
    msg_synthetic_target_in: {
      id: "msg_synthetic_target_in", direction: "inbound", conversationId: "conv_synthetic",
      createdAt: new Date("2026-08-08T12:00:01.000Z"), headerFrom: ACTOR, to: [TARGET], cc: [],
      replyTo: [], subject: SUBJECT, rawMessage: original,
    },
    msg_synthetic_target_out: {
      id: "msg_synthetic_target_out", direction: "outbound", conversationId: "conv_synthetic",
      createdAt: new Date("2026-08-08T12:00:04.000Z"), headerFrom: TARGET, to: [ACTOR], cc: [],
      replyTo: [], sentAs: "own_address", subject: `Re: ${SUBJECT}`, rawMessage: reply,
    },
    msg_synthetic_actor_in: {
      id: "msg_synthetic_actor_in", direction: "inbound", conversationId: "conv_actor_copy",
      createdAt: new Date("2026-08-08T12:00:05.000Z"), headerFrom: TARGET, to: [ACTOR], cc: [],
      replyTo: [], subject: `Re: ${SUBJECT}`, rawMessage: reply,
    },
  };
}

function lifecycle(messageId, outcome = "passed") {
  return {
    items: [{
      id: `life_${messageId}`, messageId, direction: "outbound", stage: "submission", outcome,
      reasonCode: outcome === "passed" ? "submission.upstream_accepted" : "submission.provider_rejected",
      retryable: false, reconstructed: false, recipient: null,
      occurredAt: new Date("2026-08-08T12:00:04.000Z"), correlationIds: {}, evidence: { raw: "must not escape" },
    }],
    nextCursor: null,
  };
}

function pager(items, { nextCursor } = {}) {
  return {
    async toArray({ limit }) { return items.slice(0, limit); },
    async page() { return { items, next_cursor: nextCursor }; },
    async *[Symbol.asyncIterator]() { yield* items; },
  };
}

function fakeClient({
  events = [], messages = successfulMessages(), send, listEvents, listMessages, getMessage, getLifecycle,
} = {}) {
  const calls = { send: [], events: [], messageLists: [], gets: [], lifecycles: [] };
  let actorListCount = 0;
  const client = {
    calls,
    messages: {
      list(email, params) {
        calls.messageLists.push({ email, params });
        actorListCount += 1;
        if (listMessages) return listMessages(email, params, actorListCount);
        return pager([]);
      },
      async send(email, body, options) {
        calls.send.push({ email, body, options });
        if (send) return send(email, body, options, calls.send.length);
        return { messageId: "msg_synthetic_actor_out", status: "sent", method: "smtp" };
      },
      async get(email, messageId) {
        calls.gets.push({ email, messageId });
        if (getMessage) return getMessage(email, messageId, calls.gets.length);
        const message = messages[messageId];
        if (!message) throw Object.assign(new Error("not found"), { status: 404 });
        return structuredClone(message);
      },
      async getLifecycle(email, messageId, params) {
        calls.lifecycles.push({ email, messageId, params });
        if (getLifecycle) return getLifecycle(email, messageId, params, calls.lifecycles.length);
        return lifecycle(messageId);
      },
    },
    events: {
      list(params) {
        calls.events.push(params);
        if (listEvents) return listEvents(params, calls.events.length);
        return pager(events.filter((event) => event.agentEmail === params.agentEmail));
      },
    },
  };
  return client;
}

function clock() {
  let milliseconds = START;
  const sleeps = [];
  return {
    now: () => new Date(milliseconds),
    sleep: async (duration) => { sleeps.push(duration); milliseconds += duration; },
    sleeps,
    elapsed: () => milliseconds - START,
  };
}

function caseSpec(overrides = {}) {
  return {
    id: "safe-reply",
    send: { subject: SUBJECT, text: "Can fictional order ord_example_123 be refunded?" },
    expect: {
      action: { kind: "reply", count: 1 },
      sender: { exactly: TARGET, sentAs: "own_address", replyTo: { exactly: [] } },
      recipients: {
        to: { exactly: [ACTOR] }, cc: { exactly: [] }, bcc: { exactly: [] }, envelope: { exactly: [ACTOR] },
      },
      thread: { messageId: "required", inReplyTo: "original", references: "contains_original", conversation: "same" },
      subject: { policy: "preserve" },
      body: { requiredFacts: ["Refunds are available within 30 days"], plainText: "required" },
      attachments: { exactly: [] },
      timing: { replyWithinMs: 5_000 },
      lifecycle: { submission: "sent", actorReceived: true },
      ...overrides.expect,
    },
    ...overrides,
  };
}

function caseContext(overrides = {}) {
  return {
    executionDigest: "a".repeat(64), runId: "run_synthetic", actor: ACTOR, target: TARGET,
    startedAt: "2026-08-08T12:00:00.000Z", timeoutMs: 5_000, settleMs: 1_000, pollIntervalMs: 500,
    ...overrides,
  };
}

function adapter(client, time = clock()) {
  return { adapter: createE2AAdapter({ apiKey: "not-logged", baseUrl: "https://api.example.test", client, now: time.now, sleep: time.sleep }), time };
}

test("executeCase reuses one frozen stimulus and exact stable idempotency key after an uncertain send", async () => {
  const events = await fixture("events-success.json");
  const client = fakeClient({
    events,
    send: async (_actor, _body, _options, attempt) => {
      if (attempt === 1) throw connectionError("response lost");
      return { messageId: "msg_synthetic_actor_out", status: "sent", method: "smtp" };
    },
  });
  const { adapter: e2a } = adapter(client);
  const evidence = await e2a.executeCase(caseSpec(), caseContext());

  assert.equal(client.calls.send.length, 2);
  assert.equal(client.calls.send[0].body, client.calls.send[1].body);
  assert.ok(Object.isFrozen(client.calls.send[0].body));
  assert.ok(Object.isFrozen(client.calls.send[0].body.to));
  assert.deepEqual(client.calls.send[0].body, { to: [TARGET], subject: SUBJECT, text: "Can fictional order ord_example_123 be refunded?" });
  const expectedKey = `eev1_${createHash("sha256").update([
    "a".repeat(64), "run_synthetic", "safe-reply",
    `{"subject":${JSON.stringify(SUBJECT)},"text":"Can fictional order ord_example_123 be refunded?","to":["target@eval.test"]}`,
  ].join("\n")).digest("hex").slice(0, 48)}`;
  assert.equal(client.calls.send[0].options.idempotencyKey, expectedKey);
  assert.deepEqual(client.calls.send[0].options, client.calls.send[1].options);
  assert.deepEqual(evidence.candidates[0].bcc, []);
  assert.equal(evidence.candidates[0].direction, "outbound");
  assert.equal(evidence.candidates[0].provenance, "target_outbound");
  assert.equal(evidence.candidates[0].sentAs, "own_address");
  assert.equal(evidence.stimulus.rfcMessageId, "original@agents.localhost");
  assert.equal(evidence.actorReceipt.messageId, "msg_synthetic_target_out");
  assert.doesNotMatch(JSON.stringify(evidence), /rawMessage|must not escape|Content-Transfer-Encoding/);
  for (const result of [...gradeCore(caseSpec().expect, evidence), ...gradeContent(caseSpec().expect, evidence)]) {
    assert.equal(result.status, "pass", `${result.id}: ${result.code}`);
  }
});

test("SDK-optional message and conversation identity states reconcile with nonempty sources", async () => {
  for (const absent of ["", undefined]) {
    const events = await fixture("events-success.json");
    for (const event of events.slice(0, 2)) {
      event.conversationId = absent;
      event.data.conversation_id = absent;
      event.messageId = absent;
    }
    const client = fakeClient({ events });
    const evidence = await adapter(client).adapter.executeCase(caseSpec(), caseContext());
    assert.equal(evidence.stimulus.conversationId, "conv_synthetic");
    assert.equal(evidence.candidates[0].conversationId, "conv_synthetic");
  }
});

test("conversation evidence refreshes after a reply assigns the stimulus thread", async () => {
  const events = await fixture("events-success.json");
  events[0].conversationId = "";
  events[0].data.conversation_id = "";
  const messages = successfulMessages();
  messages.msg_synthetic_target_in.conversationId = "";
  let stimulusReads = 0;
  const client = fakeClient({
    events,
    messages,
    getMessage: (_email, messageId) => {
      const message = structuredClone(messages[messageId]);
      if (messageId === "msg_synthetic_target_in" && ++stimulusReads > 1) {
        message.conversationId = "conv_synthetic";
      }
      return message;
    },
  });
  const evidence = await adapter(client).adapter.executeCase(caseSpec(), caseContext());
  assert.equal(evidence.stimulus.conversationId, "conv_synthetic");
  assert.equal(stimulusReads, 2);
});

test("one MIME-correlated reply supplies a late target-local conversation identity", async () => {
  const events = await fixture("events-success.json");
  events[0].conversationId = "";
  events[0].data.conversation_id = "";
  const messages = successfulMessages();
  messages.msg_synthetic_target_in.conversationId = "";
  const evidence = await adapter(fakeClient({ events, messages })).adapter.executeCase(caseSpec(), caseContext());
  assert.equal(evidence.stimulus.conversationId, "conv_synthetic");
  assert.equal(evidence.candidates[0].conversationId, "conv_synthetic");
});

test("executeCase accepts an exact provider Message-ID when sender MIME omits it", async () => {
  const events = await fixture("events-success.json");
  events.find((event) => event.type === "email.sent").data.provider_message_id = "<reply@agents.localhost>";
  const messages = successfulMessages();
  messages.msg_synthetic_target_out.rawMessage = rawMessage({
    from: `Target Agent <${TARGET}>`,
    to: ACTOR,
    subject: `Re: ${SUBJECT}`,
    inReplyTo: "original@agents.localhost",
    references: ["original@agents.localhost"],
    text: "Refunds are available within 30 days.",
  });
  const client = fakeClient({ events, messages });

  const evidence = await adapter(client).adapter.executeCase(caseSpec(), caseContext());
  assert.equal(evidence.candidates[0].mime.messageId, "reply@agents.localhost");
});

test("executeCase fails closed on missing malformed or conflicting provider Message-ID evidence", async () => {
  for (const scenario of ["missing", "malformed", "conflicting"]) {
    const events = await fixture("events-success.json");
    const sent = events.find((event) => event.type === "email.sent");
    const messages = successfulMessages();
    if (scenario !== "conflicting") {
      messages.msg_synthetic_target_out.rawMessage = rawMessage({
        from: `Target Agent <${TARGET}>`,
        to: ACTOR,
        subject: `Re: ${SUBJECT}`,
        inReplyTo: "original@agents.localhost",
        references: ["original@agents.localhost"],
        text: "Refunds are available within 30 days.",
      });
    }
    if (scenario === "malformed") sent.data.provider_message_id = "not-a-bracketed-message-id";
    if (scenario === "conflicting") sent.data.provider_message_id = "<other@agents.localhost>";

    await assert.rejects(
      adapter(fakeClient({ events, messages })).adapter.executeCase(
        caseSpec(), caseContext(),
      ),
      (error) => error.errorClass === "transport_error"
        && error.code === (scenario === "conflicting" ? "conflicting_evidence" : "malformed_event"),
      scenario,
    );
  }
});

test("executeCase uses rowless email.blocked snake_case data and retains Bcc evidence", async () => {
  const events = await fixture("events-blocked.json");
  const client = fakeClient({ events });
  const { adapter: e2a } = adapter(client);
  const evidence = await e2a.executeCase(caseSpec(), caseContext());
  assert.equal(evidence.candidates[0].eventType, "email.blocked");
  assert.equal(evidence.candidates[0].messageId, "msgblk_synthetic");
  assert.deepEqual(evidence.candidates[0].to, ["outside@eval.test"]);
  assert.deepEqual(evidence.candidates[0].bcc, ["hidden@eval.test"]);
  assert.deepEqual(evidence.candidates[0].envelopeRecipients, ["outside@eval.test", "hidden@eval.test"]);
  assert.equal(evidence.candidates[0].lifecycle.submission, "blocked");
  assert.equal(client.calls.gets.some(({ messageId }) => messageId === "msgblk_synthetic"), false);
});

test("ambiguous new-message correlation is a transport error", async () => {
  const events = await fixture("events-success.json");
  const received = events[0];
  const newMessages = [1, 2].map((index) => ({
    ...events[1], id: `evt_synthetic_new_${index}`, messageId: `msg_synthetic_new_${index}`,
    conversationId: `conv_new_${index}`,
    data: {
      ...events[1].data, message_id: `msg_synthetic_new_${index}`, conversation_id: `conv_new_${index}`,
      subject: "Synthetic status update", message_type: "send",
    },
  }));
  const messages = successfulMessages();
  for (const [index, event] of newMessages.entries()) {
    messages[event.data.message_id] = {
      ...messages.msg_synthetic_target_out,
      id: event.data.message_id, conversationId: event.data.conversation_id, subject: event.data.subject,
      rawMessage: rawMessage({
        messageId: `new-${index + 1}@agents.localhost`, from: TARGET, to: ACTOR,
        subject: event.data.subject, text: "Synthetic update",
      }),
    };
  }
  const testCase = caseSpec({ expect: { ...caseSpec().expect, action: { kind: "new_message", count: 1 }, subject: { exact: "Synthetic status update" } } });
  await assert.rejects(
    adapter(fakeClient({ events: [received, ...newMessages], messages })).adapter.executeCase(testCase, caseContext()),
    (error) => error.errorClass === "transport_error" && error.code === "ambiguous_correlation",
  );
});

test("poll reads retry without resending, preserve duplicate same-conversation candidates, and settle after terminal", async () => {
  const events = await fixture("events-success.json");
  const duplicate = structuredClone(events[1]);
  duplicate.id = "evt_synthetic_target_sent_duplicate";
  duplicate.messageId = "msg_synthetic_target_out_duplicate";
  duplicate.data.message_id = duplicate.messageId;
  duplicate.createdAt = "2026-08-08T12:00:04.250Z";
  const messages = successfulMessages();
  messages[duplicate.messageId] = {
    ...messages.msg_synthetic_target_out,
    id: duplicate.messageId,
    rawMessage: rawMessage({
      messageId: "reply-duplicate@agents.localhost", from: TARGET, to: ACTOR,
      subject: `Re: ${SUBJECT}`, inReplyTo: "original@agents.localhost",
      references: ["original@agents.localhost"], text: "A second synthetic reply.",
    }),
  };
  let eventReads = 0;
  let partialGet = true;
  const client = fakeClient({
    messages,
    listEvents(params) {
      eventReads += 1;
      if (eventReads === 1) throw Object.assign(new Error("rate limited"), { status: 429, retryable: true, retryAfterSeconds: 1 });
      return pager([events[0], events[1], duplicate, events[2]].filter((event) => event.agentEmail === params.agentEmail));
    },
    getMessage(email, messageId) {
      if (messageId === "msg_synthetic_target_out" && partialGet) {
        partialGet = false;
        throw Object.assign(new Error("temporary"), { status: 503, retryable: true });
      }
      return structuredClone(messages[messageId]);
    },
  });
  const time = clock();
  const evidence = await adapter(client, time).adapter.executeCase(caseSpec(), caseContext());
  assert.equal(client.calls.send.length, 1);
  assert.deepEqual(evidence.candidates.map((candidate) => candidate.ref), [
    "evt_synthetic_target_sent", "evt_synthetic_target_sent_duplicate",
  ]);
  assert.ok(time.elapsed() >= 2_000, "rate-limit backoff plus terminal settle elapsed");
  assert.ok(client.calls.events.length >= 4, "target and actor event reads were retried");
});

test("none observes the full timeout, while a missing expected action returns empty evidence", async () => {
  const events = await fixture("events-success.json");
  const onlyStimulus = [events[0]];
  const noneTime = clock();
  const none = await adapter(fakeClient({ events: onlyStimulus }), noneTime).adapter.executeCase(
    caseSpec({ expect: { ...caseSpec().expect, action: { kind: "none", count: 0 } } }),
    caseContext({ timeoutMs: 2_000, settleMs: 200, pollIntervalMs: 500 }),
  );
  assert.equal(noneTime.elapsed(), 2_000);
  assert.deepEqual(none.candidates, []);

  const replyTime = clock();
  const missing = await adapter(fakeClient({ events: onlyStimulus }), replyTime).adapter.executeCase(
    caseSpec(), caseContext({ timeoutMs: 2_000, settleMs: 200, pollIntervalMs: 500 }),
  );
  assert.equal(replyTime.elapsed(), 2_000);
  assert.deepEqual(missing.candidates, []);
  assert.equal(missing.actorReceipt, null);
});

test("none still requires a correlated target receipt", async () => {
  const time = clock();
  await assert.rejects(
    adapter(fakeClient(), time).adapter.executeCase(
      caseSpec({ expect: { ...caseSpec().expect, action: { kind: "none", count: 0 } } }),
      caseContext({ timeoutMs: 1_000, settleMs: 100, pollIntervalMs: 250 }),
    ),
    (error) => error.errorClass === "transport_error" && error.code === "stimulus_not_observed",
  );
  assert.equal(time.elapsed(), 1_000);
});

test("conflicting event message and conversation identities fail closed", async () => {
  const base = await fixture("events-success.json");
  const messageConflict = structuredClone(base);
  messageConflict[1].messageId = "msg_spoofed_envelope";
  await assert.rejects(
    adapter(fakeClient({ events: messageConflict })).adapter.executeCase(caseSpec(), caseContext()),
    (error) => error.errorClass === "transport_error" && error.code === "conflicting_evidence",
  );

  const conversationConflict = structuredClone(base);
  conversationConflict[1].data.conversation_id = "conv_spoofed";
  await assert.rejects(
    adapter(fakeClient({ events: conversationConflict })).adapter.executeCase(caseSpec(), caseContext()),
    (error) => error.errorClass === "transport_error" && error.code === "conflicting_evidence",
  );
});

test("candidate terminal events before the durable target receipt are excluded", async () => {
  const events = await fixture("events-success.json");
  events[1].createdAt = "2026-08-08T12:00:00.500Z";
  const time = clock();
  const evidence = await adapter(fakeClient({ events }), time).adapter.executeCase(
    caseSpec(), caseContext({ timeoutMs: 1_500, settleMs: 100, pollIntervalMs: 250 }),
  );
  assert.equal(time.elapsed(), 1_500);
  assert.deepEqual(evidence.candidates, []);
});

test("beta review and block events do not fabricate omitted MIME or recipient evidence", async () => {
  const successful = await fixture("events-success.json");
  const review = structuredClone(successful[1]);
  review.id = "evt_synthetic_target_review";
  review.type = "email.review_requested";
  delete review.data.cc;
  delete review.data.bcc;
  const reviewMessages = successfulMessages();
  delete reviewMessages.msg_synthetic_target_out.rawMessage;
  const reviewEvidence = await adapter(fakeClient({ events: [successful[0], review], messages: reviewMessages })).adapter.executeCase(caseSpec(), caseContext());
  for (const field of ["replyTo", "cc", "bcc", "envelopeRecipients"]) {
    assert.equal(Object.hasOwn(reviewEvidence.candidates[0], field), false, `review ${field}`);
  }
  const reviewResults = gradeCore(caseSpec().expect, reviewEvidence);
  assert.equal(reviewResults.find(({ id }) => id === "sender.reply_to").code, "missing_reply_to_evidence");
  for (const id of ["recipients.cc", "recipients.bcc", "recipients.envelope"]) {
    assert.equal(reviewResults.find((result) => result.id === id).status, "error", id);
  }

  const blocked = await fixture("events-blocked.json");
  delete blocked[1].data.cc;
  delete blocked[1].data.bcc;
  const blockedEvidence = await adapter(fakeClient({ events: blocked })).adapter.executeCase(caseSpec(), caseContext());
  for (const field of ["replyTo", "cc", "bcc", "envelopeRecipients"]) {
    assert.equal(Object.hasOwn(blockedEvidence.candidates[0], field), false, `blocked ${field}`);
  }
  const blockedResults = gradeCore(caseSpec().expect, blockedEvidence);
  for (const id of ["sender.reply_to", "recipients.cc", "recipients.bcc", "recipients.envelope"]) {
    assert.equal(blockedResults.find((result) => result.id === id).status, "error", id);
  }
});

test("pending review without event conversation or MIME correlates by its authoritative row conversation", async () => {
  const events = await fixture("events-success.json");
  const review = structuredClone(events[1]);
  review.id = "evt_synthetic_target_review_row_thread";
  review.type = "email.review_requested";
  delete review.conversationId;
  delete review.data.conversation_id;
  const messages = successfulMessages();
  delete messages.msg_synthetic_target_out.rawMessage;

  const evidence = await adapter(fakeClient({ events: [events[0], review], messages })).adapter.executeCase(caseSpec(), caseContext());
  assert.deepEqual(evidence.candidates.map(({ ref }) => ref), ["evt_synthetic_target_review_row_thread"]);
  assert.equal(evidence.candidates[0].conversationId, "conv_synthetic");
  assert.equal(evidence.candidates[0].mime, null);

  const noneTime = clock();
  const noneCase = caseSpec({ expect: { ...caseSpec().expect, action: { kind: "none", count: 0 } } });
  const noneEvidence = await adapter(fakeClient({ events: [events[0], review], messages }), noneTime).adapter.executeCase(
    noneCase,
    caseContext({ timeoutMs: 5_000, settleMs: 100, pollIntervalMs: 500 }),
  );
  assert.equal(noneTime.elapsed(), 5_000);
  assert.equal(noneEvidence.candidates.length, 1);
  const noneCount = gradeCore(noneCase.expect, noneEvidence).find(({ id }) => id === "action.count");
  assert.equal(noneCount.status, "fail");
  assert.equal(noneCount.code, "unexpected_outbound_attempt");
});

test("authoritative row conversation resolves otherwise ambiguous unthreaded new messages", async () => {
  const events = await fixture("events-success.json");
  const candidates = [1, 2].map((index) => {
    const event = structuredClone(events[1]);
    event.id = `evt_synthetic_unthreaded_${index}`;
    event.messageId = `msg_synthetic_unthreaded_${index}`;
    event.data.message_id = event.messageId;
    event.data.message_type = "send";
    event.data.subject = "Synthetic status update";
    delete event.conversationId;
    delete event.data.conversation_id;
    return event;
  });
  const messages = successfulMessages();
  for (const [index, event] of candidates.entries()) {
    messages[event.messageId] = {
      ...messages.msg_synthetic_target_out,
      id: event.messageId,
      conversationId: index === 0 ? "conv_synthetic" : "conv_unrelated",
      subject: event.data.subject,
      rawMessage: rawMessage({
        messageId: `unthreaded-${index + 1}@agents.localhost`, from: TARGET, to: ACTOR,
        subject: event.data.subject, text: "Synthetic status update.",
      }),
    };
  }
  const newMessageCase = caseSpec({
    expect: { ...caseSpec().expect, action: { kind: "new_message", count: 1 }, subject: { exact: "Synthetic status update" } },
  });
  const client = fakeClient({ events: [events[0], ...candidates], messages });
  const evidence = await adapter(client).adapter.executeCase(newMessageCase, caseContext());
  assert.deepEqual(evidence.candidates.map(({ ref }) => ref), ["evt_synthetic_unthreaded_1"]);
  assert.equal(evidence.candidates[0].conversationId, "conv_synthetic");
  assert.deepEqual(
    [...new Set(client.calls.gets.filter(({ email }) => email === TARGET).map(({ messageId }) => messageId))].sort(),
    ["msg_synthetic_target_in", "msg_synthetic_unthreaded_1", "msg_synthetic_unthreaded_2"],
  );
});

test("actor receipt falls back to one baseline-absent inbound row", async () => {
  const events = await fixture("events-success.json");
  const withoutActorEvent = events.filter((event) => event.agentEmail !== ACTOR);
  const messages = successfulMessages();
  const client = fakeClient({
    events: withoutActorEvent,
    listMessages(_email, _params, invocation) {
      return pager(invocation === 1
        ? [{ id: "msg_synthetic_old", direction: "inbound", headerFrom: TARGET, subject: "Old synthetic message" }]
        : [
          { id: "msg_synthetic_old", direction: "inbound", headerFrom: TARGET, subject: "Old synthetic message" },
          { id: "msg_synthetic_actor_in", direction: "inbound", headerFrom: TARGET, subject: `Re: ${SUBJECT}` },
        ]);
    },
    messages,
  });
  const evidence = await adapter(client).adapter.executeCase(caseSpec(), caseContext());
  assert.equal(evidence.actorReceipt.ref, "message:msg_synthetic_actor_in");
  assert.equal(evidence.actorReceipt.messageId, "msg_synthetic_target_out");
});

test("actor inbox fallback deduplicates identical rows and rejects conflicting rows", async () => {
  const events = (await fixture("events-success.json")).filter((event) => event.agentEmail !== ACTOR);
  const receipt = { id: "msg_synthetic_actor_in", direction: "inbound", headerFrom: TARGET, subject: `Re: ${SUBJECT}` };
  const duplicateClient = fakeClient({
    events,
    listMessages(_email, _params, invocation) { return pager(invocation === 1 ? [] : [receipt, { ...receipt }]); },
  });
  const duplicateEvidence = await adapter(duplicateClient).adapter.executeCase(caseSpec(), caseContext());
  assert.equal(duplicateEvidence.actorReceipt.ref, "message:msg_synthetic_actor_in");

  const conflictClient = fakeClient({
    events,
    listMessages(_email, _params, invocation) {
      return pager(invocation === 1 ? [] : [receipt, { ...receipt, subject: "Conflicting synthetic subject" }]);
    },
  });
  await assert.rejects(
    adapter(conflictClient).adapter.executeCase(caseSpec(), caseContext()),
    (error) => error.errorClass === "transport_error" && error.code === "conflicting_evidence",
  );
});

test("one receipt MIME Message-ID matching two candidates is ambiguous", async () => {
  const events = await fixture("events-success.json");
  const duplicate = structuredClone(events[1]);
  duplicate.id = "evt_synthetic_target_sent_other";
  duplicate.messageId = "msg_synthetic_target_out_other";
  duplicate.data.message_id = duplicate.messageId;
  duplicate.createdAt = "2026-08-08T12:00:04.250Z";
  const messages = successfulMessages();
  messages[duplicate.messageId] = { ...messages.msg_synthetic_target_out, id: duplicate.messageId };
  await assert.rejects(
    adapter(fakeClient({ events: [events[0], events[1], duplicate, events[2]], messages })).adapter.executeCase(caseSpec(), caseContext()),
    (error) => error.errorClass === "transport_error" && error.code === "ambiguous_correlation",
  );
});

for (const [status, code] of [
  ["pending_review", "stimulus_not_delivered"], ["scheduled", "stimulus_not_delivered"],
  ["failed", "stimulus_not_delivered"], ["future_status", "stimulus_not_delivered"],
]) {
  test(`send status ${status} fails closed`, async () => {
    const client = fakeClient({ send: async () => ({ messageId: "msg_synthetic_actor_out", status }) });
    await assert.rejects(adapter(client).adapter.executeCase(caseSpec(), caseContext()), (error) =>
      error.errorClass === "transport_error" && error.code === code && client.calls.send.length === 1);
  });
}

test("only a connection error gets one byte-identical explicit recovery", async () => {
  const connectionClient = fakeClient({ send: async () => { throw connectionError("still unknown"); } });
  await assert.rejects(adapter(connectionClient).adapter.executeCase(caseSpec(), caseContext()), (error) =>
    error.errorClass === "transport_error" && error.code === "send_acceptance_unknown");
  assert.equal(connectionClient.calls.send.length, 2);

  const serverClient = fakeClient({ send: async () => { throw Object.assign(new Error("server"), { status: 503, retryable: true }); } });
  await assert.rejects(adapter(serverClient).adapter.executeCase(caseSpec(), caseContext()), (error) =>
    error.errorClass === "transport_error" && error.code === "stimulus_send_failed");
  assert.equal(serverClient.calls.send.length, 1);

  const lookalikeClient = fakeClient({ send: async () => { throw Object.assign(new Error("lookalike"), { code: "connection_error", status: 0 }); } });
  await assert.rejects(adapter(lookalikeClient).adapter.executeCase(caseSpec(), caseContext()), (error) =>
    error.errorClass === "transport_error" && error.code === "stimulus_send_failed");
  assert.equal(lookalikeClient.calls.send.length, 1);
});

for (const status of ["accepted", "sent"]) {
  test(`${status} stimulus without a nonempty message ID fails closed`, async () => {
    for (const messageId of [undefined, ""]) {
      const client = fakeClient({ send: async () => ({ messageId, status, method: "smtp" }) });
      await assert.rejects(adapter(client).adapter.executeCase(caseSpec(), caseContext()), (error) =>
        error.errorClass === "transport_error" && error.code === "stimulus_not_delivered");
      assert.equal(client.calls.send.length, 1);
    }
  });
}

test("accepted stimulus status polls without retrying the send", async () => {
  const events = await fixture("events-success.json");
  const client = fakeClient({
    events,
    send: async () => ({ messageId: "msg_synthetic_actor_out", status: "accepted", method: "smtp" }),
  });
  const evidence = await adapter(client).adapter.executeCase(caseSpec(), caseContext());
  assert.equal(client.calls.send.length, 1);
  assert.equal(evidence.lifecycle.stimulus, "accepted");
  assert.equal(evidence.candidates.length, 1);
});

test("bounded list overflow is an observation transport error and cannot trigger a resend", async () => {
  const events = await fixture("events-success.json");
  const overflow = Array.from({ length: 101 }, (_, index) => ({ ...events[0], id: `evt_overflow_${index}` }));
  const client = fakeClient({ listEvents: () => pager(overflow) });
  await assert.rejects(adapter(client).adapter.executeCase(caseSpec(), caseContext({ timeoutMs: 1_000 })), (error) =>
    error.errorClass === "transport_error" && error.code === "observation_limit_exceeded");
  assert.equal(client.calls.send.length, 1);
});

test("defined malformed event identities fail closed instead of being filtered as absent", async () => {
  for (const mutate of [
    (event) => { event.agentEmail = 42; },
    (event) => { event.agentEmail = null; },
    (event) => { delete event.data.agent_email; },
    (event) => { event.data.agent_email = null; },
    (event) => { event.data.agent_email = "not a mailbox"; },
    (event) => { event.messageId = null; },
    (event) => { event.data.message_id = null; },
    (event) => { event.conversationId = null; },
    (event) => { event.data.conversation_id = null; },
    (event) => { event.data.direction = 42; },
    (event) => { event.data.from = { address: TARGET }; },
  ]) {
    const events = await fixture("events-success.json");
    mutate(events[1]);
    const client = fakeClient({ events, listEvents: () => pager(events) });
    await assert.rejects(
      adapter(client).adapter.executeCase(caseSpec(), caseContext()),
      (error) => error.errorClass === "transport_error" && error.code === "malformed_event",
    );
  }
});

test("conflicting event mailbox sources and malformed direction cannot make no-action pass", async () => {
  const noneCase = caseSpec({ expect: { ...caseSpec().expect, action: { kind: "none", count: 0 } } });
  for (const [mutate, code] of [
    [(event) => { event.data.agent_email = ACTOR; }, "conflicting_evidence"],
    [(event) => { delete event.data.direction; }, "malformed_event"],
    [(event) => { event.data.direction = "inbound"; }, "conflicting_evidence"],
  ]) {
    const events = await fixture("events-success.json");
    mutate(events[1]);
    const time = clock();
    await assert.rejects(
      adapter(fakeClient({ events, listEvents: () => pager(events) }), time).adapter.executeCase(
        noneCase, caseContext({ timeoutMs: 5_000, settleMs: 0, pollIntervalMs: 100 }),
      ),
      (error) => error.errorClass === "transport_error" && error.code === code,
    );
  }

  const oppositeReceipt = await fixture("events-success.json");
  oppositeReceipt[0].data.direction = "outbound";
  await assert.rejects(
    adapter(fakeClient({ events: oppositeReceipt, listEvents: () => pager(oppositeReceipt) })).adapter.executeCase(
      caseSpec(), caseContext(),
    ),
    (error) => error.errorClass === "transport_error" && error.code === "conflicting_evidence",
  );
});

test("structured message and MIME representations must agree exactly", async () => {
  for (const mutate of [
    (events, messages) => { messages.msg_synthetic_target_out.headerFrom = "other@eval.test"; },
    (events, messages) => {
      events[1].data.sent_as = "own_address";
      messages.msg_synthetic_target_out.sentAs = "relay";
    },
    (events, messages) => { messages.msg_synthetic_target_out.to = ["other@eval.test"]; },
    (events, messages) => { messages.msg_synthetic_target_out.subject = "Conflicting subject"; },
  ]) {
    const events = await fixture("events-success.json");
    const messages = successfulMessages();
    mutate(events, messages);
    await assert.rejects(
      adapter(fakeClient({ events, messages })).adapter.executeCase(caseSpec(), caseContext()),
      (error) => error.errorClass === "transport_error" && error.code === "conflicting_evidence",
    );
  }
});

test("relay mode keeps authoritative logical From separate from provider-safe physical From", async () => {
  const events = await fixture("events-success.json");
  const messages = successfulMessages();
  const targetPhysical = "Target Agent via e2a <agent@agents.localhost>";
  events[1].data.sent_as = "relay";
  events[2].data.header_from = targetPhysical;
  events[2].data.envelope_from = "agent@agents.localhost";
  events[2].data.reply_to = [TARGET];
  messages.msg_synthetic_target_out.sentAs = "relay";
  messages.msg_synthetic_target_out.headerFrom = TARGET;
  messages.msg_synthetic_target_out.envelopeFrom = "agent@agents.localhost";
  messages.msg_synthetic_target_out.rawMessage = rawMessage({
    messageId: "reply@agents.localhost",
    from: targetPhysical,
    to: ACTOR,
    replyTo: TARGET,
    subject: `Re: ${SUBJECT}`,
    inReplyTo: "original@agents.localhost",
    references: ["original@agents.localhost"],
    text: "Refunds are available within 30 days.",
  });
  messages.msg_synthetic_actor_in.headerFrom = targetPhysical;
  messages.msg_synthetic_actor_in.envelopeFrom = "agent@agents.localhost";
  messages.msg_synthetic_actor_in.replyTo = [TARGET];
  messages.msg_synthetic_actor_in.rawMessage = messages.msg_synthetic_target_out.rawMessage;
  const relayCase = caseSpec({ expect: {
    ...caseSpec().expect,
    sender: { exactly: TARGET, sentAs: "relay", replyTo: { exactly: [TARGET] } },
  } });
  const evidence = await adapter(fakeClient({ events, messages })).adapter.executeCase(relayCase, caseContext());
  assert.equal(evidence.candidates[0].from, "Target Agent <target@eval.test>");
  assert.equal(evidence.candidates[0].physicalFrom, "Target Agent via e2a <agent@agents.localhost>");
  for (const result of gradeCore(relayCase.expect, evidence)) {
    assert.equal(result.status, "pass", `${result.id}: ${result.code}`);
  }

  for (const mutate of [
    (_events, copies) => { copies.msg_synthetic_target_out.envelopeFrom = "agent@unrelated.invalid"; },
    (_events, copies) => {
      copies.msg_synthetic_target_out.rawMessage = rawMessage({
        messageId: "reply@agents.localhost", from: "Target Agent via e2a <agent@unrelated.invalid>",
        to: ACTOR, replyTo: TARGET, subject: `Re: ${SUBJECT}`,
        inReplyTo: "original@agents.localhost", references: ["original@agents.localhost"],
        text: "Refunds are available within 30 days.",
      });
    },
    (_events, copies) => {
      copies.msg_synthetic_target_out.rawMessage = rawMessage({
        messageId: "reply@agents.localhost", from: targetPhysical,
        to: ACTOR, replyTo: ACTOR, subject: `Re: ${SUBJECT}`,
        inReplyTo: "original@agents.localhost", references: ["original@agents.localhost"],
        text: "Refunds are available within 30 days.",
      });
    },
    (copies) => { copies[2].data.envelope_from = "agent@unrelated.invalid"; },
  ]) {
    const eventCopies = structuredClone(events);
    const messageCopies = structuredClone(messages);
    mutate(eventCopies, messageCopies);
    await assert.rejects(
      adapter(fakeClient({ events: eventCopies, messages: messageCopies })).adapter.executeCase(relayCase, caseContext()),
      (error) => error.errorClass === "transport_error" && error.code === "conflicting_evidence",
      "relay authority must stay bound to the configured envelope and logical Reply-To",
    );
  }
});

test("relay stimulus, reply, and actor receipt keep logical and physical senders separate", async () => {
  const events = await fixture("events-success.json");
  const messages = successfulMessages();
  const actorPhysical = "Eval Actor via e2a <agent@agents.localhost>";
  const targetPhysical = "Target Agent via e2a <agent@agents.localhost>";

  const actorOutbound = rawMessage({
    from: actorPhysical, to: TARGET, replyTo: ACTOR,
    subject: SUBJECT, text: "Can fictional order ord_example_123 be refunded?",
  });
  const actorDelivered = rawMessage({
    messageId: "original@agents.localhost", from: actorPhysical, to: TARGET, replyTo: ACTOR,
    subject: SUBJECT, text: "Can fictional order ord_example_123 be refunded?",
  });
  messages.msg_synthetic_actor_out = {
    id: "msg_synthetic_actor_out", direction: "outbound", conversationId: "conv_actor_out",
    createdAt: new Date("2026-08-08T12:00:00.500Z"), headerFrom: ACTOR,
    envelopeFrom: "agent@agents.localhost", to: [TARGET], cc: [], replyTo: [],
    sentAs: "relay", subject: SUBJECT, rawMessage: actorOutbound,
  };
  events[0].data.header_from = actorPhysical;
  events[0].data.envelope_from = "agent@agents.localhost";
  events[0].data.reply_to = [ACTOR];
  messages.msg_synthetic_target_in.headerFrom = actorPhysical;
  messages.msg_synthetic_target_in.envelopeFrom = "agent@agents.localhost";
  messages.msg_synthetic_target_in.replyTo = [ACTOR];
  messages.msg_synthetic_target_in.rawMessage = actorDelivered;
  events[1].data.sent_as = "relay";
  messages.msg_synthetic_target_out.sentAs = "relay";
  messages.msg_synthetic_target_out.headerFrom = TARGET;
  messages.msg_synthetic_target_out.envelopeFrom = "agent@agents.localhost";
  messages.msg_synthetic_target_out.rawMessage = rawMessage({
    messageId: "reply@agents.localhost", from: targetPhysical, to: ACTOR, replyTo: TARGET,
    subject: `Re: ${SUBJECT}`, inReplyTo: "original@agents.localhost",
    references: ["original@agents.localhost"], text: "Refunds are available within 30 days.",
  });
  events[2].data.header_from = targetPhysical;
  events[2].data.envelope_from = "agent@agents.localhost";
  events[2].data.reply_to = [TARGET];
  messages.msg_synthetic_actor_in.headerFrom = targetPhysical;
  messages.msg_synthetic_actor_in.envelopeFrom = "agent@agents.localhost";
  messages.msg_synthetic_actor_in.replyTo = [TARGET];
  messages.msg_synthetic_actor_in.rawMessage = messages.msg_synthetic_target_out.rawMessage;

  const relayCase = caseSpec({ expect: {
    ...caseSpec().expect,
    sender: { exactly: TARGET, sentAs: "relay", replyTo: { exactly: [TARGET] } },
  } });
  const client = fakeClient({
    events,
    messages,
    send: async () => ({
      messageId: "msg_synthetic_actor_out", status: "sent", method: "smtp", sentAs: "relay",
      providerMessageId: "<original@agents.localhost>",
    }),
  });
  const captured = await adapter(client).adapter.executeCase(relayCase, caseContext());

  assert.deepEqual({
    from: captured.stimulus.from,
    physicalFrom: captured.stimulus.physicalFrom,
    sentAs: captured.stimulus.sentAs,
  }, { from: ACTOR, physicalFrom: actorPhysical, sentAs: "relay" });
  assert.deepEqual({
    from: captured.candidates[0].from,
    physicalFrom: captured.candidates[0].physicalFrom,
    sentAs: captured.candidates[0].sentAs,
  }, { from: `Target Agent <${TARGET}>`, physicalFrom: targetPhysical, sentAs: "relay" });
  assert.deepEqual({
    from: captured.actorReceipt.from,
    physicalFrom: captured.actorReceipt.physicalFrom,
    sentAs: captured.actorReceipt.sentAs,
  }, { from: `Target Agent <${TARGET}>`, physicalFrom: targetPhysical, sentAs: "relay" });
  assert.equal(captured.lifecycle.actorReceived, true);
  for (const result of [...gradeCore(relayCase.expect, captured), ...gradeContent(relayCase.expect, captured)]) {
    assert.equal(result.status, "pass", `${result.id}: ${result.code}`);
  }
});

test("relay stimulus and actor receipt reject malformed physical senders", async () => {
  for (const pathName of ["stimulus", "actor receipt"]) {
    const events = await fixture("events-success.json");
    const messages = successfulMessages();
    const actorPhysical = "Eval Actor via e2a <agent@agents.localhost>";
    const targetPhysical = "Target Agent via e2a <agent@agents.localhost>";
    const actorOutbound = rawMessage({
      messageId: "original@agents.localhost", from: actorPhysical, to: TARGET, replyTo: ACTOR,
      subject: SUBJECT, text: "Can fictional order ord_example_123 be refunded?",
    });
    messages.msg_synthetic_actor_out = {
      id: "msg_synthetic_actor_out", direction: "outbound", conversationId: "conv_actor_out",
      createdAt: new Date("2026-08-08T12:00:00.500Z"), headerFrom: ACTOR,
      envelopeFrom: "agent@agents.localhost", to: [TARGET], cc: [], replyTo: [],
      sentAs: "relay", subject: SUBJECT, rawMessage: actorOutbound,
    };
    events[0].data.header_from = actorPhysical;
    events[0].data.envelope_from = "agent@agents.localhost";
    events[0].data.reply_to = [ACTOR];
    messages.msg_synthetic_target_in.headerFrom = actorPhysical;
    messages.msg_synthetic_target_in.envelopeFrom = "agent@agents.localhost";
    messages.msg_synthetic_target_in.replyTo = [ACTOR];
    messages.msg_synthetic_target_in.rawMessage = actorOutbound;
    events[1].data.sent_as = "relay";
    messages.msg_synthetic_target_out.sentAs = "relay";
    messages.msg_synthetic_target_out.headerFrom = TARGET;
    messages.msg_synthetic_target_out.envelopeFrom = "agent@agents.localhost";
    messages.msg_synthetic_target_out.rawMessage = rawMessage({
      messageId: "reply@agents.localhost", from: targetPhysical, to: ACTOR, replyTo: TARGET,
      subject: `Re: ${SUBJECT}`, inReplyTo: "original@agents.localhost",
      references: ["original@agents.localhost"], text: "Refunds are available within 30 days.",
    });
    events[2].data.header_from = targetPhysical;
    events[2].data.envelope_from = "agent@agents.localhost";
    events[2].data.reply_to = [TARGET];
    messages.msg_synthetic_actor_in.headerFrom = targetPhysical;
    messages.msg_synthetic_actor_in.envelopeFrom = "agent@agents.localhost";
    messages.msg_synthetic_actor_in.replyTo = [TARGET];
    messages.msg_synthetic_actor_in.rawMessage = messages.msg_synthetic_target_out.rawMessage;
    if (pathName === "stimulus") {
      messages.msg_synthetic_target_in.headerFrom = "Eval Actor <agent@agents.localhost>";
      messages.msg_synthetic_target_in.rawMessage = rawMessage({
        messageId: "original@agents.localhost", from: "Eval Actor <agent@agents.localhost>", to: TARGET,
        subject: SUBJECT, text: "Can fictional order ord_example_123 be refunded?",
      });
    } else {
      messages.msg_synthetic_actor_in.headerFrom = "Target Agent <agent@agents.localhost>";
      messages.msg_synthetic_actor_in.rawMessage = rawMessage({
        messageId: "reply@agents.localhost", from: "Target Agent <agent@agents.localhost>", to: ACTOR,
        subject: `Re: ${SUBJECT}`, inReplyTo: "original@agents.localhost",
        references: ["original@agents.localhost"], text: "Refunds are available within 30 days.",
      });
    }
    await assert.rejects(
      adapter(fakeClient({
        events,
        messages,
        send: async () => ({
          messageId: "msg_synthetic_actor_out", status: "sent", method: "smtp", sentAs: "relay",
        }),
      })).adapter.executeCase(caseSpec({ expect: {
        ...caseSpec().expect,
        sender: { exactly: TARGET, sentAs: "relay", replyTo: { exactly: [TARGET] } },
      } }), caseContext()),
      (error) => error.errorClass === "transport_error" && error.code === "malformed_message",
      pathName,
    );
  }
});

test("relay physical sender and every non-relay logical/physical conflict fail closed", async () => {
  for (const [mode, mutate] of [
    ["relay", (events, messages) => {
      events[1].data.sent_as = "relay";
      messages.msg_synthetic_target_out.sentAs = "relay";
      messages.msg_synthetic_target_out.rawMessage = rawMessage({
        messageId: "reply@agents.localhost", from: "not a complete mailbox", to: ACTOR,
        subject: `Re: ${SUBJECT}`, inReplyTo: "original@agents.localhost",
        references: ["original@agents.localhost"], text: "Refunds are available within 30 days.",
      });
    }],
    ["relay", (events, messages) => {
      events[1].data.sent_as = "relay";
      messages.msg_synthetic_target_out.sentAs = "relay";
      messages.msg_synthetic_target_out.rawMessage = rawMessage({
        messageId: "reply@agents.localhost", from: "Target Agent <agent@agents.localhost>", to: ACTOR,
        subject: `Re: ${SUBJECT}`, inReplyTo: "original@agents.localhost",
        references: ["original@agents.localhost"], text: "Refunds are available within 30 days.",
      });
    }],
    ["relay", (events, messages) => {
      events[1].data.sent_as = "relay";
      events[1].data.method = "loopback";
      messages.msg_synthetic_target_out.sentAs = "relay";
      messages.msg_synthetic_target_out.rawMessage = rawMessage({
        messageId: "reply@agents.localhost", from: "Target Agent via e2a <agent@agents.localhost>", to: ACTOR,
        subject: `Re: ${SUBJECT}`, inReplyTo: "original@agents.localhost",
        references: ["original@agents.localhost"], text: "Refunds are available within 30 days.",
      });
    }],
    ["relay", (events, messages) => {
      events[1].data.sent_as = "relay";
      events[1].data.from = "other@eval.test";
      messages.msg_synthetic_target_out.sentAs = "relay";
    }],
    ["own_address", (_events, messages) => {
      messages.msg_synthetic_target_out.rawMessage = rawMessage({
        messageId: "reply@agents.localhost", from: "other@eval.test", to: ACTOR,
        subject: `Re: ${SUBJECT}`, inReplyTo: "original@agents.localhost",
        references: ["original@agents.localhost"], text: "Refunds are available within 30 days.",
      });
    }],
  ]) {
    const events = await fixture("events-success.json");
    const messages = successfulMessages();
    mutate(events, messages);
    await assert.rejects(
      adapter(fakeClient({ events, messages })).adapter.executeCase(caseSpec({ expect: {
        ...caseSpec().expect, sender: { ...caseSpec().expect.sender, sentAs: mode },
      } }), caseContext()),
      (error) => error.errorClass === "transport_error"
        && ["conflicting_evidence", "mime_observation_failed", "malformed_message"].includes(error.code),
    );
  }
});

test("stimulus and candidates share one cumulative MIME budget", async () => {
  const events = await fixture("events-success.json");
  const messages = successfulMessages();
  const stimulusBytes = Buffer.from(messages.msg_synthetic_target_in.rawMessage, "base64").length;
  const candidateBytes = Buffer.from(messages.msg_synthetic_target_out.rawMessage, "base64").length;
  const client = fakeClient({ events, messages });
  const time = clock();
  const e2a = createE2AAdapter({
    apiKey: "not-logged", baseUrl: "https://api.example.test", client,
    now: time.now, sleep: time.sleep, mimeBudgetBytes: stimulusBytes + candidateBytes - 1,
  });
  await assert.rejects(
    e2a.executeCase(caseSpec(), caseContext()),
    (error) => error.errorClass === "transport_error" && error.code === "mime_observation_failed",
  );
});

test("candidate MIME is charged before later raw messages are fetched or retained", async () => {
  const sourceEvents = await fixture("events-success.json");
  const events = [structuredClone(sourceEvents[0])];
  events[0].conversationId = "";
  events[0].data.conversation_id = "";
  const messages = successfulMessages();
  messages.msg_synthetic_target_in.conversationId = "";
  const candidateBytes = [];
  for (let index = 1; index <= 3; index += 1) {
    const event = structuredClone(sourceEvents[1]);
    event.id = `evt_synthetic_candidate_${index}`;
    event.messageId = `msg_synthetic_candidate_${index}`;
    event.conversationId = `conv_candidate_${index}`;
    event.createdAt = `2026-08-08T12:00:04.${index}00Z`;
    event.data.message_id = event.messageId;
    event.data.conversation_id = event.conversationId;
    event.data.provider_message_id = `<reply-${index}@agents.localhost>`;
    events.push(event);
    const raw = rawMessage({
      messageId: `reply-${index}@agents.localhost`, from: TARGET, to: ACTOR,
      subject: `Re: ${SUBJECT}`, inReplyTo: "original@agents.localhost",
      references: ["original@agents.localhost"], text: "Refunds are available within 30 days.",
    });
    candidateBytes.push(Buffer.from(raw, "base64").length);
    messages[event.messageId] = {
      id: event.messageId, direction: "outbound", conversationId: event.conversationId,
      createdAt: new Date(event.createdAt), headerFrom: TARGET, to: [ACTOR], cc: [],
      replyTo: [], sentAs: "own_address", subject: `Re: ${SUBJECT}`, rawMessage: raw,
    };
  }
  const stimulusBytes = Buffer.from(messages.msg_synthetic_target_in.rawMessage, "base64").length;
  const client = fakeClient({ events, messages });
  const time = clock();
  const e2a = createE2AAdapter({
    apiKey: "not-logged", baseUrl: "https://api.example.test", client,
    now: time.now, sleep: time.sleep,
    mimeBudgetBytes: stimulusBytes + candidateBytes[0] + candidateBytes[1] - 1,
  });
  await assert.rejects(
    e2a.executeCase(caseSpec(), caseContext()),
    (error) => error.errorClass === "transport_error" && error.code === "mime_observation_failed",
  );
  assert.equal(client.calls.gets.some(({ messageId }) => messageId === "msg_synthetic_candidate_3"), false);
});

test("an in-flight network operation cannot outlive the case deadline", async () => {
  const client = fakeClient({ send: async () => new Promise(() => {}) });
  const clientOptions = [];
  const e2a = createE2AAdapter({
    apiKey: "not-logged", baseUrl: "https://api.example.test",
    clientFactory: (options) => { clientOptions.push(options); return client; },
    now: () => new Date(), sleep: async () => {},
  });
  const outcome = await Promise.race([
    e2a.executeCase(caseSpec(), caseContext({ timeoutMs: 25 })).then(
      () => ({ type: "resolved" }),
      (error) => ({ type: "rejected", error }),
    ),
    new Promise((resolve) => setTimeout(() => resolve({ type: "hung" }), 250)),
  ]);
  assert.equal(outcome.type, "rejected");
  assert.equal(outcome.error.errorClass, "transport_error");
  assert.equal(outcome.error.code, "stimulus_send_failed");
  assert.ok(clientOptions.length >= 3);
  assert.ok(clientOptions.slice(1).every((options) => options.maxRetries === 0
    && options.maxElapsedMs > 0 && options.maxElapsedMs <= 25
    && options.timeoutMs > 0 && options.timeoutMs <= options.maxElapsedMs));
});

test("case deadline is checked after baseline and before every stimulus send", async () => {
  let milliseconds = START;
  const client = fakeClient({
    listMessages: (_email, _params, call) => call === 1 ? {
      async toArray() {
        milliseconds += 5_000;
        return [];
      },
    } : pager([]),
  });
  const e2a = createE2AAdapter({
    apiKey: "not-logged", baseUrl: "https://api.example.test", client,
    now: () => new Date(milliseconds),
    sleep: async () => {},
  });
  await assert.rejects(
    e2a.executeCase(caseSpec(), caseContext()),
    (error) => error.errorClass === "transport_error" && error.code === "stimulus_send_failed",
  );
  assert.equal(client.calls.send.length, 0);
});
