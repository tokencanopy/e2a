# CrewAI × e2a

Single-agent CrewAI 1.x crew that drives the hosted e2a [MCP server](https://api.e2a.dev/mcp) through CrewAI's native `MCPServerHTTP` integration. It discovers the e2a tool surface and uses it to answer natural-language email tasks.

`agent.py` connects to the hosted endpoint at `https://api.e2a.dev/mcp` (Streamable HTTP) with your API key in the `Authorization` header.

## Prerequisites

- Python 3.10+
- An **agent-scoped** [e2a API key](https://e2a.dev), recommended so the runtime can act only as its own inbox
- An [Anthropic API key](https://console.anthropic.com/)

## Run

```bash
pip install -r requirements.txt
export E2A_API_KEY=e2a_…
export ANTHROPIC_API_KEY=sk-ant-…
# Optional: export E2A_MCP_URL=https://your-e2a.example/mcp
# Optional: export CREWAI_MODEL=anthropic/claude-sonnet-4-6

python agent.py "what's in my inbox?"
python agent.py "reply to the most recent message politely"
```

The example deliberately uses an agent-scoped key. An account-scoped key exposes
admin/setup tools as well and requires `email` on inbox calls when the
account owns multiple agents; use that broader scope only for a provisioning UI.

## How it works

`MCPServerHTTP(streamable=True)` is attached directly through `Agent(mcps=[...])`,
the current CrewAI-recommended integration. CrewAI discovers and cleans up the
remote tools around the crew run. Tool schemas are cached for that run. The
backstory tells the agent to retry only structured errors marked `retryable`,
and to treat `pending_review` as accepted rather than retrying it.

CrewAI namespaces discovered tool names with the MCP server identifier (for
example, a name ending in `_list_messages`). The model still receives each
tool's original description and schema; this prevents collisions when an agent
uses more than one MCP server.

To swap models, change `"anthropic/claude-sonnet-4-6"` to any [LiteLLM-compatible model string](https://docs.litellm.ai/docs/providers) CrewAI accepts (CrewAI uses LiteLLM under the hood).
