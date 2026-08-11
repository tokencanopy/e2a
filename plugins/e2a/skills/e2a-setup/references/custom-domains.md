# Custom-domain setup

Recommend the shared `agents.e2a.dev` domain unless the user owns a domain and
wants branded addresses. Shared-domain inboxes need no DNS: confirm the full
address, create the agent, and verify the inbox read.

## Custom-domain sequence

1. Confirm the user owns the domain and wants branded addresses.
2. Call `register_domain`, then call `get_domain` to obtain its current state
   and the required DNS records.
3. Ask which service hosts authoritative DNS. Present every proposed change as
   one complete DNS diff: record type, host/name, value, TTL, and MX priority.
4. Obtain one confirmation for that complete diff before any provider-assisted
   write. Do not seek piecemeal record approvals.
5. Apply the records through the chosen provider or give the user a clean,
   copyable record table for manual entry.
6. Call `verify_domain` once. Then make one user-driven post-verification
   `get_domain` read and report inbound verification and outbound branded-
   sending capability from that response separately.
7. If DNS propagation or either capability is not ready, report the observed
   state and stop. Resume only when the user asks: retry `verify_domain`, make
   one new `get_domain` read, and report the new state. Do not poll.
8. When both capabilities are ready, ask for the desired local part and show
   the complete branded address. Obtain confirmation for that complete branded
   address before calling `create_agent`, then call `list_messages` for the new
   inbox to verify the harmless read.

## Provider guidance

- **Cloudflare:** when an authorized [Cloudflare API MCP server](https://developers.cloudflare.com/agents/model-context-protocol/cloudflare/servers-for-cloudflare/) is available, it can assist with DNS changes. Read the current zone first, show the full diff, obtain the one confirmation above, and then apply only that diff.
- **GoDaddy:** use the authenticated [`gddy` DNS workflow](https://developer.godaddy.com/en/docs/api-users/manage-domains/dns) or its agent skill/CLI for DNS changes. The official [GoDaddy MCP server](https://developer.godaddy.com/en/docs/api-users/mcp) is read-only: it supports public search and availability, not DNS modification. Never present it as a write path.
- **Other providers or unavailable tools:** provide the record table for manual entry. Wait for the user to confirm they have published it before verification.

## Capability report

Registration alone does not make a domain ready. From the post-verification
`get_domain` response, report these separately:

- **Inbound:** e2a domain verification and DNS records needed to receive mail.
- **Outbound branded sending:** e2a's reported sending readiness for the domain.

If either remains pending, name the missing record, propagation state, or next
manual action without claiming that mail is ready. Do not create the branded
inbox until both are ready.
