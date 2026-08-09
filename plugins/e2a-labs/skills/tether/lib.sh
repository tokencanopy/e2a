#!/usr/bin/env bash
# lib.sh — shared config + e2a helpers for the tether skill.
#
# Config comes from the environment, falling back to ~/.e2a-tether.env.
# Required: E2A_API_KEY, E2A_AGENT_EMAIL. The recipient is supplied at
# `tether.sh start <email>` and kept in the state file.
#
# Transport is the e2a CLI (@e2a/cli), NOT raw curl: send/reply/list/get all
# go through `t_cli`, which resolves $E2A_CLI → `e2a` on PATH (if new enough)
# → `npx -y @e2a/cli@^MIN`. Node is always present where this skill runs
# (Claude Code requires it), so existing users need to install nothing and
# minor CLI upgrades flow automatically through npx. Python 3 is still needed
# for local state/JSON handling only.

t_load_config() {
  # Explicit env vars win; each fallback source fills only the vars still missing
  # (so exporting just E2A_API_KEY in the env doesn't skip an email set in the file).
  local envk="${E2A_API_KEY:-}" enve="${E2A_AGENT_EMAIL:-}" envu="${E2A_URL:-${E2A_BASE_URL:-}}"

  # 1) explicit tether config
  if { [ -z "${E2A_API_KEY:-}" ] || [ -z "${E2A_AGENT_EMAIL:-}" ]; } && [ -f "${HOME}/.e2a-tether.env" ]; then
    # shellcheck disable=SC1091
    set -a; . "${HOME}/.e2a-tether.env"; set +a
  fi
  # 2) reuse the CLI's agent creds from `e2a login` (~/.e2a/config.json)
  if { [ -z "${E2A_API_KEY:-}" ] || [ -z "${E2A_AGENT_EMAIL:-}" ]; } && [ -f "${HOME}/.e2a/config.json" ]; then
    eval "$(python3 -c 'import json,shlex,os
try:
  d=json.load(open(os.path.expanduser("~/.e2a/config.json")))
  if d.get("api_key"):     print("export E2A_API_KEY="+shlex.quote(d["api_key"]))
  if d.get("agent_email"): print("export E2A_AGENT_EMAIL="+shlex.quote(d["agent_email"]))
  if d.get("api_url"):     print("export E2A_URL="+shlex.quote(d["api_url"].rstrip("/")))
except Exception:pass')"
  fi
  # A ~/.e2a-tether.env written before the rename still says E2A_BASE_URL. Carry
  # it over rather than silently falling back to the default host (which would
  # point a self-hoster at production), then drop the stale name so the CLI —
  # which no longer reads it — doesn't warn about a var tether already handled.
  if [ -z "${E2A_URL:-}" ] && [ -n "${E2A_BASE_URL:-}" ]; then
    E2A_URL="$E2A_BASE_URL"
    echo "tether: E2A_BASE_URL is deprecated — rename it to E2A_URL in ~/.e2a-tether.env" >&2
  fi
  unset E2A_BASE_URL

  # explicit env always wins over whatever a fallback source supplied
  [ -n "$envk" ] && E2A_API_KEY="$envk"
  [ -n "$enve" ] && E2A_AGENT_EMAIL="$enve"
  [ -n "$envu" ] && E2A_URL="$envu"

  # Treat the copied-but-unfilled tether.env.example placeholders as unset, so a
  # user who ran `cp … tether.env.example` without editing gets `config: MISSING`
  # (the new-user signal) instead of a bogus `config: OK` that fails at `start`.
  # Match ONLY the template's exact defaults — a real corp address that happens
  # to end in .example must not be silently discarded by a heuristic.
  case "${E2A_API_KEY:-}" in *...*) E2A_API_KEY="";; esac
  case "${E2A_AGENT_EMAIL:-}" in tether@you.example) E2A_AGENT_EMAIL="";; esac

  # EXPORTED: the transport is a child process (the e2a CLI). An unexported
  # default here would leave the CLI on its own default host while status
  # reports this one — a silent split-brain once the hosts diverge.
  #
  # E2A_URL is the CLI's deployment root (it serves the dashboard and proxies
  # /v1), NOT the API host — so this default tracks the CLI's own default
  # rather than forcing api.e2a.dev on it as the old E2A_BASE_URL name did.
  export E2A_URL="${E2A_URL:-https://e2a.dev}"
  [ -n "${E2A_API_KEY:-}" ] && [ -n "${E2A_AGENT_EMAIL:-}" ]
}

t_now_iso()  { python3 -c 'import datetime;print(datetime.datetime.now(datetime.timezone.utc).isoformat())'; }

# --- e2a CLI resolution --------------------------------------------------------
# The minimum CLI this skill's flags require (send --conversation-id, reply
# --html-file/--attach, messages list TSV, listen --once, exit-code contract).
#
# tether depends on the CLI's *contract*, not just its flags: the TSV column
# order of `messages list`, the exit-code table (0/3/4/5/7), and the
# whoami --json field names. A MAJOR bump is exactly the event that is allowed
# to change all three — CLI 2.0.0 renamed AccountView.agent_address to
# agent_email and added exit 7, both of which tether had to be taught. So the
# accepted range is MAJOR-BOUNDED on BOTH resolution paths:
#
#   npx  → @e2a/cli@^2.0.0   (caret: 2.x, never 3.x)
#   PATH → >= 2.0.0 AND major == 2
#
# The caret is deliberate, not an oversight. A bare floor (">=2.0.0") would
# auto-adopt a future 3.0.0 whose renames silently break reply pickup — the
# unattended-session failure this whole harness exists to avoid. Capping means
# a new major is *inert* until someone verifies tether against it, which is the
# safe direction to fail.
#
# MAINTENANCE: bump this on every CLI MAJOR (not just on new flag use — that
# was the rule that let the 1.6.0 floor go stale across 2.0.0), and re-verify
# the TSV columns, exit codes, and whoami fields before you do.
TETHER_MIN_CLI="2.0.0"

# t_ver_ge "<e2a 2.0.1>" "2.0.0" → 0 when the version (last token) >= min.
t_ver_ge() {
  python3 -c 'import sys
def v(s):
    s = s.strip().split()[-1] if s.strip() else "0"
    return [int(x) for x in s.lstrip("v").split(".")[:3] if x.isdigit()] or [0]
sys.exit(0 if v(sys.argv[1]) >= v(sys.argv[2]) else 1)' "$1" "$2" 2>/dev/null
}

# t_ver_major "<e2a 2.0.1>" → 2 (0 when unparseable).
t_ver_major() {
  python3 -c 'import sys
s = sys.argv[1].strip()
s = s.split()[-1] if s else "0"
p = s.lstrip("v").split(".")[0]
print(p if p.isdigit() else "0")' "$1" 2>/dev/null || echo 0
}

# t_ver_supported "<e2a 2.0.1>" → 0 when the version is inside tether's
# supported range: >= TETHER_MIN_CLI and the same major.
t_ver_supported() {
  t_ver_ge "$1" "$TETHER_MIN_CLI" || return 1
  [ "$(t_ver_major "$1")" = "$(t_ver_major "$TETHER_MIN_CLI")" ]
}

# t_cli <args...> — run the e2a CLI, bounded by a hard timeout. Resolution:
#   1. $E2A_CLI override (multi-word ok, e.g. "node /repo/cli/dist/bin/e2a.js";
#      paths containing SPACES are unsupported — use a wrapper script)
#   2. `e2a` on PATH when --version is inside tether's supported major range
#      (too old OR too new → warn, fall through)
#   3. npx -y @e2a/cli@^MIN  (auto-fetch; nothing for the user to install)
# The result is exported (T_CLI_RESOLVED) so the many $(t_cli …) subshells
# don't re-probe versions or re-print the staleness warning on every API call.
T_CLI=()
t_cli_resolve() {
  [ "${#T_CLI[@]}" -gt 0 ] && return 0
  if [ -n "${T_CLI_RESOLVED:-}" ]; then
    # shellcheck disable=SC2206
    T_CLI=($T_CLI_RESOLVED)
    return 0
  fi
  if [ -n "${E2A_CLI:-}" ]; then
    # shellcheck disable=SC2206  # intentional word-split for multi-word overrides
    T_CLI=($E2A_CLI)
    if [ "${#T_CLI[@]}" -eq 0 ] || ! command -v "${T_CLI[0]}" >/dev/null 2>&1; then
      echo "tether: E2A_CLI is set but not runnable: '${E2A_CLI}'" >&2
      T_CLI=()
      return 127
    fi
  elif command -v e2a >/dev/null 2>&1 && t_ver_supported "$(e2a --version 2>/dev/null)"; then
    T_CLI=(e2a)
  elif command -v npx >/dev/null 2>&1; then
    if command -v e2a >/dev/null 2>&1; then
      local gv; gv="$(e2a --version 2>/dev/null)"
      # "too new" and "too old" need OPPOSITE fixes — never collapse them into
      # one "upgrade" hint. A newer MAJOR is unverified against tether's TSV/
      # exit-code contract, so upgrading further would make it worse.
      if t_ver_ge "$gv" "$TETHER_MIN_CLI"; then
        echo "tether: global e2a CLI (${gv}) is a NEWER major than tether supports (^${TETHER_MIN_CLI}) — using npx to pin ${TETHER_MIN_CLI%%.*}.x for this run. If that major is known good, bump TETHER_MIN_CLI in lib.sh." >&2
      else
        echo "tether: global e2a CLI (${gv}) is older than ${TETHER_MIN_CLI} — using npx for this run (upgrade: npm i -g @e2a/cli@latest)" >&2
      fi
    fi
    T_CLI=(npx -y "@e2a/cli@^${TETHER_MIN_CLI}")
  else
    echo "tether: no e2a CLI available — install Node/npm, or set E2A_CLI" >&2
    return 127
  fi
  export T_CLI_RESOLVED="${T_CLI[*]}"
}

# Hard deadline on every transport call (default 150s; the old curl layer used
# -m 30/-m 120). A hung CLI/npx/DNS must never freeze an unattended session —
# the watchdog TERMs it and the wrappers report "transient". Long-lived waits
# (t_ws_wait) raise the bound per call to cover their window.
t_cli() {
  t_cli_resolve || return 127
  local tmo="${E2A_TETHER_CLI_TIMEOUT:-150}"
  "${T_CLI[@]}" "$@" &
  local pid=$!
  ( sleep "$tmo"; kill "$pid" 2>/dev/null ) >/dev/null 2>&1 &
  local wd=$!
  local rc=0
  wait "$pid" || rc=$?
  kill "$wd" 2>/dev/null
  wait "$wd" 2>/dev/null || true
  return "$rc"
}

# Human-readable description of what t_cli would run (for status/_selftest).
t_cli_desc() {
  if [ -n "${E2A_CLI:-}" ]; then echo "\$E2A_CLI (${E2A_CLI})"
  elif command -v e2a >/dev/null 2>&1 && t_ver_supported "$(e2a --version 2>/dev/null)"; then
    echo "e2a on PATH ($(e2a --version 2>/dev/null))"
  elif command -v npx >/dev/null 2>&1; then echo "npx @e2a/cli@^${TETHER_MIN_CLI}"
  else echo "MISSING (no e2a, no npx)"; fi
}

# --- state -------------------------------------------------------------------
# One tether = one session = one state file. The default path is keyed by the
# repo (git toplevel, else cwd) so concurrent tethered sessions in DIFFERENT
# repos can't clobber each other's thread pointer, watermark, seen-set, or
# ask-lock. Two sessions in the SAME repo still resolve to one key — `start`
# refuses to arm over a live session (loud, instead of silently hijacking its
# thread); set TETHER_STATE to a unique path to run them in parallel anyway.
# A legacy machine-global state.json (pre-keying) is honored while it exists,
# so a live tether isn't orphaned by a plugin update; it disappears at `stop`.

t_state_key() {
  local root
  root="$(git rev-parse --show-toplevel 2>/dev/null)" || root="$PWD"
  python3 -c 'import hashlib,sys;print(hashlib.sha1(sys.argv[1].encode()).hexdigest()[:12])' "$root"
}

T_STATE_PATH=""   # memoized: t_state_path runs in every poll tick
t_state_path() {
  if [ -z "$T_STATE_PATH" ]; then
    if [ -n "${TETHER_STATE:-}" ]; then
      T_STATE_PATH="$TETHER_STATE"
    elif [ -f "$HOME/.e2a-tether/state.json" ]; then
      T_STATE_PATH="$HOME/.e2a-tether/state.json"
    else
      T_STATE_PATH="$HOME/.e2a-tether/state-$(t_state_key).json"
    fi
  fi
  echo "$T_STATE_PATH"
}

t_state_get() {
  local f; f="$(t_state_path)"; [ -f "$f" ] || return 0
  python3 -c 'import json,sys
try:print(json.load(open(sys.argv[1])).get(sys.argv[2],"") or "")
except Exception:pass' "$f" "$1"
}

# State writes are flocked read-modify-writes with an atomic rename: a
# background `listen` and a foreground `update` share this file, so an
# unlocked truncate-then-write would (a) let a concurrent reader see a torn/
# empty file — whose empty conversation_id then reads as "no filter" — and
# (b) let last-writer-wins drop the other process's mutation (a lost `seen`
# entry re-executes an already-handled instruction).
t_state_set() {  # t_state_set k1 v1 [k2 v2 ...]
  local f; f="$(t_state_path)"; mkdir -p "$(dirname "$f")"
  python3 -c 'import json,sys,os,fcntl
f=sys.argv[1];kv=sys.argv[2:]
lock=open(f+".lock","w"); fcntl.flock(lock,fcntl.LOCK_EX)
d={}
if os.path.exists(f):
  try:d=json.load(open(f))
  except Exception:d={}
for i in range(0,len(kv),2):d[kv[i]]=kv[i+1]
tmp="%s.tmp.%d"%(f,os.getpid())
json.dump(d,open(tmp,"w"),indent=2)
os.replace(tmp,f)' "$f" "$@"
}

t_state_clear() {
  local f; f="$(t_state_path)"
  rm -f "$f" "${f}.lock" "$(t_ask_lock_path)"
}

# --- ask/listen mutex --------------------------------------------------------
# `ask` and `listen` both poll the same inbox and share one dedup cursor, so if
# they run at once whichever polls first consumes the reply — a running `listen`
# would eat the answer `ask` is blocking for, and `ask` would spin to timeout.
# While an `ask` is in flight it holds this lock; `listen` sees it and pauses
# polling so the answer flows to the blocked `ask`.

# The lock lives NEXT TO its session's state file (state-<key>.ask.lock), not
# at a fixed name — a fixed name would couple ask/listen across unrelated
# sessions that share ~/.e2a-tether/.
t_ask_lock_path() { local f; f="$(t_state_path)"; echo "${f%.json}.ask.lock"; }
# The lock records the ask's PID so it can't outlive its owner: an EXIT trap
# can't catch SIGKILL (OOM killer, force-quit), and a lock that survives its
# process would pause the background listen FOREVER — replies silently ignored
# in exactly the unattended session tether exists for. A lock whose PID is
# dead is stale and gets reaped on the next check.
t_ask_begin()  { local f; f="$(t_ask_lock_path)"; mkdir -p "$(dirname "$f")"; echo "$$" > "$f"; }
t_ask_end()    { rm -f "$(t_ask_lock_path)"; }
t_ask_active() {
  local f pid; f="$(t_ask_lock_path)"; [ -f "$f" ] || return 1
  pid="$(cat "$f" 2>/dev/null)"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then return 0; fi
  rm -f "$f"   # stale lock from a killed ask — reap so listen resumes
  return 1
}

# --- duration / expiry -------------------------------------------------------

# t_duration_to_expiry <dur> → ISO expires_at (empty = no expiry / "until stop";
# "INVALID" = unparseable, so the caller can reject it instead of silently
# treating a mistyped "1h30m"/"90 min" as an unbounded window).
# accepts a SINGLE unit: 30m, 2h, 8h, 1d ; "" / forever / until-stop → empty
t_duration_to_expiry() {
  python3 -c 'import sys,datetime,re
d=(sys.argv[1] if len(sys.argv)>1 else "").strip().lower()
if not d or d in ("forever","none","off","until-stop","stop"):print("");raise SystemExit
m=re.fullmatch(r"(\d+)\s*([mhd])",d)
if not m:print("INVALID");raise SystemExit
secs=int(m.group(1))*{"m":60,"h":3600,"d":86400}[m.group(2)]
print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=secs)).isoformat())' "$1"
}

t_parse_until() {
  python3 -c 'import datetime,sys
try:
    raw=sys.argv[1]
    value=datetime.datetime.fromisoformat(raw.replace("Z", "+00:00"))
    if value.tzinfo is None or value.utcoffset() is None:
        raise ValueError("timezone required")
    utc=value.astimezone(datetime.timezone.utc).replace(microsecond=0)
    print(utc.isoformat().replace("+00:00", "Z"))
except Exception:
    print("INVALID")' "$1"
}

# t_remaining_seconds → seconds until expires_at; huge sentinel if no expiry
t_remaining_seconds() {
  local f; f="$(t_state_path)"
  [ -f "$f" ] || { echo 2147483647; return; }
  python3 -c 'import sys,datetime,json
try:
  state=json.load(open(sys.argv[1]))
  if not isinstance(state,dict): raise ValueError("state must be an object")
  if "expires_at" not in state or state["expires_at"] == "":
    print(2147483647); raise SystemExit
  exp=state["expires_at"]
  if not isinstance(exp,str): raise ValueError("expires_at must be a string")
  t=datetime.datetime.fromisoformat(exp.replace("Z","+00:00"))
  if t.tzinfo is None or t.utcoffset() is None: raise ValueError("timezone required")
  print(int((t-datetime.datetime.now(datetime.timezone.utc)).total_seconds()))
except Exception:print(-1)' "$f"
}

# --- e2a API (via the CLI) -----------------------------------------------------
# The CLI's exit-code contract does the heavy lifting (CLI 2.x):
#   0 sent OR accepted (durably queued — 2.0.0 stopped lumping `accepted` in
#     with HELD, so a queued send no longer looks like a review hold)
#   3 HELD (status pending_review — accepted by the API, NOT delivered)
#   4 auth / 5 permanent request error / 7 terminal send outcome / 1 transient
#
# Exit 7 (SEND_OUTCOME) is new in 2.0.0: the server persisted a terminal
# `failed` (or an unrecognized) status and returned a message id, so the send
# must NOT be retried — a retry could duplicate. Before this mapping it fell
# into the `*)` transient bucket, and because the CLI still prints the message
# id on stdout, tether saw a non-empty id with a non-fatal code and cheerfully
# reported "update sent" for a message that never went anywhere.
#
# These wrappers map the CLI's codes onto tether's internal ones:
#   0 ok / 2 held / 3 anchor-not-repliable / 4 auth / 5 terminal (do not retry)
#   1 transient
# The CLI reads E2A_API_KEY / E2A_AGENT_EMAIL / E2A_URL straight from the
# environment t_load_config resolves — no flag plumbing needed.

# t_api_send <to> <subject> <body> <conversation_id> → prints message_id;
# returns 2 if the send was HELD so the caller can refuse to arm; 4 = auth
# failure (key revoked/invalid — callers must fail loud, not retry); 5 = the
# send reached a terminal failed/unknown outcome (do NOT retry — inspect the
# printed message id instead).
# The body travels via --body-file: free text may legitimately start with
# "--" (which the flag parser rejects) and may exceed argv limits.
t_api_send() {
  local mid rc bf
  bf="$(mktemp "${TMPDIR:-/tmp}/tether-body.XXXXXX")" || return 1
  printf '%s' "$3" > "$bf"
  mid="$(t_cli send --to "$1" --subject "$2" --body-file "$bf" --conversation-id "$4")"; rc=$?
  rm -f "$bf"
  printf '%s' "$mid"
  case "$rc" in 0) return 0;; 3) return 2;; 4) return 4;; 7) return 5;; *) return 1;; esac
}

# Attachment cap: 15 MB of raw bytes total per send. Base64 inflates ~4/3 and
# the server rejects requests over 25 MB, so 15 MB raw (~20 MB encoded) leaves
# headroom for the body; typical mail providers cap around 25 MB too.
T_ATTACH_MAX_BYTES=$((15 * 1024 * 1024))

# t_attach_check <file>... → 0 ok; 3 a file is missing; 4 over the total cap.
# Validates BEFORE encoding so callers can fail fast with a clear message.
t_attach_check() {
  python3 -c 'import sys,os
maxb=int(sys.argv[1]); total=0
for f in sys.argv[2:]:
    if not os.path.isfile(f):
        sys.stderr.write("tether: attachment not found: %s\n"%f); sys.exit(3)
    total+=os.path.getsize(f)
if total>maxb:
    sys.stderr.write("tether: attachments total %d bytes — over the %d MB cap; send a link instead\n"%(total,maxb//1048576)); sys.exit(4)' \
    "$T_ATTACH_MAX_BYTES" "$@"
}

# t_api_reply <in_reply_to_id> <body> [html_file] [attach_file ...] → prints
# new message_id. NOTE the third arg is now an HTML *file path* (the CLI takes
# --html-file and derives the plain-text fallback itself when body is empty).
# Returns: 0 sent; 2 HELD (pending_review, NOT delivered — CLI exit 3); 3 anchor
# not repliable here (CLI exit 5, permanent request error) so t_reply_anchored
# can try the next anchor; 4 auth failure (fail loud, don't retry); 5 terminal
# send outcome (CLI exit 7 — the server persisted a failure; do NOT retry and do
# NOT fall through to another anchor, which would risk a duplicate); 1 anything
# else (transient).
t_api_reply() {
  local rid="$1" body="${2:-}" htmlfile="${3:-}"
  shift 2; [ $# -gt 0 ] && shift   # remaining args = attachment file paths
  local args=(reply "$rid") bf=""
  if [ -n "$body" ]; then
    # --body-file, not --body: free text may start with "--" or exceed argv.
    bf="$(mktemp "${TMPDIR:-/tmp}/tether-body.XXXXXX")" || return 1
    printf '%s' "$body" > "$bf"
    args+=(--body-file "$bf")
  fi
  [ -n "$htmlfile" ] && args+=(--html-file "$htmlfile")
  local f; for f in "$@"; do args+=(--attach "$f"); done
  local mid rc
  mid="$(t_cli "${args[@]}")"; rc=$?
  [ -n "$bf" ] && rm -f "$bf"
  printf '%s' "$mid"
  case "$rc" in 0) return 0;; 3) return 2;; 4) return 4;; 5) return 3;; 7) return 5;; *) return 1;; esac
}

# t_reply_anchored <body> <html_file> [attach_file ...] → send a threaded reply,
# trying anchors in order: the last message in the thread (best Gmail
# threading), then the user's last inbound reply, then the intro. Hosted APIs
# that predate reply-to-own-outbound (#360) 404 when the anchor is a
# reply-created outbound message — without this fallback, the SECOND of two
# consecutive updates with no user reply in between is silently undeliverable.
# Prints the new message_id; propagates t_api_reply's return code.
t_reply_anchored() {
  local body="$1" html="${2:-}"   # html = FILE PATH (passed through to --html-file)
  shift 1; [ $# -gt 0 ] && shift
  local tried="" rid mid rc
  for rid in "$(t_state_get last_message_id)" "$(t_state_get last_inbound_id)" "$(t_state_get intro_id)"; do
    [ -n "$rid" ] || continue
    case " $tried " in *" $rid "*) continue;; esac
    tried="$tried $rid"
    mid="$(t_api_reply "$rid" "$body" "$html" "$@")"; rc=$?
    # ONLY 3 (anchor not repliable) advances to the next anchor. A terminal
    # outcome (5) means the server already persisted this send — retrying it
    # against a different anchor would risk delivering a duplicate — so it
    # propagates immediately, as do held (2) and auth (4).
    [ "$rc" = "3" ] && continue   # anchor not repliable here — try the next one
    printf '%s' "$mid"; return $rc
  done
  echo "tether: no repliable anchor (tried:${tried})" >&2
  return 3
}

# t_api_poll <conversation_id> <since_iso> → TSV lines: id<TAB>from<TAB>created_at
# (inbound, oldest first). The CLI's TSV shape and defaults (read_status=all,
# sort asc) are this exact contract — that's not a coincidence, it was designed
# from this function.
t_api_poll() {
  t_cli messages list --direction inbound --conversation "$1" --since "$2" --limit 20 2>/dev/null
}

# t_api_body <message_id> → command text (parsed.text preferred; quoted history
# stripped). Empty means the message genuinely has no text (or a transient CLI
# failure) — t_poll_once consumes either case with a placeholder.
t_api_body() {
  t_cli messages get "$1" --text 2>/dev/null
}

# t_ws_wait <seconds> — block until a new inbound arrives in the thread OR the
# window elapses. A pure WAKE SIGNAL: output is discarded and nothing is marked
# seen — the caller re-runs the dedup-safe t_poll_once to actually consume.
# This is what turns the old 20s poll cadence into real-time pickup. Any CLI
# failure degrades to a plain poll-interval sleep so loops keep their cadence.
t_ws_wait() {
  local secs="$1" conv until rc t0
  conv="$(t_state_get conversation_id)"
  # No conversation = stopped session or torn state — never wait unfiltered.
  if [ -z "$conv" ]; then sleep "${E2A_TETHER_POLL_INTERVAL:-20}"; return 0; fi
  until="$(python3 -c 'import sys,datetime
print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=int(sys.argv[1]))).isoformat())' "$secs")"
  t0=$SECONDS
  # Raise the transport deadline past this wait's own window, else the
  # watchdog would kill a legitimate long listen.
  E2A_TETHER_CLI_TIMEOUT=$((secs + 30)) t_cli listen --conversation "$conv" --once --until "$until" >/dev/null 2>&1
  rc=$?
  # 0 = something arrived, 6 = clean window expiry; anything else is a
  # transient WS problem (e.g. deployments without the WS endpoint). Belt and
  # braces: if the CLI came back early for ANY non-arrival reason, sleep one
  # poll interval so the caller's loop degrades to polling cadence instead of
  # spinning hot.
  if [ "$rc" != "0" ] && [ $((SECONDS - t0)) -lt "$secs" ]; then
    sleep "${E2A_TETHER_POLL_INTERVAL:-20}"
  fi
  return 0
}

# --- dedup + poll core -------------------------------------------------------
# Replies are deduped by message-id (a `seen` set in state), NOT a bare time
# cursor — the since boundary is inclusive with second-truncated wire
# timestamps, so the watermark message always reappears and must be deduped.

t_seen_has() {  # <id> → 0 if already processed
  local f; f="$(t_state_path)"; [ -f "$f" ] || return 1
  python3 -c 'import json,sys
try:sys.exit(0 if sys.argv[2] in (json.load(open(sys.argv[1])).get("seen") or []) else 1)
except Exception:sys.exit(1)' "$f" "$1"
}

t_seen_add() {  # <id> → record as processed (cap at last 500); flocked + atomic
  local f; f="$(t_state_path)"; mkdir -p "$(dirname "$f")"
  python3 -c 'import json,sys,os,fcntl
f,i=sys.argv[1],sys.argv[2]
lock=open(f+".lock","w"); fcntl.flock(lock,fcntl.LOCK_EX)
d={}
if os.path.exists(f):
  try:d=json.load(open(f))
  except Exception:d={}
s=d.get("seen") or []
if i not in s:s.append(i)
d["seen"]=s[-500:]
tmp="%s.tmp.%d"%(f,os.getpid())
json.dump(d,open(tmp,"w"),indent=2)
os.replace(tmp,f)' "$f" "$1"
}

# t_poll_once → print any new replies (deduped), else "(no new replies)".
# Returns 1 (transient) without touching anything when the session has no
# conversation id — cleared state must never widen the poll to the whole
# mailbox and deliver strangers' emails as "replies".
t_poll_once() {
  local conv since rows n advance id from created body
  conv="$(t_state_get conversation_id)"; since="$(t_state_get last_poll)"
  if [ -z "$conv" ]; then
    echo "tether: no conversation in state (session stopped?)" >&2
    return 1
  fi
  rows="$(t_api_poll "$conv" "$since")" || return 1
  n=0; advance="$since"
  while IFS=$'\t' read -r id from created; do
    [ -n "$id" ] || continue
    if t_seen_has "$id"; then advance="$created"; continue; fi
    body="$(t_api_body "$id")"
    # Parsing is synchronous server-side (CLI contract), so an empty body is
    # real content absence — an attachment-only or fully-quoted reply — not a
    # race to retry. Consume it WITH a placeholder: the old retry-then-drop
    # window silently lost such replies (and could hot-loop the WS wake).
    [ -n "$body" ] || body="[no text content — attachment-only or fully-quoted reply; message ${id}]"
    t_seen_add "$id"; advance="$created"; n=$((n+1))
    printf '── reply from %s @ %s ──\n%s\n\n' "$from" "$created" "$body"
    # last_inbound_id is the always-repliable fallback anchor for
    # t_reply_anchored (an inbound message is a valid reply target on every
    # API version; the agent's own reply-created outbound may not be).
    t_state_set last_message_id "$id" last_inbound_id "$id"
  done <<< "$rows"
  t_state_set last_poll "$advance"
  [ "$n" -gt 0 ] || echo "(no new replies)"
}
