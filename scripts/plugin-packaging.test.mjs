import assert from "node:assert/strict";
import { access, readdir, readFile } from "node:fs/promises";
import { test } from "node:test";

const skillNames = async (plugin) => (await readdir(`plugins/${plugin}/skills`, { withFileTypes: true }))
  .filter((entry) => entry.isDirectory())
  .map((entry) => entry.name)
  .sort();

test("core and experimental workflows have exact skill ownership", async () => {
  assert.deepEqual(await skillNames("e2a-labs"), ["agentify", "autopilot", "tether"]);
  assert.deepEqual(await skillNames("e2a"), [
    "e2a", "e2a-doctor", "e2a-integrate", "e2a-setup",
  ]);
});

test("only core registers the e2a MCP server", async () => {
  await access("plugins/e2a/.mcp.json");
  await assert.rejects(access("plugins/e2a-labs/.mcp.json"));
  for (const client of [".claude-plugin", ".codex-plugin"]) {
    const labs = JSON.parse(await readFile(`plugins/e2a-labs/${client}/plugin.json`, "utf8"));
    assert.equal(labs.mcpServers, undefined);
  }
});

test("marketplaces expose the supported plugin set and release versions", async () => {
  const claudeMarket = JSON.parse(await readFile(".claude-plugin/marketplace.json", "utf8"));
  const codexMarket = JSON.parse(await readFile(".agents/plugins/marketplace.json", "utf8"));
  const cursorMarket = JSON.parse(await readFile(".cursor-plugin/marketplace.json", "utf8"));

  assert.deepEqual(claudeMarket.plugins.map((plugin) => plugin.name).sort(), ["e2a", "e2a-labs"]);
  assert.deepEqual(codexMarket.plugins.map((plugin) => plugin.name).sort(), ["e2a", "e2a-labs"]);
  assert.deepEqual(cursorMarket.plugins.map((plugin) => plugin.name), ["e2a"]);
  assert.equal(claudeMarket.metadata.version, "0.7.0");
  assert.equal(cursorMarket.metadata.version, "0.7.0");

  for (const client of [".claude-plugin", ".codex-plugin", ".cursor-plugin"]) {
    const core = JSON.parse(await readFile(`plugins/e2a/${client}/plugin.json`, "utf8"));
    assert.equal(core.version, "0.7.0");
  }
  for (const client of [".claude-plugin", ".codex-plugin"]) {
    const labs = JSON.parse(await readFile(`plugins/e2a-labs/${client}/plugin.json`, "utf8"));
    assert.equal(labs.version, "0.1.0");
  }
});

test("client manifests retain only their supported visual fields", async () => {
  const claude = JSON.parse(await readFile("plugins/e2a/.claude-plugin/plugin.json", "utf8"));
  const codex = JSON.parse(await readFile("plugins/e2a/.codex-plugin/plugin.json", "utf8"));
  const cursor = JSON.parse(await readFile("plugins/e2a/.cursor-plugin/plugin.json", "utf8"));

  assert.equal(claude.icon, undefined);
  assert.equal(codex.interface.composerIcon, "./assets/icon.svg");
  assert.equal(cursor.logo, "assets/icon.svg");
});

test("Labs pins its Claude dependency to the compatible core release", async () => {
  const labs = JSON.parse(await readFile("plugins/e2a-labs/.claude-plugin/plugin.json", "utf8"));
  assert.deepEqual(labs.dependencies, [{ name: "e2a", version: "^0.7.0" }]);
});

test("migration guidance refreshes core and keeps the Cursor boundary conservative", async () => {
  const coreReadme = await readFile("plugins/e2a/README.md", "utf8");
  const labsReadme = await readFile("plugins/e2a-labs/README.md", "utf8");

  assert.match(
    labsReadme,
    /codex plugin marketplace upgrade e2a\ncodex plugin add e2a@e2a\ncodex plugin add e2a-labs@e2a/,
  );
  assert.match(
    coreReadme,
    /Cursor lists core and receives its MCP configuration and canonical setup\s+documentation/,
  );
  assert.match(coreReadme, /Labs is not listed, so Labs skills are unavailable in Cursor/);
  assert.doesNotMatch(coreReadme, /does not deliver the core or Labs skills/);
});
