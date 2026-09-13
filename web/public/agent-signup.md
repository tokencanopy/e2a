# Let an agent sign itself up for e2a

This is the no-credential path. A coding agent can create one provisional e2a
inbox and receive its own agent-scoped API key before a human has an e2a
account. The human named during signup receives a six-digit code and can also
approve or reject the request from the e2a dashboard.

The hosted API base is `https://api.e2a.dev`. Self-hosters replace that URL
with their deployment's API URL; signup is available only when the deployment
has a shared agent domain and outbound verification mail configured.

## Safety model while the request is pending

The provisional inbox can receive email from anyone immediately. Its key can
operate only that inbox. Until the human verifies or approves it, the agent:

- can send only to the named human;
- can make at most five accepted sends in a rolling 24-hour window;
- cannot create other inboxes, keys, domains, or account resources.

A send to anyone else fails with `403 pending_human_verification`. A sixth send
within 24 hours fails with `429 rate_limited`.

## MCP: complete signup without curl

Connect an MCP client to the public bootstrap endpoint:

```text
https://api.e2a.dev/mcp/signup
```

This endpoint intentionally exposes only two tools and does not need OAuth or
an API key:

1. Call `signup_agent` with:

   ```json
   {
     "human_email": "owner@example.test",
     "display_name": "Build Bot",
     "note_to_human": "I need an inbox for build notifications.",
     "harness": "codex"
   }
   ```

2. Save `api_key` and `inbox` from the result. The API key is shown once.
3. Ask the human for the six-digit code emailed to them.
4. Call `verify_agent_signup`:

   ```json
   {
     "api_key": "e2a_agt_example_only",
     "code": "123456",
     "review_outbound": true
   }
   ```

`review_outbound: true` means later messages to anyone other than the human
enter e2a's existing review queue instead of being sent immediately. After
verification, reconnect to `https://api.e2a.dev/mcp` with the returned key as
Bearer authentication to use the normal inbox tools.

For Codex, the bootstrap connection is:

```sh
codex mcp add e2a-signup --url https://api.e2a.dev/mcp/signup
```

For Claude Code:

```sh
claude mcp add --transport http e2a-signup https://api.e2a.dev/mcp/signup
```

## CLI

No saved login or API key is required for `signup create`:

```sh
e2a signup create \
  --human-email owner@example.test \
  --display-name "Build Bot" \
  --note "I need an inbox for build notifications." \
  --harness codex \
  --json
```

Save the returned `apiKey`, then verify after the human shares the code:

```sh
e2a signup verify \
  --api-key e2a_agt_example_only \
  --code 123456 \
  --review-outbound \
  --json
```

## TypeScript SDK

```ts
import { E2AClient, signupAgent } from "@e2a/sdk/v1";

const created = await signupAgent({
  humanEmail: "owner@example.test",
  displayName: "Build Bot",
  noteToHuman: "I need an inbox for build notifications.",
  harness: "custom-runner",
});

// Persist these before continuing. created.apiKey is shown once.
console.log(created.inbox, created.apiKey);

const agent = new E2AClient({ apiKey: created.apiKey });
await agent.agentSignup.verify({ code: "123456", reviewOutbound: true });
```

## Python SDK

```python
from e2a.v1 import E2AClient, signup_agent

created = signup_agent({
    "human_email": "owner@example.test",
    "display_name": "Build Bot",
    "note_to_human": "I need an inbox for build notifications.",
    "harness": "custom-runner",
})

# Persist these before continuing. created.api_key is shown once.
print(created.inbox, created.api_key)

with E2AClient(api_key=created.api_key) as agent:
    agent.agent_signup.verify({"code": "123456", "review_outbound": True})
```

Async Python uses `async_signup_agent(...)` and `AsyncE2AClient`.

## Raw REST

Create a provisional identity. This one endpoint is public:

```sh
curl -sS https://api.e2a.dev/v1/agent-signup \
  -H 'Content-Type: application/json' \
  -d '{
    "human_email":"owner@example.test",
    "display_name":"Build Bot",
    "note_to_human":"I need an inbox for build notifications.",
    "harness":"custom-runner"
  }'
```

Then authenticate the verification request with the returned agent key:

```sh
curl -sS https://api.e2a.dev/v1/agent-signup/verify \
  -H 'Authorization: Bearer e2a_agt_example_only' \
  -H 'Content-Type: application/json' \
  -d '{"code":"123456","review_outbound":true}'
```

Repeating the same normalized `human_email` plus `display_name` while pending
keeps the inbox but rotates the API key and code. The previous key stops
working. Do not repeat signup merely because the verification email is delayed
unless rotating the key is acceptable.

## Human approval or rejection

The human can sign in at `https://e2a.dev/agent-signups`. Approving the request
unlocks the same agent; rejecting it revokes every signup key and deactivates
the provisional inbox. The human may enable the same outbound review option
during approval.

Exact request and response schemas are in the
[OpenAPI contract](https://e2a.dev/v1/openapi.yaml).
