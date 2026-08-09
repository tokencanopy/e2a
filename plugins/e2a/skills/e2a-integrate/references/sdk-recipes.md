# Official SDK recipes

Use the canonical [SDK guide](../../../docs/sdk.md) for current imports,
resource methods, webhook helpers, and outcome semantics. Keep SDK calls inside
the application's e2a adapter.

| Application language | Supported package | Client approach |
| --- | --- | --- |
| TypeScript / JavaScript | `@e2a/sdk` | Use the official `E2AClient` high-level client and its namespaced resources. |
| Python | `e2a` | Use the official `E2AClient` high-level client for a synchronous application; use the official `AsyncE2AClient` high-level client for an asynchronous application. Both expose the same namespaced resources. |
| Any other server language | No official SDK | Follow [REST/OpenAPI](rest-openapi.md). |

Install the package through the repository-native dependency command and lockfile
workflow; do not introduce another package manager solely for e2a. Construct
the client in the existing server composition root, inject the adapter where
needed, and close it according to the framework lifecycle.

Configure a bounded per-attempt timeout in the adapter: `timeoutMs` on the
TypeScript `E2AClient`, or `timeout_ms` on either Python client. Keep it
nonzero and within the application's request or job budget; use `maxElapsedMs`
or `max_elapsed_ms` as the total retry deadline when the application needs one.

Map SDK transport and API failures through the adapter by catching `E2AError`
subclasses. Preserve the stable `.code`, `.status`, and `.retryable` fields;
also apply the TypeScript `.retryAfterSeconds` or Python
`.retry_after_seconds` hint when present. Let the application's retry policy
decide whether a retryable error is retried within its deadline and idempotency
boundary.

Keep those exceptions separate from accepted, held-for-review, and delivery
outcomes. An accepted or held result is not a transport/API error and must not
be resent; observe its eventual delivery through the requested webhook,
event-log, or polling flow.

Do not write an unofficial SDK, copy generated client internals, or expose the
client or its API key to browser code. For a TypeScript or Python webhook,
use the SDK's documented signature helper on raw bytes before reading event
fields.
