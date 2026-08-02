# Policy-First Autopilot Design

**Status:** Approved design

**Date:** 2026-08-02

## Goal

Turn the e2a `autopilot` plugin skill into a policy-first, production-grade onboarding and runtime system. The skill interviews the owner conversationally, applies enforceable e2a protections, validates a least-privilege execution sandbox, installs an always-on service on macOS or Linux only after explicit confirmation, and processes authorized email through a durable recoverable queue.

Customer Support is the first polished task profile. Coding/Repository and Custom profiles remain available as advanced profiles.

## Non-goals

- Migrating, importing, backing up, or reactivating legacy autopilot installations. This is a new-install flow.
- Building a hosted Token Canopy orchestration service. Autopilot remains a local-machine or user-managed-host runtime.
- Supporting Windows services in the first hardened release.
- Allowing email-triggered sessions to run without a verified isolation boundary.
- Implementing thread-aware outbound policy such as “auto-send replies but review new threads.” The current server gate is recipient-based; this mode remains out of scope until the server can enforce it without relying on agent compliance.
- Promoting Kimi, OpenClaw, or Hermes to supported status before each passes the same isolation and lifecycle suite as Claude Code and Codex.

## Design principles

1. **Policy before machinery.** The owner chooses the agent’s job, correspondents, egress rules, data boundary, and review posture before installation.
2. **Server enforcement before prompts.** Sender authentication, inbound holds, outbound holds, expiry behavior, and required owner CC are enforced by e2a. Prompts explain policy; they do not enforce it.
3. **Allowlist access, not denylist access.** The runtime sees only explicitly mounted data and explicitly allowed network destinations.
4. **Durable before acknowledged.** A received message is persisted to a recoverable job spool before the listener reports successful handoff.
5. **Human-visible by default.** The owner is CC’d by default, every outbound message is reviewed by default, and held items expire rejected.
6. **Fail closed.** Missing authentication, unverifiable sandboxing, invalid service configuration, ambiguous runtime outcomes, and policy drift stop autonomous execution.
7. **Profiles grant capabilities.** Natural-language task instructions never grant filesystem, tool, network, account, or review privileges.

## Architecture

### 1. Conversational onboarding skill

`/autopilot` conducts a one-question-at-a-time interview. It discovers the authenticated e2a account and candidate inboxes, recommends secure defaults, records explicit opt-outs, and emits a versioned policy document. It shows both a machine-readable policy and a plain-English summary before making any change.

The skill may generate configuration and run read-only preflight checks before confirmation. It may apply policy, mint credentials, and install the service only after the user explicitly confirms **Install and start autopilot** against the final summary.

### 2. Policy configurator

Setup temporarily uses account-scoped authorization to:

- read the existing protection posture;
- apply the selected inbound and outbound controls;
- enable durable owner notifications for holds;
- configure required owner CC when enabled; and
- mint a dedicated agent-scoped runtime credential.

The account-scoped credential is not written to autopilot configuration and is never passed to the daemon or a spawned runtime. The installed runtime cannot read or change its own protection configuration, approve its own holds, administer agents, or access another inbox.

### 3. Autopilot daemon

The daemon owns transport, durable job state, serial execution, retry timing, bounded logs, health reporting, and graceful shutdown. It does not reimplement the e2a sender gate. The e2a server holds inbound messages that do not satisfy the configured policy; the daemon receives only released messages.

The daemon uses a metadata-only listener path so receipt does not mark a message read. It fetches a body only after a job is durably claimed for execution.

### 4. Runtime adapters

Runtime-specific behavior lives behind a common interface. An adapter detects the executable and version, validates noninteractive operation and isolation, builds argv without a shell, constructs a minimal environment, launches a process group, terminates it safely, normalizes its outcome, and verifies the expected e2a reply or held draft.

Initial status:

- Claude Code: supported after sandbox validation.
- Codex: supported after workspace, MCP, and network validation.
- Kimi: experimental.
- OpenClaw: experimental and requires an explicitly isolated execution backend.
- Hermes: experimental and requires an isolated terminal/container backend plus credential filtering.

### 5. Service adapters

Autopilot supports macOS `launchd` and Linux `systemd`. Service definitions are agent-specific, contain no secrets, use no shell interpolation, and support idempotent install, status, stop, and uninstall operations. Multiple autopilot inboxes may run on one host without sharing ports, state, logs, credentials, or process identifiers.

## e2a protection extensions

Two server-enforced additions are required because the desired guarantees cannot be implemented safely in a prompt or local wrapper.

### Composable authenticated-inbound requirement

Add `require_authenticated` to the inbound gate. It composes with the existing `open`, `allowlist`, and `domain` policies:

- `open` plus `require_authenticated=true` admits any sender whose aligned RFC 5322 From domain passed DMARC and holds unauthenticated mail according to the configured action. This is the Customer Support default.
- `allowlist` and `domain` continue to require DMARC before matching. Setting `require_authenticated` does not weaken their existing behavior.
- Providerless or identity-unresolvable mail cannot satisfy the requirement unless it is released by a human reviewer.

The default remains `false` outside autopilot so existing agents do not change posture merely because the field is introduced.

### Required outbound CC

Add `required_cc` to the outbound protection posture as an account-controlled list of normalized recipient addresses. Autopilot initially configures either the confirmed owner address or an empty list after a warned opt-out.

The server merges and deduplicates `required_cc` into sends, replies, and forwards before recipient validation, gate evaluation, hold creation, provider submission, and reviewer override validation. Neither an agent-scoped caller nor a reviewer override can remove a required address. This guarantees owner visibility without depending on runtime instructions or CLI feature parity.

## Conversational onboarding

The interview follows this order.

### Agent identity

1. Select an existing e2a inbox or create one.
2. Read the account owner email and ask the user to confirm it.
3. Ask what job the agent should perform.

### Task profile

Offer:

- Customer Support — recommended and fully guided.
- Coding/Repository — advanced.
- Custom — advanced and permission-explicit.

The profile supplies safe starting values but cannot silently widen permissions.

### Inbound authorization

For Coding/Repository and Custom profiles, ask whether trusted operators are exact addresses or verified domains, then collect the corresponding values. Nonmatches default to `review`.

For Customer Support, default to any authenticated customer entering the restricted support lane. Ask separately for exact-address or verified-domain operator identities that may request the broader actions already granted to the profile. Unauthenticated mail is held for human review.

Every held inbound message remains retained in the account review queue. With notifications enabled, the e2a platform emails the owner a preview and approve/reject links. Approval releases the message to the inbox and emits the delivery notification that wakes autopilot. The daemon does not generate this notification.

### Outbound authorization

Offer three enforceable policies:

1. Review every outbound send, reply, and forward — default.
2. Auto-send only to an exact recipient allowlist; review all other recipients.
3. No outbound review — warned opt-out.

Every review posture uses reject-on-expiry. The summary states that `pending_review` is successful handoff to a human, not delivery and not a retry signal.

### Owner visibility

Default `required_cc` to the owner email. Allow an explicit opt-out after warning that the owner will then need to rely on the review queue, audit trail, and status command for visibility.

### Execution boundary

Collect:

- runtime and support status;
- repository or approved data sources;
- read-only mounts;
- isolated writable job area;
- network allowlist;
- maximum job runtime;
- queue-depth limit;
- retry limit;
- actions that always require human confirmation; and
- service manager, auto-detected as launchd or systemd.

### Summary and confirmation

The skill renders the agent’s job, sender lanes, outbound posture, owner visibility, data mounts, write boundary, network boundary, runtime, sandbox, service manager, and experimental warnings in plain English. It runs preflight and escape probes, then asks for the exact final confirmation. Any answer other than explicit confirmation leaves the service uninstalled.

## Versioned policy document

The policy is non-secret JSON and the single source of truth for onboarding, preflight, installation, status, drift detection, and tests. A representative Customer Support policy is:

```json
{
  "schema_version": 1,
  "agent": {
    "email": "support@agents.example.test",
    "owner_email": "owner@example.test",
    "profile": "customer_support"
  },
  "inbound": {
    "require_authenticated": true,
    "customer_mode": "authenticated",
    "operator_rule": {
      "kind": "address",
      "values": ["owner@example.test"]
    },
    "nonmatch_action": "review",
    "approved_hold_action": "enqueue",
    "notify_owner": true
  },
  "outbound": {
    "mode": "review_all",
    "allowlist": [],
    "hold_expiry": "reject",
    "required_cc": ["owner@example.test"]
  },
  "execution": {
    "runtime": "claude",
    "runtime_status": "supported",
    "sandbox_backend": "container",
    "read_only_mounts": ["/srv/support-kb"],
    "write_mode": "ephemeral_job_directory",
    "network_allowlist": ["e2a.dev", "api.e2a.dev"],
    "timeout_seconds": 1800,
    "max_queue_depth": 100,
    "max_attempts": 3
  },
  "service": {
    "manager": "systemd"
  },
  "support": {
    "knowledge_base_mounts": ["/srv/support-kb"],
    "response_mode": "review_held",
    "escalation_categories": [
      "refund",
      "legal",
      "security",
      "account_change",
      "private_data",
      "uncertain_answer"
    ]
  }
}
```

Schema validation rejects unknown keys, unsupported combinations, relative paths, unsafe mount relationships, invalid addresses/domains, nonpositive bounds, and a runtime marked supported without a passing adapter preflight.

Secrets live outside this file. Secret storage is owner-readable only and platform-appropriate; systemd credentials are used where available. Secrets never appear in service definitions, argv, logs, status output, or the policy summary.

## Customer Support profile

Customer Support is the default polished profile and the initial launch-video journey. Onboarding collects approved knowledge-base mounts, product context, response tone, supported categories, escalation categories, allowed external actions, service hours, and response expectations.

Authenticated customers receive the restricted support capability set:

- read the current message and thread;
- read the approved knowledge base;
- classify and draft a threaded response;
- submit that response to the configured outbound review gate; and
- escalate when policy requires it.

The profile has no source-code, general host filesystem, browser-profile, cloud-account, account-administration, review-approval, payment, refund, or customer-account mutation capability by default. Refunds, legal/security matters, account changes, private-data requests, and uncertain answers always escalate.

Trusted operators may request only the additional capabilities explicitly present in the policy. Operator identity never expands mounts, tools, network, or account privileges dynamically.

## Coding/Repository profile

Each job receives an isolated per-job worktree or disposable clone. The primary checkout is not mounted writable. Source inputs outside the worktree are absent or read-only. The agent produces a patch, commit, or draft PR artifact for human review. Direct primary-workspace modification is an advanced opt-in recorded in policy and rejected unless the selected sandbox can prove the configured boundary.

## Custom profile

Custom onboarding begins with no data, tool, network, or write grants. The user must select each capability explicitly. Natural-language instructions describe the job but do not alter the capability document.

## Data and execution isolation

Supported mode is allowlist-based isolation. Each job runs in a fresh sandbox containing only its approved read-only mounts, isolated writable job directory or worktree, runtime executable, minimal runtime configuration, and agent-scoped e2a credential.

The sandbox does not expose the host home directory, shell startup files, SSH keys, cloud CLI state, GitHub credentials, browser profiles, personal documents, unrelated repositories, other agents’ configuration/state, service secrets, or the autopilot forwarding token.

The runtime receives a minimal environment assembled by its adapter. The daemon’s full environment is never inherited. Network access is denied except for policy entries. Symlink and path traversal cannot escape a mount. A job may delete only inside its disposable writable area; deletion of host or primary-workspace data is structurally impossible rather than prompt-prohibited.

Preflight creates canary files inside and outside the allowed boundary and proves:

- allowed reads succeed;
- forbidden reads fail;
- allowed writes and deletes succeed only inside the disposable area;
- writes, deletes, traversal, and symlink escapes outside fail;
- unapproved network connections fail;
- the runtime cannot enumerate or access excluded credentials; and
- process termination stops the complete runtime process group.

Installation fails if any required probe cannot be executed or produces an ambiguous result. There is no unsandboxed autopilot installation mode in the hardened design.

## Durable job lifecycle

Autopilot uses a dependency-light filesystem spool with one JSON record per job and atomic rename transitions:

```text
received -> queued -> running -> succeeded
                      |-> retry_wait -> queued
                      |-> dead_letter
```

The daemon persists `received` before acknowledging the local listener handoff. A single worker claims jobs serially. On startup, interrupted `running` jobs return to `retry_wait` with their attempt metadata preserved. Job identifiers are stable e2a message IDs; completed records provide deduplication across WebSocket delivery, reconciliation, and restarts.

Nonzero runtime exit, spawn failure, missing expected reply/draft, timeout, and unverifiable result are failures. Retries use capped exponential backoff and stop at the configured attempt limit. A dead-letter record retains metadata and diagnostics but not the email body. Operational notification uses the configured owner-review path when e2a is reachable and always remains visible through status and owner-only logs.

Queue size, per-job output, log files, completed-state retention, and retry history are bounded. When intake cannot be durably recorded because the queue is full or storage fails, the local receiver returns failure. Because metadata forwarding does not mark the message read, reconciliation can recover it later.

## Listener and reconciliation

Extend `e2a listen` with a metadata-only forwarding mode that sends the authenticated event envelope without fetching the full message. Generic full-message forwarding retains its existing behavior for existing consumers.

The daemon binds an ephemeral loopback port, generates a forwarding token in memory for that process lifetime, and passes it only to the listener child. It persists an event before returning success. The independent reconciliation path lists unread inbound messages and feeds the same spool.

A WebSocket “replaced” outcome is terminal. The daemon exits cleanly rather than reconnecting and stealing the socket back. Other transient exits use capped exponential backoff that resets only after a stable connection interval. Scheduled reconnect and reconciliation remain independent liveness mechanisms until the SDK exposes a reliable idle-health signal.

## Runtime lifecycle and outcome

An adapter starts a fresh process group for one job. It caps captured stdout/stderr while preserving bounded diagnostics. Timeout first requests graceful termination, then kills the complete process group after a fixed grace period. Service stop either drains the active job within a bounded window or terminates it and returns the job to recoverable state.

A job succeeds only when both conditions hold:

1. the runtime returns its adapter-defined successful outcome; and
2. e2a shows an in-thread outbound reply or a review-held outbound draft correlated to the source message.

`pending_review` therefore counts as successful handoff. A silent exit zero does not. Retrying a failed job uses the same stable job identity and instructs the runtime to inspect the thread and existing job artifacts before acting, reducing duplicate work.

## Service installation and health

Service names include a stable hash of the agent address. Policy, spool, logs, runtime files, and credentials live in agent-specific owner-only directories. launchd plist and systemd unit values are escaped structurally and never assembled by interpolating user input into executable shell.

`autopilot status` reports:

- policy schema and server-policy drift;
- service and daemon process state;
- listener connectivity;
- queued, retrying, running, and dead-letter counts;
- runtime version, support status, and last sandbox preflight;
- last successful job timestamp; and
- actionable remediation without message bodies or secrets.

Logs are structured, redacted, rotated, size-bounded, and owner-readable only. Message bodies and credentials are not logged by default.

## Error handling

- Invalid interview answer: explain the constraint and ask the same question again.
- Account policy write failure: leave the service uninstalled and show the unchanged/current server posture.
- Credential mint failure: leave the service uninstalled and do not write partial secret state.
- Sandbox ambiguity or failed escape probe: fail closed.
- Service install failure: preserve diagnostics and policy, but do not report autopilot active.
- Listener executable missing: report unhealthy without an unhandled crash or restart storm.
- Listener replaced: stop cleanly and tell the owner which agent connection was displaced.
- Runtime unavailable or unsupported: do not dequeue work.
- Runtime failure or timeout: retry durably, then dead-letter.
- e2a unavailable: keep jobs durable, back off, and avoid duplicate sends.
- Policy drift after installation: stop dequeuing new jobs until the owner reviews and reapplies policy.

## Verification strategy

### Unit and contract tests

- Interview answers serialize to the exact policy schema and secure defaults.
- Warned opt-outs remain explicit and visible in summaries.
- `require_authenticated` composes correctly with open, address, and domain policies.
- `required_cc` is merged and cannot be removed by agent input or reviewer overrides.
- Metadata-only forwarding does not fetch or mark the message read.
- Spool transitions, restart recovery, deduplication, retry caps, and dead-letter behavior.
- Runtime argv construction, minimal environment, bounded output, timeout escalation, and process-group shutdown.
- launchd/systemd escaping, permissions, idempotency, multiple-agent isolation, and secret absence.
- Configuration/path injection, malicious sender headers, symlink traversal, oversized output, queue floods, and invalid policy combinations.

### Local end-to-end tests

Run against a real local e2a service and captured SMTP transport:

- trusted operator wakes the selected profile;
- authenticated customer enters restricted support mode;
- unauthenticated mail becomes an inbound hold and owner notification;
- owner approval releases the message and wakes autopilot;
- reply includes required owner CC;
- outbound reply becomes pending review under the default posture;
- restart during execution recovers the job exactly once;
- replacement WebSocket stops without a connection fight;
- missing listener and runtime produce bounded unhealthy states;
- read, write, delete, traversal, symlink, credential, process, and network escape probes fail outside the allowed boundary; and
- a dead-letter remains recoverable and visible without exposing content in logs.

### Runtime support gate

Claude Code and Codex must pass the same adapter contract and isolation suite before release. Kimi, OpenClaw, and Hermes remain experimental until they pass it. Experimental selection requires an explicit warning and confirmation and never bypasses the base isolation probes.

### Platform service gate

Run service-install smoke tests on macOS launchd and Linux systemd: install, start, status, restart, stop during an active synthetic job, uninstall, multi-agent coexistence, file permissions, and secret inspection.

## Acceptance criteria

The design is complete when a new user can invoke `/autopilot`, choose Customer Support, answer the policy interview, accept secure defaults, pass preflight, explicitly confirm installation, and demonstrate the complete authenticated-customer -> restricted sandbox -> owner-CC’d pending-review reply path.

No email can wake broader capabilities without satisfying the configured server gate or receiving human approval. No spawned runtime can read, modify, or delete data outside its explicit mounts. A crash, restart, timeout, missing binary, or WebSocket replacement cannot silently lose work, duplicate a completed job, or create a restart fight. Status and logs make failures actionable without revealing message content or secrets.
