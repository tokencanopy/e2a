#!/usr/bin/env bash
# comms_send.sh — the ONLY outbound-mail surface for the comms lane.
#
# WHY THIS EXISTS: the raw e2a MCP send tools (reply_to_message /
# send_message) accept `to`/`cc`/`bcc`/`reply_all`/`attachments`, so a
# prompt injection in untrusted inbound email could turn the lane into an
# open mail relay from the adopter's verified domain, or bcc thread content
# (and run-env secrets) to an attacker. The comms lane therefore DISALLOWS
# those tools and sends only through this wrapper, which computes recipients
# from the thread / config and never sets cc/bcc/reply_all. The model
# controls the body text only — not who receives it.
#
#   comms_send.sh reply    <message_id> <body>   # reply IN-THREAD (recipient
#                                                # is server-derived from the
#                                                # inbound; no cc/bcc)
#   comms_send.sh approval <subject>    <body>   # NEW thread to the configured
#                                                # approver ONLY
#   comms_send.sh _selftest                      # payload-construction tests (no HTTP)
#
# Env (the workflow exports these from config; set them for interactive use):
#   E2A_API_KEY                the agent-scoped key (secret)
#   AUTOREPO_E2A_API_URL       e2a REST base, e.g. https://api.e2a.dev
#   AUTOREPO_SUPPORT_ADDRESS   the support mailbox (comms.support_address)
#   AUTOREPO_APPROVER          fix_gate.approver (the ONLY new-thread recipient)
#   AUTOREPO_SEND_DRYRUN=1      print the request instead of sending (tests)
set -euo pipefail

_need() { [ -n "${!1:-}" ] || { echo "comms_send.sh: $1 is required" >&2; exit 2; }; }

# _emit METHOD PATH PAYLOAD — curl to e2a, or print under DRYRUN.
_emit() {
  local method="$1" path="$2" payload="$3"
  if [ "${AUTOREPO_SEND_DRYRUN:-}" = "1" ]; then
    printf '%s %s\n%s\n' "$method" "$path" "$payload"; return 0
  fi
  _need E2A_API_KEY; _need AUTOREPO_E2A_API_URL
  curl -sS -X "$method" "${AUTOREPO_E2A_API_URL}${path}" \
    -H "Authorization: Bearer ${E2A_API_KEY}" \
    -H "Content-Type: application/json" \
    --data "$payload" \
    --fail-with-body
}

cmd="${1:-}"; shift || true
case "$cmd" in
  reply)
    _need AUTOREPO_SUPPORT_ADDRESS
    mid="$1"; body="$2"
    [ -n "$mid" ] && [ -n "$body" ] || { echo "usage: comms_send.sh reply <message_id> <body>" >&2; exit 2; }
    # Reply endpoint derives the recipient + Re: subject + thread headers
    # server-side from the inbound. We send ONLY the body. No cc/bcc/reply_all.
    payload="$(jq -cn --arg b "$body" '{body:$b}')"
    _emit POST "/v1/agents/${AUTOREPO_SUPPORT_ADDRESS}/messages/${mid}/reply" "$payload"
    ;;
  approval)
    _need AUTOREPO_SUPPORT_ADDRESS; _need AUTOREPO_APPROVER
    subject="$1"; body="$2"
    [ -n "$subject" ] && [ -n "$body" ] || { echo "usage: comms_send.sh approval <subject> <body>" >&2; exit 2; }
    # New thread, recipient is the configured approver ONLY — never an
    # address from email content.
    payload="$(jq -cn --arg to "$AUTOREPO_APPROVER" --arg s "$subject" --arg b "$body" \
      '{to:[$to], subject:$s, body:$b}')"
    resp="$(_emit POST "/v1/agents/${AUTOREPO_SUPPORT_ADDRESS}/messages" "$payload")"
    if [ "${AUTOREPO_SEND_DRYRUN:-}" = "1" ]; then
      printf '%s\n' "$resp"                    # tests inspect the request
    else
      # print ONLY the new thread's conversation_id (comms records it as
      # approval.conversation_id; the agent has no jq to parse the response).
      printf '%s' "$resp" | jq -r '.conversation_id // empty'
    fi
    ;;
  _selftest)
    fail=0
    export AUTOREPO_SEND_DRYRUN=1 AUTOREPO_SUPPORT_ADDRESS="support@x.test" AUTOREPO_APPROVER="boss@x.test"
    r="$(bash "$0" reply 42 $'thanks\nmore')"
    echo "$r" | grep -q '/v1/agents/support@x.test/messages/42/reply' || { echo "FAIL reply path"; fail=1; }
    echo "$r" | grep -Eqi 'cc|bcc|reply_all|"to"' && { echo "FAIL reply leaked recipient/cc fields"; fail=1; }
    [ "$(echo "$r" | tail -1 | jq -r '.body')" = $'thanks\nmore' ] || { echo "FAIL reply body"; fail=1; }
    a="$(bash "$0" approval 'Approve #7?' 'reply approve')"
    echo "$a" | grep -q '/v1/agents/support@x.test/messages$' || { echo "FAIL approval path"; fail=1; }
    [ "$(echo "$a" | tail -1 | jq -r '.to[0]')" = "boss@x.test" ] || { echo "FAIL approval recipient not the configured approver"; fail=1; }
    echo "$a" | grep -Eqi 'cc|bcc|reply_all' && { echo "FAIL approval leaked cc/bcc"; fail=1; }
    if [ "$fail" = 0 ]; then echo "comms_send.sh selftest: OK"; else echo "comms_send.sh selftest: FAILED"; exit 1; fi
    ;;
  *)
    echo "usage: comms_send.sh {reply|approval|_selftest} ..." >&2; exit 2 ;;
esac
