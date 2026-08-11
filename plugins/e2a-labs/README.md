# e2a Labs plugin

Experimental autonomous workflows for agents using e2a. Labs supplies the
`/agentify`, `/autopilot`, and `/tether` skills; it does **not** register an
MCP server. Install the core **e2a** plugin first — core supplies the hosted
MCP connection and authentication.

## Install

### Claude Code

Install core first, then Labs:

```
claude plugin marketplace add tokencanopy/e2a
claude plugin install e2a@e2a
claude plugin install e2a-labs@e2a
```

### Codex

Add the marketplace, then use `/plugins` to install **e2a** before
**e2a-labs**:

```
codex plugin marketplace add tokencanopy/e2a
```

Labs is experimental: its workflows may change without the stable compatibility
guarantees of core e2a.

## Migrating from core 0.6.x

Agentify, Autopilot, and Tether moved from core to Labs in core 0.7.0. Existing
Claude Code users can refresh core and install the moved workflows with:

```
claude plugin marketplace update e2a
claude plugin update e2a@e2a
claude plugin install e2a-labs@e2a
```

Existing Codex users can refresh the repository marketplace and install Labs
without adding a second marketplace or changing MCP configuration:

```
codex plugin marketplace upgrade e2a
codex plugin add e2a@e2a
codex plugin add e2a-labs@e2a
```

Core remains installed in both cases because it is the only package that
supplies the e2a MCP connection and authentication.

## Experimental workflows

- **Agentify — experimental:** deploys an autonomous repository feedback loop.
- **Autopilot — experimental:** runs a policy-first local email agent.
- **Tether — experimental:** hands off long-running sessions over email.

## What's inside

```
plugins/e2a-labs/
├── plugin.meta.json            # SOURCE OF TRUTH — the manifests below are generated from it
├── plugin.json                 # Agent Plugins v1 portable manifest (generated)
├── .claude-plugin/plugin.json  # Claude Code manifest; depends on core e2a (generated)
├── .codex-plugin/plugin.json   # Codex manifest; skills only (generated)
├── assets/icon.svg
└── skills/
    ├── agentify/               # autonomous-repo feedback-loop deployment
    ├── autopilot/               # policy-first local email agent
    └── tether/                  # email handoff for long-running sessions
```

There is intentionally no Cursor manifest and no `.mcp.json`/`mcp.json`: core
e2a is the sole owner of the e2a MCP connection. Edit `plugin.meta.json` and
run `node scripts/generate-plugin-manifests.mjs` to change any manifest —
hand edits fail CI.
