# Autopilot Harness-Only Implementation Plan

**Design:** `docs/superpowers/specs/2026-08-02-autopilot-policy-first-design.md`

## Scope guard

The implementation may modify only:

- `plugins/e2a/skills/autopilot/**`;
- plugin-local tests and fixtures;
- `plugins/e2a/` manifests or documentation when required; and
- repository validation scripts or CI wiring needed to validate this plugin.

It must not modify server, migrations, API/OpenAPI, core CLI, SDK, MCP, or web
code. Installation may exercise current CLI commands against a fake CLI in tests
and against the user's existing e2a account after interactive confirmation.

## Phase 1: Policy and interview core

1. Add a versioned local policy schema containing:
   - task/profile;
   - agent and owner addresses;
   - one inbound authorization mode per inbox (exact addresses or verified
     domains) and its entries;
   - inbound fallback action;
   - outbound review and owner-CC booleans;
   - screening choice;
   - runtime/sandbox adapter;
   - service manager; and
   - retry, timeout, and reconciliation settings.
2. Add schema parsing, normalization, and validation with actionable errors.
3. Implement the conversational interview as a resumable state machine.
4. Add the customer-support starter questions and task prompt generator.
5. Implement default-on review and owner CC with explicit warned opt-outs.
6. Render a deterministic mutation/limitation plan and require a confirmation
   token tied to the plan digest.
7. Test branching, resume, validation, warning acknowledgements, and plan digest
   invalidation.

## Phase 2: Confirmed installation

1. Wrap the existing `e2a` executable behind a small command adapter so all
   behavior can be tested with a fake binary.
2. Preflight executable availability, login scope, agent ownership, runtime,
   filesystem permissions, and selected service manager.
3. Read and save the current protection document before any mutation.
4. Create the dedicated agent key using existing CLI behavior.
5. Apply the confirmed inbound allow/review and outbound review configuration
   using current behavior only.
6. Create owner-only directories, policy/task files, and secret storage using
   atomic writes.
7. Generate but do not start the service until post-install verification passes.
8. On failure, restore prior protection, revoke the newly created key, and remove
   only files created by this attempted install.
9. Test success, every intermediate failure point, rollback failure reporting,
   idempotency, permissions, and redaction.

## Phase 3: Durable supervisor

1. Replace the prototype in-memory seen set with an atomic filesystem spool.
2. Bind the receiver to loopback on an ephemeral or configured local port and
   require a random forward token.
3. Supervise the existing `e2a listen --forward` child with bounded restart
   backoff and the current forced-reconnect behavior.
4. Persist message ID and safe metadata before acknowledging intake; discard the
   forwarded body.
5. Implement atomic `pending/running/retry/done/dead` transitions and startup
   recovery for stale running jobs.
6. Reconcile unread messages periodically with the existing CLI and deduplicate
   them by message ID.
7. Add bounded parallelism, runtime timeout, exponential retry, and dead-letter
   owner notification.
8. Test crash points, duplicate events, reconnects, released-review messages,
   retry timing, and shutdown.

## Phase 4: Job-scoped gateway

1. Create a per-job local socket and random capability.
2. Implement only `get_current_message`, `get_current_thread`, `submit_reply`,
   `escalate`, and `complete`.
3. Resolve message/thread reads inside the supervisor using the agent key.
4. Reject any message ID or thread ID not bound to the current job.
5. Lock reply recipients to the thread, inject owner CC when enabled, and reject
   recipient expansion or owner-CC removal.
6. Interpret the existing e2a reply result, including `pending_review`, without
   granting the runtime review permissions.
7. Strip e2a and account credentials from the runtime environment and prompt.
8. Test cross-job access, malformed calls, replay, token mismatch, recipient
   tampering, CC insertion, and credential absence.

## Phase 5: Runtime and service adapters

1. Define a common adapter contract for command, environment, prompt transport,
   timeout, and sandbox declaration.
2. Add documented adapters for Claude Code and Codex.
3. Add OpenClaw and Hermes Agent adapters after verifying their current local
   invocation and isolation controls; otherwise generate an explicit custom
   adapter template and mark the named adapter unavailable.
4. Add a custom command adapter that requires the operator to acknowledge its
   isolation boundary.
5. Generate launchd and systemd definitions without embedding secrets in command
   arguments or unit files.
6. Preserve foreground `run` mode for debugging and container/custom supervisors.
7. Test command construction, environment filtering, timeout/termination, and
   generated service definitions.

## Phase 6: Operator commands and docs

1. Provide `interview`, `plan`, `install`, `run`, `start`, `stop`, `status`,
   `logs`, and `uninstall` through one plugin-local entrypoint.
2. Make read-only and mutating commands visually distinct.
3. Add `status --verify` to compare local policy with protection fields available
   through the current account-scoped CLI session.
4. Document unauthorized-message review and review-release reconciliation.
5. Document all accepted limitations from the design.
6. Update `SKILL.md` so the coding agent follows the conversational flow and does
   not bypass confirmation.

## Phase 7: Release gates

1. Run plugin-local unit and integration tests with fake runtime and e2a binaries.
2. Run the repository plugin validator.
3. Run shell and JavaScript syntax checks.
4. Verify secret-like fixtures use synthetic values and no production data.
5. Enforce the scope guard by checking the branch diff for forbidden paths.
6. Perform a code review focused on authorization, persistence, rollback,
   redaction, and truthful limitations.
7. Run a clean-room scripted installation against the fake CLI, then exercise a
   message through review release, reply submission, restart, and status.

## Completion criteria

- A new user can configure a customer-support autopilot conversationally.
- The exact plan is confirmed before local or remote mutation.
- Unauthorized inbound messages are configured to enter existing e2a review.
- Outbound review and owner CC are default-on with warned opt-outs.
- The runtime cannot read or delete unrelated mailbox data and receives no e2a
  credential.
- The daemon recovers from crashes and listener gaps without duplicate execution.
- The implementation diff contains no server, API, database, core CLI, SDK, MCP,
  or web changes.
