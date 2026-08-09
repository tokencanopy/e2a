#!/usr/bin/env bash
set -euo pipefail

plugin_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"

if ! command -v node >/dev/null 2>&1; then
  echo "email-evals requires Node.js 18 or newer." >&2
  exit 2
fi

node_version="$(node -p 'process.versions.node')"
node_major="${node_version%%.*}"
if [[ ! "$node_major" =~ ^[0-9]+$ ]] || (( node_major < 18 )); then
  echo "email-evals requires Node.js 18 or newer (found ${node_version})." >&2
  exit 2
fi

# The trusted dependency-free launcher owns full command grammar validation
# before it resolves or executes any suite-local runtime code.
exec node "$plugin_root/launcher.mjs" "$@"
