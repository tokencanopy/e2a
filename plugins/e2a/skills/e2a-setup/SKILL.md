---
name: e2a-setup
description: "Use when a user wants to connect or authorize the e2a MCP server, select or create an agent inbox, verify first-run readiness, or set up a custom email domain. Guides native client OAuth, shared-domain onboarding, and confirmed DNS-provider assistance without changing application code."
---

# Set up e2a

## Boundaries

- This skill starts after the e2a plugin is installed; it cannot bootstrap the plugin that contains it.
- Do not change client configuration, create an inbox, or change DNS without the confirmation stated below. Use OAuth and never ask an interactive user for an API key.
- Do not change application code. Never send a test unless the user selects its recipient.

## MCP bootstrap

1. Inspect the current client's tool registry for e2a tools.
2. If absent, first check whether the plugin is disabled or needs a reload before offering manual registration; do not create a duplicate registration.
3. Only if registration is genuinely absent, explain the native flow from [clients.md](references/clients.md), confirm before a client-config write, then offer to register `https://api.e2a.dev/mcp`.
4. Complete the client's OAuth flow, reload or restart if required, then call the e2a MCP `whoami` tool. Treat auth failures as reauthorization; preserve configuration for operational failures.

## Select or create the inbox

1. For agent scope, use the `agent_email` from `whoami`.
2. For account scope, call `list_agents`: honor an inbox named by the task, use the sole result, or ask the user to choose among several.
3. If none exists, offer a shared-domain address on `agents.e2a.dev`. Confirm the full address before calling `create_agent`; do not infer it from a local part.

## Choose shared or custom domain

Ask whether the user wants the recommended shared domain or a custom domain. The shared path needs no DNS. For a custom domain, confirm ownership and branded-address intent, then follow [custom-domains.md](references/custom-domains.md).

Use a Cloudflare API MCP server when available for Cloudflare-hosted DNS. For GoDaddy, use the authenticated `gddy` path; the official GoDaddy MCP is read-only and cannot modify DNS.

Before any provider-assisted DNS write, show the complete proposed DNS diff and obtain one confirmation for the complete DNS diff.

## Verify readiness

Call `list_messages` for the selected inbox (pass its email for account scope). This is a harmless read and must succeed before readiness is claimed. Offer a send test only after the user selects a recipient; otherwise do not send one.

## Completion report

Report the MCP registration and OAuth state, credential scope, selected inbox, successful inbox read, and domain state. For a custom domain, report inbound verification and outbound branded-sending capability separately, including any pending DNS or propagation action.
