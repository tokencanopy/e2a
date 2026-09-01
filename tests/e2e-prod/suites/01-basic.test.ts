import { test, after, afterEach } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { cleanup, track } from "../harness/cleanup.ts";
import { uniqueSlug, uniqueSubject, SINK_EMAIL, holdAllOutbound } from "../harness/fixtures.ts";
import { fail, info, warn, writeReport } from "../harness/report.ts";

const client = new ApiClient();
const SUITE = "01-basic";

afterEach(async () => {
  const result = await cleanup(client);
  if (result.failed.length > 0) {
    warn(SUITE, "cleanup-after-each", `failed to delete ${result.failed.length} resources`, result.failed);
  }
});

after(async () => {
  const result = await cleanup(client);
  if (result.failed.length > 0) {
    warn(SUITE, "cleanup", `failed to delete ${result.failed.length} resources`, result.failed);
  } else {
    info(SUITE, "cleanup", `cleaned up ${result.succeeded} resources`);
  }
  writeReport(`./reports/01-basic.json`);
});

test("info: public deployment metadata is returned", async () => {
  const r = await client.get<{ shared_domain: string; slug_registration_enabled: boolean; public_url: string }>(
    "/v1/info",
  );
  assert.equal(r.status, 200);
  assert.ok(r.body?.shared_domain, "shared_domain present");
  assert.ok(r.body?.public_url?.startsWith("http"), "public_url is a URL");
  assert.equal(typeof r.body?.slug_registration_enabled, "boolean");
});

test("agents: list returns user's agents with required fields", async () => {
  const r = await client.get<{ items: Array<{ email: string; domain: string }> }>(
    "/v1/agents",
  );
  assert.equal(r.status, 200);
  assert.ok(Array.isArray(r.body?.items));
  for (const a of r.body!.items) {
    assert.ok(a.email && a.email.includes("@"), `agent.email valid: ${a.email}`);
    assert.ok(a.domain, `agent.domain set: ${a.email}`);
  }
});

test("agents: get primary agent by email", async () => {
  const email = client.env.primaryAgentEmail;
  const r = await client.get<{ email: string; domain: string; domain_verified: boolean }>(
    `/v1/agents/${encodeURIComponent(email)}`,
  );
  assert.equal(r.status, 200);
  assert.equal(r.body?.email, email);
  assert.equal(typeof r.body?.domain_verified, "boolean");
});

test("agents: get nonexistent agent returns 404 not_found (matches every other resource; anti-enumeration preserved)", async () => {
  const r = await client.get<{ error?: { code?: string } }>(`/v1/agents/nonexistent-${Date.now()}@agents.e2a.dev`);
  assert.equal(r.status, 404, `unknown/non-owned agent → 404, got ${r.status}`);
  assert.equal(r.body?.error?.code, "not_found", `expected code not_found, got ${r.body?.error?.code}`);
});

test("agents: get malformed email returns 4xx", async () => {
  const r = await client.get(`/v1/agents/not-an-email`);
  assert.ok(r.status >= 400 && r.status < 500, `expected 4xx, got ${r.status}`);
});

test("agents: create + read + delete (slug on shared domain)", async () => {
  const slug = uniqueSlug();
  const create = await client.post<{ email: string; id: string; domain: string }>("/v1/agents", {
    body: { email: `${slug}@${client.env.sharedDomain}`, name: `e2e ${slug}` },
  });
  assert.equal(create.status, 201, `create slug-agent expected 201, got ${create.status}: ${create.raw.slice(0, 200)}`);
  assert.ok(create.body?.email, "create returns email");
  assert.equal(create.body?.domain, client.env.sharedDomain);
  const email = create.body!.email;
  track("agent", email);

  const got = await client.get<{ email: string; domain_verified: boolean }>(
    `/v1/agents/${encodeURIComponent(email)}`,
  );
  assert.equal(got.status, 200);
  assert.equal(got.body?.email, email);
  assert.equal(got.body?.domain_verified, true, "slug-domain agent should be auto-verified");

  const del = await client.delete<{ deleted: boolean; email: string; messages_deleted: number }>(
    `/v1/agents/${encodeURIComponent(email)}?confirm=DELETE&permanent=true`,
  );
  assert.equal(del.status, 200, `delete expected 200 + deletion receipt, got ${del.status}`);
  assert.equal(del.body?.deleted, true, "deletion receipt has deleted:true");
  assert.equal(del.body?.email, email, "deletion receipt echoes the agent email");
  assert.equal(typeof del.body?.messages_deleted, "number", "deletion receipt carries messages_deleted");

  const after = await client.get(`/v1/agents/${encodeURIComponent(email)}`);
  assert.equal(after.status, 404, `deleted agent should 404 not_found (anti-enumeration), got ${after.status}`);
});

test("agents: create with missing slug AND email returns 4xx", async () => {
  const r = await client.post("/v1/agents", { body: { name: "no identifier" } });
  assert.ok(r.status >= 400 && r.status < 500, `expected 4xx, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("agents: create with duplicate slug returns 409 (or 4xx)", async () => {
  const slug = uniqueSlug();
  const first = await client.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${client.env.sharedDomain}`, name: "first" },
  });
  assert.equal(first.status, 201);
  track("agent", first.body!.email);
  const second = await client.post("/v1/agents", { body: { email: `${slug}@${client.env.sharedDomain}`, name: "second" } });
  assert.ok(second.status >= 400 && second.status < 500, `dup slug expected 4xx, got ${second.status}`);
  if (second.status !== 409) {
    info(SUITE, "dup-slug-code", `spec documents 409 for "Agent already exists" but got ${second.status}: ${second.raw.slice(0, 120)}`);
  }
});

test("agents: hold-all-outbound via protection persists", async () => {
  const slug = uniqueSlug();
  const c = await client.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${client.env.sharedDomain}`, name: "update target" },
  });
  assert.equal(c.status, 201);
  const email = c.body!.email;
  track("agent", email);

  const upd = await holdAllOutbound(client, email);
  assert.equal(upd.status, 200, `protection update expected 200, got ${upd.status}: ${upd.raw.slice(0, 200)}`);

  const g = await client.get<{ outbound: { gate: { action: string } } }>(
    `/v1/agents/${encodeURIComponent(email)}/protection`,
  );
  assert.equal(g.body?.outbound?.gate?.action, "review", "outbound review gate persisted");
});

test("send: review-gated agent returns 202 pending_review", async () => {
  const slug = uniqueSlug("hitl");
  const c = await client.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${client.env.sharedDomain}`, name: "hitl" },
  });
  assert.equal(c.status, 201);
  const email = c.body!.email;
  track("agent", email);
  const u = await holdAllOutbound(client, email);
  assert.equal(u.status, 200);

  const send = await client.post<{ message_id: string; status: string }>(
    `/v1/agents/${encodeURIComponent(email)}/messages`,
    {
      body: {
        to: [SINK_EMAIL],
        subject: uniqueSubject("hitl pending"),
        text: "test body — should never go out, immediately rejected",
      },
    },
  );
  assert.ok(
    send.status === 202 || send.status === 200,
    `expected 202/200, got ${send.status}: ${send.raw.slice(0, 200)}`,
  );
  if (send.status !== 202) {
    fail(SUITE, "send-hitl-pending", `review-gated agent should yield 202 pending_review, got ${send.status}`, send.body);
    return;
  }
  assert.equal(send.body?.status, "pending_review", "status field should be pending_review");
  assert.ok(send.body?.message_id?.startsWith("msg_"), "message_id has msg_ prefix");

  const reject = await client.post(
    `/v1/reviews/${send.body!.message_id}/reject`,
    {
      body: { reason: "e2e test rejection" },
    },
  );
  assert.ok(reject.status === 200, `reject expected 200, got ${reject.status}: ${reject.raw.slice(0, 200)}`);
});

test("send: missing 'to' returns 4xx", async () => {
  const r = await client.post(`/v1/agents/${encodeURIComponent(client.env.primaryAgentEmail)}/messages`, {
    body: { subject: "no to", text: "hi" },
  });
  assert.ok(r.status >= 400 && r.status < 500, `expected 4xx, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("send: unowned sending agent returns 4xx (auth/scope guard)", async () => {
  // The sending agent is threaded through the path; sending as an agent the
  // caller doesn't own must be rejected (no cross-tenant impersonation).
  const r = await client.post(`/v1/agents/${encodeURIComponent("someone-else@example.com")}/messages`, {
    body: { to: [SINK_EMAIL], subject: "spoof", text: "hi" },
  });
  assert.ok(r.status >= 400 && r.status < 500, `expected 4xx (cannot send as non-owned), got ${r.status}`);
});

test("messages: list with limit + pagination cursor", async () => {
  const r = await client.get<{ items: unknown[]; next_cursor?: string | null }>(
    `/v1/agents/${encodeURIComponent(client.env.primaryAgentEmail)}/messages`,
    { query: { limit: 5 } },
  );
  assert.equal(r.status, 200);
  assert.ok(Array.isArray(r.body?.items));
  if (r.body!.items.length >= 5) {
    assert.ok(
      r.body!.next_cursor === undefined || r.body!.next_cursor === null || typeof r.body!.next_cursor === "string",
      "next_cursor is string|null|absent",
    );
  }
});

test("domains: list returns documented shape", async () => {
  const r = await client.get<{ items: Array<{ domain: string }> }>("/v1/domains");
  assert.equal(r.status, 200);
  assert.ok(Array.isArray(r.body?.items));
});
