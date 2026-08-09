# Guided repairs

Diagnose first. Present the exact target, intended change, expected impact, and the
narrowest follow-up read. Obtain explicit confirmation before every state-changing
repair. A tool's idempotence does not remove this confirmation boundary.

| Finding | Confirmed repair | Narrow read-only verification |
| --- | --- | --- |
| MCP authorization or expired credentials | Reauthorize through the current client's MCP flow. | `whoami` |
| Missing requested inbox | `create_agent` for the exact confirmed full address. | `get_agent` or one scoped `list_messages` read |
| Incorrect protection posture or a known hold policy | `update_protection` with the reviewed field-level diff. | `get_protection`; use `list_pending_messages` only when checking a specific hold |
| Account or agent recipient block | Add or remove only the identified suppression (`create_agent_suppression`, `delete_agent_suppression`, or `delete_suppression`). Do not remove bounce/complaint suppression without deliverability evidence. | The matching suppression list |
| Webhook configuration or delivery recovery | Mutate the named webhook, `test_webhook`, or `redeliver_event` only after confirmation. Treat synthetic tests and redelivery as state-changing. | `get_webhook` or `list_webhook_deliveries` for the exact webhook/delivery |
| Issued DNS records published but domain not yet checked | Call `verify_domain` only after confirmation and a user-driven propagation wait. | `get_domain`, including inbound and outbound capabilities |
| Missing or mismatched DNS records | Show the full proposed DNS diff—every record type, name, value, priority, and delete/replace action—then obtain one confirmation for the complete diff. Apply that confirmed complete DNS diff as one repair, without abbreviating TXT/DKIM values. | Read-only public DNS for changed records, then `get_domain`; request a separately confirmed `verify_domain` only if needed |

Do not repair `accepted`, `scheduled`, or `pending_review` by sending again. For a
pending review, report the hold and wait for the authorized reviewer. For queued or
scheduled delivery, use the exact message's read-only lifecycle to observe the next
transition. For DNS propagation or transient service errors, keep configuration intact
and offer a later read-only recheck.

When confirmation is refused, record the proposed repair and leave all state unchanged.
