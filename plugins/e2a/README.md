# e2a plugin

Gives an AI coding agent a real, authenticated email inbox. Installing this
plugin registers the hosted **e2a MCP server** (`https://api.e2a.dev/mcp`,
Streamable HTTP + OAuth 2.1) and an **operate-well skill** so the agent can send
and receive email, reply in-thread, manage agents and custom domains, and work
with attachments.

After installation, authorize e2a through your client's MCP flow (Claude Code:
run `/mcp`; Codex CLI: `codex mcp login e2a`) — no API key to paste. For
headless/CI, an account API key works too; see [`clients/`](./clients).

## Install

The same plugin ships native manifests for Claude Code, Codex, and Cursor.

### Claude Code

```
claude plugin marketplace add tokencanopy/e2a
claude plugin install e2a@e2a
```

### Codex

```
codex plugin marketplace add tokencanopy/e2a
```

Then launch `codex`, run `/plugins`, search for **e2a**, and install — it walks
you through the OAuth path. (Codex desktop: **Plugins → Add more + →** paste
`https://github.com/tokencanopy/e2a`.)

### Cursor

Point Cursor at the hosted MCP server. Project-level config lives in
`.cursor/mcp.json`, global in `~/.cursor/mcp.json` (project wins on conflict):

```json
{
  "mcpServers": {
    "e2a": { "url": "https://api.e2a.dev/mcp" }
  }
}
```

Remote servers take `url` only — `type`/`transport` are stdio-only in Cursor.
On first use Cursor registers itself via OAuth Dynamic Client Registration and
opens your browser; there is no API key to paste and no `auth` block to fill in.

Cursor gets the MCP server this way rather than the plugin, so it does not pick
up the skill — the tools work, the operating guidance doesn't come with them.

This file used to recommend two things that don't work, so they're worth naming
before someone re-adds them: a bare `/add-plugin e2a` resolves only against
Cursor's curated marketplace, which e2a isn't published to; and there is no
"paste a repo URL into marketplace search" flow — importing a repo is
**Dashboard → Plugins → Add Marketplace → Import from Repo**, which creates a
*team* marketplace and is Teams/Enterprise only.

### Other MCP clients (manual)

Clients without native plugin support (Zed, Goose, Windsurf, Claude Desktop, raw
`mcp.json`) can point straight at the hosted server. Ready-to-paste configs are
in [`clients/`](./clients); the full per-client guide is at
<https://e2a.dev/setup.md>.

## What's inside

```
plugins/e2a/
├── .claude-plugin/plugin.json   # Claude Code manifest
├── .codex-plugin/plugin.json    # Codex manifest (skills + mcpServers + interface)
├── .cursor-plugin/plugin.json   # Cursor manifest
├── .mcp.json                    # the hosted MCP server (single source of truth)
├── assets/icon.svg
├── docs/                        # canonical agent docs mirrored at e2a.dev
│   ├── setup.md                 # connect guide + first-inbox workflow
│   ├── auth.md                  # OAuth, API keys, scopes, agent identity
│   ├── sdk.md                   # SDK + webhook integration guide
│   ├── templates.md             # email-template guide
│   └── llms.txt                 # machine-readable hosted docs index
├── skills/e2a/SKILL.md          # the "operate-well" skill (surfaces as /e2a)
├── skills/agentify/SKILL.md     # deploy the autonomous-repo feedback loop (/agentify)
├── skills/tether/SKILL.md       # email handoff for long-running sessions (/tether)
├── skills/autopilot/SKILL.md    # policy-first, always-on local email agent (/autopilot)
└── clients/                     # manual paste-in configs for non-plugin clients
```

The marketplace manifests that expose this plugin live at the repo root:
`.claude-plugin/marketplace.json`, `.cursor-plugin/marketplace.json`, and
`.agents/plugins/marketplace.json` (Codex).

## Developing

The skill is authored in `skills/<name>/SKILL.md` with YAML frontmatter:

```markdown
---
name: e2a
description: Use when operating e2a over its MCP tools — sending/receiving email, ...
version: 12
---

...guide body...
```

- `name` (required) — must match the directory; lowercase letters, digits,
  hyphens; ≤64 chars.
- `description` (required) — write it as "Use when…"; this is how Claude Code
  decides to load the skill. ≤1024 chars.

`node scripts/validate-plugin.mjs` (run by the **Plugin manifests** CI job)
validates every manifest parses, that the version is identical across all
Claude/Codex/Cursor manifests, that marketplace `source` paths resolve, and that
each `SKILL.md` satisfies Claude Code's frontmatter constraints. A change that
wouldn't load fails CI.

Agent-facing Markdown is authored in `docs/`. Run
`node ../../scripts/sync-agent-docs.mjs` from this directory (or
`node scripts/sync-agent-docs.mjs` from the repository root) to refresh its
committed `web/public/` mirrors. The repository-integrity CI job runs the same
script with `--check` and fails if a hosted mirror drifts from its canonical
source.

When bumping the plugin version, update `.claude-plugin/plugin.json` (the source
of truth) **and** the other manifests + marketplace metadata to match — the
validator fails on drift.

## Reference

- Connect / clients / first inbox: <https://e2a.dev/setup.md>
- Auth (OAuth 2.1 DCR + PKCE, API keys, scopes): <https://e2a.dev/auth.md>
- Webhook + SDK code: <https://e2a.dev/sdk.md>
- Docs index: <https://e2a.dev> (machine-readable: <https://e2a.dev/llms.txt>)
