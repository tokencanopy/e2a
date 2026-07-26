# Message filtering (`q`)

`GET /v1/agents/{email}/messages?q=…` accepts a small, boolean filter
language. It is an addition to—not a replacement for—the existing flat list
parameters. See the [filter-query design](superpowers/specs/2026-07-25-filter-query-language-design.md)
for rationale and rollout details.

## Syntax

Keywords are uppercase and case-sensitive: `AND`, `OR`, and `NOT`.

```ebnf
query        = expression [ WS ] ;
expression   = sequence { WS* "AND" WS* sequence } ;
sequence     = factor { WS factor } ;
factor       = term { WS* "OR" WS* term } ;
term         = [ "NOT" WS | "-" ] simple ;
simple       = restriction | "(" WS* expression WS* ")" ;
restriction  = member [ WS* comparator WS* value ] ;
member       = ( TEXT | STRING ) { "." TEXT } ;
comparator   = ":" | "=" | "!=" | "<" | "<=" | ">" | ">=" ;
value        = STRING | TEXT { ( "." | ":" ) TEXT } ;
```

Whitespace between expressions is implicit `AND`. Binding, from tightest to
loosest, is **`NOT` > `OR` > implicit `AND` > explicit `AND`**. Use
parentheses when that is not what you mean. A restriction without a comparator
is syntactically a bare term, but v1 rejects it: qualify the value with a field.

`STRING` is double-quoted. Quote values containing whitespace or reserved
operator/parenthesis characters; `.` and `:` may remain unquoted in values
(RFC3339 timestamps, email addresses, and `e2a:` labels). Only `\"` (a literal
quote) and `\\` (a literal backslash) are valid escapes. For example:
`subject:"build \"green\""`. A quoted field name is syntactically accepted,
but it must still resolve exactly to a supported v1 field name.

## V1 fields

| Field | Operators | Meaning |
| --- | --- | --- |
| `label` | `:` | Message has the label. Values match `[a-z0-9:_-]+` and are at most 64 characters. Use `NOT label:newsletter` to exclude it. |
| `from` | `:` `=` `!=` | Sender. `:` is case-insensitive substring matching; `=` and `!=` are case-insensitive exact matching. |
| `subject` | `:` `=` `!=` | Subject, with the same matching rules as `from`. A NULL subject matches none of these predicates. |
| `created` | `=` `!=` `<` `<=` `>` `>=` | Creation time. Value is RFC3339 or a `YYYY-MM-DD` UTC calendar date. |

For `from:` and `subject:`, `*` matches any sequence of characters. Literal
`%`, `_`, and `\` are escaped, so they never become SQL wildcards. The wildcard
is only meaningful for `:`; `=` and `!=` remain exact comparisons.

A date-only `created` value denotes the full UTC day `[00:00, next 00:00)`:

- `created=2026-07-01` matches that day; `!=` matches outside it.
- `<` is before that day's midnight; `<=` is before the following midnight.
- `>` starts at the following midnight; `>=` starts at that day's midnight.

Examples:

```text
label:urgent OR label:follow-up
label:urgent AND NOT label:newsletter
(from:"alerts.example" OR subject:"release *") created>=2026-07-01
created=2026-07-01
```

## Composition, errors, and limits

`q` is ANDed with every flat list filter (`direction`, `read_status`, `from`,
`labels`, `since`, and so on). A pagination cursor is tied to the complete
filter identity, including `q`; start a new query if any filter changes.

Invalid syntax, unknown fields, unsupported operators, invalid values, and
limit violations return HTTP 400 with error code `invalid_filter`. Parse and
validation errors identify their source position, for example `at column 12`.

The request accepts at most **500 Unicode code points**, nesting depth **64**,
and **512** AST nodes. Invalid UTF-8 and NUL are rejected.

`has:attachment` is intentionally deferred. Inbound attachments are canonical
in raw MIME while outbound attachments currently use JSON, so a correct filter
needs a normalized attachment-count field. It will ship only after a
blue/green-safe expand, backfill, and contract rollout makes that field reliable
for every live writer.
