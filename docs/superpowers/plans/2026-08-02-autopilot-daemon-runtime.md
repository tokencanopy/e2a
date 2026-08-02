# Durable Autopilot Daemon and Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the prototype runner with a durable, least-privilege daemon that safely supervises the listener, job spool, capability gateway, supported/experimental runtimes, and launchd/systemd services.

**Architecture:** A dependency-light Node daemon owns the agent credential, durable filesystem spool, reconciliation, protection-revision checks, and one serial worker. Each claimed job receives an authenticated Unix-socket capability gateway and a fresh runtime sandbox; the spawned model process receives no e2a key and can perform only current-message, current-thread, one reply, or owner escalation operations. Focused ES modules isolate policy, persistence, transport, sandbox, runtime, and service responsibilities.

**Tech Stack:** Node.js 18+ ESM and built-in test runner, e2a CLI JSON interface, filesystem atomic rename/fsync, Unix sockets, `launchd`, `systemd`, Claude Code, Codex CLI, macOS Seatbelt/Claude sandbox, Linux bubblewrap/runtime sandbox.

## Global Constraints

- No legacy autopilot migration, import, backup, detection, reactivation, or compatibility path.
- No spawned runtime receives a raw e2a API key, account credential, forwarding token, or daemon environment.
- The daemon may hold only a dedicated agent-scoped e2a key; account-scoped credentials are never persisted here.
- Runtime mail access is job-scoped: current message/thread, one correlated reply, or one fixed owner escalation.
- The runtime cannot list inboxes, read unrelated mail, trash/delete, approve holds, administer agents, alter protection, or send/forward arbitrarily.
- Every inbound event is durably persisted before the local intake returns 2xx.
- Job states are exactly `received`, `queued`, `running`, `retry_wait`, `succeeded`, and `dead_letter`.
- Message bodies and secrets never enter spool records, dead letters, status output, or normal logs.
- `pending_review` is a successful human handoff; exit zero without a verified gateway result is failure.
- Protection revision missing, unreachable, or mismatched stops dequeue.
- Claude Code and Codex are supported only after the same isolation/lifecycle probes pass; Kimi, OpenClaw, and Hermes remain experimental and fail closed on any unproved boundary.
- There is no unsandboxed installation mode; only macOS and Linux are supported.
- Policy schema version 1 accepts exactly `sandbox_backend: "native_layered"`; unimplemented backend names are rejected rather than silently downgraded.
- In the first supported release, Codex accepts an empty tool-network allowlist only; a nonempty allowlist fails preflight until its sandbox can enforce exact destinations. Claude may use only domains proven by its sandbox allowlist probes.
- Runtime stdout and stderr are capped independently at 1 MiB per job; persisted diagnostic text is capped at 4 KiB after redaction.
- Rotated daemon logs are mode 0600 and retain at most five 5 MiB files. Succeeded-job metadata retains the newest 1,000 records for at most 30 days; dead letters persist until an operator explicitly resolves them.
- The listener reconnects after 1 second with exponential backoff capped at 60 seconds, resets only after 60 healthy seconds, and reconciles unread mail every 60 seconds independently of the socket.
- Local gateway requests are capped at 64 KiB. Escalation summaries are capped at 2 KiB and use an enumerated policy-defined category. Job timeout comes from policy; termination allows 10 seconds before killing the entire process group, and service shutdown drains for at most 30 seconds.
- Listener/job gateway bearer values are 32 random bytes. Every containing directory is mode 0700, and token files, result files, sockets, spool records, and logs are owner-only.
- All tests and fixtures use synthetic `.test`/`.invalid` identities and content.

## File Map

- Replace `plugins/e2a/skills/autopilot/runner.mjs` with a thin entry point.
- Create `policy.mjs`, `spool.mjs`, `e2a-cli.mjs`, `listener.mjs`, `gateway.mjs`, `job-mcp.mjs`, `sandbox.mjs`, `daemon.mjs`, `service.mjs`.
- Create runtime modules under `plugins/e2a/skills/autopilot/runtime/`.
- Create Node tests under `plugins/e2a/skills/autopilot/test/`.
- Create `plugins/e2a/skills/autopilot/policy.schema.json` and synthetic test fixtures.
- Modify `.github/workflows/test.yml` to run the daemon unit/contract tests.

---

### Task 1: Versioned Policy Validation and Agent-Isolated Paths

**Files:**
- Create: `plugins/e2a/skills/autopilot/policy.mjs`
- Create: `plugins/e2a/skills/autopilot/policy.schema.json`
- Create: `plugins/e2a/skills/autopilot/test/policy.test.mjs`

**Interfaces:**
- Produces: `loadPolicy(path): Promise<AutopilotPolicy>`.
- Produces: `validatePolicy(input, { platform, preflightResult? }): AutopilotPolicy`.
- Produces: `agentSlug(email): string`, the first 20 lowercase hex characters of SHA-256 over the normalized address.
- Produces: `statePaths(baseDir, email)` returning absolute agent-specific policy, secret, spool, log, runtime, result, and socket paths.
- Produces: `classifySenderLane(policy, message): "customer" | "operator" | "review_approved" | "deny"` from canonical Header-From, aligned DMARC evidence, and the server's inbound review-release status.

- [ ] **Step 1: Write failing schema and normalization tests**

Use the approved schema-v1 Customer Support example with synthetic values. Cover unknown keys, any sandbox backend other than `native_layered`, relative mount paths, mount nesting/symlink conflicts, malformed mailboxes/domains, zero/negative timeout/queue/attempt values, unsupported platform, a supported runtime without a passing preflight, owner CC opt-out, and experimental-runtime acknowledgement. Test sender lanes: aligned exact operator, aligned operator domain, aligned non-operator customer under Customer Support, `review_approved` for a server-released held message, and `deny` for missing/failing DMARC, missing canonical Header-From, or a Coding/Custom nonmatch without human release.

- [ ] **Step 2: Run the policy tests**

Run: `node --test plugins/e2a/skills/autopilot/test/policy.test.mjs`

Expected: FAIL because `policy.mjs` does not exist.

- [ ] **Step 3: Implement strict schema-v1 parsing**

Use explicit key sets at every object layer rather than permissive object spreading. Freeze the normalized returned object. Require absolute canonical mounts and reject any writable directory that contains or is contained by a read-only mount. Validate runtime/status pairs:

```js
const runtimeStatus = new Map([
  ["claude", "supported"],
  ["codex", "supported"],
  ["kimi", "experimental"],
  ["openclaw", "experimental"],
  ["hermes", "experimental"],
]);
```

- [ ] **Step 4: Implement deterministic state paths**

Return paths beneath `<baseDir>/<agentSlug>/`; never put the raw address into a service filename or socket pathname. Reject a base path owned by another uid or writable by group/other.

- [ ] **Step 5: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/policy.test.mjs`

Expected: PASS.

```bash
git add plugins/e2a/skills/autopilot/policy.mjs plugins/e2a/skills/autopilot/policy.schema.json plugins/e2a/skills/autopilot/test/policy.test.mjs
git commit -m "feat(plugin): validate versioned autopilot policy"
```

### Task 2: Durable Filesystem Job Spool

**Files:**
- Create: `plugins/e2a/skills/autopilot/spool.mjs`
- Create: `plugins/e2a/skills/autopilot/test/spool.test.mjs`

**Interfaces:**
- Produces class `JobSpool` with `init()`, `receive(event)`, `promoteReceived(id)`, `claimNext()`, `retry(id, failure)`, `succeed(id, result)`, `recoverRunning()`, `promoteDueRetries(now)`, `stats()`, and `prune(now)`.
- Produces records containing only event/message IDs, agent, timestamps, attempt count, next-attempt time, bounded diagnostic code, and final outbound ID/disposition.

- [ ] **Step 1: Write failing state-transition tests**

Pin this graph and reject every other transition:

```text
received -> queued -> running -> succeeded
running -> retry_wait -> queued
running -> dead_letter
```

Test normalization of both `email.received` and inbound `email.review_approved` envelopes to the same stable message ID. Test atomic deduplication by message ID, the policy's exact max queue depth, crash recovery from `running`, capped exponential retry, max-attempt dead letter, the 4 KiB diagnostic limit, the 1,000-record/30-day succeeded retention rule, and absence of subject/body/HTML fields in every serialized record.

- [ ] **Step 2: Run spool tests**

Run: `node --test plugins/e2a/skills/autopilot/test/spool.test.mjs`

Expected: FAIL because `JobSpool` is undefined.

- [ ] **Step 3: Implement atomic storage**

For every write: create a mode-0600 temp file in the destination directory, write the complete JSON, `fsync` the file, rename to `<message-id>.json`, and `fsync` the directory. Treat the state directory containing the record as authoritative after a crash. Validate message IDs before using them as filenames.

- [ ] **Step 4: Implement recovery, retry, bounds, and pruning**

Use `min(300_000, 1_000 * 2 ** (attempt - 1))` milliseconds plus deterministic test-injectable jitter. `recoverRunning()` moves interrupted work to retry or dead letter without losing attempts. Truncate redacted diagnostics to 4 KiB. `prune()` retains only the newest 1,000 succeeded records and removes any succeeded record older than 30 days; it never automatically removes dead letters.

- [ ] **Step 5: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/spool.test.mjs`

Expected: PASS.

```bash
git add plugins/e2a/skills/autopilot/spool.mjs plugins/e2a/skills/autopilot/test/spool.test.mjs
git commit -m "feat(plugin): add durable autopilot job spool"
```

### Task 3: Minimal e2a CLI Boundary and Job Capability Gateway

**Files:**
- Create: `plugins/e2a/skills/autopilot/e2a-cli.mjs`
- Create: `plugins/e2a/skills/autopilot/gateway.mjs`
- Create: `plugins/e2a/skills/autopilot/job-mcp.mjs`
- Create: `plugins/e2a/skills/autopilot/test/e2a-cli.test.mjs`
- Create: `plugins/e2a/skills/autopilot/test/gateway.test.mjs`
- Create: `plugins/e2a/skills/autopilot/test/job-mcp.test.mjs`

**Interfaces:**
- Produces `E2ACLI` methods: `getMessage`, `getThread`, `reply`, `sendOwnerAlert`, `getProtectionRevision`, `listUnread`, `getOutbound`.
- Produces `JobGateway.start({ job, policy, e2a, resultPath }): Promise<{ socketPath, tokenPath, close }>`.
- Produces Unix-socket HTTP routes: `GET /v1/job/message`, `GET /v1/job/thread`, `POST /v1/job/reply`, `POST /v1/job/escalate`.
- Produces MCP tools: `get_current_message`, `get_current_thread`, `reply_to_current_message`, `escalate_to_owner`.

- [ ] **Step 1: Write failing CLI-wrapper tests**

Inject a fake spawn function and assert argv arrays contain no shell, secrets, or user-controlled command fragments. Parse JSON stdout, cap stderr, treat CLI exit code 3/`pending_review` as successful handoff, and redact keys from errors. Assert `listUnread` requests inbound unread only and `getProtectionRevision` calls only the revision endpoint command.

- [ ] **Step 2: Write failing gateway authorization tests**

Start on a temporary Unix socket. Assert missing/wrong tokens get 401; the claimed message and its thread are readable; unrelated IDs are rejected; only one terminal reply/escalation is accepted; no list/trash/delete/admin/protection/review/send/forward route exists. Reject requests above 64 KiB, summaries above 2 KiB, categories outside the policy enum, wrong reply parent/thread, and symlinked result paths.

- [ ] **Step 3: Write failing idempotency and immutable-result tests**

Derive keys as SHA-256 over `autopilot:v1:<job-id>:reply` or `:escalate`. Assert retries reuse the same key. Write the returned outbound ID and disposition through an owner-only temp+rename outside the runtime writable directory. Pin gateway JSON: reply accepts `{ "text": string, "html"?: string }`; escalation accepts `{ "category": string, "summary": string }`; terminal success returns `{ "message_id": string, "status": string }`. The read routes return only the claimed server `MessageView` and at most 100 messages sharing its public `thread_id`.

Verify escalation recipients are exactly `[policy.agent.owner_email]`, with subject `[e2a autopilot] Escalation: <category>` and text containing only agent address, source message ID, category, and the at-most-2-KiB summary. It must not quote the source body. Reply text/HTML together are capped at 1 MiB before the normal server request limits.

- [ ] **Step 4: Run the three test files**

Run: `node --test plugins/e2a/skills/autopilot/test/{e2a-cli,gateway,job-mcp}.test.mjs`

Expected: FAIL because the modules do not exist.

- [ ] **Step 5: Implement `E2ACLI` with a minimal environment**

Pass only `PATH`, locale, the dedicated agent key variable, and a test-injected configuration directory. Use argv arrays and `shell: false`; never echo env or raw stdout on parse failures.

- [ ] **Step 6: Implement the authenticated Unix-socket gateway**

Read the bearer token from a mode-0600 token file created per job. Submit replies only through the server's reply-to-source-message endpoint, then require the returned outbound message to share the source `thread_id`; do not expose or depend on the internal `thread_parent_id`. Accept dispositions only from `accepted`, `scheduled`, `sent`, or `pending_review`; fetch and verify the exact outbound ID before recording success.

- [ ] **Step 7: Implement the dependency-free stdio MCP bridge**

Speak JSON-RPC/MCP over stdin/stdout, expose only the four tools, and call the Unix socket using the token read from a file path in the minimal environment. The MCP process must not read the e2a credential.

- [ ] **Step 8: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/{e2a-cli,gateway,job-mcp}.test.mjs`

Expected: PASS.

```bash
git add plugins/e2a/skills/autopilot/e2a-cli.mjs plugins/e2a/skills/autopilot/gateway.mjs plugins/e2a/skills/autopilot/job-mcp.mjs plugins/e2a/skills/autopilot/test
git commit -m "feat(plugin): add job-scoped email capability gateway"
```

### Task 4: Supervise Metadata Listener and Reconciliation

**Files:**
- Create: `plugins/e2a/skills/autopilot/listener.mjs`
- Create: `plugins/e2a/skills/autopilot/test/listener.test.mjs`

**Interfaces:**
- Consumes the metadata-only CLI flag from the listener plan.
- Produces `ListenerSupervisor` with `start()`, `stop()`, `status()`, and injected clock/spawn/fetch dependencies.
- Emits callbacks `onEvent(event)` and `onTerminalReplacement(details)`.

- [ ] **Step 1: Write failing durable-ack tests**

Start the intake server on `127.0.0.1:0`. Assert it returns 204 only after `onEvent` resolves, returns 503 when persistence fails or queue is full, rejects non-loopback or bad bearer tokens, rejects bodies above 64 KiB, and never logs the token or event body.

- [ ] **Step 2: Write failing lifecycle tests**

Assert argv includes `--forward-metadata-only`, the chosen ephemeral URL, and `--forward-token-file <owner-only-path>` but never the process-lifetime token value. Feed both `email.received` and inbound `email.review_approved` envelopes through `onEvent`; reject outbound review events. Pin 1-second exponential backoff capped at 60 seconds, reset only after 60 healthy seconds, 60-second independent reconciliation, 10-second graceful child shutdown, missing executable health, and replaced exit code as terminal with no reconnect.

- [ ] **Step 3: Run the listener test**

Run: `node --test plugins/e2a/skills/autopilot/test/listener.test.mjs`

Expected: FAIL because `ListenerSupervisor` does not exist.

- [ ] **Step 4: Implement supervisor and reconciliation hooks**

Write a 32-byte random process-lifetime token to a mode-0600 daemon-owned file, then use `e2a listen <agent> --forward <url> --forward-token-file <path> --forward-metadata-only` without `--json`, keeping listener stdout free of event metadata. Delete/replace that token file on listener restart. Reset backoff only after 60 healthy seconds. Every 60 seconds, reconciliation calls `listUnread`; an SDK/CLI message with `inboundReviewStatus` becomes a synthetic review-approved envelope, while ordinary unread mail becomes a received envelope. Both feed the same `onEvent` callback and rely on spool dedupe.

- [ ] **Step 5: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/listener.test.mjs`

Expected: PASS.

```bash
git add plugins/e2a/skills/autopilot/listener.mjs plugins/e2a/skills/autopilot/test/listener.test.mjs
git commit -m "feat(plugin): supervise metadata listener and reconciliation"
```

### Task 5: Runtime Adapter Contract and Process Lifecycle

**Files:**
- Create: `plugins/e2a/skills/autopilot/runtime/base.mjs`
- Create: `plugins/e2a/skills/autopilot/runtime/claude.mjs`
- Create: `plugins/e2a/skills/autopilot/runtime/codex.mjs`
- Create: `plugins/e2a/skills/autopilot/runtime/kimi.mjs`
- Create: `plugins/e2a/skills/autopilot/runtime/openclaw.mjs`
- Create: `plugins/e2a/skills/autopilot/runtime/hermes.mjs`
- Create: `plugins/e2a/skills/autopilot/test/runtime.test.mjs`

**Interfaces:**
- Produces adapters with `detect()`, `preflight(context)`, `buildInvocation(context)`, `normalizeOutcome(result)`, and `terminate(handle, graceMs)`.
- Produces `runRuntime(adapter, context): Promise<RuntimeResult>` with bounded output and full process-group termination.

- [ ] **Step 1: Write the shared adapter contract tests**

For every adapter, assert argv is an array with `shell: false`, the environment contains only adapter-approved names, stdout and stderr each stop at 1 MiB, timeout sends graceful termination then kills the process group after 10 seconds, and exit zero without a gateway result is unsuccessful. No email body or generated task prompt may appear in argv or process titles. Verify exact command shapes:

```text
claude --bare -p --no-session-persistence --tools "" --strict-mcp-config --settings <isolated-settings> --mcp-config <job-mcp-config> --output-format json
codex exec --ignore-user-config --ignore-rules --sandbox workspace-write --ephemeral --json -
kimi -p "Read and execute /job/prompt.txt" --auto --skills-dir <isolated-skills-dir>
openclaw agent exec "Read and execute /job/prompt.txt"
hermes chat -q "Read and execute /job/prompt.txt" --ignore-user-config --ignore-rules
```

Claude and Codex receive prompt content on stdin. Experimental runtimes receive only the constant file instruction above and a mode-0600 `/job/prompt.txt` inside the disposable sandbox. Secrets, tokens, subjects, senders, and bodies never appear in argv.

- [ ] **Step 2: Add success/failure normalization tests**

Assert success requires runtime success plus a verified gateway result. Pin nonzero exit, spawn error, timeout, malformed output, missing terminal action, wrong outbound ID, and `pending_review` as successful handoff.

- [ ] **Step 3: Run the runtime test**

Run: `node --test plugins/e2a/skills/autopilot/test/runtime.test.mjs`

Expected: FAIL because adapters do not exist.

- [ ] **Step 4: Implement the common runner and adapters**

Build a fresh process group, cap each output stream at 1 MiB, persist only a redacted 4 KiB diagnostic, and return structured outcomes. Claude and Codex report `supported` only after sandbox preflight. Kimi/OpenClaw/Hermes always report `experimental` and require the explicit policy acknowledgement plus the common isolation probes.

- [ ] **Step 5: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/runtime.test.mjs`

Expected: PASS.

```bash
git add plugins/e2a/skills/autopilot/runtime plugins/e2a/skills/autopilot/test/runtime.test.mjs
git commit -m "feat(plugin): add isolated runtime adapter contract"
```

### Task 6: Fail-Closed Filesystem, Credential, Process, and Network Sandbox

**Files:**
- Create: `plugins/e2a/skills/autopilot/sandbox.mjs`
- Create: `plugins/e2a/skills/autopilot/test/sandbox.test.mjs`
- Create: `plugins/e2a/skills/autopilot/test/fixtures/escape-probe.mjs`

**Interfaces:**
- Produces `createSandboxPlan(policy, adapter, jobPaths): SandboxPlan`.
- Produces `runEscapeProbes(plan): Promise<PreflightReport>`.
- `SandboxPlan` contains outer sandbox argv/profile, isolated HOME, explicit read-only mounts, disposable write path, runtime-auth mounts, Unix-socket/token mounts, and minimal environment.

- [ ] **Step 1: Write failing structural sandbox tests**

Assert the plan excludes host HOME, shell startup files, SSH, cloud CLI, GitHub, browser, personal documents, unrelated repositories, other agent state, daemon key, and forwarding token. Assert only policy read-only mounts and one disposable write directory are visible. Reject symlink/traversal and mount overlap.

- [ ] **Step 2: Write failing escape-probe tests**

The fixture must attempt allowed read/write/delete; forbidden read/write/delete; `..` traversal; symlink escape; excluded credential enumeration; unapproved network connection; and a grandchild process surviving termination. Every forbidden operation must fail unambiguously.

- [ ] **Step 3: Run sandbox tests**

Run: `node --test plugins/e2a/skills/autopilot/test/sandbox.test.mjs`

Expected: FAIL because the sandbox planner and probes do not exist.

- [ ] **Step 4: Implement macOS and Linux plans**

On macOS, require a generated Seatbelt profile plus the runtime’s own tool sandbox/network allowlist. On Linux, require bubblewrap for the outer filesystem namespace plus the runtime’s own command/network sandbox. Create an isolated ephemeral HOME and adapter config. Broker only the selected runtime’s model authentication read-only; do not inherit unrelated environment variables.

For Claude, generate private settings with sandbox enabled, `WebFetch`/`WebSearch` denied, and only policy domains plus the job Unix socket allowed. For Codex, generate an isolated `CODEX_HOME` with `workspace-write`, tool-command network disabled, and only the local job MCP configured; reject a nonempty policy network allowlist for Codex in this release. If installed runtime versions cannot express these controls, preflight fails.

- [ ] **Step 5: Make experimental adapters fail closed**

Kimi, OpenClaw, and Hermes may run only when the common outer boundary and their own network/tool boundary pass every probe. An unsupported or ambiguous probe returns `{ ok: false, reason: "isolation_unproved" }`; there is no bypass flag.

- [ ] **Step 6: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/sandbox.test.mjs`

Expected: PASS on the current platform unit fixtures; platform smoke remains gated in Task 9.

```bash
git add plugins/e2a/skills/autopilot/sandbox.mjs plugins/e2a/skills/autopilot/test/sandbox.test.mjs plugins/e2a/skills/autopilot/test/fixtures/escape-probe.mjs
git commit -m "feat(plugin): fail closed on autopilot sandbox escapes"
```

### Task 7: Orchestrate Durable Daemon State and Verified Outcomes

**Files:**
- Create: `plugins/e2a/skills/autopilot/daemon.mjs`
- Replace: `plugins/e2a/skills/autopilot/runner.mjs`
- Create: `plugins/e2a/skills/autopilot/test/daemon.test.mjs`

**Interfaces:**
- Consumes all modules from Tasks 1–6.
- Produces `AutopilotDaemon.start()`, `status()`, `drainAndStop(deadline)`, and `runOnce()`.
- Produces thin entry point `node runner.mjs --state-dir <absolute-agent-state-dir>`.

- [ ] **Step 1: Write failing end-to-end daemon tests with fakes**

Cover: persist-before-ack; serial claim; revision check before each claim; revision mismatch/unavailable pauses dequeue; fetch body only after claim; authenticated intake; human-approved unauthenticated intake through `inboundReviewStatus`; rejection of unauthenticated non-released mail; verified reply success; owner-only escalation success; silent exit retry; restart recovery; dedupe across listener/reconciliation; retry cap/dead letter; replaced listener clean stop; missing runtime/listener bounded unhealthy status; stop drains or returns running job to recovery.

- [ ] **Step 2: Run daemon tests**

Run: `node --test plugins/e2a/skills/autopilot/test/daemon.test.mjs`

Expected: FAIL because `AutopilotDaemon` does not exist.

- [ ] **Step 3: Implement startup ordering and worker loop**

Load/validate policy and secrets, initialize/recover spool, verify the current opaque revision, run runtime preflight, start intake/listener, start reconciliation, then dequeue one job at a time. Do not start worker claims when any fail-closed health condition is active.

- [ ] **Step 4: Implement verified completion and bounded shutdown**

For each job: claim, fetch current message/thread, classify the sender lane from canonical Header-From, aligned DMARC evidence, and inbound review status, refuse a `deny` result, start the gateway with the fixed `execution.local_capabilities`, launch the sandboxed runtime, require the exact gateway result, verify the exact outbound, atomically succeed or retry, then tear down token/socket/workdir. A `review_approved` lane authorizes only that released message. Operator classification changes only the policy-approved instruction lane; it never adds mounts, tools, network, or account rights dynamically. On shutdown, stop intake first, drain for at most 30 seconds, kill the process group after the 10-second termination grace if needed, and restore the job to recoverable state.

- [ ] **Step 5: Implement redacted structured health/log records**

Report policy schema, expected/current revision, drift, listener state, state counts, runtime version/status, last preflight, last success, and remediation. Enforce owner-only mode, five 5 MiB rotated log files, and a sanitizer that drops body/HTML/credential/token fields before persistence.

- [ ] **Step 6: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/daemon.test.mjs`

Expected: PASS.

```bash
git add plugins/e2a/skills/autopilot/daemon.mjs plugins/e2a/skills/autopilot/runner.mjs plugins/e2a/skills/autopilot/test/daemon.test.mjs
git commit -m "feat(plugin): orchestrate durable autopilot daemon"
```

### Task 8: Generate Agent-Isolated launchd and systemd Services

**Files:**
- Create: `plugins/e2a/skills/autopilot/service.mjs`
- Create: `plugins/e2a/skills/autopilot/test/service.test.mjs`

**Interfaces:**
- Produces `renderLaunchd(config)`, `renderSystemd(config)`, `installService(config)`, `serviceStatus(config)`, `stopService(config)`, `uninstallService(config)`.
- Service command is exactly `node <absolute-runner> --state-dir <absolute-state-dir>`.

- [ ] **Step 1: Write failing service serialization tests**

Assert structural XML/systemd escaping against spaces, quotes, newlines, `%`, and shell metacharacters. Assert service names contain only `com.e2a.autopilot.<agentSlug>` or `e2a-autopilot-<agentSlug>`. Assert no key/token/message content appears in plist, unit, argv, or environment.

- [ ] **Step 2: Add lifecycle and multi-agent tests**

With command execution faked, pin idempotent install/start/status/stop/uninstall, two-agent isolation, owner-only state permissions, systemd credential-file use when supported, launchd secret-path-only behavior, and failure that never reports active.

- [ ] **Step 3: Run service tests**

Run: `node --test plugins/e2a/skills/autopilot/test/service.test.mjs`

Expected: FAIL because service adapters do not exist.

- [ ] **Step 4: Implement adapters without shell interpolation**

Write service documents atomically, invoke `launchctl`/`systemctl --user` with argv arrays, and query actual service state before reporting success. Store credentials outside service definitions in owner-only files or systemd credential storage.

- [ ] **Step 5: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/service.test.mjs`

Expected: PASS.

```bash
git add plugins/e2a/skills/autopilot/service.mjs plugins/e2a/skills/autopilot/test/service.test.mjs
git commit -m "feat(plugin): install isolated autopilot services"
```

### Task 9: Add CI, Local End-to-End, and Platform Gates

**Files:**
- Create: `plugins/e2a/skills/autopilot/test/local-e2e.test.mjs`
- Create: `plugins/e2a/skills/autopilot/test/platform-service-smoke.mjs`
- Modify: `.github/workflows/test.yml`
- Modify: `scripts/validate-plugin.mjs`

**Interfaces:**
- Verifies all daemon/runtime interfaces and makes Node unit tests a required plugin gate.

- [ ] **Step 1: Add a local e2a integration harness**

Use the repository’s local server/Mailpit and synthetic accounts to test authenticated customer intake, unauthenticated inbound hold/release, owner-CC’d pending-review reply, owner-only escalation/no customer reply, denied list/unrelated-read/trash/review/protection operations, revision drift pause, crash recovery, replacement stop, and body-free dead letter.

- [ ] **Step 2: Add platform smoke commands**

The macOS/Linux script performs install, start, status, restart, active-job stop, recovery, uninstall, two-agent coexistence, permissions inspection, secret scan, and actual escape probes. Skip only with a clear unsupported-platform result in generic CI; release qualification requires explicit passing runs on both platforms.

- [ ] **Step 3: Wire dependency-free unit tests into CI**

Add:

```yaml
- name: Test autopilot plugin runtime
  run: node --test plugins/e2a/skills/autopilot/test/*.test.mjs
```

Keep existing plugin guidance and validation commands.

- [ ] **Step 4: Run all plugin tests and validator**

Run: `node --test plugins/e2a/skills/autopilot/test/*.test.mjs`

Run: `node --test scripts/plugin-agent-guidance.test.mjs && node scripts/validate-plugin.mjs`

Expected: PASS.

- [ ] **Step 5: Run the repository unit gate and inspect the diff**

Run: `make test-unit && git diff --check && git status --short`

Expected: PASS; no secrets, real customer data, generated body fixtures, or unplanned files.

- [ ] **Step 6: Commit**

```bash
git add plugins/e2a/skills/autopilot/test .github/workflows/test.yml scripts/validate-plugin.mjs
git commit -m "test(plugin): gate durable autopilot runtime"
```
