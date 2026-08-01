# Canonical API Contract Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Freeze the pre-GA REST contract for validation fields, response headers, error codes, trash reads, and beta operations.

**Architecture:** Keep Go handler metadata as the OpenAPI source of truth. Add one post-registration response-header normalizer beside the existing evolution-stance pass, extend existing AST/spec drift tests instead of introducing a second generator, and document runtime behavior without changing endpoint identities or success semantics.

**Tech Stack:** Go 1.25+, Huma v2, Chi, OpenAPI 3.1 YAML, TypeScript/OpenAPI Generator, Python/Pydantic/OpenAPI Generator, Markdown contract documentation.

---

## File map

- `internal/httpapi/errors.go`: required validation location and 503 retry metadata.
- `internal/httpapi/account.go`: attach the frozen five-second retry hint to `limits_unavailable`.
- `internal/httpapi/contract_headers.go`: new OpenAPI response-header normalizer.
- `internal/httpapi/httpapi.go`: invoke the normalizer after operation registration.
- `internal/httpapi/contract_headers_test.go`: header-component, response-reference, and live-wire tests.
- `internal/httpapi/errorcode_vocab_test.go`: structured error catalog and four-way drift checks.
- `internal/httpapi/errors_test.go`: validation-detail serialization tests.
- `internal/httpapi/messages.go`: explicit direct-read trash prose.
- `internal/httpapi/messages_trash_test.go`: new trash visibility regression tests.
- `internal/httpapi/stability_test.go`: exact OpenAPI/docs experimental-operation comparison.
- `docs/api.md`: complete error and stability tables plus trash GET semantics.
- `api/openapi.yaml`: regenerated Huma contract.
- `sdks/typescript/src/v1/generated/`: regenerated required `FieldError.location` model.
- `sdks/typescript/test/v1/client.types.ts`: TypeScript compile-time contract assertion.
- `sdks/python/src/e2a/v1/generated/`: regenerated required `FieldError.location` model.
- `sdks/python/tests/test_v1_client.py`: Python model contract assertion.

### Task 1: Require validation error locations

**Files:**
- Modify: `internal/httpapi/errors.go:63-79`
- Modify: `internal/httpapi/errors_test.go`

- [ ] **Step 1: Write failing JSON-shape tests**

Add tests that construct both a Huma field detail and a plain request-wide
detail, marshal the resulting `ErrorEnvelope`, and assert `location` exists:

```go
func TestValidationFieldLocationAlwaysSerializes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"field", &huma.ErrorDetail{Location: "body.to", Message: "required"}, "body.to"},
		{"request-wide", errors.New("invalid request"), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := humaErrorConstructor(http.StatusUnprocessableEntity, "validation failed", tc.err).(*ErrorEnvelope)
			b, err := json.Marshal(env)
			if err != nil { t.Fatal(err) }
			var got map[string]any
			if err := json.Unmarshal(b, &got); err != nil { t.Fatal(err) }
			fields := got["error"].(map[string]any)["details"].(map[string]any)["fields"].([]any)
			field := fields[0].(map[string]any)
			if location, ok := field["location"]; !ok || location != tc.want {
				t.Fatalf("location = %#v, present=%v; want %q", location, ok, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the focused test and verify failure**

Run: `go test ./internal/httpapi -run TestValidationFieldLocationAlwaysSerializes -count=1`

Expected: request-wide case fails because `location` is omitted.

- [ ] **Step 3: Make location required on the wire**

Change the field tag only:

```go
type FieldError struct {
	Location string `json:"location" doc:"Path-like pointer to the offending field, prefixed with the request part it came from, e.g. body.events, body.items[3].tags, query.limit, path.id. Empty when the failure is not tied to a single field."`
	Message  string `json:"message" doc:"Human-readable reason this field is invalid."`
}
```

- [ ] **Step 4: Run validation tests**

Run: `go test ./internal/httpapi -run 'TestValidation|TestHumaError' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi/errors.go internal/httpapi/errors_test.go
git commit -m "fix(api): require validation error locations"
```

### Task 2: Publish and enforce response headers

**Files:**
- Create: `internal/httpapi/contract_headers.go`
- Create: `internal/httpapi/contract_headers_test.go`
- Modify: `internal/httpapi/httpapi.go:408-421`
- Modify: `internal/httpapi/account.go:243-258`
- Modify: `internal/httpapi/errors.go:130-196`

- [ ] **Step 1: Write failing OpenAPI header tests**

Create tests that build `testServer(t).API.OpenAPI()` and assert:

```go
func TestOpenAPIResponseHeaderContract(t *testing.T) {
	oapi := testServer(t).API.OpenAPI()
	for _, name := range []string{"XRequestID", "RetryAfter", "RateLimitLimit", "RateLimitRemaining", "RateLimitReset"} {
		if oapi.Components.Headers[name] == nil {
			t.Errorf("missing components.headers.%s", name)
		}
	}
	forEachOperation(oapi, func(op *huma.Operation) {
		for status, response := range op.Responses {
			h := response.Headers["X-Request-Id"]
			if h == nil || h.Ref != "#/components/headers/XRequestID" {
				t.Errorf("%s %s missing X-Request-Id", op.OperationID, status)
			}
			if status == "429" || status == "503" {
				retry := response.Headers["Retry-After"]
				if retry == nil || retry.Ref != "#/components/headers/RetryAfter" {
					t.Errorf("%s %s missing Retry-After", op.OperationID, status)
				}
			}
			for name := range response.Headers {
				if strings.HasPrefix(strings.ToLower(name), "x-ratelimit-") {
					t.Errorf("%s %s declares legacy header %s", op.OperationID, status, name)
				}
			}
		}
	})
}
```

Add a second test for `pollLimitedOps` and `createAgent` that expects the three
standard `RateLimit-*` references on all existing responses.

- [ ] **Step 2: Run the OpenAPI tests and verify failure**

Run: `go test ./internal/httpapi -run TestOpenAPIResponseHeaderContract -count=1`

Expected: FAIL because header components and references do not exist.

- [ ] **Step 3: Implement the central normalizer**

Create header components and a reference helper:

```go
const (
	headerXRequestID        = "XRequestID"
	headerRetryAfter       = "RetryAfter"
	headerRateLimitLimit   = "RateLimitLimit"
	headerRateLimitRemain  = "RateLimitRemaining"
	headerRateLimitReset   = "RateLimitReset"
)

func headerRef(name string) *huma.Param {
	return &huma.Param{Ref: "#/components/headers/" + name}
}

func (s *Server) applyResponseHeaderContract() {
	oapi := s.API.OpenAPI()
	minRetryAfter := float64(1)
	if oapi.Components.Headers == nil { oapi.Components.Headers = map[string]*huma.Header{} }
	oapi.Components.Headers[headerXRequestID] = &huma.Header{Description: "Always present request correlation id.", Schema: &huma.Schema{Type: huma.TypeString}}
	oapi.Components.Headers[headerRetryAfter] = &huma.Header{Description: "Positive integer seconds before retrying a transient 429 or 503.", Schema: &huma.Schema{Type: huma.TypeInteger, Minimum: &minRetryAfter}}
	oapi.Components.Headers[headerRateLimitLimit] = &huma.Header{Description: "Request quota for the current limiter window.", Schema: &huma.Schema{Type: huma.TypeInteger}}
	oapi.Components.Headers[headerRateLimitRemain] = &huma.Header{Description: "Requests remaining in the current limiter window.", Schema: &huma.Schema{Type: huma.TypeInteger}}
	oapi.Components.Headers[headerRateLimitReset] = &huma.Header{Description: "Seconds until the current limiter window resets.", Schema: &huma.Schema{Type: huma.TypeInteger}}
	forEachOperation(oapi, func(op *huma.Operation) {
		for status, response := range op.Responses {
			if response.Headers == nil { response.Headers = map[string]*huma.Param{} }
			response.Headers["X-Request-Id"] = headerRef(headerXRequestID)
			if status == "429" || status == "503" {
				response.Headers["Retry-After"] = headerRef(headerRetryAfter)
			}
			if pollLimitedOps[op.OperationID] || op.OperationID == "createAgent" {
				response.Headers["RateLimit-Limit"] = headerRef(headerRateLimitLimit)
				response.Headers["RateLimit-Remaining"] = headerRef(headerRateLimitRemain)
				response.Headers["RateLimit-Reset"] = headerRef(headerRateLimitReset)
			}
		}
	})
}
```

Call `s.applyResponseHeaderContract()` after `s.applyEvolutionStance()` in
`New`.

- [ ] **Step 4: Add a failing live 503 retry test**

Construct a server with `GetLimits: nil`, call `GET /v1/account`, and assert:

```go
if resp.StatusCode != http.StatusServiceUnavailable { t.Fatalf(...) }
if got := resp.Header.Get("Retry-After"); got != "5" { t.Fatalf(...) }
if details["retry_after_seconds"] != float64(5) { t.Fatalf(...) }
```

Run: `go test ./internal/httpapi -run TestLimitsUnavailableRetryContract -count=1`

Expected: FAIL because the current 503 has no retry header/details.

- [ ] **Step 5: Implement the 503 retry signal**

Add a reusable details type and apply the existing retry-header mechanism:

```go
type RetryAfterDetails struct {
	RetryAfterSeconds int `json:"retry_after_seconds" minimum:"1"`
}

const limitsUnavailableRetrySeconds = 5

return nil, NewError(http.StatusServiceUnavailable, "limits_unavailable", "limits subsystem not configured").
	WithDetails(RetryAfterDetails{RetryAfterSeconds: limitsUnavailableRetrySeconds}).
	WithRetryAfter(limitsUnavailableRetrySeconds)
```

Also add an explicit `503` response to the `getAccount` Huma operation using
`ErrorEnvelope`; the post-registration normalizer then attaches the
`Retry-After` header reference to that real status response. Keep the existing
`default` response for other errors.

- [ ] **Step 6: Add live request-id and legacy-header absence cases**

Extend tests to cover a successful Huma call, an error call, and a raw
attachment download. Assert every response has `X-Request-Id`, error body and
header IDs match, and no header name begins with `X-RateLimit-` case-
insensitively.

- [ ] **Step 7: Run header and rate-limit tests**

Run: `go test ./internal/httpapi -run 'TestOpenAPIResponseHeader|TestLimitsUnavailableRetry|Test.*RequestID|Test.*RateLimit' -count=1`

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/httpapi/contract_headers.go internal/httpapi/contract_headers_test.go internal/httpapi/httpapi.go internal/httpapi/account.go internal/httpapi/errors.go
git commit -m "feat(api): publish response header contract"
```

### Task 3: Make the error vocabulary four-way consistent

**Files:**
- Modify: `internal/httpapi/errorcode_vocab_test.go`
- Modify: `docs/api.md:85-151`

- [ ] **Step 1: Replace string-only catalog entries with metadata**

Define test metadata:

```go
type errorCodeContract struct {
	Code       string
	Statuses   []string
	Family     string
	Retryable  bool
	FallbackOnly bool
}
```

Populate every existing code with the status text used by `docs/api.md`, using
multiple statuses for `invalid_request` and `domain_not_verified`.

- [ ] **Step 2: Write a failing Markdown-table drift test**

Read `../../api.md`, restrict parsing to the `## Error codes` section,
parse rows whose first cell contains backticked codes, split grouped code cells
on commas, and compare code/status pairs with the catalog. Report missing,
extra, and mismatched entries separately.

Run: `go test ./internal/httpapi -run TestDocsErrorCodeTableMatchesCatalog -count=1`

Expected: FAIL listing the five undocumented state codes.

- [ ] **Step 3: Add missing documentation rows**

Add:

```markdown
| `confirmation_required` | 400 | The irreversible operation requires the documented confirmation literal. |
| `address_in_trash` | 409 | Agent creation conflicts with a soft-deleted address that may be restored or purged. |
| `message_held` | 409 | The message is held for review and cannot enter the requested lifecycle transition. |
| `not_in_trash` | 409 | Restore or permanent deletion targeted a resource that is not in trash. |
| `send_in_progress` | 409 | Permanent deletion is blocked while outbound submission is in progress. |
```

- [ ] **Step 4: Keep AST and OpenAPI prose checks on the structured catalog**

Update the existing emitter and `ErrorBody.Code` tests to derive their desired
sets from the metadata. Add a status-pair assertion for literal `NewError`,
`WriteError`, `writeRawError`, and `OutboundError` construction sites where both
status and code are statically visible.

- [ ] **Step 5: Run vocabulary tests**

Run: `go test ./internal/httpapi -run 'Test.*ErrorCode.*Catalog|TestDocsErrorCodeTableMatchesCatalog' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/httpapi/errorcode_vocab_test.go docs/api.md
git commit -m "docs(api): synchronize error code catalog"
```

### Task 4: Freeze trash direct-read semantics

**Files:**
- Modify: `internal/httpapi/messages.go:418-440`
- Modify: `docs/api.md:290-310`
- Create: `internal/httpapi/messages_trash_test.go`

- [ ] **Step 1: Add a failing contract-prose test**

Inspect `getMessage` in the emitted spec and require its description to mention
`deleted_at`, direct GET, and exclusion from normal lists/reply targets.

Run: `go test ./internal/httpapi -run TestGetMessageDocumentsTrashVisibility -count=1`

Expected: FAIL against the current short description.

- [ ] **Step 2: Add an end-to-end handler visibility test**

Using the existing trash test store, soft-delete a message and assert:

- ordinary list excludes it;
- `deleted=true` list includes it;
- direct GET returns 200 with non-empty `deleted_at`;
- reply and forward return `404 not_found` or the store's canonical non-
  repliable response, never send.

- [ ] **Step 3: Update handler and public prose**

Use the same wording in the operation description and `docs/api.md`:

```text
A trashed message is excluded from ordinary lists, conversations, and
reply/forward targets, but direct GET remains available for recovery and audit
until permanent deletion or retention purge; trashed responses carry deleted_at.
```

- [ ] **Step 4: Run trash tests**

Run: `go test ./internal/httpapi -run 'Test.*Trash|TestGetMessageDocumentsTrashVisibility' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi/messages.go internal/httpapi/messages_trash_test.go docs/api.md
git commit -m "docs(api): freeze trashed message reads"
```

### Task 5: Publish the beta operation inventory

**Files:**
- Modify: `docs/api.md:157-200`
- Modify: `internal/httpapi/stability_test.go`

- [ ] **Step 1: Add a failing docs/OpenAPI inventory test**

Parse a fenced table under `### Beta operations` and compare its
backticked operation IDs with operations carrying
`x-stability-level: beta`. Require exact set equality and sorted output.

Run: `go test ./internal/httpapi -run TestDocumentedBetaOperationsMatchOpenAPI -count=1`

Expected: FAIL because the exact table does not exist.

- [ ] **Step 2: Publish the exact inventory**

Add a table containing these 13 IDs and categories:

```text
createTemplate, deleteMessage, deleteTemplate, getAgentProtection,
getStarterTemplate, getTemplate, listStarterTemplates, listTemplates,
putAgentProtection, restoreAgent, restoreMessage, updateTemplate,
validateTemplate
```

Correct the existing prose so trash operations are explicitly experimental and
replace the stale `email.pending_review` event spelling with
`email.review_requested`.

- [ ] **Step 3: Run stability tests**

Run: `go test ./internal/httpapi -run 'Test.*Stability|TestDocumentedBetaOperationsMatchOpenAPI|TestOperationID' -count=1`

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/httpapi/stability_test.go docs/api.md
git commit -m "docs(api): publish beta operation inventory"
```

### Task 6: Regenerate OpenAPI and SDK contracts

**Files:**
- Modify: `api/openapi.yaml`
- Modify: `sdks/typescript/src/v1/generated/`
- Modify: `sdks/python/src/e2a/v1/generated/`
- Modify: `sdks/typescript/test/v1/client.types.ts`
- Modify: `sdks/python/tests/test_v1_client.py`

- [ ] **Step 1: Regenerate the committed contract and clients**

Run: `make generate`

Expected: OpenAPI gains header components/references and required
`FieldError.location`; both generated clients update accordingly.

- [ ] **Step 2: Add TypeScript required-field type assertions**

In the existing type-test file, construct a valid `FieldError` with both fields
and mark omission as an expected compiler error:

```ts
const validationField: FieldError = { location: "", message: "invalid request" };
void validationField;
// @ts-expect-error location is required by the GA validation contract
const missingValidationLocation: FieldError = { message: "invalid request" };
void missingValidationLocation;
```

- [ ] **Step 3: Add Python model assertions**

Assert `FieldError(message="invalid")` raises Pydantic `ValidationError`, while
`FieldError(location="", message="invalid")` succeeds.

- [ ] **Step 4: Run generated-code and SDK checks**

Run:

```bash
make spec-check
make generate-sdk-check
npm test --workspace @e2a/sdk
cd sdks/python && mypy && pytest tests/test_v1_client.py -v
```

Expected: all commands pass and a second generation produces no diff.

- [ ] **Step 5: Commit**

```bash
git add api/openapi.yaml sdks/typescript/src/v1/generated sdks/typescript/test/v1/client.types.ts sdks/python/src/e2a/v1/generated sdks/python/tests/test_v1_client.py
git commit -m "chore(api): regenerate canonical contract clients"
```

### Task 7: Full verification and handoff

**Files:**
- Modify only if a verification failure exposes a defect in this slice.

- [ ] **Step 1: Run focused Go verification**

Run: `go test ./internal/httpapi -count=1`

Expected: PASS.

- [ ] **Step 2: Run repository contract gates**

Run:

```bash
make spec-check
make generate-sdk-check
npm test --workspace @e2a/sdk
npm run build --workspace @e2a/sdk
cd sdks/python && mypy && pytest tests/ -v
```

Expected: PASS with no generated drift.

- [ ] **Step 3: Inspect the final diff**

Run:

```bash
git diff origin/main...HEAD --check
git status --short
git log --oneline origin/main..HEAD
```

Expected: no whitespace errors, no uncommitted product files, and only the
design/plan plus scoped implementation commits.

- [ ] **Step 4: Request code review**

Review the complete diff against
`docs/superpowers/specs/2026-07-15-canonical-contract-cleanup-design.md`, fix any
P0/P1 findings, rerun the affected verification, and then prepare the PR.
