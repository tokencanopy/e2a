import assert from "node:assert/strict";
import test from "node:test";

import { E2aMailClient } from "../mail-client.mjs";

function jsonResponse(status, body) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

test("getMessage scopes the existing API request to the configured agent", async () => {
  const calls = [];
  const client = new E2aMailClient({
    baseUrl: "https://api.example.test/",
    apiKey: "e2a_agt_synthetic",
    agentEmail: "support+west@example.test",
    fetchImpl: async (url, init) => {
      calls.push([url, init]);
      return jsonResponse(200, { id: "msg/one", conversation_id: "conv_1" });
    },
  });

  const message = await client.getMessage("msg/one");

  assert.equal(message.id, "msg/one");
  assert.equal(
    calls[0][0],
    "https://api.example.test/v1/agents/support%2Bwest%40example.test/messages/msg%2Fone",
  );
  assert.equal(calls[0][1].method, "GET");
  assert.equal(calls[0][1].headers.authorization, "Bearer e2a_agt_synthetic");
});

test("getThread follows current pagination for only the source conversation", async () => {
  const calls = [];
  const client = new E2aMailClient({
    baseUrl: "https://api.example.test",
    apiKey: "e2a_agt_synthetic",
    agentEmail: "support@example.test",
    fetchImpl: async (url, init) => {
      calls.push([url, init]);
      if (url.includes("/messages/msg_current")) {
        return jsonResponse(200, {
          id: "msg_current",
          conversation_id: "conv/current",
        });
      }
      if (url.includes("cursor=cursor_2")) {
        return jsonResponse(200, {
          items: [{ id: "msg_current", conversation_id: "conv/current" }],
        });
      }
      return jsonResponse(200, {
        items: [{ id: "msg_prior", conversation_id: "conv/current" }],
        next_cursor: "cursor_2",
      });
    },
  });

  const messages = await client.getThread("msg_current");

  assert.deepEqual(messages.map((message) => message.id), ["msg_prior", "msg_current"]);
  assert.match(calls[1][0], /conversation_id=conv%2Fcurrent/);
  assert.match(calls[1][0], /read_status=all/);
  assert.match(calls[1][0], /sort=asc/);
  assert.match(calls[2][0], /cursor=cursor_2/);
});

test("reply uses the existing reply endpoint with gateway-selected CC and idempotency", async () => {
  const calls = [];
  const client = new E2aMailClient({
    baseUrl: "https://api.example.test",
    apiKey: "e2a_agt_synthetic",
    agentEmail: "support@example.test",
    fetchImpl: async (url, init) => {
      calls.push([url, init]);
      return jsonResponse(202, { message_id: "msg_reply", status: "pending_review" });
    },
  });

  const result = await client.reply("msg_current", {
    text: "Here are the setup steps.",
    cc: ["owner@example.test"],
    replyAll: true,
    idempotencyKey: "autopilot-example-key",
  });

  assert.equal(result.status, "pending_review");
  assert.equal(
    calls[0][0],
    "https://api.example.test/v1/agents/support%40example.test/messages/msg_current/reply",
  );
  assert.equal(calls[0][1].method, "POST");
  assert.equal(calls[0][1].headers["idempotency-key"], "autopilot-example-key");
  assert.deepEqual(JSON.parse(calls[0][1].body), {
    text: "Here are the setup steps.",
    cc: ["owner@example.test"],
    reply_all: true,
  });
});

test("notifyOwner uses the existing send endpoint with one fixed recipient", async () => {
  const calls = [];
  const client = new E2aMailClient({
    baseUrl: "https://api.example.test",
    apiKey: "e2a_agt_synthetic",
    agentEmail: "support@example.test",
    fetchImpl: async (url, init) => {
      calls.push([url, init]);
      return jsonResponse(202, { message_id: "msg_notice", status: "pending_review" });
    },
  });

  const result = await client.notifyOwner({
    ownerEmail: "owner@example.test",
    subject: "Autopilot needs attention",
    text: "A billing decision is required.",
    idempotencyKey: "autopilot-notice-example",
  });

  assert.equal(result.status, "pending_review");
  assert.equal(
    calls[0][0],
    "https://api.example.test/v1/agents/support%40example.test/messages",
  );
  assert.deepEqual(JSON.parse(calls[0][1].body), {
    to: ["owner@example.test"],
    subject: "Autopilot needs attention",
    text: "A billing decision is required.",
  });
  assert.equal(calls[0][1].headers["idempotency-key"], "autopilot-notice-example");
});

test("API failures do not expose the credential or response body", async () => {
  const client = new E2aMailClient({
    baseUrl: "https://api.example.test",
    apiKey: "e2a_agt_synthetic_do_not_leak",
    agentEmail: "support@example.test",
    fetchImpl: async () =>
      jsonResponse(403, {
        error: {
          code: "forbidden",
          message: "internal details that should not enter supervisor logs",
        },
      }),
  });

  await assert.rejects(
    () => client.getMessage("msg_current"),
    (error) => {
      assert.equal(error.code, "forbidden");
      assert.match(error.message, /status 403.*forbidden/);
      assert.doesNotMatch(error.message, /do_not_leak|internal details/);
      return true;
    },
  );
});

test("mail client refuses cleartext non-loopback base URLs", () => {
  assert.throws(
    () =>
      new E2aMailClient({
        baseUrl: "http://api.e2a.example.test",
        apiKey: "e2a_agt_synthetic",
        agentEmail: "support@example.test",
      }),
    /must use HTTPS/,
  );

  const loopback = new E2aMailClient({
    baseUrl: "http://127.0.0.1:8787",
    apiKey: "e2a_agt_synthetic",
    agentEmail: "support@example.test",
  });
  assert.equal(loopback.baseUrl, "http://127.0.0.1:8787");
});
