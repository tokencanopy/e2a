# OpenAI Codex CLI × e2a

[Codex CLI](https://github.com/openai/codex) is OpenAI's terminal coding agent (peer to Claude Code). It speaks MCP natively — you declare servers in `~/.codex/config.toml` and Codex connects to them at session start, then surfaces their tools to whatever model you're running.

Unlike the [LangChain](../langchain/), [CrewAI](../crewai/), [Google ADK](../adk/), and [OpenAI Agents SDK](../openai-agents/) examples — which are Python scripts you run — Codex is itself the agent. The "example" here is the TOML you paste into your Codex config to add e2a's MCP tools (71 total; the visible set depends on your key's scope).

Codex opens a Streamable HTTP session to the hosted endpoint at `https://api.e2a.dev/mcp` with your API key in the `Authorization: Bearer …` header — no Node toolchain on the host (works on CI runners, dev containers, remote app servers, etc.).

## Prerequisites

- [Codex CLI](https://github.com/openai/codex) 0.133+ (`npm i -g @openai/codex` or `brew install --cask codex`)
- An [e2a API key](https://e2a.dev), exported as `E2A_API_KEY`

## Install (the TOML way)

Open `~/.codex/config.toml` (Codex creates it on first run; `mkdir -p ~/.codex && touch ~/.codex/config.toml` if it doesn't exist yet) and paste the block from [`config.toml.example`](./config.toml.example):

```toml
[mcp_servers.e2a-hosted]
url                  = "https://api.e2a.dev/mcp"
bearer_token_env_var = "E2A_API_KEY"
```

Export the key in your shell so Codex can read it at session start:

```bash
export E2A_API_KEY=e2a_...
```

## Install (the CLI way)

Codex ships a first-class command that writes the same TOML for you:

```bash
codex mcp add e2a-hosted \
  --url https://api.e2a.dev/mcp \
  --bearer-token-env-var E2A_API_KEY
```

Verify with `codex mcp list` (add `--json` for machine-readable output) or `codex doctor`, which reports configured servers and surfaces missing env vars.

## Run

```bash
codex
```

Then in the session:

> What's in my inbox?
> Reply to the latest message politely.
> Send an email to alice@example.com — subject "test", body "hello from Codex".

If your credential resolves a single agent, the hosted endpoint scopes tools to it server-side. With an account-scoped key and multiple agents, ask Codex to pass `email` per tool call.

## How it works

Codex reads MCP server definitions from `~/.codex/config.toml` under `[mcp_servers.<id>]`. Setting a `url` (with optional `bearer_token_env_var` / `http_headers` / `env_http_headers` / `oauth*`) selects the Streamable HTTP transport.

An inline `bearer_token = "..."` is rejected at load time — secrets must come from an env var via `bearer_token_env_var`. Codex also supports OAuth (`oauth.client_id`, `oauth_resource`, `scopes`) for the HTTP transport, which e2a's hosted server accepts; a bearer env var is the simplest choice for programmatic use.

## Pointing at a self-hosted e2a

Change the `url` literal in the `[mcp_servers.e2a-hosted]` block to your deployment's MCP endpoint.
