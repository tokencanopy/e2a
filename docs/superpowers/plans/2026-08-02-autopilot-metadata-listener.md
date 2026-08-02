# Autopilot Metadata-Only Listener Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a backward-compatible `e2a listen` mode that forwards the authenticated WebSocket event envelope without fetching or marking the message read.

**Architecture:** Preserve the existing full-message forwarding path for every current caller. A new explicit flag selects metadata forwarding, and the listener posts the exact normalized WebSocket event envelope directly to the receiver. Delivery success remains the receiver’s HTTP 2xx response; failures retain existing exit and stderr behavior so a supervising daemon can retry safely.

**Tech Stack:** TypeScript, Node 18+, `@e2a/sdk`, the existing dependency-free CLI argument parser, Vitest.

## Global Constraints

- `--forward-metadata-only` is opt-in and valid only with `--forward <url>`.
- Metadata-only mode must never call `client.messages.get`, so it cannot mark a message read.
- Metadata-only mode forwards `email.received` and inbound `email.review_approved` wake events; it ignores outbound review events and unknown types.
- Full-message `--forward` behavior and OpenClaw-compatible output remain unchanged.
- The posted payload is the authenticated event envelope, not a hand-built message projection.
- WebSocket replacement remains terminal and exits with the CLI’s established replaced exit code; transient errors preserve established behavior.
- Forward tokens are sent only in the `Authorization: Bearer` header and never logged.
- The daemon uses `--forward-token-file <path>` so its process-lifetime forwarding token never appears in argv; existing `--forward-token` remains backward compatible for interactive callers.
- Use only synthetic events and addresses in tests.

## File Map

- Modify `cli/src/commands/listen.ts`: option, metadata envelope forwarding, validation.
- Modify `cli/src/bin/e2a.ts`: argument allowlist, option mapping, and help text.
- Modify `cli/src/__tests__/listen.test.ts`: no-fetch, wire-envelope, error, and compatibility tests.
- Modify `README.md` and/or `docs/api.md`: CLI usage and durability semantics.

---

### Task 1: Define and Test the Metadata Forwarding Contract

**Files:**
- Modify: `cli/src/commands/listen.ts`
- Modify: `cli/src/__tests__/listen.test.ts`

**Interfaces:**
- Produces: `ListenOptions.forwardMetadataOnly?: boolean` and `ListenOptions.forwardTokenFile?: string`.
- Consumes: SDK `WSEvent = { type: string; id: string; schema_version: string; created_at: string; data: unknown }`.
- Produces: `forwardNotification(event: WSEvent, forwardUrl: string, token?: string, fetchImpl?: typeof fetch): Promise<boolean>` that posts the envelope without a message GET.
- Produces: `isAutopilotWakeEvent(event: WSEvent): boolean`, true only for `email.received` or `email.review_approved` with `data.direction === "inbound"` and a string `data.message_id`.

- [ ] **Step 1: Add a failing no-fetch test**

Create a notification envelope:

```ts
const event = {
  type: "email.received",
  id: "evt_example",
  schema_version: "1",
  created_at: "2026-08-02T12:00:00Z",
  data: {
    message_id: "msg_example",
    agent_email: "support@agents.example.test",
    direction: "inbound",
  },
};
```

Run the listener loop with `forwardMetadataOnly: true`, make `messages.get` throw if invoked, and assert the POST body equals `event` byte-for-JSON-value and includes `Authorization: Bearer test-token`. Repeat with an inbound `email.review_approved` event. Assert outbound review-approved and unknown events are skipped.

- [ ] **Step 2: Add failing validation and compatibility tests**

Assert metadata-only without `forward` rejects before connecting. Assert ordinary `--forward` still calls `messages.get` once and posts its existing full-message body. Assert `--once`, `--until`, and `--json` do not change the metadata POST shape or trigger a GET; `--json` prints the event envelope. Reject `--text` or `--conversation` with metadata-only because both require message-specific fetching/filtering not present on every wake event. Assert a mode-0600 token file is read and used, a group/world-accessible token file is rejected, and `forwardToken` plus `forwardTokenFile` is a usage error.

- [ ] **Step 3: Run the focused tests**

Run: `npm test --workspace @e2a/cli -- listen.test.ts`

Expected: FAIL because the option and direct-event forwarding function do not exist.

- [ ] **Step 4: Implement the smallest separate forwarding path**

Keep `forwardMessage` unchanged. Add:

```ts
export async function forwardNotification(
  event: WSEvent,
  forwardUrl: string,
  forwardToken: string | undefined,
  fetchImpl: typeof fetch = fetch,
): Promise<boolean> {
  const headers: Record<string, string> = { "content-type": "application/json" };
  if (forwardToken) headers.authorization = `Bearer ${forwardToken}`;
  try {
    const response = await fetchImpl(forwardUrl, {
      method: "POST",
      headers,
      body: JSON.stringify(event),
    });
    if (!response.ok) {
      process.stderr.write(`Forward failed (${response.status})\n`);
      return false;
    }
    return true;
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : String(error);
    process.stderr.write(`Forward failed: ${message}\n`);
    return false;
  }
}
```

At the top of the stream loop, when metadata mode is active, evaluate `isAutopilotWakeEvent(event)`, forward the original event, optionally print the event under `--json`, honor `--once`, and `continue` before the existing `isEmailReceived`/message-rendering branch. Never construct an envelope from `EmailReceivedData`. On non-2xx, use the existing forwarding failure path and exit semantics.

- [ ] **Step 5: Run tests and commit**

Run: `npm test --workspace @e2a/cli -- listen.test.ts`

Expected: PASS.

```bash
git add cli/src/commands/listen.ts cli/src/__tests__/listen.test.ts
git commit -m "feat(cli): forward listener metadata without fetching mail"
```

### Task 2: Wire the CLI Flag and Stable Usage Errors

**Files:**
- Modify: `cli/src/bin/e2a.ts`
- Modify: `cli/src/__tests__/listen.test.ts`

**Interfaces:**
- Consumes: `ListenOptions.forwardMetadataOnly` from Task 1.
- Produces CLI flags: `--forward-metadata-only` and `--forward-token-file <path>`.

- [ ] **Step 1: Add failing CLI parsing tests**

Assert:

```text
e2a listen support@agents.example.test --forward http://127.0.0.1:8123/intake --forward-token-file /private/state/listener.token --forward-metadata-only
```

passes `{ forwardMetadataOnly: true, forwardTokenFile: "/private/state/listener.token" }` to `listen`, while the same command without `--forward` exits with a usage error containing `--forward-metadata-only requires --forward` and never opens a WebSocket. Also assert token-file and inline-token flags are mutually exclusive.

- [ ] **Step 2: Run the CLI test**

Run: `npm test --workspace @e2a/cli -- listen.test.ts`

Expected: FAIL because the CLI argument parser does not recognize the flag.

- [ ] **Step 3: Add the option and pre-connection validation**

Add `--forward-metadata-only` and `--forward-token-file` to the `listen` branch's `checkFlags` allowlist; map them with `hasFlag`/`getFlagChecked`. Add both descriptions to root help. At the first line of `listen`, before `createClient`, `requireAgentEmail`, or socket creation, validate flag combinations, `stat` the token file, reject non-regular or group/world-accessible files, and read one nonempty trimmed token. Build an internal options copy whose `forwardToken` is that resolved value and pass it to both once and long-running forwarding paths; never copy the token into logs or errors.

- [ ] **Step 4: Run tests and commit**

Run: `npm test --workspace @e2a/cli -- listen.test.ts`

Expected: PASS.

```bash
git add cli/src/bin/e2a.ts cli/src/commands/listen.ts cli/src/__tests__/listen.test.ts
git commit -m "feat(cli): expose metadata-only listener forwarding"
```

### Task 3: Pin Failure, Authorization, and Replacement Semantics

**Files:**
- Modify: `cli/src/__tests__/listen.test.ts`
- Modify if required: `cli/src/commands/listen.ts`

**Interfaces:**
- Verifies: receiver 2xx means accepted; non-2xx/network errors fail; close code 4000 `replaced` remains terminal.

- [ ] **Step 1: Add failure-mode tests**

Cover HTTP 503, aborted connection, invalid forward URL, and bearer token redaction. Assert no token appears in stderr. Verify an event is not rendered as successfully forwarded on any failure.

- [ ] **Step 2: Add replacement regression coverage in metadata mode**

Reuse the existing `E2AConnectionReplacedError` fixture while metadata mode is enabled. Assert the listener prints the existing replacement guidance once, stops reconnecting, and preserves the established replacement exit code.

- [ ] **Step 3: Run the test file**

Run: `npm test --workspace @e2a/cli -- listen.test.ts`

Expected: either PASS immediately or FAIL only where the new branch bypasses existing redaction/exit handling.

- [ ] **Step 4: Route failures through existing helpers**

If the new branch failed Step 3, reuse the same stderr sanitizer, `process.exitCode`, and replacement catch used by `forwardMessage`/`listen`; do not introduce a second exit-code table.

- [ ] **Step 5: Run and commit**

Run: `npm test --workspace @e2a/cli -- listen.test.ts`

Expected: PASS.

```bash
git add cli/src/commands/listen.ts cli/src/__tests__/listen.test.ts
git commit -m "test(cli): pin metadata listener failure semantics"
```

### Task 4: Document and Run the CLI Release Gate

**Files:**
- Modify: `README.md`
- Modify: `docs/api.md`

**Interfaces:**
- Documents the command consumed by the autopilot daemon:
  `e2a listen <agent> --forward <loopback-url> --forward-token-file <owner-only-path> --forward-metadata-only`.

- [ ] **Step 1: Document the mode and acknowledgement boundary**

Explain that metadata mode posts the event without fetching the message, does not mark it read, and considers only an HTTP 2xx a successful handoff. State that the receiver must persist before returning 2xx and should reconcile unread mail after failures.

- [ ] **Step 2: Build and run the entire CLI suite**

Run: `npm run build --workspace @e2a/sdk && npm run build --workspace @e2a/cli && npm test --workspace @e2a/cli`

Expected: PASS, including all existing generic forwarding and OpenClaw tests.

- [ ] **Step 3: Run repository checks**

Run: `git diff --check && make test-unit`

Expected: PASS; no Go behavior changed.

- [ ] **Step 4: Commit**

```bash
git add README.md docs/api.md
git commit -m "docs: explain metadata-only listener forwarding"
```
