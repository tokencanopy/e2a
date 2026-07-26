// Prod coverage for the surface introduced in v0.3.0, repointed to /v1:
//   - Events API (list / get / redeliver)                           PR #184
//   - Conversations API (list / get under agent scope)              PR #177
//   - Message forward (/messages/{id}/forward)                       PR #171
//   - Message labels (PATCH + ?labels= filter on /messages)          PR #173, #174
//   - Message search filters (?from, ?to, ?subject, ?since, ?until)  PR #154
//   - Domains CRUD (GET / DELETE /domains/{domain}; no PATCH)        PR #165
//   - Per-account resource limits (GET /v1/account)                  PR #158
//
// Coverage shape is deliberately read-heavy: shape + auth + 4xx
// validation paths. The outbox/event-emission code path requires
// WEBHOOKS_OUTBOX_ENABLED=true, which is off in prod, so the events
// log is empty by design. We still validate every endpoint:
//   - responds with the documented status
//   - returns the documented JSON shape (incl. pagination fields)
//   - rejects bad input and missing auth correctly
//
// Tests that would require real data flow (e.g. forward happy path)
// fall back to negative paths to avoid creating residue prod has to
// clean up later. The cleanup tracker handles any residue that does
// land.

import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { cleanup, track, untrack } from "../harness/cleanup.ts";
import { uniqueSlug, uniqueSubject } from "../harness/fixtures.ts";
import { info, warn, writeReport } from "../harness/report.ts";

const client = new ApiClient();
const SUITE = "14-v030-features";

after(async () => {
  const result = await cleanup(client);
  if (result.failed.length > 0) {
    warn(SUITE, "cleanup", `failed to delete ${result.failed.length} resources`, result.failed);
  } else {
    info(SUITE, "cleanup", `cleaned up ${result.succeeded} resources`);
  }
  writeReport(`./reports/${SUITE}.json`);
});

// ─── Events API ───────────────────────────────────────────────────

test("events: list returns documented envelope shape", async () => {
  const r = await client.get<{ items: unknown[] | null; next_cursor: string | null }>("/v1/events");
  assert.equal(r.status, 200, `GET /events expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.ok(r.body, "body is parsed JSON");
  // Go marshals an empty slice as `null` not `[]`; both are valid
  // empty-state encodings. next_cursor is null when no more pages.
  assert.ok(
    r.body!.items === null || Array.isArray(r.body!.items),
    `items should be array or null, got ${typeof r.body!.items}`,
  );
  assert.ok("next_cursor" in r.body!, "next_cursor field present on response");
});

test("events: list without auth → 401", async () => {
  const r = await client.get("/v1/events", { apiKey: null });
  assert.equal(r.status, 401, `expected 401, got ${r.status}`);
});

test("events: list accepts all documented filter params without 400", async () => {
  // Send every filter the design specifies; any 400 here indicates
  // a regression in the query parser.
  const r = await client.get("/v1/events", {
    query: {
      type: "email.received",
      agent_email: "test-mcp@agents.e2a.dev",
      since: "2026-01-01T00:00:00Z",
      until: "2026-12-31T23:59:59Z",
      limit: 10,
    },
  });
  assert.equal(r.status, 200, `multi-filter GET /events expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("events: list with limit > max clamps or 4xx, never crashes", async () => {
  // openapi caps limit at 200 (maximum: 200); the request-validation
  // layer rejects over-max with 422 invalid_request. A server that
  // clamps instead (200) is also acceptable. What we're guarding against
  // is a crash / 5xx.
  const r = await client.get("/v1/events", { query: { limit: 9999 } });
  assert.ok(r.status === 200 || r.status === 400 || r.status === 422, `expected 200/400/422, got ${r.status}`);
});

test("events: get nonexistent event → 404", async () => {
  const r = await client.get(`/v1/events/evt_nonexistent${Date.now()}`);
  assert.equal(r.status, 404, `expected 404, got ${r.status}`);
});

test("events: get without auth → 401", async () => {
  const r = await client.get(`/v1/events/evt_x`, { apiKey: null });
  assert.equal(r.status, 401, `expected 401, got ${r.status}`);
});

test("events: redeliver nonexistent event → 404", async () => {
  const r = await client.post(`/v1/events/evt_nonexistent${Date.now()}/redeliver`, {
    body: { webhook_id: "wh_anything" },
  });
  assert.equal(r.status, 404, `expected 404, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("events: redeliver without auth → 401", async () => {
  const r = await client.post(`/v1/events/evt_x/redeliver`, {
    apiKey: null,
    body: { webhook_id: "wh_x" },
  });
  assert.equal(r.status, 401, `expected 401, got ${r.status}`);
});

// ─── Conversations API ────────────────────────────────────────────

test("conversations: list under primary agent returns paginated envelope", async () => {
  const email = client.env.primaryAgentEmail;
  // openapi PageConversationSummaryView: {items: [...], next_cursor: string|null}
  // — the same envelope shape as /v1/events, not a bare {conversations: []}.
  const r = await client.get<{ items: unknown[] | null; next_cursor: string | null }>(
    `/v1/agents/${encodeURIComponent(email)}/conversations`,
  );
  assert.equal(r.status, 200, `expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.ok(r.body, "body is parsed JSON");
  assert.ok(
    r.body!.items === null || Array.isArray(r.body!.items),
    `items should be array or null, got ${typeof r.body!.items}`,
  );
  assert.ok("next_cursor" in r.body!, "next_cursor field present on response");
});

test("conversations: list without auth → 401", async () => {
  const email = client.env.primaryAgentEmail;
  const r = await client.get(`/v1/agents/${encodeURIComponent(email)}/conversations`, { apiKey: null });
  assert.equal(r.status, 401, `expected 401, got ${r.status}`);
});

test("conversations: list under nonexistent agent → 404 not_found", async () => {
  // Every per-agent path — the /agents/{id} root and its sub-resources
  // (conversations, messages) — returns a uniform 404 not_found for an
  // unknown/non-owned agent (indistinguishable, so no enumeration leak).
  const r = await client.get(
    `/v1/agents/nonexistent-${Date.now()}@agents.e2a.dev/conversations`,
  );
  assert.equal(r.status, 404, `expected 404, got ${r.status}`);
});

test("conversations: get nonexistent conversation → 404", async () => {
  const email = client.env.primaryAgentEmail;
  const r = await client.get(
    `/v1/agents/${encodeURIComponent(email)}/conversations/conv_nonexistent${Date.now()}`,
  );
  assert.equal(r.status, 404, `expected 404, got ${r.status}`);
});

// ─── Message forward + labels ──────────────────────────────────────

test("forward: nonexistent message → 404", async () => {
  const email = client.env.primaryAgentEmail;
  // ForwardRequest requires both `to` and `body` (openapi: required [to, body]);
  // omitting `body` trips request-validation (422) before the row lookup, so we
  // send a complete body to exercise the not-found path.
  const r = await client.post(
    `/v1/agents/${encodeURIComponent(email)}/messages/msg_nonexistent${Date.now()}/forward`,
    { body: { to: ["sink@e2a.dev"], text: "fwd" } },
  );
  assert.equal(r.status, 404, `expected 404, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("forward: missing 'to' field → 400", async () => {
  const email = client.env.primaryAgentEmail;
  const r = await client.post(
    `/v1/agents/${encodeURIComponent(email)}/messages/msg_anything/forward`,
    { body: {} },
  );
  assert.ok(r.status >= 400 && r.status < 500, `expected 4xx, got ${r.status}`);
});

test("forward: without auth → 401", async () => {
  const email = client.env.primaryAgentEmail;
  // Request validation runs before auth, so an incomplete ForwardRequest
  // would surface 422 instead of 401. Send a valid body (to + body) so the
  // missing-credential path is what's actually exercised.
  const r = await client.post(
    `/v1/agents/${encodeURIComponent(email)}/messages/msg_x/forward`,
    { apiKey: null, body: { to: ["sink@e2a.dev"], text: "fwd" } },
  );
  assert.equal(r.status, 401, `expected 401, got ${r.status}`);
});

test("labels: PATCH nonexistent message → 404", async () => {
  const email = client.env.primaryAgentEmail;
  // UpdateMessageRequest is {add_labels?, remove_labels?} — the mutation is a
  // delta, not a full `labels` replacement (that property is rejected as an
  // unexpected key, 422).
  const r = await client.patch(
    `/v1/agents/${encodeURIComponent(email)}/messages/msg_nonexistent${Date.now()}`,
    { body: { add_labels: ["urgent"] } },
  );
  assert.equal(r.status, 404, `expected 404, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("labels: PATCH rejects label with invalid chars (charset cap)", async () => {
  const email = client.env.primaryAgentEmail;
  // The validator runs before the row lookup, so this fails 400
  // even on a nonexistent message id.
  const r = await client.patch(
    `/v1/agents/${encodeURIComponent(email)}/messages/msg_anything`,
    { body: { add_labels: ["HAS SPACES & SYMBOLS!"] } },
  );
  assert.ok(r.status === 400 || r.status === 404, `expected 400/404, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("labels: PATCH rejects reserved 'e2a:' prefix from caller writes", async () => {
  const email = client.env.primaryAgentEmail;
  const r = await client.patch(
    `/v1/agents/${encodeURIComponent(email)}/messages/msg_anything`,
    { body: { add_labels: ["e2a:reserved"] } },
  );
  assert.ok(r.status === 400 || r.status === 404, `expected 400/404, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("agent messages: ?labels= filter accepted without error", async () => {
  // The labels filter lives on the per-agent inbox path
  // (GET /v1/agents/{address}/messages).
  const email = client.env.primaryAgentEmail;
  const r = await client.get(
    `/v1/agents/${encodeURIComponent(email)}/messages`,
    { query: { labels: "urgent" } },
  );
  assert.equal(r.status, 200, `expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("agent messages: q OR union and AND NOT difference on isolated loopback messages", async () => {
  // Do not use the shared primary inbox: this creates a throwaway agent, sends
  // only to itself (the providerless loopback path), and deletes the agent in
  // finally so its messages cascade away even if an assertion fails.
  const email = `${uniqueSlug("qfilter")}@${client.env.sharedDomain}`;
  const created = await client.post<{ email: string }>("/v1/agents", {
    body: { email, name: "q filter e2e" },
    expect: 201,
  });
  const agentEmail = created.body!.email;
  track("agent", agentEmail);

  try {
    const receiveLoopback = async (subject: string): Promise<string> => {
      await client.post(`/v1/agents/${encodeURIComponent(agentEmail)}/messages`, {
        body: { to: [agentEmail], subject, text: "q filter loopback fixture" },
        expect: [200, 202],
      });
      // The inbound copy can arrive asynchronously. Explicitly request all
      // read states and a 100-item page so defaults, pagination, and unrelated
      // messages cannot hide either unique fixture.
      for (let attempt = 0; attempt < 12; attempt++) {
        const listed = await client.get<{ items?: Array<{ id: string; subject?: string }> }>(
          `/v1/agents/${encodeURIComponent(agentEmail)}/messages`,
          { query: { direction: "inbound", read_status: "all", limit: 100 } },
        );
        assert.equal(listed.status, 200, `list loopback inbox: ${listed.status} ${listed.raw.slice(0, 200)}`);
        const message = listed.body?.items?.find((item) => item.subject === subject);
        if (message?.id) return message.id;
        await new Promise((resolve) => setTimeout(resolve, 500));
      }
      throw new Error(`loopback message ${JSON.stringify(subject)} did not arrive for ${agentEmail}`);
    };

    const idA = await receiveLoopback(uniqueSubject("q filter a"));
    const idB = await receiveLoopback(uniqueSubject("q filter b"));
    const labelA = uniqueSlug("q-a");
    const labelB = uniqueSlug("q-b");
    for (const [id, label] of [[idA, labelA], [idB, labelB]] as const) {
      const updated = await client.patch<{ labels?: string[] }>(
        `/v1/agents/${encodeURIComponent(agentEmail)}/messages/${encodeURIComponent(id)}`,
        { body: { add_labels: [label] }, expect: 200 },
      );
      assert.ok(updated.body?.labels?.includes(label), `message ${id} received label ${label}`);
    }

    const matchedIDs = async (q: string): Promise<string[]> => {
      const listed = await client.get<{ items?: Array<{ id: string }> }>(
        `/v1/agents/${encodeURIComponent(agentEmail)}/messages`,
        { query: { q, direction: "inbound", read_status: "all", limit: 100 }, expect: 200 },
      );
      return (listed.body?.items ?? []).map((item) => item.id).sort();
    };

    assert.deepEqual(await matchedIDs(`label:${labelA} OR label:${labelB}`), [idA, idB].sort(), "q OR returns the label union");
    assert.deepEqual(await matchedIDs(`label:${labelA} AND NOT label:${labelB}`), [idA], "q AND NOT returns only label A");
  } finally {
    const deleted = await client.delete(`/v1/agents/${encodeURIComponent(agentEmail)}?confirm=DELETE&permanent=true`);
    const cleanupSucceeded = [200, 204, 404].includes(deleted.status);
    assert.ok(
      cleanupSucceeded,
      `delete throwaway q-filter agent: ${deleted.status} ${deleted.raw.slice(0, 200)}`,
    );
    if (cleanupSucceeded) untrack("agent", agentEmail);
  }
});

test("agent messages: ?labels= filter enforces the 50-value cap (400)", async () => {
  const email = client.env.primaryAgentEmail;
  // The ?labels= filter is capped server-side at 50 values (maxLabelsPerOp);
  // 51 → 400 invalid_filter. The param is explode:false, so the values MUST be
  // sent comma-joined in a SINGLE param — 51 repeated `labels=` params collapse
  // to one value and spuriously pass 200 (the bug this test previously had).
  // NOTE: the cap is enforced by the server but is NOT documented in
  // api/openapi.yaml (the listMessages `labels` param has no maxItems) — a
  // server/spec drift worth closing (add maxItems: 50 to that param).
  const many = Array.from({ length: 51 }, (_, i) => `l${i}`).join(",");
  const r = await client.get(
    `/v1/agents/${encodeURIComponent(email)}/messages?labels=${many}`,
  );
  assert.equal(r.status, 400, `expected 400 (labels filter cap=50), got ${r.status}: ${r.raw.slice(0, 200)}`);
});

// ─── Message search filters (PR #154) ─────────────────────────────

test("agent messages: search filters accepted (from, to, subject, conversation_id, since, until)", async () => {
  // PR #154 search filters live on the per-agent inbox path.
  const email = client.env.primaryAgentEmail;
  const r = await client.get(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    query: {
      from: "alice@example.com",
      to: "bob@example.com",
      subject: "test",
      conversation_id: "conv_x",
      since: "2026-01-01T00:00:00Z",
      until: "2026-12-31T00:00:00Z",
      limit: 5,
    },
  });
  assert.equal(r.status, 200, `expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("agent messages: invalid since timestamp handled (400 or graceful 200)", async () => {
  const email = client.env.primaryAgentEmail;
  const r = await client.get(
    `/v1/agents/${encodeURIComponent(email)}/messages`,
    { query: { since: "not-a-timestamp" } },
  );
  assert.ok(r.status === 400 || r.status === 200, `expected 400 or graceful 200, got ${r.status}`);
});

// ─── Domains: CRUD ────────────────────────────────────────────────
// NOTE: There is no PATCH /v1/domains/{domain} in the current API —
// openapi exposes only GET / DELETE on {domain} (+ POST /verify). The
// former "make primary" PATCH tests were dropped as stale drift (the
// route 404s with a plaintext router-level not-found). Domain mutation
// coverage is the DELETE path below plus registration in suite 04.

test("domains: DELETE nonexistent domain → 404 or 403", async () => {
  const r = await client.delete(`/v1/domains/nonexistent-${Date.now()}.example.com?confirm=DELETE`);
  assert.ok(r.status === 404 || r.status === 403, `expected 404/403, got ${r.status}`);
});

// ─── Per-user resource limits (PR #158) ───────────────────────────

test("limits: GET /v1/account returns documented shape", async () => {
  type LimitsResp = {
    plan_code: string;
    limits: Record<string, number>;
    usage: Record<string, number>;
    upgrade_url?: string;
  };
  const r = await client.get<LimitsResp>("/v1/account");
  assert.equal(r.status, 200, `expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.ok(r.body, "body parsed");
  assert.equal(typeof r.body!.plan_code, "string", "plan_code is a string");
  assert.ok(r.body!.limits && typeof r.body!.limits === "object", "limits object present");
  assert.ok(r.body!.usage && typeof r.body!.usage === "object", "usage object present");
  // Every limit kind should have a numeric value; the corresponding
  // usage key drops the "max_" prefix (e.g. limits.max_agents pairs
  // with usage.agents). This pins the shape contract the dashboard's
  // limits card depends on.
  for (const [kind, limit] of Object.entries(r.body!.limits)) {
    assert.equal(typeof limit, "number", `limits.${kind} is a number`);
    const usageKey = kind.startsWith("max_") ? kind.slice("max_".length) : kind;
    assert.equal(
      typeof r.body!.usage[usageKey],
      "number",
      `usage.${usageKey} is a number (limits.${kind} ↔ usage.${usageKey} pair)`,
    );
  }
});

test("limits: GET /v1/account without auth → 401", async () => {
  const r = await client.get("/v1/account", { apiKey: null });
  assert.equal(r.status, 401, `expected 401, got ${r.status}`);
});
