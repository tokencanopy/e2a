# Filter query language (`q`) for `list_messages` — design

Date: 2026-07-25
Status: Approved (design), pre-implementation
Audience: e2a server, SDK, and MCP maintainers

## Context and goals

`GET /v1/agents/{email}/messages` today filters via flat params: `direction`,
`read_status`, `from`, `subject_contains`, `conversation_id`, `labels`
(AND-matched), `since`, `until`, `deleted`. Two gaps motivate a real filter
language:

1. **No boolean composition.** Label filtering is AND-only — `label:urgent OR
   label:follow-up` requires two queries plus client-side union (which breaks
   cursor pagination), and exclusion (`NOT label:newsletter`) is inexpressible.
2. **No growth path.** Each new filter dimension is a new bespoke param. A
   grammar with a field registry lets the vocabulary grow without API shape
   changes.

Benchmarking (2026-07-25): Gmail's `labelIds` is AND-only like ours, but Gmail,
Microsoft Graph (OData), JMAP (operator trees), and IMAP all provide a boolean
escape hatch. e2a is the outlier. AIP-160 (google.aip.dev/160) is the best
prior art: published EBNF, familiar syntax family (`gcloud --filter`), no
official Google library (aip-dev/google.aip.dev issues #684/#738 are open
requests for one), and existing third-party Go implementations are wrong-shape
for us (LUCI's is Spanner-dialect, einride's is protobuf-coupled, jaqx0r's is
parser-only, blocky-aip extends the grammar).

**Goal:** a `q` query param on `list_messages` accepting a boolean filter
expression, backed by a small, dependency-free, schema-agnostic Go package
(`internal/filterquery`) that parses, validates, and emits parameterized
Postgres SQL.

**Non-goals (v1):** full-text/body search, `to`/`cc` fields, attachment
presence/name/size filters, bare-term (unqualified) search, custom functions
(`hasAttachment()`), SDK-side parsing or validation, Spanner/MySQL dialects
(the `Dialect` interface admits them later), extraction as a standalone OSS
library (the design only keeps that door open).

## Locked decisions

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | Full boolean grammar from day one; narrow v1 vocabulary | Grammars version poorly; vocabulary grows non-breakingly. The grammar accepts everything AIP-160-shaped; the *validator* rejects unsupported fields. |
| D2 | `q` composes with existing flat params via AND | User decision (Gmail composes `labelIds` with `q`). Consequence: DSL field semantics MUST match flat-param semantics (`from:` = ILIKE substring, same as flat `from`) so the two spellings can never disagree. |
| D3 | Hand-rolled recursive-descent parser, zero dependencies | ~15 productions; owning lexer/parser/AST gives exact error messages and eliminates dependency-rot risk. Rejected: participle (declaration tax, mediocre errors), PEG generators (hostile-to-review output, heavy toolchain). |
| D4 | `internal/filterquery` is schema-agnostic and e2a-free | Extraction potential is a hard constraint, enforced structurally (see Genericity). All e2a knowledge lives in `internal/identity`'s field registry. |
| D5 | Symmetric ship: server + MCP + Python SDK + TS SDK + CLI + docs in one coordinated batch | House rule: API changes land symmetrically. |
| D6 | Two PRs: (1) `filterquery` package alone, no API change; (2) wired surface + all clients | Keeps the pure-addition core reviewable on its own; PR2 is the API-visible batch. |

## Package architecture (`internal/filterquery`)

Dependency direction: `identity` → `filterquery`. The package imports only
stdlib; it contains no e2a concepts (no "message", "label", "agent") — proven
by construction (see Testing: the conformance suite runs against an in-package
toy registry).

Five files, five stages:

- **`lexer.go`** — tokens: `TEXT`, quoted `STRING` (with `*` wildcard),
  `NUMBER`, case-sensitive keyword tokens `AND`/`OR`/`NOT`, `( ) : = != < <= >
  >= -`, and significant whitespace. Carries byte offsets for error positions.
- **`parser.go`** — recursive descent over the AIP-160 EBNF
  (google.aip.dev/assets/misc/ebnf-filtering.txt):
  `expression = sequence {AND sequence}` · `sequence = factor {WS factor}`
  (implicit AND, binds tighter than OR) · `factor = term {OR term}` ·
  `term = [NOT|-] simple` · `simple = restriction | ( expression )` ·
  `restriction = comparable [comparator arg]`.
  Hard caps: input length (handler-bound, 500 chars), nesting depth (64),
  node count (512) — fail with a positioned error, never panic or stack-overflow.
- **`ast.go`** — small tree: `And`, `Or`, `Not`, `Comparison{Field, Op, Value}`,
  `Bare{Text}`. Every node carries its source offset.
- **`validate.go`** — walks the AST against a `FieldRegistry`: unknown field,
  operator-not-allowed-for-field, uncoercible value (bad date, bad label
  charset) → typed error with column position. Bare terms parse but are
  rejected here ("qualify with a field") — the D1 mechanism: grammar complete,
  registry small.
- **`emit.go`** — walks the *validated* AST to `(fragment string, args []any)`,
  placeholders numbered from a caller-supplied start index so the fragment
  splices into the store's existing `$n` chain.

### Generic interfaces (D4)

- **`FieldRegistry`** — adopter-supplied: field name → `FieldSpec{Name,
  AllowedOps, Coerce(string) (any, error), Emit(PredicateCtx) (string, any)}`.
- **`Dialect`** — `Placeholder(n int) string` (`$n` Postgres built-in),
  identifier quoting, case-insensitive-match operator (ILIKE vs LOWER()).
- e2a's registry lives in `internal/identity` (`messagesFieldRegistry()`):
  maps `label` → `m.labels` ops, `from` → `m.sender` (matching the existing
  flat `from` filter), `subject` → `m.subject`, and `created` →
  `m.created_at`.
- CI guard: a tiny test asserting `go list -deps ./internal/filterquery`
  contains no `internal/identity` (or any non-stdlib) import.

## v1 vocabulary

| Field | Operators | Semantics |
|---|---|---|
| `label` | `:` | `label:urgent` = message carries label. Exclusion via `NOT label:x`. Value must pass the existing label charset rule (`[a-z0-9:_-]+`, ≤64). |
| `from` | `:` `=` `!=` | `:` = case-insensitive substring (ILIKE) on `m.sender` — identical to the flat `from` filter. `=`/`!=` = case-insensitive exact. |
| `subject` | `:` `=` `!=` | Same pattern on `subject`. NULL subjects never match `:`/`=` (SQL NULL semantics; document it). |
| `created` | `=` `!=` `<` `<=` `>` `>=` | On `created_at`. Values: RFC3339 or `YYYY-MM-DD`. A date-only value denotes that whole UTC day: `=`/`!=` test membership in that day's `[midnight, next-midnight)` range, `<=` includes the whole day, and `>` begins at the next midnight. Full RFC3339 values are exact. |

Everything else (`to:`, `body:`, bare text, …) parses but fails validation:
`unknown field 'to' — supported fields: created, from, label, subject`.

Attachment filtering is deliberately deferred. Inbound attachments are
canonical in the raw MIME message while outbound attachments use
`attachments_json`, so the existing JSON column cannot implement
`has:attachment` correctly. Shipping it requires a normalized attachment-count
field introduced with a blue/green-safe expand/backfill/contract rollout:
first make every live writer populate a nullable count and run an exact,
resumable Go backfill with `mailparse.Attachments`; only after old writers are
gone and the backfill is complete may the query field rely on that count.

`*` inside quoted strings maps to ILIKE `%` for `from:`/`subject:`; literal
user `%`, `_`, `\` are escaped and the predicate carries `ESCAPE '\'`.

## Emission pipeline (worked example)

Input: `label:urgent OR (from:alerts AND NOT subject:newsletter) created>=2026-07-01`

AST (precedence: NOT > OR > implicit AND > explicit AND):

```
And(
  Or( Has{label,"urgent"},
      And( Has{from,"alerts"}, Not(Has{subject,"newsletter"}) ) ),
  GreaterEq{created,2026-07-01T00:00:00Z} )
```

Emitted (store already bound `$1`=agent_id, `$2..$3` flat filters):

```sql
AND ( ( (m.labels @> $4)
        OR ( (m.sender ILIKE $5 ESCAPE '\')
             AND NOT (m.subject ILIKE $6 ESCAPE '\') ) )
      AND (m.created_at >= $7) )
-- args += []string{"urgent"}, "%alerts%", "%newsletter%", 2026-07-01T00:00:00Z
```

Rules that make this safe and correct:

- **Identifiers are registry constants only** — never user input. User input
  flows into SQL exclusively as bound parameters, including ILIKE patterns.
- **One operator map per field, booleans in the walker.** A `label:` leaf
  always emits single-element containment (`m.labels @> $n`, pgx serializes
  `[]string`); `OR`/`AND`/`NOT` composition happens structurally. No special
  cases, no string splicing.
- **Dialect = one function.** `Placeholder(4)` → `"$4"` today; `"@p4"` / `"?"`
  are one-line dialects later. Same walker.

## API integration

- `ListMessagesInput` gains `Q string \`query:"q"\`` (≤500 chars). OpenAPI doc:
  grammar summary + v1 field table + link to docs page.
- Handler: parse+validate once → 400 `invalid_filter` with column-marked
  message on failure. The validated expression's emitted predicate is ANDed
  with flat-param predicates in `GetMessagesByAgent` (D2).
- `messagesCursor` gains `Q string`; a changed `q` under a cursor hits the
  existing filter-identity rejection, same as other filters.
- MCP `list_messages` gains optional `q` (pass-through, same validation).
- SDKs regenerate from OpenAPI (Python `messages.list(..., q=...)`, TS
  `{ q }`), CLI `messages list --q`, docs page documenting the grammar.

## Error model

- Parse error: 400 `invalid_filter`, `unexpected ")" at column 12` style,
  position from lexer offsets.
- Validation error: 400, names the field/operator/value problem and lists v1
  fields for unknown-field errors.
- Fail-closed everywhere: a bad `q` is never silently dropped; an absent `q`
  adds no predicate.
- Resource caps (length/depth/node count) return 400, not panics.

## Testing and coverage hardening

Query generation fails silently, so coverage is layered to catch precedence,
escaping, NULL-handling, and injection bugs independently:

1. **Golden conformance vectors** (`testdata/conformance.json`, table-driven):
   each vector asserts all three stages — parse (AST or positioned error),
   validate (typed error or coerced values), emit (exact SQL fragment + args)
   — against an **in-package toy registry** (fake `products` table with
   text/int/bool/text[] columns), proving D4 by construction. This file is the
   future extraction seed.
2. **Precedence/enumeration matrix:** enumerate all valid expressions over 3
   fields with operators up to depth 3; assert AST shape and emitted SQL for
   each. Precedence bugs cannot hide.
3. **Differential testing against a reference evaluator (DB-backed):** a
   seeded fixture table (including NULL `header_from`, NULL `subject`, NULL
   vs empty `labels`, unicode subjects, labels containing `:`/`_`) is queried
   via emitted SQL in Postgres AND via a naive in-Go evaluator that filters
   the same fixture rows in memory. Result sets must be identical for every
   vector in layers 1–2. This is the primary defense against "SQL says one
   thing, intent says another."
4. **Go native fuzzing:** lexer+parser fuzzed (short CI run + corpus). No
   panics, no hangs, offsets always valid; round-trip property where defined.
5. **Injection invariant test:** for every vector, the emitted SQL fragment
   must contain none of the user-supplied value strings (all values
   parameterized). Explicit attack vectors: quotes, `$1`, `;`, `DROP`,
   backslashes, wildcard runs, `ESCAPE` probes.
6. **Caps tests:** 501-char input, depth-65 nesting, node-count overflow →
   positioned 400s, no panic.
7. **Handler tests:** 400 shapes, cursor pinning (`q` changed mid-cursor),
   `q` + flat-param composition, `q` on trash view.
8. **E2E:** one `q` round-trip case in `tests/e2e-prod` (staging conformance
   gate picks it up automatically).

## Ship plan

- **PR 1** — `internal/filterquery` package + conformance suite + fuzz +
  reference-evaluator harness (toy registry only, no API change).
- **PR 2** — API surface: `q` param, handler wiring, `messagesFieldRegistry`,
  store composition, cursor pinning, MCP param, regenerated SDKs, CLI flag,
  docs page, e2e case. One coordinated batch per D5.
