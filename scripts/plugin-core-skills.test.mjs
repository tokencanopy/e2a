import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

const read = (path) => readFile(path, "utf8");
const section = (source, heading) =>
  source.split(`## ${heading}\n`)[1]?.split(/\n## /)[0] ?? "";

test("e2a-setup bootstraps MCP, inboxes, and optional custom domains", async () => {
  const [source, customDomains, setupGuide, setupMirror] = await Promise.all([
    read("plugins/e2a/skills/e2a-setup/SKILL.md"),
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
