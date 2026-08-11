#!/usr/bin/env bash
# install.sh — optional: wire the Notification hook into a repo's
# .claude/settings.local.json. Sending/receiving need NO hook (the agent calls
# tether.sh directly), so this is only for the "agent is blocked, email me"
# alert. Idempotent.
#
#   install.sh [--to <repo-root>]      # default: current directory
#   install.sh --uninstall [--to <repo-root>]
#   install.sh _selftest
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
notify="${here}/hooks/tether-notify.sh"

target="$PWD"; mode="install"
while [ $# -gt 0 ]; do
  case "$1" in
    --to) target="$2"; shift 2;;
    --uninstall) mode="uninstall"; shift;;
    _selftest) mode="selftest"; shift;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

if [ "$mode" = "selftest" ]; then
  bash "${here}/tether.sh" _selftest
  printf '{"message":"x"}' | env -u E2A_API_KEY -u E2A_AGENT_EMAIL HOME=/nonexistent \
    TETHER_STATE=/tmp/tether-notify-selftest.json bash "$notify" && echo "# notify hook OK (exit 0)"
  rm -f /tmp/tether-notify-selftest.json
  exit 0
fi

settings="${target}/.claude/settings.local.json"
mkdir -p "${target}/.claude"
[ -f "$settings" ] || echo '{}' > "$settings"

NOTIFY="$notify" MODE="$mode" python3 - "$settings" <<'PY'
import json,os,sys
f=sys.argv[1];d=json.load(open(f));notify=os.environ["NOTIFY"];mode=os.environ["MODE"]
hooks=d.setdefault("hooks",{})
hooks["Notification"]=[g for g in hooks.get("Notification",[])
                       if not any(h.get("command")==notify for h in g.get("hooks",[]))]
if not hooks["Notification"]:del hooks["Notification"]
if mode=="install":
    hooks.setdefault("Notification",[]).append({"hooks":[{"type":"command","command":notify}]})
if not hooks:d.pop("hooks",None)
json.dump(d,open(f,"w"),indent=2)
print(("installed" if mode=="install" else "uninstalled")+" Notification hook → "+f)
PY
