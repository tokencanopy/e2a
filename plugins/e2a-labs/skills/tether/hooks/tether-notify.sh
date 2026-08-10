#!/usr/bin/env bash
# tether-notify.sh — optional Claude Code Notification hook.
#
# Fires when the agent is blocked (needs permission) or goes idle. If a tether
# is armed, it emails you into the thread so you know the agent wants attention
# even when you're away. No-ops when not armed / not configured. Always exits 0.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib.sh
. "${here}/../lib.sh"

payload="$(cat)"
message="$(printf '%s' "$payload" | python3 -c 'import json,sys
try:print(json.load(sys.stdin).get("message","") or "")
except Exception:print("")')"
[ -n "$message" ] || message="The agent needs your attention."

t_load_config || exit 0
[ "$(t_state_get armed)" = "1" ] || exit 0

# Anchored reply (last message → last inbound → intro), NOT a bare reply to
# last_message_id: when that anchor is the agent's own reply-created outbound,
# older hosted APIs 404 it — and this "agent is stuck" alert is the one email
# that must not silently vanish in exactly that situation.
t_reply_anchored "⏸️ Needs you: ${message}

Reply to this thread and I'll pick it up at the next check." >/dev/null 2>&1 || true
exit 0
