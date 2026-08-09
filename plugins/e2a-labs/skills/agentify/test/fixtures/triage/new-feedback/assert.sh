#!/usr/bin/env bash
# new feedback -> a triaged issue is created; NO auto agent-fix (hitl); NO send.
log="$1"; fail=0
grep -q 'gh issue create' "$log" || { echo "FAIL: no issue created"; fail=1; }
grep -Eq 'gh issue create.*feedback' "$log" || { echo "FAIL: issue not labeled feedback"; fail=1; }
grep -q 'ticket_card init' "$log" || { echo "FAIL: no ticket-card written"; fail=1; }
grep -Eq 'add-label[^|]*agent-fix|--label[^|]*agent-fix' "$log" && { echo "FAIL: applied agent-fix in hitl mode"; fail=1; }
grep -q 'comms_send' "$log" && { echo "FAIL: triage sent mail (comms owns sending)"; fail=1; }
[ "$fail" = 0 ] && echo "PASS"; exit $fail
