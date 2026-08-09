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

command="${1:-}"
if [[ "$command" == "scaffold" ]]; then
  shift
  exec node "$plugin_root/scaffold.mjs" "$@"
fi

if [[ "$command" == "setup" ]]; then
  shift
  exec node "$plugin_root/setup.mjs" "$@"
fi

if [[ -z "$command" ]]; then
  echo "Usage: email-evals.sh <scaffold|setup|validate|run|regrade> ..." >&2
  exit 2
fi

suite_file=""
arguments=("$@")
for ((index = 0; index < ${#arguments[@]}; index++)); do
  if [[ "${arguments[index]}" == "--suite" ]]; then
    if (( index + 1 >= ${#arguments[@]} )); then
      echo "--suite requires a suite file." >&2
      exit 2
    fi
    suite_file="${arguments[index + 1]}"
    break
  fi
done

if [[ -z "$suite_file" ]]; then
  echo "Runtime commands require --suite <suite.yaml>." >&2
  exit 2
fi

suite_root="$(cd -- "$(dirname -- "$suite_file")" && pwd -P)"
runtime_root="$suite_root/.eval-runtime"
runtime_cli="$runtime_root/cli.mjs"
if [[ -L "$runtime_root" || -L "$runtime_cli" || ! -f "$runtime_cli" ]]; then
  echo "Email eval runtime is not installed. Run: $plugin_root/email-evals.sh setup --root <suite-root>" >&2
  exit 2
fi

exec node "$runtime_cli" "$@"
