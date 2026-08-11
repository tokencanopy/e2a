#!/usr/bin/env node
// Generates every plugin and marketplace manifest from the per-package
// metadata sources: plugins/e2a/plugin.meta.json and
// plugins/e2a-labs/plugin.meta.json.
//
// Four clients read four different manifest shapes per plugin, and three
// marketplaces repeat the same metadata again. Hand-maintaining those files
// means a version bump or a copy edit lands in some of them and skews the
// rest — the old guardrail could only notice the skew after the fact.
// Generating removes the class.
//
// Shapes emitted, per plugin package:
//   plugins/<name>/plugin.json            Agent Plugins v1.0.0 portable manifest
//   plugins/<name>/mcp.json               Agent Plugins v1.0.0 portable MCP config (core only)
//   plugins/<name>/.mcp.json              legacy MCP config (core only)
//   plugins/<name>/.claude-plugin/plugin.json
//   plugins/<name>/.codex-plugin/plugin.json
//   plugins/<name>/.cursor-plugin/plugin.json (core only — Labs has no Cursor client)
// and the three shared marketplaces, which list every plugin:
//   .claude-plugin/marketplace.json
//   .cursor-plugin/marketplace.json       (core only — Labs is not on Cursor)
//   .agents/plugins/marketplace.json
//
// Usage:
//   node scripts/generate-plugin-manifests.mjs           write
//   node scripts/generate-plugin-manifests.mjs --check   fail if any file is stale
//
// The --check mode runs in the Package-and-manifests job of
// .github/workflows/plugin-tests.yml via scripts/validate-plugin.mjs.

import { readFileSync, writeFileSync, existsSync, mkdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

// E2A_PLUGIN_TREE points generation at a fixture tree instead of this
// repository (see scripts/validate-plugin.test.mjs). Unset in normal and CI use.
const ROOT = process.env.E2A_PLUGIN_TREE ?? join(dirname(fileURLToPath(import.meta.url)), "..");

const PLUGIN_DEFS = [
  {
    name: "e2a",
    clients: [".claude-plugin", ".codex-plugin", ".cursor-plugin"],
    ownsMcp: true,
  },
  {
    name: "e2a-labs",
    clients: [".claude-plugin", ".codex-plugin"],
    ownsMcp: false,
  },
];

const AGENT_PLUGINS_SPEC = "1.0.0";
const PLUGIN_SCHEMA = `https://agent-plugins.org/schemas/${AGENT_PLUGINS_SPEC}/plugin.schema.json`;
const MCP_SCHEMA = `https://agent-plugins.org/schemas/${AGENT_PLUGINS_SPEC}/mcp.schema.json`;

const pluginDir = (name) => join(ROOT, "plugins", name);
const readMeta = (name) => JSON.parse(readFileSync(join(pluginDir(name), "plugin.meta.json"), "utf8"));

const metas = Object.fromEntries(PLUGIN_DEFS.map((def) => [def.name, readMeta(def.name)]));
const core = metas.e2a;

// Strip the "$comment" keys the sources use for maintainer notes.
const clean = (o) => Object.fromEntries(Object.entries(o).filter(([k]) => k !== "$comment"));

// ---------------------------------------------------------------- generators

// Agent Plugins v1: closed schema. Only $schema, name, version, description,
// author, homepage, repository, license, keywords, extensions may appear at the
// top level.
//
// No `extensions` block. Spec §3 defines an extension namespace as a
// CLIENT-owned identifier and §8 gives its owning client sole authority over
// the contents; the upstream migration guidance is explicit that you must not
// invent a namespace and expect an unrelated client to load it. e2a is the
// plugin author, not a client, so `dev.e2a.*` would be inert data no client
// ever reads. displayName and icon are client-specific presentation and already
// live in the client manifests, which is where they belong.
const portableManifest = (meta) => ({
  $schema: PLUGIN_SCHEMA,
  name: meta.name,
  version: meta.version,
  description: meta.descriptions.manifest,
  author: { name: meta.author.name, url: meta.author.url },
  homepage: meta.homepage,
  repository: meta.repository,
  license: meta.license,
  keywords: meta.keywords,
});

// Agent Plugins v1 MCP config. The transport token is "streamable-http"; the
// legacy clients spell the same transport "http".
const portableMcp = (meta) => ({
  $schema: MCP_SCHEMA,
  mcpServers: {
    [meta.mcp.id]: { type: "streamable-http", url: meta.mcp.url },
  },
});

const legacyMcp = (meta) => ({
  mcpServers: {
    [meta.mcp.id]: { type: "http", url: meta.mcp.url },
  },
});

const claudeManifest = (meta, def) => ({
  name: meta.name,
  displayName: meta.displayName,
  version: meta.version,
  description: meta.descriptions.manifest,
  author: { name: meta.author.name, url: meta.author.url },
  homepage: meta.homepage,
  repository: meta.repository,
  license: meta.license,
  keywords: meta.keywords,
  ...(def.ownsMcp ? { mcpServers: "./.mcp.json" } : {}),
  ...(meta.dependencies ? { dependencies: meta.dependencies } : {}),
});

const codexManifest = (meta, def) => ({
  name: meta.name,
  displayName: meta.displayName,
  version: meta.version,
  description: meta.descriptions.codex ?? meta.descriptions.manifest,
  author: { name: meta.author.name },
  homepage: meta.homepage,
  repository: meta.repository,
  license: meta.license,
  keywords: meta.keywords,
  skills: "./skills/",
  ...(def.ownsMcp ? { mcpServers: "./.mcp.json" } : {}),
  interface: {
    displayName: meta.displayName,
    shortDescription: meta.codexInterface.shortDescription,
    longDescription: meta.codexInterface.longDescription,
    developerName: meta.codexInterface.developerName,
    category: meta.codexInterface.category,
    websiteURL: meta.homepage,
    composerIcon: meta.icon,
  },
});

const cursorManifest = (meta) => ({
  name: meta.name,
  displayName: meta.displayName,
  version: meta.version,
  description: meta.descriptions.manifest,
  author: { name: meta.author.name, url: meta.author.url },
  license: meta.license,
  keywords: [...meta.keywords, core.marketplaces.cursor.extraKeyword],
  logo: meta.icon.replace(/^\.\//, ""),
  mcpServers: "./.mcp.json",
});

// Marketplaces are shared outputs: each lists every plugin it carries, so they
// are built from all metadata sources at once. Marketplace-level metadata
// (blurb, version) always follows the core package.
const claudeMarketplace = () => ({
  name: core.name,
  owner: { name: core.author.name, email: core.author.email, url: core.author.url },
  metadata: { description: core.marketplaces.claude.blurb, version: core.version },
  plugins: PLUGIN_DEFS.map((def) => {
    const meta = metas[def.name];
    return {
      name: meta.name,
      description: meta.descriptions.marketplaceLong,
      category: meta.marketplaces.claude.category,
      source: meta.source,
      homepage: meta.homepage,
    };
  }),
});

// Cursor carries core only — Labs has no Cursor client.
const cursorMarketplace = () => ({
  name: core.name,
  owner: { name: core.author.name, url: core.author.url },
  metadata: { description: core.marketplaces.cursor.blurb, version: core.version },
  plugins: PLUGIN_DEFS.filter((def) => def.clients.includes(".cursor-plugin")).map((def) => {
    const meta = metas[def.name];
    return { name: meta.name, source: meta.source, description: meta.descriptions.marketplaceShort };
  }),
});

const agentsMarketplace = () => ({
  name: core.name,
  interface: { displayName: core.displayName },
  plugins: PLUGIN_DEFS.map((def) => {
    const meta = metas[def.name];
    return {
      name: meta.name,
      source: { source: "local", path: meta.source },
      policy: { installation: "AVAILABLE", authentication: "ON_USE" },
      category: meta.marketplaces.agents.category,
      description: meta.descriptions.marketplaceShort,
    };
  }),
});

const OUTPUTS = [];

for (const def of PLUGIN_DEFS) {
  const meta = metas[def.name];
  const dir = pluginDir(def.name);
  OUTPUTS.push([join(dir, "plugin.json"), () => portableManifest(meta)]);
  if (def.ownsMcp) {
    OUTPUTS.push([join(dir, "mcp.json"), () => portableMcp(meta)]);
    OUTPUTS.push([join(dir, ".mcp.json"), () => legacyMcp(meta)]);
  }
  for (const client of def.clients) {
    const build = {
      ".claude-plugin": () => claudeManifest(meta, def),
      ".codex-plugin": () => codexManifest(meta, def),
      ".cursor-plugin": () => cursorManifest(meta),
    }[client];
    OUTPUTS.push([join(dir, client, "plugin.json"), build]);
  }
}

OUTPUTS.push([join(ROOT, ".claude-plugin", "marketplace.json"), claudeMarketplace]);
OUTPUTS.push([join(ROOT, ".cursor-plugin", "marketplace.json"), cursorMarketplace]);
OUTPUTS.push([join(ROOT, ".agents", "plugins", "marketplace.json"), agentsMarketplace]);

// -------------------------------------------------------------------- driver

const check = process.argv.includes("--check");
const rel = (p) => p.slice(ROOT.length + 1);
const render = (obj) => JSON.stringify(clean(obj), null, 2) + "\n";

const stale = [];
let written = 0;

for (const [path, build] of OUTPUTS) {
  const next = render(build());
  const current = existsSync(path) ? readFileSync(path, "utf8") : null;
  if (current === next) continue;

  if (check) {
    stale.push({ path: rel(path), reason: current === null ? "missing" : "stale" });
    continue;
  }
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, next);
  console.log(`  ${current === null ? "created" : "updated"}  ${rel(path)}`);
  written += 1;
}

const SOURCES = PLUGIN_DEFS.map((def) => `plugins/${def.name}/plugin.meta.json`).join(" + ");

if (check) {
  if (stale.length > 0) {
    console.error(
      `\n✗ Generated manifests are out of date:\n` +
        stale.map((s) => `  - ${s.path} (${s.reason})`).join("\n") +
        `\n\n  Run: node scripts/generate-plugin-manifests.mjs\n` +
        `  Edit ${SOURCES}, never a generated manifest.\n`,
    );
    process.exit(1);
  }
  console.log(`✓ All ${OUTPUTS.length} generated manifests match ${SOURCES}`);
} else {
  console.log(
    written === 0
      ? `✓ All ${OUTPUTS.length} manifests already current`
      : `✓ Generated ${written}/${OUTPUTS.length} manifest(s) from ${SOURCES}`,
  );
}
