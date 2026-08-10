# Agent Plugins v1.0.0 conformance for `plugins/e2a` and `plugins/e2a-labs`

**Status:** implemented (additive; no client behavior changed)
**Date:** 2026-08-09; updated 2026-08-10 for the core/Labs split

> **2026-08-10 update — ported onto the two-package layout.** This change was
> written when every skill lived in `plugins/e2a`. Before it landed, #885 split
> the plugin: the four stable skills stayed in core `plugins/e2a`, and Agentify,
> Autopilot, and Tether moved to the experimental `plugins/e2a-labs`. The
> conformance work ported with them:
>
> - Each package now has its own `plugin.meta.json` and its own generated
>   portable `plugin.json`. `mcp.json` (and legacy `.mcp.json`) exist only in
>   core, which remains the sole MCP owner; Labs ships no MCP config at all.
> - `scripts/generate-plugin-manifests.mjs` is per-package: it reads both
>   metadata sources, emits each package's manifests, and builds the three
>   shared marketplaces (which list both plugins; Cursor lists core only).
> - The path-portability rewrite (change 3 below) applied to the three skills'
>   new home under `plugins/e2a-labs/skills/`.
>
> The body below is the original document, with the layout-specific passages
> corrected; the reasoning is unchanged.

## Why

[Agent Plugins](https://agent-plugins.org/) 1.0.0 is a vendor-neutral packaging
spec for distributing Agent Skills and MCP servers, governed by a TSC drawn from
Amazon, Cursor, Microsoft, OpenAI, and Vercel. The project's site lists Codex,
ChatGPT, Cursor, GitHub Copilot, Kiro, and VS Code as implementing clients, with
support varying by component and MCP transport. We have not verified any of them
against this plugin — see Known limitations.

We already ship four skills and one hosted MCP server. Before this change those
skills reached Claude Code, Codex, and Cursor through three hand-written
manifests; Copilot, Kiro, and VS Code got the MCP server alone (via
`plugins/e2a/clients/vscode.mcp.json`) and none of the skills.

Anthropic is not on the TSC and Claude Code does not implement the spec, so this
is **additive**: nothing about the Claude Code install path changes.

## What the spec fixes, and what it leaves alone

Agent Plugins standardizes only the package layout: a root `plugin.json`
(closed schema, ten permitted top-level keys), `skills/<name>/SKILL.md`, and a
root `mcp.json`. It explicitly does **not** standardize installation, registries
or marketplaces, credentials or OAuth, sandboxing, or hooks/agents/commands/LSP.

Two consequences we live with:

- **Marketplaces are unaffected.** All three root `marketplace.json` files stay
  exactly as they are. Conformance buys reach into manually-installed contexts
  and into clients that implement the spec — not new marketplace distribution.
- **Auth does not travel.** The portable `mcp.json` can carry the endpoint URL
  and nothing else; `headers` are visible data, not a secret channel. Our OAuth
  flow remains a client-side concern.

## Artifact mapping

| Artifact | Classification | Location |
|---|---|---|
| Portable manifest (core) | portable core | `plugins/e2a/plugin.json` *(new)* |
| Portable manifest (Labs) | portable core | `plugins/e2a-labs/plugin.json` *(new)* |
| Portable MCP config | portable core | `plugins/e2a/mcp.json` *(new; core only — Labs owns no MCP)* |
| Four stable skills | portable core | `plugins/e2a/skills/<name>/SKILL.md` (unchanged paths) |
| Three experimental skills | portable core | `plugins/e2a-labs/skills/<name>/SKILL.md` |
| `displayName`, `icon` | client-specific, omitted from portable manifest | the client manifests only |
| Claude Code manifests | compatibility layer | `plugins/e2a{,-labs}/.claude-plugin/plugin.json` (retained) |
| Codex manifests | compatibility layer | `plugins/e2a{,-labs}/.codex-plugin/plugin.json` (retained) |
| Cursor manifest | compatibility layer | `plugins/e2a/.cursor-plugin/plugin.json` (retained; core only) |
| Legacy MCP config | compatibility layer | `plugins/e2a/.mcp.json` (retained) |
| Three marketplaces | distribution metadata | generated, but outside the portable format |
| Editor MCP snippets | distribution metadata | `plugins/e2a/clients/` unchanged |

Nothing was removed.

### Why the legacy directories keep their names

The spec allows client-specific data under a reverse-domain namespace
(`com.vendor.client/`), but its guidance is explicit: *"Do not invent a vendor
namespace and assume an unrelated client will load it."* No client has published
one, so `.claude-plugin/`, `.codex-plugin/`, and `.cursor-plugin/` stay as a
compatibility layer rather than being renamed into namespaces that nothing reads.

## Three changes

### 1. The portable pair

`plugin.json` and `mcp.json` at the plugin root. Two details that are easy to get
wrong: the v1 manifest schema is **closed** (`additionalProperties: false`), so
`displayName`, `icon`, and `mcpServers` cannot ride along at the top level; and
the transport token is `streamable-http`, where the legacy clients spell the same
transport `http`.

`displayName` and `icon` are simply **omitted** from the portable manifest rather
than moved under `extensions`. An extension namespace is a *client-owned*
identifier (spec §3) whose contents only its owning client defines (§8.1). We are
the plugin author, not a client, so a `dev.e2a.*` namespace would be data no
client ever reads — the same invented-namespace mistake the section below avoids
for the legacy directories. Both keys already live in the three client manifests,
which is the surface that actually renders them. `validate-plugin.mjs` rejects an
extensions namespace of ours if one is ever reintroduced.

Claude Code reads neither file — its manifest is only `.claude-plugin/plugin.json`
and its MCP config only `.mcp.json` or inline. There is no double-registration.

### 2. One metadata source per package

The plugin manifests and three marketplaces repeated the same name, version,
description, and license. `scripts/validate-plugin.mjs` compared them after the
fact, which catches skew but not the hand-edit that caused it — and adding the
portable pair would have made that worse.

Every manifest is now generated by `scripts/generate-plugin-manifests.mjs` from
the per-package metadata sources: `plugins/e2a/plugin.meta.json` and
`plugins/e2a-labs/plugin.meta.json`. The generator emits each package's
portable `plugin.json` (plus `mcp.json`/`.mcp.json` for the MCP-owning core),
its client manifests, and the three shared marketplace files.
`validate-plugin.mjs` runs the generator in `--check` mode, so a hand-edited
manifest fails CI with the file to fix. This mirrors the
`make generate-sdk-check` freshness gate.

**To change plugin metadata:** edit the package's `plugin.meta.json`, run
`node scripts/generate-plugin-manifests.mjs`, commit both.

### 3. Skill path portability

Three skills located their own scripts through
`${CLAUDE_PLUGIN_ROOT}` — 21 references across `agentify`, `autopilot`, and
`tether` (all now in `plugins/e2a-labs/skills/` after the core/Labs split).
That variable only expands on Claude Code. Everywhere else the agent
receives a literal unexpanded path, so the skill would install cleanly, validate
cleanly, and fail at its first command.

Each skill now names its scripts relative to its own directory (`$AUTOPILOT`,
`$TETHER_DIR`, `$AGENTIFY_DIR`, resolved from where the agent read `SKILL.md`).
This is safe because every bundled script already self-locates:

```sh
AUTOPILOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
```

so invocation from any working directory resolves correctly. `validate-plugin.mjs`
now rejects any file in a skill reintroducing a client-specific path variable.

## Known limitations

- **Copilot/Kiro/VS Code are unverified.** Conformance is validated statically;
  no live install on a spec-implementing client has been run. That is the
  remaining gate before claiming those clients as supported.
- **Two discovery paths coexist once Codex or Cursor implement the spec.** Both
  are on the TSC, so this is a when. The plugin will then carry a native path
  (`.codex-plugin/plugin.json` → `.mcp.json`) *and* the portable pair, i.e. two
  routes to the same MCP server in one directory. The spec says clients map the
  portable format to their native configuration (§7) but is silent on
  coexistence, and client-native mapping is an explicit non-goal — so whether a
  spec-implementing Codex dedupes, prefers one, or registers the server twice is
  unknown. The additive argument below is verified for Claude Code only. Check
  this at the same time as the live-install gate.
- **Description variants ship today.** Core's manifest copy omits HITL; both
  marketplace variants mention it, and Labs carries its own manifest/Codex/
  marketplace variants. `plugin.meta.json` models all of them rather than
  silently unifying — deciding whether that divergence is intentional is a copy
  decision, not a migration one.
- **`README.md` references `../../scripts/sync-agent-docs.mjs`**, one path in the
  package that points outside the plugin root. It is prose in a maintainer note,
  not a path any client resolves, so it is not a containment violation — but it
  is the only such reference and worth not multiplying.
- **No credential story.** Per the spec, not per our packaging.

## Verification

- `node scripts/validate-plugin.mjs` — schema, freshness, portability, regression guard
- `node scripts/generate-plugin-manifests.mjs --check` — all twelve files current
- `node --test scripts/validate-plugin.test.mjs` — 12/12, guard regressions
- `node --test scripts/plugin-agent-guidance.test.mjs` — 9/9
- `plugins/e2a-labs/skills/agentify/test/run.sh` — pass
- `node --test 'test/*.test.mjs'` in `plugins/e2a-labs/skills/autopilot` — 117/117
- The generated portable manifests validated with `ajv` against the canonical v1.0.0 schemas

The upstream checklist has 30 items and is written to be read and judged, not
executed. Of the 30: the Package, Manifest, Skills, and MCP sections hold; the
Client-compatibility section's live-install item is the open gate above; and the
Handoff section is this document. An earlier draft of this note reported a pass
count from a local mechanization of the checklist — that number was the script's
tally, not the checklist's verdict, and the script was blind to the extensions
item it should have failed. Treat the sections above as the claim, not a score.
