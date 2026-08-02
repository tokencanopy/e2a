#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

boundary="$(tr -d '[:space:]' < ROLLBACK_UNSAFE_BEFORE)"
if ! [[ "$boundary" =~ ^(none|[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.]+)?)$ ]]; then
  echo "ROLLBACK_UNSAFE_BEFORE must be 'none' or an exact semver" >&2
  exit 1
fi

# Once a release crosses a migration boundary, every descendant must carry the
# same first-unsafe release. Comparing only one caller-supplied snapshot is not
# enough: a tag/manual publish could omit that ref after an earlier commit had
# already reset the file. Resolve a trusted ancestor, then scan the file's full
# reachable HEAD history chronologically, including every commit in a PR.
base_ref=${ROLLBACK_BASE_REF:-${1:-}}
require_base=${REQUIRE_ROLLBACK_BASE:-false}

case "$require_base" in
  true|false) ;;
  *) echo "REQUIRE_ROLLBACK_BASE must be true or false" >&2; exit 1 ;;
esac

if [ -z "$base_ref" ] || [[ "$base_ref" =~ ^0+$ ]]; then
  base_ref=""
  if git rev-parse --verify HEAD^ >/dev/null 2>&1; then
    base_ref=HEAD^
  fi
fi

if [ -z "$base_ref" ]; then
  if [ "$require_base" = true ]; then
    echo "a resolvable rollback-boundary base commit is required for publishing" >&2
    exit 1
  fi
  printf '%s\n' "$boundary"
  exit 0
fi

if ! git cat-file -e "${base_ref}^{commit}" 2>/dev/null; then
  echo "rollback-boundary base is not a commit: ${base_ref}" >&2
  exit 1
fi
if ! git merge-base --is-ancestor "$base_ref" HEAD; then
  echo "rollback-boundary base ${base_ref} is not an ancestor of HEAD" >&2
  exit 1
fi

historical_boundary=""
while IFS= read -r commit; do
  value="$(git show "${commit}:ROLLBACK_UNSAFE_BEFORE" 2>/dev/null | tr -d '[:space:]' || true)"
  [ -n "$value" ] || continue
  if ! [[ "$value" =~ ^(none|[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.]+)?)$ ]]; then
    echo "historical ROLLBACK_UNSAFE_BEFORE at ${commit} is invalid: ${value}" >&2
    exit 1
  fi
  if [ -z "$historical_boundary" ]; then
    [ "$value" = none ] || historical_boundary="$value"
  elif [ "$value" != "$historical_boundary" ]; then
    echo "ROLLBACK_UNSAFE_BEFORE changed historically after ${historical_boundary}: ${value} at ${commit}" >&2
    exit 1
  fi
done < <(git log --reverse --format=%H HEAD -- ROLLBACK_UNSAFE_BEFORE)

if [ -n "$historical_boundary" ] && [ "$boundary" != "$historical_boundary" ]; then
  echo "ROLLBACK_UNSAFE_BEFORE is cumulative: ${historical_boundary} cannot change to ${boundary}" >&2
  exit 1
fi

printf '%s\n' "$boundary"
