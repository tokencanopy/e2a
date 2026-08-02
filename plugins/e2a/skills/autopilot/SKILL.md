---
name: autopilot
description: Conversationally configure and operate a policy-first, always-on local e2a email agent. Use when someone wants an agent to monitor an inbox, handle support or another bounded task, review unauthorized senders, require human approval for outbound mail, CC an owner, or run Claude Code, Codex, OpenClaw, Hermes Agent, or a custom runtime unattended.
version: 2
---

# e2a Autopilot

Autopilot turns one existing e2a inbox into an always-on local agent trigger. It
uses the current e2a protection and CLI surfaces; it does not require or propose
server, API, database, SDK, MCP, or core-CLI changes.

Use `tether` instead when the owner wants to steer an already-running interactive
session by email. Use a cloud webhook instead when a stable public service should
own the workflow. Autopilot is the local-machine daemon: `e2a listen` connects
outbound, so the machine does not need a public endpoint.

## Required conversational behavior

Conduct onboarding as a conversation. Ask one logical question at a time,
reflect material answers, explain a tradeoff before asking for a risky opt-out,
and skip facts the user already supplied. Do not paste the entire questionnaire.

The interview must establish:

1. The task outcome. Offer the built-in customer-support starter or a custom task.
2. For support: scope, exclusions, approved knowledge, escalation conditions,
   tone/signature, response expectations, and refund/billing/security/legal limits.
3. An existing e2a inbox and its human owner.
4. One inbound authorization mode: exact sender addresses or verified sender
   domains. The current protection gate cannot mix both modes on one inbox.
5. Non-matching inbound mail goes to e2a human review. Do not offer a silent
   bypass or “any authenticated internet sender”; the current surface cannot
   express the latter safely.
6. Outbound review, default **on**. Turning it off requires the warning and the
   exact acknowledgement requested by the interview.
7. Owner CC on every reply, default **on**. Turning it off requires a warned
   acknowledgement.
8. Prompt-injection screening, recommended and default **on**. Turning it off
   requires a warned acknowledgement.
9. Claude Code, Codex, OpenClaw, Hermes Agent, or a custom executable; its
   absolute path, workspace, and isolation mode. OpenClaw, Hermes, and custom
   executables require an acknowledged external isolation boundary because
   Autopilot cannot verify one for them. A built-in container runner is not
   implemented in this release; use an explicitly reviewed custom wrapper.
10. launchd, systemd, or foreground operation.

Use the plugin-local entrypoint for the authoritative interview and policy:

```bash
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" interview
```

It saves after every answer and can resume. When this skill is installed outside
Claude Code, replace `${CLAUDE_PLUGIN_ROOT}` with the plugin root.

## Confirmation is a hard boundary

Render the deterministic plan after onboarding:

```bash
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" plan
```

Show the complete output, limitations, and 64-character plan digest to the user.
Then ask for confirmation of that exact digest. A casual earlier “yes,” approval
of the design, or permission to continue polishing is not installation approval.

Only after the user confirms the displayed digest may you run:

```bash
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" install --confirm <digest>
```

`install` is visibly mutating. It uses the operator's current account-scoped e2a
CLI session to verify inbox ownership, save the existing protection document,
mint one dedicated agent-scoped supervisor key, apply the confirmed protection,
verify it, and create owner-only local state and a secret-free service definition.
It rolls back protection and revokes the new key if installation fails. It never
stores the account credential.

Installation deliberately does **not** start the service. Starting an always-on
agent is a second intentional action. Ask before running:

```bash
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" start --agent support@example.com
```

Never start Autopilot merely because install succeeded.

## Enforced architecture

The supervisor owns the agent-scoped e2a key. A task runtime never receives an
e2a key, MCP credential, forward token, or arbitrary mailbox access. Each job gets
a new local socket and capability exposing only:

- `get_current_message`
- `get_current_thread`
- `submit_reply`
- `escalate`
- `complete`

The gateway binds reads and replies to the current job, chooses reply recipients
from the existing thread, and injects the owner CC when configured. It has no
list, search, delete, arbitrary-recipient, key-management, protection, or review-
approval operation. Do not “help” a runtime by adding direct e2a MCP or CLI access.

Inbound delivery is defense in depth:

- e2a's existing protection gate allows the selected authenticated senders and
  sends every non-match to human review;
- the local receiver independently refuses an event that does not match policy;
- prompt-injection screening is a separate content check for allowed mail; and
- a message released by a reviewer is found by listener delivery or periodic
  unread reconciliation.

Durable metadata-only jobs move through `pending`, `running`, `retry`, `done`,
and `dead`. Message bodies are fetched only for the active job and are not written
to the spool or logs. Stale running jobs recover after restart. Listener bouncing
and independent unread reconciliation cover silent WebSocket gaps.

## Runtime isolation

Adapters are one-shot and receive a sanitized environment plus the job gateway.
Claude Code and Codex use non-persistent, restricted native invocations.
OpenClaw uses its documented embedded headless command and Hermes uses one-shot
safe mode, but neither is treated as an isolation boundary: both require the
same explicit custom-isolation acknowledgement as a custom executable.

The OS account is still a trust boundary. A hostile process running as the same
user may inspect owner-readable files or process state. A prompt that says “do not
read secrets” is not a sandbox; do not describe it as one.

## Operations

```bash
# Foreground debugging
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" run --agent support@example.com

# Service lifecycle
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" start --agent support@example.com
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" stop --agent support@example.com

# Local status; --verify temporarily uses the current account CLI session to
# compare observable e2a protection fields with the installed policy.
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" status --agent support@example.com
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" status --agent support@example.com --verify

# Paths only by default; add --follow to tail
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" logs --agent support@example.com --follow
```

After starting, verify all of these before declaring success:

1. Status reports the service running and `status --verify` reports
   `matches-policy`.
2. Logs show the loopback receiver and supervisor starting without secret values.
3. An authorized synthetic sender creates one job and a reply stays in-thread,
   CCs the owner, and enters outbound review when enabled.
4. A non-authorized synthetic sender is held by e2a review and creates no local
   runtime job.
5. Releasing that held message makes reconciliation enqueue it once.
6. Restarting the service neither loses nor duplicates durable work.

Use only synthetic addresses and content in repository fixtures and public logs.

## Uninstall

Show the uninstall plan and require the literal confirmation `DELETE`:

```bash
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" uninstall \
  --agent support@example.com --confirm DELETE
```

Uninstall stops the service, revokes its dedicated key, removes the service
definition, and moves local state to a recoverable timestamped archive. It does
not restore the pre-install protection automatically: an administrator may have
changed it later, and a blind restore could overwrite intentional security work.

## Truthful limitations

Always disclose these in the plan:

1. Owner CC is enforced by the local gateway, not the server.
2. Account administrators can later change protection; the daemon has no durable
   revision signal. `status --verify` is an explicit drift check.
3. The current CLI puts the listener forward token in a child-process argument,
   visible to the same OS user.
4. Public-any-authenticated-sender mode is not supported in this release.
5. Human reviewers are trusted and can edit an approved outbound message.
