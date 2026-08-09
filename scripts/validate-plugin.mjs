#!/usr/bin/env node
// Guardrail for the e2a core and e2a-labs plugins. A malformed manifest or
// skill silently fails to load in a target client, so this script turns those
// failures into a CI error.
//
// It checks, dependency-free:
//   1. Each plugin has exactly its declared client-manifest set.
//   2. Each plugin's client manifests agree on that plugin's own version.
//   3. Marketplace manifests parse and their sources resolve; core marketplace
//      metadata agrees with the independently versioned core plugin.
//   4. Every skill directory in every plugin has valid SKILL.md frontmatter.
//   5. Core alone owns the e2a MCP configuration; Labs cannot register one.
//   6. Numeric MCP-tool claims match the canonical tool-name catalog.

import { readFileSync, existsSync, readdirSync, statSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = join(dirname(fileURLToPath(import.meta.url)), "..");
const PLUGIN_DEFS = [
  {
    name: "e2a",
    dir: join(ROOT, "plugins", "e2a"),
    clients: [".claude-plugin", ".codex-plugin", ".cursor-plugin"],
    ownsMcp: true,
  },
  {
    name: "e2a-labs",
    dir: join(ROOT, "plugins", "e2a-labs"),
    clients: [".claude-plugin", ".codex-plugin"],
    ownsMcp: false,
  },
];

// Claude Code skill-name rules: lowercase letters, digits, hyphens; ≤64 chars.
const NAME_RE = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;
const MAX_NAME = 64;
const MAX_DESCRIPTION = 1024;
const FRONTMATTER_RE = /^---\r?\n([\s\S]*?)\r?\n---/;
const CLIENT_DIR_RE = /^\.(?:claude|codex|cursor)-plugin$/;

const errors = [];
const pluginResults = [];
const fail = (msg) => errors.push(msg);
const rel = (p) => p.slice(ROOT.length + 1);
const getAt = (obj, key) => key.split(".").reduce((o, k) => (o == null ? o : o[k]), obj);

function readJSON(absPath) {
  try {
    return JSON.parse(readFileSync(absPath, "utf8"));
  } catch (err) {
    fail(`${rel(absPath)}: ${err.message}`);
    return null;
  }
}

function clientManifestPath(plugin, client) {
  return join(plugin.dir, client, "plugin.json");
}

function validateMcpToolClaims(file, value, expectedCount) {
  if (Array.isArray(value)) {
    for (const item of value) validateMcpToolClaims(file, item, expectedCount);
    return;
  }
  if (!value || typeof value !== "object") return;
  for (const [key, item] of Object.entries(value)) {
    if (key === "description" && typeof item === "string") {
      for (const match of item.matchAll(/\b(\d+) MCP tools\b/g)) {
        const advertised = Number(match[1]);
        if (advertised !== expectedCount) {
          fail(`${rel(file)}: description advertises ${advertised} MCP tools; canonical catalog has ${expectedCount}`);
        }
      }
    }
    validateMcpToolClaims(file, item, expectedCount);
  }
}

function validateSkill(plugin, dir) {
  const file = join(plugin.dir, "skills", dir, "SKILL.md");
  if (!existsSync(file)) {
    fail(`${plugin.name}/${dir}: missing SKILL.md`);
    return;
  }
  const match = readFileSync(file, "utf8").match(FRONTMATTER_RE);
  if (!match) {
    fail(`${plugin.name}/${dir}: missing YAML frontmatter (--- ... ---)`);
    return;
  }
  const frontmatter = {};
  for (const line of match[1].split(/\r?\n/)) {
    const entry = line.match(/^([A-Za-z0-9_-]+):\s*(.*)$/);
    if (entry) frontmatter[entry[1]] = entry[2].trim();
  }

  const { name, description } = frontmatter;
  if (!name) {
    fail(`${plugin.name}/${dir}: SKILL.md frontmatter missing "name"`);
  } else {
    if (name !== dir) fail(`${plugin.name}/${dir}: skill name "${name}" must match its directory`);
    if (!NAME_RE.test(name)) fail(`${plugin.name}/${dir}: name "${name}" must be lowercase letters, digits, and hyphens`);
    if (name.length > MAX_NAME) fail(`${plugin.name}/${dir}: name "${name}" exceeds ${MAX_NAME} chars`);
  }
  if (!description) {
    fail(`${plugin.name}/${dir}: SKILL.md frontmatter missing "description"`);
  } else if (description.length > MAX_DESCRIPTION) {
    fail(`${plugin.name}/${dir}: description is ${description.length} chars (max ${MAX_DESCRIPTION})`);
  }
}

function validatePlugin(plugin, mcpToolCount) {
  if (!existsSync(plugin.dir) || !statSync(plugin.dir).isDirectory()) {
    fail(`missing plugin directory: ${rel(plugin.dir)}`);
    return;
  }

  const actualClients = readdirSync(plugin.dir, { withFileTypes: true })
    .filter((entry) => entry.isDirectory() && CLIENT_DIR_RE.test(entry.name))
    .map((entry) => entry.name)
    .sort();
  const expectedClients = [...plugin.clients].sort();
  if (JSON.stringify(actualClients) !== JSON.stringify(expectedClients)) {
    fail(`${plugin.name}: client manifests must be exactly ${expectedClients.join(", ")}; found ${actualClients.join(", ") || "none"}`);
  }

  const manifests = plugin.clients.map((client) => ({
    client,
    file: clientManifestPath(plugin, client),
    manifest: readJSON(clientManifestPath(plugin, client)),
  }));
  const claudeManifest = manifests.find(({ client }) => client === ".claude-plugin")?.manifest;
  const version = claudeManifest?.version;
  if (!version || typeof version !== "string") {
    fail(`${rel(clientManifestPath(plugin, ".claude-plugin"))}: missing string "version"`);
  }

  for (const { file, manifest } of manifests) {
    if (!manifest) continue;
    validateMcpToolClaims(file, manifest, mcpToolCount);
    for (const field of ["name", "description", "version"]) {
      if (!manifest[field]) fail(`${rel(file)}: missing required field "${field}"`);
    }
    if (manifest.name && manifest.name !== plugin.name) {
      fail(`${rel(file)}: name "${manifest.name}" must be "${plugin.name}"`);
    }
    if (version && manifest.version !== version) {
      fail(`${rel(file)}: version "${manifest.version}" != ${plugin.name} canonical "${version}" (.claude-plugin is source of truth)`);
    }
    for (const iconKey of ["icon", "logo"]) {
      const reference = manifest[iconKey] ?? manifest.interface?.composerIcon;
      if (reference && !existsSync(join(plugin.dir, reference.replace(/^\.\//, "")))) {
        fail(`${rel(file)}: ${iconKey} "${reference}" not found`);
      }
    }
    if (!plugin.ownsMcp && Object.hasOwn(manifest, "mcpServers")) {
      fail(`${rel(file)}: non-MCP-owner plugin must not declare "mcpServers"`);
    }
    if (plugin.ownsMcp && manifest.mcpServers !== "./.mcp.json") {
      fail(`${rel(file)}: MCP owner must reference "./.mcp.json"`);
    }
  }

  if (plugin.name === "e2a-labs" && claudeManifest) {
    const dependencies = claudeManifest.dependencies;
    if (!Array.isArray(dependencies) || dependencies.length !== 1 || dependencies[0] !== "e2a") {
      fail(`${rel(clientManifestPath(plugin, ".claude-plugin"))}: dependencies must be exactly ["e2a"]`);
    }
  }

  const mcpPath = join(plugin.dir, ".mcp.json");
  if (!plugin.ownsMcp && existsSync(mcpPath)) {
    fail(`${rel(mcpPath)}: non-MCP-owner plugin must not contain .mcp.json`);
  }
  if (plugin.ownsMcp) {
    if (!existsSync(mcpPath)) {
      fail(`${rel(mcpPath)}: MCP owner is missing .mcp.json`);
    } else {
      const mcp = readJSON(mcpPath);
      if (mcp && (!mcp.mcpServers || Object.keys(mcp.mcpServers).length === 0)) {
        fail(`${rel(mcpPath)}: "mcpServers" must define at least one server`);
      }
    }
  }

  const skillsDir = join(plugin.dir, "skills");
  const skillDirs = existsSync(skillsDir)
    ? readdirSync(skillsDir, { withFileTypes: true }).filter((entry) => entry.isDirectory()).map((entry) => entry.name).sort()
    : [];
  if (skillDirs.length === 0) fail(`${plugin.name}: no skills found under ${rel(skillsDir)}`);
  for (const dir of skillDirs) validateSkill(plugin, dir);
  pluginResults.push({ name: plugin.name, skills: skillDirs.length, version: version ?? "missing" });
}

const mcpToolCatalogFile = join(ROOT, "mcp", "tool-names.v1.json");
const mcpToolCatalog = readJSON(mcpToolCatalogFile);
if (!Array.isArray(mcpToolCatalog)) {
  fail(`${rel(mcpToolCatalogFile)}: canonical MCP tool catalog must be an array`);
}
const mcpToolCount = Array.isArray(mcpToolCatalog) ? mcpToolCatalog.length : 0;

for (const plugin of PLUGIN_DEFS) validatePlugin(plugin, mcpToolCount);

// Claude and Codex expose both packages; Cursor exposes only core. Marketplace
// metadata versions follow core, never the independently versioned Labs package.
const coreVersion = pluginResults.find(({ name }) => name === "e2a")?.version;
const MARKETPLACE_MANIFESTS = [
  {
    file: join(ROOT, ".claude-plugin", "marketplace.json"),
    pluginNames: ["e2a", "e2a-labs"],
    versionKey: "metadata.version",
  },
  {
    file: join(ROOT, ".agents", "plugins", "marketplace.json"),
    pluginNames: ["e2a", "e2a-labs"],
    versionKey: null,
  },
  {
    file: join(ROOT, ".cursor-plugin", "marketplace.json"),
    pluginNames: ["e2a"],
    versionKey: "metadata.version",
  },
];

for (const { file, pluginNames, versionKey } of MARKETPLACE_MANIFESTS) {
  if (!existsSync(file)) {
    fail(`missing marketplace manifest: ${rel(file)}`);
    continue;
  }
  const marketplace = readJSON(file);
  if (!marketplace) continue;
  validateMcpToolClaims(file, marketplace, mcpToolCount);
  if (!Array.isArray(marketplace.plugins) || marketplace.plugins.length === 0) {
    fail(`${rel(file)}: "plugins" must be a non-empty array`);
    continue;
  }
  const actualPluginNames = marketplace.plugins.map((entry) => entry.name);
  if (JSON.stringify(actualPluginNames) !== JSON.stringify(pluginNames)) {
    fail(`${rel(file)}: plugins must be exactly ${pluginNames.join(", ")}; found ${actualPluginNames.join(", ")}`);
  }
  for (const entry of marketplace.plugins) {
    const source = typeof entry.source === "string" ? entry.source : entry.source?.path;
    if (!source) {
      fail(`${rel(file)}: plugin "${entry.name}" has no source path`);
      continue;
    }
    const expectedSource = `./plugins/${entry.name}`;
    if (source !== expectedSource) {
      fail(`${rel(file)}: plugin "${entry.name}" source "${source}" must be "${expectedSource}"`);
    }
    const sourcePath = join(ROOT, source.replace(/^\.\//, ""));
    if (!existsSync(sourcePath) || !statSync(sourcePath).isDirectory()) {
      fail(`${rel(file)}: plugin "${entry.name}" source "${source}" is not a directory`);
    }
  }
  if (versionKey) {
    const version = getAt(marketplace, versionKey);
    if (coreVersion && version !== coreVersion) {
      fail(`${rel(file)}: ${versionKey} "${version}" != core e2a version "${coreVersion}"`);
    }
  }
}

if (errors.length > 0) {
  console.error(`\n✗ Plugin validation failed:\n${errors.map((error) => `  - ${error}`).join("\n")}\n`);
  process.exit(1);
}
console.log(`✓ Plugin valid: ${pluginResults.map(({ name, skills, version }) => `${name} (${skills} skill${skills === 1 ? "" : "s"}, version ${version})`).join("; ")}`);
