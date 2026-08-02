const DEFAULT_TIMEOUT_MS = 30_000;
const MAX_THREAD_PAGES = 100;
const MAX_API_RESPONSE_BYTES = 10 * 1024 * 1024;
const MAX_RUNTIME_BODY_BYTES = 64 * 1024;

export class E2aMailError extends Error {
  constructor(message, { status, code, retryable = false } = {}) {
    super(message);
    this.name = "E2aMailError";
    this.status = status;
    this.code = code;
    this.retryable = retryable;
  }
}

function requiredText(value, name) {
  if (typeof value !== "string" || !value.trim()) {
    throw new Error(`${name} is required.`);
  }
  return value.trim();
}

function encoded(value) {
  return encodeURIComponent(requiredText(value, "resource identifier"));
}

function boundedString(value, max = MAX_RUNTIME_BODY_BYTES) {
  if (typeof value !== "string") return "";
  return Buffer.byteLength(value, "utf8") <= max
    ? value
    : Buffer.from(value, "utf8").subarray(0, max).toString("utf8");
}

function projectMessage(message) {
  const projected = {};
  for (const field of [
    "id",
    "direction",
    "header_from",
    "verified_domain",
    "to",
    "cc",
    "reply_to",
    "delivered_to",
    "subject",
    "conversation_id",
    "thread_id",
    "read_status",
    "review_status",
    "created_at",
    "flagged",
    "flag_reason",
    "attachments",
  ]) {
    if (message[field] !== undefined) projected[field] = message[field];
  }
  if (message.parsed) {
    const text = boundedString(message.parsed.text);
    projected.parsed = {
      text,
      truncated:
        Boolean(message.parsed.truncated) || text !== String(message.parsed.text || ""),
    };
  }
  if (message.body) {
    projected.body = { text: boundedString(message.body.text) };
  }
  return projected;
}

async function boundedJson(response) {
  const declared = Number(response.headers?.get?.("content-length"));
  if (Number.isFinite(declared) && declared > MAX_API_RESPONSE_BYTES) {
    throw new E2aMailError("e2a response exceeded the local safety limit.", {
      code: "response_too_large",
    });
  }
  if (!response.body?.getReader) return response.json();
  const reader = response.body.getReader();
  const chunks = [];
  let size = 0;
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > MAX_API_RESPONSE_BYTES) {
      await reader.cancel();
      throw new E2aMailError("e2a response exceeded the local safety limit.", {
        code: "response_too_large",
      });
    }
    chunks.push(Buffer.from(value));
  }
  return JSON.parse(Buffer.concat(chunks).toString("utf8"));
}

export class E2aMailClient {
  constructor({
    baseUrl = "https://api.e2a.dev",
    apiKey,
    agentEmail,
    fetchImpl = globalThis.fetch,
    timeoutMs = DEFAULT_TIMEOUT_MS,
  }) {
    if (typeof fetchImpl !== "function") throw new Error("A fetch implementation is required.");
    const parsed = new URL(baseUrl);
    if (!["http:", "https:"].includes(parsed.protocol) || parsed.username || parsed.password) {
      throw new Error("e2a API URL must be an HTTP(S) origin without embedded credentials.");
    }
    this.baseUrl = parsed.origin;
    this.apiKey = requiredText(apiKey, "e2a agent credential");
    this.agentEmail = requiredText(agentEmail, "e2a agent email").toLowerCase();
    this.fetchImpl = fetchImpl;
    this.timeoutMs = timeoutMs;
  }

  async request(resource, { method = "GET", body, idempotencyKey } = {}) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    let response;
    let value;
    try {
      try {
        response = await this.fetchImpl(`${this.baseUrl}${resource}`, {
          method,
          headers: {
            authorization: `Bearer ${this.apiKey}`,
            accept: "application/json",
            ...(body !== undefined ? { "content-type": "application/json" } : {}),
            ...(idempotencyKey ? { "idempotency-key": idempotencyKey } : {}),
          },
          ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
          signal: controller.signal,
        });
      } catch {
        const timedOut = controller.signal.aborted;
        throw new E2aMailError(
          timedOut ? "e2a request timed out." : "e2a request failed before a response.",
          { code: timedOut ? "timeout" : "transport_error", retryable: true },
        );
      }
      try {
        value = await boundedJson(response);
      } catch (error) {
        if (error instanceof E2aMailError) throw error;
        throw new E2aMailError(`e2a returned non-JSON content with status ${response.status}.`, {
          status: response.status,
          code: "invalid_response",
          retryable: response.status >= 500,
        });
      }
    } finally {
      clearTimeout(timer);
    }

    if (!response.ok) {
      const code =
        typeof value?.error?.code === "string" ? value.error.code : "request_failed";
      throw new E2aMailError(`e2a request failed with status ${response.status} (${code}).`, {
        status: response.status,
        code,
        retryable: response.status === 429 || response.status >= 500,
      });
    }
    return value;
  }

  agentResource(suffix) {
    return `/v1/agents/${encoded(this.agentEmail)}${suffix}`;
  }

  async getMessage(messageId) {
    const id = requiredText(messageId, "message ID");
    const message = await this.request(
      this.agentResource(`/messages/${encoded(id)}`),
    );
    if (!message || message.id !== id) {
      throw new E2aMailError("e2a returned a different message than requested.", {
        code: "invalid_response",
      });
    }
    return projectMessage(message);
  }

  async getThread(messageId) {
    const source = await this.getMessage(messageId);
    const conversationId = source.conversation_id ?? source.conversationId;
    if (!conversationId) {
      throw new E2aMailError("The current message has no conversation identifier.", {
        code: "invalid_response",
      });
    }

    const messages = [];
    const seenCursors = new Set();
    let cursor;
    for (let pageNumber = 0; pageNumber < MAX_THREAD_PAGES; pageNumber += 1) {
      const query = new URLSearchParams({
        conversation_id: conversationId,
        direction: "all",
        read_status: "all",
        sort: "asc",
        limit: "100",
        ...(cursor ? { cursor } : {}),
      });
      const page = await this.request(this.agentResource(`/messages?${query}`));
      if (!Array.isArray(page?.items)) {
        throw new E2aMailError("e2a returned an invalid thread page.", {
          code: "invalid_response",
        });
      }
      for (const message of page.items) {
        const itemConversation = message?.conversation_id ?? message?.conversationId;
        if (itemConversation !== conversationId) {
          throw new E2aMailError("e2a returned a message outside the current thread.", {
            code: "invalid_response",
          });
        }
        messages.push(projectMessage(message));
      }
      const next = page.next_cursor ?? page.nextCursor;
      if (!next) return messages;
      if (seenCursors.has(next)) {
        throw new E2aMailError("e2a repeated a thread cursor.", {
          code: "invalid_response",
        });
      }
      seenCursors.add(next);
      cursor = next;
    }
    throw new E2aMailError("e2a thread exceeded the pagination safety limit.", {
      code: "pagination_limit",
    });
  }

  async reply(messageId, { text, cc = [], replyAll = true, idempotencyKey }) {
    const id = requiredText(messageId, "message ID");
    const replyText = requiredText(text, "reply text");
    if (!Array.isArray(cc) || cc.some((address) => typeof address !== "string" || !address)) {
      throw new Error("Reply CC must be an array of email address strings.");
    }
    return this.request(this.agentResource(`/messages/${encoded(id)}/reply`), {
      method: "POST",
      body: {
        text: replyText,
        cc,
        reply_all: Boolean(replyAll),
      },
      idempotencyKey: requiredText(idempotencyKey, "reply idempotency key"),
    });
  }

  async notifyOwner({ ownerEmail, subject, text, idempotencyKey }) {
    const recipient = requiredText(ownerEmail, "owner email");
    return this.request(this.agentResource("/messages"), {
      method: "POST",
      body: {
        to: [recipient],
        subject: requiredText(subject, "notification subject"),
        text: requiredText(text, "notification text"),
      },
      idempotencyKey: requiredText(idempotencyKey, "notification idempotency key"),
    });
  }
}
