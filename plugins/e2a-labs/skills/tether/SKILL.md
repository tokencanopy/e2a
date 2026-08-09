---
name: tether
description: Beta — Stay in the loop over email during a long-running coding session. The agent sends threaded status updates to your inbox as it sees fit and picks up your emailed replies (questions/instructions) within a few minutes, so you can steer a working agent while AFK. Transport is e2a. Use when you want to walk away from a session but keep commanding it from your inbox.
---

# tether — steer a long-running session from your inbox

> **Beta.** The transport (e2a) and the flow are real; polish (adaptive
> backoff, richer digests) is still open.

`/tether` keeps you connected to a working session over email. The agent emails
you updates **when it judges there's something worth reporting** (not on a
timer, not every turn), and checks your inbox on an interval so your replies —
questions or new instructions — are picked up within a few minutes. Reply
`stop` to end.

> **Transport primer.** tether rides on e2a. The e2a *operate-well manual* —
> the `e2a` skill (`${CLAUDE_PLUGIN_ROOT}/skills/e2a/SKILL.md`) — carries the
> mental model this skill assumes: how threading really works (reply headers +
> stable subject, **not** `conversation_id`), when a shared `agents.e2a.dev`
> address is all you need vs. a custom domain, and how to handle send statuses.
> Read it if you're new to e2a; tether does not re-explain those.

## Architecture (why this shape)

Coding agents are turn-based; nothing native listens to an inbox mid-session. So
the two directions use different mechanisms:

- **Send = agent-driven.** The agent calls `tether.sh update "…"` at meaningful
  moments (finished a slice, hit a blocker, needs a decision). Cadence is the
  model's judgment → no per-turn spam, nothing to throttle.
- **Receive = real-time wait.** The agent runs `tether.sh listen` — it blocks
  on the e2a CLI's WebSocket (`e2a listen --once`; no LLM tokens while waiting)
  for the duration set at `start --for`, so replies are picked up within
  seconds; if the WebSocket is unavailable it degrades automatically to
  interval polling. It wakes the agent *only* when a reply actually arrives (or
  the window ends). See **Durability tiers** for keeping it alive across
  idle/sleep.
- **Questions = ask by email.** When the agent needs a decision or clarification
  from you, it must **not** use a terminal prompt / AskUserQuestion — you're AFK
  and can't see it, which would stall the whole session. It calls
  `tether.sh ask "<question>"`, which emails the question into the thread and
  **blocks until you reply**, then prints your answer. This is the hard rule:
  **while tethered, every question goes over email, never the terminal.**
- **Blocked-alert = hook (optional).** A `Notification` hook emails you when the
  agent is stuck on a *permission prompt* it can't proceed past. Note: an emailed
  reply cannot answer a CLI permission prompt (there's no native way to inject
  approval), so for unattended runs **pre-authorize** the tools the session needs
  (a permission allowlist / a less-prompting mode). The hook is the safety net,
  not the approval channel.

## Setup (once)

> **Prerequisites:** Node (the transport is the `e2a` CLI, currently the **2.x**
> line pinned as `TETHER_MIN_CLI` in `lib.sh` — resolved from `$E2A_CLI`, then
> PATH, then fetched automatically via `npx -y @e2a/cli@^MIN`, so there is
> nothing to install by hand) and Python 3 (local state handling only).
>
> The range is **major-bounded on purpose**. tether parses the CLI's TSV columns
> and branches on its exit codes, and a major bump is allowed to change both —
> 2.0.0 did. So a CLI newer than the pinned major is *not* adopted automatically
> (tether falls back to npx and says so); someone re-verifies tether against the
> new major and bumps `TETHER_MIN_CLI`. Failing inert beats failing silently in
> an unattended session.

### Fast path (recommended): `tether.sh setup`

If the e2a CLI is logged in (`e2a login` in a browser, or a key persisted with
`e2a config set api_key <key>` on a headless box), one command does the whole
bootstrap:

```bash
"${CLAUDE_PLUGIN_ROOT}/skills/tether/tether.sh" setup
```

It verifies the credential (`e2a whoami`), ensures a tether inbox (reusing an
existing `tether-…` agent or creating one on the shared domain — it will
**not** silently adopt a non-tether inbox), mints a **least-privilege
agent-scoped key** (`e2a_agt_…`), and writes `~/.e2a-tether.env` (previous file
kept as `.bak`). Every step fails hard — it never stores the broad account key
and never reports "ready" with a half-working config. Flags: `--email
you@yourdomain` to use/create a specific inbox, `--new` to force a fresh one.

### Manual setup (fallback)

1. **Get an agent-scoped API key** (`e2a_agt_…` — least privilege, so a leaked
   key can't touch the rest of the account): from the CLI with
   `e2a keys create --agent <inbox>`, or from **https://e2a.dev/api-keys**.
   No domain? Create the agent on the **shared `agents.e2a.dev`** domain
   (`e2a agents create name@agents.e2a.dev` — live immediately, no DNS).
2. **Save the credentials:**
   ```bash
   cp "${CLAUDE_PLUGIN_ROOT}/skills/tether/tether.env.example" ~/.e2a-tether.env
   chmod 600 ~/.e2a-tether.env   # fill E2A_API_KEY (e2a_agt_…) + E2A_AGENT_EMAIL
   ```
3. **(Optional) blocked-alert hook:**
   ```bash
   "${CLAUDE_PLUGIN_ROOT}/skills/tether/install.sh" --to <repo-root>
   ```

Credentials resolve in order: explicit env vars → `~/.e2a-tether.env` →
`~/.e2a/config.json`. Note that `e2a login` saves an **account-scoped** key and
does **not** set `agent_email` — so `~/.e2a/config.json` alone is not enough for
tether, which needs a specific inbox. Either set both explicitly:

```bash
e2a keys create --agent <inbox>          # least-privilege e2a_agt_… key
e2a config set agent_email <inbox>       # or export E2A_AGENT_EMAIL
```

or put the agent key + inbox in `~/.e2a-tether.env` and leave the CLI config alone.

> **Two ways a send can be accepted but never delivered — both fail loudly.**
> `pending_review` means the message was held for approval; a terminal `failed`
> means the server persisted a delivery failure. `tether.sh` detects both on
> every send: an affected intro makes `start` **refuse to arm**, and an affected
> `update`/`ask` exits non-zero — held is **2** for `update`, **4** for `ask`;
> terminal failure is **5** for both — so the session never mistakes either for
> a delivered message. A terminal failure is
> **not** retryable — the server already recorded an outcome for that message
> id, so re-sending risks a duplicate. Inspect it with `e2a messages get <id>`.

## First run (new user) — set them up, then tether

Before tethering, confirm the harness can actually send. Run `"$T" status`
(where `T` is defined below). If it reports **`config: MISSING`**, this is a
first-time user — **help them get to a working setup instead of just failing**:

1. **Is e2a connected at all?** If they've never used e2a, hand them
   `e2a login` — or, when `e2a` isn't installed globally, the npx form:
   `npx -y @e2a/cli login` (same auto-fetch the harness itself uses). It opens
   the browser sign-up/sign-in and saves an account-scoped key to
   `~/.e2a/config.json`. (Headless box: they mint an account key in the
   dashboard, persist it with `e2a config set api_key <key>`, then validate it
   with `e2a whoami`.) Interactive sign-in is theirs to complete: hand them the
   command, don't drive it.
2. **Run the bootstrap:** `"$T" setup` — it creates the inbox, mints the
   agent-scoped key, and writes
   `~/.e2a-tether.env`. See **Setup** above for what it refuses to do.
3. **Re-check.** `"$T" status` should now print `config: OK (agent …)`. Proceed
   to the runtime flow.

If `status` already prints `config: OK`, skip this — they're a returning user.
Don't put a configured user through onboarding.

## Runtime flow (what the agent does when `/tether` is invoked)

Let `T="${CLAUDE_PLUGIN_ROOT}/skills/tether/tether.sh"`.

0. **Preflight.** Run `"$T" status`. If `config: MISSING`, do **First run (new
   user)** above before continuing — don't call `start` and let it error out.
1. **Ask** the user's email address **and how long to stay tethered** (e.g. 30m,
   2h, 8h/overnight, or until they say stop). They're present at this step, so a
   normal question is fine.
2. **Start**: `"$T" start <email> --title "<work>" --for <duration>` (or
   `--until <ISO>`; omit both for until-stop) — sends the intro, opens the
   thread, arms, records the window. **`--title` is required** (start refuses
   without it): a short description of the work being done (e.g. `"migrate
   loft → @e2a/ui"`, `"fix webhook retries"`) — it becomes the thread's subject
   line (`Tether: <repo> — <title>`), which is how the user tells this session
   apart from others in their inbox. The subject is fixed at start (threading
   needs it stable), so title the *work*, not the first step. `--for` takes a **single
   unit** (`30m`, `2h`, `8h`, `1d`); a compound
   like `1h30m` is rejected rather than silently treated as no-limit. If the intro
   comes back `pending_review`, `start` refuses to arm because the intro was not
   dispatched.
3. **Work**, and **send updates as you see fit** — **prefer HTML**, it renders
   far better in mail clients: write the HTML to a file and run
   `"$T" update --html <file>` — a plain-text fallback is auto-derived (or pass
   `--text "<fallback>"`). Plain `"$T" update "<text>"` is for quick one-liner
   acks only. To send a file (a rendered PDF, a screenshot, a small log), add
   `--attach <file>` — repeatable, on either form, capped at 15 MB total per
   send (**exit 3** = file not found, **exit 4** = over the cap; past the cap,
   upload the file somewhere and send a link instead). Good moments: finished a
   slice, made a decision that's worth surfacing, hit a blocker, or before a
   long unattended stretch. Skip trivial
   turns. If `update` reports `pending_review` (**exit 2**), the update did
   **not** reach the user — stop and fix the inbox configuration before
   continuing. **Exit 5** means the send reached a terminal `failed` outcome:
   also undelivered, but do **not** re-send it (the server already recorded that
   message id) — inspect it with `e2a messages get <id>` first.
4. **Need a decision from the user? Ask by email — never the terminal.** Run
   `"$T" ask "<question>"` (in the background); it emails the question and blocks
   until the user replies, then prints the answer. `--attach <file>` works here
   too — attach the artifact the decision hinges on (a diff, a mockup) rather
   than describing it. **Do not** use AskUserQuestion
   or a bare terminal prompt while tethered — an AFK user can't answer it and the
   session stalls. `ask` coordinates with `listen` automatically (it holds a lock
   so a background `listen` pauses and can't swallow your answer). Handle its exit
   codes: **exit 3** = timed out with no reply (default 30m) — re-`ask`, send a
   nudge `update`, or keep working and listening, but never fall back to a
   terminal prompt; **exit 4** = the question was held for review and not
   dispatched (fix the inbox configuration); **exit 5** = the question hit a
   terminal `failed` outcome — it never reached the user, so `ask` returns
   immediately instead of blocking for the full timeout; don't blindly re-ask.
5. **Listen for the whole window**: run `"$T" listen` **in the background**. It
   waits on the CLI's WebSocket (real-time, no tokens while waiting; degrades
   to polling if the WS is unavailable) and exits with either:
   - `REPLY_RECEIVED:` + the message → act on it (then `update` with the result),
     and **relaunch `listen`** for the remaining window; or
   - `TETHER_EXPIRED` → the window is up; run `"$T" stop`.
   Replies are deduped by message-id and survive e2a's async parse, so none are
   dropped or repeated. (`poll` is the same one-shot check if you want it manually.)
6. **Stop** when the user replies `stop`/`done`, the window expires, or the work
   is complete: `"$T" stop`.

## Writing good emails

The recipient is a **person reading email (often on a phone)**, not a terminal.
Write for that medium, not for a CLI.

**HTML (`update --html <file>` — the default; use it for any substantive update):**
- HTML renders far better than plain text in real mail clients. Reach for it
  for anything beyond a quick one-liner: a status update, a summary, a
  question with options, a diagram, a table, a before/after.
- **Mobile-first** (learned the hard way): `max-width:~480px`, **inline styles
  only** (email strips `<style>`/`<head>`), readable sizes (14–15px), and a
  **vertical/stacked layout**. Avoid wide tables and big ASCII in `<pre>` — they
  force horizontal scroll and shrink to unreadable on phones.
- Prefer real elements (stacked `<div>` boxes, small `<table>`s) over ASCII art.
- Use a system font stack; keep colors subtle. `update` auto-derives the
  plain-text fallback, so HTML sends are always safe.

**Plain text (`update "<text>"` — quick one-liner acks only):**
- Fine for a fast acknowledgement ("on it — rerunning the tests") or a
  single-sentence status. Anything with structure should be HTML.
- **No markdown** — `**bold**`, `` `code` ``, `#` headings render as literal
  characters in a plain-text email. Use plain prose.
- Note: `ask` bodies are plain-text only (no `--html`) — keep questions short
  and prose-only there. `--attach` does work on `ask`: attach the artifact the
  decision hinges on (a diff, a mockup) rather than describing it.

**Both:**
- Lead with the takeaway (what changed / what you need), then details. Keep it
  short and scannable.
- Be concrete: name the file / PR / decision ("merged #357"), not "did some work".
- If you need something, end with **one clear ask** ("Reply A or B?").
- No large code/log dumps — summarize or link. Don't paste stack traces. If the
  artifact itself matters (a rendered PDF, a screenshot, a report), send it as
  an attachment (`--attach`) instead of inlining it.
- **Acknowledge fast.** When a reply comes in, a quick "on it — doing X" beats
  silence; there's inherent email latency, so don't leave the user wondering if
  you heard them.
- **Keep it in one thread — always `update`, never a fresh send.** `tether.sh`
  threads by *replying* (In-Reply-To/References + a stable subject), which is what
  Gmail/Outlook actually stitch on. e2a's `conversation_id` is application
  correlation, and Gmail ignores it—so a fresh send with the same value still
  lands as a *second* thread in the user's inbox (the split Gmail showed).
  While tethered, send every update through `"$T" update` (it replies into the
  thread); do **not**
  reach for the e2a MCP `send_message` or start a new subject to reach the user
  mid-session. One session = one thread = one subject.

## Wait behavior & knobs

`listen`/`ask` block on the e2a CLI's WebSocket wait (**no LLM tokens while
waiting**), so reply latency is seconds. The poll interval only matters as the
degraded cadence when the WebSocket is unavailable, and as the backfill check
between waits. The agent is only woken (a real turn) when a reply actually
lands.

| env var | default | effect |
|---|---|---|
| `E2A_TETHER_POLL_INTERVAL` | `20` (s) | fallback poll cadence when the WS wait is unavailable |
| `E2A_TETHER_ASK_TIMEOUT` | `1800` (s) | how long `ask` blocks for an answer before giving up |
| `E2A_URL` | `https://e2a.dev` | e2a deployment root (set for self-host) |
| `E2A_CLI` | (auto) | override the e2a CLI invocation (e.g. `node /repo/cli/dist/bin/e2a.js`) |

The only thing that costs a turn per tick is a `/loop` **heartbeat** (tier 2
below) — keep that coarse (e.g. 30m).

## Durability tiers

1. **In-session (default):** `listen` polls for the whole `--for` window —
   automatic while the terminal stays open, and cheap (curl only, no tokens).
   This is what the duration setup buys you: one long-lived poller, not manual
   restarts. Add **`listen --awake`** to keep the machine from *idle*-sleeping
   during the window (macOS `caffeinate`, auto-released when listening ends).
   Note: `--awake` does **not** survive *closing the lid* (macOS clamshell still
   sleeps) — that's tier 3.
2. **Heartbeat (optional):** a slow `/loop` (e.g. every 30m) can relaunch
   `listen` if it dies and keep the session warm. `/loop` wakes the *agent* (a
   full turn each tick) — use it as a supervisor, not the poller.
3. **Always-on (survives a closed laptop):** nothing in-session outlives a
   closed terminal, regardless of duration — that needs an **e2a webhook firing
   a cloud Routine** (a *fresh* session per fire, loses live context). Follow-on.

## Multiple sessions

Each `start` opens a **dedicated email thread** (fresh send, fresh application
conversation ID, its own subject; replies anchor by In-Reply-To), and local
state is **keyed per repo** (git toplevel), so tethered sessions in different
repos coexist without
touching each other's thread, watermark, or ask-lock. Within one repo,
`start` **refuses to arm over a live session** instead of silently hijacking
its thread. To run a second session in the *same* repo, start it with
`--parallel`: it self-keys a fresh state file and prints a
`TETHER_STATE="…"` handle — **prefix every subsequent tether call in that
session with it** (`TETHER_STATE="…" "$T" update …`), and pass a distinct
`--title` so the inbox threads are tellable apart. Forgetting the prefix is
warned about (commands notice parallel peers exist) and every send echoes its
thread id, so misdirection is observable. (A pre-existing machine-global
`state.json` from an older tether keeps working until its session stops —
note that WHILE it exists it shadows repo keying, so it also blocks `start`
in other repos; `stop` that session to retire it.)

## Files

| file | role |
|---|---|
| `tether.sh` | runtime CLI: `setup` / `start --title [--for] [--parallel]` / `update [--html] [--attach]` / `ask [--attach]` / `listen` / `poll` / `status` / `stop` |
| `lib.sh` | config + e2a-CLI resolution (`t_cli`) + send/reply/wait helpers |
| `hooks/tether-notify.sh` | optional Notification hook (blocked-alert) |
| `install.sh` | wire/unwire the Notification hook; `_selftest` |
| `tether.env.example` | credentials template |
