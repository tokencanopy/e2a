// Regression tests for the plugin guardrails in scripts/validate-plugin.mjs.
//
// Every case here is a real hole that shipped in the first Agent Plugins
// conformance pass and was found in review. They share one shape: the plugin
// installs, the validator says OK, and the failure only appears later on a
// client — a silently dead MCP server, a plugin a conforming client rejects
// outright, or a skill instruction that resolves on exactly one client.
//
// The tests build a throwaway plugin tree and run the real scripts against it
// via E2A_PLUGIN_TREE, so they never touch plugins/e2a or plugins/e2a-labs.

import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync, cpSync } from "node:fs";
import { join, dirname } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const REPO = join(SCRIPT_DIR, "..");
const REAL_CORE = join(REPO, "plugins", "e2a");
const REAL_LABS = join(REPO, "plugins", "e2a-labs");

// Minimal but valid metadata sources; each test mutates one thing.
const baseCoreMeta = () => ({
  name: "e2a",
  displayName: "e2a",
  version: "0.7.0",
  license: "Apache-2.0",
  homepage: "https://e2a.dev",
  repository: "https://github.com/tokencanopy/e2a",
  icon: "./assets/icon.svg",
  author: { name: "TokenCanopy", url: "https://e2a.dev", email: "support@e2a.dev" },
  keywords: ["email", "mcp"],
  descriptions: { manifest: "d", marketplaceLong: "d", marketplaceShort: "d" },
  mcp: { id: "e2a", url: "https://api.e2a.dev/mcp" },
  codexInterface: {
    shortDescription: "s",
    longDescription: "l",
    developerName: "TokenCanopy",
    category: "Productivity",
  },
  marketplaces: {
    claude: { blurb: "b", category: "productivity" },
    cursor: { blurb: "b", extraKeyword: "cursor" },
    agents: { category: "Productivity" },
  },
  source: "./plugins/e2a",
});

const baseLabsMeta = () => ({
  name: "e2a-labs",
  displayName: "e2a Labs",
  version: "0.1.0",
  license: "Apache-2.0",
  homepage: "https://e2a.dev",
  repository: "https://github.com/tokencanopy/e2a",
  icon: "./assets/icon.svg",
  author: { name: "TokenCanopy", url: "https://e2a.dev", email: "support@e2a.dev" },
  keywords: ["email", "agents"],
  descriptions: { manifest: "d", codex: "d", marketplaceLong: "d", marketplaceShort: "d" },
  dependencies: ["e2a"],
  codexInterface: {
    shortDescription: "s",
    longDescription: "l",
    developerName: "TokenCanopy",
    category: "Productivity",
  },
  marketplaces: {
    claude: { category: "productivity" },
    agents: { category: "Productivity" },
  },
  source: "./plugins/e2a-labs",
});

/**
 * Build a fixture tree with both plugin packages, generate its manifests,
 * then validate it.
 * @param mutate  applied to the core metadata source before generation
 * @param extraFiles  { "<path under the tree root>": "contents" }
 */
function validateFixture(mutate = (m) => m, extraFiles = {}) {
  const tree = mkdtempSync(join(tmpdir(), "e2a-plugin-fixture-"));
  try {
    scaffoldTree(tree, mutate(baseCoreMeta()));

    const env = { ...process.env, E2A_PLUGIN_TREE: tree };
    const gen = spawnSync(
      process.execPath,
      [join(SCRIPT_DIR, "generate-plugin-manifests.mjs")],
      { encoding: "utf8", env },
    );
    assert.equal(gen.status, 0, `fixture generation failed: ${gen.stderr}`);

    for (const [relPath, contents] of Object.entries(extraFiles)) {
      const abs = join(tree, relPath);
      mkdirSync(dirname(abs), { recursive: true });
      writeFileSync(abs, contents);
    }

    const res = spawnSync(process.execPath, [join(SCRIPT_DIR, "validate-plugin.mjs")], {
      encoding: "utf8",
      env,
    });
    return { code: res.status, out: `${res.stdout}${res.stderr}`, tree };
  } finally {
    rmSync(tree, { recursive: true, force: true });
  }
}

// Both plugin packages plus the MCP tool catalog — the validator walks all of
// them. Real skills and assets are copied so the fixture stays honest about
// the tree it is standing in for (frontmatter, icons, path-variable scan).
function scaffoldTree(tree, coreMeta) {
  for (const [name, realDir, meta] of [
    ["e2a", REAL_CORE, coreMeta],
    ["e2a-labs", REAL_LABS, baseLabsMeta()],
  ]) {
    const pluginDir = join(tree, "plugins", name);
    mkdirSync(pluginDir, { recursive: true });
    cpSync(join(realDir, "skills"), join(pluginDir, "skills"), { recursive: true });
    cpSync(join(realDir, "assets"), join(pluginDir, "assets"), { recursive: true });
    writeFileSync(join(pluginDir, "plugin.meta.json"), JSON.stringify(meta, null, 2) + "\n");
  }
  mkdirSync(join(tree, "mcp"), { recursive: true });
  cpSync(join(REPO, "mcp", "tool-names.v1.json"), join(tree, "mcp", "tool-names.v1.json"));
}

test("a well-formed tree passes", () => {
  const { code, out } = validateFixture();
  assert.equal(code, 0, out);
});

test("an http server with no url is rejected", () => {
  // JSON.stringify drops undefined, so a missing url reaches mcp.json as an
  // absent key — a server entry a conforming client skips in silence.
  const { code, out } = validateFixture((m) => {
    delete m.mcp.url;
    return m;
  });
  assert.equal(code, 1, "missing url must fail");
  assert.match(out, /missing its required "url"/);
});

test("a url with a fragment is rejected", () => {
  const { code, out } = validateFixture((m) => {
    m.mcp.url = "https://api.e2a.dev/mcp#frag";
    return m;
  });
  assert.equal(code, 1, "fragment must fail");
  assert.match(out, /must not contain a fragment/);
});

test("a non-loopback plaintext url is rejected", () => {
  const { code, out } = validateFixture((m) => {
    m.mcp.url = "http://api.e2a.dev/mcp";
    return m;
  });
  assert.equal(code, 1, "plaintext must fail");
  assert.match(out, /must use HTTPS off loopback/);
});

test("a loopback url may be plaintext, anywhere in 127.0.0.0/8", () => {
  // The whole /8 is loopback, not just 127.0.0.1.
  const { code, out } = validateFixture((m) => {
    m.mcp.url = "http://127.0.0.2:8765/mcp";
    return m;
  });
  assert.equal(code, 0, out);
});

test("a non-object server entry fails cleanly instead of crashing", () => {
  // Reading cfg.type before a type guard threw an uncaught TypeError, which is
  // a crash rather than a diagnostic — the validator has to survive whatever a
  // hand edit leaves behind and say what is wrong.
  const { code, out } = validateFixture(undefined, {
    "plugins/e2a/mcp.json": JSON.stringify(
      {
        $schema: "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
        mcpServers: { e2a: null },
      },
      null,
      2,
    ) + "\n",
  });
  assert.equal(code, 1, "null server entry must fail");
  assert.doesNotMatch(out, /TypeError/, "must not crash");
  assert.match(out, /server "e2a" must be an object, got null/);
});

test("an unexpected top-level key in mcp.json is rejected", () => {
  const { code, out } = validateFixture(undefined, {
    "plugins/e2a/mcp.json": JSON.stringify(
      {
        $schema: "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
        mcpServers: { e2a: { type: "streamable-http", url: "https://api.e2a.dev/mcp" } },
        servers: {},
      },
      null,
      2,
    ) + "\n",
  });
  assert.equal(code, 1, "stray key must fail");
  assert.match(out, /unexpected top-level key "servers"/);
});

test("wrong-typed manifest metadata is rejected", () => {
  // Spec §5.2: any schema violation beyond an unknown top-level field is fatal,
  // so a conforming client rejects the whole plugin rather than degrading.
  const { code, out } = validateFixture((m) => {
    m.author.name = 123;
    return m;
  });
  assert.equal(code, 1, "wrong-typed author.name must fail");
  assert.match(out, /author\.name must be a string, got number/);
});

test("a client path variable hidden outside SKILL.md is rejected", () => {
  const { code, out } = validateFixture(undefined, {
    "plugins/e2a-labs/skills/agentify/references/adapters.md":
      "Run `${CLAUDE_PLUGIN_ROOT}/skills/agentify/agentify-render.sh`.\n",
  });
  assert.equal(code, 1, "variable in references/ must fail");
  assert.match(out, /interpolates \$CLAUDE_PLUGIN_ROOT/);
});

test("prose may name a client variable without interpolating it", () => {
  // The guard must not stop a skill from documenting why the variable is gone.
  const { code, out } = validateFixture(undefined, {
    "plugins/e2a-labs/skills/agentify/references/history.md":
      "Paths used to be written with CLAUDE_PLUGIN_ROOT; they are now relative.\n",
  });
  assert.equal(code, 0, out);
});

test("a hand-edited generated manifest fails the freshness gate", () => {
  const tree = mkdtempSync(join(tmpdir(), "e2a-plugin-fixture-"));
  try {
    scaffoldTree(tree, baseCoreMeta());

    const env = { ...process.env, E2A_PLUGIN_TREE: tree };
    spawnSync(process.execPath, [join(SCRIPT_DIR, "generate-plugin-manifests.mjs")], { env });

    // Bump the version in one generated manifest, the way a hand edit would.
    const target = join(tree, "plugins", "e2a", ".claude-plugin", "plugin.json");
    const edited = JSON.parse(readFileSync(target, "utf8"));
    edited.version = "9.9.9";
    writeFileSync(target, JSON.stringify(edited, null, 2) + "\n");

    const res = spawnSync(process.execPath, [join(SCRIPT_DIR, "validate-plugin.mjs")], {
      encoding: "utf8",
      env,
    });
    assert.equal(res.status, 1, "hand edit must fail");
    assert.match(`${res.stdout}${res.stderr}`, /out of date|stale/);
  } finally {
    rmSync(tree, { recursive: true, force: true });
  }
});

test("an extensions namespace we own is rejected", () => {
  // Extension namespaces are client-owned (spec §3, §8); one of ours would be
  // data no client ever reads.
  const tree = mkdtempSync(join(tmpdir(), "e2a-plugin-fixture-"));
  try {
    scaffoldTree(tree, baseCoreMeta());

    const env = { ...process.env, E2A_PLUGIN_TREE: tree };
    spawnSync(process.execPath, [join(SCRIPT_DIR, "generate-plugin-manifests.mjs")], { env });

    const target = join(tree, "plugins", "e2a", "plugin.json");
    const manifest = JSON.parse(readFileSync(target, "utf8"));
    manifest.extensions = { "dev.e2a.plugin": { displayName: "e2a" } };
    writeFileSync(target, JSON.stringify(manifest, null, 2) + "\n");

    const res = spawnSync(process.execPath, [join(SCRIPT_DIR, "validate-plugin.mjs")], {
      encoding: "utf8",
      env,
    });
    assert.equal(res.status, 1, "own-namespace extension must fail");
    assert.match(`${res.stdout}${res.stderr}`, /our own namespace|out of date|stale/);
  } finally {
    rmSync(tree, { recursive: true, force: true });
  }
});
