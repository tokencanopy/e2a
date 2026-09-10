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
# shellcheck source=./lib.sh
. "${here}/lib.sh"

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

  fail=0
  echo "# installer Python interpreter resolution (Windows/Git Bash python3 shim):"
  ( fakebin=/tmp/tether-install-selftest-fakepy
    target=/tmp/tether-install-selftest-target
    rm -rf "$fakebin" "$target"
    mkdir -p "$fakebin"
    real_py="$(T_PYTHON='' t_python -c 'import sys; print(sys.executable)' 2>/dev/null)"
    [ -n "$real_py" ] || { echo "FAIL: could not resolve a real Python interpreter for the installer self-test"; exit 1; }
    printf '#!/usr/bin/env bash\nexit 49\n' > "$fakebin/python3"
    chmod +x "$fakebin/python3"
    printf '#!/usr/bin/env bash\nexec %q "$@"\n' "$real_py" > "$fakebin/python"
    chmod +x "$fakebin/python"
    PATH="$fakebin:$PATH" bash "${here}/install.sh" --to "$target"
    grep -Fq "$notify" "$target/.claude/settings.local.json" || {
      echo "FAIL: installer did not wire the hook when python3 is a broken shim but python works"
      exit 1
    }
    echo "ok: installer falls back to a working 'python' when python3 is a non-functional shim" ) || fail=1
  rm -rf /tmp/tether-install-selftest-fakepy /tmp/tether-install-selftest-target
  [ "$fail" = "0" ] || exit 1
  exit 0
fi

settings="${target}/.claude/settings.local.json"
mkdir -p "${target}/.claude"
[ -f "$settings" ] || echo '{}' > "$settings"

NOTIFY="$notify" MODE="$mode"
export NOTIFY MODE
t_python - "$settings" <<'PY'
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
