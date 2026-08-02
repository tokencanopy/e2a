import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtempSync } from "node:fs";
import http from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { JobSpool } from "../spool.mjs";
import {
  AutopilotSupervisor,
  createForwardReceiver,
  isAuthorizedMessage,
  jobSocketRoot,
} from "../supervisor.mjs";

const helperPath = path.resolve(import.meta.dirname, "..", "job-tool.mjs");

function basePolicy(mode = "addresses") {
  return {
    task: {
      profile: "customer-support",
      objective: "Resolve routine support questions.",
      instructions: "Use the approved handbook. Escalate billing requests.",
      replyMode: "submit-for-review",
    },
    mailbox: {
      agentEmail: "support@example.test",
      ownerEmail: "owner@example.test",
    },
    inbound: {
      mode,
      addresses: mode === "addresses" ? ["customer@buyer.test"] : [],
      domains: mode === "domains" ? ["buyer.test"] : [],
      fallback: "review",
    },
    outbound: { requireReview: true, ccOwner: true },
    screening: { promptInjection: true },
    runtime: {
      adapter: "custom",
      command: process.execPath,
      workdir: "/tmp",
      sandbox: "custom",
    },
    service: { manager: "foreground" },
    acknowledgements: ["custom_sandbox_acknowledged"],
  };
}

function post(port, token, body) {
  return new Promise((resolve, reject) => {
    const payload = typeof body === "string" ? body : JSON.stringify(body);
    const req = http.request(
      {
        host: "127.0.0.1",
        port,
        path: "/hook",
        method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "content-type": "application/json",
          "content-length": Buffer.byteLength(payload),
        },
      },
      (response) => {
        let data = "";
        response.setEncoding("utf8");
        response.on("data", (chunk) => (data += chunk));
        response.on("end", () => resolve({ status: response.statusCode, body: data }));
      },
    );
    req.on("error", reject);
    req.end(payload);
  });
}

function runHelper(command, invocation, input = "") {
  return new Promise((resolve) => {
    const child = spawn(process.execPath, [helperPath, command], {
      env: invocation.env,
      stdio: ["pipe", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.setEncoding("utf8");
    child.stderr.setEncoding("utf8");
    child.stdout.on("data", (chunk) => (stdout += chunk));
    child.stderr.on("data", (chunk) => (stderr += chunk));
    child.on("close", (code) =>
      resolve({ code: code ?? -1, stdout, stderr, timedOut: false }),
    );
    child.stdin.end(input);
  });
}

test("job socket root stays short even when installed state paths are deep", () => {
  const deepState = `/Users/synthetic-operator/${"nested/".repeat(30)}state`;
  const socketRoot = jobSocketRoot(deepState, "/tmp");

  assert.match(socketRoot, /^\/tmp\/e2a-autopilot-[a-f0-9]{16}$/);
  assert.ok(Buffer.byteLength(path.join(socketRoot, `${"f".repeat(16)}.sock`)) < 100);
});

test("address authorization requires exact From plus aligned verified domain", () => {
  const policy = basePolicy("addresses");

  assert.equal(
    isAuthorizedMessage(policy, {
      headerFrom: "Customer <customer@buyer.test>",
      verifiedDomain: "buyer.test",
    }),
    true,
  );
  assert.equal(
    isAuthorizedMessage(policy, {
      headerFrom: "customer@buyer.test",
      verifiedDomain: "spoof.test",
    }),
    false,
  );
  assert.equal(
    isAuthorizedMessage(policy, {
      headerFrom: "other@buyer.test",
      verifiedDomain: "buyer.test",
    }),
    false,
  );
});

test("domain authorization accepts only senders under a verified configured domain", () => {
  const policy = basePolicy("domains");

  assert.equal(
    isAuthorizedMessage(policy, {
      headerFrom: "person@buyer.test",
      verifiedDomain: "buyer.test",
    }),
    true,
  );
  assert.equal(
    isAuthorizedMessage(policy, {
      headerFrom: "person@sub.buyer.test",
      verifiedDomain: "sub.buyer.test",
    }),
    false,
  );
});

test("forward receiver persists an authorized job before acknowledging it", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-receiver-"));
  const spool = new JobSpool(path.join(root, "jobs"));
  const receiver = await createForwardReceiver({
    port: 0,
    token: "synthetic_forward_token",
    policy: basePolicy(),
    spool,
  });
  t.after(() => receiver.close());

  const response = await post(receiver.port, "synthetic_forward_token", {
    id: "msg_authorized",
    headerFrom: "customer@buyer.test",
    verifiedDomain: "buyer.test",
    body: { text: "This body must not enter the spool." },
  });

  assert.equal(response.status, 200);
  const pending = spool.list("pending");
  assert.equal(pending.length, 1);
  assert.equal(pending[0].messageId, "msg_authorized");
  assert.equal(pending[0].body, undefined);
});

test("forward receiver refuses unauthorized, unauthenticated, and malformed events", async (t) => {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-receiver-"));
  const spool = new JobSpool(path.join(root, "jobs"));
  const receiver = await createForwardReceiver({
    port: 0,
    token: "synthetic_forward_token",
    policy: basePolicy(),
    spool,
  });
  t.after(() => receiver.close());

  const unauthorized = await post(receiver.port, "synthetic_forward_token", {
    id: "msg_unauthorized",
    headerFrom: "attacker@evil.test",
    verifiedDomain: "evil.test",
  });
  const wrongToken = await post(receiver.port, "wrong", {
    id: "msg_authorized",
    headerFrom: "customer@buyer.test",
    verifiedDomain: "buyer.test",
  });
  const malformed = await post(receiver.port, "synthetic_forward_token", "not-json");

  assert.equal(unauthorized.status, 202);
  assert.equal(wrongToken.status, 401);
  assert.equal(malformed.status, 400);
  assert.deepEqual(spool.counts(), {
    pending: 0,
    running: 0,
    retry: 0,
    done: 0,
    dead: 0,
  });
});

test("supervisor completes a job only after the runtime calls the scoped complete operation", async () => {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-supervisor-"));
  const spool = new JobSpool(path.join(root, "jobs"));
  spool.enqueue({
    messageId: "msg_current",
    from: "customer@buyer.test",
    verifiedDomain: "buyer.test",
    source: "listener",
  });
  const mail = {
    getMessage: async (id) => ({ id, conversation_id: "conv_current" }),
    getThread: async (id) => [{ id, conversation_id: "conv_current" }],
    reply: async () => ({ message_id: "msg_reply", status: "pending_review" }),
  };
  const supervisor = new AutopilotSupervisor({
    policy: basePolicy(),
    spool,
    mail,
    stateRoot: root,
    helperPath,
    runtimeExecutor: async (invocation) => {
      const message = await runHelper("current-message", invocation);
      assert.equal(message.code, 0, message.stderr);
      return runHelper("complete", invocation, "Handled safely.\n");
    },
  });

  const result = await supervisor.runNextJob();

  assert.equal(result.state, "done");
  assert.equal(spool.list("done")[0].outcome.completed, true);
  assert.equal(spool.list("done")[0].outcome.summary, undefined);
});

test("accepted reply is not submitted again when the runtime crashes and redrafts", async () => {
  let current = 1_700_000_000_000;
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-supervisor-"));
  const spool = new JobSpool(path.join(root, "jobs"), { now: () => current });
  spool.enqueue({
    messageId: "msg_reply_retry",
    from: "customer@buyer.test",
    source: "listener",
  });
  const replies = [];
  let attempt = 0;
  const supervisor = new AutopilotSupervisor({
    policy: basePolicy(),
    spool,
    mail: {
      getMessage: async (id) => ({
        id,
        conversation_id: "conv_current",
        header_from: "customer@buyer.test",
        reply_to: [],
      }),
      getThread: async () => [],
      reply: async (_id, input) => {
        replies.push(input);
        return { message_id: "msg_reply", status: "pending_review" };
      },
    },
    stateRoot: root,
    helperPath,
    baseDelayMs: 1,
    runtimeExecutor: async (invocation) => {
      attempt += 1;
      const response = await runHelper(
        "reply",
        invocation,
        attempt === 1 ? "Original answer.\n" : "Changed answer.\n",
      );
      if (attempt === 1) return { ...response, code: 1 };
      assert.equal(response.code, 1);
      assert.match(response.stderr, /already submitted/i);
      return runHelper("complete", invocation, "Reply was already submitted.\n");
    },
  });

  assert.equal((await supervisor.runNextJob()).state, "retry");
  current += 1;
  assert.equal((await supervisor.runNextJob()).state, "done");
  assert.equal(replies.length, 1);
  assert.equal(spool.list("done")[0].effects.reply.status, "pending_review");
});

test("a zero-exit runtime that never calls complete is retried", async () => {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-supervisor-"));
  const spool = new JobSpool(path.join(root, "jobs"));
  spool.enqueue({
    messageId: "msg_incomplete",
    from: "customer@buyer.test",
    source: "reconcile",
  });
  const supervisor = new AutopilotSupervisor({
    policy: basePolicy(),
    spool,
    mail: {
      getMessage: async (id) => ({ id, conversation_id: "conv_current" }),
      getThread: async () => [],
      reply: async () => ({ status: "pending_review" }),
    },
    stateRoot: root,
    helperPath,
    runtimeExecutor: async () => ({ code: 0, timedOut: false, stdout: "", stderr: "" }),
  });

  const result = await supervisor.runNextJob();

  assert.equal(result.state, "retry");
  assert.match(spool.list("retry")[0].lastError, /without completing/i);
});

test("escalation notifies only the configured owner and records the held result", async () => {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-supervisor-"));
  const spool = new JobSpool(path.join(root, "jobs"));
  spool.enqueue({
    messageId: "msg_escalate",
    from: "customer@buyer.test",
    source: "listener",
  });
  const notices = [];
  const mail = {
    getMessage: async (id) => ({ id, conversation_id: "conv_current" }),
    getThread: async () => [],
    reply: async () => ({ status: "pending_review" }),
    notifyOwner: async (input) => {
      notices.push(input);
      return { message_id: "msg_notice", status: "pending_review" };
    },
  };
  const supervisor = new AutopilotSupervisor({
    policy: basePolicy(),
    spool,
    mail,
    stateRoot: root,
    helperPath,
    runtimeExecutor: async (invocation) => {
      const escalated = await runHelper("escalate", invocation, "Billing decision.\n");
      assert.equal(escalated.code, 0, escalated.stderr);
      return runHelper("complete", invocation, "Escalated to owner.\n");
    },
  });

  const result = await supervisor.runNextJob();

  assert.equal(result.state, "done");
  assert.equal(notices.length, 1);
  assert.equal(notices[0].ownerEmail, "owner@example.test");
  assert.match(notices[0].subject, /needs attention/);
  assert.match(notices[0].idempotencyKey, /^autopilot-notice-[a-f0-9]{64}$/);
  assert.equal(spool.list("done")[0].outcome.notificationStatus, "pending_review");
});

test("dead-lettering notifies the configured owner", async () => {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-supervisor-"));
  const spool = new JobSpool(path.join(root, "jobs"));
  spool.enqueue({
    messageId: "msg_dead",
    from: "customer@buyer.test",
    source: "listener",
  });
  const notices = [];
  const supervisor = new AutopilotSupervisor({
    policy: basePolicy(),
    spool,
    mail: {
      getMessage: async (id) => ({ id, conversation_id: "conv_current" }),
      getThread: async () => [],
      reply: async () => ({ status: "pending_review" }),
      notifyOwner: async (input) => {
        notices.push(input);
        return { status: "pending_review" };
      },
    },
    stateRoot: root,
    helperPath,
    maxAttempts: 1,
    runtimeExecutor: async () => ({ code: 1, timedOut: false, stdout: "", stderr: "" }),
  });

  const result = await supervisor.runNextJob();

  assert.equal(result.state, "dead");
  assert.equal(notices.length, 1);
  assert.equal(notices[0].ownerEmail, "owner@example.test");
  assert.match(notices[0].subject, /stopped after repeated failures/);
});
