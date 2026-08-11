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
//   7. Every generated manifest still matches the per-package plugin.meta.json.
//   8. Each plugin's Agent Plugins v1 portable manifest (plugin.json; plus
//      mcp.json for the MCP-owning core) satisfies the parts of the closed
//      schemas that matter in practice.
//   9. No file in a skill interpolates a client-specific path variable — those
//      resolve on one client and land as literal text on every other.
//
// Run by the Package-and-manifests job in .github/workflows/plugin-tests.yml.

import { readFileSync, existsSync, readdirSync, statSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
// E2A_PLUGIN_TREE points the checks at a fixture tree instead of this
// repository, so scripts/validate-plugin.test.mjs can exercise the failure
// paths without mutating the real plugin. Unset in normal and CI use.
const ROOT = process.env.E2A_PLUGIN_TREE ?? join(SCRIPT_DIR, "..");
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
const typeOf = (v) => (Array.isArray(v) ? "array" : v === null ? "null" : typeof v);

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

// --- 9: no client-specific path variables in skills --------------------------

// A skill body is portable prose, but ${CLAUDE_PLUGIN_ROOT} only expands on
// Claude Code. Everywhere else the agent is handed a literal unexpanded path and
// the skill fails at the first command — silently, since the plugin still loads.
//
// Scan the WHOLE skill, not just SKILL.md: skills route the agent onward to
// references/ and scripts/, so a variable hiding one file deeper fails exactly
// the same way. Match only the interpolation forms (`${VAR}` / `$VAR`) so prose
// can still name the variable when documenting why it is gone.
const CLIENT_VARS = [
  ["CLAUDE_PLUGIN_ROOT", "Claude Code"],
  ["CODEX_PLUGIN_ROOT", "Codex"],
  ["CURSOR_PLUGIN_ROOT", "Cursor"],
];
const SCANNABLE = /\.(md|sh|bash|zsh|mjs|cjs|js|ts|py|rb|yml|yaml|toml|json|txt|example|tmpl)$/;

function skillFiles(dir, acc = []) {
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    if (e.name === "node_modules" || e.name === ".git") continue;
    const p = join(dir, e.name);
    if (e.isDirectory()) skillFiles(p, acc);
    else if (e.isFile() && SCANNABLE.test(e.name)) acc.push(p);
  }
  return acc;
}

function validateSkillPathVariables(plugin, skillsDir, skillDirs) {
  for (const dir of skillDirs) {
    const skillRoot = join(skillsDir, dir);
    if (!existsSync(skillRoot)) continue;
    for (const file of skillFiles(skillRoot)) {
      const body = readFileSync(file, "utf8");
      for (const [v, client] of CLIENT_VARS) {
        const interpolated = new RegExp(`\\$\\{${v}\\}|\\$${v}\\b`);
        if (interpolated.test(body)) {
          fail(
            `${rel(file)}: interpolates $${v}, which only expands on ${client} — ` +
              `describe the path relative to the skill's own directory instead`,
          );
        }
      }
    }
  }
}

// --- 8: Agent Plugins v1 portable pair ---------------------------------------

// Agent Plugins fixes both locations and closes both schemas, and the transport
// token differs from the one the legacy clients use ("streamable-http", not
// "http").
//
// On severity, since the two cases differ: an unknown top-level field is
// explicitly NON-fatal — §5.2 has clients report and ignore it and continue
// loading. Every other schema violation IS fatal: the client rejects the plugin
// and discovers none of its components. We reject stray keys anyway, as author
// hygiene rather than spec necessity — the closed schema exists to catch typos,
// and we control what the generator emits.
const AP_SPEC = "1.0.0";
const AP_PLUGIN_SCHEMA = `https://agent-plugins.org/schemas/${AP_SPEC}/plugin.schema.json`;
const AP_MCP_SCHEMA = `https://agent-plugins.org/schemas/${AP_SPEC}/mcp.schema.json`;
const AP_TOP_LEVEL = new Set([
  "$schema", "name", "version", "description", "author",
  "homepage", "repository", "license", "keywords", "extensions",
]);
const AP_NAME_RE = /^(?!.*(?:--|\.\.))[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$/;
const AP_TRANSPORTS = new Set(["stdio", "streamable-http", "sse"]);
const RDN_RE = /^[a-z0-9]+(?:[.-][a-z0-9]+)+$/;

// The whole 127.0.0.0/8 block is loopback, not just 127.0.0.1.
const isLoopbackHost = (hostname) => {
  const h = hostname.replace(/^\[|\]$/g, "");
  if (h === "localhost" || h === "::1") return true;
  const m = h.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  return m !== null && Number(m[1]) === 127 && m.slice(1).every((o) => Number(o) <= 255);
};

function validatePortableManifest(plugin) {
  const portablePath = join(plugin.dir, "plugin.json");
  const portable = existsSync(portablePath) ? readJSON(portablePath) : null;
  if (!portable) {
    fail(`${rel(portablePath)}: missing Agent Plugins v1 manifest`);
    return null;
  }
  if (portable.$schema !== AP_PLUGIN_SCHEMA) {
    fail(`${rel(portablePath)}: "$schema" must be "${AP_PLUGIN_SCHEMA}"`);
  }
  if (!portable.name || !AP_NAME_RE.test(portable.name) || portable.name.length > MAX_NAME) {
    fail(`${rel(portablePath)}: "name" violates the Agent Plugins v1 name rules`);
  }
  const strays = Object.keys(portable).filter((k) => !AP_TOP_LEVEL.has(k));
  if (strays.length > 0) {
    fail(
      `${rel(portablePath)}: non-portable top-level key(s) ${strays.join(", ")} — ` +
        `the v1 schema is closed; move client-specific data under "extensions"`,
    );
  }

  // Spec §5.2: every schema violation except an unknown top-level field or a
  // non-object "extensions" is FATAL — a conforming client rejects the entire
  // plugin rather than degrading. A wrong-typed field is therefore a
  // whole-plugin outage on a spec client, so type these rather than trusting
  // the generator's input.
  for (const key of ["version", "description", "homepage", "repository", "license"]) {
    if (key in portable && typeof portable[key] !== "string") {
      fail(`${rel(portablePath)}: "${key}" must be a string, got ${typeOf(portable[key])}`);
    }
  }
  if ("keywords" in portable) {
    if (!Array.isArray(portable.keywords) || portable.keywords.some((k) => typeof k !== "string")) {
      fail(`${rel(portablePath)}: "keywords" must be an array of strings`);
    }
  }
  if ("author" in portable) {
    const a = portable.author;
    if (typeOf(a) !== "object") {
      fail(`${rel(portablePath)}: "author" must be an object, got ${typeOf(a)}`);
    } else {
      for (const [k, v] of Object.entries(a)) {
        if (!["name", "email", "url"].includes(k)) {
          fail(`${rel(portablePath)}: author has no such field "${k}" (schema is closed)`);
        } else if (typeof v !== "string") {
          fail(`${rel(portablePath)}: author.${k} must be a string, got ${typeOf(v)}`);
        }
      }
    }
  }

  // Extension namespaces are client-owned (spec §3, §8). We are the plugin
  // author, not a client, so anything we emit here would be data no client
  // reads — reject rather than ship inert bytes.
  if ("extensions" in portable) {
    if (typeOf(portable.extensions) !== "object") {
      fail(`${rel(portablePath)}: "extensions" must be an object`);
    } else {
      for (const [ns, val] of Object.entries(portable.extensions)) {
        if (!RDN_RE.test(ns)) {
          fail(`${rel(portablePath)}: extensions namespace "${ns}" is not reverse-domain`);
        } else if (typeOf(val) !== "object") {
          fail(`${rel(portablePath)}: extensions["${ns}"] must be an object`);
        }
        if (/(^|\.)e2a(\.|$)/.test(ns)) {
          fail(
            `${rel(portablePath)}: extensions["${ns}"] is our own namespace — ` +
              `extension namespaces are client-owned, so no client will read it`,
          );
        }
      }
    }
  }
  return portable;
}

function validatePortableMcp(plugin, portable) {
  const portableMcpPath = join(plugin.dir, "mcp.json");
  const portableMcp = existsSync(portableMcpPath) ? readJSON(portableMcpPath) : null;
  if (!portableMcp) {
    fail(`${rel(portableMcpPath)}: missing Agent Plugins v1 MCP config`);
    return;
  }
  if (portableMcp.$schema !== AP_MCP_SCHEMA) {
    fail(`${rel(portableMcpPath)}: "$schema" must be "${AP_MCP_SCHEMA}"`);
  }
  // The spec permits any top-level key here no more than in plugin.json.
  for (const k of Object.keys(portableMcp)) {
    if (k !== "$schema" && k !== "mcpServers") {
      fail(`${rel(portableMcpPath)}: unexpected top-level key "${k}"`);
    }
  }
  if (portableMcp.mcpServers !== undefined && typeOf(portableMcp.mcpServers) !== "object") {
    fail(`${rel(portableMcpPath)}: "mcpServers" must be an object`);
  }
  const entries = Object.entries(portableMcp.mcpServers ?? {});
  // Stricter than the spec, which allows an empty map: a plugin whose whole
  // reason to exist is one MCP server should never ship zero of them.
  if (entries.length === 0) {
    fail(`${rel(portableMcpPath)}: "mcpServers" must define at least one server`);
  }
  for (const [id, cfg] of entries) {
    if (typeOf(cfg) !== "object") {
      fail(`${rel(portableMcpPath)}: server "${id}" must be an object, got ${typeOf(cfg)}`);
      continue;
    }
    if (!AP_TRANSPORTS.has(cfg.type)) {
      fail(`${rel(portableMcpPath)}: server "${id}" transport "${cfg.type}" is not a v1 transport`);
      continue;
    }

    if (cfg.type === "stdio") {
      if (typeof cfg.command !== "string" || cfg.command.length === 0) {
        fail(`${rel(portableMcpPath)}: stdio server "${id}" needs a "command"`);
      } else if (/\s/.test(cfg.command)) {
        fail(`${rel(portableMcpPath)}: stdio server "${id}" command must be one token, not a shell string`);
      }
      if (cfg.cwd !== undefined && !/^(\.\/|\$\{PLUGIN_(ROOT|DATA)\}(\/|$))/.test(cfg.cwd)) {
        fail(`${rel(portableMcpPath)}: stdio server "${id}" cwd must start with ./ or a reserved placeholder`);
      }
      continue;
    }

    // streamable-http / sse. `url` is required by the schema — a missing one
    // makes a client skip the entry silently, which is the exact
    // installs-clean-but-no-tools failure this check exists to prevent.
    if (typeof cfg.url !== "string" || cfg.url.length === 0) {
      fail(`${rel(portableMcpPath)}: server "${id}" (${cfg.type}) is missing its required "url"`);
      continue;
    }
    let u = null;
    try {
      u = new URL(cfg.url);
    } catch {
      fail(`${rel(portableMcpPath)}: server "${id}" has an unparseable url`);
    }
    if (u) {
      if (u.protocol !== "https:" && !isLoopbackHost(u.hostname)) {
        fail(`${rel(portableMcpPath)}: server "${id}" must use HTTPS off loopback`);
      }
      if (u.username || u.password) {
        fail(`${rel(portableMcpPath)}: server "${id}" embeds credentials in its url`);
      }
      if (u.hash) {
        fail(`${rel(portableMcpPath)}: server "${id}" url must not contain a fragment`);
      }
    }
    // Header names collide case-insensitively; a client picking either one
    // makes behavior depend on object order.
    const seen = new Map();
    for (const h of Object.keys(cfg.headers ?? {})) {
      const k = h.toLowerCase();
      if (seen.has(k)) {
        fail(`${rel(portableMcpPath)}: server "${id}" has duplicate header "${seen.get(k)}"/"${h}"`);
      }
      seen.set(k, h);
      if (/^(authorization|proxy-authorization|cookie|x-api-key)$/.test(k)) {
        fail(`${rel(portableMcpPath)}: server "${id}" header "${h}" carries a credential; headers are visible package data`);
      }
    }
  }
  // Both files declare the spec version; a mismatch disables MCP on load.
  const specOf = (s) => String(s).match(/schemas\/([\d.]+)\//)?.[1];
  if (portable && specOf(portable.$schema) !== specOf(portableMcp.$schema)) {
    fail(`${plugin.name}: plugin.json and mcp.json declare different Agent Plugins spec versions`);
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

  // Agent Plugins v1 portable pair: every plugin carries plugin.json; only the
  // MCP-owning core carries mcp.json.
  const portable = validatePortableManifest(plugin);
  const portableMcpPath = join(plugin.dir, "mcp.json");
  if (plugin.ownsMcp) {
    validatePortableMcp(plugin, portable);
  } else if (existsSync(portableMcpPath)) {
    fail(`${rel(portableMcpPath)}: non-MCP-owner plugin must not contain mcp.json`);
  }

  const skillsDir = join(plugin.dir, "skills");
  const skillDirs = existsSync(skillsDir)
    ? readdirSync(skillsDir, { withFileTypes: true }).filter((entry) => entry.isDirectory()).map((entry) => entry.name).sort()
    : [];
  if (skillDirs.length === 0) fail(`${plugin.name}: no skills found under ${rel(skillsDir)}`);
  for (const dir of skillDirs) validateSkill(plugin, dir);
  validateSkillPathVariables(plugin, skillsDir, skillDirs);
  pluginResults.push({ name: plugin.name, skills: skillDirs.length, version: version ?? "missing" });
}

const mcpToolCatalogFile = join(ROOT, "mcp", "tool-names.v1.json");
const mcpToolCatalog = readJSON(mcpToolCatalogFile);
if (!Array.isArray(mcpToolCatalog)) {
  fail(`${rel(mcpToolCatalogFile)}: canonical MCP tool catalog must be an array`);
}
const mcpToolCount = Array.isArray(mcpToolCatalog) ? mcpToolCatalog.length : 0;

for (const plugin of PLUGIN_DEFS) validatePlugin(plugin, mcpToolCount);

// --- 7: generated manifests are fresh ----------------------------------------

// Every manifest above is emitted from the per-package plugin.meta.json
// sources. Checking version parity after the fact (2) catches skew; this
// catches the hand edit that caused it, and names the file to fix.
const gen = spawnSync(
  process.execPath,
  [join(SCRIPT_DIR, "generate-plugin-manifests.mjs"), "--check"],
  { encoding: "utf8", env: { ...process.env, E2A_PLUGIN_TREE: ROOT } },
);
if (gen.status !== 0) {
  const detail = (gen.stderr || gen.stdout || "").trim().replace(/\n/g, "\n    ");
  fail(`generated manifests are stale:\n    ${detail}`);
}

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
console.log(
  `✓ Plugin valid: ${pluginResults.map(({ name, skills, version }) => `${name} (${skills} skill${skills === 1 ? "" : "s"}, version ${version})`).join("; ")}; ` +
    `manifests generated from plugin.meta.json, Agent Plugins v${AP_SPEC} portable manifests OK`,
);
