#!/usr/bin/env bash
# Regenerate the Python /v1 client base from the canonical api/openapi.yaml
# using OpenAPI Generator's `python` generator with the **httpx** library:
# async-native (matches the async-only Python decision) and httpx-based (the
# same HTTP client the hand-written layer uses — no second HTTP dependency).
# Output lands as the package e2a.v1.generated; the hand-written ergonomic layer wraps it.
# Pinned image tag → reproducible for the drift gate. Run via Docker (no Java).
#
# packageName=e2a.v1.generated so the generator's absolute imports (`from
# e2a.v1.generated ...`) match the package's final location. We generate to a
# temp dir and copy only the leaf package, so nothing pollutes the source root
# and the existing e2a/__init__.py and e2a/v1/__init__.py are never touched.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
TMP="$ROOT/sdks/python/.oag-tmp"
DEST="$ROOT/sdks/python/src/e2a/v1/generated"
CODEGEN_SPEC="$ROOT/sdks/python/.oag-openapi.yaml"
IMG="openapitools/openapi-generator-cli:v7.16.0"

trap 'rm -rf "$TMP"; rm -f "$CODEGEN_SPEC"' EXIT
rm -rf "$TMP"
go run "$ROOT/cmd/e2a-openapi-codegen-normalize" "$ROOT/api/openapi.yaml" "$CODEGEN_SPEC"
# Run as the invoking host user (not the container's default root) so the
# generated files + the .oag-tmp scratch dir are host-user-owned and removable
# on CI's non-root runner. HOME is a writable path for tools that expect it.
# (Docker Desktop/macOS maps ownership already, so this is a no-op there but
# required on Linux CI.)
# --name-mappings / --parameter-name-mappings: the wire field `from` is a
# Python keyword, which the generator would otherwise escape to the ugly
# `var_from`. Map it to the PEP-8-standard `from_` (trailing underscore), the
# same spelling the hand-written layer already uses for request-side params,
# so the SDK teaches exactly one spelling. Wire JSON stays `from` (the pydantic
# alias / query-param name are unchanged).
# The canonical 3.1 document is validated by Huma's golden test. This command
# consumes the deliberate 3.0 compatibility rewrite above, whose nullable-ref
# shape OpenAPI Generator's validator rejects even though its generator needs
# that exact shape; skip only this redundant generator-side validation.
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$ROOT:/work" "$IMG" generate \
  --skip-validate-spec \
  -i /work/sdks/python/.oag-openapi.yaml -g python \
  -o /work/sdks/python/.oag-tmp \
  --name-mappings from=from_ --parameter-name-mappings from=from_ \
  --additional-properties=packageName=e2a.v1.generated,library=httpx >/dev/null

rm -rf "$DEST"
cp -r "$TMP/e2a/v1/generated" "$DEST"
rm -rf "$TMP"

# Strip the generator's `*_validate_enum` validators from open response
# vocabularies so the client tolerates additive values. The post-processor
# preserves validators for the canonical, deliberately closed message
# lifecycle vocabulary.
python3 "$ROOT/sdks/python/scripts/strip-enum-validators.py" "$DEST"

# Normalize selected generator-known unused imports without making hand edits
# that regeneration would undo.
python3 "$ROOT/scripts/strip-unused-generated-imports.py" \
  re "$DEST/models/message_view.py" \
  field_validator "$DEST/models/message_view.py" \
  re "$DEST/models/dkim_result.py" \
  field_validator "$DEST/models/dkim_result.py" \
  re "$DEST/models/dmarc_result.py" \
  field_validator "$DEST/models/dmarc_result.py" \
  re "$DEST/models/spf_result.py" \
  field_validator "$DEST/models/spf_result.py" \
  re "$DEST/models/sending_ramp_view.py" \
  re "$DEST/models/domain_capabilities.py"

# OpenAPI Generator leaves multiple terminal newlines on standalone component
# models. Keep the dedicated push envelope deterministic for diff hygiene.
perl -0pi -e 's/\n+\z/\n/' "$DEST/models/event_envelope.py"

# Keep the expanded agents surface deterministic and diff-check clean.
perl -pi -e 's/[ \t]+$//' "$DEST/api/agents_api.py"

perl -0pi -e 's/\n+\z/\n/' \
  "$DEST/models/agent_suppression_added_data.py" \
  "$DEST/models/agent_suppression_view.py" \
  "$DEST/models/create_agent_suppression_request.py" \
  "$DEST/models/page_agent_suppression_view.py" \
  "$DEST/models/unsubscribe_options.py"

rm -f "$CODEGEN_SPEC"
trap - EXIT

echo "Python /v1 client base regenerated at sdks/python/src/e2a/v1/generated"
