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
# same first-unsafe release. A PR/push cannot reset or move that cumulative
# boundary. The caller supplies the immutable base SHA used by CI.
base_ref=${ROLLBACK_BASE_REF:-${1:-}}
if [ -n "$base_ref" ] && git cat-file -e "${base_ref}:ROLLBACK_UNSAFE_BEFORE" 2>/dev/null; then
  previous="$(git show "${base_ref}:ROLLBACK_UNSAFE_BEFORE" | tr -d '[:space:]')"
  if [ "$previous" != none ] && [ "$boundary" != "$previous" ]; then
    echo "ROLLBACK_UNSAFE_BEFORE is cumulative: ${previous} cannot change to ${boundary}" >&2
    exit 1
  fi
fi

printf '%s\n' "$boundary"
