# Addon setup: submit_feedback MCP bridge

The framework loop runs without this — it is an extra intake surface. To turn
it on:

1. **Give the bridge its own e2a identity** (separate from the lane keys):
   create an e2a agent (e.g. `feedback-intake@<domain>`) and mint an
   **agent-scoped** key bound to it. This is the address feedback is sent
   FROM; the support mailbox is where it's sent TO.
2. **Install + run the bridge** (`tools/submit-feedback-mcp/`):
   ```
   cd tools/submit-feedback-mcp && npm install
   E2A_API_URL=https://api.e2a.dev \
   E2A_API_KEY=<the bridge agent key> \
   FEEDBACK_INTAKE_ADDRESS=feedback-intake@<domain> \
   SUPPORT_ADDRESS=<comms.support_address> \
   node server.mjs
   ```
   Run it wherever your agents reach MCP servers (stdio transport), or host it.
3. **Register it** with the agent clients that should be able to file feedback
   (the same way you register any MCP server).
4. **Rate limit** (`FEEDBACK_RATE_PER_HOUR`, default 20) is a per-process
   backstop; the durable limit is your MCP host's / e2a's.

## Model + honest scope

- The bridge sends from ITS identity, so replies land in the bridge's mailbox
  and the filer reads progress via `feedback_status` (in-band). It never
  accepts a caller-supplied "email me here" address (spoof/spam vector).
- `feedback_status` is coarse (`received` → `answered`) — precise lifecycle
  lives in the GitHub ticket-card, not here.
- **Richer variant (follow-on):** put `submit_feedback` inside a host MCP
  server that authenticates the *caller* and sends as them — then the comms
  lane's acks reach the filer's own inbox directly. For e2a that means a tool
  in e2a's own `mcp/` server; the contract above is unchanged.
