# Client connection flows

Setup cannot install or enable the plugin that contains it. Direct the user to
their marketplace first; after installation, inspect the client tool registry
before changing configuration. Confirm before every client-config write.

## Claude

The e2a plugin supplies the hosted MCP configuration. If its tools are absent,
check whether the plugin is disabled and reload Claude before manually adding a
server. When manual recovery is required, use Claude's native remote-MCP flow:

```sh
claude mcp add --transport http --scope user e2a https://api.e2a.dev/mcp
```

Open `/mcp` and complete browser OAuth. Reload if Claude asks for it, then use
the MCP `whoami` tool to prove the connection.

## Codex

The e2a plugin likewise owns its MCP configuration. First check the Plugins
view for a disabled plugin and reload Codex. Only after confirmation and only
when e2a is not registered, use Codex's native remote-MCP command:

```sh
codex mcp add e2a --url https://api.e2a.dev/mcp
codex mcp login e2a
```

The login is browser OAuth. Do not replace it with an API key. Restart or
reload when prompted, then call the MCP `whoami` tool.

## Other remote-MCP clients

Use the client's documented Streamable HTTP configuration for this endpoint:

```json
{
  "mcpServers": {
    "e2a": { "url": "https://api.e2a.dev/mcp" }
  }
}
```

Confirm before editing that configuration, complete the client's OAuth prompt,
reload if needed, and call MCP `whoami`. Clients that do not receive plugin
skills should follow the canonical [setup guide](https://e2a.dev/setup.md).
