import { createHash, timingSafeEqual } from "node:crypto";
import { chmodSync, lstatSync, mkdirSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";

import { JobGateway } from "./gateway.mjs";
import {
  buildRuntimeInvocation,
  resolveOsSandbox,
  runRuntimeInvocation,
  wrapRuntimeInvocation,
} from "./runtime.mjs";

const MAX_FORWARD_BYTES = 2 * 1024 * 1024;

// e2a's terminal released/approved inbound review statuses. A message carrying
// one was held by the e2a review gate and then released by a human reviewer
// (review_approved) or the configured review TTL (review_expired_approved) —
// the release is the authorization decision, so the harness treats it as
// locally authorized. pending_review and every other value stay refused
// (fail-closed).
const RELEASED_REVIEW_STATUSES = new Set(["review_approved", "review_expired_approved"]);

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
    if (policy.inbound.addresses.includes(address)) return true;
  } else if (policy?.inbound?.mode === "domains") {
    if (policy.inbound.domains.includes(verifiedDomain)) return true;
  } else {
    return false;
  }

  // The static policy did not match, but the From/verified-domain alignment
  // check above held: a terminal released review status means an e2a reviewer
  // already authorized this exact message, on both intake paths.
  const reviewStatus = String(message?.reviewStatus ?? message?.review_status ?? "")
    .trim()
    .toLowerCase();
  return RELEASED_REVIEW_STATUSES.has(reviewStatus);
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
    maxAttempts = policy?.limits?.maxAttempts ?? 3,
    baseDelayMs = policy?.limits?.retryBaseDelayMs ?? 1_000,
    installRoot = null,
    secretsPath = null,
    policyPath = null,
    sandboxPlatform = process.platform,
    environment = process.env,
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
    this.installRoot = installRoot;
    this.secretsPath = secretsPath;
    this.policyPath = policyPath;
    this.sandboxPlatform = sandboxPlatform;
    this.environment = environment;
    this.socketRoot = jobSocketRoot(stateRoot);
    mkdirSync(this.socketRoot, { recursive: true, mode: 0o700 });
    const socketRootInfo = lstatSync(this.socketRoot);
    if (!socketRootInfo.isDirectory() || socketRootInfo.isSymbolicLink()) {
      throw new Error("Supervisor socket root must be a real directory.");
    }
    chmodSync(this.socketRoot, 0o700);
  }

  // The runtime must never read the supervisor's own install root: it holds
  // the e2a credential, policy, spool, and logs. The runtime bundle directory
  // (the job helper) stays readable, and the denies are granular rather than
  // a whole-root subpath deny for exactly that reason. Returns null when no
  // OS sandbox tool exists; the acknowledged external-isolation model remains
  // the documented mitigation there.
  planJobSandbox(messageId) {
    if (!this.installRoot) return null;
    const denyPaths = [
      { path: this.stateRoot, subpath: true },
      { path: path.join(this.installRoot, "logs"), subpath: true },
      { path: path.join(this.installRoot, "install.json"), subpath: false },
    ];
    if (this.secretsPath) denyPaths.push({ path: this.secretsPath, subpath: false });
    if (this.policyPath) denyPaths.push({ path: this.policyPath, subpath: false });
    return resolveOsSandbox({
      denyPaths,
      maskPath: this.installRoot,
      allowPaths: [path.dirname(this.helperPath)],
      profilePath: path.join(this.socketRoot, `sandbox-${shortJobKey(messageId)}.sb`),
      platform: this.sandboxPlatform,
      environment: this.environment,
    });
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
        let notification;
        try {
          notification = await this.mail.notifyOwner({
            ownerEmail: this.policy.mailbox.ownerEmail,
            subject: "Autopilot needs attention",
            text: `Autopilot escalated message ${job.messageId}.\n\nReason: ${value.reason}`,
            idempotencyKey: noticeKey("escalation", job.messageId),
          });
        } catch {
          // The escalation itself must not be lost with its notification: the
          // durable checkpoint keeps the job visible in `autopilot status`
          // with notificationStatus "failed" instead of a silent retry loop.
          notification = { status: "failed" };
        }
        escalation = {
          ...value,
          notificationStatus: notification.status,
        };
        this.spool.checkpointEffects(job.messageId, {
          escalation: { reason: value.reason, notificationStatus: notification.status },
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
        timeoutMs: this.policy?.limits?.runtimeTimeoutMs ?? 5 * 60 * 1_000,
      });
      const result = await this.runtimeExecutor(
        wrapRuntimeInvocation(invocation, this.planJobSandbox(job.messageId)),
      );
      if (result.timedOut) {
        return this.failJob(job.messageId, "runtime timed out");
      }
      if (result.code !== 0) {
        return this.failJob(job.messageId, `runtime exited ${result.code}`);
      }
      if (!completion) {
        return this.failJob(job.messageId, "runtime exited without completing the job");
      }
      // A completion after a failed escalation notification is not a clean
      // done: the job stays visible in status with the failed notification
      // outcome instead of looking fully handled.
      const escalationFailed = escalation?.notificationStatus === "failed";
      const done = this.spool.complete(job.messageId, {
        completed: !escalationFailed,
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
