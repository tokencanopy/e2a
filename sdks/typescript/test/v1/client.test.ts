import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { E2AClient } from "../../src/v1/client.js";
import {
  E2AError,
  E2ANotFoundError,
  E2AConflictError,
  E2AValidationError,
  E2AConnectionError,
} from "../../src/v1/errors.js";

// These exercise the full hand-written stack — namespaced resources →
// generated `Promise*Api` → bearer auth → retry layer → fetch → envelope
// unwrap → typed-error mapping — by mocking the global `fetch` the generated
// `IsomorphicFetchHttpLibrary` calls. That's deliberately closer to the wire
// than mocking the generated API: header/URL/body encoding all get covered.

const BASE = "http://localhost:9998";

/** A minimal `fetch` Response the generated http library understands:
 *  it reads `.status`, iterates `.headers`, and calls `.text()` / `.blob()`. */
function mockFetch(status: number, jsonBody?: unknown, headers: Record<string, string> = {}) {
  const text = JSON.stringify(jsonBody ?? {});
  return vi.fn(async () => ({
    status,
    headers: new Headers({ "content-type": "application/json", ...headers }),
    text: async () => text,
    blob: async () => new Blob([text]),
  }) as unknown as Response);
}

function lastCall() {
  const mock = globalThis.fetch as ReturnType<typeof vi.fn>;
  const [url, init] = mock.mock.calls[mock.mock.calls.length - 1] as [string, RequestInit];
  return { url, init, headers: init.headers as Record<string, string> };
}

/** A `fetch` mock that pages: looks up the response by the request's `cursor`
 *  query param (absent → the "" key). Records the requested URLs for assertions. */
function pagingFetch(pages: Record<string, { items: unknown[]; next_cursor: string | null }>) {
  const calls: string[] = [];
  const fn = vi.fn(async (url: string) => {
    calls.push(url);
    const cursor = new URL(url).searchParams.get("cursor") ?? "";
    const text = JSON.stringify(pages[cursor]);
    return {
      status: 200,
      headers: new Headers({ "content-type": "application/json" }),
      text: async () => text,
      blob: async () => new Blob([text]),
    } as unknown as Response;
  });
  return { fn, calls };
}

describe("E2AClient", () => {
  const originalFetch = globalThis.fetch;
  let client: E2AClient;
  let savedEnv: Record<string, string | undefined>;

  beforeEach(() => {
    savedEnv = {
      E2A_API_KEY: process.env.E2A_API_KEY,
      E2A_API_URL: process.env.E2A_API_URL,
      E2A_BASE_URL: process.env.E2A_BASE_URL,
      E2A_AGENT_EMAIL: process.env.E2A_AGENT_EMAIL,
    };
    delete process.env.E2A_API_KEY;
    delete process.env.E2A_API_URL;
    delete process.env.E2A_BASE_URL;
    delete process.env.E2A_AGENT_EMAIL;
    client = new E2AClient({ apiKey: "e2a_test", baseUrl: BASE });
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    for (const [k, v] of Object.entries(savedEnv)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  });

  // ── Construction ────────────────────────────────────────────────

  it("requires an apiKey (throws when none is given or in env)", () => {
    expect(() => new E2AClient({ baseUrl: BASE })).toThrow(/apiKey is required/);
  });

  it("falls back to E2A_API_KEY from the environment", () => {
    process.env.E2A_API_KEY = "e2a_env";
    expect(() => new E2AClient({ baseUrl: BASE })).not.toThrow();
  });

  // Base-URL resolution: opts.baseUrl > E2A_API_URL > E2A_BASE_URL (deprecated)
  // > the api.e2a.dev default. Asserted through the wire because baseUrl is
  // private — the URL fetch is called with is the observable contract.
  describe("base URL resolution", () => {
    async function baseUrlOf(opts: { baseUrl?: string } = {}) {
      globalThis.fetch = mockFetch(200, {});
      await new E2AClient({ apiKey: "e2a_test", ...opts }).info();
      return new URL(lastCall().url).origin;
    }

    it("defaults to the API host", async () => {
      expect(await baseUrlOf()).toBe("https://api.e2a.dev");
    });

    it("reads E2A_API_URL", async () => {
      process.env.E2A_API_URL = "https://api.self-host.example";
      expect(await baseUrlOf()).toBe("https://api.self-host.example");
    });

    it("still honours the deprecated E2A_BASE_URL, with a warning", async () => {
      const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
      process.env.E2A_BASE_URL = "https://legacy.example";
      expect(await baseUrlOf()).toBe("https://legacy.example");
      expect(warn).toHaveBeenCalledWith(expect.stringContaining("E2A_BASE_URL is deprecated"));
      warn.mockRestore();
    });

    it("prefers E2A_API_URL over the deprecated name", async () => {
      process.env.E2A_API_URL = "https://canonical.example";
      process.env.E2A_BASE_URL = "https://legacy.example";
      expect(await baseUrlOf()).toBe("https://canonical.example");
    });

    it("lets an explicit baseUrl beat the environment", async () => {
      process.env.E2A_API_URL = "https://api.self-host.example";
      expect(await baseUrlOf({ baseUrl: BASE })).toBe(BASE);
    });
  });

  it("maps a per-request timeout to E2AConnectionError", async () => {
    // fetch hangs until its abort signal fires — i.e. only the per-attempt
    // timeout can end it. Exercises the full client → retry → fetch → typed-error
    // path (Python has the equivalent test_request_timeout_surfaces_as_connection_error).
    globalThis.fetch = vi.fn(
      (_url: string, init?: { signal?: AbortSignal }) =>
        new Promise((_resolve, reject) => {
          const s = init?.signal;
          if (s?.aborted) return reject(s.reason);
          s?.addEventListener("abort", () => reject(s.reason), { once: true });
        }),
    ) as unknown as typeof fetch;
    const c = new E2AClient({ apiKey: "e2a_test", baseUrl: BASE, timeoutMs: 5, maxRetries: 0 });
    await expect(c.agents.get("bot@test.dev")).rejects.toBeInstanceOf(E2AConnectionError);
  });

  it("exposes the namespaced resources", () => {
    expect(client.agents).toBeDefined();
    expect(client.messages).toBeDefined();
    expect(client.conversations).toBeDefined();
    expect(client.domains).toBeDefined();
    expect(client.events).toBeDefined();
    expect(client.webhooks).toBeDefined();
    expect(client.inbound).toBeDefined();
    expect(client.account).toBeDefined();
    expect(client.account.suppressions).toBeDefined();
    expect(client.templates).toBeDefined();
  });

  // ── Auth + transport ────────────────────────────────────────────

  it("sends the bearer Authorization header", async () => {
    globalThis.fetch = mockFetch(200, { id: "ag_1", email: "bot@test.dev" });
    await client.agents.get("bot@test.dev");
    expect(lastCall().headers["Authorization"]).toBe("Bearer e2a_test");
  });

  // ── Agents ──────────────────────────────────────────────────────

  it("agents.get hits GET /v1/agents/{address} (URL-encoded)", async () => {
    globalThis.fetch = mockFetch(200, { id: "ag_1", email: "bot@test.dev" });
    const agent = await client.agents.get("bot@test.dev");
    const { url, init } = lastCall();
    expect(init.method).toBe("GET");
    expect(url).toContain("/v1/agents/");
    expect(url).toContain("bot%40test.dev");
    expect(agent.email).toBe("bot@test.dev");
  });

  it("agents.create POSTs the body to /v1/agents", async () => {
    globalThis.fetch = mockFetch(201, { email: "new@test.dev", domain: "test.dev" });
    const res = await client.agents.create({ email: "new@test.dev" });
    const { url, init } = lastCall();
    expect(init.method).toBe("POST");
    expect(url).toContain("/v1/agents");
    expect(JSON.parse(init.body as string)).toMatchObject({ email: "new@test.dev" });
    expect(res.email).toBe("new@test.dev");
  });

  it("agents.delete auto-sends confirm=DELETE and returns the deletion receipt", async () => {
    globalThis.fetch = mockFetch(200, { deleted: true, email: "bot@test.dev", messages_deleted: 12 });
    const res = await client.agents.delete("bot@test.dev");
    const { url, init } = lastCall();
    expect(init.method).toBe("DELETE");
    expect(url).toContain("/v1/agents/bot%40test.dev");
    expect(url).toContain("confirm=DELETE");
    expect(res.deleted).toBe(true);
    expect(res.email).toBe("bot@test.dev");
    expect(res.messagesDeleted).toBe(12);
  });

  it("agents.delete({ permanent: true }) sends permanent=true; the default omits it", async () => {
    globalThis.fetch = mockFetch(200, { deleted: true, email: "bot@test.dev", messages_deleted: 12 });
    await client.agents.delete("bot@test.dev", { permanent: true });
    const { url, init } = lastCall();
    expect(init.method).toBe("DELETE");
    const params = new URL(url).searchParams;
    expect(params.get("permanent")).toBe("true");
    // The typed call is the confirmation; the SDK supplies the raw-API guard.
    expect(params.get("confirm")).toBe("DELETE");

    // The trash path is reversible, so `permanent` is omitted entirely.
    await client.agents.delete("bot@test.dev");
    expect(new URL(lastCall().url).searchParams.get("permanent")).toBeNull();
  });

  it("domains.delete forwards a stable caller idempotency key", async () => {
    globalThis.fetch = mockFetch(200, {
      deleted: true,
      domain: "mail.example.test",
      sending_teardown: "confirmed",
    });
    const result = await client.domains.delete("mail.example.test", {
      idempotencyKey: "delete-domain-incarnation-1",
    });
    const { url, init, headers } = lastCall();
    expect(init.method).toBe("DELETE");
    expect(url).toContain("/v1/domains/mail.example.test");
    expect(headers["Idempotency-Key"]).toBe("delete-domain-incarnation-1");
    expect(result.sendingTeardown).toBe("confirmed");
  });

  it("agents.list returns an AutoPager over the agents array", async () => {
    globalThis.fetch = mockFetch(200, { items: [{ id: "ag_1", email: "bot@test.dev" }], next_cursor: null });
    const items = await client.agents.list().toArray({ limit: 10 });
    expect(items).toHaveLength(1);
    expect(items[0].email).toBe("bot@test.dev");
  });

  it("agents.list({ deleted: true }) lists the trash", async () => {
    globalThis.fetch = mockFetch(200, { items: [], next_cursor: null });
    await client.agents.list({ deleted: true }).toArray({ limit: 10 });
    expect(new URL(lastCall().url).searchParams.get("deleted")).toBe("true");
  });

  it("agents.restore POSTs to the restore endpoint", async () => {
    globalThis.fetch = mockFetch(200, { email: "bot@test.dev", domain: "test.dev" });
    const restored = await client.agents.restore("bot@test.dev");
    const { url, init } = lastCall();
    expect(init.method).toBe("POST");
    expect(url).toContain("/v1/agents/bot%40test.dev/restore");
    expect(restored.email).toBe("bot@test.dev");
  });

  it("agents exposes exact-agent suppression list/create/delete", async () => {
    const row = {
      agent_email: "sender@example.com",
      address: "recipient@example.net",
      source: "manual",
      created_at: "2026-07-18T00:00:00Z",
    };
    globalThis.fetch = mockFetch(200, { items: [row], next_cursor: null });
    const listed = await client.agents
      .listSuppressions("sender@example.com")
      .toArray({ limit: 10 });
    expect(listed[0].address).toBe("recipient@example.net");

    globalThis.fetch = mockFetch(200, row);
    const created = await client.agents.createSuppression("sender@example.com", {
      address: "recipient@example.net",
    });
    expect(lastCall().init.method).toBe("POST");
    expect(created.agentEmail).toBe("sender@example.com");

    globalThis.fetch = mockFetch(200, { deleted: true, address: "recipient@example.net" });
    const deleted = await client.agents.deleteSuppression(
      "sender@example.com",
      "recipient@example.net",
    );
    expect(lastCall().init.method).toBe("DELETE");
    expect(lastCall().url).toContain("confirm=DELETE");
    expect(deleted.deleted).toBe(true);
  });

  // ── Messages: idempotency + pagination ──────────────────────────

  it("messages.getLifecycle forwards cursor/limit and parses canonical transitions", async () => {
    globalThis.fetch = mockFetch(200, {
      items: [
        {
          id: "mlt_1",
          message_id: "msg_1",
          direction: "outbound",
          recipient: null,
          stage: "accepted",
          outcome: "accepted",
          reason_code: "acceptance.outbound_api",
          retryable: false,
          evidence: { source: "api", nested: { future: true } },
          correlation_ids: { request_id: "req_1", future_id: "future_1" },
          occurred_at: "2026-07-22T00:00:00Z",
          reconstructed: false,
        },
        {
          id: "mlt_recon_2",
          message_id: "msg_1",
          direction: "outbound",
          stage: "delivery",
          outcome: "delivered",
          reason_code: "delivery.recipient_server_accepted",
          retryable: false,
          evidence: { source: "recipient_status" },
          correlation_ids: {},
          occurred_at: "2026-07-22T01:00:00Z",
          reconstructed: true,
        },
      ],
      next_cursor: "cur_2",
    });

    const page = await client.messages.getLifecycle("bot@test.dev", "msg_1", {
      cursor: "cur_1",
      limit: 2,
    });

    const { url, init } = lastCall();
    const parsedUrl = new URL(url);
    expect(init.method).toBe("GET");
    expect(parsedUrl.pathname).toContain("/v1/agents/bot%40test.dev/messages/msg_1/lifecycle");
    expect(parsedUrl.searchParams.get("cursor")).toBe("cur_1");
    expect(parsedUrl.searchParams.get("limit")).toBe("2");
    expect(page.nextCursor).toBe("cur_2");
    expect(page.items[0].recipient).toBeNull();
    expect(page.items[0].evidence.nested).toEqual({ future: true });
    expect(page.items[0].correlationIds.future_id).toBe("future_1");
    expect(page.items[1].recipient).toBeUndefined();
    expect(page.items[1].reconstructed).toBe(true);
    expect(page.items[1].stage).toBe("delivery");
  });

  it("messages.send mints an Idempotency-Key for the POST", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_s1", status: "sent" });
    await client.messages.send("bot@test.dev", { to: ["a@x.com"], subject: "Hi", text: "Hello" } as never);
    const { url, init, headers } = lastCall();
    expect(init.method).toBe("POST");
    expect(url).toContain("/v1/agents/bot%40test.dev/messages");
    expect(headers["Idempotency-Key"]).toBeTruthy();
  });

  it("messages.send serializes the managed unsubscribe literal", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_managed", status: "sent" });
    await client.messages.send("sender@example.com", {
      to: ["recipient@example.net"],
      subject: "Update",
      text: "Hello",
      unsubscribe: { mode: "managed" },
    });
    expect(JSON.parse(lastCall().init.body as string).unsubscribe).toEqual({ mode: "managed" });
  });

  it("messages.send serializes sendAt and parses the scheduled result", async () => {
    const sendAt = new Date("2026-08-01T16:00:00.000Z");
    globalThis.fetch = mockFetch(202, {
      message_id: "msg_scheduled",
      status: "scheduled",
      scheduled_at: sendAt.toISOString(),
    });

    const result = await client.messages.send("sender@example.com", {
      to: ["recipient@example.net"],
      subject: "Scheduled update",
      text: "Hello later",
      sendAt,
    });

    expect(JSON.parse(lastCall().init.body as string).send_at).toBe(sendAt.toISOString());
    expect(result.status).toBe("scheduled");
    expect(result.scheduledAt).toEqual(sendAt);
  });

  it("messages.send serializes a single-address replyTo as a scalar string", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_rt", status: "sent" });
    await client.messages.send("sender@example.com", {
      to: ["recipient@example.net"],
      subject: "Hi",
      text: "Hello",
      replyTo: "Support <support@acme.com>",
    });
    expect(JSON.parse(lastCall().init.body as string).reply_to).toBe("Support <support@acme.com>");
  });

  it("messages.send serializes an array replyTo as a JSON address-list", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_rt", status: "sent" });
    // The array form also exercises the type: replyTo accepts string | string[]
    // via the generated oneOf union, so this would fail tsc if it didn't.
    await client.messages.send("sender@example.com", {
      to: ["recipient@example.net"],
      subject: "Hi",
      text: "Hello",
      replyTo: ["support@acme.com", "owner@acme.com"],
    });
    expect(JSON.parse(lastCall().init.body as string).reply_to).toEqual([
      "support@acme.com",
      "owner@acme.com",
    ]);
  });

  it("messages.reply serializes a single-address replyTo as a scalar string", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_rt", status: "sent" });
    await client.messages.reply("sender@example.com", "msg_1", {
      text: "Thanks",
      replyTo: "Support <support@acme.com>",
    });
    expect(JSON.parse(lastCall().init.body as string).reply_to).toBe("Support <support@acme.com>");
  });

  it("messages.reply serializes an array replyTo as a JSON address-list", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_rt", status: "sent" });
    await client.messages.reply("sender@example.com", "msg_1", {
      text: "Thanks",
      replyTo: ["support@acme.com", "owner@acme.com"],
    });
    expect(JSON.parse(lastCall().init.body as string).reply_to).toEqual([
      "support@acme.com",
      "owner@acme.com",
    ]);
  });

  it("messages.forward serializes a single-address replyTo as a scalar string", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_rt", status: "sent" });
    await client.messages.forward("sender@example.com", "msg_1", {
      to: ["recipient@example.net"],
      text: "FYI",
      replyTo: "Support <support@acme.com>",
    });
    expect(JSON.parse(lastCall().init.body as string).reply_to).toBe("Support <support@acme.com>");
  });

  it("messages.forward serializes an array replyTo as a JSON address-list", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_rt", status: "sent" });
    await client.messages.forward("sender@example.com", "msg_1", {
      to: ["recipient@example.net"],
      text: "FYI",
      replyTo: ["support@acme.com", "owner@acme.com"],
    });
    expect(JSON.parse(lastCall().init.body as string).reply_to).toEqual([
      "support@acme.com",
      "owner@acme.com",
    ]);
  });

  it("messages.reply serializes sendAt and parses the scheduled result", async () => {
    const sendAt = new Date("2026-08-01T16:00:00.000Z");
    globalThis.fetch = mockFetch(202, {
      message_id: "msg_scheduled_reply",
      status: "scheduled",
      scheduled_at: sendAt.toISOString(),
    });

    const result = await client.messages.reply("bot@test.dev", "msg_1", {
      text: "Scheduled reply",
      sendAt,
    });

    expect(JSON.parse(lastCall().init.body as string).send_at).toBe(sendAt.toISOString());
    expect(result.status).toBe("scheduled");
    expect(result.scheduledAt).toEqual(sendAt);
  });

  it("messages.reply serializes the beta quoteHistory flag", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_quoted_reply", status: "sent" });

    await client.messages.reply("bot@test.dev", "msg_1", {
      text: "Quoted reply",
      quoteHistory: true,
    });

    expect(JSON.parse(lastCall().init.body as string).quote_history).toBe(true);
  });

  it("messages.reply omits quote_history when quoteHistory is not set", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_plain_reply", status: "sent" });

    await client.messages.reply("bot@test.dev", "msg_1", { text: "Plain reply" });

    expect(JSON.parse(lastCall().init.body as string)).not.toHaveProperty("quote_history");
  });

  it("messages.forward serializes sendAt and parses the scheduled result", async () => {
    const sendAt = new Date("2026-08-01T16:00:00.000Z");
    globalThis.fetch = mockFetch(202, {
      message_id: "msg_scheduled_forward",
      status: "scheduled",
      scheduled_at: sendAt.toISOString(),
    });

    const result = await client.messages.forward("bot@test.dev", "msg_1", {
      to: ["recipient@example.net"],
      text: "Scheduled forward",
      sendAt,
    });

    expect(JSON.parse(lastCall().init.body as string).send_at).toBe(sendAt.toISOString());
    expect(result.status).toBe("scheduled");
    expect(result.scheduledAt).toEqual(sendAt);
  });

  it("messages.send uses a caller-supplied idempotency key", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_s2", status: "sent" });
    await client.messages.send(
      "bot@test.dev",
      { to: ["a@x.com"], subject: "Hi", text: "Hello" } as never,
      { idempotencyKey: "caller-key-123" },
    );
    expect(lastCall().headers["Idempotency-Key"]).toBe("caller-key-123");
  });

  it("messages.send/reply/forward forward wait=sent as the bounded-wait query param", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_w1", status: "sent" });
    await client.messages.send(
      "bot@test.dev",
      { to: ["a@x.com"], subject: "Hi", text: "Hello" } as never,
      { wait: "sent" },
    );
    expect(new URL(lastCall().url).searchParams.get("wait")).toBe("sent");

    await client.messages.reply("bot@test.dev", "msg_1", { text: "Re" } as never, { wait: "sent" });
    expect(new URL(lastCall().url).searchParams.get("wait")).toBe("sent");

    await client.messages.forward("bot@test.dev", "msg_1", { to: ["b@x.com"] } as never, { wait: "sent" });
    expect(new URL(lastCall().url).searchParams.get("wait")).toBe("sent");
  });

  it("messages.send omits the wait param by default", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_w2", status: "accepted" });
    await client.messages.send("bot@test.dev", { to: ["a@x.com"], subject: "Hi", text: "Hello" } as never);
    expect(new URL(lastCall().url).searchParams.get("wait")).toBeNull();
  });

  it("messages.list threads next_cursor across pages", async () => {
    const calls: string[] = [];
    globalThis.fetch = vi.fn(async (url: string) => {
      calls.push(url);
      const cursor = new URL(url).searchParams.get("cursor");
      const text = cursor
        ? JSON.stringify({ items: [{ id: "msg_2" }], next_cursor: null })
        : JSON.stringify({ items: [{ id: "msg_1" }], next_cursor: "cur_2" });
      return {
        status: 200,
        headers: new Headers({ "content-type": "application/json" }),
        text: async () => text,
        blob: async () => new Blob([text]),
      } as unknown as Response;
    }) as unknown as typeof fetch;

    const items = await client.messages.list("bot@test.dev").toArray({ limit: 50 });
    expect(items.map((m) => m.id)).toEqual(["msg_1", "msg_2"]);
    expect(calls).toHaveLength(2);
    expect(calls[1]).toContain("cursor=cur_2");
  });

  it("messages.list exposes from_ and serializes it as the wire from query", async () => {
    globalThis.fetch = mockFetch(200, { items: [], next_cursor: null });

    await client.messages.list("bot@test.dev", { from_: "alice@example.com" }).page();

    const url = new URL(lastCall().url);
    expect(url.searchParams.get("from")).toBe("alice@example.com");
    expect(url.searchParams.has("from_")).toBe(false);
  });

  it("sends the structured filter after all existing list parameters", async () => {
    globalThis.fetch = mockFetch(200, { items: [], next_cursor: null });
    const messages = client.messages as unknown as {
      api: { listMessages: (...args: unknown[]) => Promise<unknown> };
    };
    const listMessagesSpy = vi.spyOn(messages.api, "listMessages");

    await client.messages.list("bot@test.dev", {
      direction: "all",
      deleted: true,
      filter: "label:urgent",
    }).toArray({ limit: 10 });

    const url = new URL(lastCall().url);
    expect(url.searchParams.get("filter")).toBe("label:urgent");
    expect(url.searchParams.has("q")).toBe(false);
    expect(url.searchParams.get("deleted")).toBe("true");
    expect(listMessagesSpy).toHaveBeenCalledWith(
      "bot@test.dev",
      "all",
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      true,
      "label:urgent",
    );
  });

  it("messages.list({ deleted: true }) lists the trash", async () => {
    globalThis.fetch = mockFetch(200, { items: [], next_cursor: null });
    await client.messages.list("bot@test.dev", { deleted: true }).toArray({ limit: 10 });
    expect(new URL(lastCall().url).searchParams.get("deleted")).toBe("true");
  });

  it("messages.delete soft-deletes by default and returns the deletion receipt", async () => {
    globalThis.fetch = mockFetch(200, { deleted: true, id: "msg_1" });
    const res = await client.messages.delete("bot@test.dev", "msg_1");
    const { url, init } = lastCall();
    expect(init.method).toBe("DELETE");
    expect(url).toContain("/v1/agents/bot%40test.dev/messages/msg_1");
    // The soft delete is reversible, so `permanent` is omitted entirely — only
    // the permanent path is gated on it server-side.
    expect(new URL(url).searchParams.get("permanent")).toBeNull();
    expect(res.deleted).toBe(true);
    expect(res.id).toBe("msg_1");
  });

  it("messages.delete({ permanent: true }) auto-sends confirm=DELETE", async () => {
    globalThis.fetch = mockFetch(200, { deleted: true, id: "msg_1" });
    const res = await client.messages.delete("bot@test.dev", "msg_1", { permanent: true });
    const { url, init } = lastCall();
    expect(init.method).toBe("DELETE");
    const params = new URL(url).searchParams;
    expect(params.get("permanent")).toBe("true");
    // The typed call is the confirmation; the SDK supplies the raw-API guard.
    expect(params.get("confirm")).toBe("DELETE");
    expect(res.deleted).toBe(true);
  });

  it("messages.restore POSTs to the restore endpoint", async () => {
    globalThis.fetch = mockFetch(200, {
      id: "msg_1", conversation_id: "conv_1", created_at: "2026-01-01T00:00:00Z",
      delivered_to: "bot@test.dev", direction: "inbound", from: "a@x.dev",
      raw_message: "", read_status: "unread", review_status: "none", subject: "hi",
    });
    const restored = await client.messages.restore("bot@test.dev", "msg_1");
    const { url, init } = lastCall();
    expect(init.method).toBe("POST");
    expect(url).toContain("/v1/agents/bot%40test.dev/messages/msg_1/restore");
    expect(restored.id).toBe("msg_1");
  });

  it("messages.getAttachment hits GET …/attachments/{index} and maps the view", async () => {
    globalThis.fetch = mockFetch(200, {
      index: 0,
      filename: "report.pdf",
      content_type: "application/pdf",
      size_bytes: 14,
      download_url: "https://api.test/d?token=tok",
      expires_at: "2026-06-20T10:15:00Z",
    });
    const att = await client.messages.getAttachment("bot@test.dev", "msg_1", 0, { inline: true });
    const { url, init } = lastCall();
    expect(init.method).toBe("GET");
    expect(url).toContain("/messages/msg_1/attachments/0");
    expect(url).toContain("inline=true");
    expect(att.downloadUrl).toBe("https://api.test/d?token=tok");
    expect(att.sizeBytes).toBe(14);
  });

  // ── webhooks.fetchMessage: email.received is metadata-only ──────
  it("webhooks.fetchMessage resolves (delivered_to, message_id) → GET the full message", async () => {
    // Held outbound drafts have no canonical MIME until approval. The field is
    // required but explicitly null in that lifecycle state.
    globalThis.fetch = mockFetch(200, { id: "msg_9", subject: "Hi", raw_message: null });
    const event = {
      id: "evt_1",
      type: "email.received",
      schema_version: "1",
      created_at: "2026-06-21T10:15:00Z",
      data: { message_id: "msg_9", delivered_to: "bot@test.dev" },
    };
    const msg = await client.webhooks.fetchMessage(event);
    const { url, init } = lastCall();
    expect(init.method).toBe("GET");
    // the fetch keys carried by the metadata-only event drive the URL
    expect(url).toContain("/messages/msg_9");
    expect(url).toContain("bot%40test.dev");
    expect(msg.id).toBe("msg_9");
    expect(msg.rawMessage).toBeNull();
  });

  it("webhooks.fetchMessage rejects a non-received event or missing fetch keys", async () => {
    expect(() =>
      client.webhooks.fetchMessage({
        type: "email.bounced", id: "evt_1", schema_version: "1",
        created_at: "2026-06-21T10:15:00Z", data: { message_id: "m", delivered_to: "r" },
      }),
    ).toThrow(/email\.received/);
    expect(() =>
      client.webhooks.fetchMessage({
        type: "email.received", id: "evt_1", schema_version: "1",
        created_at: "2026-06-21T10:15:00Z", data: { message_id: "m" },
      }),
    ).toThrow(/delivered_to/);
  });

  // ── webhooks.create: server-deduped one-time-secret mint ────────
  it("webhooks.create mints an Idempotency-Key for the POST", async () => {
    globalThis.fetch = mockFetch(201, { id: "wh_1", signing_secret: "whsec_x" });
    await client.webhooks.create({ url: "https://x.com/h", events: ["email.received"] } as never);
    const { url, init, headers } = lastCall();
    expect(init.method).toBe("POST");
    expect(url).toContain("/v1/webhooks");
    expect(headers["Idempotency-Key"]).toBeTruthy();
  });

  it("webhooks.create uses a caller-supplied idempotency key", async () => {
    globalThis.fetch = mockFetch(201, { id: "wh_1", signing_secret: "whsec_x" });
    await client.webhooks.create(
      { url: "https://x.com/h", events: ["email.received"] } as never,
      { idempotencyKey: "wh-key-123" },
    );
    expect(lastCall().headers["Idempotency-Key"]).toBe("wh-key-123");
  });

  // ── Reviews: account-scoped, id-addressed (no inbox email) ──────
  it("reviews.approve hits POST /v1/reviews/{id}/approve (no inbox email) + mints Idempotency-Key", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_r1", status: "sent" });
    await client.reviews.approve("msg_r1");
    const { url, init, headers } = lastCall();
    expect(init.method).toBe("POST");
    expect(url).toContain("/v1/reviews/msg_r1/approve");
    expect(url).not.toContain("/agents/");
    expect(headers["Idempotency-Key"]).toBeTruthy();
  });

  it("reviews.reject hits POST /v1/reviews/{id}/reject", async () => {
    globalThis.fetch = mockFetch(200, { message_id: "msg_r2", status: "rejected" });
    await client.reviews.reject("msg_r2", { reason: "spam" } as never);
    const { url, init } = lastCall();
    expect(init.method).toBe("POST");
    expect(url).toContain("/v1/reviews/msg_r2/reject");
  });

  it("reviews.list reads GET /v1/reviews (single page)", async () => {
    globalThis.fetch = mockFetch(200, {
      items: [{ id: "msg_r1", agent: "bot@test.dev", direction: "outbound" }],
      next_cursor: null,
    });
    const items = await client.reviews.list().toArray({ limit: 50 });
    expect(items.map((r) => r.id)).toEqual(["msg_r1"]);
    expect(lastCall().url).toContain("/v1/reviews");
  });

  // ── Pagination: cursor-walking endpoints ────────────────────────
  // conversations/events/suppressions take a `cursor` query param; the pager
  // must replay next_cursor until null, threading the cursor each follow-up.
  // (messages is covered above.)

  it("conversations.list threads next_cursor across pages", async () => {
    const { fn, calls } = pagingFetch({
      "": { items: [{ id: "conv_1" }], next_cursor: "cur_2" },
      cur_2: { items: [{ id: "conv_2" }], next_cursor: null },
    });
    globalThis.fetch = fn as unknown as typeof fetch;
    const items = await client.conversations.list("bot@test.dev").toArray({ limit: 50 });
    expect(items.map((c) => c.id)).toEqual(["conv_1", "conv_2"]);
    expect(calls).toHaveLength(2);
    expect(calls[1]).toContain("cursor=cur_2");
  });

  it("events.list threads next_cursor across pages", async () => {
    const { fn, calls } = pagingFetch({
      "": { items: [{ id: "evt_1" }], next_cursor: "cur_2" },
      cur_2: { items: [{ id: "evt_2" }], next_cursor: null },
    });
    globalThis.fetch = fn as unknown as typeof fetch;
    const items = await client.events.list().toArray({ limit: 50 });
    expect(items.map((e) => e.id)).toEqual(["evt_1", "evt_2"]);
    expect(calls).toHaveLength(2);
    expect(calls[1]).toContain("cursor=cur_2");
  });

  it("account.suppressions.list threads next_cursor across pages", async () => {
    const { fn, calls } = pagingFetch({
      "": { items: [{ address: "a@x.com" }], next_cursor: "cur_2" },
      cur_2: { items: [{ address: "b@x.com" }], next_cursor: null },
    });
    globalThis.fetch = fn as unknown as typeof fetch;
    const items = await client.account.suppressions.list().toArray({ limit: 50 });
    expect(items.map((s) => s.address)).toEqual(["a@x.com", "b@x.com"]);
    expect(calls).toHaveLength(2);
    expect(calls[1]).toContain("cursor=cur_2");
  });

  // ── Pagination: keyset-cursor list endpoints ────────────────────
  // agents/domains/webhooks/templates/starter-templates/api-keys are all
  // keyset-paginated on (created_at, id) now — the AutoPager must thread
  // next_cursor to completion, exactly like messages/events/suppressions. This
  // locks in the consistent-pagination contract (no more silent single-page cap).

  it.each([
    ["agents", () => client.agents.list(), [{ email: "a@x.dev" }, { email: "b@x.dev" }], (r: { email: string }) => r.email],
    ["domains", () => client.domains.list(), [{ domain: "a.dev" }, { domain: "b.dev" }], (r: { domain: string }) => r.domain],
    ["webhooks", () => client.webhooks.list(), [{ id: "wh_1" }, { id: "wh_2" }], (r: { id: string }) => r.id],
    ["contacts", () => client.contacts.list(), [{ address: "a@x.vc" }, { address: "b@x.vc" }], (r: { address: string }) => r.address],
    ["templates", () => client.templates.list(), [{ id: "t_1", name: "A" }, { id: "t_2", name: "B" }], (r: { id: string }) => r.id],
    ["templates.listStarters", () => client.templates.listStarters(), [{ alias: "welcome" }, { alias: "receipt" }], (r: { alias: string }) => r.alias],
    ["account.apiKeys", () => client.account.apiKeys.list(), [{ id: "key_1" }, { id: "key_2" }], (r: { id: string }) => r.id],
  ] as const)("%s.list threads next_cursor across pages", async (_name, lister, rows, keyOf) => {
    const { fn, calls } = pagingFetch({
      "": { items: [rows[0]], next_cursor: "cur_2" },
      cur_2: { items: [rows[1]], next_cursor: null },
    });
    globalThis.fetch = fn as unknown as typeof fetch;
    const items = await (lister() as { toArray: (o: { limit: number }) => Promise<unknown[]> }).toArray({ limit: 50 });
    expect(items.map((it) => keyOf(it as never))).toEqual([keyOf(rows[0] as never), keyOf(rows[1] as never)]);
    expect(calls).toHaveLength(2);
    expect(calls[1]).toContain("cursor=cur_2");
  });

  // ── Contacts (beta) ─────────────────────────────────────────────
  // Contacts are account-level identity. The address is the resource key, so
  // these pin that it is URL-encoded on the wire and that the SDK supplies the
  // ?confirm=DELETE guard the raw API requires.

  it("contacts.list exposes both creation-time filters", async () => {
    globalThis.fetch = mockFetch(200, { items: [], next_cursor: null });
    const createdAfter = new Date("2026-07-01T00:00:00Z");
    const createdBefore = new Date("2026-07-31T00:00:00Z");
    await client.contacts.list({ createdAfter, createdBefore }).toArray({ limit: 50 });
    const { url } = lastCall();
    expect(url).toContain("created_after=2026-07-01T00%3A00%3A00.000Z");
    expect(url).toContain("created_before=2026-07-31T00%3A00%3A00.000Z");
  });

  it("contacts.get URL-encodes the address in the path", async () => {
    globalThis.fetch = mockFetch(200, {
      address: "partner@fund.vc", display_name: "A. Partner",
      metadata: { fund: "Example Capital" }, source: "import",
      import_batch_id: "imp_1",
      created_at: "2026-07-01T00:00:00Z", updated_at: "2026-07-01T00:00:00Z",
    });
    const c = await client.contacts.get("partner@fund.vc");
    const { url, init } = lastCall();
    expect(init.method).toBe("GET");
    // The @ must not reach the path raw — this is the encoded-routing contract.
    expect(url).toContain("/v1/contacts/partner%40fund.vc");
    expect(c.displayName).toBe("A. Partner");
    expect(c.importBatchId).toBe("imp_1");
  });

  it("contacts exposes ETags and sends If-Match on conditional writes", async () => {
    const wire = {
      address: "partner@fund.vc", display_name: "A. Partner", metadata: {},
      source: "manual", created_at: "2026-07-01T00:00:00Z",
      updated_at: "2026-07-01T00:00:00Z",
    };
    globalThis.fetch = mockFetch(200, wire, { etag: '"contact-v1"' });
    const versioned = await client.contacts.getWithETag("partner@fund.vc");
    expect(versioned.data.address).toBe("partner@fund.vc");
    expect(versioned.etag).toBe('"contact-v1"');

    globalThis.fetch = mockFetch(200, { ...wire, display_name: "Renamed" });
    await client.contacts.update(
      "partner@fund.vc",
      { displayName: "Renamed" },
      { ifMatch: versioned.etag },
    );
    expect(lastCall().headers["If-Match"]).toBe('"contact-v1"');
  });

  it("outreach exposes ETags and sends If-Match on conditional writes", async () => {
    const wire = {
      agent_email: "raise@example.com", address: "partner@fund.vc", stage: "touch1",
      metadata: {}, replied: false, suppressed: false, outbound_count: 0,
      inbound_count: 0, contact: { address: "partner@fund.vc", display_name: "", metadata: {} },
      created_at: "2026-07-01T00:00:00Z", updated_at: "2026-07-01T00:00:00Z",
    };
    globalThis.fetch = mockFetch(200, wire, { etag: '"outreach-v1"' });
    const versioned = await client.contacts.getOutreachWithETag("raise@example.com", "partner@fund.vc");
    expect(versioned.etag).toBe('"outreach-v1"');

    globalThis.fetch = mockFetch(200, { ...wire, stage: "touch2" });
    await client.contacts.setOutreach(
      "raise@example.com", "partner@fund.vc", { stage: "touch2" },
      { ifMatch: versioned.etag },
    );
    expect(lastCall().headers["If-Match"]).toBe('"outreach-v1"');
  });

  it("contacts.create POSTs the body and returns the canonicalized address", async () => {
    globalThis.fetch = mockFetch(201, {
      address: "partner@fund.vc", display_name: "A. Partner", metadata: {},
      source: "manual",
      created_at: "2026-07-01T00:00:00Z", updated_at: "2026-07-01T00:00:00Z",
    });
    const c = await client.contacts.create(
      { address: "A. Partner <Partner@Fund.VC>", displayName: "A. Partner" },
      { idempotencyKey: "contact:partner" },
    );
    const { url, init } = lastCall();
    expect(init.method).toBe("POST");
    expect(new Headers(init.headers).get("Idempotency-Key")).toBe("contact:partner");
    expect(url).toContain("/v1/contacts");
    expect(JSON.parse(init.body as string)).toEqual({
      address: "A. Partner <Partner@Fund.VC>", display_name: "A. Partner",
    });
    expect(c.address).toBe("partner@fund.vc");
  });

  it("contacts.update PATCHes only the fields given, so metadata survives", async () => {
    globalThis.fetch = mockFetch(200, {
      address: "partner@fund.vc", display_name: "Renamed",
      metadata: { fund: "Example Capital" }, source: "import",
      created_at: "2026-07-01T00:00:00Z", updated_at: "2026-07-02T00:00:00Z",
    });
    const c = await client.contacts.update("partner@fund.vc", { displayName: "Renamed" });
    const { url, init } = lastCall();
    expect(init.method).toBe("PATCH");
    expect(url).toContain("/v1/contacts/partner%40fund.vc");
    // Only the caller's field reaches the wire — an omitted metadata key is
    // what makes the server leave the stored value alone.
    expect(JSON.parse(init.body as string)).toEqual({ display_name: "Renamed" });
    expect(c.metadata).toEqual({ fund: "Example Capital" });
  });

  it("contacts.delete supplies confirm=DELETE so callers are not burdened", async () => {
    globalThis.fetch = mockFetch(200, { deleted: true, address: "partner@fund.vc" });
    const res = await client.contacts.delete("partner@fund.vc");
    const { url, init } = lastCall();
    expect(init.method).toBe("DELETE");
    expect(url).toContain("confirm=DELETE");
    expect(res.deleted).toBe(true);
  });

  it("contacts.import returns per-row results including suppressed marking", async () => {
    globalThis.fetch = mockFetch(200, {
      batch_id: "imp_9", created: 1, updated: 0, skipped: 0, failed: 1,
      results: [
        { index: 0, address: "ok@fund.vc", status: "created", suppressed: true },
        { index: 1, status: "failed", code: "invalid_recipient", message: "bad" },
      ],
    });
    const res = await client.contacts.import({
      contacts: [{ address: "ok@fund.vc" }, { address: "nope" }],
    });
    const { url, init, headers } = lastCall();
    expect(init.method).toBe("POST");
    expect(url).toContain("/v1/contacts/import");
    expect(headers["Idempotency-Key"]).toBeTruthy();
    expect(res.batchId).toBe("imp_9");
    // A suppressed row is still reported as created — marked, never dropped.
    expect(res.results[0].status).toBe("created");
    expect(res.results[0].suppressed).toBe(true);
    expect(res.results[1].code).toBe("invalid_recipient");
  });

  it("contacts.import preserves a caller key for restart-safe replay", async () => {
    globalThis.fetch = mockFetch(200, {
      batch_id: "imp_replay", created: 0, updated: 0, skipped: 1, failed: 0,
      results: [{ index: 0, address: "ok@fund.vc", status: "skipped" }],
    });
    await client.contacts.import(
      { contacts: [{ address: "ok@fund.vc" }] },
      { idempotencyKey: "contacts:upload:sha256" },
    );
    expect(lastCall().headers["Idempotency-Key"]).toBe("contacts:upload:sha256");
  });

  it("contacts.deleteImport reverses a batch with the confirm guard", async () => {
    globalThis.fetch = mockFetch(200, {
      deleted: true, batch_id: "imp_9", contacts_deleted: 2, contacts_retained: 0,
    });
    const res = await client.contacts.deleteImport("imp_9");
    const { url, init } = lastCall();
    expect(init.method).toBe("DELETE");
    expect(url).toContain("/v1/contacts/imports/imp_9");
    expect(url).toContain("confirm=DELETE");
    expect(res.contactsDeleted).toBe(2);
  });

  it("contacts.outreach builds the follow-up sweep query", async () => {
    globalThis.fetch = mockFetch(200, { items: [], next_cursor: null });
    // AutoPager is an async iterable; draining it issues the request.
    for await (const _ of client.contacts.outreach("raise@example.com", {
      replied: false,
      nextActionBefore: new Date("2026-07-29T09:00:00Z"),
      lastOutboundBefore: new Date("2026-07-24T09:00:00Z"),
    })) {
      // no rows in this fixture
    }
    const { url } = lastCall();
    expect(url).toContain("/v1/agents/raise%40example.com/contacts");
    expect(url).toContain("replied=false");
    // last_outbound_before is what makes a lost state-write safe — without it a
    // failed update can send the same person twice.
    expect(url).toContain("last_outbound_before=");
    expect(url).toContain("next_action_before=");
  });

  it("contacts.setOutreach PUTs only the fields given", async () => {
    globalThis.fetch = mockFetch(200, {
      agent_email: "raise@example.com", address: "partner@fund.vc",
      stage: "touch2", next_action_at: null, metadata: {},
      replied: false, suppressed: false,
      first_outbound_at: null, last_outbound_at: null, last_inbound_at: null,
      outbound_count: 0, inbound_count: 0,
      contact: { address: "partner@fund.vc", display_name: "", metadata: {} },
      created_at: "2026-07-01T00:00:00Z", updated_at: "2026-07-01T00:00:00Z",
    });
    await client.contacts.setOutreach("raise@example.com", "partner@fund.vc", { stage: "touch2" });
    const { url, init } = lastCall();
    expect(init.method).toBe("PUT");
    expect(url).toContain("/v1/agents/raise%40example.com/contacts/partner%40fund.vc");
    // Only the caller's field reaches the wire — omitting next_action_at is
    // what tells the server to leave the schedule alone.
    expect(JSON.parse(init.body as string)).toEqual({ stage: "touch2" });
  });

  it("contacts.deleteOutreach un-enrols with the confirm guard", async () => {
    globalThis.fetch = mockFetch(200, { deleted: true, address: "partner@fund.vc" });
    const res = await client.contacts.deleteOutreach("raise@example.com", "partner@fund.vc");
    const { url, init } = lastCall();
    expect(init.method).toBe("DELETE");
    expect(url).toContain("confirm=DELETE");
    expect(res.deleted).toBe(true);
  });

  // ── Templates (beta) ────────────────────────────────────────────
  // camelCase model fields ↔ snake_case wire (the generated serializer maps
  // them), plus the two starter-catalog reads.

  it("templates.get hits GET /v1/templates/{id} and maps snake_case wire fields", async () => {
    globalThis.fetch = mockFetch(200, {
      id: "tmpl_1",
      name: "Welcome",
      subject: "Welcome, {{name}}!",
      text: "Hi {{name}}",
      html: "<p>Hi {{name}}</p>",
      from_starter_alias: "welcome",
      from_starter_version: "1",
      created_at: "2026-06-01T00:00:00Z",
      updated_at: "2026-06-01T00:00:00Z",
    });
    const tmpl = await client.templates.get("tmpl_1");
    const { url, init } = lastCall();
    expect(init.method).toBe("GET");
    expect(url).toContain("/v1/templates/tmpl_1");
    expect(tmpl.html).toBe("<p>Hi {{name}}</p>");
    expect(tmpl.fromStarterAlias).toBe("welcome");
    expect(tmpl.fromStarterVersion).toBe("1");
  });

  it("templates.create POSTs camelCase input as the snake_case wire body", async () => {
    globalThis.fetch = mockFetch(201, {
      id: "tmpl_new", name: "Approvals", subject: "s", text: "b",
      created_at: "2026-06-01T00:00:00Z", updated_at: "2026-06-01T00:00:00Z",
    });
    await client.templates.create({ fromStarter: "approval-request", alias: "my-approvals" });
    const { url, init } = lastCall();
    expect(init.method).toBe("POST");
    expect(url).toContain("/v1/templates");
    // Exactly the caller's fields reach the wire, snake_cased — no fabricated
    // subject/body keys that would trip the server's from_starter exclusivity.
    expect(JSON.parse(init.body as string)).toEqual({
      from_starter: "approval-request",
      alias: "my-approvals",
    });
  });

  it("templates.update PATCHes the id and keeps an explicit html:'' clear", async () => {
    globalThis.fetch = mockFetch(200, {
      id: "tmpl_1", name: "Welcome", subject: "New {{x}}", text: "b",
      created_at: "2026-06-01T00:00:00Z", updated_at: "2026-06-02T00:00:00Z",
    });
    await client.templates.update("tmpl_1", { subject: "New {{x}}", html: "" });
    const { url, init } = lastCall();
    expect(init.method).toBe("PATCH");
    expect(url).toContain("/v1/templates/tmpl_1");
    expect(JSON.parse(init.body as string)).toEqual({ subject: "New {{x}}", html: "" });
  });

  it("templates.delete issues DELETE /v1/templates/{id} and returns the deletion object", async () => {
    globalThis.fetch = mockFetch(200, { deleted: true, id: "tmpl_1" });
    const res = await client.templates.delete("tmpl_1");
    const { url, init } = lastCall();
    expect(init.method).toBe("DELETE");
    expect(url).toContain("/v1/templates/tmpl_1");
    expect(res.deleted).toBe(true);
    expect(res.id).toBe("tmpl_1");
  });

  it("templates.validate POSTs to /v1/templates/validate and maps the response", async () => {
    globalThis.fetch = mockFetch(200, {
      valid: true,
      errors: [],
      rendered: { subject: "Welcome, Ada!", text: "Hi Ada", html: "<p>Hi Ada</p>" },
      // suggested_data is nested (dot-path variables emit nested objects).
      suggested_data: { user: { name: "example" } },
    });
    const res = await client.templates.validate({
      subject: "Welcome, {{user.name}}!",
      text: "Hi {{user.name}}",
      testData: { user: { name: "Ada" } },
    });
    const { url, init } = lastCall();
    expect(init.method).toBe("POST");
    expect(url).toContain("/v1/templates/validate");
    expect(JSON.parse(init.body as string)).toEqual({
      subject: "Welcome, {{user.name}}!",
      text: "Hi {{user.name}}",
      test_data: { user: { name: "Ada" } },
    });
    expect(res.valid).toBe(true);
    expect(res.rendered?.html).toBe("<p>Hi Ada</p>");
    expect(res.suggestedData).toEqual({ user: { name: "example" } });
  });

  it("templates.getStarter hits GET /v1/starter-templates/{alias} with body sources", async () => {
    globalThis.fetch = mockFetch(200, {
      alias: "approval-request",
      name: "Approval request",
      description: "Ask a human to approve an action.",
      version: "1",
      subject: "Approval needed: {{action}}",
      text: "Approve: {{approve_url}}",
      html: '<a href="{{approve_url}}">Approve</a>',
      variables: [
        { name: "approve_url", required: true, raw: false, description: "d", example: "https://x/approve" },
      ],
    });
    const starter = await client.templates.getStarter("approval-request");
    const { url, init } = lastCall();
    expect(init.method).toBe("GET");
    expect(url).toContain("/v1/starter-templates/approval-request");
    expect(starter.html).toContain("{{approve_url}}");
    expect(starter.variables[0].name).toBe("approve_url");
  });

  it("maps a template-part parse failure to E2AValidationError with the machine code", async () => {
    globalThis.fetch = mockFetch(400, {
      error: { code: "invalid_template", message: "template part body failed to parse" },
    });
    const err = await client.templates
      .create({ name: "x", subject: "s", text: "{{#bad}}" })
      .catch((e) => e);
    expect(err).toBeInstanceOf(E2AValidationError);
    expect(err.code).toBe("invalid_template");
    expect(err.retryable).toBe(false);
  });

  // ── Error mapping ───────────────────────────────────────────────

  it("maps a 404 envelope to E2ANotFoundError", async () => {
    globalThis.fetch = mockFetch(404, { error: { code: "agent_not_found", message: "no such agent" } });
    await expect(client.agents.get("ghost@test.dev")).rejects.toBeInstanceOf(E2ANotFoundError);
  });

  it("maps a 409 envelope to E2AConflictError", async () => {
    globalThis.fetch = mockFetch(409, { error: { code: "domain_exists", message: "already registered" } });
    await expect(
      client.domains.create({ domain: "dup.dev" } as never),
    ).rejects.toBeInstanceOf(E2AConflictError);
  });

  it("maps a 422 envelope to E2AValidationError", async () => {
    globalThis.fetch = mockFetch(422, { error: { code: "invalid_request", message: "bad input" } });
    await expect(
      client.agents.create({ email: "" } as never),
    ).rejects.toBeInstanceOf(E2AValidationError);
  });

  it("surfaces the envelope code/message/requestId on the typed error", async () => {
    globalThis.fetch = mockFetch(
      404,
      { error: { code: "agent_not_found", message: "no such agent" } },
      { "x-request-id": "req_abc" },
    );
    try {
      await client.agents.get("ghost@test.dev");
      throw new Error("expected to throw");
    } catch (e) {
      expect(e).toBeInstanceOf(E2AError);
      const err = e as E2AError;
      expect(err.code).toBe("agent_not_found");
      expect(err.message).toBe("no such agent");
      expect(err.requestId).toBe("req_abc");
      expect(err.status).toBe(404);
    }
  });

  // ── webhooks.deliveries pagination ──────────────────────────────

  it("webhooks.deliveries threads next_cursor across pages", async () => {
    // The delivery log is keyset-paginated now — the AutoPager walks the cursor
    // to completion instead of silently capping at one page.
    const { fn, calls } = pagingFetch({
      "": { items: [{ id: "del_1" }], next_cursor: "cur_2" },
      cur_2: { items: [{ id: "del_2" }], next_cursor: null },
    });
    globalThis.fetch = fn as unknown as typeof fetch;
    const items = await client.webhooks.deliveries("wh_1").toArray({ limit: 100 });
    expect(items.map((d) => d.id)).toEqual(["del_1", "del_2"]);
    expect(calls).toHaveLength(2);
    expect(calls[1]).toContain("cursor=cur_2");
  });

  // ── account + suppressions smoke (thin passthroughs) ────────────

  it("account.apiKeys.create sends a caller-supplied Idempotency-Key", async () => {
    globalThis.fetch = mockFetch(201, { id: "key_1", key: "e2a_agt_secret" });
    await client.account.apiKeys.create(
      { agentEmail: "bot@test.dev", name: "ci" } as never,
      { idempotencyKey: "key-req-123" },
    );
    const { url, init, headers } = lastCall();
    expect(init.method).toBe("POST");
    expect(url).toContain("/v1/account/api-keys");
    expect(headers["Idempotency-Key"]).toBe("key-req-123");
  });

  it("account.get / export / suppressions hit the right operations", async () => {
    globalThis.fetch = mockFetch(200, { plan: "free" });
    await client.account.get();
    expect(lastCall().url).toContain("/v1/account");

    globalThis.fetch = mockFetch(200, { items: [{ address: "blocked@x.com" }], next_cursor: null });
    const supp = await client.account.suppressions.list().toArray({ limit: 10 });
    expect(supp).toHaveLength(1);
    expect(lastCall().url).toContain("/v1/account/suppressions");
  });

  // ── connection-error path through call() ────────────────────────

  it("maps a transport-level failure to E2AConnectionError", async () => {
    // fetch rejects (DNS/refused/abort) with no HTTP response. With retries
    // off, the retry layer rethrows and call() wraps it as a connection error.
    const c = new E2AClient({ apiKey: "e2a_test", baseUrl: BASE, maxRetries: 0 });
    globalThis.fetch = vi.fn(async () => { throw new TypeError("fetch failed"); }) as unknown as typeof fetch;
    await expect(c.agents.get("bot@test.dev")).rejects.toBeInstanceOf(E2AConnectionError);
  });

  // ── listen() ────────────────────────────────────────────────────

  it("listen() requires an email", () => {
    expect(() => client.listen("")).toThrow(/email is required/);
  });

  // ── dot-segment path-parameter guard (e2a#792) ───────────────────
  // ".." collapses the built URL onto the preceding segment, and "." onto the
  // segment's own parent collection, retargeting each of these mutations at a
  // different, larger resource than the caller named.
  //
  // The full openapi.yaml-driven enumeration (every path param whose ".." or
  // "." collapse lands on a DIFFERENT route sharing the same HTTP method)
  // lives once, in sdks/python/tests/test_dot_segment_enumeration.py: both
  // SDKs share the one spec and the one set of ergonomic call sites (same
  // shape guarded at the same routes), so a second full parser here would
  // duplicate that denominator computation rather than add coverage; this
  // file pins the same ten guarded call sites directly against the TS
  // client instead.

  describe("dot-segment path guard", () => {
    it("rejects agents.deleteSuppression(email, '..') before any request is sent", async () => {
      globalThis.fetch = mockFetch(200, {});
      await expect(
        client.agents.deleteSuppression("sender@example.com", ".."),
      ).rejects.toMatchObject({ code: "unsafe_path_segment" });
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    it("rejects contacts.deleteOutreach(email, '.') before any request is sent", async () => {
      globalThis.fetch = mockFetch(200, {});
      await expect(
        client.contacts.deleteOutreach("sender@example.com", "."),
      ).rejects.toMatchObject({ code: "unsafe_path_segment" });
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    it("rejects messages.delete(email, '..') before any request is sent", async () => {
      globalThis.fetch = mockFetch(200, {});
      await expect(
        client.messages.delete("sender@example.com", ".."),
      ).rejects.toMatchObject({ code: "unsafe_path_segment" });
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    it("rejects account.suppressions.delete('..') before any request is sent", async () => {
      globalThis.fetch = mockFetch(200, {});
      await expect(client.account.suppressions.delete("..")).rejects.toMatchObject({
        code: "unsafe_path_segment",
      });
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    it("rejects account.apiKeys.delete('..') before any request is sent", async () => {
      globalThis.fetch = mockFetch(200, {});
      await expect(client.account.apiKeys.delete("..")).rejects.toMatchObject({
        code: "unsafe_path_segment",
      });
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    it("still allows an ordinary address through", async () => {
      globalThis.fetch = mockFetch(200, { deleted: true, address: "recipient@example.net" });
      const res = await client.agents.deleteSuppression("sender@example.com", "recipient@example.net");
      expect(res.deleted).toBe(true);
    });

    it("throws E2AValidationError, not the bare base class", async () => {
      globalThis.fetch = mockFetch(200, {});
      await expect(
        client.agents.deleteSuppression("sender@example.com", ".."),
      ).rejects.toBeInstanceOf(E2AValidationError);
    });

    // review follow-up (2026-08-19): deleteOutreach's `address` param was
    // guarded from the start, but `email` was not: collapsing it retargets
    // DELETE /v1/agents/{email}/contacts/{address} onto
    // DELETE /v1/contacts/{address} (deleteContact), which the same
    // ?confirm=DELETE already satisfies and which permanently deletes the
    // contact record the docstring says survives this call.
    it("rejects contacts.deleteOutreach('..', address) before any request is sent", async () => {
      globalThis.fetch = mockFetch(200, {});
      await expect(
        client.contacts.deleteOutreach("..", "recipient@example.net"),
      ).rejects.toMatchObject({ code: "unsafe_path_segment" });
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    // review follow-up (2026-08-19): messages.restore() had no guard at all:
    // collapsing `id` retargets POST /v1/agents/{email}/messages/{id}/restore
    // onto POST /v1/agents/{email}/restore (restoreAgent), undoing a
    // deliberate agent deletion instead of restoring a message. The response
    // is typed MessageView but the server actually returns an AgentView.
    it("rejects messages.restore(email, '..') before any request is sent", async () => {
      globalThis.fetch = mockFetch(200, {});
      await expect(
        client.messages.restore("sender@example.com", ".."),
      ).rejects.toMatchObject({ code: "unsafe_path_segment" });
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    // Found by re-running the issue's own enumeration methodology (every
    // route whose param collapse lands on ANOTHER route sharing the same
    // HTTP method, not just DELETE): messages.updateLabels is a PATCH, and
    // collapsing `id` retargets PATCH /v1/agents/{email}/messages/{id} onto
    // PATCH /v1/agents/{email} (updateAgent). Not independently confirmed
    // live against a real server (updateAgent's body schema rejects the
    // labels fields via additionalProperties: false), but the client should
    // never send a PATCH at the wrong resource regardless of what the server
    // does with it.
    it("rejects messages.updateLabels(email, '..', body) before any request is sent", async () => {
      globalThis.fetch = mockFetch(200, {});
      await expect(
        client.messages.updateLabels("sender@example.com", "..", { addLabels: ["x"] }),
      ).rejects.toMatchObject({ code: "unsafe_path_segment" });
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    // review follow-up (2026-08-20): the enumeration gate only swept ".."
    // collapses, so this "." collapse was missed: deleteImport(".") drops to
    // /v1/contacts/imports, which the router backtracks onto
    // DELETE /v1/contacts/{address} (deleteContact) with address "imports",
    // and the confirm=DELETE this method already sends satisfies
    // deleteContact's own guard.
    it("rejects contacts.deleteImport('.') before any request is sent", async () => {
      globalThis.fetch = mockFetch(200, {});
      await expect(client.contacts.deleteImport(".")).rejects.toMatchObject({
        code: "unsafe_path_segment",
      });
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    // review follow-up (2026-08-20): a surviving path param is caller-
    // controlled and can equal a route literal, so setOutreach(".",
    // "protection", body) collapses PUT /v1/agents/{email}/contacts/{address}
    // onto PUT /v1/agents/{email}/protection (putAgentProtection), a
    // same-method retarget.
    it("rejects contacts.setOutreach('.', address, body) before any request is sent", async () => {
      globalThis.fetch = mockFetch(200, {});
      await expect(
        client.contacts.setOutreach(".", "protection", {}),
      ).rejects.toMatchObject({ code: "unsafe_path_segment" });
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });
  });
});
