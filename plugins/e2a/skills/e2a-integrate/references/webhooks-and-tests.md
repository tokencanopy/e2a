# Webhooks and tests

## Signed webhook handler

1. Configure the framework route to retain the exact raw request bytes.
2. Read the signature header and perform signature verification before parsing,
   validating, logging, or dispatching the payload.
3. Reject a missing, invalid, or stale signature without invoking application
   work. Only then decode the verified bytes and tolerate unknown additive event
   fields and types.
4. Insert the event ID into durable application state when available. Treat an
   existing ID as a successful no-op; make downstream work idempotent as well.
5. Commit the durable record and enqueue/perform application work using the
   repository's transaction and retry pattern. Respond according to the
   framework's established failure behavior so e2a can retry transient faults.

Use the canonical SDK guide for the official TypeScript `constructEvent` or
Python `construct_event` helper. Do not reserialize parsed JSON for signature
checking: the signed value is the original byte sequence.

## Test boundary

Use synthetic `.test` addresses, fixture event IDs, and a test-only signing
secret. Unit tests must prove valid verification, invalid signature rejection
before parsing/dispatch, replay/duplicate no-op behavior, and adapter outcome
handling. Contract tests may use a local fake HTTP server or the repository's
existing test harness; do not put production credentials or live-address
fixtures in source control.

Keep a live smoke test separate from unit and contract tests. Run it only after
explicit approval, with server-only credentials supplied outside source control
and an explicitly selected synthetic recipient. Record that approval and the
sanitized outcome outside public repository artifacts.
