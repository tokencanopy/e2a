# Policy-First Autopilot: Harness-Only Design

**Status:** Approved after scope revision on 2026-08-02

## Decision

Polish Autopilot entirely inside the e2a plugin and its local harness.

This work will make **no changes** to:

- the e2a server;
- the public or private API contract;
- the e2a database or migrations;
- the core e2a CLI; or
- the TypeScript, Python, or MCP SDKs.

Autopilot may invoke existing CLI commands and configure existing e2a protection
settings after the user confirms the exact installation plan. That is use of the
current product, not a server or API implementation change.

The earlier proposal for server-enforced owner CC, protection revisions,
authenticated-public inbound policy, and a metadata-only listener is deferred.
Git history preserves that proposal; it is not part of this implementation.

## Goal

Turn the prototype `autopilot` skill into a safe, conversational installer and
durable local supervisor for one long-running email agent. The initial built-in
profile is customer support, while the policy and runtime interfaces remain
task-agnostic.

The owner should be able to say, in conversation:

> Set up an agent that handles support email, lets messages from these customers
> reach it, asks e2a to review everyone else, CCs me on every reply, and asks for
> human approval before any outbound email.

The skill interviews the owner, shows an exact plan, applies it only after
confirmation, verifies the installation, and leaves behind a service that can be
operated without an interactive coding-agent session.

## Non-goals

- Adding a general workflow engine to e2a.
- Making the local harness a multi-tenant security boundary.
- Supporting arbitrary unauthenticated public senders in the first profile.
- Hiding configuration drift that the current e2a API cannot expose to an
  agent-scoped credential.
- Preserving compatibility with the prototype's installation or state format.
- Giving the task runtime direct e2a, MCP, shell, or filesystem credentials.

## Security model

### Trust boundaries

Autopilot has three local trust zones:

1. **Supervisor** — owns the e2a agent key, receives events, persists jobs,
   reconciles missed work, and starts runtimes.
2. **Job gateway** — exposes only the current job's permitted operations over a
   per-job local socket.
3. **Task runtime** — interprets one message and performs the configured task. It
   receives no e2a key and cannot access arbitrary mailbox messages.

The operating-system account remains a trust boundary. Another process running
as the same OS user may be able to inspect local process state or owner-readable
files. The installer states this explicitly.

### Mailbox least privilege

The runtime may:

- read the current message and its thread;
- prepare one or more replies within that thread;
- request escalation to the owner; and
- return a structured outcome.

The runtime may not:

- list or search the mailbox;
- read unrelated messages or threads;
- delete messages;
- create, rotate, or inspect e2a keys;
- change e2a protection settings;
- choose arbitrary recipients; or
- invoke the e2a CLI or MCP server directly.

The supervisor validates every gateway request against the current job before it
uses the e2a CLI.

### Inbound authorization

The interview collects exact sender addresses and/or sender domains. The
installer configures the agent's existing inbound protection as follows:

- approved address/domain entries are allowed;
- non-matching senders use the existing `review` action;
- e2a's current sender-authentication evidence remains authoritative; and
- a released message is discovered by normal listener delivery or periodic
  unread reconciliation.

The first release does not offer “any authenticated internet sender” as an
inbound profile. The current protection surface cannot express “allow every
DMARC-aligned sender, review unauthenticated senders” without platform work.
Autopilot must say so instead of silently widening access.

### Outbound review and owner visibility

The recommended default is:

- every outbound message is submitted to e2a's existing human-review queue; and
- every outbound thread CCs the owner.

Both are default-on and require a warned opt-out during onboarding.

Outbound review is configured with the current e2a protection interface. The
gateway injects the owner CC locally and rejects attempts to remove it. This is
defense in depth, not a server guarantee: an account administrator may later
change protection configuration, and a human reviewer may edit an approved
message. `autopilot status --verify` detects observable configuration drift when
run with an account-scoped CLI session; continuous server-side revision checks
are deferred.

### Prompt-injection screening

Autopilot asks whether e2a prompt-injection screening should be enabled and
recommends it. Screening and inbound authorization are distinct:

- sender authorization determines whether the message is allowed, reviewed, or
  blocked;
- content screening evaluates allowed content for prompt-injection risk.

An unauthorized message is held for e2a review before the local task runtime
processes it. Releasing it is a human decision.

## Conversational onboarding

The skill conducts an adaptive interview rather than dumping a questionnaire.
It asks one logical topic at a time, reflects the answer, explains material
tradeoffs, and skips questions already answered in the conversation.

### Required decisions

1. **Task** — what outcome should the agent produce?
2. **Profile** — customer support or custom.
3. **Mailbox** — existing or newly created agent address.
4. **Owner** — the human notification and default CC address.
5. **Authorized inbound senders** — exact addresses and/or domains.
6. **Inbound fallback** — defaults to e2a review.
7. **Outbound review** — defaults on; opting out requires a warning and explicit
   confirmation.
8. **Owner CC** — defaults on; opting out requires a warning and explicit
   confirmation.
9. **Screening** — recommends e2a prompt-injection screening.
10. **Runtime** — Claude Code, Codex, OpenClaw, Hermes Agent, or a custom command.
11. **Sandbox** — recommended adapter or acknowledged custom isolation.
12. **Service manager** — launchd, systemd, or foreground/manual.

### Customer-support starter profile

The starter profile asks for:

- support scope and excluded requests;
- knowledge sources the agent may use;
- tone and signature;
- escalation conditions;
- response-time expectations;
- refund, billing, security, and legal boundaries; and
- whether the agent may draft only or submit replies for review.

It generates a job prompt and a policy document. It does not grant extra mailbox
or filesystem permissions.

### Confirmation contract

Before writing files, creating a key, or changing remote configuration, the
skill renders an exact summary containing:

- files and service definitions to create;
- the selected runtime command and sandbox;
- authorized senders and fallback action;
- existing e2a protection fields that will change;
- whether outbound review and owner CC are enabled;
- secrets that will be created and where they will be stored;
- operational limitations; and
- rollback behavior.

The user must confirm this summary. The installer never treats an earlier casual
“yes” as confirmation for later mutations.

## Installation architecture

### Local layout

The default state root is owner-readable only:

```text
~/.local/share/e2a-autopilot/<agent-id>/
  policy.json
  task.md
  secrets.env
  state/
    jobs/
      pending/
      running/
      retry/
      done/
      dead/
    locks/
  logs/
```

Policy and task files contain no API keys. `secrets.env` is mode `0600`; parent
directories are mode `0700`. Message bodies are not written to logs or terminal
output. Durable job records contain identifiers and delivery metadata, not the
message body.

### Credentials

Installation uses the operator's current account-scoped CLI session only for
confirmed setup operations. It creates a dedicated agent-scoped key for the
supervisor and stores it locally with restrictive permissions. The task runtime
never receives either credential.

If installation fails, the installer restores the prior protection document
where possible and revokes a key it created. It reports any rollback failure
prominently.

### Existing e2a configuration

The installer uses current CLI/API behavior to:

- read the current protection document;
- apply the confirmed inbound allow/review policy;
- apply review-all outbound protection when selected;
- enable notification behavior supported by the current product;
- create a dedicated agent key; and
- verify the resulting configuration.

No new fields, endpoints, or API semantics are introduced.

## Runtime architecture

### Event intake

The supervisor launches the existing command:

```text
e2a listen --forward <loopback-url> --forward-token <random-token>
```

The receiver binds only to loopback. It authenticates the forwarding request,
extracts stable message identifiers, writes a durable job atomically, and only
then returns success. It discards the forwarded body after intake. The supervisor
fetches the current message again when a worker claims the job.

Because the current CLI accepts the forward token as an argument, the token may
be visible to same-user process inspection. It protects against accidental local
posts, not a hostile same-user process. Eliminating that limitation would require
a CLI change and is outside this polish.

### Reconciliation

The supervisor periodically reconciles unread messages through the existing CLI.
This covers:

- listener reconnect gaps;
- process crashes after the upstream event but before local persistence;
- messages released from e2a review; and
- duplicate WebSocket delivery.

Jobs are keyed by stable message ID. Atomic creation and state transitions make
delivery at-least-once with idempotent local scheduling.

### Job lifecycle

```text
event/reconcile -> pending -> running -> done
                               |  |
                               |  +-> retry -> pending
                               +----> dead
```

On startup, stale `running` jobs return to `retry`. Retries use bounded
exponential backoff. A dead job triggers owner notification through the current
e2a channel when possible and always appears in local status output.

### Job gateway

Each job gets a new random local socket and capability token. The runtime receives
only the socket path, capability token, job ID, and task workspace. Supported
operations are deliberately small:

- `get_current_message`
- `get_current_thread`
- `submit_reply`
- `escalate`
- `complete`

`submit_reply` enforces thread recipients, injects owner CC when enabled, rejects
new recipients, and calls the existing e2a reply command. Raw message content is
returned only for the current job and is never logged.

### Runtime adapters

Adapters translate the common job contract into a command for:

- Claude Code;
- Codex;
- OpenClaw;
- Hermes Agent; and
- custom executables.

An adapter must declare how it disables or limits shell, filesystem, network, and
external tools. If a selected runtime cannot meet the requested isolation, the
installer must warn the user and require explicit acknowledgement. It may not
describe best-effort prompt instructions as a sandbox.

## Operations

The plugin exposes a single entrypoint with these commands:

```text
autopilot interview
autopilot plan
autopilot install
autopilot start
autopilot stop
autopilot run
autopilot status [--verify]
autopilot logs
autopilot uninstall
```

`plan` and `status` are read-only. `install` and `uninstall` display mutations
and require confirmation. `run` supports foreground debugging; `start` and
`stop` control the chosen service manager.

Logs are structured, bounded, and redact message bodies, authorization headers,
API keys, forward tokens, and gateway capabilities.

## Failure handling

- **Listener exits:** restart with bounded backoff; reconciliation continues.
- **Receiver cannot persist:** return a non-success response so the forwarder
  reports failure; reconciliation recovers the message.
- **Runtime times out or crashes:** move the job to retry without marking it
  complete.
- **Reply needs review:** record the pending-review result as a successful
  submission, then finish the job according to policy.
- **Protection verification fails:** do not start; show the mismatched fields.
- **Local permissions are broad:** refuse to start until fixed or deliberately
  acknowledged for a custom deployment.
- **Owner notification fails:** retain the dead/escalated job and surface it in
  status; never discard the failure.

## Testing strategy

Plugin-level tests cover:

- interview branching, defaults, and warned opt-outs;
- deterministic plan rendering;
- file permissions and secret redaction;
- install rollback with a fake e2a CLI;
- unauthorized-sender configuration using the existing review action;
- atomic spool writes, deduplication, recovery, and retry/dead transitions;
- job gateway authorization and cross-job denial;
- recipient locking and owner-CC insertion;
- absence of e2a credentials in runtime environment and prompts;
- reconciliation of a message released from review;
- service definition generation for launchd and systemd; and
- plugin manifest/skill validation.

No backend, migration, OpenAPI, SDK, MCP, or core CLI files should change in the
implementation diff. A release check enforces this scope mechanically.

## Known limitations accepted for this release

1. Owner CC is enforced by the local gateway, not the server.
2. Existing e2a protection configuration can be changed later by an account
   administrator without a durable revision signal to the daemon.
3. The listener forward token may be visible to processes owned by the same OS
   user.
4. Public-any-authenticated-sender support is deferred.
5. Human reviewers remain trusted and can edit an approved outbound message.

These limitations must appear in the generated plan and operator documentation;
the skill must not imply guarantees it cannot provide.
