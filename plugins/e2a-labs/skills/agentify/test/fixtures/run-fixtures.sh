#!/usr/bin/env bash
# run-fixtures.sh — the golden-fixture lane tests.
#   1. assert-selftest (deterministic: the assertions discriminate — no model)
#   2. the model layer: drive each lane prompt via `claude -p` over a mocked
#      world, assert on recorded actions (token-gated; SKIPs without a token).
set -uo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
fail=0

echo "== assertion self-test (deterministic) =="
bash "$DIR/harness/assert-selftest.sh" || fail=1

echo; echo "== model layer (token-gated) =="
for fx in "$DIR"/triage/*/; do
  [ -f "${fx}assert.sh" ] || continue
  bash "$DIR/harness/runner.sh" triage "${fx%/}" || fail=1
done

echo
[ "$fail" = 0 ] && echo "LANE FIXTURES: PASS" || echo "LANE FIXTURES: FAILURES ABOVE"
exit $fail
