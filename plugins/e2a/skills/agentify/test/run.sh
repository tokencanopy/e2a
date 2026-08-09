#!/usr/bin/env bash
# run.sh — the agentify harness test suite (deterministic; no network/secrets).
# Runs from anywhere; cd's to the agentify dir. Exit non-zero on any failure.
set -uo pipefail
cd "$(dirname "$0")/.."
fail=0
run() { echo "+ $*"; "$@" || fail=1; }
section() { echo; echo "== $1 =="; }

section "script selftests"
for s in templates/scripts/ticket_card templates/scripts/comms_send templates/scripts/released_markers; do
  run bash "$s.sh" _selftest
done
run bash agentify-render.sh _selftest

section "addon bridge unit tests"
run node templates/addons/submit-feedback-mcp/files/bridge.test.mjs

section "permission contract"
run node test/permission-contract.test.mjs

section "bash syntax"
while IFS= read -r f; do run bash -n "$f"; done < <(find . -name '*.sh')

section "js syntax"
for f in templates/addons/submit-feedback-mcp/files/server.mjs templates/addons/submit-feedback-mcp/files/bridge.mjs; do
  run node --check "$f"
done

section "yaml + config validation"
run python3 test/validate.py

section "lane-fixture assertions (deterministic; model layer runs in CI)"
run bash test/fixtures/harness/assert-selftest.sh

echo
if [ "$fail" = 0 ]; then echo "AGENTIFY TESTS: ALL PASS"; else echo "AGENTIFY TESTS: FAILURES ABOVE"; fi
exit $fail
