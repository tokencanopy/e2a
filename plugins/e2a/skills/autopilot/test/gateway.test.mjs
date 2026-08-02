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

function fixture({ ccOwner = true } = {}) {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-gateway-"));
  const calls = [];
  const mail = {
    async getMessage(messageId) {
      calls.push(["getMessage", messageId]);
      return {
        id: messageId,
        conversationId: "conv_current",
        headerFrom: "customer@example.test",
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
    job: { messageId: "msg_current", jobId: "job_current" },
    ownerEmail: "owner@example.com",
    ccOwner,
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

test("reply injects owner CC, chooses recipients server-side, and uses a stable idempotency key", async (t) => {
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
  assert.equal(calls[0][0], "reply");
  assert.equal(calls[0][1], "msg_current");
  assert.deepEqual(calls[0][2].cc, ["owner@example.com"]);
  assert.equal(calls[0][2].replyAll, true);
  assert.match(calls[0][2].idempotencyKey, /^autopilot-[a-f0-9]{64}$/);
  assert.equal(calls[0][2].idempotencyKey, calls[1][2].idempotencyKey);
  assert.deepEqual(first.body, second.body);
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
  assert.deepEqual(calls[0][2].cc, []);
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
});
