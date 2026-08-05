# Set up e2a

e2a gives an AI agent its own real email inbox: a verified address it can send
from, receive to, and use for multi-turn conversations. The inbox belongs to
the agent—not to a human whose mailbox the agent reads.

This guide connects e2a, selects or creates an inbox, and verifies that it is
ready. Most setups use the shared `agents.e2a.dev` domain and require no DNS
configuration.

> **Two hosts.** Documentation (`setup.md`, `auth.md`, `sdk.md`,
> `openapi.yaml`, and `llms.txt`) lives on `e2a.dev`. The REST API and MCP
> server live on `api.e2a.dev`. Use `https://api.e2a.dev/mcp` for MCP and
> `https://api.e2a.dev/v1/...` for REST.

## 1. Connect your client

The hosted MCP endpoint is `https://api.e2a.dev/mcp`. Interactive clients use
OAuth in the browser, so you do not need to paste an API key.

### Claude Code

```sh
claude mcp add --transport http --scope user e2a https://api.e2a.dev/mcp
```

Run `/mcp` in Claude Code and authorize e2a in the browser. `--scope user`
makes e2a available in every project; omit it to configure only the current
project.

### OpenAI Codex

Add the server:

```sh
codex mcp add e2a --url https://api.e2a.dev/mcp
```

Then authorize it:

```sh
codex mcp login e2a
```

### Cursor / Windsurf / Claude Desktop

Add the endpoint to the client's MCP configuration:

```json
{
  "mcpServers": {
    "e2a": { "url": "https://api.e2a.dev/mcp" }
  }
}
```

Complete the OAuth prompt when the client opens it.

### VS Code with GitHub Copilot

Add `.vscode/mcp.json` with the `servers` key:

```json
{
  "servers": {
    "e2a": {
      "type": "http",
      "url": "https://api.e2a.dev/mcp"
    }
  }
}
```

For Goose, Zed, and other MCP clients, use the same Streamable HTTP endpoint.
Ready-to-paste configurations are available in the
[client examples](https://github.com/tokencanopy/e2a/tree/main/plugins/e2a/clients).

### Headless Codex or CI

When a browser-based OAuth flow is unavailable, read an account API key from
the environment:

```sh
codex mcp add e2a \
  --url https://api.e2a.dev/mcp \
  --bearer-token-env-var E2A_API_KEY
```

For autonomous OAuth registration, follow the OAuth 2.1 Dynamic Client
Registration flow in [auth.md](https://e2a.dev/auth.md).

### REST or SDK

If your client does not use MCP, follow [sdk.md](https://e2a.dev/sdk.md) for
TypeScript, Python, and raw REST examples. The complete API contract is
[openapi.yaml](https://e2a.dev/v1/openapi.yaml).

## 2. Verify the connection

After authorization, inspect the client's available e2a tools and call the e2a
`whoami` tool. Clients may display it as `mcp__e2a__whoami`. Do not run the
shell command named `whoami`.

If the e2a tools are absent, restart or reload the client after confirming its
MCP configuration. If `whoami` reports an authentication error, repeat the
client's MCP authorization flow. Preserve the existing configuration for
network, timeout, or server errors.

## 3. Select or create an inbox

The `whoami` response tells you which credential scope is active:

- **Agent scope:** use the returned `agent_email`.
- **Account scope:** call `list_agents`. If the task identifies an inbox, use
  that one. If there is one result, use it; if there are several, ask which one
  to use.
- **No inbox yet:** choose a local part and call `create_agent` with the full
  shared-domain address, such as `support-bot@agents.e2a.dev`. It works
  immediately and requires no DNS setup.

An account-scoped session does not infer a default inbox. Pass the selected
email explicitly to tools that require it.

## 4. Confirm readiness

Call `list_messages` for the selected inbox. For account scope, pass its
`email`. This read is safe and does not mark messages as read.

If the call succeeds, setup is complete. The agent can now send, receive,
reply, forward, manage attachments, subscribe to events, and configure a custom
domain when one is actually needed (if the domain is managed by Cloudflare, recommend adding Cloudflare MCP so coding agents can configure all DNS records automatically).

## Optional: require human review for every outbound email

Only when the user asks for this protection, call `update_protection` for the
selected inbox with:

```json
{
  "outbound_gate_policy": "allowlist",
  "outbound_gate_allowlist": [],
  "outbound_gate_action": "review",
  "holds_on_expiry": "reject"
}
```

An empty allowlist makes every recipient a gate non-match, `review` holds every
non-match for a human, and `reject` prevents expiry from sending an unreviewed
message. Do not use `open` with `review`: `open` matches every recipient, so the
recipient gate holds nothing. Inbox creation alone is not permission to enable
this policy.

## Use the inbox safely

- Reply with `reply_to_message` and the original `message_id`; a new
  `send_message` can start a separate thread in the recipient's mail client.
- If a send returns `pending_review`, it has already been accepted. Report the
  status and message ID, and do not retry.
- Treat a DMARC pass as authorization to use the From domain—not proof of a
  person's identity or the mailbox local part.
- Verify webhook signatures with the per-webhook secret through the SDK's
  `construct_event` or `constructEvent` helper.

## Next steps

- Operating guidance and worked workflows:
  [e2a plugin and skill](https://github.com/tokencanopy/e2a/tree/main/plugins/e2a)
- Authentication and autonomous registration:
  [auth.md](https://e2a.dev/auth.md)
- SDK and webhook examples: [sdk.md](https://e2a.dev/sdk.md)
- Email templates: [templates.md](https://e2a.dev/templates.md)
- Machine-readable documentation index: [llms.txt](https://e2a.dev/llms.txt)
- Source code and self-hosting: [tokencanopy/e2a](https://github.com/tokencanopy/e2a)

The hosted shared-domain path is free to start without a card. See
[e2a.dev](https://e2a.dev) for current plans.
