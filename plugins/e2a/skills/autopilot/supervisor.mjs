import { createHash, timingSafeEqual } from "node:crypto";
import { chmodSync, lstatSync, mkdirSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";

import { JobGateway } from "./gateway.mjs";
import { buildRuntimeInvocation, runRuntimeInvocation } from "./runtime.mjs";

const MAX_FORWARD_BYTES = 2 * 1024 * 1024;

function safeEqual(actual, expected) {
  const left = Buffer.from(actual || "", "utf8");
  const right = Buffer.from(expected, "utf8");
  return left.length === right.length && timingSafeEqual(left, right);
}

function extractAddress(headerFrom) {
  if (typeof headerFrom !== "string") return "";
  const value = headerFrom.trim().toLowerCase();
  const angle = value.match(/<([^<>]+)>\s*$/);
  const address = (angle?.[1] || value).trim();
  if (!/^[^\s@<>]+@[^\s@<>]+\.[^\s@<>]+$/.test(address)) return "";
  return address;
}

export function isAuthorizedMessage(policy, message) {
  const address = extractAddress(message?.headerFrom ?? message?.header_from);
  const verifiedDomain = String(
    message?.verifiedDomain ?? message?.verified_domain ?? "",
  )
    .trim()
    .toLowerCase();
  if (!address || !verifiedDomain) return false;
  const addressDomain = address.slice(address.lastIndexOf("@") + 1);
  if (addressDomain !== verifiedDomain) return false;

  if (policy?.inbound?.mode === "addresses") {
    return policy.inbound.addresses.includes(address);
  }
  if (policy?.inbound?.mode === "domains") {
    return policy.inbound.domains.includes(verifiedDomain);
  }
  return false;
}

function respond(response, status, value) {
  const body = `${JSON.stringify(value)}\n`;
  response.writeHead(status, {
    "content-type": "application/json",
    "content-length": Buffer.byteLength(body),
    "cache-control": "no-store",
  });
  response.end(body);
}

export async function createForwardReceiver({
  port,
  token,
  policy,
  spool,
  onJob = () => {},
}) {
  if (!token) throw new Error("Forward receiver token is required.");
  const server = createServer((request, response) => {
    void (async () => {
      if (request.method !== "POST" || request.url !== "/hook") {
        respond(response, 404, { error: "not_found" });
        return;
      }
      const header = request.headers.authorization || "";
      if (!header.startsWith("Bearer ") || !safeEqual(header.slice(7), token)) {
        respond(response, 401, { error: "unauthorized" });
        return;
      }

      let size = 0;
      const chunks = [];
      for await (const chunk of request) {
        size += chunk.length;
        if (size > MAX_FORWARD_BYTES) {
          respond(response, 413, { error: "payload_too_large" });
          return;
        }
        chunks.push(chunk);
      }

      let message;
      try {
        message = JSON.parse(Buffer.concat(chunks).toString("utf8"));
      } catch {
        respond(response, 400, { error: "invalid_json" });
        return;
      }
      const messageId = message?.id ?? message?.message_id;
      if (typeof messageId !== "string" || !messageId.trim()) {
        respond(response, 400, { error: "missing_message_id" });
        return;
      }
      if (!isAuthorizedMessage(policy, message)) {
        respond(response, 202, { accepted: false, action: "human_review" });
        return;
      }

      const result = spool.enqueue({
        messageId,
        from: message.headerFrom ?? message.header_from,
        verifiedDomain: message.verifiedDomain ?? message.verified_domain,
        source: "listener",
      });
      if (result.created) await onJob(result.job);
      respond(response, 200, {
        accepted: true,
        deduplicated: !result.created,
      });
    })().catch(() => {
      if (!response.headersSent) respond(response, 500, { error: "receiver_failed" });
      else response.destroy();
    });
  });
  server.on("clientError", (_error, socket) => {
    socket.end("HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n");
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(port, "127.0.0.1", () => {
      server.off("error", reject);
      resolve();
    });
  });
  const address = server.address();
  return {
    port: address.port,
    close: () => new Promise((resolve) => server.close(resolve)),
  };
}

function shortJobKey(messageId) {
  return createHash("sha256").update(messageId, "utf8").digest("hex").slice(0, 16);
}

export function jobSocketRoot(stateRoot, temporaryRoot = tmpdir()) {
  const digest = createHash("sha256")
    .update(path.resolve(stateRoot), "utf8")
    .digest("hex")
    .slice(0, 16);
  return path.join(temporaryRoot, `e2a-autopilot-${digest}`);
}

function noticeKey(kind, messageId) {
  const digest = createHash("sha256")
    .update(JSON.stringify({ kind, messageId }), "utf8")
    .digest("hex");
  return `autopilot-notice-${digest}`;
}

export class AutopilotSupervisor {
  constructor({
    policy,
    spool,
    mail,
    stateRoot,
    helperPath,
    runtimeExecutor = runRuntimeInvocation,
    maxAttempts = 3,
    baseDelayMs = 1_000,
  }) {
    if (!path.isAbsolute(stateRoot)) throw new Error("Supervisor state root must be absolute.");
    this.policy = policy;
    this.spool = spool;
    this.mail = mail;
    this.stateRoot = stateRoot;
    this.helperPath = helperPath;
    this.runtimeExecutor = runtimeExecutor;
    this.maxAttempts = maxAttempts;
    this.baseDelayMs = baseDelayMs;
    this.socketRoot = jobSocketRoot(stateRoot);
    mkdirSync(this.socketRoot, { recursive: true, mode: 0o700 });
    const socketRootInfo = lstatSync(this.socketRoot);
    if (!socketRootInfo.isDirectory() || socketRootInfo.isSymbolicLink()) {
      throw new Error("Supervisor socket root must be a real directory.");
    }
    chmodSync(this.socketRoot, 0o700);
  }

  async runNextJob() {
    this.spool.promoteReadyRetries();
    const job = this.spool.claimNext();
    if (!job) return { state: "idle" };

    let completion = null;
    let escalation = job.effects?.escalation || null;
    const socketPath = path.join(this.socketRoot, `${shortJobKey(job.messageId)}.sock`);
    const gateway = new JobGateway({
      socketPath,
      job: {
        jobId: job.messageId,
        messageId: job.messageId,
        authorizedFrom: job.from,
      },
      ownerEmail: this.policy.mailbox.ownerEmail,
      ccOwner: this.policy.outbound.ccOwner,
      replyMode: this.policy.task.replyMode,
      replyCheckpoint: job.effects?.reply,
      escalationCheckpoint: job.effects?.escalation,
      mail: this.mail,
      onReplySubmitted: async (value) => {
        this.spool.checkpointEffects(job.messageId, { reply: value });
      },
      onEscalate: async (value) => {
        const notification = await this.mail.notifyOwner({
          ownerEmail: this.policy.mailbox.ownerEmail,
          subject: "Autopilot needs attention",
          text: `Autopilot escalated message ${job.messageId}.\n\nReason: ${value.reason}`,
          idempotencyKey: noticeKey("escalation", job.messageId),
        });
        escalation = {
          ...value,
          notificationStatus: notification.status,
        };
        this.spool.checkpointEffects(job.messageId, {
          escalation: { notificationStatus: notification.status },
        });
      },
      onComplete: async (value) => {
        completion = value;
      },
    });

    try {
      const connection = await gateway.start();
      const invocation = buildRuntimeInvocation(this.policy, {
        jobId: job.messageId,
        messageId: job.messageId,
        socketPath: connection.socketPath,
        token: connection.token,
        helperPath: this.helperPath,
        timeoutMs: 5 * 60 * 1_000,
      });
      const result = await this.runtimeExecutor(invocation);
      if (result.timedOut) {
        return this.failJob(job.messageId, "runtime timed out");
      }
      if (result.code !== 0) {
        return this.failJob(job.messageId, `runtime exited ${result.code}`);
      }
      if (!completion) {
        return this.failJob(job.messageId, "runtime exited without completing the job");
      }
      const done = this.spool.complete(job.messageId, {
        completed: true,
        escalated: Boolean(escalation),
        ...(escalation
          ? { notificationStatus: escalation.notificationStatus }
          : {}),
      });
      return { state: "done", job: done };
    } catch (error) {
      return this.failJob(job.messageId, error?.message || "supervisor job failed");
    } finally {
      await gateway.close();
    }
  }

  async failJob(messageId, reason) {
    const failed = this.spool.fail(messageId, reason, {
      maxAttempts: this.maxAttempts,
      baseDelayMs: this.baseDelayMs,
    });
    if (failed.state === "dead") {
      try {
        await this.mail.notifyOwner({
          ownerEmail: this.policy.mailbox.ownerEmail,
          subject: "Autopilot stopped after repeated failures",
          text: `Autopilot could not complete message ${messageId}.\n\nLast failure: ${reason}`,
          idempotencyKey: noticeKey("dead", messageId),
        });
      } catch {
        // The durable dead-letter remains visible to `autopilot status` even
        // when the out-of-band owner notification cannot be submitted.
      }
    }
    return failed;
  }
}
