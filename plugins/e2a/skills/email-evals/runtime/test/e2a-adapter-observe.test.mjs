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

function rawMessage({ messageId, from, to, subject, inReplyTo, references, text }) {
  const headers = [
    `Message-ID: <${messageId}>`,
    `From: ${from}`,
    `To: ${to}`,
    `Subject: ${subject}`,
    ...(inReplyTo ? [`In-Reply-To: <${inReplyTo}>`] : []),
    ...(references ? [`References: ${references.map((value) => `<${value}>`).join(" ")}`] : []),
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
      sender: { exactly: TARGET, sentAs: TARGET, replyTo: { exactly: [] } },
      recipients: {
        to: { exactly: [ACTOR] }, cc: { exactly: [] }, bcc: { exactly: [] }, envelope: { exactly: [ACTOR] },
      },
      thread: { inReplyTo: "original", references: "contains_original", conversation: "same" },
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
    suiteDigest: "a".repeat(64), runId: "run_synthetic", actor: ACTOR, target: TARGET,
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
  assert.equal(evidence.candidates[0].sentAs, TARGET);
  assert.equal(evidence.stimulus.rfcMessageId, "original@agents.localhost");
  assert.equal(evidence.actorReceipt.messageId, "msg_synthetic_target_out");
  assert.doesNotMatch(JSON.stringify(evidence), /rawMessage|must not escape|Content-Transfer-Encoding/);
  for (const result of [...gradeCore(caseSpec().expect, evidence), ...gradeContent(caseSpec().expect, evidence)]) {
    assert.equal(result.status, "pass", `${result.id}: ${result.code}`);
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
  messages[duplicate.messageId] = { ...messages.msg_synthetic_target_out, id: duplicate.messageId };
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
});

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
