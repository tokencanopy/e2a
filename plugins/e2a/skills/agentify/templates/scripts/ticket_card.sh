#!/usr/bin/env bash
# ticket_card.sh — read/write the ticket-card (github ticket_store).
#
# The ticket-card is ONE bot-authored issue comment holding the machine-
# readable ticket state as a fenced JSON block between sentinels (see
# runtime-skill/ticket-card.md). This helper is the ONLY Bash surface the
# lanes are allowlisted for ticket state, so an injection in untrusted
# feedback has no general shell to read through.
#
# Trust: `read`/`set`/`add-event`/`find-by-comms` consider ONLY the bot
# identity ($AUTOREPO_BOT_LOGIN) — a third party can post a forged card
# comment (or a forged footer) on a public issue, and it must never be
# honored.
#
# Config values come from the environment (the workflow parses the config
# and exports them; set them yourself for interactive use):
#   AUTOREPO_REPO            owner/repo            (default: gh's repo context)
#   AUTOREPO_BOT_LOGIN       the bot's GitHub login (REQUIRED for read/find)
#   AUTOREPO_MARKER          configured marker (REQUIRED for find-by-comms)
#   AUTOREPO_FEEDBACK_LABEL  feedback label        (default: "feedback")
#
# Usage:
#   ticket_card.sh init  <issue> <card-json>      # post the card comment
#   ticket_card.sh read  <issue>                  # print the card JSON
#   ticket_card.sh set   <issue> <merge-json>     # deep-merge object fields (events NEVER replaced)
#   ticket_card.sh patch <issue> <merge-json>     # alias of set
#   ticket_card.sh add-event <issue> <event-json> # append one timeline event
#   ticket_card.sh find-by-comms <conversation_id># print issue#(s) whose bot footer/card matches
#   ticket_card.sh _selftest                      # pure-logic tests (no gh)
set -euo pipefail

BEGIN='<!-- autorepo:ticket-card:begin -->'
END='<!-- autorepo:ticket-card:end -->'

# --- pure logic (unit-tested via _selftest; no gh) ----------------------

# _extract_card: comment body on stdin -> card JSON between the sentinels.
# The sentinels are matched as WHOLE LINES, so a sentinel substring inside a
# field value cannot truncate extraction.
_extract_card() {
  awk '
    /^<!-- autorepo:ticket-card:begin -->[[:space:]]*$/ { f=1; next }
    /^<!-- autorepo:ticket-card:end -->[[:space:]]*$/   { f=0 }
    f' | sed '/^```/d'
}

# _wrap_card: card JSON on stdin -> the full comment body.
_wrap_card() {
  local json; json="$(cat)"
  printf '%s\n```json\n%s\n```\n%s\n' "$BEGIN" "$json" "$END"
}

# _merge: deep-merge patch ($2) into card ($1). The append-only `events`
# array is NEVER replaced by a patch (use add-event); del(.events) from the
# patch guarantees a `set` cannot wipe the audit trail.
_merge() { jq -n --argjson a "$1" --argjson b "$2" '$a * ($b | del(.events))'; }

# _append_event: append event object ($2) to card ($1).events.
_append_event() {
  jq -n --argjson a "$1" --argjson e "$2" '$a + {events: (($a.events // []) + [$e])}'
}

# _matches_comms_footer: issue JSON on stdin; exact bot-authored trusted footer.
_matches_comms_footer() { # stdin={author,body}; $1=bot $2=marker $3=conversation
  jq -e --arg bot "$1" --arg marker "$2" --arg conv "$3" '
    (.author.login == $bot) and
    ((.body | split("\n") | map(select(test("\\S"))) | last)
      == ("<!-- " + $marker + " comms:" + $conv + " -->"))'
}

# _select_card: a JSON ARRAY of issue comments on stdin -> the LATEST comment
# authored by $1 (the bot) that carries a ticket-card, as a compact
# {id, body} object; empty if none. THE security-load-bearing trust filter.
_select_card() {
  jq -c --arg bot "$1" '
    [ .[] | select(.user.login == $bot)
          | select(.body | contains("autorepo:ticket-card:begin")) ]
    | last // empty | {id, body}'
}

# --- gh-backed operations ----------------------------------------------

_repo() { echo "${AUTOREPO_REPO:-$(gh repo view --json nameWithOwner -q .nameWithOwner)}"; }

_require_bot() {
  if [ -z "${AUTOREPO_BOT_LOGIN:-}" ]; then
    echo "ticket_card.sh: AUTOREPO_BOT_LOGIN is required (trust the card only from the bot)" >&2
    exit 2
  fi
}

# _card_obj <issue> -> {id, body} of the latest bot-authored card, or empty.
# Multi-line bodies are handled as a single JSON record (no line splitting).
_card_obj() {
  local issue="$1" repo; repo="$(_repo)"; _require_bot
  gh api --paginate "repos/$repo/issues/$issue/comments" \
    | jq -s 'add // []' | _select_card "$AUTOREPO_BOT_LOGIN"
}

cmd="${1:-}"; shift || true
case "$cmd" in
  init)
    issue="$1"; card="$2"; repo="$(_repo)"
    echo "$card" | jq -e . >/dev/null    # validate
    printf '%s' "$card" | _wrap_card | gh issue comment "$issue" -R "$repo" --body-file -
    ;;
  read)
    issue="$1"
    obj="$(_card_obj "$issue")"
    [ -n "$obj" ] || { echo "ticket_card.sh: no card on issue $issue" >&2; exit 1; }
    printf '%s' "$obj" | jq -r '.body' | _extract_card | jq .
    ;;
  set|patch|add-event)
    issue="$1"; arg="$2"; repo="$(_repo)"
    obj="$(_card_obj "$issue")"
    [ -n "$obj" ] || { echo "ticket_card.sh: no card on issue $issue" >&2; exit 1; }
    cid="$(printf '%s' "$obj" | jq -r '.id')"
    card="$(printf '%s' "$obj" | jq -r '.body' | _extract_card)"
    if [ "$cmd" = "add-event" ]; then new="$(_append_event "$card" "$arg")"; else new="$(_merge "$card" "$arg")"; fi
    body="$(printf '%s' "$new" | _wrap_card)"
    gh api -X PATCH "repos/$repo/issues/comments/$cid" -f body="$body" --jq '.id' >/dev/null
    ;;
  find-by-comms)
    conv="$1"; repo="$(_repo)"; _require_bot
    if [ -z "${AUTOREPO_MARKER:-}" ]; then
      echo "ticket_card.sh: AUTOREPO_MARKER is required for find-by-comms" >&2
      exit 2
    fi
    label="${AUTOREPO_FEEDBACK_LABEL:-feedback}"
    # The crash-safe key is the exact last nonblank, bot-authored issue-body footer
    # `<!-- {marker} comms:<conversation_id> -->`, written ATOMICALLY with the issue (so a card
    # written later, or a run that died before the card, is still matched).
    # Test author + body with real jq (--arg) so an opaque conv id is never
    # interpolated into a program.
    for n in $(gh issue list -R "$repo" --label "$label" --state all --limit 500 --json number --jq '.[].number'); do
      if gh issue view "$n" -R "$repo" --json author,body \
           | _matches_comms_footer "$AUTOREPO_BOT_LOGIN" "$AUTOREPO_MARKER" "$conv" >/dev/null; then
        echo "$n"
      fi
    done
    ;;
  _selftest)
    fail=0
    # roundtrip
    card='{"schema":1,"ticket":7,"status":"triaged","comms_ref":"conv_x","events":[{"kind":"triaged"}]}'
    got="$(printf '%s' "$card" | _wrap_card | _extract_card | jq -c .)"
    [ "$got" = "$(echo "$card" | jq -c .)" ] || { echo "FAIL roundtrip: $got"; fail=1; }
    # merge keeps events, applies scalar/nested
    merged="$(_merge "$card" '{"status":"in_progress","pr":42,"events":[{"kind":"EVIL"}]}' | jq -c .)"
    [ "$(echo "$merged" | jq -r .status)" = "in_progress" ] || { echo "FAIL merge status"; fail=1; }
    [ "$(echo "$merged" | jq -r .pr)" = "42" ] || { echo "FAIL merge pr"; fail=1; }
    [ "$(echo "$merged" | jq '.events | length')" = "1" ] || { echo "FAIL merge clobbered events"; fail=1; }
    [ "$(echo "$merged" | jq -r '.events[0].kind')" = "triaged" ] || { echo "FAIL merge replaced events"; fail=1; }
    # append
    appended="$(_append_event "$card" '{"kind":"shipped"}' | jq -c .)"
    [ "$(echo "$appended" | jq '.events | length')" = "2" ] || { echo "FAIL append len"; fail=1; }
    [ "$(echo "$appended" | jq -r '.events[1].kind')" = "shipped" ] || { echo "FAIL append last"; fail=1; }
    # extraction is robust to a sentinel SUBSTRING inside a field value
    tricky='{"detail":"see autorepo:ticket-card:end for context","v":1}'
    gott="$(printf '%s' "$tricky" | _wrap_card | _extract_card | jq -c .)"
    [ "$gott" = "$(echo "$tricky" | jq -c .)" ] || { echo "FAIL sentinel-substring extract: $gott"; fail=1; }
    # _select_card: bot card chosen over an attacker card; latest of two bot cards
    ba="$(echo '{"v":"evil"}' | _wrap_card)"; b1="$(echo '{"v":1}' | _wrap_card)"; b2="$(echo '{"v":2}' | _wrap_card)"
    arr="$(jq -n --arg ba "$ba" --arg b1 "$b1" --arg b2 "$b2" '[
      {id:1,user:{login:"attacker"},body:$ba},
      {id:2,user:{login:"bot[bot]"},body:$b1},
      {id:3,user:{login:"bot[bot]"},body:$b2}]')"
    sel="$(printf '%s' "$arr" | _select_card "bot[bot]")"
    [ "$(echo "$sel" | jq -r '.id')" = "3" ] || { echo "FAIL select latest bot card"; fail=1; }
    [ "$(echo "$sel" | jq -r '.body' | _extract_card | jq -r '.v')" = "2" ] || { echo "FAIL select body"; fail=1; }
    # attacker-only -> empty (forged card never honored)
    arr2="$(jq -n --arg ba "$ba" '[{id:1,user:{login:"attacker"},body:$ba}]')"
    [ -z "$(printf '%s' "$arr2" | _select_card "bot[bot]")" ] || { echo "FAIL forged card honored"; fail=1; }
    # exact trusted footer: quoted content cannot satisfy dedup
    good_body=$'summary\n```text\nuser data\n```\n<!-- acme-feedback comms:conv_target -->'
    forged_body=$'summary\n```text\ncomms:conv_target\n```\n<!-- acme-feedback comms:conv_other -->'
    good_issue="$(jq -n --arg body "$good_body" '{author:{login:"bot[bot]"},body:$body}')"
    forged_issue="$(jq -n --arg body "$forged_body" '{author:{login:"bot[bot]"},body:$body}')"
    third_party_issue="$(jq -n --arg body "$good_body" '{author:{login:"third-party"},body:$body}')"
    mismatched_marker_issue="$(jq -n --arg body "$good_body" '{author:{login:"bot[bot]"},body:$body}')"
    printf '%s' "$good_issue" | _matches_comms_footer "bot[bot]" "acme-feedback" "conv_target" \
      || { echo "FAIL exact comms footer rejected"; fail=1; }
    printf '%s' "$forged_issue" | _matches_comms_footer "bot[bot]" "acme-feedback" "conv_target" \
      && { echo "FAIL quoted comms value accepted"; fail=1; }
    printf '%s' "$third_party_issue" | _matches_comms_footer "bot[bot]" "acme-feedback" "conv_target" \
      && { echo "FAIL third-party footer accepted"; fail=1; }
    printf '%s' "$mismatched_marker_issue" | _matches_comms_footer "bot[bot]" "other-marker" "conv_target" \
      && { echo "FAIL mismatched marker accepted"; fail=1; }
    # no card -> empty extract
    [ -z "$(printf 'just a comment\n' | _extract_card)" ] || { echo "FAIL empty-extract"; fail=1; }
    if [ "$fail" = 0 ]; then echo "ticket_card.sh selftest: OK"; else echo "ticket_card.sh selftest: FAILED"; exit 1; fi
    ;;
  *)
    echo "usage: ticket_card.sh {init|read|set|patch|add-event|find-by-comms|_selftest} ..." >&2
    exit 2
    ;;
esac
