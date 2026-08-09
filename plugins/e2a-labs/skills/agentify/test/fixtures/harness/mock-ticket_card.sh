#!/usr/bin/env bash
# Fake ticket_card.sh for fixtures: records calls; find-by-comms/read return
# canned fixture data so the dedup/claim path is deterministic.
echo "ticket_card $*" >> "$ACTION_LOG"
case "${1:-}" in
  find-by-comms) cat "$FIXTURE_DIR/findbycomms.txt" 2>/dev/null || true ;;
  read)          cat "$FIXTURE_DIR/card-read.json" 2>/dev/null || { echo "no card" >&2; exit 1; } ;;
  *)             : ;;  # init/set/add-event: logged above
esac
exit 0
