#!/usr/bin/env bash
# run.sh — the agentify harness test suite (deterministic; no network/secrets).
# Runs from anywhere; cd's to the agentify dir. Exit non-zero on any failure.
set -uo pipefail
cd "$(dirname "$0")/.."
RENDERER=$(pwd)/agentify-render.sh
fail=0
run() { echo "+ $*"; "$@" || fail=1; }
section() { echo; echo "== $1 =="; }
file_mode() {
  stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1"
}
run_renderer() {
  env \
    ANS_PRODUCT_NAME="Synthetic Product" \
    ANS_OWNER="synthetic-owner" \
    ANS_REPO="synthetic-repo" \
    ANS_MARKER="synthetic-feedback" \
    ANS_REVIEWER_LOGIN="synthetic-reviewer" \
    ANS_BOT_LOGIN="synthetic-bot[bot]" \
    ANS_SUPPORT_ADDRESS="support@example.test" \
    ANS_FIX_GATE_MODE="hitl" \
    ANS_APPROVER_ADDRESS="approver@example.test" \
    ANS_VERIFY_SETUP_SCRIPT="scripts/verify.sh" \
    bash "$RENDERER" "$@"
}

section "target symlink rejection"
for spelling in plain trailing-slash; do
  for force in no yes; do
    outside=$(mktemp -d)
    link_parent=$(mktemp -d)
    printf 'sentinel\n' > "$outside/sentinel"
    chmod 640 "$outside/sentinel"
    outside_mode=$(file_mode "$outside/sentinel")
    ln -s "$outside" "$link_parent/target-root"
    target_root="$link_parent/target-root"
    [ "$spelling" = plain ] || target_root="$target_root/"
    args=(--to "$target_root")
    [ "$force" = no ] || args+=(--force)
    if run_renderer "${args[@]}" >/dev/null 2>&1; then
      echo "FAIL: symbolic-link target root was accepted (spelling=$spelling force=$force)"; fail=1
    fi
    [ "$(cat "$outside/sentinel")" = sentinel ] || { echo "FAIL: outside root sentinel changed (spelling=$spelling force=$force)"; fail=1; }
    [ "$(file_mode "$outside/sentinel")" = "$outside_mode" ] || { echo "FAIL: outside root sentinel mode changed (spelling=$spelling force=$force)"; fail=1; }
    [ ! -e "$outside/autonomous-repo.config.yml" ] || { echo "FAIL: symbolic-link target root was populated (spelling=$spelling force=$force)"; fail=1; }
    rm -rf "$link_parent" "$outside"
  done
done

outside=$(mktemp -d)
target=$(mktemp -d)
printf 'sentinel\n' > "$outside/sentinel"
chmod 640 "$outside/sentinel"
outside_mode=$(file_mode "$outside/sentinel")
ln -s "$outside" "$target/scripts"
if run_renderer --to "$target" >/dev/null 2>&1; then
  echo "FAIL: symlinked scripts directory was accepted"; fail=1
fi
[ "$(cat "$outside/sentinel")" = sentinel ] || { echo "FAIL: outside sentinel changed through scripts link"; fail=1; }
[ "$(file_mode "$outside/sentinel")" = "$outside_mode" ] || { echo "FAIL: outside sentinel mode changed through scripts link"; fail=1; }
rm -rf "$target" "$outside"

for force in no yes; do
  outside=$(mktemp -d)
  target=$(mktemp -d)
  printf 'sentinel\n' > "$outside/sentinel"
  chmod 640 "$outside/sentinel"
  outside_mode=$(file_mode "$outside/sentinel")
  ln -s "$outside/sentinel" "$target/autonomous-repo.config.yml"
  args=(--to "$target")
  [ "$force" = no ] || args+=(--force)
  if run_renderer "${args[@]}" >/dev/null 2>&1; then
    echo "FAIL: symlinked config destination was accepted (force=$force)"; fail=1
  fi
  [ "$(cat "$outside/sentinel")" = sentinel ] || { echo "FAIL: outside config sentinel changed (force=$force)"; fail=1; }
  [ "$(file_mode "$outside/sentinel")" = "$outside_mode" ] || { echo "FAIL: outside config sentinel mode changed (force=$force)"; fail=1; }
  rm -rf "$target" "$outside"
done

section "CDPATH neutralization"
workdir=$(mktemp -d)
decoy=$(mktemp -d)
mkdir -p "$workdir/mytarget" "$decoy/mytarget"
printf 'decoy\n' > "$decoy/mytarget/sentinel"
(
  cd "$workdir" || exit 1
  export CDPATH="$decoy"
  run_renderer --to mytarget >/dev/null 2>&1
)
[ -f "$workdir/mytarget/autonomous-repo.config.yml" ] || { echo "FAIL: relative --to did not scaffold into the cwd target"; fail=1; }
[ -e "$decoy/mytarget/autonomous-repo.config.yml" ] && { echo "FAIL: CDPATH redirected --to canonicalization"; fail=1; }
[ "$(cat "$decoy/mytarget/sentinel")" = decoy ] || { echo "FAIL: CDPATH decoy was mutated"; fail=1; }
rm -rf "$workdir" "$decoy"

section "script selftests"
for s in templates/scripts/ticket_card templates/scripts/comms_send templates/scripts/released_markers; do
  run bash "$s.sh" _selftest
done
run bash agentify-render.sh _selftest
run python3 test/render-config.test.py

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

section "target write confinement"
echo "+ rg -n '(>>?|cp .*\\\$t|chmod .*\\\$t)' agentify-render.sh"
if audit_output=$(rg -n '(>>?|cp .*\$t|chmod .*\$t)' agentify-render.sh); then
  echo "$audit_output"
  echo "FAIL: agentify-render.sh writes directly to target paths"
  fail=1
fi

echo
if [ "$fail" = 0 ]; then echo "AGENTIFY TESTS: ALL PASS"; else echo "AGENTIFY TESTS: FAILURES ABOVE"; fi
exit $fail
