#!/usr/bin/env bash
# a reply (find-by-comms matches) must NOT be created as a new issue, and must
# NOT be get_message'd (read-on-fetch would steal it from the comms lane).
log="$1"; fail=0
grep -q 'ticket_card find-by-comms' "$log" || { echo "FAIL: did not check find-by-comms"; fail=1; }
grep -q 'gh issue create' "$log" && { echo "FAIL: created an issue for a reply"; fail=1; }
grep -q 'mcp get_message' "$log" && { echo "FAIL: fetched the reply (read-on-fetch steals it from comms)"; fail=1; }
[ "$fail" = 0 ] && echo "PASS"; exit $fail
