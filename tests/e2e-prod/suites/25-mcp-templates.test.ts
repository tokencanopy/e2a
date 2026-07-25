import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { HttpMcpClient, callTool } from "../harness/mcp.ts";
import { uniqueSlug } from "../harness/fixtures.ts";
import { info, warn, writeReport } from "../harness/report.ts";

// Black-box MCP conformance for the beta template tools (create/get/list/
// update/delete/validate_template + list/get_starter_template) and the
// API-key tools (create/list/delete_api_key), exercised over the DEPLOYED
// streamable-HTTP /mcp server — the same surface 08-mcp.test.ts and
// 12-mcp-extended.test.ts target, not a locally-built stdio binary.
//
// All 8 template tools are account-scope only (agent-scoped credentials get
// a 403), and create_api_key/delete_api_key are likewise account-scope —
// the conformance key here is account-scoped, matching every other MCP
// suite in this repo.
//
// Cleanup is self-contained (like 17-templates.test.ts's REST suite): every
// created template/key is deleted inline via the very tool under test
// (delete_template / delete_api_key), in a try/finally, rather than via the
// shared cleanup harness (harness/cleanup.ts only tracks agent/domain
// kinds). The api-key lifecycle binds to the account's existing primary
// probe agent instead of minting a throwaway agent, to stay well inside the
// free-plan 3-agent cap.
const SUITE = "25-mcp-templates";
const apiClient = new ApiClient();
const mcp = new HttpMcpClient(apiClient.env.mcpUrl, apiClient.env.apiKey);

const REQUIRED_TOOLS = [
  "create_template",
  "get_template",
  "list_templates",
  "update_template",
  "delete_template",
  "validate_template",
  "list_starter_templates",
  "get_starter_template",
  "create_api_key",
  "list_api_keys",
  "delete_api_key",
] as const;

before(async () => {
  info(SUITE, "transport", `MCP over HTTP → ${apiClient.env.mcpUrl}`);
});

after(async () => {
  await mcp.stop();
  writeReport(`./reports/${SUITE}.json`);
});

function extractText(r: { content?: Array<{ type: string; text?: string }> }): string {
  return r.content?.find((c) => c.type === "text")?.text ?? "";
}

function parseJson<T>(r: { content?: Array<{ type: string; text?: string }> }): T {
  return JSON.parse(extractText(r)) as T;
}

interface TemplateView {
  id?: string;
  name?: string;
  subject?: string;
  text?: string;
  html?: string;
  alias?: string;
  created_at?: string;
  updated_at?: string;
  from_starter_alias?: string;
  from_starter_version?: string;
}
interface DeleteResult {
  deleted?: boolean;
  id?: string;
}
interface StarterTemplateView {
  alias?: string;
  name?: string;
  description?: string;
  version?: string;
  subject?: string;
  variables?: Array<{ name: string; required: boolean; raw: boolean; description: string; example: string }>;
}
interface StarterTemplateDetailView extends StarterTemplateView {
  text?: string;
  html?: string;
}
interface ValidateTemplateResponse {
  valid?: boolean;
  errors?: Array<{ part: string; message: string }>;
  rendered?: { subject?: string; text?: string; html?: string };
  suggested_data?: Record<string, unknown>;
}
interface ApiKeyView {
  id?: string;
  name?: string;
  key_prefix?: string;
  scope?: string;
  agent_email?: string;
  created_at?: string;
}
interface CreateApiKeyResult extends ApiKeyView {
  key?: string;
}

test("mcp-templates: tools/list advertises all 11 template + API-key tools", async () => {
  const r = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  const names = new Set(r.tools.map((t) => t.name));
  for (const req of REQUIRED_TOOLS) {
    assert.ok(names.has(req), `expected tool "${req}" in the advertised surface`);
  }
});

test("mcp-templates: create_template + get_template + list_templates + update_template + delete_template lifecycle", async () => {
  const alias = uniqueSlug("mcptmpl");
  let id: string | null = null;
  try {
    const create = await callTool(mcp, "create_template", {
      name: `e2e ${alias}`,
      subject: "Hi {{name}}",
      text: "Hello {{name}}, welcome to {{company}}.",
      alias,
    });
    assert.equal(create.isError, undefined, `create_template isError: ${extractText(create).slice(0, 300)}`);
    const created = parseJson<TemplateView>(create);
    assert.ok(created.id?.startsWith("tmpl_"), `expected tmpl_ id, got "${created.id}"`);
    id = created.id!;
    assert.equal(created.name, `e2e ${alias}`, "create_template echoes name");
    assert.equal(created.subject, "Hi {{name}}", "create_template echoes subject");
    assert.equal(created.alias, alias, "create_template echoes alias");

    const get = await callTool(mcp, "get_template", { id });
    assert.equal(get.isError, undefined, `get_template isError: ${extractText(get).slice(0, 300)}`);
    const gotten = parseJson<TemplateView>(get);
    assert.equal(gotten.id, id, "get_template returns the created template");
    assert.equal(gotten.text, "Hello {{name}}, welcome to {{company}}.", "get_template returns the full body source");

    const list = await callTool(mcp, "list_templates");
    assert.equal(list.isError, undefined, `list_templates isError: ${extractText(list).slice(0, 300)}`);
    const listed = parseJson<{ templates?: TemplateView[] }>(list);
    assert.ok(Array.isArray(listed.templates), "list_templates returns a templates array");
    // Assert on the specific resource we created, never on account-wide count.
    assert.ok(listed.templates!.some((t) => t.id === id), `created template ${id} appears in list_templates`);

    const update = await callTool(mcp, "update_template", { id, name: `${alias}-updated`, subject: "Hi {{name}}!" });
    assert.equal(update.isError, undefined, `update_template isError: ${extractText(update).slice(0, 300)}`);
    const updated = parseJson<TemplateView>(update);
    assert.equal(updated.name, `${alias}-updated`, "update_template applies the new name");
    assert.equal(updated.subject, "Hi {{name}}!", "update_template applies the new subject");
    assert.equal(updated.text, "Hello {{name}}, welcome to {{company}}.", "update_template leaves an unpassed field untouched");

    const del = await callTool(mcp, "delete_template", { id, confirm: true });
    assert.equal(del.isError, undefined, `delete_template isError: ${extractText(del).slice(0, 300)}`);
    const deleted = parseJson<DeleteResult>(del);
    assert.equal(deleted.deleted, true, "delete_template reports deleted:true");
    assert.equal(deleted.id, id, "delete_template echoes the deleted id");
    const deletedId = id;
    id = null;

    const getAfterDelete = await callTool(mcp, "get_template", { id: deletedId! });
    if (getAfterDelete.isError !== true) {
      warn(SUITE, "get-after-delete", `get_template for a deleted id ${deletedId} did not surface isError`);
    }
  } finally {
    if (id) await callTool(mcp, "delete_template", { id, confirm: true });
  }
});

test("mcp-templates: validate_template dry-run renders a preview without persisting", async () => {
  const r = await callTool(mcp, "validate_template", {
    subject: "Hi {{name}}",
    text: "Welcome {{name}} to {{company}}",
    test_data: { name: "Ada", company: "Acme" },
  });
  assert.equal(r.isError, undefined, `validate_template isError: ${extractText(r).slice(0, 300)}`);
  const parsed = parseJson<ValidateTemplateResponse>(r);
  assert.equal(parsed.valid, true, "valid source reports valid:true");
  assert.equal(parsed.rendered?.subject, "Hi Ada", "subject rendered against test_data");
  assert.equal(parsed.rendered?.text, "Welcome Ada to Acme", "body rendered against test_data");
  assert.ok(parsed.suggested_data, "suggested_data present");
  assert.ok(
    "name" in (parsed.suggested_data as object) && "company" in (parsed.suggested_data as object),
    "suggested_data covers every referenced variable",
  );
});

test("mcp-templates: list_starter_templates + get_starter_template", async () => {
  const list = await callTool(mcp, "list_starter_templates");
  assert.equal(list.isError, undefined, `list_starter_templates isError: ${extractText(list).slice(0, 300)}`);
  const listed = parseJson<{ starter_templates?: StarterTemplateView[] }>(list);
  assert.ok(Array.isArray(listed.starter_templates), "list_starter_templates returns a starter_templates array");
  assert.ok(listed.starter_templates!.length >= 1, "deployment ships at least one starter template");
  const alias = listed.starter_templates![0].alias;
  assert.ok(alias, "picked a starter alias from the list");

  const get = await callTool(mcp, "get_starter_template", { alias: alias! });
  assert.equal(get.isError, undefined, `get_starter_template isError: ${extractText(get).slice(0, 300)}`);
  const detail = parseJson<StarterTemplateDetailView>(get);
  assert.equal(detail.alias, alias, "get_starter_template returns the requested alias");
  assert.equal(typeof detail.text, "string", "detail carries a plain-text body source");
  assert.equal(typeof detail.html, "string", "detail carries an html body source");
  assert.ok(Array.isArray(detail.variables), "detail carries a variables array");
});

test("mcp-templates: create_api_key + list_api_keys + delete_api_key — agent-scoped key lifecycle", async () => {
  // create_api_key needs an existing agent to bind to. This account starts
  // with zero agents (the pinned E2A_AGENT_EMAIL is a reserved slug, not a
  // pre-provisioned inbox), so create it here if absent and tear it back
  // down afterwards — stays well inside the free-plan 3-agent cap either way.
  const agentEmail = apiClient.env.primaryAgentEmail;
  let createdAgent = false;
  const existing = await apiClient.get(`/v1/agents/${encodeURIComponent(agentEmail)}`);
  if (existing.status === 404) {
    const createAgent = await apiClient.post("/v1/agents", { body: { email: agentEmail, name: "mcp templates probe" } });
    assert.equal(createAgent.status, 201, `agent setup failed: ${createAgent.status} ${createAgent.raw.slice(0, 200)}`);
    createdAgent = true;
  } else {
    assert.equal(existing.status, 200, `unexpected status probing agent: ${existing.status} ${existing.raw.slice(0, 200)}`);
  }

  const keyName = uniqueSlug("mcpkey");
  let id: string | null = null;
  try {
    const create = await callTool(mcp, "create_api_key", {
      agent_email: agentEmail,
      name: keyName,
    });
    assert.equal(create.isError, undefined, `create_api_key isError: ${extractText(create).slice(0, 300)}`);
    const created = parseJson<CreateApiKeyResult>(create);
    assert.ok(created.id, "create_api_key returns an id");
    id = created.id!;
    // The plaintext secret is shown exactly once — assert it exists without
    // ever logging or persisting its value (report.ts writes findings to disk).
    assert.equal(typeof created.key, "string", "create_api_key returns the one-time plaintext key");
    assert.ok(created.key!.length > 0, "plaintext key is non-empty");
    assert.equal(created.scope, "agent", "create_api_key mints an agent-scoped key");
    assert.equal(created.agent_email, agentEmail, "key is bound to the requested agent");
    assert.equal(created.name, keyName, "create_api_key echoes the name");

    const list = await callTool(mcp, "list_api_keys");
    assert.equal(list.isError, undefined, `list_api_keys isError: ${extractText(list).slice(0, 300)}`);
    const listed = parseJson<{ api_keys?: ApiKeyView[] }>(list);
    assert.ok(Array.isArray(listed.api_keys), "list_api_keys returns an api_keys array");
    // Assert on the specific key we created, never on account-wide count —
    // this account accumulates keys across suite runs.
    const found = listed.api_keys!.find((k) => k.id === id);
    assert.ok(found, `created key ${id} appears in list_api_keys`);
    assert.equal(found!.name, keyName, "listed key carries the name");
    assert.ok(!("key" in (found as object)), "list_api_keys never echoes the plaintext secret");

    const del = await callTool(mcp, "delete_api_key", { id, confirm: true });
    assert.equal(del.isError, undefined, `delete_api_key isError: ${extractText(del).slice(0, 300)}`);
    const deleted = parseJson<DeleteResult>(del);
    assert.equal(deleted.deleted, true, "delete_api_key reports deleted:true");
    assert.equal(deleted.id, id, "delete_api_key echoes the revoked id");
    id = null;
  } finally {
    if (id) await callTool(mcp, "delete_api_key", { id, confirm: true });
    // Only remove the agent if this test provisioned it — leave a
    // pre-existing primary agent alone.
    if (createdAgent) {
      const del = await apiClient.delete(`/v1/agents/${encodeURIComponent(agentEmail)}?confirm=DELETE`);
      if (del.status !== 204 && del.status !== 200) {
        warn(SUITE, "agent-cleanup-failed", `could not delete probe agent ${agentEmail}: HTTP ${del.status}`);
      }
    }
  }
});
