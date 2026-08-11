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
    "e2a", "e2a-doctor", "e2a-integrate", "e2a-setup", "email-evals",
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
  assert.equal(claudeMarket.metadata.version, "0.9.0");
  assert.equal(cursorMarket.metadata.version, "0.9.0");

  for (const client of [".claude-plugin", ".codex-plugin", ".cursor-plugin"]) {
    const core = JSON.parse(await readFile(`plugins/e2a/${client}/plugin.json`, "utf8"));
    assert.equal(core.version, "0.9.0");
  }
  for (const client of [".claude-plugin", ".codex-plugin"]) {
    const labs = JSON.parse(await readFile(`plugins/e2a-labs/${client}/plugin.json`, "utf8"));
    assert.equal(labs.version, "0.2.0");
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

test("core Codex description advertises every stable capability", async () => {
  const codex = JSON.parse(await readFile("plugins/e2a/.codex-plugin/plugin.json", "utf8"));
  assert.match(codex.description, /\b78 MCP tools\b/);
  assert.match(codex.interface.longDescription, /setup/i);
  assert.match(codex.interface.longDescription, /application integration/i);
  assert.match(codex.interface.longDescription, /inbox operation/i);
  assert.match(codex.interface.longDescription, /diagnosis/i);
  assert.match(codex.interface.longDescription, /evaluation/i);
});

test("Labs tracks the core plugin without requiring release tags", async () => {
  const labs = JSON.parse(await readFile("plugins/e2a-labs/.claude-plugin/plugin.json", "utf8"));
  assert.deepEqual(labs.dependencies, ["e2a"]);
});

test("plugin CI is consolidated into parallel package and skill lanes", async () => {
  const workflow = await readFile(".github/workflows/plugin-tests.yml", "utf8");
  const generalTests = await readFile(".github/workflows/test.yml", "utf8");

  await assert.rejects(access(".github/workflows/agentify-test.yml"));
  await assert.rejects(access(".github/workflows/agentify-lane-fixtures.yml"));

  assert.match(workflow, /^name: Plugin tests$/m);
  assert.match(workflow, /^  package:$/m);
  assert.match(workflow, /^  agentify:$/m);
  assert.match(workflow, /^  autopilot:$/m);
  assert.match(workflow, /^  tether:$/m);
  assert.match(workflow, /^  agentify-fixtures:$/m);
  assert.doesNotMatch(workflow, /^\s+needs:/m);
  assert.doesNotMatch(workflow, /^    paths(?:-ignore)?:/m);
  assert.match(
    workflow,
    /bash plugins\/e2a-labs\/skills\/tether\/tether\.sh _selftest/,
  );
  assert.match(
    workflow,
    /bash plugins\/e2a-labs\/skills\/agentify\/test\/run\.sh/,
  );
  assert.match(
    workflow,
    /node --test plugins\/e2a-labs\/skills\/autopilot\/test\/\*\.test\.mjs/,
  );
  assert.match(
    workflow,
    /bash plugins\/e2a-labs\/skills\/agentify\/test\/fixtures\/run-fixtures\.sh/,
  );
  assert.doesNotMatch(generalTests, /name: Plugin manifests/);
});

test("consolidated plugin CI preserves release gates and fixture isolation", async () => {
  const workflow = await readFile(".github/workflows/plugin-tests.yml", "utf8");
  const packageJob = workflow.match(/^  package:\n([\s\S]*?)(?=^  agentify:\n)/m)?.[1];
  assert.ok(packageJob, "package job must exist before the agentify job");
  const packageLines = packageJob.split("\n");

  const stepIndex = (step) => {
    const marker = `      - ${step}`;
    const matches = packageLines
      .map((line, index) => ({ line, index }))
      .filter(({ line }) => line === marker);
    assert.equal(matches.length, 1, `${step} must appear exactly once in the package job`);
    return matches[0].index;
  };
  const orderedSteps = [
    "uses: actions/checkout@v7",
    "uses: actions/setup-node@v7",
    "name: Require plugin version bumps in pull requests",
    "name: Require plugin version bumps pushed to main",
    "run: npm install --global \"@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}\"",
    "name: Install email-evals runtime dependencies",
  ].map(stepIndex);

  assert.match(workflow, /github\.event\.pull_request\.base\.sha/);
  assert.match(workflow, /github\.event\.before/);
  assert.deepEqual(orderedSteps, [...orderedSteps].sort((left, right) => left - right));
  assert.match(workflow, /plugins\/e2a-labs\/skills\/agentify\/templates\/runtime-skill/);
  assert.match(workflow, /plugins\/e2a-labs\/skills\/agentify\/templates\/workflows\/feedback-triage\.yml\.tmpl/);
  assert.match(workflow, /plugins\/e2a-labs\/skills\/agentify\/examples\/e2a\/autonomous-repo\.config\.yml/);
  assert.match(workflow, /plugins\/e2a-labs\/skills\/agentify\/test\/fixtures/);
  assert.match(workflow, /git diff --quiet "\$BASE_SHA"\.\.\.HEAD --/);
  assert.match(workflow, /CLAUDE_CODE_OAUTH_TOKEN: \$\{\{ secrets\.CLAUDE_CODE_OAUTH_TOKEN \}\}/);
  assert.match(workflow, /ANTHROPIC_API_KEY: \$\{\{ secrets\.ANTHROPIC_API_KEY \}\}/);
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
