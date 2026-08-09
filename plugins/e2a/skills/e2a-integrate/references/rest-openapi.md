# REST and OpenAPI

For a server language without an official SDK, implement the adapter against
the canonical OpenAPI contract:

`https://api.e2a.dev/v1/openapi.yaml`

Send bearer authentication only from the server:

```text
Authorization: Bearer <server-held credential>
```

Use the application's normal HTTP client, explicit timeout, cancellation, and
error-mapping conventions. Parse non-success error envelopes sufficiently to
branch on their machine-readable code; preserve safe request diagnostics without
logging credentials, webhook secrets, or message content.

For a retried write, send an `Idempotency-Key` that is stable for the same
logical request and body. Reuse that key only for the byte-identical retry;
give a genuinely new request a new key. Treat an accepted or review-held send
as an accepted operation, not an instruction to send it again.

Generate a client only when the repository already has a compatible generation
workflow and commit policy. Never hand-edit generated clients; fix the source
contract or generation pipeline, then regenerate. Otherwise keep a small,
typed-or-validated application adapter around the few required REST operations.
