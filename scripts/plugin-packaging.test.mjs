import assert from "node:assert/strict";
import { access, readdir, readFile } from "node:fs/promises";
import { test } from "node:test";

const skillNames = async (plugin) => (await readdir(`plugins/${plugin}/skills`, { withFileTypes: true }))
  .filter((entry) => entry.isDirectory())
  .map((entry) => entry.name)
  .sort();

test("experimental workflows live only in e2a-labs", async () => {
  assert.deepEqual(await skillNames("e2a-labs"), ["agentify", "autopilot", "tether"]);
  const core = await skillNames("e2a");
  assert.ok(core.includes("e2a"));
  for (const experimental of ["agentify", "autopilot", "tether"]) {
    assert.ok(!core.includes(experimental), `${experimental} leaked into core`);
  }
});

test("only core registers the e2a MCP server", async () => {
  await access("plugins/e2a/.mcp.json");
  await assert.rejects(access("plugins/e2a-labs/.mcp.json"));
  for (const client of [".claude-plugin", ".codex-plugin"]) {
    const labs = JSON.parse(await readFile(`plugins/e2a-labs/${client}/plugin.json`, "utf8"));
    assert.equal(labs.mcpServers, undefined);
  }
});
