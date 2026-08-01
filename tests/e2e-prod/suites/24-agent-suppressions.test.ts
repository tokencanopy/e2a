import { test, after, afterEach } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { uniqueSlug } from "../harness/fixtures.ts";
import { track, cleanup } from "../harness/cleanup.ts";
import { warn, writeReport } from "../harness/report.ts";

// Black-box conformance for the agent-scoped suppression surface (beta):
// createAgentSuppression, listAgentSuppressions, deleteAgentSuppression.
// Unlike ACCOUNT suppressions (which only a real SES bounce/complaint can
// create, hence deleteSuppression's coverage-gate allowlist entry), the
// agent-scoped list has a manual-create API — so the full lifecycle IS
// black-box testable: create → list shows it → delete → list no longer
// shows it. Everything runs against fresh throwaway agents on the shared
// domain; no dependency on shared-account state.
const SUITE = "24-agent-suppressions";
const client = new ApiClient();

afterEach(async () => {
  const r = await cleanup(client);
  if (r.failed.length) warn(SUITE, "cleanup-after-each", `failed ${r.failed.length}`, r.failed);
});

interface AgentSuppressionView {
  agent_email: string;
  address: string;
  source: string;
  created_at: string;
  reason?: string;
}
interface PageAgentSuppressionView {
  items: AgentSuppressionView[];
  next_cursor: string | null;
}
interface DeleteSuppressionResult {
  deleted: boolean;
  address: string;
}
interface ErrorEnvelope {
  error?: { code?: string; message?: string; request_id?: string };
}

async function freshAgent(label: string): Promise<string> {
  const email = `${uniqueSlug(label)}@${client.env.sharedDomain}`;
  await client.post(`/v1/agents`, { body: { email, name: `suppr ${label}` }, expect: 201 });
  track("agent", email);
  return email;
}

function suppressionsPath(agent: string, address?: string): string {
  const base = `/v1/agents/${encodeURIComponent(agent)}/suppressions`;
  return address === undefined ? base : `${base}/${encodeURIComponent(address)}`;
}

// ---------------------------------------------------------------------------
// Full lifecycle: create → idempotent re-create → list shows it → delete →
// list no longer shows it. This is the happy-path 2xx coverage for all three
// operationIds.
// ---------------------------------------------------------------------------
test("lifecycle: create → list shows it → delete → gone", async () => {
  const agent = await freshAgent("suplc");
  const address = `blocked-${uniqueSlug("rcpt")}@example.com`;
  const reason = "conformance manual block";

  // createAgentSuppression → 200 AgentSuppressionView.
  const create = await client.post<AgentSuppressionView>(suppressionsPath(agent), {
    body: { address, reason },
    expect: 200,
  });
  const sp = create.body!;
  // AgentSuppressionView required: agent_email, address, source, created_at.
  assert.equal(sp.agent_email, agent, "view.agent_email echoes the owning agent");
  assert.equal(sp.address, address, "view.address echoes the suppressed recipient");
  assert.equal(sp.source, "manual", `API-created block has source=manual, got: ${sp.source}`);
  assert.ok(sp.created_at, "view.created_at present");
  assert.equal(sp.reason, reason, "view.reason echoes the request");

  // The spec documents create as idempotent — a byte-identical re-create must
  // succeed (200 on the same entry), not conflict.
  const again = await client.post<AgentSuppressionView>(suppressionsPath(agent), {
    body: { address, reason },
    expect: 200,
  });
  assert.equal(again.body?.address, address, "idempotent re-create returns the same entry");

  // listAgentSuppressions → PageAgentSuppressionView envelope containing it.
  const list = await client.get<PageAgentSuppressionView>(suppressionsPath(agent), {
    query: { limit: 100 },
    expect: 200,
  });
  assert.ok(Array.isArray(list.body?.items), "items is an array");
  assert.ok(
    list.body!.next_cursor === null || typeof list.body!.next_cursor === "string",
    "next_cursor is string|null (required by PageAgentSuppressionView)",
  );
  const listed = list.body!.items.find((s) => s.address === address);
  assert.ok(listed, "created suppression appears in the agent's list");
  assert.equal(listed!.agent_email, agent, "list items are self-describing (agent_email)");
  assert.equal(listed!.source, "manual", "listed entry keeps source=manual");

  // deleteAgentSuppression (?confirm=DELETE) → DeleteSuppressionResult.
  const del = await client.delete<DeleteSuppressionResult>(`${suppressionsPath(agent, address)}?confirm=DELETE`, {
    expect: 200,
  });
  assert.equal(del.body?.deleted, true, "deletion object has deleted:true");
  assert.equal(del.body?.address, address, "deletion object echoes the un-suppressed address");

  // Gone from the list.
  const afterDel = await client.get<PageAgentSuppressionView>(suppressionsPath(agent), { expect: 200 });
  assert.ok(
    !afterDel.body!.items.some((s) => s.address === address),
    "deleted suppression no longer appears in the list",
  );
});

// ---------------------------------------------------------------------------
// Normalization: the lookup key is NormalizeEmail'd (lower-cased), so a
// mixed-case create stores — and later matches — the canonical form.
// ---------------------------------------------------------------------------
test("create normalizes the address (mixed case → canonical lower-case)", async () => {
  const agent = await freshAgent("supnc");
  const canonical = `mixed-${uniqueSlug("rcpt")}@example.com`;
  const mixed = canonical.toUpperCase();

  const create = await client.post<AgentSuppressionView>(suppressionsPath(agent), {
    body: { address: mixed },
    expect: 200,
  });
  assert.equal(create.body?.address, canonical, "stored address is the normalized (lower-case) form");

  // Deleting by the canonical form removes the mixed-case-created entry.
  const del = await client.delete<DeleteSuppressionResult>(`${suppressionsPath(agent, canonical)}?confirm=DELETE`, {
    expect: 200,
  });
  assert.equal(del.body?.deleted, true, "delete by canonical form finds the normalized entry");
});

// ---------------------------------------------------------------------------
// Validation: a non-email address is rejected up front.
// ---------------------------------------------------------------------------
test("create: a syntactically invalid address returns 400 invalid_request", async () => {
  const agent = await freshAgent("supiv");
  const r = await client.post<ErrorEnvelope>(suppressionsPath(agent), {
    body: { address: "not-an-email-address" },
  });
  assert.equal(r.status, 400, `invalid address → 400, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.equal(r.body?.error?.code, "invalid_request", "error envelope code=invalid_request");
});

// ---------------------------------------------------------------------------
// Anti-enumeration: a missing (or foreign) agent is a uniform 404 not_found
// on every op of this surface.
// ---------------------------------------------------------------------------
test("unknown agent: all three ops return 404 not_found", async () => {
  const ghost = `${uniqueSlug("supgh")}@${client.env.sharedDomain}`; // never created

  const list = await client.get<ErrorEnvelope>(suppressionsPath(ghost));
  assert.equal(list.status, 404, `list on unknown agent → 404, got ${list.status}: ${list.raw.slice(0, 200)}`);
  assert.equal(list.body?.error?.code, "not_found");

  const create = await client.post<ErrorEnvelope>(suppressionsPath(ghost), {
    body: { address: "someone@example.com" },
  });
  assert.equal(create.status, 404, `create on unknown agent → 404, got ${create.status}: ${create.raw.slice(0, 200)}`);
  assert.equal(create.body?.error?.code, "not_found");

  const del = await client.delete<ErrorEnvelope>(`${suppressionsPath(ghost, "someone@example.com")}?confirm=DELETE`);
  assert.equal(del.status, 404, `delete on unknown agent → 404, got ${del.status}: ${del.raw.slice(0, 200)}`);
  assert.equal(del.body?.error?.code, "not_found");
});

test("delete: unknown address on an owned agent returns 404 not_found", async () => {
  const agent = await freshAgent("supna");
  const addr = `never-suppressed-${Date.now()}@example.com`;
  const r = await client.delete<ErrorEnvelope>(`${suppressionsPath(agent, addr)}?confirm=DELETE`);
  assert.equal(r.status, 404, `un-suppressing an unknown address → 404, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.equal(r.body?.error?.code, "not_found", "error envelope code=not_found");
});

// ---------------------------------------------------------------------------
// Isolation: this is the DEFINING semantic of an agent-scoped block — it binds
// to one exact sending agent, not to the account. Without this, the surface is
// indistinguishable from the account-wide list it deliberately is not.
// ---------------------------------------------------------------------------
test("isolation: a block on one agent does not leak to another agent", async () => {
  const [agentA, agentB] = [await freshAgent("supisoa"), await freshAgent("supisob")];
  const address = `shared-${uniqueSlug("rcpt")}@example.com`;

  await client.post(suppressionsPath(agentA), { body: { address }, expect: 200 });

  const listB = await client.get<PageAgentSuppressionView>(suppressionsPath(agentB), { expect: 200 });
  assert.ok(
    !listB.body!.items.some((s) => s.address === address),
    "agent B's list must not contain a block created on agent A",
  );

  // The same address is independently blockable on B, and the two entries are
  // distinct rows: removing B's must leave A's standing.
  await client.post(suppressionsPath(agentB), { body: { address }, expect: 200 });
  await client.delete(`${suppressionsPath(agentB, address)}?confirm=DELETE`, { expect: 200 });

  const listA = await client.get<PageAgentSuppressionView>(suppressionsPath(agentA), { expect: 200 });
  assert.ok(
    listA.body!.items.some((s) => s.address === address),
    "deleting agent B's block must not remove agent A's block on the same address",
  );

  // And the delete on B is not silently satisfied by A's row: it is gone now.
  const del = await client.delete<ErrorEnvelope>(`${suppressionsPath(agentB, address)}?confirm=DELETE`);
  assert.equal(del.status, 404, `re-deleting agent B's block → 404, got ${del.status}: ${del.raw.slice(0, 200)}`);
});

// ---------------------------------------------------------------------------
// Pagination: the list auto-grows with every unsubscribe, so the cursor is
// load-bearing for every client (dashboard, CLI, MCP all page through it). The
// envelope shape is asserted in the lifecycle test; this walks a REAL cursor
// and pins the server's agent-pinning guard — a cursor minted for one agent
// must not be replayable against another.
// ---------------------------------------------------------------------------
test("pagination: cursor walks the list and is pinned to its agent", async () => {
  const agent = await freshAgent("suppg");
  const addresses = [
    `page-a-${uniqueSlug("rcpt")}@example.com`,
    `page-b-${uniqueSlug("rcpt")}@example.com`,
    `page-c-${uniqueSlug("rcpt")}@example.com`,
  ];
  for (const address of addresses) {
    await client.post(suppressionsPath(agent), { body: { address }, expect: 200 });
  }

  // Walk one row at a time; every page must advance and never repeat a row.
  const seen: string[] = [];
  let cursor: string | undefined;
  for (let page = 0; page < addresses.length + 1; page += 1) {
    const query: Record<string, string | number> = { limit: 1 };
    if (cursor) query.cursor = cursor;
    const r = await client.get<PageAgentSuppressionView>(suppressionsPath(agent), { query, expect: 200 });
    assert.ok(r.body!.items.length <= 1, `limit=1 returns at most one row, got ${r.body!.items.length}`);
    for (const item of r.body!.items) seen.push(item.address);
    const next = r.body!.next_cursor;
    if (!next) break;
    assert.notEqual(next, cursor, "next_cursor must advance, or a client paging this list would loop forever");
    cursor = next;
  }
  assert.deepEqual(
    [...seen].sort(),
    [...addresses].sort(),
    "walking the cursor yields every row exactly once",
  );

  // A cursor is scoped to the agent that minted it (the handler compares the
  // cursor's agent against the route's). Replaying it on another owned agent
  // must be rejected rather than silently paging the wrong list.
  const other = await freshAgent("suppgo");
  const first = await client.get<PageAgentSuppressionView>(suppressionsPath(agent), {
    query: { limit: 1 },
    expect: 200,
  });
  const borrowed = first.body!.next_cursor;
  assert.ok(borrowed, "precondition: the seeded list is longer than one page");
  const replay = await client.get<ErrorEnvelope>(suppressionsPath(other), { query: { limit: 1, cursor: borrowed! } });
  assert.equal(replay.status, 400, `replayed cursor → 400, got ${replay.status}: ${replay.raw.slice(0, 200)}`);
  assert.equal(replay.body?.error?.code, "invalid_cursor", "error envelope code=invalid_cursor");
});

// ---------------------------------------------------------------------------
// Destructive-confirm guard: like every delete op, un-suppressing requires
// ?confirm=DELETE. Huma enforces it declaratively (required + enum), so the
// omission is a 422 BEFORE the handler runs — and the entry must survive.
// ---------------------------------------------------------------------------
test("delete: omitting ?confirm=DELETE is rejected and leaves the block intact", async () => {
  const agent = await freshAgent("supcf");
  const address = `confirm-${uniqueSlug("rcpt")}@example.com`;
  await client.post(suppressionsPath(agent), { body: { address }, expect: 200 });

  const noConfirm = await client.delete<ErrorEnvelope>(suppressionsPath(agent, address));
  assert.equal(noConfirm.status, 422, `delete without confirm → 422, got ${noConfirm.status}: ${noConfirm.raw.slice(0, 200)}`);

  const wrongConfirm = await client.delete<ErrorEnvelope>(`${suppressionsPath(agent, address)}?confirm=yes`);
  assert.equal(wrongConfirm.status, 422, `delete with a wrong confirm token → 422, got ${wrongConfirm.status}`);

  const list = await client.get<PageAgentSuppressionView>(suppressionsPath(agent), { expect: 200 });
  assert.ok(
    list.body!.items.some((s) => s.address === address),
    "a rejected delete must not remove the block",
  );
});

// ---------------------------------------------------------------------------
// Scope: the surface is account-administration — an agent-scoped credential
// must be rejected with 403 forbidden. Mirrors 19-account's key hygiene: the
// probe key is minted fresh and only THAT key is ever deleted.
// ---------------------------------------------------------------------------
test("scope: an agent-scoped credential is rejected with 403 forbidden", async () => {
  const agent = await freshAgent("supsc");
  const minted = await client.post<{ id: string; key: string }>("/v1/account/api-keys", {
    body: { name: `conf-24-agent-scope-${Date.now()}`, scope: "agent", agent_email: agent },
    expect: 201,
  });
  const agentKey = minted.body!.key;
  assert.notEqual(agentKey, client.env.apiKey, "probe key is distinct from the auth key");
  try {
    const r = await client.get<ErrorEnvelope>(suppressionsPath(agent), { apiKey: agentKey });
    assert.equal(r.status, 403, `agent-scoped list → 403, got ${r.status}: ${r.raw.slice(0, 200)}`);
    assert.equal(r.body?.error?.code, "forbidden", "error envelope code=forbidden");
  } finally {
    await client.delete(`/v1/account/api-keys/${encodeURIComponent(minted.body!.id)}?confirm=DELETE`);
  }
});

// ---------------------------------------------------------------------------
// Unauthenticated access — every op must reject with 401. As in 19-account:
// body/param validation runs BEFORE the auth gate, so the POST sends a
// well-formed body and the DELETE carries confirm=DELETE to make the 401
// observable.
// ---------------------------------------------------------------------------
test("unauth: every agent-suppression op rejects an unauthenticated caller with 401", async () => {
  const noAuth = { apiKey: null as null };
  const agent = `unauth-probe@${client.env.sharedDomain}`;

  const cases: Array<{ label: string; run: () => Promise<{ status: number; raw: string }> }> = [
    { label: "GET  …/suppressions", run: () => client.get(suppressionsPath(agent), noAuth) },
    {
      label: "POST …/suppressions",
      run: () => client.post(suppressionsPath(agent), { ...noAuth, body: { address: "unauth@example.com" } }),
    },
    {
      label: "DELETE …/suppressions/{address}",
      run: () => client.delete(`${suppressionsPath(agent, "unauth@example.com")}?confirm=DELETE`, noAuth),
    },
  ];
  for (const c of cases) {
    const r = await c.run();
    assert.equal(r.status, 401, `${c.label} unauthenticated → 401, got ${r.status}: ${r.raw.slice(0, 160)}`);
  }
});

after(async () => {
  await cleanup(client);
  await writeReport(`./reports/${SUITE}.json`);
});
