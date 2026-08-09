# Diagnostic matrix

Use the minimum reads for the stated symptom. Begin every diagnosis with the e2a MCP
`whoami` tool. Keep each read scoped to the identified resource and preserve cursors
only when the first page cannot answer the question.

| Symptom | Minimum read-only evidence | Interpret before repairing |
| --- | --- | --- |
| MCP connection or authorization | `whoami` | Separate unavailable MCP/authentication from account versus agent scope. If `whoami` cannot run, stop dependent reads and record them as skipped. |
| Inbox access | `get_agent` for a named inbox; `list_agents` only to discover account inboxes | A bound agent credential can read its own inbox; `list_agents` is account scope. Missing access is authorization, not an absent inbox, until an authorized read says so. |
| Held inbound or outbound mail | `get_protection`, `list_pending_messages` | Both are account scope. Compare the configured gate/scan policy with the hold; a returned hold is pending work, not a failed resend. |
| Recipient suppression | `list_suppressions` and `list_agent_suppressions` | Account suppressions (bounce/complaint) and agent suppression (unsubscribe/manual) are distinct. Both lists are account scope. |
| Custom-domain receive/send readiness | `get_domain`, then read-only public DNS for the exact issued records | Evaluate inbound and outbound capabilities separately. Compare every issued record; do not treat registration as readiness. |
| Webhook not arriving | `get_webhook`, `list_webhook_deliveries` | Read enabled state, filters, subscribed events, then delivery status, attempts, error, and HTTP status. Delivery history is evidence; do not fire a test during diagnosis. |
| One message's delivery | `get_message_lifecycle` | Read the ordered transitions for the exact message. It records accepted/submitted/provider feedback separately and does not prove inbox placement. |

## State classification

Classify evidence before ranking it:

- **Configuration failure:** A valid read identifies a wrong or missing domain record,
  disabled/misfiltered webhook, known suppression, incompatible protection policy, or
  terminal lifecycle failure. Offer one specific repair.
- **Authorization failure:** `whoami` fails, the credential scope cannot access the
  required read, or the server returns 401/403. Mark every blocked dependent check as
  **skipped**, not passed, and name the needed reauthorization or account scope.
- **Asynchronous pending:** A message is `accepted`, `scheduled`, or `pending_review`;
  a lifecycle or webhook delivery is pending; or domain capability/sending status is
  pending while DNS propagates. Do not retry the send and do not mistake pending for a
  configuration defect without contrary evidence.
- **Transient service failure:** A timeout, network error, 429, or 5xx prevents a read.
  Preserve state, report the failed read and evidence, and propose a later read-only
  recheck rather than a mutation.

If several classes apply, rank a definite configuration or authorization cause above a
transient possibility, and explain the dependency between them. Never infer a pass from
an unavailable read, an empty partial page, or unobserved public DNS.
