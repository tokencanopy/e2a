#!/usr/bin/env bash
set -euo pipefail

# Prevent Node preload injection before any Node invocation, including the
# version probe. The trusted launcher constructs narrower child environments.
unset NODE_OPTIONS NODE_PATH

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
# and never resolves or executes suite-local runtime code.
exec node "$plugin_root/launcher.mjs" "$@"
