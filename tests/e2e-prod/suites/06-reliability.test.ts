import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { cleanup, track } from "../harness/cleanup.ts";
import { uniqueSlug, holdAllOutbound } from "../harness/fixtures.ts";
import { fail, info, warn, writeReport } from "../harness/report.ts";

const client = new ApiClient();
const SUITE = "06-reliability";

let sharedAgentEmail = "";

before(async () => {
  // Single agent shared across reliability tests to avoid hitting the agent-creation rate limit.
  sharedAgentEmail = `${uniqueSlug("rel")}@${client.env.sharedDomain}`;
  // Track before the create, not after: a create that succeeds server-side but
  // whose response is lost (or malformed) would otherwise be invisible to
  // teardown, and a throw here skips the rest of this hook entirely.
  track("agent", sharedAgentEmail);
  const c = await client.post<{ email: string }>("/v1/agents", {
    body: { email: sharedAgentEmail, name: "rel-shared" },
  });
  if (c.status !== 201) {
    throw new Error(`shared-agent setup failed: ${c.status} ${c.raw.slice(0, 200)}`);
  }
  if (c.body?.email !== sharedAgentEmail) {
    throw new Error(`server echoed a different address: requested ${sharedAgentEmail}, got ${c.body?.email}`);
  }
});

after(async () => {
  const r = await cleanup(client);
  if (r.failed.length) warn(SUITE, "cleanup", `failed ${r.failed.length}`, r.failed);
  writeReport(`./reports/06-reliability.json`);
});

function wsUrl(email: string): string {
  const base = client.env.apiUrl.replace(/^http/, "ws");
  return `${base}/v1/agents/${encodeURIComponent(email)}/ws`;
}

function openWS(url: string, key?: string | null, timeoutMs = 5_000): Promise<WebSocket> {
  return new Promise((resolve, reject) => {
    // Auth is the `Authorization: Bearer` handshake header — the legacy
    // `?token=` query param was retired (the server 401s it). Node's global
    // WebSocket (undici) supports the non-standard `headers` init option.
    const opts = key ? { headers: { Authorization: `Bearer ${key}` } } : undefined;
    const ws = new WebSocket(url, opts as unknown as string[]);
    const t = setTimeout(() => {
      try {
        ws.close();
      } catch {}
      reject(new Error(`WS open timed out after ${timeoutMs}ms`));
    }, timeoutMs);
    ws.addEventListener("open", () => {
      clearTimeout(t);
      resolve(ws);
    });
    ws.addEventListener("error", (e: Event) => {
      clearTimeout(t);
      reject(new Error(`WS error: ${(e as ErrorEvent).message ?? "unknown"}`));
    });
  });
}

function waitClose(ws: WebSocket, timeoutMs = 3_000): Promise<{ code: number; reason: string }> {
  return new Promise((resolve) => {
    const t = setTimeout(() => resolve({ code: -1, reason: "timeout" }), timeoutMs);
    ws.addEventListener("close", (e: CloseEvent) => {
      clearTimeout(t);
      resolve({ code: e.code, reason: e.reason });
    });
  });
}

test("reliability: WS to own agent opens", async () => {
  const ws = await openWS(wsUrl(sharedAgentEmail), client.env.apiKey, 5_000);
  assert.equal(ws.readyState, 1, "WS readyState should be OPEN");
  ws.close();
  await waitClose(ws);
});

test("reliability: WS without api_key fails to open", async () => {
  let opened = false;
  try {
    const ws = await openWS(wsUrl(sharedAgentEmail), null, 3_000);
    opened = true;
    try { ws.close(); } catch {}
  } catch {
    // expected — rejection is correct
  }
  if (opened) {
    fail(SUITE, "ws-unauth-open", "WS opened without auth — should have rejected");
    assert.fail("WS without api_key opened successfully — auth gate broken");
  }
  info(SUITE, "ws-unauth-rejected", "WS without api_key correctly rejected");
});

test("reliability: WS with wrong api_key fails to open", async () => {
  let opened = false;
  try {
    const ws = await openWS(wsUrl(sharedAgentEmail), "e2a_00000000000000000000000000000000", 3_000);
    opened = true;
    try { ws.close(); } catch {}
  } catch {
    // expected
  }
  if (opened) {
    fail(SUITE, "ws-badkey-open", "WS opened with bogus key — should have rejected");
    assert.fail("WS with bogus api_key opened successfully — auth gate broken");
  }
  info(SUITE, "ws-badkey-rejected", "WS with bogus api_key correctly rejected");
});

test("reliability: WS to non-owned agent fails to open", async () => {
  let opened = false;
  try {
    const ws = await openWS(wsUrl("nobody@example.com"), client.env.apiKey, 3_000);
    opened = true;
    try { ws.close(); } catch {}
  } catch {
    // expected
  }
  if (opened) {
    fail(SUITE, "ws-cross-tenant", "WS opened against non-owned agent — cross-tenant break");
    assert.fail("WS opened against an unowned agent — cross-tenant guard broken");
  }
  info(SUITE, "ws-cross-tenant-rejected", "WS to non-owned agent correctly rejected");
});

test("reliability: WS reconnect cycle (open → close → open) works", async () => {
  const first = await openWS(wsUrl(sharedAgentEmail), client.env.apiKey, 5_000);
  first.close();
  await waitClose(first);
  await new Promise((r) => setTimeout(r, 250));
  const second = await openWS(wsUrl(sharedAgentEmail), client.env.apiKey, 5_000);
  assert.equal(second.readyState, 1, "reconnect should succeed");
  second.close();
  await waitClose(second);
});

test("reliability: second WS session supersedes the first — close 4000 'replaced'", async () => {
  // Frozen close-code contract (docs/api.md "Connection lifecycle & close
  // codes"): the server holds ONE connection per agent. A newer connection
  // wins; the superseded one is closed with the application code 4000 and the
  // stable reason token "replaced" — NOT 1008, which would read as a genuine
  // policy rejection (and, pre-contract, made auto-reconnecting SDKs steal
  // the socket back from each other in a loop).
  const first = await openWS(wsUrl(sharedAgentEmail), client.env.apiKey, 5_000);
  const firstClosed = waitClose(first, 5_000);
  const second = await openWS(wsUrl(sharedAgentEmail), client.env.apiKey, 5_000);
  assert.equal(second.readyState, 1, "newer WS session should open");

  const closed = await firstClosed;
  assert.equal(closed.code, 4000, `superseded WS must close with 4000, got ${closed.code} "${closed.reason}"`);
  assert.equal(closed.reason, "replaced", `close reason must be the stable token "replaced", got "${closed.reason}"`);
  info(SUITE, "ws-single-session", "newer connection won; older closed 4000 'replaced'");

  try { second.close(); } catch {}
  await waitClose(second);
});

test("reliability: idempotent protection PUT — applying same payload twice yields same state", async () => {
  const first = await holdAllOutbound(client, sharedAgentEmail);
  const second = await holdAllOutbound(client, sharedAgentEmail);
  assert.equal(first.status, 200);
  assert.equal(second.status, 200);
  const g = await client.get<{ outbound: { gate: { policy: string; action: string } } }>(
    `/v1/agents/${encodeURIComponent(sharedAgentEmail)}/protection`,
  );
  assert.equal(g.body?.outbound?.gate?.action, "review");
  assert.equal(g.body?.outbound?.gate?.policy, "allowlist");
});

test("reliability: server timestamps are RFC3339-parseable", async () => {
  const r = await client.get<{ created_at: string }>(`/v1/agents/${encodeURIComponent(sharedAgentEmail)}`);
  assert.ok(r.body?.created_at);
  const t = new Date(r.body!.created_at).valueOf();
  assert.ok(!Number.isNaN(t), `created_at should parse as date: ${r.body?.created_at}`);
  assert.ok(t > 0 && t < Date.now() + 60_000, "created_at is plausibly recent");
});

test("reliability: GET /messages with bogus cursor returns 4xx, not 500", async () => {
  const r = await client.get(`/v1/agents/${encodeURIComponent(sharedAgentEmail)}/messages`, {
    query: { limit: 5, cursor: "completely-invalid-token-xx" },
  });
  if (r.status >= 500) {
    fail(SUITE, "pagination-500", `bogus cursor caused ${r.status}: ${r.raw.slice(0, 200)}`);
  }
  assert.ok(r.status < 500, `expected <500, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("reliability: GET /messages with negative limit returns 4xx, not 500", async () => {
  const r = await client.get(`/v1/agents/${encodeURIComponent(sharedAgentEmail)}/messages`, { query: { limit: -5 } });
  if (r.status >= 500) {
    fail(SUITE, "negative-limit-500", `negative limit caused ${r.status}: ${r.raw.slice(0, 200)}`);
  }
  assert.ok(r.status < 500, `expected <500, got ${r.status}`);
});

test("reliability: GET /messages with absurdly large limit returns 4xx or capped result, not 500", async () => {
  const r = await client.get<{ items: unknown[] }>(`/v1/agents/${encodeURIComponent(sharedAgentEmail)}/messages`, { query: { limit: 1_000_000 } });
  if (r.status >= 500) {
    fail(SUITE, "huge-limit-500", `huge limit caused ${r.status}: ${r.raw.slice(0, 200)}`);
  }
  assert.ok(r.status < 500, `expected <500, got ${r.status}`);
  if (r.status === 200 && Array.isArray(r.body?.items) && r.body!.items.length > 1000) {
    info(SUITE, "limit-uncapped", `server returned ${r.body!.items.length} items for limit=1M — no server-side cap`);
  } else if (r.status === 200) {
    info(SUITE, "limit-capped", `server capped result to ${r.body?.items.length ?? "?"} items for limit=1M`);
  }
});
