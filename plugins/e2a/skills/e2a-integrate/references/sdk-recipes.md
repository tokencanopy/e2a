# Official SDK recipes

Use the canonical [SDK guide](../../../docs/sdk.md) for current imports,
resource methods, webhook helpers, and outcome semantics. Keep SDK calls inside
the application's e2a adapter.

| Application language | Supported package | Client approach |
| --- | --- | --- |
| TypeScript / JavaScript | `@e2a/sdk` | Use the official `E2AClient` high-level client and its namespaced resources. |
| Python | `e2a` | Use the official `AsyncE2AClient` high-level client and its namespaced resources. |
| Any other server language | No official SDK | Follow [REST/OpenAPI](rest-openapi.md). |

Install the package through the repository-native dependency command and lockfile
workflow; do not introduce another package manager solely for e2a. Construct
the client in the existing server composition root, inject the adapter where
needed, and close it according to the framework lifecycle.

Do not write an unofficial SDK, copy generated client internals, or expose the
client or its API key to browser code. For a TypeScript or Python webhook,
use the SDK's documented signature helper on raw bytes before reading event
fields.
