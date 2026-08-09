# Addons

Optional, self-contained extensions the deploy flow can install on top of the
core loop. Each addon is a directory here:

```
addons/<name>/
  manifest.yml     # what it provides, the env it needs, config + setup notes
  files/           # scaffolded into the target repo at tools/<name>/
  setup.md         # appended to the target's addon-setup doc
```

`/agentify` offers the available addons; for each opted in (passed to
`agentify-render.sh` via `ANS_ADDONS="<name> ..."`) the render:
1. copies `files/` → `<target>/tools/<name>/`,
2. appends `setup.md` → `<target>/AGENTIFY-ADDON-SETUP.md`,
3. surfaces the manifest's `env` / `config_note` for the adopter.

Addons are **additive** — the core loop runs without any of them. The first
addon is `submit-feedback-mcp` (an intake adapter: a `submit_feedback` MCP
tool that email-bridges into the existing support mailbox).

`provides:` in a manifest is one of `intake` / `comms` / `store` — the seam the
addon plugs into. Future addons (a GitHub-issue intake, an SMTP comms adapter,
the durable backend store) follow the same shape.
