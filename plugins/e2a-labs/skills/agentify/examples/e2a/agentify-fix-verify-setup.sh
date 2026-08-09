#!/usr/bin/env bash
# agentify-fix-verify-setup.sh — e2a's fix-lane verification bootstrap.
#
# Product-specific (it lives in the target repo and is named by the config
# key `verify_setup_script`, so the fix-lane workflow YAML stays neutral).
# Stands up the RUNNING stack the fix agent verifies against, with THROWAWAY
# credentials only — nothing here can reach production.
#
# For e2a: Postgres on :5433 (the dev port, per CLAUDE.md) via docker
# compose, schema applied, test DB URL exported. The fix agent then runs
# `make test-unit` / the package tests against it.
set -euo pipefail

# Local Postgres (+ Mailpit) the way dev runs it.
make docker-up

# Wait for Postgres to accept connections (compose --wait isn't always set).
for i in $(seq 1 30); do
  if docker compose exec -T postgres pg_isready -U e2a >/dev/null 2>&1; then break; fi
  sleep 1
done

# The e2a binary auto-applies embedded migrations at startup, but the test
# DB needs the schema for direct-SQL package tests:
make migrate || true

# Throwaway test DB URL (matches CLAUDE.md's E2A_TEST_DATABASE_URL); worthless
# outside this run. Exported to the agent's environment.
{
  echo "E2A_TEST_DATABASE_URL=postgres://e2a:e2a@localhost:5433/e2a_test?sslmode=disable"
} >> "${GITHUB_ENV:-/dev/stdout}"

echo "verify stack up: Postgres :5433 (throwaway), schema applied."
