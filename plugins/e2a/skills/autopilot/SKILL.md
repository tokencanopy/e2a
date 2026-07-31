---
name: autopilot
description: Set up an always-on daemon that turns an e2a inbox into a trigger for autonomous coding-agent sessions — each trusted inbound email spawns a headless run (Claude Code, Codex, or Kimi Code) that reads the message, does the work, and replies in-thread. Use when someone wants an agent to "check email periodically", "react to emails automatically", or "listen for mail and do work" unattended — distinct from tether, which keeps a live interactive session steerable from your inbox rather than spawning new sessions.
---

# autopilot — turn an inbox into an agent trigger

`/autopilot` supervises `e2a listen --forward` (the CLI's own real-time WebSocket bridge)
and, for each trusted inbound email, spawns a fresh headless coding-agent session that
reads the message, does the work, and replies in-thread — all without a human present.

> **Relationship to `tether`.** Both skills put e2a in the loop of a coding-agent workflow,
> but for opposite directions of control: `tether` (`${CLAUDE_PLUGIN_ROOT}/skills/tether/SKILL.md`)
> keeps a **live session you're already running** steerable from your inbox while you're
> AFK — same session, same context, bounded by how long you keep it open. `autopilot`
> has **no live session** to steer; it's a standing daemon that spawns a **brand-new**
> session per trusted email and lets it exit when done. Reach for tether when you're
> working and want to step away; reach for autopilot when nobody's actively working the
> repo and you want mail to keep getting handled anyway. They compose: nothing stops a
> tethered session from also being one that autopilot spawned.

> **Relationship to a cloud webhook.** e2a's `create_webhook` API is the right answer once
> there's a stable public endpoint (a cloud deployment). autopilot is the local-machine
> answer: `e2a listen` dials **out**, so nothing needs to be reachable from the internet —
> exactly why this fits a laptop. Migrate to a webhook when the setup leaves one machine.

## Architecture

```
e2a cloud ──WebSocket──▶ `e2a listen --forward` (CLI child process, supervised)
                              │ POSTs each message
                              ▼
                    local HTTP receiver (127.0.0.1 only, token-checked)
                              │ trusted sender?
                              ▼
                spawn: claude -p "…" / codex exec "…" / kimi -p --auto "…"
                (headless session: reads mail, acts, replies, exits)
```

One Node process (`runner.mjs`) owns all of it: it spawns and supervises the `e2a listen`
child, runs the local receiver, applies the trust check, queues and runs headless sessions
serially, and drives two independent liveness safety nets (below). It never calls an LLM
itself — all judgment happens inside the spawned session.

**Why this rides on the CLI instead of a hand-rolled WebSocket client:** `e2a listen
--forward <url> --forward-token <token>` already implements the full bridge — auth,
envelope parsing, and reconnect-with-backoff on transient closes (via `@e2a/sdk`'s
`client.listen()`) — mirroring the `stripe listen --forward-to` pattern. Re-implementing
that in a bespoke script duplicates logic the CLI already owns and maintains, and drifts
out of sync with it. Use the CLI; supervise the CLI.

## Prerequisites

1. **The target agent inbox already exists** (`e2a agents create <email>` or the `e2a`
   skill), on a verified domain or the shared `agents.e2a.dev`.
2. **The e2a CLI is reachable** — installed on PATH, or `lib.sh`'s `a_cli` will fall back
   to `npx -y @e2a/cli@^2` automatically. Node ≥18.
3. **The runtime CLI** (Claude Code / Codex / Kimi Code) installed, with its **real binary
   path resolved** (`readlink -f $(which claude)` etc.) — some environments alias these
   through a launcher wrapper that a background spawn shouldn't go through.
4. **launchd** (this skill ships a macOS installer; the daemon itself — `runner.mjs` — is
   plain Node and works anywhere, so adapting `autopilot.sh install` to systemd/etc. is a
   small, self-contained change if needed).

## Setup

```bash
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" setup
```

Interactive bootstrap: verifies the e2a credential, asks which existing agent inbox to
listen on, **mints a fresh least-privilege agent-scoped key just for this daemon** (never
reuses an account-scoped or another purpose's key — a leaked listener key can only touch
this one inbox's mail), generates a forward-token secret, and asks for the trust
allowlist, the CC'd human, the runtime, and the workdir. Writes `~/.e2a-autopilot.env`
(chmod 600). Review the file, then:

```bash
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" install
```

**`install` is a step a human runs deliberately** — it loads a launchd job that
auto-spawns agent sessions in reaction to inbound email from then on. Don't script this
step into another agent's autonomous flow; the trust boundary being crossed here
(always-on autonomous triggering) is exactly the kind of thing a human should knowingly
turn on, not something an agent quietly enables for itself.

For Claude Code, also set `E2A_AUTOPILOT_SETTINGS` to a sandboxed settings file — see
`headless-settings.claude.example.json` and "Sandboxing the spawned session" below.
**Strongly recommended, not optional**: without it, spawned sessions run with the account's
default (broad) tool access.

## The trust model (read this before going further)

Inbound email is untrusted input from the internet. This skill turns it into an automatic
trigger for an agent session — so the single most important property of the whole setup is
**never auto-triggering on unauthenticated or unlisted senders**. `runner.mjs`'s `isTrusted`
enforces:

- The sender must be on `E2A_AUTOPILOT_ALLOWLIST` (exact address match).
- Mail from the listening agent's own domain (agent-to-agent traffic on the same
  deployment) is trusted by allowlist membership alone.
- Mail from any other domain (e.g. a human's personal address) additionally requires
  `verifiedDomain` on the message to match the sender's domain — i.e. **DMARC passed**.
  This defeats a spoofed `From:` header merely claiming to be an allowlisted address.
- Anything that fails this check is logged (`untrusted sender, leaving for human triage`)
  and **not acted on**. It stays in the inbox, unread by the daemon, for a human to review
  normally.

Do not widen the allowlist to a whole domain or "anyone who's emailed before" without
understanding you're widening exactly this trigger surface.

> **Verified caveat — the shared `agents.e2a.dev` domain does not preserve individual
> sender identity in `headerFrom`.** Confirmed by live testing during development, and not
> currently documented elsewhere in this repo: an agent-to-agent send where the *sender*
> is on the shared domain arrives with `headerFrom` = `agent@send.e2a.dev` (a shared relay
> identity, display-named `"<sender> via e2a"`), **not** the sender's own address. The
> real sender only appears in `replyTo` — which is exactly as spoofable as `From`, i.e. not
> a trust signal at all. Practical effect: an allowlist entry naming a specific peer agent's
> own `...@agents.e2a.dev` address will **never match** mail that peer actually sends,
> because that address never appears in `headerFrom` — same-domain auto-trust silently
> never fires for shared-domain senders (a safe-but-broken failure: the mail just sits
> untriaged, it doesn't get wrongly trusted). Agents on a **custom verified domain**
> (`e2a agents create name@yourdomain.com` after `register_domain`/`verify_domain`) do not
> have this problem — their own address is what shows up in `headerFrom`, confirmed
> against real traffic. **If a peer agent's mail needs to reliably auto-trigger this skill,
> put both agents on a custom verified domain**, not the shared one.

## Sandboxing the spawned session

**Do not spawn with a full permission bypass** (e.g. Claude Code's
`--dangerously-skip-permissions`). Reasons this matters twice over: (1) an email-triggered
session should run inside a real OS sandbox regardless, since the triggering input is
untrusted even after the sender check passes — the sender being trusted doesn't make the
*content* of their email trusted; and (2) permission-bypass flags are increasingly rejected
outright for headless/automated invocations by classifier-based permission systems, so
designing around a sandboxed-settings file from the start avoids hitting that wall later.

`headless-settings.claude.example.json` is a working starting point:

- **Scopes e2a MCP access to plain inbox operations only** (`get_message`,
  `list_messages`, `reply_to_message`, `send_message`, `get_conversation`,
  `list_conversations`, `get_attachment`) — never the whole e2a MCP server if the
  session's own e2a credential is account-scoped. The full surface includes admin/approval
  tools; granting it to an autonomously-triggered session would let it release its own
  held outbound mail and defeat e2a's human-review gate entirely.
- **Enables the sandbox** with a network allowlist scoped to what the work needs, not the
  open internet.
- **Denies reads on credential paths** the session has no business touching — extend the
  `denyRead` list for the actual machine: other agents' config/credential directories,
  cloud CLI config, shell rc files (which often export API keys), and any personal-data
  folders that happen to live near the work directories on disk. The template's list is a
  floor, not a ceiling.
- **Confine writes** to `E2A_AUTOPILOT_WORKDIR`, not the whole home directory.

Codex and Kimi Code don't take a per-invocation settings file the same way — their
tool/credential scope comes from their own MCP config (`~/.codex/config.toml`,
`~/.kimi-code/mcp.json`). The equivalent discipline there is: give the persona's e2a MCP
entry an **agent-scoped** key (not account-scoped) if it's going to be driven headlessly,
for the same reason as above.

## Why two independent liveness safety nets (read before trimming either)

`runner.mjs` does two things beyond "spawn `e2a listen --forward` and restart it if it
exits":

1. **A periodic forced reconnect** (`E2A_AUTOPILOT_BOUNCE_INTERVAL_MS`, default 15 min):
   kill the `e2a listen` child and let it respawn, on a timer, regardless of apparent
   health.
2. **An independent reconcile poll** (`E2A_AUTOPILOT_RECONCILE_INTERVAL_MS`, default
   10 min): `e2a messages list --read-status unread`, fed through the same trust+intake
   path, deduped against the same seen-set.

Neither is decorative. **There is no ping/pong or idle-timeout at the SDK layer today**
(`sdks/typescript/src/v1/ws.ts` — confirmed by reading it; the WebSocket reconnects only in
response to a `close` event). A TCP connection that goes silently dead without ever firing
`close` — the textbook case is a laptop going to sleep, which freezes the process without a
clean shutdown — leaves `e2a listen` holding an object that reports itself as open, doing
nothing, indefinitely, while any process-level supervisor (launchd `KeepAlive`, systemd)
sees a perfectly healthy, non-exited process and does nothing about it either. This is not
a hypothetical: a naive version of this daemon (a hand-rolled WebSocket client with the
same "reconnect on close" logic and no watchdog) went silent for **over nine hours across a
real inbound email** in exactly this way during development.

Rather than trying to *detect* a zombied connection — which would mean re-implementing
some form of ping/pong that the SDK doesn't currently expose — the forced bounce
sidesteps the problem: restarting a perfectly healthy connection costs one brief reconnect
and is never wrong, so there's no need to distinguish "dead" from "just quiet." The
reconcile poll is a second, connection-independent backstop for the same failure class,
using a different code path entirely (REST list, not the WS stream) so a bug in one
doesn't blind the other. If you're tempted to drop either to simplify the daemon, don't —
this was a real production incident, not defensive theater.

*(If the SDK grows ping/pong or an idle-close in the future, this daemon can drop the
periodic bounce in favor of a real staleness check — track `sdks/typescript/src/v1/ws.ts`.
Until then, treat the bounce as required.)*

## Verification (do this before calling it done)

1. `autopilot.sh status` — confirm config is complete and the service reports running.
2. `autopilot.sh logs` — confirm a `receiver listening on 127.0.0.1:<port>/hook` line and
   a `Connected.` line from the `e2a listen` child within a few seconds.
3. Send a real test email from an allowlisted address to the listening agent. If the
   sender is itself an e2a agent, use one on a **custom verified domain** for this test —
   see the shared-domain caveat above; a shared-domain sender will (correctly, per that
   caveat) fail to trigger and can look like a bug in this skill when it isn't one.
4. Watch the log for `spawning session for <id>` then `session for <id> exited 0`.
5. Confirm the reply actually landed in the **same email thread** (`Re:` subject
   and reply placement) in the sender's real mailbox—not merely under the same
   application `conversation_id` or in the log.
6. **Send a test from a non-allowlisted address and confirm nothing spawns** — the log
   should show `untrusted sender, leaving for human triage` and the message should remain
   in the inbox unread by the daemon. This negative test matters as much as the positive
   one; skipping it is how a too-broad trust rule goes unnoticed.

## Files

| file | role |
|---|---|
| `runner.mjs` | the daemon: supervises `e2a listen --forward`, runs the local receiver, trust check, headless-session queue, bounce + reconcile timers |
| `autopilot.sh` | operator CLI: `setup` / `status` / `install` / `logs` / `stop` |
| `lib.sh` | config resolution + e2a CLI wrapper (mirrors `tether`'s `lib.sh`) |
| `autopilot.env.example` | credentials/config template |
| `headless-settings.claude.example.json` | sandboxed settings for spawned Claude Code sessions |

## Stopping / uninstalling

```bash
"${CLAUDE_PLUGIN_ROOT}/skills/autopilot/autopilot.sh" stop
```

Unloads the launchd job without deleting `~/.e2a-autopilot.env` or the listener key —
`install` again to resume. To fully tear down, also revoke the listener key
(`e2a keys delete <id>`, from `e2a keys list`) and delete the env file.
