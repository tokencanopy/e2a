import { createHash, randomBytes, timingSafeEqual } from "node:crypto";
import { chmodSync, existsSync, lstatSync, unlinkSync } from "node:fs";
import { createServer } from "node:http";

const MAX_REQUEST_BYTES = 1024 * 1024;

function equalCapability(actual, expected) {
  const left = Buffer.from(actual || "", "utf8");
  const right = Buffer.from(expected, "utf8");
  return left.length === right.length && timingSafeEqual(left, right);
}

function sendJson(response, status, value) {
  const body = `${JSON.stringify(value)}\n`;
  response.writeHead(status, {
    "content-type": "application/json",
    "content-length": Buffer.byteLength(body),
    "cache-control": "no-store",
  });
  response.end(body);
}

function exactKeys(value, allowed) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  return Object.keys(value).every((key) => allowed.includes(key));
}

function boundedText(value, field, max) {
  if (typeof value !== "string" || !value.trim()) {
    throw new RequestError(400, `${field} must be a non-empty string.`);
  }
  if (Buffer.byteLength(value, "utf8") > max) {
    throw new RequestError(413, `${field} is too large.`);
  }
  return value.trim();
}

function mailboxAddress(value) {
  if (typeof value !== "string") return "";
  const trimmed = value.trim().toLowerCase();
  const angle = trimmed.match(/<([^<>]+)>\s*$/);
  const address = (angle?.[1] || trimmed).trim();
  return /^[^\s@<>]+@[^\s@<>]+\.[^\s@<>]+$/.test(address) ? address : "";
}

class RequestError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

async function readJson(request) {
  const contentType = request.headers["content-type"] || "";
  if (!contentType.toLowerCase().startsWith("application/json")) {
    throw new RequestError(415, "Content-Type must be application/json.");
  }
  let size = 0;
  const chunks = [];
  for await (const chunk of request) {
    size += chunk.length;
    if (size > MAX_REQUEST_BYTES) {
      throw new RequestError(413, "Request body is too large.");
    }
    chunks.push(chunk);
  }
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    throw new RequestError(400, "Request body must be valid JSON.");
  }
}

function replyIdempotencyKey(job) {
  const digest = createHash("sha256")
    .update(JSON.stringify({ jobId: job.jobId, messageId: job.messageId }), "utf8")
    .digest("hex");
  return `autopilot-${digest}`;
}

function textDigest(value) {
  return createHash("sha256").update(value, "utf8").digest("hex");
}

function normalizedReplyResult(value = {}) {
  return {
    status: typeof value.status === "string" ? value.status : "accepted",
    ...((value.messageId ?? value.message_id)
      ? { messageId: value.messageId ?? value.message_id }
      : {}),
  };
}

export class JobGateway {
  constructor({
    socketPath,
    job,
    ownerEmail,
    ccOwner,
    replyMode = "submit-for-review",
    replyCheckpoint = null,
    escalationCheckpoint = null,
    mail,
    onReplySubmitted = async () => {},
    onEscalate = async () => {},
    onComplete = async () => {},
  }) {
    if (!socketPath) throw new Error("A gateway socket path is required.");
    if (!job?.messageId || !job?.jobId || !mailboxAddress(job?.authorizedFrom)) {
      throw new Error("A job ID, message ID, and authorized sender are required.");
    }
    if (!mail?.getMessage || !mail?.getThread || !mail?.reply) {
      throw new Error("The gateway requires scoped mail operations.");
    }
    if (ccOwner && !ownerEmail) {
      throw new Error("Owner email is required when owner CC is enabled.");
    }
    this.socketPath = socketPath;
    this.job = Object.freeze({
      jobId: job.jobId,
      messageId: job.messageId,
      authorizedFrom: mailboxAddress(job.authorizedFrom),
    });
    this.ownerEmail = ownerEmail;
    this.ccOwner = Boolean(ccOwner);
    this.replyMode = replyMode;
    this.replyCheckpoint = replyCheckpoint;
    this.escalationCheckpoint = escalationCheckpoint;
    this.mail = mail;
    this.onReplySubmitted = onReplySubmitted;
    this.onEscalate = onEscalate;
    this.onComplete = onComplete;
    this.token = randomBytes(32).toString("base64url");
    this.server = null;
    this.replyInFlight = false;
    this.escalated = Boolean(escalationCheckpoint);
    this.completed = false;
  }

  async start() {
    if (this.server) throw new Error("Job gateway is already running.");
    if (existsSync(this.socketPath)) {
      const existing = lstatSync(this.socketPath);
      if (!existing.isSocket()) {
        throw new Error(`Refusing to replace non-socket path: ${this.socketPath}`);
      }
      unlinkSync(this.socketPath);
    }
    this.server = createServer((request, response) => {
      void this.handle(request, response);
    });
    this.server.on("clientError", (_error, socket) => {
      socket.end("HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n");
    });
    await new Promise((resolve, reject) => {
      this.server.once("error", reject);
      this.server.listen(this.socketPath, () => {
        this.server.off("error", reject);
        resolve();
      });
    });
    chmodSync(this.socketPath, 0o600);
    return { socketPath: this.socketPath, token: this.token };
  }

  authorized(request) {
    const header = request.headers.authorization || "";
    const prefix = "Bearer ";
    if (!header.startsWith(prefix)) return false;
    return equalCapability(header.slice(prefix.length), this.token);
  }

  async handle(request, response) {
    try {
      if (!this.authorized(request)) {
        sendJson(response, 401, { error: "unauthorized" });
        return;
      }

      if (request.method === "GET" && request.url === "/v1/current-message") {
        const message = await this.mail.getMessage(this.job.messageId);
        if (!message || message.id !== this.job.messageId) {
          throw new Error("Scoped mail client returned a different message.");
        }
        sendJson(response, 200, { message });
        return;
      }

      if (request.method === "GET" && request.url === "/v1/current-thread") {
        const messages = await this.mail.getThread(this.job.messageId);
        sendJson(response, 200, { messages });
        return;
      }

      if (request.method === "POST" && request.url === "/v1/reply") {
        if (this.replyMode === "draft-only") {
          throw new RequestError(403, "Reply submission is disabled by the confirmed draft-only policy.");
        }
        const body = await readJson(request);
        if (!exactKeys(body, ["text"])) {
          throw new RequestError(
            400,
            "Reply accepts text only; recipients and the source message are job-scoped.",
          );
        }
        const text = boundedText(body.text, "text", MAX_REQUEST_BYTES);
        const digest = textDigest(text);
        if (this.replyCheckpoint) {
          if (this.replyCheckpoint.textDigest !== digest) {
            throw new RequestError(409, "A reply was already submitted for this job.");
          }
          sendJson(response, 200, { result: this.replyCheckpoint.result });
          return;
        }
        if (this.replyInFlight) {
          throw new RequestError(409, "A reply submission is already in progress for this job.");
        }
        this.replyInFlight = true;
        const cc = this.ccOwner ? [this.ownerEmail] : [];
        const idempotencyKey = replyIdempotencyKey(this.job);
        try {
          const source = await this.mail.getMessage(this.job.messageId);
          const sourceFrom = mailboxAddress(source?.headerFrom ?? source?.header_from);
          const replyTo = source?.replyTo ?? source?.reply_to ?? [];
          if (sourceFrom !== this.job.authorizedFrom) {
            throw new RequestError(409, "The current message sender no longer matches the authorized job sender.");
          }
          if (
            !Array.isArray(replyTo) ||
            replyTo.some((value) => mailboxAddress(value) !== this.job.authorizedFrom)
          ) {
            throw new RequestError(409, "Reply-To does not match the authorized sender; escalate this job instead.");
          }
          const result = normalizedReplyResult(
            await this.mail.reply(this.job.messageId, {
              text,
              cc,
              replyAll: false,
              idempotencyKey,
            }),
          );
          this.replyCheckpoint = {
            idempotencyKey,
            textDigest: digest,
            status: result.status,
            result,
          };
          await this.onReplySubmitted(this.replyCheckpoint);
          sendJson(response, 200, { result });
        } finally {
          this.replyInFlight = false;
        }
        return;
      }

      if (request.method === "POST" && request.url === "/v1/escalate") {
        if (this.escalated) {
          throw new RequestError(409, "This job has already been escalated.");
        }
        const body = await readJson(request);
        if (!exactKeys(body, ["reason"])) {
          throw new RequestError(400, "Escalation accepts a reason only.");
        }
        const reason = boundedText(body.reason, "reason", 4_000);
        this.escalated = true;
        await this.onEscalate({
          jobId: this.job.jobId,
          messageId: this.job.messageId,
          reason,
        });
        sendJson(response, 200, { escalated: true });
        return;
      }

      if (request.method === "POST" && request.url === "/v1/complete") {
        if (this.completed) {
          throw new RequestError(409, "This job has already been completed.");
        }
        const body = await readJson(request);
        if (!exactKeys(body, ["summary"])) {
          throw new RequestError(400, "Completion accepts a summary only.");
        }
        const summary = boundedText(body.summary, "summary", 4_000);
        this.completed = true;
        await this.onComplete({
          jobId: this.job.jobId,
          messageId: this.job.messageId,
          summary,
        });
        sendJson(response, 200, { completed: true });
        return;
      }

      sendJson(response, 404, { error: "not_found" });
    } catch (error) {
      if (response.headersSent) {
        response.destroy();
        return;
      }
      if (error instanceof RequestError) {
        sendJson(response, error.status, { error: error.message });
      } else {
        sendJson(response, 500, { error: "gateway_operation_failed" });
      }
    }
  }

  async close() {
    if (!this.server) return;
    const server = this.server;
    this.server = null;
    await new Promise((resolve) => server.close(resolve));
    if (existsSync(this.socketPath) && lstatSync(this.socketPath).isSocket()) {
      unlinkSync(this.socketPath);
    }
  }
}
