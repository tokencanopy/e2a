# Examples

End-to-end demos showing how to wire the e2a MCP surface into popular agent frameworks and CLIs.

| Framework | Path | LLM | Example |
| --- | --- | --- | --- |
| LangChain | [langchain/](./langchain/) | Anthropic Claude | `agent.py` |
| CrewAI | [crewai/](./crewai/) | Anthropic Claude | `agent.py` |
| Google ADK | [adk/](./adk/) | Google Gemini | `agent.py` |
| OpenAI Agents SDK | [openai-agents/](./openai-agents/) | OpenAI GPT | `agent.py` |
| OpenAI Codex CLI | [codex/](./codex/) | Codex (OpenAI) | `[mcp_servers.e2a-hosted]` in `config.toml` |

## One hosted endpoint, same tool surface

Each example exercises the same e2a tool surface (78 tools; the visible set depends on your key's scope) against the hosted MCP server. The Python framework examples ship an `agent.py` script; the Codex example ships a TOML block (Codex is itself the agent, so you configure it instead of writing a script).

Every example connects to `https://api.e2a.dev/mcp` over Streamable HTTP with an
agent-scoped API key in the `Authorization` header. Set `E2A_MCP_URL` to point
the LangChain, CrewAI, and OpenAI Agents SDK examples at a self-hosted
deployment (the Google ADK example currently hardcodes the hosted URL — see
[adk/](./adk/)). No Node toolchain or local build is needed.

Bring your own [e2a API key](https://e2a.dev) and an LLM key for whichever framework you're trying.

The LangChain, CrewAI, and OpenAI Agents SDK prompts share three email
invariants: reply with `reply_to_message` to preserve the wire thread, treat
`pending_review` as an accepted hold rather than an error, and retry only
tool failures whose structured error reports `retryable: true`.

## Things to try once a demo is running

- `what's in my inbox?` — exercises `list_messages` + `get_message`
- `reply to the most recent message politely` — exercises `reply_to_message` (preserves threading headers)
- `who am I?` — exercises `whoami`
- `what's waiting for my approval?` — exercises `list_reviews`. This is an
  account-owner review-queue tool, not agent self-approval, so it's only
  visible to an **account-scoped** key — it won't register as a tool at all
  under the agent-scoped key these examples otherwise use.

## Pointing at a self-hosted e2a

Set `E2A_MCP_URL=https://your-e2a.example/mcp` for the LangChain, CrewAI, and
OpenAI Agents SDK examples. Update the URL in the Codex
`[mcp_servers.e2a-hosted]` block for the configuration-only example. The
Google ADK example does not read `E2A_MCP_URL` — edit the hardcoded URL in
`adk/agent.py` to point it at a self-hosted deployment.
