#!/usr/bin/env bash
# Regenerate the TypeScript /v1 client base from the canonical api/openapi.yaml
# using OpenAPI Generator's `typescript` generator (NOT typescript-fetch, which
# fails TS2590 on wide models — see api-v1-redesign Slice 8). Output lands in
# sdks/typescript/src/v1/generated/; the hand-written ergonomic layer wraps it.
# Pinned image tag → reproducible for the drift gate. Run via Docker (no Java).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
OUT="$ROOT/sdks/typescript/src/v1/generated"
CODEGEN_SPEC="$ROOT/sdks/typescript/.oag-openapi.yaml"
IMG="openapitools/openapi-generator-cli:v7.16.0"

trap 'rm -f "$CODEGEN_SPEC"' EXIT
go run "$ROOT/cmd/e2a-openapi-codegen-normalize" "$ROOT/api/openapi.yaml" "$CODEGEN_SPEC"

# Clean prior output but keep .openapi-generator-ignore (suppresses scaffolding).
find "$OUT" -name '*.ts' -delete 2>/dev/null || true

# importFileExtension=.js → emit ESM-correct `.js` relative imports directly, so
#                      no extension post-process is needed (Node16/NodeNext).
# (Default platform: its fetch call uses a STANDARD RequestInit — no node-fetch
#  `agent`/`buffer` — so it is native-fetch-compatible once the polyfill import
#  is stripped below.)
# --name-mappings / --parameter-name-mappings: the wire field `from` is a
# reserved word for the generator, which would otherwise escape it to the
# private-looking `_from`. Map it to `from_` so both SDKs uniformly expose
# `from_` (matching Python's PEP-8 trailing-underscore convention). Wire JSON
# stays `from` (baseName / setQueryParam are unchanged).
# The canonical 3.1 document is validated by Huma's golden test. This command
# consumes the deliberate 3.0 compatibility rewrite above, whose nullable-ref
# shape OpenAPI Generator's validator rejects even though its generator needs
# that exact shape; skip only this redundant generator-side validation.
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$ROOT:/work" "$IMG" generate \
  --skip-validate-spec \
  -i /work/sdks/typescript/.oag-openapi.yaml -g typescript \
  -o /work/sdks/typescript/src/v1/generated \
  --name-mappings from=from_ --parameter-name-mappings from=from_ \
  --additional-properties=supportsES6=true,importFileExtension=.js >/dev/null

# Use the runtime's native global fetch (Node 18+, browsers, edge/Workers) — strip
# the generator's `whatwg-fetch` polyfill import so the SDK carries no fetch
# dependency and stays universal. Re-applied on every regen.
find "$OUT" -name '*.ts' -print0 | xargs -0 perl -i -ne \
  'print unless /^\s*import\s+["'"'"']whatwg-fetch["'"'"'];\s*$/'

# OpenAPI Generator imports every schema into its API wrapper variants and
# imports HttpFile into standalone models even when those symbols are unused.
# Normalize selected generator-known unused imports so static analysis and the
# generated-code freshness gate agree on the committed output.
python3 "$ROOT/scripts/strip-unused-generated-imports.py" \
  HttpFile "$OUT/models/SendingRampView.ts" \
  HttpFile "$OUT/models/DomainCapabilities.ts" \
  HttpFile "$OUT/models/DKIMResult.ts" \
  HttpFile "$OUT/models/DMARCResult.ts" \
  DKIMResultStatusEnum "$OUT/models/ObjectSerializer.ts" \
  DMARCResultAlignedByEnum "$OUT/models/ObjectSerializer.ts" \
  DMARCResultPolicyEnum "$OUT/models/ObjectSerializer.ts" \
  DMARCResultStatusEnum "$OUT/models/ObjectSerializer.ts" \
  MessageSummaryViewDirectionEnum "$OUT/models/ObjectSerializer.ts" \
  MessageViewDirectionEnum "$OUT/models/ObjectSerializer.ts" \
  ReviewViewDirectionEnum "$OUT/models/ObjectSerializer.ts" \
  SPFResultStatusEnum "$OUT/models/ObjectSerializer.ts" \
  Authentication "$OUT/types/PromiseAPI.ts" \
  DKIMResult "$OUT/types/PromiseAPI.ts" \
  SendingRampView "$OUT/types/PromiseAPI.ts" \
  DomainCapabilities "$OUT/types/PromiseAPI.ts" \
  Authentication "$OUT/types/ObjectParamAPI.ts" \
  DKIMResult "$OUT/types/ObjectParamAPI.ts" \
  SendingRampView "$OUT/types/ObjectParamAPI.ts" \
  DomainCapabilities "$OUT/types/ObjectParamAPI.ts" \
  Authentication "$OUT/types/ObservableAPI.ts" \
  DKIMResult "$OUT/types/ObservableAPI.ts" \
  SendingRampView "$OUT/types/ObservableAPI.ts" \
  DomainCapabilities "$OUT/types/ObservableAPI.ts"

# The upstream template emits a whitespace-only JSDoc line in standalone
# component models. Normalize the newly published docs-only envelope so the
# repository's `git diff --check` release gate remains clean and reproducible.
perl -pi -e 's/[ \t]+$//' \
  "$OUT/models/EventEnvelope.ts" \
  "$OUT/models/AgentSuppressionAddedData.ts" \
  "$OUT/models/AgentSuppressionView.ts" \
  "$OUT/models/CreateAgentSuppressionRequest.ts" \
  "$OUT/models/PageAgentSuppressionView.ts" \
  "$OUT/models/UnsubscribeOptions.ts" \
  "$OUT/apis/AgentsApi.ts" \
  "$OUT/types/ObjectParamAPI.ts"

perl -0pi -e 's/\n+\z/\n/' "$OUT/models/UnsubscribeOptions.ts"

rm -f "$CODEGEN_SPEC"
trap - EXIT

echo "TS /v1 client base regenerated at sdks/typescript/src/v1/generated"
