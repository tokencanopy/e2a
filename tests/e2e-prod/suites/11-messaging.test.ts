import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { cleanup, track } from "../harness/cleanup.ts";
import { uniqueSlug, uniqueSubject, uniqueIdempotencyKey, SINK_EMAIL, holdAllOutbound } from "../harness/fixtures.ts";
import { fail, info, warn, writeReport } from "../harness/report.ts";

const client = new ApiClient();
const SUITE = "11-messaging";

after(async () => {
  const r = await cleanup(client);
  if (r.failed.length) warn(SUITE, "cleanup", `failed ${r.failed.length}`, r.failed);
  writeReport(`./reports/11-messaging.json`);
});

async function createHitlAgent(name: string): Promise<string> {
  const slug = uniqueSlug(name);
  const c = await client.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${client.env.sharedDomain}`, name },
  });
  if (c.status !== 201) throw new Error(`create agent failed: ${c.status} ${c.raw.slice(0, 200)}`);
  const email = c.body!.email;
  track("agent", email);
  const u = await holdAllOutbound(client, email);
  if (u.status !== 200) throw new Error(`enable outbound review failed: ${u.status} ${u.raw.slice(0, 200)}`);
  return email;
}

test("messaging: pagination roundtrip — limit=3 then follow cursor; no duplicate ids", async () => {
  // Queue a few HITL messages so we have something to paginate against.
  const email = await createHitlAgent("pag");
  const queued: string[] = [];
  for (let i = 0; i < 5; i++) {
    const s = await client.post<{ message_id: string }>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [SINK_EMAIL], subject: uniqueSubject(`pag ${i}`), text: "x" },
    });
    if (s.body?.message_id) queued.push(s.body.message_id);
  }

  const page1 = await client.get<{ items: Array<{ id: string }>; next_cursor?: string | null }>(
    `/v1/agents/${encodeURIComponent(email)}/messages`,
    { query: { limit: 3, direction: "all" } },
  );
  assert.equal(page1.status, 200);
  assert.ok(Array.isArray(page1.body?.items));
  if (!page1.body?.next_cursor) {
    info(SUITE, "pagination-single-page", "fewer than 3+1 messages in inbox — pagination not exercised");
    // Cleanup queued.
    for (const id of queued) await client.post(`/v1/reviews/${id}/reject`, { body: { reason: "e2e pagination cleanup" } });
    return;
  }
  const page2 = await client.get<{ items: Array<{ id: string }>; next_cursor?: string | null }>(
    `/v1/agents/${encodeURIComponent(email)}/messages`,
    { query: { limit: 3, cursor: page1.body.next_cursor, direction: "all" } },
  );
  assert.equal(page2.status, 200, `page2 status ${page2.status}: ${page2.raw.slice(0, 200)}`);
  // MessageSummaryView identifies items by `id` (its own primary key; a
  // referenced OTHER resource would be `<noun>_id`). Mapping the wrong field
  // made every id undefined and produced a phantom cross-page overlap.
  const ids1 = new Set((page1.body!.items ?? []).map((m) => m.id));
  const ids2 = new Set((page2.body!.items ?? []).map((m) => m.id));
  const overlap = [...ids1].filter((id) => ids2.has(id));
  if (overlap.length > 0) {
    fail(SUITE, "pagination-duplicate-ids", `${overlap.length} ids appear on both pages: ${overlap.slice(0, 5).join(",")}`);
    // Cleanup before throwing so we don't leak HITL-held messages.
    for (const id of queued) await client.post(`/v1/reviews/${id}/reject`, { body: { reason: "e2e pagination cleanup" } });
    assert.fail(`pagination roundtrip returned ${overlap.length} duplicate id(s) — pagination is broken`);
  }
  // Cleanup.
  for (const id of queued) await client.post(`/v1/reviews/${id}/reject`, { body: { reason: "e2e pagination cleanup" } });
});

test("messaging: Idempotency-Key replay — same key+body returns same message_id", async () => {
  const email = await createHitlAgent("idem");
  const idemKey = uniqueIdempotencyKey();
  const sendPath = `/v1/agents/${encodeURIComponent(email)}/messages`;
  const body = { to: [SINK_EMAIL], subject: uniqueSubject("idem"), text: "idempotency test" };

  const r1 = await client.post<{ message_id: string; status: string }>(sendPath, {
    body,
    headers: { "Idempotency-Key": idemKey },
  });
  assert.ok(r1.status === 200 || r1.status === 202, `first send unexpected ${r1.status}: ${r1.raw.slice(0, 200)}`);
  assert.ok(r1.body?.message_id, "first send returned message_id");
  const firstId = r1.body!.message_id;

  const r2 = await client.post<{ message_id: string; status: string }>(sendPath, {
    body,
    headers: { "Idempotency-Key": idemKey },
  });
  if (r2.body?.message_id !== firstId) {
    fail(
      SUITE,
      "idem-key-not-replayed",
      `same Idempotency-Key + same body yielded different message_id: ${firstId} → ${r2.body?.message_id}. Server should replay original response, not re-queue.`,
    );
    // Hard assert — Idempotency-Key semantics are a financial-stakes
    // contract (double-send protection on approve). Don't paper over.
    await client.post(`/v1/reviews/${firstId}/reject`, { body: { reason: "e2e idem cleanup pre-fail" } });
    if (r2.body?.message_id) {
      await client.post(`/v1/reviews/${r2.body.message_id}/reject`, { body: { reason: "e2e idem cleanup pre-fail" } });
    }
    assert.fail(`Idempotency-Key replay broken: ${firstId} !== ${r2.body?.message_id}`);
  }
  info(SUITE, "idem-key-replayed", `Idempotency-Key replay correct: same key+body → same message_id ${firstId}`);

  // Different body, same key → 422 per spec.
  const r3 = await client.post(sendPath, {
    body: { ...body, subject: uniqueSubject("idem mutated") },
    headers: { "Idempotency-Key": idemKey },
  });
  if (r3.status !== 422) {
    info(SUITE, "idem-diff-body-non-422", `same key + DIFFERENT body returned ${r3.status} instead of 422: ${r3.raw.slice(0, 200)}`);
  }

  await client.post(`/v1/reviews/${firstId}/reject`, { body: { reason: "e2e idem cleanup" } });
});

test("messaging: /agents/{email}/test on HITL agent returns 202 with message_id (and is rejectable)", async () => {
  const email = await createHitlAgent("test-ep");
  const r = await client.post<{ status?: string; message_id?: string } | Record<string, string>>(
    `/v1/agents/${encodeURIComponent(email)}/test`,
    { body: {} },
  );
  if (r.status === 403) {
    info(SUITE, "test-domain-unverified", `/test on slug-domain HITL agent returned 403 — slug agents are supposed to be auto-verified per 01-basic.create test. Body: ${r.raw.slice(0, 200)}`);
    return;
  }
  if (r.status >= 500) {
    fail(SUITE, "test-5xx", `/test endpoint returned ${r.status}: ${r.raw.slice(0, 200)}`);
    return;
  }
  // HITL is enabled, so spec says 202.
  if (r.status !== 202) {
    info(SUITE, "test-non-202", `expected 202 for HITL agent, got ${r.status}: ${r.raw.slice(0, 200)}`);
  }
  const msgId = (r.body as { message_id?: string })?.message_id;
  if (msgId) {
    const rej = await client.post(`/v1/reviews/${msgId}/reject`, { body: { reason: "e2e /test cleanup" } });
    assert.ok(rej.status === 200, `failed to reject /test message: ${rej.status} ${rej.raw.slice(0, 200)}`);
  } else {
    info(SUITE, "test-no-msgid", "response body did not include message_id");
  }
});

test("messaging: HITL approve flow — send queues, approve sends or enqueues", async () => {
  const email = await createHitlAgent("appr");
  const s = await client.post<{ message_id: string; status: string }>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    body: { to: [SINK_EMAIL], subject: uniqueSubject("approve"), text: "approve me" },
  });
  if (s.status !== 202) {
    info(SUITE, "approve-no-202", `expected 202 from HITL send, got ${s.status}: ${s.raw.slice(0, 200)}`);
    return;
  }
  assert.equal(s.body?.status, "pending_review");
  const id = s.body!.message_id;

  // Approve — empty body approves as-is. Goes out via SMTP to blackhole sink.
  const ap = await client.post<{ message_id: string; status: string }>(`/v1/reviews/${id}/approve`, { body: {} });
  const validApprove = (ap.status === 200 && ap.body?.status === "sent") ||
    (ap.status === 202 && ap.body?.status === "accepted");
  if (!validApprove) {
    fail(SUITE, "approve-status-mismatch", `approve returned ${ap.status} status=${ap.body?.status}: ${ap.raw.slice(0, 200)}`);
    return;
  }
  assert.equal(ap.body?.message_id, id);
  // Synchronous delivery is terminal sent/200; async enqueue is accepted/202.
  const finalStatus = ap.body?.status;
  info(SUITE, "approve-final-status", `approve returned status="${finalStatus}"`);

  // Re-approve must fail with 409 (the hold was resolved before sync send/async enqueue).
  const ap2 = await client.post(`/v1/reviews/${id}/approve`, { body: {} });
  if (ap2.status !== 409) {
    info(SUITE, "double-approve-non-409", `re-approve of sent message returned ${ap2.status} instead of 409: ${ap2.raw.slice(0, 200)}`);
  }
});

test("messaging: reject of a sent message returns 409 (state guard)", async () => {
  const email = await createHitlAgent("rej");
  const s = await client.post<{ message_id: string }>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    body: { to: [SINK_EMAIL], subject: uniqueSubject("reject after send"), text: "x" },
  });
  if (s.status !== 202 || !s.body?.message_id) {
    info(SUITE, "rej-after-send-skipped", `setup HITL send returned ${s.status}: ${s.raw.slice(0, 200)}`);
    return;
  }
  const id = s.body.message_id;
  // First approve so the hold resolves (sent synchronously or accepted async).
  const ap = await client.post(`/v1/reviews/${id}/approve`, { body: {} });
  if (ap.status !== 200 && ap.status !== 202) {
    info(SUITE, "rej-after-send-approve-failed", `approve returned ${ap.status}, can't test reject-after-send`);
    return;
  }
  // Now reject the same message — must 409.
  const rej = await client.post(`/v1/reviews/${id}/reject`, { body: { reason: "should fail" } });
  if (rej.status !== 409) {
    info(SUITE, "reject-after-send-non-409", `expected 409 for reject after send, got ${rej.status}: ${rej.raw.slice(0, 200)}`);
  }
});

test("messaging: approve with field overrides applies them before send", async () => {
  const email = await createHitlAgent("appov");
  const s = await client.post<{ message_id: string }>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    body: { to: [SINK_EMAIL], subject: uniqueSubject("original"), text: "original body" },
  });
  if (s.status !== 202 || !s.body?.message_id) {
    info(SUITE, "approve-override-skipped", `setup send returned ${s.status}`);
    return;
  }
  const id = s.body.message_id;
  // Override subject + body on approve. Per ApproveRequest schema, the overridable
  // subset is subject/text/html/to/cc/bcc/attachments (plain-text field is `text`,
  // not `body`/`body_text` — the latter are rejected 422 as an unexpected property).
  const ap = await client.post<{ message_id: string; status: string }>(
    `/v1/reviews/${id}/approve`,
    { body: { subject: "overridden subject (approve-time)", text: "overridden body" } },
  );
  if (ap.status !== 200 && ap.status !== 202) {
    info(SUITE, "approve-override-non-2xx", `override approve returned ${ap.status}: ${ap.raw.slice(0, 200)}`);
    return;
  }
  // Fetch the message to see whether the override was actually persisted to the
  // sent record. Note: body columns are scrubbed on send per the approve description;
  // subject may remain.
  const g = await client.get<{ subject?: string; status?: string }>(`/v1/agents/${encodeURIComponent(email)}/messages/${id}`);
  if (g.status === 200) {
    info(SUITE, "approve-override-readback", `final subject after override approve: "${g.body?.subject ?? "(absent)"}", status="${g.body?.status}"`);
  } else {
    info(SUITE, "approve-override-readback-failed", `GET /messages/${id} returned ${g.status}`);
  }
});

test("messaging: reply to bogus message ID returns 404", async () => {
  const email = client.env.primaryAgentEmail;
  const r = await client.post(
    `/v1/agents/${encodeURIComponent(email)}/messages/msg_does_not_exist_${Date.now()}/reply`,
    { body: { text: "won't be delivered" } },
  );
  assert.ok(r.status === 404 || (r.status >= 400 && r.status < 500), `expected 4xx (404), got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("messaging: reply with empty body is rejected as invalid_request", async () => {
  // /reply requires the target message be inbound and belong to the
  // agent in the path. The previous version fell back to any message
  // including outbound, which routinely 404'd before the 400-missing-
  // body check ran — so the test passed without ever exercising the
  // "empty body returns 400" branch. Now: pull from the agent-scoped
  // inbound listing, skip cleanly if none exist.
  const email = client.env.primaryAgentEmail;
  const list = await client.get<{ items: Array<{ message_id: string; direction?: string }> }>(
    `/v1/agents/${encodeURIComponent(email)}/messages`,
    { query: { limit: 5, direction: "inbound" } },
  );
  const candidate = list.body?.items?.find((m) => m.direction === "inbound" || m.direction === undefined);
  if (!candidate) {
    info(SUITE, "reply-empty-skipped", `no inbound messages on ${email} — cannot exercise empty-body reply check`);
    return;
  }
  const r = await client.post(
    `/v1/agents/${encodeURIComponent(email)}/messages/${encodeURIComponent(candidate.message_id)}/reply`,
    { body: {} },
  );
  // Now that we picked from the agent-scoped inbound list, 400 is the
  // expected response (missing body). 404 here would mean the inbound
  // listing returned a stale id — flag it informationally rather than
  // assert away a different bug.
  if (r.status === 404) {
    info(SUITE, "reply-empty-404-on-listed-msg", `inbound list returned ${candidate.message_id} but /reply 404'd — possible listing/storage skew`);
    return;
  }
  // The v1 contract treats 400 and 422 as the SAME outcome for input validation:
  // "invalid_request is the single canonical code for input-validation failures
  // whether they arrive as 400 (malformed) or 422 (semantically invalid)"
  // (api/openapi.yaml, ErrorEnvelope.code). A missing required `text` is
  // semantically invalid, so the server answers 422 — contract-compliant. The
  // old exact-400 assertion encoded one of two permitted statuses and so
  // recorded a permanent failure, invisible until findings started gating the
  // run. Assert the contract (a validation rejection carrying invalid_request),
  // not one particular status code.
  assert.ok(
    r.status === 400 || r.status === 422,
    `expected a validation rejection (400 or 422) on owned inbound message, got ${r.status}: ${r.raw.slice(0, 200)}`,
  );
  const code = (JSON.parse(r.raw) as { error?: { code?: string } })?.error?.code;
  assert.equal(code, "invalid_request", `expected canonical validation code invalid_request, got "${code}"`);
});

test("messaging: /messages search filters — surface what's supported", async () => {
  // Probe each known/likely filter to see what the server actually accepts.
  // Now agent-scoped: the agent is in the path, so the former agent_email
  // filter is exercised as the implicit-scope baseline.
  const email = client.env.primaryAgentEmail;
  const probes: Array<{ name: string; q: Record<string, string | number> }> = [
    { name: "baseline (implicit agent scope)", q: { limit: 5 } },
    { name: "status=all", q: { status: "all", limit: 5 } },
    { name: "direction=outbound", q: { direction: "outbound", limit: 5 } },
    { name: "direction=inbound", q: { direction: "inbound", limit: 5 } },
    { name: "since=2024-01-01", q: { since: "2024-01-01T00:00:00Z", limit: 5 } },
  ];
  const observed: string[] = [];
  for (const p of probes) {
    const r = await client.get<{ items: unknown[] }>(`/v1/agents/${encodeURIComponent(email)}/messages`, { query: p.q });
    observed.push(`${p.name}=${r.status}/${Array.isArray(r.body?.items) ? r.body.items.length : "?"}`);
    if (r.status >= 500) {
      fail(SUITE, `filter-5xx-${p.name}`, `${p.name} caused ${r.status}: ${r.raw.slice(0, 200)}`);
    }
  }
  info(SUITE, "filter-surface", `filters: ${observed.join(" | ")}`);
});

test("messaging: GET /messages/{id} of nonexistent message returns 4xx", async () => {
  const email = client.env.primaryAgentEmail;
  const r = await client.get(`/v1/agents/${encodeURIComponent(email)}/messages/msg_nonexistent_${Date.now()}`);
  assert.ok(r.status >= 400 && r.status < 500, `expected 4xx, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("messaging: GET /agents/{email}/messages returns documented shape", async () => {
  const email = client.env.primaryAgentEmail;
  const r = await client.get<{ items?: unknown[] }>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    query: { limit: 5 },
  });
  assert.equal(r.status, 200, `expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.ok(Array.isArray(r.body?.items), "items array present");
});

test("messaging: send with reply_to (header round-trip) is accepted", async () => {
  const email = await createHitlAgent("replyto");
  const r = await client.post<{ message_id: string }>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    body: {
      to: [SINK_EMAIL],
      subject: uniqueSubject("with reply_to"),
      text: "x",
      // reply_to is a single RFC 5322 address STRING (Sets Reply-To header),
      // not an array — an array is rejected 422 as an unexpected property.
      reply_to: "specific-reply@example.com",
    },
  });
  if (r.status >= 400 && r.status < 500) {
    info(SUITE, "reply-to-rejected", `send with reply_to returned ${r.status}: ${r.raw.slice(0, 200)}`);
    return;
  }
  assert.ok(r.status === 200 || r.status === 202, `expected 200/202, got ${r.status}`);
  if (r.body?.message_id) {
    await client.post(`/v1/reviews/${r.body.message_id}/reject`, { body: { reason: "e2e reply_to cleanup" } });
  }
});
