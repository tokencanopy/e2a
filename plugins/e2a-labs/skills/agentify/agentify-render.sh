#!/usr/bin/env bash
# agentify-render.sh — the deterministic scaffolder behind `/agentify`.
#
# Copies the framework templates into a target repo's real paths and renders
# autonomous-repo.config.yml from the adopter's answers. The interactive
# wizard (SKILL.md) gathers the answers, exports them as ANS_*, and runs this;
# keeping the mechanical part here makes it testable and reproducible.
#
#   agentify-render.sh --to <target-repo-root>
#   agentify-render.sh _selftest          # render into a temp dir + assert
#
# Answers (env, gathered by the wizard):
#   ANS_PRODUCT_NAME ANS_OWNER ANS_REPO ANS_MARKER ANS_REVIEWER_LOGIN
#   ANS_BOT_LOGIN ANS_SUPPORT_ADDRESS ANS_FIX_GATE_MODE ANS_APPROVER_ADDRESS
#   ANS_VERIFY_SETUP_SCRIPT
#
# Renders (idempotent — safe to re-run to update):
#   autonomous-repo.config.yml.tmpl -> <target>/autonomous-repo.config.yml
#   runtime-skill/**                -> <target>/.claude/skills/autonomous-repo/**
#   scripts/*.sh                    -> <target>/scripts/
#   workflows/*.yml.tmpl            -> <target>/.github/workflows/*.yml  (.tmpl stripped)
set -euo pipefail

BASE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATES="$BASE/templates"

_yaml_dq_content() {
  python3 -c 'import json,sys
s=json.dumps(sys.argv[1], ensure_ascii=False)
s=s[1:-1]
out=[]
for ch in s:
  cp=ord(ch)
  if cp in (0x85, 0x2028, 0x2029) or cp == 0x7f or 0x80 <= cp <= 0x84 or 0x86 <= cp <= 0x9f or cp in (0xfffe, 0xffff):
    out.append("\\x%02X" % cp if cp <= 0xff else "\\u%04X" % cp)
  else:
    out.append(ch)
print("".join(out))' "$1"
}

_sed_replacement() {
  _yaml_dq_content "$1" | sed -e 's/[\\&|]/\\&/g'
}

_single_line() {
  case "$2" in
    *$'\n'*|*$'\r'*) echo "agentify: $1 must be one line" >&2; exit 2 ;;
  esac
}

_validate_answers() {
  local name
  for name in ANS_OWNER ANS_REPO ANS_MARKER ANS_REVIEWER_LOGIN ANS_BOT_LOGIN \
    ANS_SUPPORT_ADDRESS ANS_FIX_GATE_MODE ANS_APPROVER_ADDRESS ANS_VERIFY_SETUP_SCRIPT; do
    _single_line "$name" "${!name:-}"
  done
  [[ "${ANS_OWNER:-}" =~ ^[A-Za-z0-9_.-]+$ ]] || { echo "agentify: invalid owner" >&2; exit 2; }
  [[ "${ANS_REPO:-}" =~ ^[A-Za-z0-9_.-]+$ ]] || { echo "agentify: invalid repo" >&2; exit 2; }
  [[ "${ANS_MARKER:-}" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || { echo "agentify: invalid marker" >&2; exit 2; }
  [[ "${ANS_FIX_GATE_MODE:-}" =~ ^(auto|hitl)$ ]] || { echo "agentify: fix gate must be auto or hitl" >&2; exit 2; }
}

render_config() {  # $1 = target root, $2 = force ("1" to overwrite)
  local out="$1/autonomous-repo.config.yml"
  # Re-runs UPDATE the code (scaffold) but must NOT clobber the adopter's
  # tuned config (always_hitl, the filled bot_login, etc.). Preserve an
  # existing config unless --force.
  if [ -f "$out" ] && [ "${2:-}" != "1" ]; then
    echo "agentify: $out exists — preserving your edits (pass --force to regenerate)."
    return 0
  fi
  sed \
    -e "s|{{PRODUCT_NAME}}|$(_sed_replacement "${ANS_PRODUCT_NAME:-}")|g" \
    -e "s|{{OWNER}}|$(_sed_replacement "${ANS_OWNER:-}")|g" \
    -e "s|{{REPO}}|$(_sed_replacement "${ANS_REPO:-}")|g" \
    -e "s|{{MARKER}}|$(_sed_replacement "${ANS_MARKER:-}")|g" \
    -e "s|{{REVIEWER_LOGIN}}|$(_sed_replacement "${ANS_REVIEWER_LOGIN:-}")|g" \
    -e "s|{{BOT_LOGIN}}|$(_sed_replacement "${ANS_BOT_LOGIN:-}")|g" \
    -e "s|{{SUPPORT_ADDRESS}}|$(_sed_replacement "${ANS_SUPPORT_ADDRESS:-}")|g" \
    -e "s|{{FIX_GATE_MODE}}|$(_sed_replacement "${ANS_FIX_GATE_MODE:-hitl}")|g" \
    -e "s|{{APPROVER_ADDRESS}}|$(_sed_replacement "${ANS_APPROVER_ADDRESS:-}")|g" \
    -e "s|{{VERIFY_SETUP_SCRIPT}}|$(_sed_replacement "${ANS_VERIFY_SETUP_SCRIPT:-}")|g" \
    "$TEMPLATES/autonomous-repo.config.yml.tmpl" > "$out"
  # Only real placeholders ({{UPPERCASE_IDENT}}) — not the literal "{{...}}"
  # in the template's explanatory comment.
  if grep -qE '\{\{[A-Z][A-Z_]*\}\}' "$out"; then
    echo "agentify-render.sh: unfilled placeholder(s) remain in $out:" >&2
    grep -nE '\{\{[A-Z][A-Z_]*\}\}' "$out" >&2; return 1
  fi
}

scaffold() {  # $1 = target root
  local t="$1"
  mkdir -p "$t/.claude/skills/autonomous-repo" "$t/scripts" "$t/.github/workflows"
  cp -R "$TEMPLATES/runtime-skill/." "$t/.claude/skills/autonomous-repo/"
  cp "$TEMPLATES"/scripts/*.sh "$t/scripts/"
  chmod +x "$t"/scripts/*.sh
  for f in "$TEMPLATES"/workflows/*.yml.tmpl; do
    cp "$f" "$t/.github/workflows/$(basename "$f" .tmpl)"
  done
}

# apply_addons: scaffold each opted-in addon (ANS_ADDONS, space-separated) to
# tools/<name>/ and append its setup.md. Addons are additive — the core loop
# runs without them.
apply_addons() {  # $1 = target root
  local t="$1" name src
  for name in ${ANS_ADDONS:-}; do
    # Reject anything that isn't a plain addon name — `..`/`/` would let the
    # cp escape tools/ (ANS_ADDONS is deployer-set, but fail safe anyway).
    case "$name" in
      ""|*[!a-z0-9-]*) echo "agentify: invalid addon name '$name' (skipped)" >&2; continue ;;
    esac
    src="$TEMPLATES/addons/$name"
    if [ ! -d "$src/files" ]; then
      echo "agentify: unknown addon '$name' (skipped)" >&2; continue
    fi
    mkdir -p "$t/tools/$name"
    cp -R "$src/files/." "$t/tools/$name/"
    if [ -f "$src/setup.md" ]; then
      { printf '\n## Addon: %s\n\n' "$name"; cat "$src/setup.md"; } >> "$t/AGENTIFY-ADDON-SETUP.md"
    fi
    echo "agentify: addon '$name' -> tools/$name/"
  done
}

if [ "${1:-}" = "_selftest" ]; then
  T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
  export ANS_PRODUCT_NAME="acme" ANS_OWNER="acme" ANS_REPO="widget" ANS_MARKER="acme-feedback" \
    ANS_REVIEWER_LOGIN="dev" ANS_BOT_LOGIN="acme-bot[bot]" ANS_SUPPORT_ADDRESS="support@acme.test" \
    ANS_FIX_GATE_MODE="hitl" ANS_APPROVER_ADDRESS="boss@acme.test" ANS_VERIFY_SETUP_SCRIPT="scripts/verify.sh"
  render_config "$T"; scaffold "$T"
  fail=0
  # re-run must preserve an existing config (update the code, not the config)
  echo 'tuned: yes' >> "$T/autonomous-repo.config.yml"
  render_config "$T"; grep -q 'tuned: yes' "$T/autonomous-repo.config.yml" || { echo "FAIL: re-run clobbered the config"; fail=1; }
  render_config "$T" 1; grep -q 'tuned: yes' "$T/autonomous-repo.config.yml" && { echo "FAIL: --force did not regenerate"; fail=1; }
  grep -q 'repo: "acme/widget"' "$T/autonomous-repo.config.yml" || { echo "FAIL: repo not rendered"; fail=1; }
  grep -q 'approver: "boss@acme.test"' "$T/autonomous-repo.config.yml" || { echo "FAIL: approver not rendered"; fail=1; }
  grep -qE '\{\{[A-Z][A-Z_]*\}\}' "$T/autonomous-repo.config.yml" && { echo "FAIL: placeholder left"; fail=1; }
  [ -f "$T/.github/workflows/feedback-triage.yml" ] || { echo "FAIL: triage workflow missing"; fail=1; }
  [ -e "$T/.github/workflows/feedback-triage.yml.tmpl" ] && { echo "FAIL: .tmpl not stripped"; fail=1; }
  for w in comms fix released; do [ -f "$T/.github/workflows/feedback-$w.yml" ] || { echo "FAIL: $w workflow missing"; fail=1; }; done
  [ -f "$T/.claude/skills/autonomous-repo/triage.md" ] || { echo "FAIL: runtime skill missing"; fail=1; }
  [ -f "$T/.claude/skills/autonomous-repo/templates/triage-ack.md" ] || { echo "FAIL: email templates missing"; fail=1; }
  [ -x "$T/scripts/ticket_card.sh" ] || { echo "FAIL: ticket_card.sh missing/not exec"; fail=1; }
  [ -x "$T/scripts/comms_send.sh" ] || { echo "FAIL: comms_send.sh missing/not exec"; fail=1; }
  # addons: none by default
  [ -e "$T/tools" ] && { echo "FAIL: tools/ created with no ANS_ADDONS"; fail=1; }
  # addons: opt in submit-feedback-mcp
  ANS_ADDONS="submit-feedback-mcp" apply_addons "$T"
  [ -f "$T/tools/submit-feedback-mcp/server.mjs" ] || { echo "FAIL: addon server.mjs not scaffolded"; fail=1; }
  [ -f "$T/tools/submit-feedback-mcp/bridge.mjs" ] || { echo "FAIL: addon bridge.mjs not scaffolded"; fail=1; }
  grep -q 'Addon: submit-feedback-mcp' "$T/AGENTIFY-ADDON-SETUP.md" || { echo "FAIL: addon setup not appended"; fail=1; }
  ANS_ADDONS="nope-addon" apply_addons "$T" 2>/dev/null; [ -e "$T/tools/nope-addon" ] && { echo "FAIL: unknown addon scaffolded"; fail=1; }
  # traversal name rejected (dest would be $T/tools/../evil = $T/evil)
  ANS_ADDONS="../evil" apply_addons "$T" 2>/dev/null; [ -e "$T/evil" ] && { echo "FAIL: traversal addon escaped tools/"; fail=1; }
  if [ "$fail" = 0 ]; then echo "agentify-render.sh selftest: OK"; else echo "agentify-render.sh selftest: FAILED"; exit 1; fi
  exit 0
fi

TARGET=""; FORCE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --to) TARGET="$2"; shift 2 ;;
    --force) FORCE="1"; shift ;;
    *) echo "agentify-render.sh: unknown arg '$1'" >&2; exit 2 ;;
  esac
done
[ -n "$TARGET" ] || { echo "agentify-render.sh: --to <target-repo-root> is required" >&2; exit 2; }
[ -d "$TEMPLATES" ] || { echo "agentify-render.sh: templates not found at $TEMPLATES" >&2; exit 2; }
_validate_answers
render_config "$TARGET" "$FORCE"
scaffold "$TARGET"
apply_addons "$TARGET"
echo "agentify: rendered into $TARGET (config + .claude/skills/autonomous-repo + scripts + .github/workflows)"
