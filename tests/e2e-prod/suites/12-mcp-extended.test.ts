import { test, before, after, afterEach } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { cleanup, track } from "../harness/cleanup.ts";
import { HttpMcpClient, callTool } from "../harness/mcp.ts";
import { uniqueSlug, uniqueSubject, SINK_EMAIL, holdAllOutbound } from "../harness/fixtures.ts";
import { fail, info, warn, writeReport } from "../harness/report.ts";

const apiClient = new ApiClient();
const SUITE = "12-mcp-extended";
// Deployed streamable-HTTP /mcp server (co-versioned mcp-server image) — the
// shipped surface, not a locally-built stdio binary. Defaults to `${E2A_URL}/mcp`.
const mcp = new HttpMcpClient(apiClient.env.mcpUrl, apiClient.env.apiKey);

before(async () => {
  info(SUITE, "transport", `MCP over HTTP → ${apiClient.env.mcpUrl}`);
});

afterEach(async () => {
  const r = await cleanup(apiClient);
  if (r.failed.length) warn(SUITE, "cleanup-after-each", `failed ${r.failed.length}`, r.failed);
});

after(async () => {
  await mcp.stop();
  // The loopback fixture must survive the afterEach cleanup between the two
  // tests that share it, so register it for deletion only at suite teardown.
  if (messageFixtureAgentEmail) track("agent", messageFixtureAgentEmail);
  const r = await cleanup(apiClient);
  if (r.failed.length) warn(SUITE, "cleanup", `failed ${r.failed.length}`, r.failed);
  writeReport(`./reports/12-mcp-extended.json`);
});

function extractText(r: { content?: Array<{ type: string; text?: string }> }): string {
  return r.content?.find((c) => c.type === "text")?.text ?? "";
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

// A single self-send loopback message, shared by the get_message and
// reply_to_message happy-path tests below. Self-send (an agent emailing
// itself) is the reliable way to produce a real inbound message on the
// shared domain without needing an external MX — the same mechanism the
// staging loopback path relies on. Sharing one fixture between both tests
// keeps mail volume down against the free-plan monthly cap instead of
// self-sending twice.
interface MessageFixture {
  agentEmail: string;
  id: string;
  subject: string;
}
let messageFixtureAgentEmail: string | undefined;
let messageFixturePromise: Promise<MessageFixture> | undefined;
function messageFixture(): Promise<MessageFixture> {
  messageFixturePromise ??= (async () => {
    // Do not depend on the persistent conformance agent's mutable protection
    // posture. A previous or interrupted test run may legitimately leave it
    // review-gated, which would turn this send into a pending draft and make
    // the loopback poll time out. A fresh agent starts with the open defaults.
    const slug = uniqueSlug("mcpe-loopback");
    const created = await apiClient.post<{ email: string }>("/v1/agents", {
      body: {
        email: `${slug}@${apiClient.env.sharedDomain}`,
        name: "mcp extended loopback fixture",
      },
      expect: 201,
    });
    const fixtureAgent = created.body?.email;
    if (!fixtureAgent) {
      throw new Error(`loopback fixture agent did not return an email: ${created.raw.slice(0, 200)}`);
    }
    messageFixtureAgentEmail = fixtureAgent;

    const subject = uniqueSubject("mcp-ext get-msg fixture");
    const send = await apiClient.post<{ message_id: string; status: string }>(
      `/v1/agents/${encodeURIComponent(fixtureAgent)}/messages`,
      { body: { to: [fixtureAgent], subject, text: "12-mcp-extended self-send fixture" }, expect: [200, 202] },
    );
    if (!send.body?.message_id) {
      throw new Error(`self-send fixture did not return a message_id: ${send.raw.slice(0, 200)}`);
    }
    if (send.body.status === "pending_review") {
      throw new Error(`fresh loopback fixture agent unexpectedly held message ${send.body.message_id} for review`);
    }
    for (let i = 0; i < 12; i++) {
      const poll = await apiClient.get<{ items: Array<{ id: string; subject: string }> }>(
        `/v1/agents/${encodeURIComponent(fixtureAgent)}/messages`,
        { query: { direction: "inbound", read_status: "all", limit: 20 } },
      );
      const m = poll.body?.items?.find((x) => x.subject === subject);
      if (m) return { agentEmail: fixtureAgent, id: m.id, subject };
      await sleep(1500);
    }
    throw new Error(`self-send fixture "${subject}" never appeared for ${fixtureAgent}`);
  })();
  return messageFixturePromise;
}

async function ensureHitlAgent(): Promise<string> {
  const slug = uniqueSlug("mcpe");
  const c = await apiClient.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${apiClient.env.sharedDomain}`, name: "mcp ext" },
  });
  if (c.status !== 201) throw new Error(`create agent: ${c.status} ${c.raw.slice(0, 200)}`);
  const email = c.body!.email;
  track("agent", email);
  const u = await holdAllOutbound(apiClient, email);
  if (u.status !== 200) throw new Error(`enable outbound review: ${u.status}`);
  return email;
}

test("mcp-ext: create_agent tool registers a new agent via MCP", async () => {
  const slug = uniqueSlug("mcpcreate");
  const r = await callTool(mcp, "create_agent", { email: `${slug}@${apiClient.env.sharedDomain}`, name: "mcp created" });
  if (r.isError) {
    fail(SUITE, "create-agent-error", `create_agent reported isError: ${extractText(r).slice(0, 200)}`);
    return;
  }
  const text = extractText(r);
  assert.ok(text, "create_agent returned text content");
  const parsed = JSON.parse(text) as { email?: string; id?: string };
  assert.ok(parsed.email, `expected email in result: ${text}`);
  track("agent", parsed.email!);
  // Should match the slug pattern (slug@shared_domain).
  assert.ok(
    parsed.email!.startsWith(`${slug}@`),
    `expected email "${slug}@*", got "${parsed.email}"`,
  );
});

test("mcp-ext: send_message tool happy path with HITL agent queues message", async () => {
  const list = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  assert.ok(list.tools.some((t) => t.name === "send_message"), "canonical send_message tool is required");
  const email = await ensureHitlAgent();
  // Pass the canonical `email` selector because this test creates a fresh
  // agent and must send from it rather than the server's configured default.
  const r = await callTool(mcp, "send_message", {
    email,
    to: [SINK_EMAIL],
    subject: uniqueSubject("mcp send"),
    text: "from MCP",
  });
  assert.equal(r.isError, undefined, `send_message isError on valid input: ${extractText(r).slice(0, 200)}`);
  const parsed = JSON.parse(extractText(r)) as { message_id?: string; status?: string };
  assert.ok(parsed.message_id?.startsWith("msg_"), `expected msg_ prefix, got "${parsed.message_id}"`);
  assert.equal(parsed.status, "pending_review", "review-gated send_message stays held and does not send externally");
  // Clean up via API.
  const cleanupReview = await apiClient.post(`/v1/reviews/${parsed.message_id}/reject`, {
    body: { reason: "e2e mcp send cleanup" },
  });
  assert.equal(cleanupReview.status, 200, `cleanup rejection failed: ${cleanupReview.status}`);
});

test("mcp-ext: list_reviews and get_review round-trip", async () => {
  const list = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  const hasList = list.tools.find((t) => t.name === "list_reviews");
  const hasGet = list.tools.find((t) => t.name === "get_review");
  if (!hasList || !hasGet) {
    info(SUITE, "pending-tools-absent", `missing tools: list=${!!hasList} get=${!!hasGet}`);
    return;
  }
  const email = await ensureHitlAgent();
  // Queue one via API (so we know we have something to inspect).
  const s = await apiClient.post<{ message_id: string }>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    body: { to: [SINK_EMAIL], subject: uniqueSubject("mcp pending"), text: "x" },
  });
  assert.equal(s.status, 202, `pending setup send expected 202: ${s.raw.slice(0, 200)}`);
  assert.ok(s.body?.message_id, "pending setup send must return a message_id — a missing fixture is a broken test, not a skip");
  const id = s.body.message_id;

  // list_reviews — should include our queued msg. The MCP
  // tool's schema is strictInputSchema({}) — it takes ZERO arguments
  // (no page_size, no token). The HTTP API does paginate; the MCP
  // wrapper deliberately doesn't expose that surface. Pass nothing.
  const lp = await callTool(mcp, "list_reviews");
  if (lp.isError) {
    fail(SUITE, "list-pending-error", `list_reviews isError: ${extractText(lp).slice(0, 200)}`);
  } else {
    const text = extractText(lp);
    assert.ok(
      text.includes(id),
      `queued ${id} must appear in list_reviews (the MCP tool exposes no pagination or filter that could hide it)`,
    );
  }

  // get_review.
  const gp = await callTool(mcp, "get_review", { message_id: id });
  if (gp.isError) {
    fail(SUITE, "get-pending-error", `get_review isError for ${id}: ${extractText(gp).slice(0, 200)}`);
  } else {
    const parsed = JSON.parse(extractText(gp)) as { id?: string; message_id?: string; status?: string };
    const returnedId = parsed.id ?? parsed.message_id;
    assert.equal(returnedId, id, `get_review returned id=${returnedId}, expected ${id}`);
  }

  // Cleanup
  await apiClient.post(`/v1/reviews/${id}/reject`, { body: { reason: "e2e mcp pending cleanup" } });
});

test("mcp-ext: reject_review via MCP transitions the message", async () => {
  const list = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  assert.ok(list.tools.some((t) => t.name === "reject_review"), "canonical reject_review tool is required");
  const email = await ensureHitlAgent();
  const s = await apiClient.post<{ message_id: string; status: string }>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    body: { to: [SINK_EMAIL], subject: uniqueSubject("mcp reject"), text: "x" },
  });
  assert.equal(s.status, 202, `reject setup send expected 202: ${s.raw.slice(0, 200)}`);
  assert.equal(s.body?.status, "pending_review", "reject setup must remain held");
  assert.ok(s.body?.message_id, "reject setup returns message_id");
  const id = s.body!.message_id;
  const r = await callTool(mcp, "reject_review", { message_id: id, reason: "e2e mcp reject" });
  assert.equal(r.isError, undefined, `reject_review isError: ${extractText(r).slice(0, 200)}`);
  const rejected = JSON.parse(extractText(r)) as { status?: string };
  assert.equal(rejected.status, "review_rejected", "reject_review transitions the hold to review_rejected");
  // Re-reject — should now fail (already rejected, 409 from API; MCP should surface as error).
  const r2 = await callTool(mcp, "reject_review", { message_id: id, reason: "should fail" });
  assert.equal(r2.isError, true, "re-reject of an already rejected message must surface a terminal-state error");
});

test("mcp-ext: approve_review via MCP sends the message", async () => {
  const list = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  assert.ok(list.tools.some((t) => t.name === "approve_review"), "canonical approve_review tool is required");
  const email = await ensureHitlAgent();
  const s = await apiClient.post<{ message_id: string; status: string }>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    body: { to: [SINK_EMAIL], subject: uniqueSubject("mcp approve"), text: "x" },
  });
  assert.equal(s.status, 202, `approve setup send expected 202: ${s.raw.slice(0, 200)}`);
  assert.equal(s.body?.status, "pending_review", "approve setup must remain held until explicit approval");
  assert.ok(s.body?.message_id, "approve setup returns message_id");
  const id = s.body!.message_id;
  const r = await callTool(mcp, "approve_review", { message_id: id });
  assert.equal(r.isError, undefined, `approve_review isError: ${extractText(r).slice(0, 200)}`);
  const approved = JSON.parse(extractText(r)) as { status?: string };
  assert.ok(
    approved.status === "accepted" || approved.status === "sent",
    `approve_review must transition to accepted/sent, got "${approved.status}"`,
  );
  // Re-approve — should fail with 409 (already sent).
  const r2 = await callTool(mcp, "approve_review", { message_id: id });
  assert.equal(r2.isError, true, "re-approve of a sent message must surface a terminal-state error");
});

test("mcp-ext: get_message returns shape and only own messages", async () => {
  const list = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  assert.ok(list.tools.some((t) => t.name === "get_message"), "canonical get_message tool is required");
  // The MCP get_message tool fetches via the AGENT-scoped endpoint
  // GET /v1/agents/{agent_email}/messages/{id} — anti-enumeration
  // 404s on any message that doesn't belong to the pinned agent. Rather
  // than hoping the pinned agent's inbox already has something in it
  // (a prior version of this test silently `return`ed when it was empty,
  // which let the suite report all-green while never actually exercising
  // get_message's happy path), we produce a real fixture via self-send.
  const { agentEmail, id } = await messageFixture();
  // The conformance credential here is account-scoped (no agent_email to
  // pin — see the 08-mcp "whoami" test), so get_message needs an explicit
  // `email` to resolve which agent's mailbox to read from.
  const r = await callTool(mcp, "get_message", { message_id: id, email: agentEmail });
  if (r.isError) {
    fail(SUITE, "get-msg-error", `get_message isError for our own ${id}: ${extractText(r).slice(0, 200)}`);
    return;
  }
  const parsed = JSON.parse(extractText(r)) as { id?: string; message_id?: string };
  const returnedId = parsed.id ?? parsed.message_id;
  assert.equal(returnedId, id, `expected id ${id}, got ${returnedId}`);

  // The same real ID under a different owned agent must remain hidden.
  const r2 = await callTool(mcp, "get_message", {
    message_id: id,
    email: apiClient.env.primaryAgentEmail,
  });
  assert.equal(r2.isError, true, "get_message must not read a message from another agent's mailbox");
  assert.match(extractText(r2), /\[not_found\]/, "cross-mailbox get_message must surface canonical not_found");
});

test("mcp-ext: reply_to_message happy path replies to a real message", async () => {
  const list = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  assert.ok(list.tools.some((t) => t.name === "reply_to_message"), "canonical reply_to_message tool is required");
  const { agentEmail, id } = await messageFixture();
  // Same account-scoped-credential caveat as get_message above.
  const r = await callTool(mcp, "reply_to_message", {
    message_id: id,
    text: "reply from 12-mcp-extended happy path",
    email: agentEmail,
  });
  assert.equal(r.isError, undefined, `reply_to_message isError: ${extractText(r).slice(0, 200)}`);
  const parsed = JSON.parse(extractText(r)) as { message_id?: string; status?: string };
  assert.ok(parsed.message_id?.startsWith("msg_"), `expected msg_ prefix, got "${parsed.message_id}"`);
  assert.ok(
    parsed.status === "accepted" || parsed.status === "sent" || parsed.status === "pending_review",
    `reply_to_message must report accepted/sent/pending_review, got "${parsed.status}"`,
  );
  if (parsed.status === "pending_review") {
    const cleanupReview = await apiClient.post(`/v1/reviews/${parsed.message_id}/reject`, {
      body: { reason: "e2e mcp reply cleanup" },
    });
    assert.equal(cleanupReview.status, 200, `cleanup rejection failed: ${cleanupReview.status}`);
  }
});

test("mcp-ext: reply_to_message via MCP — to bogus id surfaces error", async () => {
  const list = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  assert.ok(list.tools.some((t) => t.name === "reply_to_message"), "canonical reply_to_message tool is required");
  const r = await callTool(mcp, "reply_to_message", {
    message_id: `msg_bogus_${Date.now()}`,
    text: "should never go out",
    email: apiClient.env.primaryAgentEmail,
  });
  assert.equal(r.isError, true, "reply_to_message with bogus id must surface as an error");
  assert.match(extractText(r), /\[not_found\]/, "bogus reply_to_message must surface canonical not_found");
});

test("mcp-ext: cross-tool consistency — list_agents matches API surface", async () => {
  const r = await callTool(mcp, "list_agents");
  const text = extractText(r);
  const mcpAgents = (JSON.parse(text) as { agents: Array<{ email: string }> }).agents.map((a) => a.email).sort();
  // REST list envelope is Page[T] = {items, next_cursor}; the MCP envelope is
  // {agents, next_cursor?} — deliberately different shapes (frozen MCP
  // contract, see paginationInput in mcp/src/tools/util.ts). Compare contents.
  const apiResp = await apiClient.get<{ items: Array<{ email: string }> }>("/v1/agents");
  const apiAgents = (apiResp.body?.items ?? []).map((a) => a.email).sort();
  assert.deepEqual(
    mcpAgents,
    apiAgents,
    `MCP list_agents (${mcpAgents.length}) must match API /agents (${apiAgents.length})`,
  );
  info(SUITE, "list-agents-aligned", `MCP and API agent lists match: ${apiAgents.length} agents`);
});
