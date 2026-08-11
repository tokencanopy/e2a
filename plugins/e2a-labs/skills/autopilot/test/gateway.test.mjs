import assert from "node:assert/strict";
import { mkdtempSync, statSync } from "node:fs";
import http from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { JobGateway } from "../gateway.mjs";

function request(socketPath, token, { method = "GET", route, body, auth = true }) {
  return new Promise((resolve, reject) => {
    const payload = body === undefined ? undefined : JSON.stringify(body);
    const req = http.request(
      {
        socketPath,
        path: route,
        method,
        headers: {
          ...(auth ? { authorization: `Bearer ${token}` } : {}),
          ...(payload
            ? {
                "content-type": "application/json",
                "content-length": Buffer.byteLength(payload),
              }
            : {}),
        },
      },
      (res) => {
        let data = "";
        res.setEncoding("utf8");
        res.on("data", (chunk) => (data += chunk));
        res.on("end", () => {
          resolve({
            status: res.statusCode,
            body: data ? JSON.parse(data) : null,
          });
        });
      },
    );
    req.on("error", reject);
    if (payload) req.write(payload);
    req.end();
  });
}

function fixture({ ccOwner = true, replyMode = "submit-for-review", replyCheckpoint } = {}) {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-gateway-"));
  const calls = [];
  const mail = {
    async getMessage(messageId) {
      calls.push(["getMessage", messageId]);
      return {
        id: messageId,
        conversationId: "conv_current",
        headerFrom: "customer@example.test",
        replyTo: [],
        subject: "Need help",
        body: { text: "How do I configure this?" },
      };
    },
    async getThread(messageId) {
      calls.push(["getThread", messageId]);
      return [{ id: messageId }, { id: "msg_prior" }];
    },
    async reply(messageId, input) {
      calls.push(["reply", messageId, input]);
      return { messageId: "msg_reply", status: "pending_review" };
    },
  };
  const escalations = [];
  const completions = [];
  const gateway = new JobGateway({
    socketPath: path.join(root, "job.sock"),
    job: {
      messageId: "msg_current",
      jobId: "job_current",
      authorizedFrom: "customer@example.test",
    },
    ownerEmail: "owner@example.com",
    ccOwner,
    replyMode,
    replyCheckpoint,
    mail,
    onEscalate: async (value) => escalations.push(value),
    onComplete: async (value) => completions.push(value),
  });
  return { root, gateway, calls, escalations, completions };
}

test("gateway requires the per-job capability and creates an owner-only socket", async (t) => {
  const { gateway } = fixture();
  const connection = await gateway.start();
  t.after(() => gateway.close());

  const denied = await request(connection.socketPath, connection.token, {
    route: "/v1/current-message",
    auth: false,
  });
  assert.equal(denied.status, 401);

  const allowed = await request(connection.socketPath, connection.token, {
    route: "/v1/current-message",
  });
  assert.equal(allowed.status, 200);
  assert.equal(allowed.body.message.id, "msg_current");
  assert.equal(statSync(connection.socketPath).mode & 0o777, 0o600);
});

test("gateway binds message and thread reads to the current job", async (t) => {
  const { gateway, calls } = fixture();
  const connection = await gateway.start();
  t.after(() => gateway.close());

  const message = await request(connection.socketPath, connection.token, {
    route: "/v1/current-message",
  });
  const thread = await request(connection.socketPath, connection.token, {
    route: "/v1/current-thread",
  });

  assert.equal(message.status, 200);
  assert.equal(thread.status, 200);
  assert.deepEqual(calls, [
    ["getMessage", "msg_current"],
    ["getThread", "msg_current"],
  ]);
});

test("reply injects owner CC and permits only one durable logical reply", async (t) => {
  const { gateway, calls } = fixture();
  const connection = await gateway.start();
  t.after(() => gateway.close());

  const first = await request(connection.socketPath, connection.token, {
    method: "POST",
    route: "/v1/reply",
    body: { text: "Here are the setup steps." },
  });
  const second = await request(connection.socketPath, connection.token, {
    method: "POST",
    route: "/v1/reply",
    body: { text: "Here are the setup steps." },
  });

  assert.equal(first.status, 200);
  assert.equal(first.body.result.status, "pending_review");
  const reply = calls.find(([name]) => name === "reply");
  assert.equal(reply[1], "msg_current");
  assert.deepEqual(reply[2].cc, ["owner@example.com"]);
  assert.equal(reply[2].replyAll, false);
  assert.match(reply[2].idempotencyKey, /^autopilot-[a-f0-9]{64}$/);
  assert.equal(calls.filter(([name]) => name === "reply").length, 1);
  assert.deepEqual(first.body, second.body);
});

test("reply checkpoint prevents a changed retry from submitting a second message", async (t) => {
  const firstFixture = fixture();
  const firstConnection = await firstFixture.gateway.start();
  const first = await request(firstConnection.socketPath, firstConnection.token, {
    method: "POST",
    route: "/v1/reply",
    body: { text: "Original answer." },
  });
  await firstFixture.gateway.close();

  const checkpoint = firstFixture.gateway.replyCheckpoint;
  const retryFixture = fixture({ replyCheckpoint: checkpoint });
  const retryConnection = await retryFixture.gateway.start();
  t.after(() => retryFixture.gateway.close());
  const changed = await request(retryConnection.socketPath, retryConnection.token, {
    method: "POST",
    route: "/v1/reply",
    body: { text: "A different answer." },
  });

  assert.equal(first.status, 200);
  assert.equal(changed.status, 409);
  assert.equal(retryFixture.calls.filter(([name]) => name === "reply").length, 0);
});

test("draft-only policy rejects the reply operation", async (t) => {
  const { gateway, calls } = fixture({ replyMode: "draft-only" });
  const connection = await gateway.start();
  t.after(() => gateway.close());

  const response = await request(connection.socketPath, connection.token, {
    method: "POST",
    route: "/v1/reply",
    body: { text: "This must remain a draft." },
  });

  assert.equal(response.status, 403);
  assert.deepEqual(calls, []);
});

test("reply rejects attempts to select recipients or another message", async (t) => {
  const { gateway, calls } = fixture();
  const connection = await gateway.start();
  t.after(() => gateway.close());

  for (const body of [
    { text: "hello", to: ["attacker@example.test"] },
    { text: "hello", cc: [] },
    { text: "hello", messageId: "msg_other" },
  ]) {
    const response = await request(connection.socketPath, connection.token, {
      method: "POST",
      route: "/v1/reply",
      body,
    });
    assert.equal(response.status, 400);
  }
  assert.deepEqual(calls, []);
});

test("reply refuses a sender-controlled Reply-To target", async (t) => {
  const { gateway, calls } = fixture();
  gateway.mail.getMessage = async (messageId) => {
    calls.push(["getMessage", messageId]);
    return {
      id: messageId,
      headerFrom: "customer@example.test",
      replyTo: ["redirect@unrelated.test"],
    };
  };
  const connection = await gateway.start();
  t.after(() => gateway.close());

  const response = await request(connection.socketPath, connection.token, {
    method: "POST",
    route: "/v1/reply",
    body: { text: "Do not redirect this." },
  });

  assert.equal(response.status, 409);
  assert.equal(calls.filter(([name]) => name === "reply").length, 0);
});

test("owner CC can only be omitted when the confirmed policy opted out", async (t) => {
  const { gateway, calls } = fixture({ ccOwner: false });
  const connection = await gateway.start();
  t.after(() => gateway.close());

  const response = await request(connection.socketPath, connection.token, {
    method: "POST",
    route: "/v1/reply",
    body: { text: "Reply without owner CC." },
  });

  assert.equal(response.status, 200);
  assert.deepEqual(calls.find(([name]) => name === "reply")[2].cc, []);
});

test("gateway exposes escalation and completion but no mailbox list or delete route", async (t) => {
  const { gateway, escalations, completions } = fixture();
  const connection = await gateway.start();
  t.after(() => gateway.close());

  const escalation = await request(connection.socketPath, connection.token, {
    method: "POST",
    route: "/v1/escalate",
    body: { reason: "Billing decision required." },
  });
  const completion = await request(connection.socketPath, connection.token, {
    method: "POST",
    route: "/v1/complete",
    body: { summary: "Escalated without replying." },
  });
  const list = await request(connection.socketPath, connection.token, {
    route: "/v1/messages",
  });
  const deletion = await request(connection.socketPath, connection.token, {
    method: "DELETE",
    route: "/v1/current-message",
  });

  assert.equal(escalation.status, 200);
  assert.equal(completion.status, 200);
  assert.equal(list.status, 404);
  assert.equal(deletion.status, 404);
  assert.deepEqual(escalations, [
    { jobId: "job_current", messageId: "msg_current", reason: "Billing decision required." },
  ]);
  assert.deepEqual(completions, [
    { jobId: "job_current", messageId: "msg_current", summary: "Escalated without replying." },
  ]);

  const repeatedEscalation = await request(connection.socketPath, connection.token, {
    method: "POST",
    route: "/v1/escalate",
    body: { reason: "Try to notify twice." },
  });
  const repeatedCompletion = await request(connection.socketPath, connection.token, {
    method: "POST",
    route: "/v1/complete",
    body: { summary: "Try to complete twice." },
  });
  assert.equal(repeatedEscalation.status, 409);
  assert.equal(repeatedCompletion.status, 409);
});

test("a failed escalation side effect stays retryable instead of latching", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-gateway-"));
  let failures = 1;
  const escalations = [];
  const gateway = new JobGateway({
    socketPath: path.join(root, "job.sock"),
    job: {
      messageId: "msg_current",
      jobId: "job_current",
      authorizedFrom: "customer@example.test",
    },
    ownerEmail: "owner@example.com",
    ccOwner: true,
    mail: {
      getMessage: async (messageId) => ({ id: messageId }),
      getThread: async () => [],
      reply: async () => ({ status: "pending_review" }),
    },
    onEscalate: async (value) => {
      if (failures > 0) {
        failures -= 1;
        throw new Error("synthetic notification failure");
      }
      escalations.push(value);
    },
  });
  const connection = await gateway.start();
  t.after(() => gateway.close());

  const failed = await request(connection.socketPath, connection.token, {
    method: "POST",
    route: "/v1/escalate",
    body: { reason: "Billing decision required." },
  });
  assert.equal(failed.status, 500);
  assert.equal(escalations.length, 0);

  const retried = await request(connection.socketPath, connection.token, {
    method: "POST",
    route: "/v1/escalate",
    body: { reason: "Billing decision required." },
  });
  assert.equal(retried.status, 200);
  assert.equal(escalations.length, 1);

  const repeated = await request(connection.socketPath, connection.token, {
    method: "POST",
    route: "/v1/escalate",
    body: { reason: "Billing decision required." },
  });
  assert.equal(repeated.status, 409);
});
