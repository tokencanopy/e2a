# Integration modes

Choose the narrowest mode that implements the requested behavior. Combined
flows may use both a signed webhook and polling reconciliation.

| Need | Use | Application responsibility |
| --- | --- | --- |
| Send email only | Send-only | Call the adapter from a server-side command or worker; retain an idempotency key for retried writes. |
| React promptly to inbound or lifecycle events | Signed webhook | Expose a server endpoint, retain raw bytes, verify first, persist/deduplicate the event ID, then enqueue or dispatch. |
| Read mail or recover missed work on an interval | Polling | Run a server-side scheduled worker, store its cursor/checkpoint and processed event IDs, and tolerate repeated reads. |
| Send and receive reliably | Combined | Use send-only plus webhook for low latency; add polling or event-log reconciliation when recovery requirements justify it. |

Do not substitute a browser callback for a server receiver. A webhook is an
at-least-once notification; a successful HTTP response is not permission to
lose the application's durable work record. Use the repository's existing
queue, transaction, and retry patterns.

For an unknown request such as "add e2a," ask whether it needs outbound,
inbound, or both. Otherwise infer the mode from the requested behavior and
proceed.
