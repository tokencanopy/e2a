# LangChain × e2a

LangChain 1.x agent that drives the hosted e2a [MCP server](https://api.e2a.dev/mcp) via [`langchain-mcp-adapters`](https://github.com/langchain-ai/langchain-mcp-adapters). Picks up the e2a tool surface and uses it to answer natural-language email tasks.

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
# Optional: export LANGCHAIN_MODEL=anthropic:claude-sonnet-4-6

python agent.py "what's in my inbox?"
python agent.py "reply to the most recent message politely"
```

The example deliberately uses an agent-scoped key. An account-scoped key exposes
admin/setup tools as well and requires `email` on inbox calls when the
account owns multiple agents; use that broader scope only for a provisioning UI.

## How it works

`MultiServerMCPClient` connects with the current `transport: "http"` spelling and
a Bearer header. `client.get_tools()` returns LangChain tools, and
`langchain.agents.create_agent` runs the tool loop. MCP `isError` results remain
model-visible failed tool messages; transport/authentication failures still
raise. The system prompt tells the agent to retry only structured errors marked
`retryable`, and to treat `pending_review` as accepted rather than retrying it.

To swap models, change `"anthropic:claude-sonnet-4-6"` to any provider:model string [`init_chat_model`](https://python.langchain.com/docs/how_to/chat_models_universal_init/) accepts.
