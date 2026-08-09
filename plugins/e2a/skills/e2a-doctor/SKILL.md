---
name: e2a-doctor
description: "Use when an existing e2a MCP connection, inbox, custom domain, protection policy, webhook, or message delivery is failing or unclear. Diagnoses read-only through MCP first, ranks evidence-backed causes, and offers individually confirmed repairs; uses the CLI doctor only when CLI or self-hosted diagnostics are specifically relevant."
---

# Diagnose and repair e2a

## Start from the symptom

Identify the failing surface and the smallest safe target: an inbox address, domain,
webhook ID, message ID, time window, or exact error. Do not request message content
unless it is necessary to diagnose the symptom. Inspect the available e2a MCP tools;
use the e2a MCP `whoami` tool, never the shell command.

Keep diagnosis separate from repair. Begin MCP-first, read-only diagnosis with the
existing authorization; do not create a second credential or alter configuration.

## Read-only MCP diagnosis

Call `whoami` first, then load only the symptom's minimum reads from
[the diagnostic matrix](references/diagnostic-matrix.md). Use `get_agent` or
`list_agents` only to establish inbox access; read `get_protection` and
`list_pending_messages` for holds; use `list_suppressions` and
`list_agent_suppressions` for recipient blocks; inspect `get_domain` and public,
read-only DNS for domain symptoms; inspect `get_webhook` and
`list_webhook_deliveries` for a webhook; and call `get_message_lifecycle` for one
message's observed transitions.

Do not convert a missing tool, account-only scope, 401/403, or unavailable permission
into a pass. Record it as an explicit skipped check, naming the unavailable read and
the scope or authorization needed. Do not call `verify_domain`, `test_webhook`,
`redeliver_event`, or any mutation while diagnosing.

## Rank findings

Return a ranked list. Every finding must include `{cause, evidence, impact,
remediation}` and state whether it is a definite configuration failure, authorization
failure, asynchronous pending state, or transient service failure. Rank definite,
high-impact causes above possibilities; keep evidence factual and targeted.

Treat `accepted`, `scheduled`, and `pending_review` as durable outcomes, not delivery
receipts. Do not retry them: a second send can duplicate mail. Report the lifecycle or
hold state, then wait for its next transition or an authorized human decision.

## Offer guided repairs

Offer the least invasive remedy only after presenting the finding and its expected
effect. Read [guided repairs](references/guided-repairs.md) before choosing one.
Obtain confirmation for each state-changing repair; never bundle unrelated changes.
A complete DNS diff confirmed as one unit is one repair. If confirmation is declined,
leave state unchanged and report the manual next step.

After an accepted repair, perform the narrowest relevant read-only verification and
update the finding; do not re-run a broad diagnostic sweep by default.

## CLI-assisted branch

Use `e2a doctor --json` only when an already-authenticated CLI is the symptom's
relevant surface, MCP is unavailable while that CLI already exists, or self-hosted
diagnosis needs local environment or SMTP visibility. Interpret its versioned
`e2a.doctor/v1` checks as evidence, then supplement with relevant MCP reads when
available. Never install or configure the CLI solely for diagnosis.

Classify the CLI report's auth/configuration/transient outcomes separately from MCP
authorization and asynchronous message or DNS states. Preserve credentials and known
configuration when evidence is inconclusive.

## Verify and report

Report the ranked findings, explicit skipped checks, accepted repairs, and the
narrowest verification result. Distinguish a configuration fix from a permission
request, pending async work, or an operational retry. State what remains pending and
the exact safe condition for a later recheck; do not claim domain readiness from
registration or message delivery from acceptance alone.
