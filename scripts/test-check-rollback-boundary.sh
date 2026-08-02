#!/usr/bin/env bash
set -euo pipefail

source_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/rollback-boundary-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/scripts"
cp "$source_root/scripts/check-rollback-boundary.sh" "$tmp/scripts/"
chmod +x "$tmp/scripts/check-rollback-boundary.sh"

git -C "$tmp" init -q
git -C "$tmp" config user.name Test
git -C "$tmp" config user.email test@example.com
printf '%s\n' none > "$tmp/ROLLBACK_UNSAFE_BEFORE"
git -C "$tmp" add ROLLBACK_UNSAFE_BEFORE
git -C "$tmp" commit -qm safe
safe_sha=$(git -C "$tmp" rev-parse HEAD)

printf '%s\n' 2.0.0 > "$tmp/ROLLBACK_UNSAFE_BEFORE"
ROLLBACK_BASE_REF="$safe_sha" "$tmp/scripts/check-rollback-boundary.sh" >/dev/null
git -C "$tmp" commit -qam boundary
boundary_sha=$(git -C "$tmp" rev-parse HEAD)

printf '%s\n' none > "$tmp/ROLLBACK_UNSAFE_BEFORE"
if ROLLBACK_BASE_REF="$boundary_sha" "$tmp/scripts/check-rollback-boundary.sh" >/dev/null 2>&1; then
  echo "resetting a cumulative rollback boundary unexpectedly passed" >&2
  exit 1
fi

# Publishing entry points such as tag push and workflow_dispatch may not have a
# useful event.before. Commit the reset and prove the checker derives HEAD^ and
# still finds the earlier cumulative value in history.
git -C "$tmp" commit -qam reset
if REQUIRE_ROLLBACK_BASE=true "$tmp/scripts/check-rollback-boundary.sh" >/dev/null 2>&1; then
  echo "history-aware reset with an omitted base unexpectedly passed" >&2
  exit 1
fi

printf '%s\n' 2.0.0 > "$tmp/ROLLBACK_UNSAFE_BEFORE"
if REQUIRE_ROLLBACK_BASE=true "$tmp/scripts/check-rollback-boundary.sh" >/dev/null 2>&1; then
  echo "a transient committed reset hidden by a later restore unexpectedly passed" >&2
  exit 1
fi

if ROLLBACK_BASE_REF=not-a-commit REQUIRE_ROLLBACK_BASE=true \
   "$tmp/scripts/check-rollback-boundary.sh" >/dev/null 2>&1; then
  echo "an unresolvable required base unexpectedly passed" >&2
  exit 1
fi

# Return to the clean boundary commit. The committed reset remains proven above
# but must not contaminate the unchanged-boundary success case below.
git -C "$tmp" restore ROLLBACK_UNSAFE_BEFORE
git -C "$tmp" checkout -q "$boundary_sha"

printf '%s\n' 2.1.0 > "$tmp/ROLLBACK_UNSAFE_BEFORE"
if ROLLBACK_BASE_REF="$boundary_sha" "$tmp/scripts/check-rollback-boundary.sh" >/dev/null 2>&1; then
  echo "moving a cumulative rollback boundary unexpectedly passed" >&2
  exit 1
fi

printf '%s\n' 2.0.0 > "$tmp/ROLLBACK_UNSAFE_BEFORE"
ROLLBACK_BASE_REF="$boundary_sha" "$tmp/scripts/check-rollback-boundary.sh" >/dev/null
echo "rollback boundary checks: PASS"
