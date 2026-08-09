import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

const read = (path) => readFile(path, "utf8");
const section = (source, heading) =>
  source.split(`## ${heading}\n`)[1]?.split(/\n## /)[0] ?? "";
const description = (source) => {
  const match = source.match(/^description: "((?:[^"\\]|\\.)*)"$/m);
  assert.ok(match, "skill description must be quoted YAML");
  return JSON.parse(`"${match[1]}"`);
};

const e2aOperationDescription = "Use when operating an already-connected e2a inbox over MCP: reading, composing, sending, replying, forwarding, handling attachments, managing contacts/outreach, scheduling mail, or using templates. Teaches correct threading, conversation correlation, concise multipart composition, and accepted/pending-review no-retry behavior.";
const diagnosisVocabulary = "diagnos(?:e|es|ed|ing|is|tic(?:s)?)?";

test("e2a-setup bootstraps MCP, inboxes, and optional custom domains", async () => {
  const [source, clients, customDomains, setupGuide, setupMirror] = await Promise.all([
    read("plugins/e2a/skills/e2a-setup/SKILL.md"),
    read("plugins/e2a/skills/e2a-setup/references/clients.md"),
    read("plugins/e2a/skills/e2a-setup/references/custom-domains.md"),
    read("plugins/e2a/docs/setup.md"),
    read("web/public/setup.md"),
  ]);
  assert.match(source, /^name: e2a-setup$/m);
  assert.match(source, /register.*https:\/\/api\.e2a\.dev\/mcp/i);
  assert.match(source, /plugin.*(?:disabled|reload).*duplicate/is);
  assert.match(source, /OAuth/i);
  assert.match(source, /never ask.*API key/i);
  assert.match(source, /whoami/);
  assert.match(source, /e2a MCP [`"]?whoami.*never.*(?:Unix|shell).*whoami/is);
  assert.match(source, /tool registry.*e2a tools/i);
  assert.match(source, /list_messages[\s\S]*?resume the user's original request/is);
  assert.match(clients, /MCP [`"]?whoami/i);
  assert.match(clients, /claude mcp add --transport http --scope user e2a https:\/\/api\.e2a\.dev\/mcp/);
  assert.match(clients, /codex mcp login e2a/);
  assert.match(clients, /mcpServers/);
  assert.match(clients, /Streamable HTTP/i);
  assert.match(source, /auth failures.*reauthorization/i);
  assert.match(source, /operational failures/i);
  assert.match(source, /confirm.*full.*address/i);
  assert.match(source, /agents\.e2a\.dev/);
  assert.match(source, /custom domain/i);
  assert.match(source, /one confirmation.*complete.*DNS diff/is);
  assert.match(source, /Cloudflare API MCP/i);
  assert.match(source, /GoDaddy.*gddy/is);
  assert.match(source, /GoDaddy MCP.*read-only/is);
  const report = section(source, "Completion report");
  for (const term of ["MCP", "scope", "inbox", "domain", "read"]) {
    assert.match(report, new RegExp(term, "i"));
  }
  assert.match(customDomains, /verify_domain[\s\S]*?get_domain/i);
  assert.match(
    customDomains,
    /get_domain[\s\S]*?inbound[\s\S]*?outbound[\s\S]*?complete.*branded address[\s\S]*?confirm[\s\S]*?create_agent[\s\S]*?list_messages/is,
  );
  assert.equal(setupMirror, setupGuide);
});

test("stable skill descriptions separate setup, integration, operation, and diagnosis", async () => {
  const sources = Object.fromEntries(await Promise.all(
    ["e2a", "e2a-setup", "e2a-integrate", "e2a-doctor"].map(async (name) => [
      name,
      await read(`plugins/e2a/skills/${name}/SKILL.md`),
    ]),
  ));
  const descriptions = Object.fromEntries(
    Object.entries(sources).map(([name, source]) => [name, description(source)]),
  );

  assert.equal(descriptions.e2a, e2aOperationDescription);
  assert.doesNotMatch(descriptions.e2a, new RegExp(`\\b(?:application|codebase|SDK|webhook|${diagnosisVocabulary}|failing|delivery)\\b`, "i"));

  assert.match(descriptions["e2a-setup"], /connect|authorize|create.*inbox/i);
  assert.doesNotMatch(descriptions["e2a-setup"], new RegExp(`\\b(?:SDK|webhook|${diagnosisVocabulary}|failing|delivery|send(?:ing)?|reply(?:ing)?|forward(?:ing)?)\\b`, "i"));

  assert.match(descriptions["e2a-integrate"], /application|codebase|SDK|webhook/i);
  assert.doesNotMatch(descriptions["e2a-integrate"], /\b(?:authorize|OAuth|create an agent inbox|custom email domain)\b/i);
  assert.doesNotMatch(descriptions["e2a-integrate"], new RegExp(`\\b(?:already-connected|contacts\\/outreach|templates|scheduling mail|${diagnosisVocabulary}|failing|delivery)\\b`, "i"));

  assert.match(descriptions["e2a-doctor"], /failing|diagnos|delivery/i);
  assert.doesNotMatch(descriptions["e2a-doctor"], /\b(?:application|codebase|SDK|webhook integration|OAuth|create an agent inbox|contacts\/outreach|templates|scheduling mail)\b/i);
});

test("e2a-integrate is language-aware and security-complete", async () => {
  const source = await read("plugins/e2a/skills/e2a-integrate/SKILL.md");
  assert.match(source, /^name: e2a-integrate$/m);
  assert.match(source, /outbound.*webhook.*polling/is);
  assert.match(source, /TypeScript.*Python.*official.*SDK/is);
  assert.match(source, /REST.*OpenAPI/is);
  assert.match(source, /never invent.*SDK/i);
  assert.match(source, /application-owned.*boundary/i);
  assert.match(source, /server-only.*credential/i);
  assert.match(source, /signature verification.*before/i);
  assert.match(source, /idempotent/i);
  assert.match(source, /synthetic/i);
  assert.match(source, /live smoke test.*separate/is);
});

test("e2a-integrate preserves SDK runtime and failure semantics", async () => {
  const source = await read("plugins/e2a/skills/e2a-integrate/references/sdk-recipes.md");
  assert.match(source, /E2AClient.*synchronous.*AsyncE2AClient.*asynchronous/is);
  assert.match(source, /timeoutMs.*timeout_ms/is);
  assert.match(source, /E2AError.*code.*retryable/is);
  assert.match(source, /separate.*accepted.*held.*delivery/is);
});

test("e2a-doctor is MCP-first, read-first, and repair-capable", async () => {
  const source = await read("plugins/e2a/skills/e2a-doctor/SKILL.md");
  assert.match(source, /^name: e2a-doctor$/m);
  assert.match(source, /MCP-first/i);
  assert.match(source, /read-only.*diagnos/is);
  for (const tool of [
    "whoami", "get_protection", "list_agent_suppressions", "get_domain",
    "list_webhook_deliveries", "get_message_lifecycle",
  ]) assert.match(source, new RegExp(tool));
  assert.match(source, /ranked.*evidence/i);
  assert.match(source, /confirmation.*each.*state-changing repair/is);
  assert.match(source, /e2a doctor --json/);
  assert.match(source, /never.*install.*CLI.*solely/is);
  assert.match(source, /accepted.*scheduled.*pending_review.*do not retry/is);
});
