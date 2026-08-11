import assert from "node:assert/strict";
import { access, readdir, readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { test } from "node:test";

const require = createRequire(import.meta.url);
const { parse: parseYaml } = require("../plugins/e2a/skills/email-evals/runtime/node_modules/yaml");

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
  const steps = parseYaml(workflow).jobs.package.steps;
  const onlyStep = (label, predicate) => {
    const matches = steps
      .map((step, index) => ({ step, index }))
      .filter(({ step }) => predicate(step));
    assert.equal(matches.length, 1, `${label} must appear exactly once in the package job`);
    return matches[0];
  };
  const checkout = onlyStep("checkout", (step) => step.uses === "actions/checkout@v7");
  const setupNode = onlyStep("Node setup", (step) => step.uses === "actions/setup-node@v7");
  const pullRequestGate = onlyStep(
    "pull-request version gate",
    (step) => step.name === "Require plugin version bumps in pull requests",
  );
  const pushGate = onlyStep(
    "push version gate",
    (step) => step.name === "Require plugin version bumps pushed to main",
  );
  const globalInstall = onlyStep(
    "Claude Code install",
    (step) => step.run === "npm install --global \"@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}\"",
  );
  const runtimeInstall = onlyStep(
    "email-evals runtime install",
    (step) => step.run === "npm ci --ignore-scripts --prefix plugins/e2a/skills/email-evals/runtime",
  );

  assert.deepEqual(checkout.step.with, { "fetch-depth": 0 });
  assert.ok(checkout.index < setupNode.index);
  for (const gate of [pullRequestGate, pushGate]) {
    assert.ok(setupNode.index < gate.index);
    assert.ok(gate.index < globalInstall.index);
    assert.ok(gate.index < runtimeInstall.index);
    assert.equal(gate.step.run, "node scripts/check-plugin-version-bump.mjs \"$PLUGIN_VERSION_BASE\"");
  }
  assert.equal(pullRequestGate.step.if, "github.event_name == 'pull_request'");
  assert.deepEqual(pullRequestGate.step.env, {
    PLUGIN_VERSION_BASE: "${{ github.event.pull_request.base.sha }}",
  });
  assert.equal(
    pushGate.step.if,
    "github.event_name == 'push' && github.event.before != '0000000000000000000000000000000000000000'",
  );
  assert.deepEqual(pushGate.step.env, { PLUGIN_VERSION_BASE: "${{ github.event.before }}" });
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
