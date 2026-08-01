import { test, before, after, afterEach } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { cleanup, track } from "../harness/cleanup.ts";
import { HttpMcpClient, callTool, type McpToolResult } from "../harness/mcp.ts";
import { uniqueSlug } from "../harness/fixtures.ts";
import { fail, info, writeReport } from "../harness/report.ts";

// Black-box MCP conformance for the suppressions tool surface against the
// DEPLOYED streamable-HTTP /mcp server. This is the MCP analogue of suite
// 24-agent-suppressions.test.ts (REST agent-scoped suppression CRUD) — same
// domain, same server-side semantics, but every call here goes through
// tools/call so the mcp_coverage_gate.py recorder credits the tool.
//
// Tools exercised (exactly 4, of the 5-tool suppressions batch):
//   create_agent_suppression, list_agent_suppressions,
//   delete_agent_suppression                       (AGENT-scoped, full lifecycle)
//   list_suppressions                              (ACCOUNT-wide, shape only)
//
// The fifth tool, delete_suppression (ACCOUNT-scoped), is DELIBERATELY not
// called: there is no create API for account suppressions (/v1/account/
// suppressions exposes only GET + DELETE) — rows originate only from a real
// SES bounce/complaint, and staging's e2a-staging-smtp IAM policy denies
// ses:SendRawEmail to the bounce/complaint simulators, so on the staging gate
// there is never a row to delete honestly. It carries an ALLOWLIST entry in
// mcp_coverage_gate.py (mirroring deleteSuppression in coverage_gate.py's
// STAGING_ONLY_ALLOWLIST); its continued advertisement is still asserted by
// the tools/list test below, so silent removal of the tool would be caught.
//
// list_suppressions is likewise shape-only: on staging the account-wide list
// is legitimately EMPTY (see above — nothing can populate it), and a
// non-error result is what the coverage recorder requires. We assert the
// page envelope (a `suppressions` array + the omit-when-done `next_cursor`
// contract) and never fabricate a row.
//
// Shapes verified against mcp/src/tools/suppressions.ts (the MCP tool
// registrations) and cross-checked against the REST AgentSuppressionView /
// DeleteSuppressionResult shapes in 24-agent-suppressions.test.ts — the MCP
// layer's toMcpOutput() snake-cases the SDK's camelCase fields back to the
// same REST vocabulary, with the one deliberate list-envelope difference:
// MCP returns a domain-named `suppressions` array and OMITS `next_cursor`
// on the last page (REST returns `{ items, next_cursor: null }`).
const SUITE = "36-mcp-suppressions";
const apiClient = new ApiClient();
const mcp = new HttpMcpClient(apiClient.env.mcpUrl, apiClient.env.apiKey);

interface AgentSuppressionView {
  agent_email: string;
  address: string;
  source: string;
  created_at: string;
  reason?: string;
}
interface ListAgentSuppressionsResult {
  suppressions: AgentSuppressionView[];
  next_cursor?: string;
}
interface AccountSuppressionView {
  address: string;
  source: string;
  created_at: string;
  reason?: string;
  source_message_id?: string;
}
interface ListSuppressionsResult {
  suppressions: AccountSuppressionView[];
  next_cursor?: string;
}
interface DeleteSuppressionResult {
  deleted: boolean;
  address: string;
}

// All five suppressions tools must stay advertised — including the
// deliberately-uncalled delete_suppression, whose allowlist entry in
// mcp_coverage_gate.py goes stale (exit 2) if the server stops advertising it.
const REQUIRED_TOOLS = [
  "list_suppressions",
  "delete_suppression",
  "create_agent_suppression",
  "list_agent_suppressions",
  "delete_agent_suppression",
];

function extractText(r: McpToolResult): string {
  return r.content?.find((c) => c.type === "text")?.text ?? "";
}

function parseOk<T>(r: McpToolResult, label: string): T {
  assert.equal(r.isError, undefined, `${label} isError: ${extractText(r).slice(0, 300)}`);
  const text = extractText(r);
  assert.ok(text, `${label} returned text content`);
  return JSON.parse(text) as T;
}

async function createAgent(label: string): Promise<string> {
  const slug = uniqueSlug(label);
  const c = await apiClient.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${apiClient.env.sharedDomain}`, name: `mcp suppressions ${label}` },
  });
  if (c.status !== 201 || !c.body?.email) {
    throw new Error(`create agent failed: ${c.status} ${c.raw.slice(0, 200)}`);
  }
  track("agent", c.body.email);
  return c.body.email;
}

// Best-effort net for tests that create a block and exit early on assertion
// failure — the happy-path delete is asserted explicitly in the lifecycle
// test. (Agent deletion cascades too; this just keeps the account tidy even
// if cleanup of the agent itself fails.)
async function deleteBlock(email: string, address: string): Promise<void> {
  await callTool(mcp, "delete_agent_suppression", { email, address }).catch(() => {});
}

before(async () => {
  info(SUITE, "transport", `MCP over HTTP -> ${apiClient.env.mcpUrl}`);
});

afterEach(async () => {
  await cleanup(apiClient);
});

after(async () => {
  await mcp.stop();
  await cleanup(apiClient);
  writeReport(`./reports/${SUITE}.json`);
});

test("mcp-suppressions: tools/list advertises all 5 suppression tools", async () => {
  const list = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  const names = new Set(list.tools.map((t) => t.name));
  const missing = REQUIRED_TOOLS.filter((n) => !names.has(n));
  assert.deepEqual(missing, [], `deployed MCP server must advertise every suppression tool; missing: ${missing.join(", ")}`);
});

// create_agent_suppression + list_agent_suppressions + delete_agent_suppression
// — the full agent-scoped block lifecycle over MCP, mirroring the REST
// lifecycle test in 24-agent-suppressions.test.ts: create → idempotent
// re-create → list shows it → delete → list no longer shows it.
test("mcp-suppressions: agent-scoped block lifecycle via MCP", async () => {
  const email = await createAgent("mcpsup");
  const address = `blocked-${uniqueSlug("rcpt")}@example.com`;
  const reason = "conformance manual block via MCP";

  try {
    // create_agent_suppression
    const created = await parseOk<AgentSuppressionView>(
      await callTool(mcp, "create_agent_suppression", { email, address, reason }),
      "create_agent_suppression",
    );
    assert.equal(created.agent_email, email, "view.agent_email echoes the owning agent");
    assert.equal(created.address, address, "view.address echoes the suppressed recipient");
    assert.equal(created.source, "manual", `MCP-created block has source=manual, got: ${created.source}`);
    assert.ok(created.created_at, "view.created_at present");
    assert.equal(created.reason, reason, "view.reason echoes the request");

    // Documented as idempotent — a byte-identical re-create must succeed
    // (same entry back), not surface as an error.
    const again = await parseOk<AgentSuppressionView>(
      await callTool(mcp, "create_agent_suppression", { email, address, reason }),
      "create_agent_suppression (idempotent re-create)",
    );
    assert.equal(again.address, address, "idempotent re-create returns the same entry");

    // list_agent_suppressions — the created block must appear.
    const listed = await parseOk<ListAgentSuppressionsResult>(
      await callTool(mcp, "list_agent_suppressions", { email, limit: 100 }),
      "list_agent_suppressions",
    );
    assert.ok(Array.isArray(listed.suppressions), "list_agent_suppressions.suppressions is an array");
    assert.ok(
      listed.next_cursor === undefined || typeof listed.next_cursor === "string",
      "next_cursor is a string when present (omitted on the last page — never null)",
    );
    const inList = listed.suppressions.find((s) => s.address === address);
    assert.ok(inList, `created block ${address} is present in list_agent_suppressions`);
    assert.equal(inList!.agent_email, email, "list items are self-describing (agent_email)");
    assert.equal(inList!.source, "manual", "listed entry keeps source=manual");

    // delete_agent_suppression — no confirm arg on the MCP tool; the wrapper
    // supplies the REST ?confirm=DELETE itself.
    const deleted = await parseOk<DeleteSuppressionResult>(
      await callTool(mcp, "delete_agent_suppression", { email, address }),
      "delete_agent_suppression",
    );
    assert.equal(deleted.deleted, true, "delete_agent_suppression returns deleted:true");
    assert.equal(deleted.address, address, "delete_agent_suppression echoes the un-suppressed address");

    // Gone from the list.
    const afterDel = await parseOk<ListAgentSuppressionsResult>(
      await callTool(mcp, "list_agent_suppressions", { email }),
      "list_agent_suppressions (after delete)",
    );
    assert.ok(
      !afterDel.suppressions.some((s) => s.address === address),
      "deleted block no longer appears in list_agent_suppressions",
    );

    // Re-deleting the now-gone block must surface as a tool error (REST 404
    // not_found) — proves the delete was real, not silently satisfied.
    const reDelete = await callTool(mcp, "delete_agent_suppression", { email, address });
    if (!reDelete.isError) {
      fail(SUITE, "re-delete-not-error", `delete_agent_suppression on an already-removed block did not surface as error`);
    }
  } finally {
    await deleteBlock(email, address);
  }
});

// Error contract (cheap negative probes; error results never count as
// coverage, so these cannot mask a broken happy path):
test("mcp-suppressions: agent-scoped tools reject bad input as tool errors", async () => {
  const email = await createAgent("mcpsupng");

  // Syntactically invalid recipient — rejected by the tool's own zod schema
  // (z.string().email()) before any API call. Nothing is created, so there is
  // nothing to clean up beyond the tracked agent.
  const badAddr = await callTool(mcp, "create_agent_suppression", {
    email,
    address: "not-an-email-address",
  });
  assert.equal(badAddr.isError, true, "create_agent_suppression must reject a non-email address");

  // Unknown (never-created) agent — uniform not_found from the server.
  const ghost = `${uniqueSlug("mcpsupgh")}@${apiClient.env.sharedDomain}`;
  const ghostList = await callTool(mcp, "list_agent_suppressions", { email: ghost });
  assert.equal(ghostList.isError, true, "list_agent_suppressions on an unknown agent must surface as error");
});

// list_suppressions — the ACCOUNT-wide list. Shape-only by design: on staging
// this list is legitimately EMPTY (only a real SES bounce/complaint creates a
// row, and staging's SES policy denies the simulators), and a non-error
// result is exactly what the coverage recorder requires. Assert the page
// envelope, and the item shape only for rows that happen to exist (a prod run
// may carry real bounce/complaint rows). Never fabricate a row.
test("mcp-suppressions: list_suppressions returns the account-wide page envelope", async () => {
  const page = await parseOk<ListSuppressionsResult>(
    await callTool(mcp, "list_suppressions", { limit: 5 }),
    "list_suppressions",
  );
  assert.ok(Array.isArray(page.suppressions), "list_suppressions.suppressions is an array");
  assert.ok(page.suppressions.length <= 5, `limit=5 returns at most 5 rows, got ${page.suppressions.length}`);
  assert.ok(
    page.next_cursor === undefined || typeof page.next_cursor === "string",
    "next_cursor is a string when present (omitted on the last page — never null)",
  );
  for (const row of page.suppressions) {
    assert.ok(typeof row.address === "string" && row.address.length > 0, "row.address is a non-empty string");
    assert.ok(typeof row.source === "string" && row.source.length > 0, "row.source is a non-empty string");
    assert.ok(typeof row.created_at === "string" && row.created_at.length > 0, "row.created_at is a non-empty string");
  }
  info(SUITE, "list_suppressions", `rows=${page.suppressions.length} next_cursor=${page.next_cursor !== undefined}`);
});
