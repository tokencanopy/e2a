# Task 5 — Golden conformance vectors and runner

## Delivered

- Added `internal/filterquery/testdata/conformance.json` with 26 parser,
  validator, and emitter vectors.
- Added `internal/filterquery/conformance_test.go`. It checks AST before
  validation for every vector that supplies one, exact SQL and normalized args
  for every successful vector, and typed/staged errors for parse, validation,
  and cap failures. Vector error labels are parsed and mapped through explicit
  switches, so an unknown label cannot fall through to an `ErrParse`
  zero-value.
- Moved `sexpr` once from `parser_test.go` to shared `helpers_test.go`.
- Preserved the controller's tracked amendments to
  `docs/superpowers/plans/2026-07-25-filter-query-language.md` in this slice.

## TDD evidence

1. Added JSON scaffolding and the runner, then ran
   `go test ./internal/filterquery/ -run TestConformance -v`.
2. The first runnable execution failed as expected: the SQL fixtures had an
   extra JSON-escaped backslash and the wildcard fixture exposed disagreement
   with the accepted parser/emitter contract.
3. Corrected the fixture encoding and expressed the wildcard vector as the
   accepted behavior; the focused runner then passed all 26 cases.

The initial sandboxed test invocation could not create Go's external build
cache; rerunning with the required cache access produced the red evidence
above.

## Verification

- `go test ./internal/filterquery/ -run TestConformance -v` — PASS (26 vectors)
- `go test ./internal/filterquery/` — PASS
- `go vet ./internal/filterquery/` — PASS
- `git diff --check` — PASS

This is a package-only library slice; the focused runner and complete package
tests are the closest real execution check. No HTTP service surface is added.

## Self-review

- Validation-error vectors that carry an AST (`not or`) assert it before
  `Validate` executes.
- Success vectors are rejected if AST or SQL is absent and compare exact SQL
  plus emitted args.
- Parse/cap errors cannot be accepted as validation errors, and vice versa.
- JSON array/number normalization is deliberately limited to the toy
  registry's `[]string` and `int` emitted values.

## Corrected plan defect: wildcard vector

The tracked plan's initial amendment incorrectly treated a quoted value as an
`=` comparison. The accepted parser keeps a quoted `:` restriction's operator
as `:`, and the toy registry's `name` field emits that as `ILIKE` with
`%a*b%`. The asterisk is literal in the toy adapter, exactly as the design's
per-field wildcard rule requires; `from:`/`subject:` are the later adapters
that translate quoted `*`. The plan and executable vector now consistently
use:

```json
{"name":"wildcard per-field choice","q":"name:\"a*b\"","ast":"(name : a*b)","sql":"(p.name ILIKE $1 ESCAPE '\\\\')","args":["%a*b%"]}
```

No Task 1–4 implementation was changed.
