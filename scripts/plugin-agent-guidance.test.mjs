import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

// Every manifest an agent-facing client reads at install time. Deliberately
// excluded: the three marketplace.json files and the two plugin.meta.json
// sources, which carry marketplace copy (it names HITL on purpose) rather
// than agent guidance.
const coreAgentFiles = [
  "plugins/e2a/README.md",
  "plugins/e2a/docs/auth.md",
  "plugins/e2a/docs/setup.md",
  "plugins/e2a/docs/llms.txt",
  "plugins/e2a/docs/sdk.md",
  "plugins/e2a/docs/templates.md",
  "plugins/e2a/skills/e2a/SKILL.md",
  "plugins/e2a/plugin.json",
  "plugins/e2a/mcp.json",
  "plugins/e2a/.claude-plugin/plugin.json",
  "plugins/e2a/.codex-plugin/plugin.json",
  "plugins/e2a/.cursor-plugin/plugin.json",
];

test("core agent guidance does not promote the HITL review surface", async () => {
  const forbidden = /\bHITL\b|human-in-the-loop|approve_review|reject_review|list_reviews|get_review|turn on a review hold|review held messages/i;

  for (const file of coreAgentFiles) {
    const source = await readFile(file, "utf8");
    assert.doesNotMatch(source, forbidden, file);
  }
});

test("the e2a skill keeps a defensive pending_review no-retry warning", async () => {
  const source = await readFile("plugins/e2a/skills/e2a/SKILL.md", "utf8");
  assert.match(source, /pending_review/);
  assert.match(source, /do not retry/i);
});

test("the e2a skill description is quoted YAML", async () => {
  const source = await readFile("plugins/e2a/skills/e2a/SKILL.md", "utf8");
  assert.match(source, /^description: "(?:[^"\\]|\\.)*"$/m);
});

test("plugin discovery surfaces use the canonical email API category", async () => {
  const canonical = /open-source email API for AI agents/i;
  const stale = /authenticated email gateway|authenticated email for AI agents|real, authenticated email inbox|verifies sender identity/i;

  const meta = JSON.parse(await readFile("plugins/e2a/plugin.meta.json", "utf8"));
  for (const [key, description] of Object.entries(meta.descriptions)) {
    if (key === "$comment") continue;
    if (typeof description === "string") assert.match(description, canonical);
  }
  assert.match(meta.codexInterface.shortDescription, canonical);
  assert.match(meta.marketplaces.claude.blurb, canonical);
  assert.match(meta.marketplaces.cursor.blurb, canonical);

  for (const file of [
    "plugins/e2a/README.md",
    "plugins/e2a/skills/e2a/SKILL.md",
    "plugins/e2a/plugin.json",
    "plugins/e2a/.claude-plugin/plugin.json",
    "plugins/e2a/.codex-plugin/plugin.json",
    "plugins/e2a/.cursor-plugin/plugin.json",
  ]) {
    const source = await readFile(file, "utf8");
    assert.match(source, canonical, file);
    assert.doesNotMatch(source, stale, file);
  }

  for (const file of [
    ".claude-plugin/marketplace.json",
    ".agents/plugins/marketplace.json",
    ".cursor-plugin/marketplace.json",
  ]) {
    const marketplace = JSON.parse(await readFile(file, "utf8"));
    const plugin = marketplace.plugins.find((candidate) => candidate.name === "e2a");
    assert.ok(plugin, `${file} is missing e2a`);
    assert.match(plugin.description, canonical, file);
  }
});

test("the e2a skill teaches concise multipart email composition", async () => {
  const source = await readFile("plugins/e2a/skills/e2a/SKILL.md", "utf8");

  assert.match(source, /### Compose before sending/);
  assert.match(source, /Lead with the outcome, decision, request, or blocker/i);
  assert.match(source, /120[–-]180 words/);
  assert.match(source, /complete plain-text body/i);
  assert.match(source, /equivalent `html` body/i);
  assert.match(source, /message can be understood in ten seconds/i);
});

test("the e2a skill teaches the contacts and outreach loop without overclaiming wake-up", async () => {
  const source = await readFile("plugins/e2a/skills/e2a/SKILL.md", "utf8");
  const outreach = source.match(
    /### Manage contacts and outreach \(beta\)[\s\S]*?(?=\n### |\n## )/,
  )?.[0] ?? "";

  assert.match(outreach, /import_contacts/);
  assert.match(outreach, /list_outreach_contacts/);
  assert.match(outreach, /replied=false/);
  assert.match(outreach, /suppressed=false/);
  assert.match(outreach, /next_action_before/);
  assert.match(outreach, /last_outbound_before/);
  assert.match(outreach, /reply_to_message/);
  assert.match(outreach, /set_outreach_contact/);
  assert.match(outreach, /deployed webhook/i);
  assert.match(outreach, /does not launch.*local coding-agent/i);
  // Derive the expected total from the frozen tool-name baseline rather than
  // repeating it here: hardcoding the count meant every tool addition had to
  // update the skill AND this assertion, and the second one was easy to miss.
  const toolNames = JSON.parse(await readFile("mcp/tool-names.v1.json", "utf8"));
  const claim = source.match(
    /MCP surface is \*\*(\d+) tools\*\* \((\d+) runtime\/inbox \+ (\d+) admin\/setup\)/,
  );
  assert.ok(claim, "the e2a skill must state the MCP surface size");
  const [, total, runtime, admin] = claim.map(Number);
  assert.equal(total, toolNames.length,
    "skill tool count must match mcp/tool-names.v1.json");
  assert.equal(runtime + admin, total,
    "the runtime + admin split must add up to the stated total");
});

test("the setup guide reaches a verified first inbox", async () => {
  const source = await readFile("plugins/e2a/docs/setup.md", "utf8");

  assert.match(source, /^# Set up e2a$/m);
  assert.match(source, /Claude Code/);
  assert.match(source, /OpenAI Codex/);
  assert.match(source, /Cursor \/ Windsurf \/ Claude Desktop/);
  assert.match(source, /whoami/);
  assert.match(source, /list_agents/);
  assert.match(source, /create_agent/);
  assert.match(source, /list_messages/);
  assert.doesNotMatch(source, /Always call `tools\/list`/);
});

const assertAlwaysReviewGuidance = (source, file) => {
  assert.match(source, /update_protection/, file);
  assert.match(source, /outbound_gate_policy["`:\s]+allowlist/, file);
  assert.match(source, /outbound_gate_allowlist["`:\s]+\[\]/, file);
  assert.match(source, /outbound_gate_action["`:\s]+review/, file);
  assert.match(source, /holds_on_expiry["`:\s]+reject/, file);
  assert.match(source, /open.*review.*hold(?:s|ing)? nothing/is, file);
  assert.match(source, /only when the user (?:asks|requests)/i, file);
};

test("agent guidance teaches the opt-in always-review protection policy", async () => {
  for (const file of [
    "plugins/e2a/skills/e2a/SKILL.md",
    "plugins/e2a/docs/setup.md",
  ]) {
    const source = await readFile(file, "utf8");
    assertAlwaysReviewGuidance(source, file);
  }
});

test("tether setup does not mutate review configuration", async () => {
  const source = await readFile("plugins/e2a-labs/skills/tether/tether.sh", "utf8");
  assert.doesNotMatch(source, /protection set|outbound-review|outbound review/i);
  assert.doesNotMatch(source, /selftest@agents\.e2a\.dev/);
  assert.match(source, /pending_review/);
});

test("tether relies on the dependency-provided e2a skill without a Labs-local core path", async () => {
  const source = await readFile("plugins/e2a-labs/skills/tether/SKILL.md", "utf8");
  assert.match(source, /the `e2a` skill/i);
  assert.doesNotMatch(source, /\$\{CLAUDE_PLUGIN_ROOT\}\/skills\/e2a\/SKILL\.md/);
});
