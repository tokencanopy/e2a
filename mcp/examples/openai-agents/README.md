# OpenAI Agents SDK × e2a

[OpenAI Agents SDK](https://openai.github.io/openai-agents-python/) `Agent` driving the hosted e2a [MCP server](https://api.e2a.dev/mcp).

`agent.py` wires `MCPServerStreamableHttp` to the hosted endpoint at `https://api.e2a.dev/mcp` with your API key in the `Authorization` header.

## Prerequisites

- Python 3.10+
- An **agent-scoped** [e2a API key](https://e2a.dev), recommended so the runtime can act only as its own inbox
- An [OpenAI API key](https://platform.openai.com/api-keys)

## Run

```bash
pip install -r requirements.txt
export E2A_API_KEY=e2a_…
export OPENAI_API_KEY=sk-…
# Optional: export E2A_MCP_URL=https://your-e2a.example/mcp

python agent.py "what's in my inbox?"
python agent.py "reply to the most recent message politely"
```

The example deliberately uses an agent-scoped key. An account-scoped key exposes
admin/setup tools as well and requires `email` on inbox calls when the
account owns multiple agents; use that broader scope only for a provisioning UI.

## How it works

`MCPServerStreamableHttp` connects to the hosted e2a tool surface, caches its
stable tool list for the run, and retries initial MCP connection failures up to
three times. The `Agent` receives those tools and `Runner.run` drives the loop.
The async context manager closes the HTTP client. Instructions tell the agent to
retry only structured errors marked `retryable`, and to treat `pending_review`
as accepted rather than retrying it.
