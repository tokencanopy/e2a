#!/usr/bin/env bash
# runner.sh <lane> <fixture-dir> — drive a lane's REAL prompt against a mocked
# world (stub e2a MCP + fake gh/scripts) and run the fixture's assert.sh on
# the recorded action log. Token-gated: SKIPs cleanly without a model token.
set -uo pipefail
LANE="$1"; FIXTURE="$2"
HARNESS="$(cd "$(dirname "$0")" && pwd)"          # .../agentify/test/fixtures/harness
AGENTIFY="$(cd "$HARNESS/../../.." && pwd)"        # .../agentify (harness → fixtures → test → agentify)
NAME="$LANE/$(basename "$FIXTURE")"

if [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}${ANTHROPIC_API_KEY:-}" ] && [ "${AGENTIFY_FIXTURES_FORCE:-}" != "1" ]; then
  echo "SKIP $NAME (no model token — set CLAUDE_CODE_OAUTH_TOKEN, or AGENTIFY_FIXTURES_FORCE=1 when the claude CLI is logged in locally)"; exit 0
fi
command -v claude >/dev/null || { echo "SKIP $NAME (claude CLI not installed)"; exit 0; }

SBX="$(mktemp -d)"; trap 'rm -rf "$SBX"' EXIT
export ACTION_LOG="$SBX/actions.log"; : > "$ACTION_LOG"
export FIXTURE_DIR="$FIXTURE"

# Sandbox: the real runtime skill + config, mock scripts + a fake gh on PATH.
mkdir -p "$SBX/.claude/skills/autonomous-repo" "$SBX/scripts" "$SBX/bin"
cp -R "$AGENTIFY/templates/runtime-skill/." "$SBX/.claude/skills/autonomous-repo/"
cp "$AGENTIFY/examples/e2a/autonomous-repo.config.yml" "$SBX/autonomous-repo.config.yml"
cp "$HARNESS/mock-ticket_card.sh" "$SBX/scripts/ticket_card.sh"
cp "$HARNESS/mock-comms_send.sh" "$SBX/scripts/comms_send.sh"
cp "$HARNESS/mock-gh" "$SBX/bin/gh"
chmod +x "$SBX/scripts/"*.sh "$SBX/bin/gh"

# Fail loudly if the sandbox is incomplete — running the agent WITHOUT its real
# procedure/config would silently test improvisation, not the prompt.
[ -f "$SBX/.claude/skills/autonomous-repo/triage.md" ] && [ -f "$SBX/autonomous-repo.config.yml" ] \
  || { echo "ERROR $NAME: sandbox setup failed (runtime skill or config missing)"; exit 1; }

cat > "$SBX/mcp.json" <<JSON
{ "mcpServers": { "e2a": { "command": "node", "args": ["$HARNESS/mock-mcp.mjs"],
  "env": { "FIXTURE_DIR": "$FIXTURE", "ACTION_LOG": "$ACTION_LOG" } } } }
JSON

( cd "$SBX" && PATH="$SBX/bin:$PATH" claude -p "$(cat "$HARNESS/prompts/$LANE.txt")" \
    --mcp-config "$SBX/mcp.json" \
    --permission-mode bypassPermissions \
    --allowedTools "Bash(scripts/ticket_card.sh:*)" "Bash(gh issue:*)" "Read" \
      "mcp__e2a__list_messages" "mcp__e2a__get_message" "mcp__e2a__get_conversation" \
    --max-turns 40 >/dev/null 2>&1 ) || true

echo "--- $NAME action log ---"; cat "$ACTION_LOG"
echo "--- asserting ---"
bash "$FIXTURE/assert.sh" "$ACTION_LOG"
