#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

conflicts="$(git grep -n -E '^(<<<<<<< |=======|>>>>>>> )' -- . || true)"
if [[ -n "$conflicts" ]]; then
  echo "unresolved merge-conflict markers found:" >&2
  echo "$conflicts" >&2
  exit 1
fi

node -e '
  const fs = require("node:fs");
  for (const file of process.argv.slice(1)) {
    JSON.parse(fs.readFileSync(file, "utf8"));
  }
' package-lock.json web/package-lock.json

node scripts/sync-agent-docs.mjs --check
node scripts/check-sdk-example-contracts.mjs

rollback_boundary="$(tr -d '[:space:]' < ROLLBACK_UNSAFE_BEFORE)"
if ! [[ "$rollback_boundary" =~ ^(none|[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.]+)?)$ ]]; then
  echo "ROLLBACK_UNSAFE_BEFORE must be 'none' or an exact semver" >&2
  exit 1
fi
if ! grep -Fq 'org.e2a.rollback-unsafe-before=${{ steps.rollback-boundary.outputs.value }}' \
     .github/workflows/build-image.yml; then
  echo "server image build must publish the rollback compatibility boundary" >&2
  exit 1
fi

legacy_agent_calls="$(git grep -n -E 'webhooks\.(fetch_message|fetchMessage)|client\.messages\.reply|messages\.reply' -- \
  examples/agent-framework-webhooks/python/agent_webhooks \
  examples/agent-framework-webhooks/typescript/src \
  examples/adk-cloud-webhook/agent.py \
  examples/adk-cloud-webhook/delivery_state.py \
  examples/adk-cloud-webhook/webhook.py || true)"
if [[ -n "$legacy_agent_calls" ]]; then
  echo "agent webhook examples must use the ergonomic inbound facade:" >&2
  echo "$legacy_agent_calls" >&2
  exit 1
fi

python3 -m unittest discover -s mcp/examples -p 'test_*.py'

echo "repository text integrity checks passed"
