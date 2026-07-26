#!/usr/bin/env bash
# lib.sh — shared config + e2a CLI helpers for the autopilot skill.
#
# Config comes from the environment, falling back to ~/.e2a-autopilot.env, then
# to ~/.e2a/config.json for the bare API credential (agent/allowlist/runtime
# settings are autopilot-specific and are NOT in config.json).
#
# Transport is the e2a CLI (@e2a/cli), NOT raw curl/WebSocket — everything
# goes through `a_cli`, which resolves $E2A_CLI → `e2a` on PATH (if new
# enough) → `npx -y @e2a/cli@^MIN`, mirroring the tether skill.

AUTOPILOT_MIN_CLI="2"

a_load_config() {
  local envk="${E2A_API_KEY:-}" enve="${E2A_AGENT_EMAIL:-}" envu="${E2A_URL:-}"

  if { [ -z "${E2A_API_KEY:-}" ] || [ -z "${E2A_AGENT_EMAIL:-}" ]; } && [ -f "${HOME}/.e2a-autopilot.env" ]; then
    # shellcheck disable=SC1091
    set -a; . "${HOME}/.e2a-autopilot.env"; set +a
  fi
  if [ -z "${E2A_API_KEY:-}" ] && [ -f "${HOME}/.e2a/config.json" ]; then
    eval "$(python3 -c 'import json,shlex,os
try:
  d=json.load(open(os.path.expanduser("~/.e2a/config.json")))
  if d.get("api_key"): print("export E2A_API_KEY="+shlex.quote(d["api_key"]))
  if d.get("api_url"): print("export E2A_URL="+shlex.quote(d["api_url"].rstrip("/")))
except Exception:pass')"
  fi

  [ -n "$envk" ] && E2A_API_KEY="$envk"
  [ -n "$enve" ] && E2A_AGENT_EMAIL="$enve"
  [ -n "$envu" ] && E2A_URL="$envu"

  # Treat unfilled autopilot.env.example placeholders as unset, so a user who
  # copied the template without editing gets a clear MISSING status.
  case "${E2A_API_KEY:-}" in *...*) E2A_API_KEY="";; esac
  case "${E2A_AGENT_EMAIL:-}" in "you@example.com") E2A_AGENT_EMAIL="";; esac

  export E2A_API_KEY E2A_AGENT_EMAIL
  [ -n "${E2A_URL:-}" ] && export E2A_URL

  E2A_AUTOPILOT_PORT="${E2A_AUTOPILOT_PORT:-8991}"
  E2A_AUTOPILOT_RUNTIME="${E2A_AUTOPILOT_RUNTIME:-claude}"
  export E2A_AUTOPILOT_PORT E2A_AUTOPILOT_RUNTIME
}

a_config_ok() {
  a_load_config
  [ -n "${E2A_API_KEY:-}" ] && [ -n "${E2A_AGENT_EMAIL:-}" ] \
    && [ -n "${E2A_AUTOPILOT_FORWARD_TOKEN:-}" ] \
    && [ -n "${E2A_AUTOPILOT_ALLOWLIST:-}" ] \
    && [ -n "${E2A_AUTOPILOT_HUMAN:-}" ] \
    && [ -n "${E2A_AUTOPILOT_RUNTIME_BIN:-}" ] \
    && [ -n "${E2A_AUTOPILOT_WORKDIR:-}" ]
}

# Resolve the e2a CLI invocation: $E2A_CLI override → `e2a` on PATH (version
# gated to the pinned major) → `npx -y @e2a/cli@^MIN` as a last resort.
a_cli_bin() {
  if [ -n "${E2A_CLI:-}" ]; then
    echo "$E2A_CLI"
    return
  fi
  if command -v e2a >/dev/null 2>&1; then
    local ver major
    ver="$(e2a --version 2>/dev/null | awk '{print $2}')"
    major="${ver%%.*}"
    if [ "$major" = "$AUTOPILOT_MIN_CLI" ] || [ "${major:-0}" -gt "$AUTOPILOT_MIN_CLI" ] 2>/dev/null; then
      echo "e2a"
      return
    fi
  fi
  echo "npx -y @e2a/cli@^${AUTOPILOT_MIN_CLI}"
}

a_cli() {
  # shellcheck disable=SC2046
  $(a_cli_bin) "$@"
}
