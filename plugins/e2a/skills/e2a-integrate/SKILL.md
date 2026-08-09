---
name: e2a-integrate
description: "Use when adding e2a email capabilities to an application or codebase: outbound sending, inbound signed webhooks, REST polling, or SDK integration. Inspects the existing language and framework, uses official TypeScript/Python SDKs or idiomatic REST/OpenAPI, adds tests, and keeps credentials server-side."
---

# Integrate e2a into an application

## Determine the integration mode

Identify whether the request needs outbound sending, inbound signed webhooks,
REST polling, or a combined flow. Ask only when the send/receive intent is
materially ambiguous. Read [integration-modes.md](references/integration-modes.md)
for the decision table.

## Inspect the existing codebase

Find the language, framework, package manager, configuration conventions,
server entry point, persistence layer, and test runner. Preserve the existing
module, dependency, and test conventions. Do not create production credentials
or put live addresses in fixtures.

## Select the supported client surface

Use the official TypeScript/JavaScript or Python SDK when the application uses
one of those languages; use REST and the OpenAPI contract for every other
server language. Never invent an SDK or an unofficial wrapper. Read
[sdk-recipes.md](references/sdk-recipes.md) or
[rest-openapi.md](references/rest-openapi.md) for the selected surface.

## Build one application boundary

Add one application-owned e2a adapter boundary. Keep routes, domain logic, and
workers dependent on that boundary rather than calling e2a throughout the
application. Define server-only credential and webhook-secret configuration;
never expose either to browser code, client bundles, logs, or test fixtures.

Make retried writes idempotent with a stable key derived from the application's
logical operation. Handle accepted, held-for-review, and failed outcomes using
the existing application's error and job conventions.

## Secure inbound webhooks

For a webhook integration, retain the raw request bytes and perform signature verification before JSON parsing or dispatch. Deduplicate at-least-once events
by event ID in durable application state when it is available, then make the
handler's side effects idempotent. Follow
[webhooks-and-tests.md](references/webhooks-and-tests.md) for framework-safe
verification and recovery details.

## Test and verify

Add synthetic unit and contract tests using `.test` addresses and fixture
payloads. Cover the adapter, configuration failures, signature rejection before
parsing, duplicate-event behavior, and requested send/poll behavior. Run the
repository's focused and relevant full test commands.

Keep an explicitly approved live smoke test separate from synthetic tests. Do
not create credentials, send mail, or run that smoke test without approval and
an explicitly selected synthetic recipient.

## Completion report

State the integration mode, selected client surface, application-owned boundary,
server-only configuration contract, webhook verification and deduplication
status where relevant, tests run, and any deliberately unrun approved-live
smoke test.
