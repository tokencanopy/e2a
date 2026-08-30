#!/usr/bin/env bash
# tether.sh — the runtime CLI the agent calls to stay in touch over email.
#
#   tether.sh setup [--email <addr>] [--new]   zero-to-tethered bootstrap (needs an account key)
#   tether.sh start <email> --title "<work>" [--for 2h|8h|30m|1d] [--until <ISO>]  open + arm
#   tether.sh update "<message>"   send a threaded update ("as you see fit")
#   tether.sh update --html <file> send an HTML update (+ auto text fallback)
#   tether.sh update --attach <f>  attach a file (repeatable; 15 MB total cap)
#   tether.sh ask "<question>" [--attach <f>]...  email a question and BLOCK until the reply
#   tether.sh listen [--awake]     wait until a reply OR the window ends (bg it)
#   tether.sh poll                 print any new replies since last poll (exit 0)
#   tether.sh status               show tether state
#   tether.sh stop                 disarm and clear state
#   tether.sh _selftest            unconfigured dry-run + syntax check
#
# Transport is the e2a CLI (see lib.sh t_cli — $E2A_CLI / PATH / npx).
# Sending is agent-driven: call `update` when there's something worth reporting.
# Receiving is real-time: `listen`/`ask` wait on the CLI's WebSocket
# (`e2a listen --once`) and fall back to interval polling if the WS misbehaves.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${here}/lib.sh"

cmd="${1:-}"; shift || true

need_config() { t_load_config || { echo "tether: not configured (need E2A_API_KEY + E2A_AGENT_EMAIL; see tether.env.example)"; exit 1; }; }
need_armed()  { [ "$(t_state_get armed)" = "1" ] || { echo "tether: not started — run 'tether.sh start <email> --title \"<work>\"' first"; exit 1; }; }

# Warn a prefix-less call when this repo has parallel sessions: without the
# TETHER_STATE handle it resolves to the PRIMARY session's state, and a
# misdirected update would post into the wrong thread reporting success.
warn_peers() {
  if [ -z "${TETHER_STATE:-}" ] && [ "$(t_state_get parallel_peers)" = "1" ]; then
    echo "tether: NOTE parallel sessions exist in this repo — this call uses the PRIMARY session's state; prefix TETHER_STATE=\"…\" if you meant a parallel session" >&2
  fi
}

case "$cmd" in
  start)
    # start <email> [--title "<work>"] [--for 30m|2h|8h|1d] [--until <ISO>]
    need_config
    to=""; forarg=""; untilarg=""; title=""; parallel=0
    # `shift 2` with only the flag left is a NO-OP in bash (returns 1, shifts
    # nothing) — without the `|| exit` guard a trailing valueless flag spins
    # this loop forever, silently hanging an unattended session.
    while [ $# -gt 0 ]; do
      case "$1" in
        --title) title="${2:-}"; shift 2 || { echo "tether: --title requires a value"; exit 2; };;
        --for) forarg="${2:-}"; shift 2 || { echo "tether: --for requires a value"; exit 2; };;
        --until) untilarg="${2:-}"; shift 2 || { echo "tether: --until requires a value"; exit 2; };;
        --parallel) parallel=1; shift;;
        *) to="$1"; shift;;
      esac
    done
    [ -n "$to" ] || { echo "usage: tether.sh start <your-email> --title \"<work>\" [--for 2h|8h|30m|1d] [--until <ISO>] [--parallel]"; exit 2; }
    # The subject is the thread's PERMANENT identity (threading needs it
    # stable), so a meaningful title is required, not suggested — a bare
    # "Tether: <repo>" makes every session in this repo indistinguishable.
    if [ -z "$title" ]; then
      echo "tether: --title is required — it becomes the thread's subject (\"Tether: ${proj:-$(basename "$PWD")} — <title>\")."
      echo "        Title the WORK, not the first step (e.g. --title \"fix webhook retries\")."
      exit 2
    fi
    # A deliberate parallel session self-keys BEFORE the armed check — it never
    # competes for the default state file at all ("--parallel = my own state
    # file", whether or not a primary is armed yet). The session MUST prefix
    # every subsequent tether call with the printed TETHER_STATE.
    if [ "$parallel" = "1" ] && [ -z "${TETHER_STATE:-}" ]; then
      default_state="$(t_state_path)"
      export TETHER_STATE="$HOME/.e2a-tether/state-$(t_state_key)-$(date +%s)-$$.json"
      T_STATE_PATH="$TETHER_STATE"
      echo "tether: parallel session — prefix EVERY subsequent tether.sh call with:"
      echo "        TETHER_STATE=\"${TETHER_STATE}\""
      # Mark the primary's state (subshell: the env prefix must not leak into
      # OUR exported TETHER_STATE) so its prefix-less commands can warn about
      # the forgotten-handle trap.
      ( TETHER_STATE="$default_state"; T_STATE_PATH=""; t_state_set parallel_peers 1 ) 2>/dev/null || true
    fi
    # Never arm over a live session: that would hijack its thread pointer and
    # watermark (each session must keep its own dedicated email thread).
    if [ "$(t_state_get armed)" = "1" ]; then
      echo "tether: already armed — thread $(t_state_get conversation_id) → $(t_state_get to)."
      if [ "$(t_state_path)" = "$HOME/.e2a-tether/state.json" ]; then
        echo "        (state is the legacy machine-global file — the live session may be in ANOTHER repo)"
      fi
      echo "        Run 'tether.sh stop' to end it first, or re-run start with --parallel"
      echo "        to open a second session (you'll get a TETHER_STATE handle to carry"
      echo "        on every subsequent call)."
      exit 1
    fi
    expires=""
    if [ -n "$untilarg" ]; then
      # Validate like --for: an unparseable expiry reads as the no-expiry
      # sentinel downstream, silently turning the asked-for window UNBOUNDED.
      expires="$(t_parse_until "$untilarg")"
      [ "$expires" = "INVALID" ] && { echo "tether: can't parse --until '$untilarg' — use RFC3339, e.g. 2026-07-02T18:00:00Z"; exit 2; }
    elif [ -n "$forarg" ]; then
      expires="$(t_duration_to_expiry "$forarg")"
      [ "$expires" = "INVALID" ] && { echo "tether: can't parse --for '$forarg' — use a single unit (30m, 2h, 8h, 1d). Omit --for for no time limit."; exit 2; }
    fi
    window="${forarg:-${untilarg:-until you say stop}}"
    proj="$(basename "$PWD")"
    conv="tether-$(date +%s)-$$"
    # Subject = the thread's one stable title (threading replies onto it), so
    # make it say what the session is DOING, not just where it's running.
    subject="Tether: ${proj} — ${title}"
    intro="🪢 Tethered — ${proj}${title:+ — ${title}}

This session is now tethered (${window}). I'll send updates to this thread as I
make meaningful progress, and I'll pick up your replies. Reply any time with a
question or instruction; reply \"stop\" to end early.

— your coding agent"
    mid="$(t_api_send "$to" "$subject" "$intro" "$conv")"; rc=$?
    if [ "$rc" = "4" ]; then
      echo "tether: intro NOT sent — auth failed (key invalid/revoked). Check 'e2a whoami' / E2A_API_KEY."
      exit 4
    fi
    if [ "$rc" = "5" ]; then
      # Terminal server-side outcome. The message id IS populated (the send was
      # persisted), so this must be checked before the empty-id test below —
      # otherwise it reads as a success and arms a session whose intro never
      # reached the user.
      echo "tether: intro reached a terminal FAILED outcome (${mid}) — it was NOT delivered, so NOT arming."
      echo "        Do not retry blindly; inspect it: e2a messages get ${mid}"
      exit 5
    fi
    [ -n "$mid" ] || { echo "tether: intro send failed (check credentials and base URL)"; exit 1; }
    if [ "$rc" = "2" ]; then
      echo "tether: intro returned pending_review — it was not dispatched, so NOT arming."
      echo "        Check the inbox configuration, then run start again."
      exit 1
    fi
    t_state_set armed 1 to "$to" conversation_id "$conv" last_message_id "$mid" \
      intro_id "$mid" \
      last_poll "$(t_now_iso)" project "$proj" started_at "$(t_now_iso)" expires_at "$expires"
    echo "tether: started — thread ${conv} → ${to} (intro ${mid}); window: ${window}${expires:+ (until ${expires})}"
    ;;

  update)
    # update "<text>"                       plain-text update
    # update --html <file> [--text "<t>"]   HTML update (+ optional text fallback)
    # either form: [--attach <file>]...     attach files (repeatable)
    need_config; need_armed; warn_peers
    htmlfile=""; textarg=""; msg=""; attach=()
    while [ $# -gt 0 ]; do
      case "$1" in
        --html) htmlfile="${2:-}"; shift 2 || { echo "tether: --html requires a value"; exit 2; };;
        --text) textarg="${2:-}"; shift 2 || { echo "tether: --text requires a value"; exit 2; };;
        --attach) attach+=("${2:-}"); shift 2 || { echo "tether: --attach requires a value"; exit 2; };;
        *) msg="$1"; shift;;
      esac
    done
    if [ -n "$htmlfile" ]; then
      [ -f "$htmlfile" ] || { echo "tether: --html file not found: $htmlfile"; exit 2; }
      # plain-text fallback: explicit --text/positional wins; otherwise the CLI
      # derives one from the HTML file itself (no --body passed).
      [ -n "$msg" ] || msg="$textarg"
    fi
    { [ -n "$msg" ] || [ -n "$htmlfile" ]; } || { echo "usage: tether.sh update \"<text>\"  |  update --html <file> [--text \"<fallback>\"]  (either form: [--attach <file>]...)"; exit 2; }
    if [ "${#attach[@]}" -gt 0 ]; then t_attach_check "${attach[@]}" || exit $?; fi
    mid="$(t_reply_anchored "$msg" "$htmlfile" ${attach[@]+"${attach[@]}"})"; rc=$?
    if [ "$rc" = "4" ]; then
      echo "tether: update NOT sent — auth failed (key invalid/revoked). Check 'e2a whoami' / E2A_API_KEY."
      exit 4
    fi
    if [ "$rc" = "5" ]; then
      # Checked BEFORE the empty-id test and before last_message_id is advanced:
      # the CLI prints the id of a terminally-failed send, so the old code fell
      # through to the success path and reported "update sent" for a message the
      # user never received.
      echo "tether: update reached a terminal FAILED outcome (${mid}) — it did NOT reach the user."
      echo "        Do not retry blindly; inspect it: e2a messages get ${mid}"
      exit 5
    fi
    if [ -z "$mid" ]; then
      echo "tether: update send failed (if anchors are exhausted, 'tether.sh stop' and start a fresh session)"
      exit 1
    fi
    t_state_set last_message_id "$mid"
    if [ "$rc" = "2" ]; then
      echo "tether: WARNING update returned pending_review — it did NOT reach the user. Check the inbox configuration."
      exit 2
    fi
    # The thread id in the success line makes cross-session misdirection
    # OBSERVABLE (a --parallel session that forgot its TETHER_STATE prefix).
    echo "tether: update sent (${mid}) [thread $(t_state_get conversation_id)]$([ -n "$htmlfile" ] && echo ' [html]')$([ "${#attach[@]}" -gt 0 ] && echo " [${#attach[@]} attachment(s)]")"
    ;;

  poll)
    need_config; need_armed
    t_poll_once
    ;;

  listen)
    # Deadline-bounded waiter. Waits until a reply (prints REPLY_RECEIVED +
    # exits) or until the window (expires_at) ends (prints TETHER_EXPIRED +
    # exits). Run it in the BACKGROUND: on a reply-exit, act then relaunch for
    # the remaining window; on TETHER_EXPIRED, run `stop`. Real-time: waits on
    # the CLI's WebSocket (t_ws_wait) as the wake signal, then consumes via the
    # dedup-safe poll — so pickup latency is seconds, not the poll interval,
    # while the seen-set/parse-retry invariants stay intact.
    #   --awake  keep the machine from IDLE-sleeping while listening (macOS
    #            caffeinate; released when listen exits). Does NOT survive the
    #            lid closing.
    need_config; need_armed; warn_peers
    awake=0
    while [ $# -gt 0 ]; do case "$1" in --awake) awake=1; shift;; *) shift;; esac; done
    if [ "$awake" = "1" ]; then
      if command -v caffeinate >/dev/null 2>&1; then
        caffeinate -i -w "$$" >/dev/null 2>&1 &   # dies with this listen process
        echo "tether: keeping the machine awake (caffeinate, idle-sleep off) while listening" >&2
      else
        echo "tether: --awake unsupported here (no caffeinate); machine may idle-sleep" >&2
      fi
    fi
    interval="${E2A_TETHER_POLL_INTERVAL:-20}"
    while :; do
      # `stop` (this session's or a takeover) may have cleared the state while
      # we waited — a disarmed listen must END, never fall through to polling
      # a cleared state (whose empty conversation would read as "no filter").
      [ "$(t_state_get armed)" = "1" ] || { echo "TETHER_EXPIRED"; exit 0; }
      rem="$(t_remaining_seconds)"
      if [ "$rem" -le 0 ]; then echo "TETHER_EXPIRED"; exit 0; fi
      # An `ask` is blocking on the same inbox — don't consume, or we'd eat the
      # answer it's waiting for. Idle until the ask releases the lock.
      if t_ask_active; then
        s="$interval"; [ "$rem" -lt "$s" ] && s="$rem"; sleep "$s"; continue
      fi
      # Poll FIRST (catches anything that arrived while we weren't waiting),
      # then block on the WS wake signal for one poll-interval chunk. Chunking
      # at the interval means: WS healthy → replies wake us in seconds; WS
      # unavailable (e.g. deployment without the endpoint — the CLI retries
      # inside the window and exits 6) → cadence degrades to exactly the old
      # 20s polling, never worse. Expiry and the ask-lock are re-checked
      # between chunks.
      out="$(t_poll_once)" || out=""
      # Empty out = a transient failure, NOT a reply — without this guard it
      # exits REPLY_RECEIVED with nothing, killing the listen.
      if [ -n "$out" ] && [ "$out" != "(no new replies)" ]; then
        echo "REPLY_RECEIVED:"; echo "$out"; exit 0
      fi
      w="$interval"; [ "$rem" -lt "$w" ] && w="$rem"
      t_ws_wait "$w"
    done
    ;;

  ask)
    # Email a question into the thread and BLOCK until the user replies, then
    # print the answer. This is how a tethered agent asks the user anything —
    # over email, never a terminal prompt the AFK user can't see. Run it in the
    # background and wait for the completion notification.
    need_config; need_armed; warn_peers
    q=""; attach=()
    while [ $# -gt 0 ]; do
      case "$1" in
        --attach) attach+=("${2:-}"); shift 2 || { echo "tether: --attach requires a value"; exit 2; };;
        *) q="$1"; shift;;
      esac
    done
    [ -n "$q" ] || { echo "usage: tether.sh ask \"<question>\" [--attach <file>]..."; exit 2; }
    if [ "${#attach[@]}" -gt 0 ]; then t_attach_check "${attach[@]}" || exit $?; fi
    # Hold the ask lock for this whole invocation so a background `listen` pauses
    # and doesn't steal the answer; released on any exit (reply, timeout, error).
    trap 't_ask_end' EXIT INT TERM
    t_ask_begin
    mid="$(t_reply_anchored "❓ ${q}

(Reply to this email with your answer — I'll wait for it.)" "" ${attach[@]+"${attach[@]}"})"; rc=$?
    if [ "$rc" = "4" ]; then
      echo "tether: ask NOT sent — auth failed (key invalid/revoked). Check 'e2a whoami' / E2A_API_KEY."
      exit 1
    fi
    if [ "$rc" = "5" ]; then
      # A terminally-failed question must never fall through to the wait loop:
      # the user never got it, so blocking would burn the full ask timeout
      # waiting for an answer to a question that was never asked.
      echo "tether: question reached a terminal FAILED outcome (${mid}) — it did NOT reach the user, so not waiting."
      echo "        Do not retry blindly; inspect it: e2a messages get ${mid}"
      exit 5
    fi
    [ -n "$mid" ] || { echo "tether: ask send failed"; exit 1; }
    t_state_set last_message_id "$mid"
    if [ "$rc" = "2" ]; then
      echo "tether: WARNING question returned pending_review — it did NOT reach the user, so not waiting. Check the inbox configuration."
      exit 4
    fi
    echo "tether: question sent (${mid}); waiting for your reply…"
    max="${E2A_TETHER_ASK_TIMEOUT:-1800}"; interval="${E2A_TETHER_POLL_INTERVAL:-20}"; start=$SECONDS
    while [ $((SECONDS - start)) -lt "$max" ]; do
      # Session stopped while we waited → the question is moot; end cleanly.
      [ "$(t_state_get armed)" = "1" ] || { echo "tether: session stopped while waiting for the answer"; exit 3; }
      out="$(t_poll_once)" || out=""
      # Empty out = transient poll failure, not an answer (same guard as listen).
      if [ -n "$out" ] && [ "$out" != "(no new replies)" ]; then echo "$out"; exit 0; fi
      # Real-time: block on the WS wake signal in poll-interval chunks (same
      # degradation logic as listen), then re-poll.
      rem2=$((max - (SECONDS - start))); w="$interval"; [ "$rem2" -lt "$w" ] && w="$rem2"
      [ "$w" -gt 0 ] && t_ws_wait "$w"
    done
    echo "tether: ask timed out after ${max}s with no answer"; exit 3
    ;;

  setup)
    # Zero-to-tethered bootstrap on the e2a CLI. Needs an ACCOUNT credential in
    # the CLI's own config (`e2a login`, or `e2a config set api_key` headless).
    # Golden path: whoami → ensure inbox (verify/create) → mint a
    # least-privilege agent key → write ~/.e2a-tether.env. Every step fails
    # HARD: no silent account-key fallback and no "ready" with a broken config.
    #   --email <addr>  use/create this inbox (bare names expand on the shared domain)
    #   --new           always create a fresh tether-<rand> inbox
    force_new=0; want=""
    while [ $# -gt 0 ]; do case "$1" in --new) force_new=1; shift;; --email) want="${2:-}"; shift 2 || { echo "tether: --email requires a value"; exit 2; };; *) shift;; esac; done
    # Setup deliberately uses the CLI's own stored credential, not whatever a
    # previous tether run left in the environment.
    unset E2A_API_KEY E2A_AGENT_EMAIL
    # Keep the probe's stderr: "npm can't find the CLI" and "not logged in"
    # need OPPOSITE fixes, and swallowing the error misdiagnosed both as a
    # credential problem.
    errf="$(mktemp "${TMPDIR:-/tmp}/tether-setup-err.XXXXXX")"
    if ! who="$(t_cli whoami --json 2>"$errf")"; then
      echo "tether setup: the e2a CLI is unavailable or not logged in:"
      sed 's/^/        /' "$errf" | tail -5
      rm -f "$errf"
      echo "        Fix: run 'e2a login' (browser), or persist an account key with 'e2a config set api_key <key>' (headless), then re-run setup."
      exit 1
    fi
    rm -f "$errf"
    scope="$(printf '%s' "$who" | python3 -c 'import json,sys
try:print(json.load(sys.stdin).get("scope",""))
except Exception:print("")')"
    if [ "$scope" = "agent" ]; then
      # `agentEmail`, not the pre-2.0.0 `agentAddress`: CLI 2.0.0 renamed
      # AccountView.agent_address → agent_email as part of the agent-reference
      # unification. Reading the old name silently yielded an empty string, so
      # this line reported "already holds an agent-scoped key ()".
      bound="$(printf '%s' "$who" | python3 -c 'import json,sys
try:print(json.load(sys.stdin).get("agentEmail",""))
except Exception:print("")')"
      echo "tether setup: the CLI already holds an agent-scoped key (${bound}) — nothing to mint."
      echo "              tether will pick it up from ~/.e2a/config.json; run 'tether.sh status' to confirm."
      exit 0
    fi
    [ "$scope" = "account" ] || { echo "tether setup: could not determine key scope (e2a whoami failed?) — check 'e2a whoami'"; exit 1; }
    inbox="$want"
    if [ -z "$inbox" ] && [ "$force_new" = "0" ]; then
      # Reuse ONLY tether-owned inboxes; never silently adopt (and reconfigure)
      # someone's production agent just because it's the account's only one.
      inbox="$(t_cli agents list 2>/dev/null | awk -F'\t' '$1 ~ /^tether-/ {print $1; exit}')"
      [ -n "$inbox" ] && echo "tether setup: reusing ${inbox}"
    fi
    if [ -n "$inbox" ] && [ "${inbox#*@}" = "$inbox" ]; then
      sd="$(t_cli config get shared_domain 2>/dev/null)"
      [ -n "$sd" ] || { echo "tether setup: can't expand bare name '${inbox}' — no shared domain known; pass a full address"; exit 1; }
      inbox="${inbox}@${sd}"
    fi
    if [ -z "$inbox" ]; then
      sd="$(t_cli config get shared_domain 2>/dev/null)"
      [ -n "$sd" ] || { echo "tether setup: no shared domain on this deployment — pass --email you@yourdomain"; exit 1; }
      inbox="tether-$(python3 -c 'import secrets;print(secrets.token_hex(3))')@${sd}"
    fi
    if ! t_cli agents get "$inbox" >/dev/null 2>&1; then
      echo "tether setup: creating ${inbox}…"
      t_cli agents create "$inbox" --name "tether" >/dev/null || { echo "tether setup: agent create failed (slug taken/invalid?)"; exit 1; }
    fi
    kjson="$(t_cli keys create --agent "$inbox" --name "tether-$(python3 -c 'import secrets;print(secrets.token_hex(2))')" --json 2>/dev/null)"
    agtkey="$(printf '%s' "$kjson" | python3 -c 'import json,sys
try:print(json.load(sys.stdin).get("key","") or "")
except Exception:print("")')"
    [ -n "$agtkey" ] || { echo "tether setup: could not mint an agent-scoped key — aborting (NOT storing the broad account key)"; exit 1; }
    envf="${HOME}/.e2a-tether.env"
    if [ -f "$envf" ]; then
      cp "$envf" "${envf}.bak" && chmod 600 "${envf}.bak" 2>/dev/null
    fi
    # umask 077 closes the window where the file briefly exists with default
    # permissions before a post-hoc chmod — it holds a plaintext key.
    ( umask 077
      { echo "# written by tether.sh setup — $(t_now_iso)"
        echo "export E2A_API_KEY=\"${agtkey}\""
        echo "export E2A_AGENT_EMAIL=\"${inbox}\""
        [ -n "${E2A_URL:-}" ] && echo "export E2A_URL=\"${E2A_URL}\""
      } > "$envf"
    )
    chmod 600 "$envf" 2>/dev/null || true
    echo "tether setup: wrote ${envf} (agent-scoped key, least privilege)$([ -f "${envf}.bak" ] && echo " — previous file kept as .bak (its key stays valid; revoke unwanted keys via 'e2a keys list' + 'e2a keys delete')")"
    export E2A_API_KEY="$agtkey" E2A_AGENT_EMAIL="$inbox"
    bash "${here}/tether.sh" status
    echo "tether setup: ready → tether.sh start <your-email> --title \"<work>\" --for 30m"
    ;;

  status)
    if t_load_config; then echo "config: OK (agent ${E2A_AGENT_EMAIL}, base ${E2A_URL:-"(unset — e2a CLI default)"})"; else echo "config: MISSING"; fi
    echo "cli:    $(t_cli_desc)"
    if [ "$(t_state_get armed)" = "1" ]; then
      echo "armed:  yes"
      echo "thread: $(t_state_get conversation_id) → $(t_state_get to)"
      echo "since:  $(t_state_get last_poll)"
      exp="$(t_state_get expires_at)"
      if [ -n "$exp" ]; then
        rem="$(t_remaining_seconds)"
        if [ "$rem" -gt 0 ]; then echo "window: until ${exp} (${rem}s left)"; else echo "window: EXPIRED (${exp})"; fi
      else
        echo "window: until you stop"
      fi
    else
      echo "armed:  no"
    fi
    ;;

  stop)
    if [ "$(t_state_get armed)" = "1" ] && t_load_config; then
      t_reply_anchored "Tether ended. Session is no longer listening." >/dev/null 2>&1 || true
    fi
    t_state_clear
    echo "tether: stopped"
    ;;

  _selftest)
    echo "# unconfigured status (must not crash):"
    env -u E2A_API_KEY -u E2A_AGENT_EMAIL HOME=/nonexistent TETHER_STATE=/tmp/tether-selftest.json \
      bash "${here}/tether.sh" status
    rm -f /tmp/tether-selftest.json

    fail=0
    ck() { if [ "$2" = "$3" ]; then echo "ok: $1"; else echo "FAIL: $1 (want [$3] got [$2])"; fail=1; fi; }

    echo "# duration parser:"
    ck "2h parses"        "$([ -n "$(t_duration_to_expiry 2h)" ] && echo good)" "good"
    ck "'' → until-stop"  "$(t_duration_to_expiry '')"     ""
    ck "1h30m → INVALID"  "$(t_duration_to_expiry '1h30m')" "INVALID"
    ck "'90 min'→INVALID" "$(t_duration_to_expiry '90 min')" "INVALID"
    ck "naive --until rejected" "$(t_parse_until '2026-08-08T18:00:00')" "INVALID"
    ck "Z --until normalized" "$(t_parse_until '2026-08-08T18:00:00Z')" "2026-08-08T18:00:00Z"
    ck "offset --until normalized" "$(t_parse_until '2026-08-08T11:00:00-07:00')" "2026-08-08T18:00:00Z"
    badf=/tmp/tether-selftest-bad-expiry.json
    printf '{"expires_at":"not-a-date"}' > "$badf"
    badrem="$(TETHER_STATE="$badf" t_remaining_seconds)"
    ck "malformed stored expiry is expired" "$badrem" "-1"
    rm -f "$badf"
    legacyf=/tmp/tether-selftest-legacy-expiry.json
    printf '{"expires_at":"2999-08-08T18:00:00"}' > "$legacyf"
    legacyrem="$(TETHER_STATE="$legacyf" t_remaining_seconds)"
    ck "legacy naive stored expiry remains active" "$([ "$legacyrem" -gt 0 ] && echo yes)" "yes"
    legacyexp="$(TETHER_STATE="$legacyf" t_state_get expires_at)"
    ck "legacy naive stored expiry is normalized" "$([ "${legacyexp%Z}" != "$legacyexp" ] && echo yes)" "yes"
    rm -f "$legacyf" "${legacyf}.lock"
    corruptf=/tmp/tether-selftest-corrupt-state.json
    printf '{not-json' > "$corruptf"
    corruptrem="$(TETHER_STATE="$corruptf" t_remaining_seconds)"
    ck "corrupt stored state is expired" "$corruptrem" "-1"
    rm -f "$corruptf"

    echo "# placeholder creds are treated as MISSING:"
    ph="$(env E2A_API_KEY='e2a_agt_...' E2A_AGENT_EMAIL='tether@you.example' \
      HOME=/nonexistent bash "${here}/tether.sh" status | grep -c 'config: MISSING')"
    ck "unfilled template → MISSING" "$ph" "1"

    echo "# ask/listen mutex:"
    ( export TETHER_STATE=/tmp/tether-selftest-lock.json
      t_ask_active && { echo "FAIL: lock present before begin"; exit 1; }
      t_ask_begin;  t_ask_active || { echo "FAIL: lock absent after begin"; exit 1; }
      t_ask_end;   ! t_ask_active || { echo "FAIL: lock present after end"; exit 1; }
      echo "ok: ask lock begin/active/end"
      # A lock whose PID is dead (SIGKILLed ask) must be treated stale and
      # reaped, or a background listen pauses forever.
      echo 99999999 > "$(t_ask_lock_path)"
      if t_ask_active; then echo "FAIL: stale lock (dead pid) treated as active"; exit 1; fi
      [ -f "$(t_ask_lock_path)" ] && { echo "FAIL: stale lock not reaped"; exit 1; }
      echo "ok: stale ask lock (dead pid) is reaped" ) || fail=1
    rm -f /tmp/tether-selftest-lock.json /tmp/tether-selftest-lock.ask.lock /tmp/ask.lock

    echo "# session isolation:"
    k1="$(cd /tmp && TETHER_STATE= bash -c '. "'"${here}"'/lib.sh"; t_state_key')"
    k2="$(cd "$HOME" && TETHER_STATE= bash -c '. "'"${here}"'/lib.sh"; t_state_key')"
    ck "different dirs → different state keys" "$([ -n "$k1" ] && [ -n "$k2" ] && [ "$k1" != "$k2" ] && echo distinct)" "distinct"
    armf=/tmp/tether-selftest-armed.json
    printf '{"armed":"1","conversation_id":"tether-old","to":"a@b.c"}' > "$armf"
    out="$(env E2A_API_KEY='e2a_agt_selftest123' E2A_AGENT_EMAIL='selftest@agents.localhost' \
      TETHER_STATE="$armf" bash "${here}/tether.sh" start someone@example.com --title selftest 2>&1)"; rc=$?
    ck "start refuses to arm over a live session" "$rc:$(printf '%s' "$out" | grep -c 'already armed')" "1:1"
    out="$(env E2A_API_KEY='e2a_agt_selftest123' E2A_AGENT_EMAIL='selftest@agents.localhost' \
      TETHER_STATE=/tmp/tether-selftest-notitle.json bash "${here}/tether.sh" start someone@example.com 2>&1)"; rc=$?
    ck "start without --title is a usage error" "$rc:$(printf '%s' "$out" | grep -c 'title is required')" "2:1"
    rm -f /tmp/tether-selftest-notitle.json
    # A trailing valueless flag must be a loud usage error, not an infinite
    # option-parse loop (bash `shift 2` with $#=1 is a no-op).
    out="$(env E2A_API_KEY='e2a_agt_selftest123' E2A_AGENT_EMAIL='selftest@agents.localhost' \
      TETHER_STATE=/tmp/tether-selftest-shift.json bash "${here}/tether.sh" start someone@example.com --title 2>&1)"; rc=$?
    ck "trailing valueless flag → usage error (no hang)" "$rc:$(printf '%s' "$out" | grep -c 'requires a value')" "2:1"
    rm -f /tmp/tether-selftest-shift.json
    # --parallel: with the DEFAULT path armed (legacy file in a sandbox HOME),
    # start must branch to a fresh self-keyed TETHER_STATE and print the
    # handle. E2A_CLI=/bin/false makes the intro send fail afterwards — the
    # assertion is that the parallel branch ran, not that mail went out.
    sb=/tmp/tether-selftest-home; mkdir -p "$sb/.e2a-tether"
    printf '{"armed":"1","conversation_id":"tether-old","to":"a@b.c"}' > "$sb/.e2a-tether/state.json"
    out="$(env HOME="$sb" E2A_CLI=/bin/false E2A_API_KEY='e2a_agt_selftest123' \
      E2A_AGENT_EMAIL='selftest@agents.localhost' \
      bash "${here}/tether.sh" start someone@example.com --title selftest --parallel 2>&1)" || true
    ck "start --parallel self-keys a fresh TETHER_STATE" \
      "$(printf '%s' "$out" | grep -c 'TETHER_STATE=')" "1"
    rm -rf "$sb" "$armf"

    echo "# CLI resolution:"
    ck "version compare: 2.0.2 >= 2.0.0" "$(t_ver_ge "e2a 2.0.2" "2.0.0" && echo yes)" "yes"
    ck "version compare: 1.6.0 <  2.0.0" "$(t_ver_ge "e2a 1.6.0" "2.0.0" || echo no)" "no"
    ck "major parse" "$(t_ver_major "e2a 2.0.1")" "2"
    # The floor must track the CLI major tether was actually verified against.
    ck "min CLI is 2.x" "$(t_ver_major "$TETHER_MIN_CLI")" "2"
    # Supported range is major-BOUNDED at both ends: the pre-2.0.0 CLI is too
    # old, and an unverified next major is too new (it may rename the TSV
    # columns / whoami fields again, exactly as 2.0.0 did).
    ck "supported: 2.0.0"      "$(t_ver_supported "e2a 2.0.0" && echo yes)" "yes"
    ck "supported: 2.9.3"      "$(t_ver_supported "e2a 2.9.3" && echo yes)" "yes"
    ck "unsupported: 1.6.0 (too old)" "$(t_ver_supported "e2a 1.6.0" || echo no)" "no"
    ck "unsupported: 3.0.0 (too new)" "$(t_ver_supported "e2a 3.0.0" || echo no)" "no"
    ck "E2A_CLI override is honored" "$(E2A_CLI="/bin/echo e2a-override" bash -c '. "'"${here}"'/lib.sh"; t_cli ping' 2>/dev/null)" "e2a-override ping"

    echo "# CLI exit-code mapping (2.x contract):"
    # A stub CLI that prints a message id and exits 7 (SEND_OUTCOME) — the
    # server persisted a terminal `failed`. The id being non-empty is the trap:
    # tether must NOT read that as a delivered message.
    stub7=/tmp/tether-selftest-exit7.sh
    printf '#!/usr/bin/env bash\necho msg_selftest_terminal\nexit 7\n' > "$stub7"; chmod +x "$stub7"
    armf7=/tmp/tether-selftest-exit7.json
    printf '{"armed":"1","conversation_id":"tether-x","to":"a@b.c","last_message_id":"msg_anchor"}' > "$armf7"
    out="$(env E2A_API_KEY='e2a_agt_selftest123' E2A_AGENT_EMAIL='selftest@agents.localhost' \
      E2A_CLI="$stub7" TETHER_STATE="$armf7" bash "${here}/tether.sh" update "terminal failure" 2>&1)"; rc=$?
    ck "update: CLI exit 7 → exit 5, reported NOT delivered" \
      "$rc:$(printf '%s' "$out" | grep -c 'terminal FAILED outcome')" "5:1"
    # And the state pointer must NOT advance onto an undelivered message.
    ck "update: exit 7 leaves last_message_id untouched" \
      "$(TETHER_STATE="$armf7" bash -c '. "'"${here}"'/lib.sh"; t_state_get last_message_id')" "msg_anchor"
    out="$(env E2A_API_KEY='e2a_agt_selftest123' E2A_AGENT_EMAIL='selftest@agents.localhost' \
      E2A_CLI="$stub7" TETHER_STATE="$armf7" bash "${here}/tether.sh" ask "will this block?" 2>&1)"; rc=$?
    ck "ask: CLI exit 7 → exit 5, does not wait" \
      "$rc:$(printf '%s' "$out" | grep -c 'not waiting')" "5:1"
    rm -f "$stub7" "$armf7" "${armf7%.json}.ask.lock" "${armf7}.lock"

    echo "# attachments:"
    af=/tmp/tether-selftest-att.txt; printf 'hello attachment' > "$af"
    t_attach_check "$af" || { echo "FAIL: attach check on a real file"; fail=1; }
    t_attach_check /nonexistent-tether-file 2>/dev/null; ck "missing file → exit 3" "$?" "3"
    big=/tmp/tether-selftest-big.bin
    python3 -c 'f=open("/tmp/tether-selftest-big.bin","wb");f.seek(16*1024*1024-1);f.write(b"\0")'
    t_attach_check "$big" 2>/dev/null; ck "16 MB → exit 4 (over cap)" "$?" "4"
    rm -f "$af" "$big"

    bash -n "${here}/lib.sh" && bash -n "${here}/tether.sh" && echo "# syntax OK"
    [ "$fail" = "0" ] && echo "# selftest PASS" || { echo "# selftest FAIL"; exit 1; }
    ;;

  ""|help|-h|--help)
    grep '^#' "${here}/tether.sh" | sed 's/^# \{0,1\}//' | sed -n '2,20p'
    ;;

  *) echo "tether: unknown command '$cmd' (try: setup|start|update|ask|listen|poll|status|stop)"; exit 2;;
esac
