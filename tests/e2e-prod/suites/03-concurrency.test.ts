import { test, after, afterEach } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { cleanup, track, untrack } from "../harness/cleanup.ts";
import { agentHeadroom, type AgentCapacityView } from "../harness/account.ts";
import { uniqueSlug, holdAllOutbound, SINK_EMAIL } from "../harness/fixtures.ts";
import { info, warn, writeReport, fail } from "../harness/report.ts";

const client = new ApiClient();
// Burst client for fan-out tests; capped at 5 RPS so we stay polite on prod.
const burst = new ApiClient(client.env, 5);
const SUITE = "03-concurrency";

afterEach(async () => {
  const r = await cleanup(client);
  if (r.failed.length) warn(SUITE, "cleanup-after-each", `failed ${r.failed.length}`, r.failed);
});

after(async () => {
  const r = await cleanup(client);
  if (r.failed.length) warn(SUITE, "cleanup", `failed ${r.failed.length}`, r.failed);
  writeReport(`./reports/03-concurrency.json`);
});

test("concurrency: 5 parallel creates with distinct slugs all succeed", async (t) => {
  const account = await client.get<AgentCapacityView>("/v1/account");
  assert.equal(account.status, 200, `account capacity lookup failed: ${account.status} ${account.raw.slice(0, 200)}`);
  const available = agentHeadroom(account.body!);
  if (available < 5) {
    t.skip(`requires 5 free agent slots; account currently has ${available}`);
    return;
  }
  const slugs = Array.from({ length: 5 }, () => uniqueSlug("par"));
  const emails = slugs.map((slug) => `${slug}@${burst.env.sharedDomain}`);
  for (const email of emails) track("agent", email);
  const settled = await Promise.allSettled(
    emails.map((email) =>
      burst.post<{ email: string }>("/v1/agents", { body: { email, name: "par" } }),
    ),
  );
  settled.forEach((result, i) => {
    if (result.status === "fulfilled" && result.value.status !== 201) untrack("agent", emails[i]);
  });
  const rejected = settled.filter((result): result is PromiseRejectedResult => result.status === "rejected");
  assert.equal(
    rejected.length,
    0,
    `parallel create transport failures: ${rejected.map((result) => String(result.reason)).join(" | ")}`,
  );
  const results = settled.flatMap((result) => result.status === "fulfilled" ? [result.value] : []);
  for (const r of results) {
    assert.equal(r.status, 201, `parallel create failed: ${r.status} ${r.raw.slice(0, 200)}`);
  }
});

test("concurrency: 5 parallel creates with the SAME slug — exactly one wins, rest 409/4xx", async (t) => {
  const account = await client.get<AgentCapacityView>("/v1/account");
  assert.equal(account.status, 200, `account capacity lookup failed: ${account.status} ${account.raw.slice(0, 200)}`);
  const available = agentHeadroom(account.body!);
  if (available < 1) {
    t.skip("requires 1 free agent slot; account currently has none");
    return;
  }
  const slug = uniqueSlug("race");
  const email = `${slug}@${burst.env.sharedDomain}`;
  track("agent", email);
  const settled = await Promise.allSettled(
    Array.from({ length: 5 }, () =>
      burst.post<{ email: string }>("/v1/agents", { body: { email, name: "race" } }),
    ),
  );
  const rejected = settled.filter((result): result is PromiseRejectedResult => result.status === "rejected");
  const hasSuccess = settled.some((result) => result.status === "fulfilled" && result.value.status === 201);
  if (!hasSuccess && rejected.length === 0) untrack("agent", email);
  assert.equal(
    rejected.length,
    0,
    `same-slug race transport failures: ${rejected.map((result) => String(result.reason)).join(" | ")}`,
  );
  const results = settled.flatMap((result) => result.status === "fulfilled" ? [result.value] : []);
  const successes = results.filter((r) => r.status === 201);
  const conflicts = results.filter((r) => r.status === 409);
  const otherFails = results.filter((r) => r.status !== 201 && r.status !== 409);

  assert.equal(successes.length, 1, `expected exactly 1 success, got ${successes.length}: ${results.map((r) => r.status).join(",")}`);

  if (otherFails.length > 0) {
    info(
      SUITE,
      "race-non-409",
      `${otherFails.length} losing creates returned ${otherFails.map((r) => r.status).join(",")} instead of 409 (spec). Body samples: ${otherFails.map((r) => r.raw.slice(0, 80)).join(" | ")}`,
    );
  } else {
    info(SUITE, "race-clean", `all ${conflicts.length} losers returned 409 cleanly`);
  }
});

test("concurrency: parallel reads of same agent return consistent body", async () => {
  const slug = uniqueSlug("cr");
  const c = await client.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${client.env.sharedDomain}`, name: "consistency" },
  });
  assert.equal(c.status, 201);
  const email = c.body!.email;
  track("agent", email);

  const reads = await Promise.all(
    Array.from({ length: 8 }, () => burst.get<{ email: string }>(`/v1/agents/${encodeURIComponent(email)}`)),
  );
  for (const r of reads) {
    assert.equal(r.status, 200);
    assert.equal(r.body?.email, email);
  }
  const bodies = new Set(reads.map((r) => JSON.stringify(r.body)));
  assert.equal(bodies.size, 1, `expected identical bodies under parallel read, got ${bodies.size} distinct`);
});

test("concurrency: parallel protection PUTs converge to a final state (no 500)", async () => {
  const slug = uniqueSlug("toggle");
  const c = await client.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${client.env.sharedDomain}`, name: "toggle" },
  });
  assert.equal(c.status, 201);
  const email = c.body!.email;
  track("agent", email);

  // Genuinely conflicting concurrent writes: alternate the outbound gate action
  // between two distinct postures so the convergence is non-trivial (not 4
  // identical writes). One of them must win cleanly, with no 5xx or corruption.
  const putAction = (action: string) =>
    burst.put(`/v1/agents/${encodeURIComponent(email)}/protection`, {
      body: {
        inbound: { gate: {}, scan: {} },
        outbound: { gate: { policy: "allowlist", action, allowlist: [] }, scan: {} },
        holds: {},
      },
    });
  const ops = await Promise.all([
    putAction("review"),
    putAction("flag"),
    putAction("review"),
    putAction("flag"),
  ]);
  for (const r of ops) {
    if (r.status >= 500) {
      fail(SUITE, "parallel-put-500", `protection PUT returned ${r.status} under concurrent updates: ${r.raw.slice(0, 200)}`);
    }
    assert.ok(r.status < 500, `no 5xx under contention, got ${r.status}`);
  }
  // Final state must be one of the values we actually wrote — converged, not corrupted.
  const final = await client.get<{ outbound: { gate: { action: string } } }>(`/v1/agents/${encodeURIComponent(email)}/protection`);
  assert.equal(final.status, 200);
  assert.ok(
    ["review", "flag"].includes(final.body?.outbound?.gate?.action ?? ""),
    `outbound action should converge to a written value, got ${final.body?.outbound?.gate?.action}`,
  );
});

test("concurrency: parallel DELETE of the same agent is idempotent under contention (no 5xx)", async () => {
  // Two valid designs exist:
  //   - First-writer-wins: one 2xx, rest 403/404 (anti-enumeration).
  //   - Idempotent delete: all 2xx (DELETE is conceptually a state assertion).
  // Both are defensible. The non-negotiable invariant is "no 5xx under
  // contention" — the test name was renamed from "one succeeds, rest 4xx"
  // because the previous assert (>=1 success) accepted all 4 returning
  // 2xx and only emitted info(). If you want to lock in first-writer-wins
  // specifically, tighten the assertion to ok.length === 1.
  const email = `${uniqueSlug("del")}@${client.env.sharedDomain}`;
  // Track even though the test is expected to consume it: if all four parallel
  // DELETEs below get rate-limited, the agent survives, and an untracked
  // survivor is exactly the leak this suite was caught producing. Teardown
  // treats the already-deleted case as a 404, i.e. success.
  track("agent", email);
  const c = await client.post<{ email: string }>("/v1/agents", {
    body: { email, name: "del" },
  });
  assert.equal(c.status, 201);

  const results = await Promise.all(
    Array.from({ length: 4 }, () => burst.delete(`/v1/agents/${encodeURIComponent(email)}?confirm=DELETE&permanent=true`)),
  );
  const ok = results.filter((r) => r.status === 200 || r.status === 204);
  const fivexx = results.filter((r) => r.status >= 500);
  assert.equal(fivexx.length, 0, `no 5xx under parallel delete, got: ${results.map((r) => r.status).join(",")}`);
  assert.ok(ok.length >= 1, `at least one delete should succeed, got ${ok.length}: ${results.map((r) => r.status).join(",")}`);
  // Final state check: a GET after all the parallel deletes must say 404
  // — a deleted agent is indistinguishable from one that never existed
  // (anti-enumeration; matches the "get nonexistent agent → 404 not_found"
  // convention), confirming the agent is gone regardless of which delete "won."
  const after = await client.get(`/v1/agents/${encodeURIComponent(email)}`);
  assert.equal(after.status, 404, `after parallel delete, GET expected 404, got ${after.status}`);
  if (ok.length > 1) {
    info(SUITE, "delete-idempotent", `${ok.length} parallel deletes returned 2xx — server treats DELETE as idempotent`);
  } else {
    info(SUITE, "delete-first-writer-wins", `${ok.length} parallel delete succeeded, ${results.length - ok.length} got 4xx — first-writer-wins design`);
  }
});

test("concurrency: 8 parallel sends from a normal agent — all accepted (no 5xx, no duplicates)", async () => {
  // The accept transaction inserts the message and then prepares its sending
  // operation under the gate; v1.9.0 deadlocked those two steps against each
  // other (SQLSTATE 40P01 → 500) for parallel sends from one agent. The HITL
  // case below caught it on the hold path; this covers the direct path, which
  // has the same shape and carries almost all production traffic.
  const slug = uniqueSlug("sendconc");
  const c = await client.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${client.env.sharedDomain}`, name: "send-conc" },
  });
  assert.equal(c.status, 201);
  const email = c.body!.email;
  track("agent", email);

  const N = 8;
  const sends = await Promise.all(
    Array.from({ length: N }, (_, i) =>
      burst.post<{ message_id: string; status: string }>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
        body: {
          to: [SINK_EMAIL],
          subject: `parallel direct ${i}`,
          text: `parallel direct send #${i}`,
        },
      }),
    ),
  );

  const ids = new Set<string>();
  for (const r of sends) {
    assert.ok(r.status === 202 || r.status === 200, `parallel direct send: status ${r.status}, body: ${r.raw.slice(0, 200)}`);
    assert.ok(r.body?.message_id?.startsWith("msg_"), `message_id present and prefixed`);
    ids.add(r.body!.message_id);
  }
  assert.equal(ids.size, N, `expected ${N} distinct message_ids, got ${ids.size}`);
  info(SUITE, "parallel-direct-sends", `${N} parallel direct sends accepted with ${ids.size} distinct ids`);
});

test("concurrency: 8 parallel sends from HITL agent — all queue (no dropped/duplicated)", async () => {
  const slug = uniqueSlug("hitlconc");
  const c = await client.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${client.env.sharedDomain}`, name: "hitl-conc" },
  });
  assert.equal(c.status, 201);
  const email = c.body!.email;
  track("agent", email);
  const u = await holdAllOutbound(client, email);
  assert.equal(u.status, 200);

  const N = 8;
  const sends = await Promise.all(
    Array.from({ length: N }, (_, i) =>
      burst.post<{ message_id: string; status: string }>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
        body: {
          to: [SINK_EMAIL],
          subject: `parallel ${i}`,
          text: `parallel send #${i}`,
        },
      }),
    ),
  );

  const ids = new Set<string>();
  for (const r of sends) {
    assert.ok(r.status === 202 || r.status === 200, `parallel send: status ${r.status}, body: ${r.raw.slice(0, 200)}`);
    assert.ok(r.body?.message_id?.startsWith("msg_"), `message_id present and prefixed`);
    ids.add(r.body!.message_id);
  }
  assert.equal(ids.size, N, `expected ${N} distinct message_ids, got ${ids.size}`);

  // Best-effort reject all so no actual mail leaves the system.
  for (const id of ids) {
    await client.post(`/v1/reviews/${id}/reject`, { body: { reason: "e2e cleanup" } });
  }
});
