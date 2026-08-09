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

## What's inside

```
plugins/e2a-labs/
├── .claude-plugin/plugin.json  # Claude Code manifest; depends on core e2a
├── .codex-plugin/plugin.json   # Codex manifest; skills only
├── assets/icon.svg
└── skills/
    ├── agentify/               # autonomous-repo feedback-loop deployment
    ├── autopilot/               # policy-first local email agent
    └── tether/                  # email handoff for long-running sessions
```

There is intentionally no Cursor manifest and no `.mcp.json`: core e2a is the
sole owner of the e2a MCP connection.
