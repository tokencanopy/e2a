import { test, before, after, afterEach } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { ApiClient } from "../harness/client.ts";
import { cleanup, forgetDomainDeleteKey, track, untrack } from "../harness/cleanup.ts";
import { HttpMcpClient, callTool } from "../harness/mcp.ts";
import { uniqueSlug, uniqueSubject, runId, SINK_EMAIL, holdAllOutbound } from "../harness/fixtures.ts";
import { info, warn, writeReport } from "../harness/report.ts";

// MCP coverage for the domains tool family (register_domain, get_domain,
// list_domains, verify_domain, delete_domain) and the pending-review-queue
// legacy alias family (approve_message, reject_message, get_pending_message,
// list_pending_messages — deprecated aliases that all route through the same
// unified review queue as approve_review/reject_review/get_review/list_reviews,
// see mcp/src/tools/legacy.ts). Both families are exercised over the deployed
// streamable-HTTP /mcp server, mirroring suites/12-mcp-extended.test.ts style.
const apiClient = new ApiClient();
const SUITE = "28-mcp-domains-reviews";
const mcp = new HttpMcpClient(apiClient.env.mcpUrl, apiClient.env.apiKey);

// .example.com is reserved by RFC 2606 for documentation/testing — safe to
// register-then-never-publish-DNS without colliding with anything real. Same
// convention as suites/10-domains.test.ts.
function fakeDomain(label: string): string {
  return `e2e-${runId()}-${label}-${Math.random().toString(36).slice(2, 8)}.example.com`;
}

function extractText(r: { content?: Array<{ type: string; text?: string }> }): string {
  return r.content?.find((c) => c.type === "text")?.text ?? "";
}

before(async () => {
  info(SUITE, "transport", `MCP over HTTP → ${apiClient.env.mcpUrl}`);
});

afterEach(async () => {
  const r = await cleanup(apiClient);
  if (r.failed.length) warn(SUITE, "cleanup-after-each", `failed ${r.failed.length}`, r.failed);
});

after(async () => {
  await mcp.stop();
  const r = await cleanup(apiClient);
  if (r.failed.length) warn(SUITE, "cleanup", `failed ${r.failed.length}`, r.failed);
  writeReport(`./reports/28-mcp-domains-reviews.json`);
});

// createHeldReview: throwaway shared-domain agent -> hold-all-outbound ->
// send. Returns the agent email + the held message id. Mirrors
// suites/18-reviews.test.ts's createHeldReview / suites/12-mcp-extended.test.ts's
// ensureHitlAgent — the proven mechanism for landing a message in
// pending_review that the review/pending-message MCP tools can then act on.
// Caller MUST delete the agent (via track("agent", email) + cleanup, or
// explicitly) once done.
async function createHeldReview(label: string): Promise<{ email: string; id: string; subject: string }> {
  const slug = uniqueSlug(label);
  const c = await apiClient.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${apiClient.env.sharedDomain}`, name: `mcp-dr ${label}` },
  });
  if (c.status !== 201) throw new Error(`create agent failed: ${c.status} ${c.raw.slice(0, 200)}`);
  const email = c.body!.email;
  track("agent", email);
  const u = await holdAllOutbound(apiClient, email);
  if (u.status !== 200) throw new Error(`hold-all-outbound failed: ${u.status} ${u.raw.slice(0, 200)}`);
  const subject = uniqueSubject(`mcp-dr ${label}`);
  const s = await apiClient.post<{ message_id: string; status: string }>(
    `/v1/agents/${encodeURIComponent(email)}/messages`,
    { body: { to: [SINK_EMAIL], subject, text: "held for review via MCP — must never actually go out" } },
  );
  if (s.status !== 202 || !s.body?.message_id) {
    throw new Error(`held send expected 202 pending_review, got ${s.status}: ${s.raw.slice(0, 200)}`);
  }
  assert.equal(s.body.status, "pending_review", "held send status is pending_review");
  return { email, id: s.body.message_id, subject };
}

test("mcp-domains: register_domain / get_domain / list_domains / verify_domain (negative control) / delete_domain", async () => {
  // Call tools/list once so the coverage gate records the live denominator
  // (harness/mcp.ts feeds mcp-coverage.ts from this same JSON-RPC call).
  const list = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  for (const name of [
    "register_domain",
    "get_domain",
    "list_domains",
    "verify_domain",
    "delete_domain",
    "approve_message",
    "reject_message",
    "get_pending_message",
    "list_pending_messages",
  ]) {
    assert.ok(list.tools.some((t) => t.name === name), `deployed server must advertise ${name}`);
  }

  const domain = fakeDomain("mcpdom");

  // register_domain — step 1 of the custom-domain flow. Track immediately so
  // the afterEach/after cleanup net catches it even if a later assertion in
  // this test throws (free-plan account: 1 domain total, must not leak it).
  const reg = await callTool(mcp, "register_domain", { domain });
  assert.equal(reg.isError, undefined, `register_domain isError: ${extractText(reg).slice(0, 200)}`);
  track("domain", domain);
  const regParsed = JSON.parse(extractText(reg)) as {
    domain?: string;
    dns_records?: Array<{ type: string; purpose: string }>;
    verified?: boolean;
  };
  assert.equal(regParsed.domain, domain, "register_domain echoes the requested domain");
  assert.ok(Array.isArray(regParsed.dns_records) && regParsed.dns_records.length > 0, "register_domain returns dns_records to publish");
  assert.equal(regParsed.verified, false, "a freshly-registered domain starts unverified");

  // get_domain — poll target, single-domain read.
  const got = await callTool(mcp, "get_domain", { domain });
  assert.equal(got.isError, undefined, `get_domain isError: ${extractText(got).slice(0, 200)}`);
  const gotParsed = JSON.parse(extractText(got)) as { domain?: string; verified?: boolean };
  assert.equal(gotParsed.domain, domain, "get_domain returns the same domain we registered");

  // list_domains — assert OUR domain is present by name. No account-global
  // count assertion: other suites/agents may share this account.
  const listed = await callTool(mcp, "list_domains", {});
  assert.equal(listed.isError, undefined, `list_domains isError: ${extractText(listed).slice(0, 200)}`);
  const listedParsed = JSON.parse(extractText(listed)) as { domains?: Array<{ domain: string }> };
  assert.ok(
    listedParsed.domains?.some((d) => d.domain === domain),
    `list_domains must include our freshly-registered ${domain}`,
  );

  // verify_domain — NEGATIVE CONTROL. We deliberately never publish the DNS
  // records for this .example.com domain, so this must report verified:false
  // as a NORMAL 200-equivalent success (not isError) — the honest way to
  // cover verify_domain here per the task brief: calling it on a domain
  // whose DNS IS published would either hang forever or (if it isn't yet
  // propagated) negative-cache the miss for up to the zone's SOA minimum
  // (~30min), which no in-test poll can outlast. This also guards against a
  // dev-mode DNS short-circuit tautology (suites/22-domain-lifecycle.test.ts
  // runs the equivalent REST-side negative control).
  const verify = await callTool(mcp, "verify_domain", { domain });
  assert.equal(verify.isError, undefined, `verify_domain (negative control) must be a normal success, not isError: ${extractText(verify).slice(0, 200)}`);
  const verifyParsed = JSON.parse(extractText(verify)) as { domain?: string; verified?: boolean };
  assert.equal(verifyParsed.domain, domain, "verify_domain echoes the domain");
  assert.equal(verifyParsed.verified, false, "unpublished-DNS domain must report verified:false, not a DNS/dev-mode short-circuit");

  // delete_domain — DESTRUCTIVE, requires confirm:true (schema-level literal
  // guard against an LLM hallucinating a delete). No agents were ever
  // created on this domain (it never verified), so no domain_has_agents risk.
  const del = await callTool(mcp, "delete_domain", { domain, confirm: true, idempotency_key: randomUUID() });
  assert.equal(del.isError, undefined, `delete_domain isError: ${extractText(del).slice(0, 200)}`);
  const delParsed = JSON.parse(extractText(del)) as { deleted?: boolean; domain?: string };
  assert.equal(delParsed.deleted, true, "delete_domain result has deleted:true");
  assert.equal(delParsed.domain, domain, "delete_domain result echoes the domain");
  // Already deleted via the tool call above — untrack so the redundant
  // afterEach cleanup DELETE (which would 404, itself harmless) is skipped.
  untrack("domain", domain);
  forgetDomainDeleteKey(domain);
});

test("mcp-domains-reviews: list_pending_messages + get_pending_message + approve_message round-trip", async () => {
  const { email, id, subject } = await createHeldReview("approve");
  try {
    // list_pending_messages — deprecated alias for list_reviews; walks every
    // page of the unified account-level review queue and returns the
    // historical {messages: [...]} envelope. Assert OUR held message is
    // present by id, not an account-global count.
    const listed = await callTool(mcp, "list_pending_messages", {});
    assert.equal(listed.isError, undefined, `list_pending_messages isError: ${extractText(listed).slice(0, 200)}`);
    const listedParsed = JSON.parse(extractText(listed)) as { messages?: Array<{ id?: string }> };
    assert.ok(
      listedParsed.messages?.some((m) => m.id === id),
      `list_pending_messages must include our held message ${id}`,
    );

    // get_pending_message — deprecated alias for get_review; full detail.
    const got = await callTool(mcp, "get_pending_message", { message_id: id });
    assert.equal(got.isError, undefined, `get_pending_message isError: ${extractText(got).slice(0, 200)}`);
    const gotParsed = JSON.parse(extractText(got)) as { id?: string; review_status?: string; subject?: string };
    assert.equal(gotParsed.id, id, "get_pending_message returns the same message id");
    assert.equal(gotParsed.review_status, "pending_review", "held message is still pending_review before approval");
    assert.equal(gotParsed.subject, subject, "get_pending_message echoes the held subject");

    // approve_message — deprecated alias for approve_review; releases the
    // outbound hold for async send (queued, not necessarily synchronous).
    const approved = await callTool(mcp, "approve_message", { message_id: id });
    assert.equal(approved.isError, undefined, `approve_message isError: ${extractText(approved).slice(0, 200)}`);
    const approvedParsed = JSON.parse(extractText(approved)) as { message_id?: string; status?: string };
    assert.equal(approvedParsed.message_id, id, "approve_message result echoes the approved message id");
    assert.ok(
      approvedParsed.status === "accepted" || approvedParsed.status === "sent",
      `approve_message must transition to accepted/sent, got "${approvedParsed.status}"`,
    );

    // Re-approve a resolved hold must surface as a terminal-state tool error.
    const reapproved = await callTool(mcp, "approve_message", { message_id: id });
    assert.equal(reapproved.isError, true, "re-approve of an already-resolved message must isError");
  } finally {
    // Agent delete cascades to its held/sent messages; explicit and tracked
    // regardless so a mid-test throw still gets cleaned up.
    await apiClient.delete(`/v1/agents/${encodeURIComponent(email)}?confirm=DELETE`);
    untrack("agent", email);
  }
});

test("mcp-domains-reviews: reject_message discards the held draft", async () => {
  const { email, id } = await createHeldReview("reject");
  try {
    const rejected = await callTool(mcp, "reject_message", { message_id: id, reason: "e2e mcp-domains-reviews reject" });
    assert.equal(rejected.isError, undefined, `reject_message isError: ${extractText(rejected).slice(0, 200)}`);
    const rejectedParsed = JSON.parse(extractText(rejected)) as { message_id?: string; status?: string };
    assert.equal(rejectedParsed.message_id, id, "reject_message result echoes the rejected message id");
    assert.equal(rejectedParsed.status, "review_rejected", "reject_message transitions the hold to review_rejected");

    // Re-reject a resolved hold must surface as a terminal-state tool error.
    const rereject = await callTool(mcp, "reject_message", { message_id: id, reason: "should fail" });
    assert.equal(rereject.isError, true, "re-reject of an already-resolved message must isError");
  } finally {
    await apiClient.delete(`/v1/agents/${encodeURIComponent(email)}?confirm=DELETE`);
    untrack("agent", email);
  }
});
