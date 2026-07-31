# Google ADK × e2a

Google ADK [`Agent`](https://adk.dev/) powered by Gemini that uses the hosted e2a [MCP server](https://api.e2a.dev/mcp) for the e2a tool surface.

`agent.py` wires `McpToolset` + `StreamableHTTPConnectionParams` to the hosted endpoint at `https://api.e2a.dev/mcp`. This works locally and for ADK agents deployed to [Cloud Run](https://docs.cloud.google.com/run/docs/host-mcp-servers). Unlike the LangChain/CrewAI/OpenAI Agents SDK examples, this URL is hardcoded — there's no `E2A_MCP_URL` override; to point at a self-hosted deployment, edit the `url` passed to `StreamableHTTPConnectionParams` directly.

## Prerequisites

- Python 3.10+
- An [e2a API key](https://e2a.dev)
- A [Google AI Studio key](https://aistudio.google.com/apikey) for Gemini

> **ADK line:** this example targets ADK 2.x (`google-adk>=2.0`). The webhook example under [`examples/adk-cloud-webhook`](../../../examples/adk-cloud-webhook/) pins the 1.x line — they intentionally track different ADK releases.

## Run

```bash
pip install -r requirements.txt
export E2A_API_KEY=e2a_…
export GOOGLE_API_KEY=…

adk web              # opens the ADK Web UI on http://localhost:8000
# or:
adk run agent.py     # interactive CLI
```

If your credential resolves a single agent, the hosted endpoint scopes tools to it server-side. With an account-scoped key and multiple agents, pass `email` per tool call.

Then in the chat:

> What's in my inbox?
> Reply to the latest message politely.
> Send an email to alice@example.com — subject "test", body "hello from ADK".

## How it works

`McpToolset` connects to the hosted Streamable HTTP endpoint. ADK auto-discovers the tools and surfaces them to Gemini. Module-scope `root_agent` is the convention ADK's `run` / `web` look for.
