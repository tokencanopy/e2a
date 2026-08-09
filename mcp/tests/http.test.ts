import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import http from "node:http";
import { readFileSync } from "node:fs";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import type { McpClient } from "../src/client.js";
import { startHttpServer, type HttpServerOptions } from "../src/http-server.js";
import { ResolveCache } from "../src/resolve.js";
import { MetricsRegistry } from "../src/metrics.js";
import type { Logger } from "../src/logging.js";

const frozenToolNames = JSON.parse(
  readFileSync(new URL("../tool-names.v1.json", import.meta.url), "utf8"),
) as string[];

// GET a URL with an explicit Host header. undici's fetch forbids overriding
// Host, so host-sensitive discovery tests drop to the raw http client.
function getJsonWithHost(
  port: number,
  path: string,
  host: string,
  extraHeaders: Record<string, string> = {},
): Promise<{ resource?: string; [k: string]: unknown }> {
  return new Promise((resolve, reject) => {
    const req = http.request(
      { host: "127.0.0.1", port, path, method: "GET", headers: { Host: host, ...extraHeaders } },
      (res) => {
        let data = "";
        res.setEncoding("utf8");
        res.on("data", (chunk) => (data += chunk));
        res.on("end", () => {
          try {
            resolve(JSON.parse(data));
          } catch {
            reject(new Error(`non-JSON response (${res.statusCode}): ${data}`));
          }
        });
      },
    );
    req.on("error", reject);
    req.end();
  });
}

function postJsonWithHost(
  port: number,
  path: string,
  host: string,
  body: unknown,
): Promise<{ statusCode?: number; wwwAuthenticate?: string }> {
  return new Promise((resolve, reject) => {
    const req = http.request(
      {
        host: "127.0.0.1",
        port,
        path,
        method: "POST",
        headers: {
          Host: host,
          "Content-Type": "application/json",
          Accept: "application/json, text/event-stream",
        },
      },
      (res) => {
        res.resume();
        res.on("end", () => {
          const challenge = res.headers["www-authenticate"];
          resolve({
            statusCode: res.statusCode,
            wwwAuthenticate: Array.isArray(challenge) ? challenge.join(", ") : challenge,
          });
        });
      },
    );
    req.on("error", reject);
    req.end(JSON.stringify(body));
  });
}

// Stub the McpClient wrapper — only the methods the tools and the
// session prefetch (listAgents / agentEmail) actually touch. List
// methods return flat arrays (the wrapper collapses the SDK pager).
function makeStubClient(): McpClient {
  const stub = {
    agentEmail: "bot@example.com",
    scope: "account" as const,
    whoami: vi.fn(async () => ({ user: "owner@example.com", scope: "account", agentEmail: undefined })),
    getMessage: vi.fn(async (id: string) => ({ id })),
    getAgent: vi.fn(async (e: string) => ({ id: e, email: e })),
    send: vi.fn(async () => ({ messageId: "msg_sent", status: "sent" })),
    reply: vi.fn(async () => ({ messageId: "msg_reply", status: "sent" })),
    listMessages: vi.fn(async () => ({ items: [], next_cursor: undefined })),
    listAgents: vi.fn(async () => []),
    createAgent: vi.fn(async () => ({ email: "x@y", id: "x", domain: "y" })),
    listReviews: vi.fn(async () => []),
    getReview: vi.fn(async () => ({ messageId: "p", status: "pending_review" })),
    approveReview: vi.fn(async () => ({ messageId: "x", status: "sent" })),
    rejectReview: vi.fn(async () => ({ messageId: "x", status: "rejected" })),
    // Templates (beta) — SDK-backed via the shared E2AClient, so a
    // factory-built session supports them like any other tool.
    listTemplates: vi.fn(async () => ({
      items: [{ id: "tmpl_1", name: "Welcome", alias: "welcome", subject: "Welcome, {{name}}!" }],
      next_cursor: undefined,
    })),
  };
  return stub as unknown as McpClient;
}

function makeHttpError(statusCode: number): Error & { statusCode: number } {
  const err = new Error(`HTTP ${statusCode}`) as Error & { statusCode: number };
  err.statusCode = statusCode;
  return err;
}

describe("HTTP MCP server", () => {
  let stub: McpClient;
  let close: () => Promise<void>;
  let url: string;

  beforeEach(async () => {
    stub = makeStubClient();
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      // Loopback hostnames vary with the random port; allow them all.
      allowedHosts: ["127.0.0.1", "localhost"],
      clientFactory: () => stub,
      // Keep test output quiet; logging behavior has dedicated tests below.
      logger: () => {},
    });
    close = c;
    url = `http://127.0.0.1:${port}/mcp`;
  });

  afterEach(async () => {
    await close();
  });

  async function connect(headers: Record<string, string> = { Authorization: "Bearer e2a_test" }) {
    const transport = new StreamableHTTPClientTransport(new URL(url), {
      requestInit: { headers },
    });
    const client = new Client({ name: "http-test", version: "0.0.0" });
    await client.connect(transport);
    return { client, transport };
  }

  it("healthz responds OK without auth", async () => {
    const res = await fetch(`${url.replace("/mcp", "")}/healthz`);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ ok: true });
  });

  it("missing bearer returns 401 with WWW-Authenticate", async () => {
    const res = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
      },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "initialize",
        params: { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "x", version: "0" } },
      }),
    });
    expect(res.status).toBe(401);
    expect(res.headers.get("www-authenticate")).toMatch(/Bearer realm="e2a"/);
    const body = await res.json();
    expect(body.error.message).toMatch(/missing bearer/);
  });

  it("rejects a missing bearer before parsing malformed JSON", async () => {
    const res = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
      },
      body: '{"jsonrpc":',
    });

    expect(res.status).toBe(401);
    expect(res.headers.get("www-authenticate")).toMatch(/Bearer realm="e2a"/);
    expect(await res.json()).toMatchObject({
      jsonrpc: "2.0",
      id: null,
      error: { code: -32001, message: "missing bearer token" },
    });
  });

  it("accepts a request body larger than 1 MB (the attachment contract), not a 413", async () => {
    // The attachment tools advertise up to 10 MB/attachment and 25 MB total,
    // and Streamable-HTTP is the only transport — so the body limit must clear
    // the attachment contract. An authenticated ~2 MB body must reach the MCP
    // handler, not be rejected with 413 by the route-local parser.
    const bigArg = "x".repeat(2 * 1024 * 1024); // ~2 MB, well over the old 1 MB cap
    const res = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: "Bearer e2a_test",
      },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "initialize",
        params: {
          protocolVersion: "2024-11-05",
          capabilities: {},
          clientInfo: { name: bigArg, version: "0" },
        },
      }),
    });
    expect(res.status).toBe(200);
  });

  it("invalid bearer (whoami 401) is rejected with an invalid_token challenge", async () => {
    await close();
    const invalidStub = makeStubClient();
    invalidStub.whoami = vi.fn(async () => {
      throw makeHttpError(401);
    }) as McpClient["whoami"];
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      clientFactory: () => invalidStub,
    });
    close = c;
    url = `http://127.0.0.1:${port}/mcp`;

    const res = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: "Bearer bogus_token",
      },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "initialize",
        params: {
          protocolVersion: "2024-11-05",
          capabilities: {},
          clientInfo: { name: "x", version: "0" },
        },
      }),
    });

    expect(res.status).toBe(401);
    expect(res.headers.get("www-authenticate")).toMatch(
      /Bearer realm="e2a", .*error="invalid_token"/,
    );
    // Stateless: no session id is ever issued.
    expect(res.headers.get("mcp-session-id")).toBeNull();
    expect(invalidStub.whoami).toHaveBeenCalledOnce();
    const body = await res.json();
    expect(body.error.message).toMatch(/invalid bearer/);
  });

  it("rejects an invalid bearer before parsing malformed JSON", async () => {
    await close();
    const invalidStub = makeStubClient();
    invalidStub.whoami = vi.fn(async () => {
      throw makeHttpError(401);
    }) as McpClient["whoami"];
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      clientFactory: () => invalidStub,
    });
    close = c;
    url = `http://127.0.0.1:${port}/mcp`;

    const res = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: "Bearer bogus_token",
      },
      body: '{"jsonrpc":',
    });

    expect(res.status).toBe(401);
    expect(invalidStub.whoami).toHaveBeenCalledOnce();
    expect(await res.json()).toMatchObject({
      jsonrpc: "2.0",
      id: null,
      error: { code: -32001, message: "invalid bearer token" },
    });
  });

  it("stateless: the initialize response carries no Mcp-Session-Id", async () => {
    // The defining property of stateless mode — the server never allocates a
    // session id, so there is no in-memory session to idle-GC and therefore
    // nothing to drop a connection between requests (#298 follow-up).
    const initRes = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: "Bearer e2a_test",
      },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "initialize",
        params: {
          protocolVersion: "2024-11-05",
          capabilities: {},
          clientInfo: { name: "x", version: "0" },
        },
      }),
    });
    expect(initRes.status).toBe(200);
    expect(initRes.headers.get("mcp-session-id")).toBeNull();
    await initRes.text();
  });

  it("stateless: each request authenticates independently from its own bearer", async () => {
    // No session-id ⇒ no session-hijack surface. Two unrelated POSTs (no
    // initialize, no session header) each resolve against their OWN bearer.
    // A bearer whose whoami 401s is rejected; a valid one is served — proven
    // back to back against the same running server.
    await close();
    const okStub = makeStubClient();
    const badStub = makeStubClient();
    badStub.whoami = vi.fn(async () => {
      throw makeHttpError(401);
    }) as McpClient["whoami"];
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      clientFactory: (bearer: string) => (bearer === "good_token" ? okStub : badStub),
    });
    close = c;
    const local = `http://127.0.0.1:${port}/mcp`;

    const bad = await fetch(local, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: "Bearer bad_token",
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" }),
    });
    expect(bad.status).toBe(401);

    const good = await fetch(local, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: "Bearer good_token",
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: 2, method: "tools/list" }),
    });
    expect(good.status).toBe(200);
  });

  it("oauth-protected-resource discovery returns expected metadata", async () => {
    const res = await fetch(`${url.replace("/mcp", "")}/.well-known/oauth-protected-resource`);
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body.authorization_servers).toEqual(["http://e2a.local"]);
    expect(body.bearer_methods_supported).toEqual(["header"]);
  });

  it("discovery scheme falls back to the connection scheme without X-Forwarded-Proto", async () => {
    const origin = url.replace("/mcp", "");
    const res = await fetch(`${origin}/.well-known/oauth-protected-resource`);
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body.resource).toBe(origin);
  });

  it("honors X-Forwarded-Proto from a trusted loopback proxy", async () => {
    const origin = url.replace("/mcp", "");
    const res = await fetch(`${origin}/.well-known/oauth-protected-resource`, {
      headers: { "X-Forwarded-Proto": "https" },
    });
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body.resource).toBe(origin.replace(/^http:/, "https:"));
  });

  it("ignores X-Forwarded-Proto when the proxy is not trusted", async () => {
    await close();
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      trustProxy: false,
      clientFactory: () => stub,
    });
    close = c;
    const res = await fetch(`http://127.0.0.1:${port}/.well-known/oauth-protected-resource`, {
      headers: { "X-Forwarded-Proto": "https" },
    });
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body.resource).toBe(`http://127.0.0.1:${port}`);
  });

  // #635: prod served https://api.e2a.dev/mcp but advertised the resource as
  // http://api.e2a.dev because the fronting proxy's X-Forwarded-Proto wasn't
  // trusted. A public (non-loopback) host must always be advertised as https,
  // even without a trusted forwarded-proto header.
  it("synthesizes https for a public host without X-Forwarded-Proto (#635)", async () => {
    await close();
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["api.e2a.dev"],
      trustProxy: false,
      clientFactory: () => stub,
    });
    close = c;
    const body = await getJsonWithHost(
      port,
      "/.well-known/oauth-protected-resource",
      "api.e2a.dev",
    );
    expect(body.resource).toBe("https://api.e2a.dev");

    const challenge = await postJsonWithHost(port, "/mcp", "api.e2a.dev", {
      jsonrpc: "2.0",
      id: 1,
      method: "initialize",
      params: {
        protocolVersion: "2024-11-05",
        capabilities: {},
        clientInfo: { name: "x", version: "0" },
      },
    });
    expect(challenge.statusCode).toBe(401);
    expect(challenge.wwwAuthenticate).toContain(
      'resource_metadata="https://api.e2a.dev/.well-known/oauth-protected-resource"',
    );
  });

  it("lists every registered tool after initialize", async () => {
    const { client, transport } = await connect();
    const { tools } = await client.listTools();
    expect(tools.map((t) => t.name).sort()).toEqual(frozenToolNames);
    await transport.close();
  });

  it("over the wire, an agent-scoped session lists only the runtime tier", async () => {
    // End-to-end scope-gating across the real Streamable-HTTP transport: a
    // session whose whoami reports agent scope sees the 15 runtime tools and
    // none of the admin tools — proven over JSON-RPC, not just in-process.
    await close();
    const agentStub = makeStubClient();
    (agentStub as { scope: string }).scope = "agent";
    agentStub.whoami = vi.fn(async () => ({
      user: "owner@example.com",
      scope: "agent",
      agentEmail: "bot@example.com",
    })) as McpClient["whoami"];
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      // Factory returns the agent-scoped stub for both probe and final make().
      clientFactory: () => agentStub,
    });
    close = c;
    url = `http://127.0.0.1:${port}/mcp`;

    const { client, transport } = await connect();
    const names = new Set((await client.listTools()).tools.map((t) => t.name));
    expect(names.size).toBe(23);
    expect(names.has("send_message")).toBe(true); // runtime present
    expect(names.has("send_email")).toBe(true); // deprecated runtime alias
    expect(names.has("get_attachment_data")).toBe(true); // deprecated runtime alias
    expect(names.has("restore_message")).toBe(true); // per-agent trash lifecycle
    expect(names.has("delete_message")).toBe(true); // soft delete is per-agent too
    expect(names.has("get_message_lifecycle")).toBe(true); // per-agent diagnostic read
    expect(names.has("create_agent")).toBe(false); // admin hidden
    expect(names.has("list_agents")).toBe(false); // REST requires account scope
    expect(names.has("restore_agent")).toBe(false); // account administration
    expect(names.has("delete_domain")).toBe(false);
    expect(names.has("list_webhooks")).toBe(false);
    // The account-wide review queue and decisions are account-scope only.
    expect(names.has("list_reviews")).toBe(false);
    expect(names.has("get_review")).toBe(false);
    expect(names.has("approve_review")).toBe(false);
    // credential minting is account-scope (an agent must not mint itself keys).
    expect(names.has("create_api_key")).toBe(false);
    expect(names.has("reject_review")).toBe(false);
    await transport.close();
  });

  it("over the wire, list_messages paginates by cursor (§6a #3)", async () => {
    // E2E for the cursor shape across the real Streamable-HTTP/JSON-RPC
    // transport: page 1 returns items + next_cursor; passing that cursor back
    // returns page 2 with no next_cursor (last page). Proves the cursor
    // round-trips over the wire, not just in-process.
    await close();
    const pgStub = makeStubClient();
    pgStub.listMessages = vi.fn(async (params: { cursor?: string }) =>
      params?.cursor === "c2"
        ? { items: [{ id: "m3" }], next_cursor: undefined }
        : { items: [{ id: "m1" }, { id: "m2" }], next_cursor: "c2" },
    ) as McpClient["listMessages"];
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      clientFactory: () => pgStub,
    });
    close = c;
    url = `http://127.0.0.1:${port}/mcp`;

    const { client, transport } = await connect();
    const page1 = JSON.parse(
      ((await client.callTool({ name: "list_messages", arguments: { limit: 2 } }))
        .content as Array<{ text: string }>)[0].text,
    );
    expect(page1.messages).toHaveLength(2);
    expect(page1.next_cursor).toBe("c2");

    const page2 = JSON.parse(
      ((await client.callTool({ name: "list_messages", arguments: { cursor: page1.next_cursor } }))
        .content as Array<{ text: string }>)[0].text,
    );
    expect(page2.messages).toEqual([{ id: "m3" }]);
    expect(page2).not.toHaveProperty("next_cursor"); // last page
    await transport.close();
  });

  describe("bearer scope + agent resolution", () => {
    // resolvePrincipal resolves the credential's scope and bound agent from
    // whoami (GET /account): agent scope pins the bound agent (whoami
    // agent_address) and exposes the runtime tier; account scope has no default
    // agent and exposes the full surface. We drive it via clientFactory to
    // observe what the client is constructed with. On a cache MISS the factory
    // is called for the whoami probe (bearer only) then for the per-request
    // client (bearer + resolved {agentEmail?, scope}); cache hits on later
    // requests of the same connection skip the probe, so whoami runs once.

    function makeProbeClient(opts: {
      scope?: "account" | "agent";
      agentEmail?: string;
      whoamiThrows?: boolean | "unauthorized";
    }): McpClient {
      return {
        agentEmail: "",
        scope: opts.scope ?? "account",
        whoami: vi.fn(async () => {
          if (opts.whoamiThrows === "unauthorized") throw makeHttpError(401);
          if (opts.whoamiThrows) throw new Error("upstream 500");
          return {
            user: "owner@example.com",
            scope: opts.scope ?? "account",
            agentEmail: opts.agentEmail,
          };
        }),
        listMessages: vi.fn(async () => ({ items: [], next_cursor: undefined })),
        listAgents: vi.fn(async () => []),
      } as unknown as McpClient;
    }

    async function startWithFactory(
      factory: (bearer: string, o?: { agentEmail?: string; scope?: string }) => McpClient,
    ) {
      await close();
      const { close: c, port } = await startHttpServer(0, {
        baseUrl: "http://e2a.local",
        allowedHosts: ["127.0.0.1", "localhost"],
        clientFactory: factory as never,
      });
      close = c;
      url = `http://127.0.0.1:${port}/mcp`;
    }

    it("agent scope pins the credential-bound agent from whoami", async () => {
      const probe = makeProbeClient({ scope: "agent", agentEmail: "solo@bot.example.com" });
      const factory = vi.fn(() => probe);
      await startWithFactory(factory);

      const { transport } = await connect();
      // whoami runs once and is cached for the connection's later POSTs.
      expect(probe.whoami).toHaveBeenCalledOnce();
      // First construction is the probe (bearer only); second is the
      // per-request client with the resolved agent + scope.
      expect(factory.mock.calls[0]).toEqual(["e2a_test"]);
      expect(factory.mock.calls[1]).toEqual([
        "e2a_test",
        { agentEmail: "solo@bot.example.com", scope: "agent" },
      ]);
      await transport.close();
    });

    it("account scope resolves to the full surface with no default agent", async () => {
      const probe = makeProbeClient({ scope: "account" });
      const factory = vi.fn(() => probe);
      await startWithFactory(factory);

      const { transport } = await connect();
      expect(probe.whoami).toHaveBeenCalledOnce();
      expect(factory.mock.calls[0]).toEqual(["e2a_test"]);
      // No agentEmail for account scope — explicit email required per §6a.
      expect(factory.mock.calls[1]).toEqual(["e2a_test", { scope: "account" }]);
      await transport.close();
    });

    it("whoami non-auth failure falls back to least-privilege agent scope (request still served)", async () => {
      const probe = makeProbeClient({ whoamiThrows: true });
      const factory = vi.fn(() => probe);
      await startWithFactory(factory);

      const { client, transport } = await connect();
      const { tools } = await client.listTools();
      expect(tools.length).toBeGreaterThan(0); // initialize not blocked
      // Fail-closed: runtime/agent scope, no default agent.
      expect(factory.mock.calls[1]).toEqual(["e2a_test", { scope: "agent" }]);
      await transport.close();
    });

    it("a transient whoami failure is NOT cached — every request re-probes", async () => {
      // The fail-closed fallback must not stick: caching it would pin a healthy
      // bearer to least-privilege for the whole TTL after one backend blip.
      // So each POST re-probes whoami until the backend recovers.
      const probe = makeProbeClient({ whoamiThrows: true });
      const factory = vi.fn(() => probe);
      await startWithFactory(factory);

      // Two independent POSTs (no session in stateless mode). Each bare
      // tools/list is served on its own (200) — a stateful revert would 400
      // the second with "no session", so the status assertion pins statelessness.
      for (const id of [1, 2]) {
        const res = await fetch(url, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Accept: "application/json, text/event-stream",
            Authorization: "Bearer e2a_test",
          },
          body: JSON.stringify({ jsonrpc: "2.0", id, method: "tools/list" }),
        });
        expect(res.status).toBe(200);
        await res.text();
      }
      // Re-probed once per request (would be 1 if the failure were cached).
      expect((probe.whoami as ReturnType<typeof vi.fn>).mock.calls.length).toBe(2);
    });

    it("a successful whoami IS cached — a second request skips the probe", async () => {
      const probe = makeProbeClient({ scope: "agent", agentEmail: "solo@bot.example.com" });
      const factory = vi.fn(() => probe);
      await startWithFactory(factory);

      for (const id of [1, 2]) {
        const res = await fetch(url, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Accept: "application/json, text/event-stream",
            Authorization: "Bearer e2a_test",
          },
          body: JSON.stringify({ jsonrpc: "2.0", id, method: "tools/list" }),
        });
        expect(res.status).toBe(200); // both served — no session gate
        await res.text();
      }
      // Cached across both requests: whoami runs exactly once.
      expect(probe.whoami).toHaveBeenCalledOnce();
    });

    it("whoami 401 rejects session init as invalid bearer", async () => {
      const probe = makeProbeClient({ whoamiThrows: "unauthorized" });
      const factory = vi.fn(() => probe);
      await startWithFactory(factory);

      const res = await fetch(url, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Accept: "application/json, text/event-stream",
          Authorization: "Bearer bogus_token",
        },
        body: JSON.stringify({
          jsonrpc: "2.0",
          id: 1,
          method: "initialize",
          params: { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "x", version: "0" } },
        }),
      });
      expect(res.status).toBe(401);
      expect(probe.whoami).toHaveBeenCalledOnce();
    });
  });

  it("stateless: a request after the old idle-GC window still works (re-resolves, never drops)", async () => {
    // The headline win of going stateless. The old stateful server reaped an
    // idle session after 5 min (300_000 ms) and answered the next request with
    // 400 "no session -- send initialize first". With no session there is
    // nothing to reap: a gap merely expires the resolve cache, so the next bare
    // request re-probes whoami and is served. We drive the cache clock past
    // BOTH the cache TTL and the old idle window and assert the second request
    // still 200s.
    await close();
    let nowMs = 0;
    const cache = new ResolveCache({ ttlMs: 60_000, maxEntries: 10, now: () => nowMs });
    const idleStub = makeStubClient();
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      clientFactory: () => idleStub,
      resolveCache: cache,
    });
    close = c;
    const local = `http://127.0.0.1:${port}/mcp`;
    const post = async (id: number) => {
      const res = await fetch(local, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Accept: "application/json, text/event-stream",
          Authorization: "Bearer idle_token",
        },
        body: JSON.stringify({ jsonrpc: "2.0", id, method: "tools/list" }),
      });
      const text = await res.text();
      return { status: res.status, text };
    };

    const first = await post(1);
    expect(first.status).toBe(200);
    expect(idleStub.whoami).toHaveBeenCalledOnce();

    nowMs = 600_000; // 10 min — twice the old idle-GC window, past the cache TTL.

    const second = await post(2);
    expect(second.status).toBe(200); // not 400 "no session": the connection never drops
    expect(second.text).toContain("send_message");
    // Re-resolved after the gap rather than dropped.
    expect((idleStub.whoami as ReturnType<typeof vi.fn>).mock.calls.length).toBe(2);
  });

  it("stateless: concurrent requests with distinct bearers don't cross-contaminate", async () => {
    // No session id ⇒ no shared per-connection state. Two bearers hitting the
    // server at the same instant each resolve and dispatch with their OWN
    // credential — proven by the factory seeing both bearers, never one twice.
    await close();
    const factory = vi.fn((bearer: string) => {
      const s = makeStubClient();
      (s as { agentEmail: string }).agentEmail = bearer === "tok_a" ? "a@x.com" : "b@x.com";
      return s;
    });
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      clientFactory: factory,
    });
    close = c;
    const local = `http://127.0.0.1:${port}/mcp`;
    const fire = (tok: string, id: number) =>
      fetch(local, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Accept: "application/json, text/event-stream",
          Authorization: `Bearer ${tok}`,
        },
        body: JSON.stringify({ jsonrpc: "2.0", id, method: "tools/list" }),
      }).then((r) => r.text().then(() => r.status));

    const [a, b] = await Promise.all([fire("tok_a", 1), fire("tok_b", 2)]);
    expect(a).toBe(200);
    expect(b).toBe(200);
    const bearersSeen = new Set(factory.mock.calls.map((call) => call[0]));
    expect(bearersSeen).toEqual(new Set(["tok_a", "tok_b"]));
  });

  it("stateless: closes the per-request transport after the response (no leak)", async () => {
    // Real cleanup assertion: the per-request transport is torn down once the
    // response closes. Spying the SDK transport's close proves the res.on(
    // "close") teardown fires on the success path (the throw-path test below
    // never builds a transport, so it can't cover this).
    const closeSpy = vi.spyOn(StreamableHTTPServerTransport.prototype, "close");
    closeSpy.mockClear();
    const res = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: "Bearer e2a_test",
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" }),
    });
    expect(res.status).toBe(200);
    await res.text();
    // The server-side "close" fires just after the body is flushed; give it a tick.
    await new Promise((r) => setTimeout(r, 25));
    expect(closeSpy).toHaveBeenCalled();
    closeSpy.mockRestore();
  });

  it("forwards Bearer transparently to the per-request E2AClient", async () => {
    // The factory we passed receives the bearer string. We can assert it
    // gets exactly what the client sent without any rewriting.
    const factorySpy = vi.fn(() => stub);
    await close();
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      clientFactory: factorySpy,
    });
    close = c;
    url = `http://127.0.0.1:${port}/mcp`;
    const { client, transport } = await connect({ Authorization: "Bearer ate2a_oauth_token_xyz" });
    await client.listTools();
    expect(factorySpy).toHaveBeenCalledWith("ate2a_oauth_token_xyz");
    await transport.close();
  });

  it("tool call dispatches to the per-request client", async () => {
    const { client, transport } = await connect();
    vi.mocked(stub.listAgents).mockClear();
    await client.callTool({ name: "list_agents", arguments: {} });
    expect(stub.listAgents).toHaveBeenCalledOnce();
    await transport.close();
  });

  it("template tools work on a clientFactory-built session (no raw-creds channel needed)", async () => {
    // Regression guard: the old raw-fetch template path needed direct API
    // creds the factory signature couldn't supply, so factory-built sessions
    // advertised the 8 template tools but every call threw. Now that
    // templates ride the SDK through the shared client, a factory-built
    // session must serve them end to end over the real transport.
    const { client, transport } = await connect();
    const res = await client.callTool({ name: "list_templates", arguments: {} });
    expect((res as { isError?: boolean }).isError).not.toBe(true);
    expect(stub.listTemplates).toHaveBeenCalledOnce();
    const payload = JSON.parse((res.content as Array<{ text: string }>)[0].text);
    expect(payload.templates[0].id).toBe("tmpl_1");
    await transport.close();
  });

  it("stateless: a bare tools/list without a prior initialize is served, not 400", async () => {
    // The stateless transport skips the initialize/session gate, so a fresh
    // POST stands on its own. This is the inverse of the old stateful server,
    // which rejected this with 400 "no session -- send initialize first".
    const res = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: "Bearer e2a_test",
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: 99, method: "tools/list" }),
    });
    expect(res.status).toBe(200);
    const text = await res.text();
    // The body (JSON or an SSE frame) carries the tool list, not a "no session"
    // error.
    expect(text).toContain("send_message");
    expect(text).not.toMatch(/no session/);
  });

  it("DELETE /mcp returns 405 (stateless: no session to terminate)", async () => {
    const res = await fetch(url, {
      method: "DELETE",
      headers: { Authorization: "Bearer e2a_test", "mcp-session-id": "anything" },
    });
    expect(res.status).toBe(405);
    const body = await res.json();
    expect(body.error.message).toMatch(/stateless/);
  });

  it("GET /mcp returns 405 (stateless: no standalone SSE stream)", async () => {
    const res = await fetch(url, {
      method: "GET",
      headers: { Authorization: "Bearer e2a_test", "mcp-session-id": "anything" },
    });
    expect(res.status).toBe(405);
    const body = await res.json();
    expect(body.error.code).toBe(-32000);
    expect(body.error.message).toMatch(/stateless/);
  });

  it("GET/DELETE 405 even without a bearer (method genuinely unsupported)", async () => {
    const get = await fetch(url, { method: "GET" });
    expect(get.status).toBe(405);
    const del = await fetch(url, { method: "DELETE" });
    expect(del.status).toBe(405);
  });

  it("an injected resolveCache is populated after a successful request and reused", async () => {
    // Exercises the cache seam end to end: the first POST resolves whoami and
    // writes one principal; a second POST is a cache hit (whoami stays at one).
    await close();
    const cache = new ResolveCache({ ttlMs: 60_000, maxEntries: 10 });
    const cachedStub = makeStubClient();
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      clientFactory: () => cachedStub,
      resolveCache: cache,
    });
    close = c;
    const local = `http://127.0.0.1:${port}/mcp`;
    expect(cache.size()).toBe(0);
    for (const id of [1, 2]) {
      const res = await fetch(local, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Accept: "application/json, text/event-stream",
          Authorization: "Bearer cache_seam_token",
        },
        body: JSON.stringify({ jsonrpc: "2.0", id, method: "tools/list" }),
      });
      await res.text();
    }
    expect(cache.size()).toBe(1);
    expect(cachedStub.whoami).toHaveBeenCalledOnce();
  });

  it("propagates a 500 when clientFactory throws before any transport is built", async () => {
    // The factory throws during the whoami probe, i.e. before a transport or
    // server is ever constructed — so there's nothing to leak here; the
    // success-path teardown is covered by the close-spy test above. This just
    // asserts the throw surfaces as a 5xx rather than hanging the request.
    await close();
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      clientFactory: () => {
        throw new Error("synthetic factory failure");
      },
    });
    close = c;
    const local = `http://127.0.0.1:${port}/mcp`;
    const res = await fetch(local, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: "Bearer e2a_test",
      },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "initialize",
        params: {
          protocolVersion: "2024-11-05",
          capabilities: {},
          clientInfo: { name: "x", version: "0" },
        },
      }),
    });
    // Express's default error handler produces 500 when our async handler
    // throws; what matters here is that we don't leak — see follow-up assertion.
    expect(res.status).toBeGreaterThanOrEqual(500);
  });

  it("a pre-body authentication failure returns a JSON-RPC error without leaking a stack trace", async () => {
    // Without a terminal error middleware, Express's default finalhandler dumps
    // the error stack into the response body when NODE_ENV !== "production"
    // (bin/http never sets it) and never produces a JSON-RPC error. The factory
    // throws during the whoami probe, before the request body is parsed.
    await close();
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      clientFactory: () => {
        throw new Error("synthetic-secret-in-stack");
      },
    });
    close = c;
    const local = `http://127.0.0.1:${port}/mcp`;
    const res = await fetch(local, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: "Bearer e2a_test",
      },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 7,
        method: "initialize",
        params: { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "x", version: "0" } },
      }),
    });
    expect(res.status).toBe(500);
    const text = await res.text();
    // Nothing internal leaks to the client: not the error message, not a stack
    // frame, not an internal file path.
    expect(text).not.toContain("synthetic-secret-in-stack");
    expect(text).not.toContain("\n    at ");
    expect(text).not.toMatch(/http-server\.(ts|js)/);
    // Proper JSON-RPC error envelope. The id is intentionally null because
    // authentication failed before the untrusted request body was parsed.
    const body = JSON.parse(text);
    expect(body.jsonrpc).toBe("2.0");
    expect(body.error).toBeTruthy();
    expect(body.id).toBeNull();
  });

  it("publicUrl override drives both protected-resource metadata and WWW-Authenticate", async () => {
    // Local-dev shape: publicUrl set to http://localhost:8765 so the
    // resource/resource_metadata values reflect the externally-
    // reachable URL exactly (the DNS-rebinding allowlist still gates
    // /mcp on the Host header — local dev adds 127.0.0.1 there too).
    // Also pins the advertised "agent" scope so the consent UI's scope-list
    // aligns with the e2a backend (Slice 5b retired the lone "mcp" scope;
    // MCP clients connect as public DCR clients, capped at scope=agent).
    await close();
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      publicUrl: "http://localhost:8765",
      clientFactory: () => stub,
    });
    close = c;

    // Discovery: the resource/scope/auth-server fields all reflect
    // publicUrl + the backend's advertised scope.
    const disc = await fetch(`http://127.0.0.1:${port}/.well-known/oauth-protected-resource`);
    expect(disc.status).toBe(200);
    const meta = await disc.json();
    expect(meta.resource).toBe("http://localhost:8765");
    expect(meta.scopes_supported).toEqual(["agent", "account"]);

    // 401 on /mcp without bearer: WWW-Authenticate's resource_metadata
    // URL must use publicUrl, not "https://127.0.0.1:port".
    const unauth = await fetch(`http://127.0.0.1:${port}/mcp`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json, text/event-stream" },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "initialize",
        params: { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "x", version: "0" } },
      }),
    });
    expect(unauth.status).toBe(401);
    expect(unauth.headers.get("www-authenticate")).toContain(
      `resource_metadata="http://localhost:8765/.well-known/oauth-protected-resource"`,
    );
  });

  it("discovery endpoint 421s on spoofed Host", async () => {
    // Re-bind the server with a strict allowlist so a request to
    // /.well-known/... over 127.0.0.1 is rejected.
    await close();
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["api.e2a.dev"],
      clientFactory: () => stub,
    });
    close = c;
    const res = await fetch(
      `http://127.0.0.1:${port}/.well-known/oauth-protected-resource`,
    );
    expect(res.status).toBe(421);
  });

  it("rejects requests when Host is not in the SDK allowlist", async () => {
    // Tear down the loopback-friendly server and bring up one with a strict
    // allowlist so the SDK's DNS-rebinding guard fires on a normal request.
    await close();
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["api.e2a.dev"], // 127.0.0.1 not allowed
      clientFactory: () => stub,
    });
    close = c;
    const res = await fetch(`http://127.0.0.1:${port}/mcp`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        Authorization: "Bearer e2a_test",
      },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "initialize",
        params: { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "x", version: "0" } },
      }),
    });
    expect(res.status).toBe(421);
  });

  // ── Correlation ids, structured logging, bounded probe ──────────────

  const initializeBody = {
    jsonrpc: "2.0",
    id: 1,
    method: "initialize",
    params: { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "x", version: "0" } },
  };

  function postMcp(body: unknown, headers: Record<string, string> = {}) {
    return fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        ...headers,
      },
      body: JSON.stringify(body),
    });
  }

  type CapturedEvent = Record<string, unknown> & {
    severity: string;
    event: string;
    message: string;
  };

  function makeLogCapture(): { events: CapturedEvent[]; logger: Logger } {
    const events: CapturedEvent[] = [];
    const logger: Logger = (severity, event, message, fields = {}) => {
      events.push({ severity, event, message, ...fields });
    };
    return { events, logger };
  }

  // restart replaces the shared server with one built from the given opts
  // (mirrors the ad-hoc teardown pattern used by the tests above).
  async function restart(opts: Partial<HttpServerOptions> = {}) {
    await close();
    const { close: c, port } = await startHttpServer(0, {
      baseUrl: "http://e2a.local",
      allowedHosts: ["127.0.0.1", "localhost"],
      clientFactory: () => stub,
      logger: () => {},
      ...opts,
    });
    close = c;
    url = `http://127.0.0.1:${port}/mcp`;
  }

  describe("correlation ids", () => {
    it("honors a valid inbound X-Request-Id on the header and the 401 error body", async () => {
      const res = await postMcp(initializeBody, { "X-Request-Id": "abc-DEF_123" });
      expect(res.status).toBe(401);
      expect(res.headers.get("x-request-id")).toBe("abc-DEF_123");
      const body = await res.json();
      expect(body.error.message).toBe("missing bearer token"); // unchanged
      expect(body.error.data).toEqual({ request_id: "abc-DEF_123" });
    });

    it("mints mcpreq_<12 hex> when the inbound id has invalid characters", async () => {
      const res = await postMcp(initializeBody, { "X-Request-Id": "bad id with spaces!" });
      expect(res.status).toBe(401);
      const id = res.headers.get("x-request-id");
      expect(id).toMatch(/^mcpreq_[0-9a-f]{12}$/);
      const body = await res.json();
      expect(body.error.data.request_id).toBe(id);
    });

    it("mints a fresh id when no inbound id is present", async () => {
      const res = await postMcp(initializeBody);
      expect(res.headers.get("x-request-id")).toMatch(/^mcpreq_[0-9a-f]{12}$/);
    });

    it("sets X-Request-Id on 405 responses with request_id in the body", async () => {
      const res = await fetch(url, { method: "GET" });
      expect(res.status).toBe(405);
      const id = res.headers.get("x-request-id");
      expect(id).toMatch(/^mcpreq_[0-9a-f]{12}$/);
      const body = await res.json();
      expect(body.error.message).toMatch(/stateless/); // unchanged
      expect(body.error.data).toEqual({ request_id: id });
    });

    it("sets X-Request-Id on 500 responses with request_id in the body", async () => {
      await restart({
        clientFactory: () => {
          throw new Error("synthetic factory failure");
        },
      });
      const res = await postMcp(initializeBody, {
        Authorization: "Bearer e2a_test",
        "X-Request-Id": "err-500-correlation",
      });
      expect(res.status).toBe(500);
      expect(res.headers.get("x-request-id")).toBe("err-500-correlation");
      const body = await res.json();
      expect(body.error.message).toBe("internal server error"); // unchanged
      expect(body.error.data).toEqual({ request_id: "err-500-correlation" });
    });

    it("includes request_id in the invalid_token challenge body (expired credential)", async () => {
      const invalidStub = makeStubClient();
      invalidStub.whoami = vi.fn(async () => {
        throw makeHttpError(401);
      }) as McpClient["whoami"];
      await restart({ clientFactory: () => invalidStub });
      const res = await postMcp(initializeBody, {
        Authorization: "Bearer bogus_token",
        "X-Request-Id": "expired-cred-1",
      });
      expect(res.status).toBe(401);
      expect(res.headers.get("www-authenticate")).toMatch(/error="invalid_token"/);
      expect(res.headers.get("x-request-id")).toBe("expired-cred-1");
      const body = await res.json();
      expect(body.error.message).toBe("invalid bearer token"); // unchanged
      expect(body.error.data).toEqual({ request_id: "expired-cred-1" });
    });
  });

  describe("structured request logging", () => {
    it("emits auth_resolution + http_request events sharing one request_id", async () => {
      const { events, logger } = makeLogCapture();
      await restart({ logger });
      const res = await postMcp(
        { jsonrpc: "2.0", id: 1, method: "tools/list" },
        { Authorization: "Bearer e2a_test" },
      );
      expect(res.status).toBe(200);
      await res.text();

      const auth = events.find((e) => e.event === "auth_resolution");
      expect(auth).toMatchObject({ severity: "INFO", result: "resolved", scope: "account" });
      expect(typeof auth!.duration_ms).toBe("number");
      expect(auth!.request_id).toBe(res.headers.get("x-request-id"));

      const httpEvent = events.find((e) => e.event === "http_request");
      expect(httpEvent).toMatchObject({
        severity: "INFO",
        route: "mcp",
        method: "POST",
        status: 200,
      });
      expect(typeof httpEvent!.duration_ms).toBe("number");
      expect(httpEvent!.request_id).toBe(res.headers.get("x-request-id"));
      // The bearer itself must never appear in any log field.
      expect(JSON.stringify(events)).not.toContain("e2a_test");
    });

    it("logs cache_hit on a repeat request from the same bearer", async () => {
      const { events, logger } = makeLogCapture();
      await restart({ logger });
      for (const id of [1, 2]) {
        const res = await postMcp(
          { jsonrpc: "2.0", id, method: "tools/list" },
          { Authorization: "Bearer repeat_token" },
        );
        expect(res.status).toBe(200);
        await res.text();
      }
      const results = events.filter((e) => e.event === "auth_resolution").map((e) => e.result);
      expect(results).toEqual(["resolved", "cache_hit"]);
    });

    it("emits tool_execution events for ok and error outcomes", async () => {
      const { events, logger } = makeLogCapture();
      const registry = new MetricsRegistry();
      await restart({ logger, metrics: registry });
      const { client, transport } = await connect();

      await client.callTool({ name: "list_agents", arguments: {} });
      vi.mocked(stub.listAgents).mockRejectedValueOnce(new Error("boom"));
      const failed = await client.callTool({ name: "list_agents", arguments: {} });
      expect((failed as { isError?: boolean }).isError).toBe(true);
      await transport.close();

      const toolEvents = events.filter((e) => e.event === "tool_execution");
      expect(toolEvents).toHaveLength(2);
      expect(toolEvents[0]).toMatchObject({
        severity: "INFO",
        tool: "list_agents",
        outcome: "ok",
      });
      expect(toolEvents[1]).toMatchObject({
        severity: "WARNING",
        tool: "list_agents",
        outcome: "error",
        error_code: "invalid_request",
      });
      // Each callTool is its own POST (stateless), so each carries its own
      // minted request id.
      expect(toolEvents[0]!.request_id).toMatch(/^mcpreq_[0-9a-f]{12}$/);
      expect(toolEvents[1]!.request_id).toMatch(/^mcpreq_[0-9a-f]{12}$/);
      expect(typeof toolEvents[0]!.duration_ms).toBe("number");
      // Same seam feeds the metric.
      const rendered = registry.render();
      expect(rendered).toContain('mcp_tool_executions_total{outcome="ok",tool="list_agents"} 1');
      expect(rendered).toContain('mcp_tool_executions_total{outcome="error",tool="list_agents"} 1');
    });

    it("emits terminal_error (structured, with request_id) on the 500 path", async () => {
      const { events, logger } = makeLogCapture();
      await restart({
        logger,
        clientFactory: () => {
          throw new Error("synthetic factory failure");
        },
      });
      const res = await postMcp(initializeBody, { Authorization: "Bearer e2a_test" });
      expect(res.status).toBe(500);
      await res.text();
      const terminal = events.find((e) => e.event === "terminal_error");
      expect(terminal).toMatchObject({ severity: "ERROR", error: "Error" });
      expect(terminal!.request_id).toBe(res.headers.get("x-request-id"));
    });

    it("logs the fail-closed fallback resolution as a WARNING", async () => {
      const { events, logger } = makeLogCapture();
      const downStub = makeStubClient();
      downStub.whoami = vi.fn(async () => {
        throw new Error("upstream 500");
      }) as McpClient["whoami"];
      await restart({ logger, clientFactory: () => downStub });
      const res = await postMcp(
        { jsonrpc: "2.0", id: 1, method: "tools/list" },
        { Authorization: "Bearer e2a_test" },
      );
      expect(res.status).toBe(200);
      await res.text();
      const auth = events.find((e) => e.event === "auth_resolution");
      expect(auth).toMatchObject({ severity: "WARNING", result: "fallback", scope: "agent" });
      expect(typeof auth!.duration_ms).toBe("number");
    });

    it("the default logger writes single-line JSON events to stderr", async () => {
      // Explicit undefined overrides restart's silent-logger default, so the
      // server falls back to its built-in stderr JSON logger.
      await restart({ logger: undefined });
      const lines: string[] = [];
      const spy = vi.spyOn(process.stderr, "write").mockImplementation((chunk) => {
        lines.push(String(chunk));
        return true;
      });
      try {
        const res = await postMcp(initializeBody); // 401 missing bearer
        expect(res.status).toBe(401);
        await res.text();
        const httpLine = lines.find((l) => l.includes('"http_request"'));
        expect(httpLine).toBeDefined();
        // Single line: trailing newline, no embedded newlines.
        expect(httpLine!.endsWith("\n")).toBe(true);
        expect(httpLine!.trimEnd()).not.toContain("\n");
        const parsed = JSON.parse(httpLine!);
        expect(parsed).toMatchObject({
          severity: "INFO",
          event: "http_request",
          route: "mcp",
          method: "POST",
          status: 401,
          request_id: res.headers.get("x-request-id"),
        });
        expect(typeof parsed.duration_ms).toBe("number");
      } finally {
        spy.mockRestore();
      }
    });

    it("meters but does not log successful probe/scrape routes; failures still log", async () => {
      const { events, logger } = makeLogCapture();
      const registry = new MetricsRegistry();
      // readyz probe fails deterministically → 503 (the loggable case).
      await restart({
        logger,
        metrics: registry,
        readyz: {
          fetcher: async () => {
            throw new Error("backend down");
          },
        },
      });
      const origin = url.replace("/mcp", "");

      for (const path of ["/healthz", "/metrics"]) {
        const res = await fetch(`${origin}${path}`);
        expect(res.status).toBe(200);
        await res.text(); // drain so the server-side finish (and log) has fired
      }
      const quiet = events.filter((e) => e.event === "http_request");
      expect(quiet).toEqual([]); // successful probe/scrape traffic is silent

      const readyz = await fetch(`${origin}/readyz`);
      expect(readyz.status).toBe(503);
      await readyz.text();
      const failure = events.filter((e) => e.event === "http_request");
      expect(failure).toHaveLength(1); // ...but failures on those routes log
      expect(failure[0]).toMatchObject({ route: "readyz", status: 503 });

      // Metrics counted every request, logged or not.
      const rendered = registry.render();
      expect(rendered).toContain('mcp_http_requests_total{route="healthz",status_class="2xx"} 1');
      expect(rendered).toContain('mcp_http_requests_total{route="readyz",status_class="5xx"} 1');
    });
  });

  describe("bounded whoami probe (resolveTimeoutMs)", () => {
    it("a hung whoami falls back fast (fail-closed, logged, metered) instead of holding the POST", async () => {
      const { events, logger } = makeLogCapture();
      const registry = new MetricsRegistry();
      const hungStub = {
        agentEmail: "",
        scope: "agent" as const,
        whoami: vi.fn(() => new Promise<never>(() => {})), // never settles
      } as unknown as McpClient;
      // 25ms bound: deterministic — the probe never resolves, so the request
      // can only complete via the timeout path. Asserts the old ~90s
      // (30s x 3 SDK attempts) worst case is gone.
      await restart({
        resolveTimeoutMs: 25,
        clientFactory: () => hungStub,
        logger,
        metrics: registry,
      });

      const started = Date.now();
      const res = await postMcp(
        { jsonrpc: "2.0", id: 1, method: "tools/list" },
        { Authorization: "Bearer e2a_test" },
      );
      expect(res.status).toBe(200); // fail-closed fallback still serves
      await res.text();
      expect(Date.now() - started).toBeLessThan(5_000);

      const auth = events.find((e) => e.event === "auth_resolution");
      expect(auth).toMatchObject({ severity: "WARNING", result: "fallback", scope: "agent" });
      expect(registry.render()).toContain('mcp_auth_resolutions_total{result="fallback"} 1');
    });
  });

  describe("backend recovery", () => {
    it("down → fallback served uncached; recovered → next request resolves the real scope", async () => {
      let down = true;
      const probe = {
        agentEmail: "",
        scope: "account" as const,
        whoami: vi.fn(async () => {
          if (down) throw new Error("upstream 500");
          return { user: "owner@example.com", scope: "account", agentEmail: undefined };
        }),
        listMessages: vi.fn(async () => ({ items: [], next_cursor: undefined })),
      } as unknown as McpClient;
      const factory = vi.fn(() => probe);
      await restart({ clientFactory: factory });

      const first = await postMcp(
        { jsonrpc: "2.0", id: 1, method: "tools/list" },
        { Authorization: "Bearer e2a_test" },
      );
      expect(first.status).toBe(200);
      await first.text();
      // Fallback: probe (bearer only), then per-request client pinned to the
      // least-privilege tier, and NOT cached.
      expect(factory.mock.calls[0]).toEqual(["e2a_test"]);
      expect(factory.mock.calls[1]).toEqual(["e2a_test", { scope: "agent" }]);

      down = false;
      const second = await postMcp(
        { jsonrpc: "2.0", id: 2, method: "tools/list" },
        { Authorization: "Bearer e2a_test" },
      );
      expect(second.status).toBe(200);
      await second.text();
      // The fallback didn't stick: re-probed, now resolved to the real scope.
      expect(probe.whoami).toHaveBeenCalledTimes(2);
      expect(factory.mock.calls[2]).toEqual(["e2a_test"]);
      expect(factory.mock.calls[3]).toEqual(["e2a_test", { scope: "account" }]);
    });
  });
});
