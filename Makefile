.PHONY: build run test test-unit test-integration test-e2e cover cover-check clean docker-up docker-down migrate spec spec-check openapi-compat-check openapi-compat-test generate generate-check generate-sdk generate-sdk-check generate-sdk-ts generate-sdk-py

# OpenAPI Generator for the /v1 SDK base. Pinned to a released tag (never
# :latest/SNAPSHOT) so output is reproducible for the drift gate. Run via
# Docker — no local Java needed.
OAG_IMAGE := openapitools/openapi-generator-cli:v7.16.0

# Base database URL for every DB-backed target. `?=` so an exported
# E2A_TEST_DATABASE_URL WINS: these recipes used to pin it inline, which
# overrides the caller's environment, so AGENTS.md's "give each runner its own
# base database" could not actually be followed through make. Concurrent
# sessions then shared one base and corrupted each other — an unmerged branch's
# migration in a shared database produced ~40 spurious failures and deadlocks
# that looked exactly like real regressions.
#
# The harness derives per-workspace + per-package names beneath this base
# (internal/testutil.TestDBURL), so overriding is rarely needed now; it stays
# available for pointing a run at an entirely separate server.
E2A_TEST_DATABASE_URL ?= postgres://e2a:e2a@localhost:5433/e2a_test?sslmode=disable
export E2A_TEST_DATABASE_URL

build:
	go build -o bin/e2a ./cmd/e2a

run: build
	./bin/e2a -config config.yaml

# -p 4 everywhere a DB is involved: the testutil harness gives each package
# its own database (<base>_pkg_<package>, self-provisioned on first run), so
# packages can run in parallel without truncating each other. Capped at 4 —
# not the core count — so N × pgxpool connections stay under Postgres's
# default max_connections=100 on many-core dev machines.
test:
	go test -tags integration -p 4 ./...

test-unit:
	go test -short ./internal/outbound/ ./internal/relay/ ./internal/config/ ./internal/webhook/ ./internal/approvaltoken/ ./internal/unsubscribe/ ./internal/limits/ ./internal/httpapi/ ./internal/ratelimit/

test-integration:
	go test -p 4 ./internal/identity/ ./internal/agent/ ./internal/hitlworker/ ./internal/hitlnotify/ ./internal/limits/ ./internal/relay/ ./internal/sendramp/

test-e2e:
	@packages="$$(find ./cmd ./internal ./tests -name '*_test.go' -exec grep -l '^//go:build integration$$' {} + | xargs -n 1 dirname | sort -u)"; \
	test -n "$$packages"; \
	go test -tags integration -p 4 $$packages

# cover writes a coverage profile across the internal packages (needs Postgres
# on :5433, like `make test`; per-package DBs make the -p 4 parallel run safe).
# cover-check enforces the per-package floors in .testcoverage.yml. CI runs the
# same gate via the vladopajic/go-test-coverage action.
GO_TEST_COVERAGE_VERSION ?= v2.14.3
cover:
	go test -p 4 -covermode=atomic -coverprofile=cover.out ./internal/...

cover-check: cover
	go run github.com/vladopajic/go-test-coverage/v2@$(GO_TEST_COVERAGE_VERSION) --config=.testcoverage.yml

clean:
	rm -rf bin/

docker-up:
	docker compose up -d

docker-down:
	docker compose down

migrate:
	@for f in migrations/*.sql; do \
		echo "Applying $$f ..."; \
		psql "postgres://e2a:e2a@localhost:5433/e2a?sslmode=disable" -f "$$f"; \
	done

# spec regenerates the /v1 OpenAPI 3.1 document (api/openapi.yaml) directly
# from the live Huma handlers — the single source of truth for SDK codegen and
# the rendered API reference. (The dashboard's API-reference page, web/public
# /scalar.html, fetches the spec live from /v1/openapi.yaml at request time —
# there is no static web/public/openapi.yaml copy. The old swag-annotation
# pipeline has been removed.)
spec:
	go test ./internal/httpapi/ -run TestSpecGoldenNoDrift -update-spec -count=1
	@echo "==> Regenerated api/openapi.yaml from the /v1 handlers"

# spec-check is the contract-drift gate: fails if api/openapi.yaml lags the
# handlers. Runs in CI as part of the normal test suite (TestSpecGoldenNoDrift);
# this is the explicit entrypoint.
spec-check:
	go test ./internal/httpapi/ -run TestSpecGoldenNoDrift -count=1

# Compare the freshly verified Huma contract with another Git revision or file.
# Examples:
#   make openapi-compat-check
#   make openapi-compat-check OPENAPI_BASE=v1.1.0:api/openapi.yaml
OPENAPI_BASE ?= origin/main:api/openapi.yaml
OPENAPI_REVISION ?= api/openapi.yaml
openapi-compat-check: spec-check
	bash scripts/check-openapi-compat.sh "$(OPENAPI_BASE)" "$(OPENAPI_REVISION)"

openapi-compat-test:
	bash scripts/test-openapi-compat.sh

generate: spec generate-sdk

# generate-sdk regenerates both /v1 SDK client bases from the canonical
# api/openapi.yaml via OpenAPI Generator (the `generate-sdk-ts` /
# `generate-sdk-py` targets below). The retired swag + datamodel-codegen
# pipeline (Swagger 2.0 → OpenAPI 3.0 → openapi-typescript / datamodel-codegen)
# has been removed; the hand-written ergonomic layer wraps the OAG output.
generate-sdk: generate-sdk-ts generate-sdk-py

generate-check: spec-check generate-sdk-check

generate-sdk-check: generate-sdk
	@echo "==> Testing generated import normalization"
	python3 -m unittest scripts/test_strip_unused_generated_imports.py
	@echo "==> Testing optional-header-param guard normalization"
	python3 -m unittest scripts/test_guard_optional_header_params.py
	@echo "==> Checking generated code is up to date"
	git diff --exit-code sdks/typescript/src/v1/generated/ sdks/python/src/e2a/v1/generated/

# generate-sdk-ts regenerates the TypeScript /v1 client base from the canonical
# api/openapi.yaml using OpenAPI Generator's `typescript` generator (NOT
# typescript-fetch, which fails TS2590 on wide models — see Slice 8). Output
# lands in sdks/typescript/src/v1/generated/; the hand-written ergonomic layer
# wraps it. Package scaffolding is suppressed via .openapi-generator-ignore.
generate-sdk-ts:
	@echo "==> Generating TS /v1 client base via $(OAG_IMAGE)"
	bash sdks/typescript/scripts/generate-oag.sh

# generate-sdk-py regenerates the Python /v1 client base (package e2a.v1.generated)
# from api/openapi.yaml using OpenAPI Generator's `python` generator with the
# httpx library (async-native, matches async-only Python + the hand-written
# layer's HTTP client). Output is the leaf package only; see the script.
generate-sdk-py:
	@echo "==> Generating Python /v1 client base via $(OAG_IMAGE)"
	bash sdks/python/scripts/generate-oag.sh
