#!/usr/bin/env bash
#
# Fail if anything credential-shaped, or any agent/tool scratch directory, is
# tracked in this PUBLIC repository.
#
# WHY THIS EXISTS
# On 2026-08-08 three commits pushed a 48,147-file agent sandbox HOME into this
# repo. Inside it were a `.e2a/config.json` and a `bootstrap.log` containing a
# live-format `e2a_acct_` API key. The commits were later unreferenced, but that
# does NOT retract them: GitHub still serves them by SHA, and all forks share
# the object store. That key turned out to be minted against an ephemeral
# localhost instance with no production account, so it was inert — but nothing
# about the path that published it depended on that luck.
#
# GitHub push protection did not stop it, and could not have: it has no detector
# for e2a's own key format. This script is the repo-side backstop that does.
#
# It checks TRACKED CONTENT ONLY. .gitignore is the first line of defence; this
# is the second, for the case where something is force-added or an ignore rule
# is removed. Both are cheap; neither alone is sufficient.

set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
note() { printf '  %s\n' "$*"; }

# --- 1. First-party API keys -------------------------------------------------
# generateAPIKey (internal/identity/store.go) mints 32 crypto-random bytes as
# lowercase hex behind a scope prefix, so a real key is exactly
# e2a_(acct|agt)_ + 64 hex. Anchoring on the full 64 keeps synthetic fixtures
# like `e2a_agt_synthetic_do_not_leak` and `e2a_acct_should_not_escape` — which
# the test suite legitimately contains — from tripping this.
KEY_RE='e2a_(acct|agt)_[0-9a-f]{64}'
if hits=$(git grep -InE "$KEY_RE" -- . ':(exclude)scripts/check-no-committed-credentials.sh' 2>/dev/null); then
  echo "FAIL: live-format e2a API key(s) in tracked content:"
  printf '%s\n' "$hits" | sed -E 's/(e2a_(acct|agt)_)[0-9a-f]{64}/\1<REDACTED>/g' | sed 's/^/  /'
  note ""
  note "A key committed to a public repo cannot be retracted — unreferencing the"
  note "commit does not remove it, and forks keep their own copy. Rotate the key,"
  note "then remove it from tracked content."
  fail=1
fi

# --- 2. Credential-bearing file types ----------------------------------------
# *.pem / *.key matter most here: this is a mail service, and DKIM private keys
# are exactly this shape.
while IFS= read -r f; do
  [ -z "$f" ] && continue
  echo "FAIL: credential-bearing file is tracked: $f"
  fail=1
done < <(git ls-files -- '*.pem' '*.key' '*.p12' '*.pfx' '*.tfstate' '*.tfstate.*' \
  '*-credentials.json' '*_credentials.json' 'service-account*.json' 2>/dev/null)

# --- 3. Agent / tool scratch directories -------------------------------------
# `outputs/` is the exact directory that carried the 2026-08-08 sandbox HOME.
while IFS= read -r f; do
  [ -z "$f" ] && continue
  echo "FAIL: agent/tool scratch output is tracked: $f"
  note "These directories hold sandbox state and can contain credentials."
  fail=1
done < <(git ls-files -- 'outputs/*' '.playwright-mcp/*' 2>/dev/null | head -20)

# --- 4. Real .env files ------------------------------------------------------
while IFS= read -r f; do
  [ -z "$f" ] && continue
  case "$f" in
    *.example|*.example.*|*.sample) continue ;;
  esac
  echo "FAIL: non-example .env file is tracked: $f"
  fail=1
done < <(git ls-files -- '.env' '.env.*' '**/.env' '**/.env.*' 2>/dev/null)

if [ "$fail" -ne 0 ]; then
  echo ""
  echo "check-no-committed-credentials: FAILED"
  exit 1
fi

echo "check-no-committed-credentials: PASS (no credential-shaped or scratch content tracked)"
