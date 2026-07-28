# Webhook delivery observability in the dashboard

Status: proposed
Date: 2026-07-27
Surfaces: `web/` dashboard, `internal/httpapi` (additive `/v1` changes in phase 2)

---

## 1. Problem statement

e2a has a complete webhook delivery backend — a durable event log, an
at-least-once outbox, a 72h retry envelope, per-event replay, and a per-webhook
delivery log. Almost none of it is visible in the dashboard.

The webhooks page renders `url`, `events`, `enabled`, and `created_at`. It does
not render `filters`, delivery history, failures, retries, or the event stream.
Every observability endpoint that exists (`GET /v1/events`,
`GET /v1/events/{id}`, `POST /v1/events/{id}/redeliver`,
`GET /v1/webhooks/{id}/deliveries`) is reachable from the API, SDKs, and MCP —
and from nowhere in the UI.

This is a client-surface parity gap, not a missing capability. It has a known
cause: `.github/pull_request_template.md`'s client-surface checklist does not
list the web dashboard, so API-side features ship without a UI counterpart.

**The concrete failure this enables.** On 2026-07-27 an account-scoped webhook
(`filters: {}`) had been fanning every inbox's mail to a third-party endpoint
for six days. That endpoint fetched each message over
`GET /v1/agents/{email}/messages/{id}`, which marks messages read by design, so
the dashboard's unread badge sat permanently at zero across all eight inboxes.
Diagnosing it took an hour and ultimately required correlating
`list_api_keys.last_used_at` against message `created_at` — because prod
deliberately omits request paths from logs (`haproxy.bootstrap.cfg:43-58`,
correctly: e2a embeds customer addresses in routes). None of the three
contributing facts — that the webhook was unscoped, that it was firing
constantly, that it was reaching inboxes it had no business reaching — were
visible on any screen.

## 2. Goals and non-goals

### Goals

1. **Answer "is my integration healthy?" at a glance**, without opening a
   detail view.
2. **Answer "why didn't my handler get this email?" in ≤3 interactions**,
   starting from the email — not from an event id the user does not have.
3. **Answer "what is this endpoint actually receiving?"** — the scope/exposure
   question, including the unscoped case.
4. **Make failure states self-explanatory**: HTTP status code and error
   excerpt inline, retry state distinguishable from terminal failure.
5. **Expose replay** with honest semantics about what replay does and does not
   do.

### Non-goals

- Changing webhook delivery, retry, or fan-out behavior. This is a read
  surface over an existing backend.
- Alerting, notification, or paging on delivery failure.
- Metrics/observability for e2a operators — that is `docs/observability.md`
  and the SLO instrumentation, a different audience.
- Extending the canonical message-lifecycle vocabulary (see D1).
- Fixing the read-on-GET semantics that produced the zero unread count. That
  is a separate GA-surface decision, tracked independently.
- Bulk reconciliation UI (`redeliver-since`) — the endpoint was designed in
  the 2026-06-01 doc but never shipped; see open questions.

## 3. Relevant context and constraints

### What the API already provides

| Endpoint | Filters | Notes |
|---|---|---|
| `GET /v1/events` | `type`, `agent_email`, `conversation_id`, `message_id`, `since`, `until` | **No `status` filter** |
| `GET /v1/events/{id}` | — | `410 Gone` after 30d |
| `POST /v1/events/{id}/redeliver` | body `webhook_id` optional | `202 Accepted`, async |
| `GET /v1/webhooks/{id}/deliveries` | `status` ∈ {pending, delivered, failed} | — |

`EventView` carries `status` ∈ {`pending`, `processed`, `no_match`} and a
fan-out rollup `delivery_status: {matched_webhooks, delivered, pending,
failed}`. That rollup is the single most useful field for this design: it
answers "did anyone receive this?" without a second request.

`WebhookDeliveryView` carries `attempts`, `last_attempt_at`, `last_status_code`,
`last_error`, `next_retry_at`, `status`, `type`. It does **not** carry an
`event_id` (see G1).

`WebhookView` carries `filters`, `enabled`, `last_delivered_at`, and
`auto_disabled_at`.

### Constraints that shape the design

**C1 — Retention is asymmetric, and orphans are normal.** Events live 30 days;
delivery rows live 90 days (migration 027). For up to 60 days a delivery row
exists whose parent event returns `410 Gone`. This is documented, intentional,
and the UI must render it as a normal state rather than an error.

**C2 — Replay is recovery, not re-delivery.** `POST /events/{id}/redeliver`
reuses the original event id. Receivers that dedupe on event id — which the
docs instruct them to do — will discard the replay. A "Redeliver" button that
appears to do nothing is worse than no button. The UI must state this.

**C3 — The dashboard is a static export.** `next.config` sets
`output: "export"`, and the app contains **zero** dynamic route segments.
Every per-resource page addresses its resource by query param
(`/inboxes/messages?email=…`, `/inboxes/settings?email=…`). A webhook detail
page must follow that convention, not `/webhooks/[id]`.

**C4 — The message lifecycle vocabulary has no webhook stage.** Stages are
`accepted`, `authentication`, `review`, `suppression`, `queued`, `submission`,
`delivery`, `complaint` — all provider observations about the email itself.

**C5 — Events already carry lifecycle transitions.** Mapped events include
`data.lifecycle_transitions`, whose rows share ids with
`GET /v1/agents/{email}/messages/{id}/lifecycle`. Events and lifecycle are
already correlated; nothing new is needed to join them.

**C6 — Per-row API probes are an established smell here.** The unread badge
issues one `list_messages` request per agent card. That pattern is what this
design must avoid repeating at webhook scale.

### Assumptions

- **A1.** Accounts have tens of webhooks, not thousands. Pagination is required
  for deliveries and events; it is not required for the webhook list itself.
- **A2.** `last_error` may contain arbitrary bytes from a customer's endpoint
  (HTML error pages, stack traces). It is untrusted text. *Unconfirmed:
  whether it is length-capped server-side — see open questions.*
- **A3.** Users debugging a webhook have access to their own endpoint's logs;
  the dashboard's job is to tell them what e2a observed, not to replace their
  logging.
- **A4.** The 30/90-day retention split is deliberate and will not change to
  accommodate this UI.

## 4. Proposed design

### D1 — Compose at the read layer; do not extend the lifecycle ledger

**Decision:** the message view renders webhook notifications as a section
*adjacent to* `MessageLifecycleTimeline`, sourced from
`GET /v1/events?message_id={id}`. The canonical lifecycle vocabulary is
untouched.

**Why.** `messagelifecycle` is the canonical, durable, deduped record of what
happened to *the email* — provider observations, reconstructable into a
snapshot. Webhook fan-out is a different domain concept: notification of
observers. Adding a `StageWebhook` would (a) change what "lifecycle" means, a
domain-model change to a ~4.5k-LOC package with a frozen event↔reason mapping
published in `docs/events.md`, and (b) pour per-subscriber retry noise —
N subscribers × up to 10 attempts — into a ledger designed for one row per
observed fact. A message with three subscribers and a flaky endpoint would
have its actual delivery story buried under thirty retry rows.

Composition is also cheap: per C5 the two are already correlated, and
`?message_id=` already exists.

**Rejected:** adding `StageWebhook` to `internal/messagelifecycle/catalog.go`.
Rejected for the reasons above — a UI need does not justify redefining a
canonical domain vocabulary.

### D2 — A webhook detail page is the primary new surface

**Route:** `/webhooks/detail?id=wh_…`, per C3.

Contents, top to bottom:

1. **Identity and scope** — url, events, and the scope summary already shipped
   on the list row. Unscoped renders as the `all agents` warning treatment.
2. **Health strip** — delivered / failed / pending counts over the selected
   window, and `auto_disabled_at` state if set.
3. **Deliveries feed** — the paginated delivery log, filterable by `status`
   using the existing server-side filter.

Each delivery row shows: event type, status, `attempts`, `last_status_code`,
a truncated `last_error`, and `last_attempt_at` / `next_retry_at`.

### D3 — List-row health uses only fields already on `WebhookView`

**Decision:** for phase 1, the list row shows health derived from
`enabled`, `auto_disabled_at`, and `last_delivered_at` — all already present in
the existing `GET /v1/webhooks` response. **Zero additional requests.**

**Why.** The obvious alternative — probe `GET /v1/webhooks/{id}/deliveries` per
row to compute a success rate — is the N+1 pattern of C6, and would issue one
request per webhook on every page load and poll. A true success-rate needs a
server-side rollup (G3), which is phase 2. Shipping a degraded-but-honest
signal now beats shipping an expensive one.

What this buys immediately: a webhook that has been auto-disabled, or that has
never delivered, or whose last delivery is stale, is visible without opening
anything.

### D4 — `no_match` is a first-class state, not a zero

An event matching zero subscribers is silent failure — the precise inverse of
the incident above. Anywhere an event is rendered, `status: "no_match"` gets
its own treatment and label ("No subscribers matched"), never a `0` inside the
fan-out rollup. This is the state that answers "why didn't my handler fire",
and it must be legible without arithmetic.

The fan-out rollup renders compactly elsewhere: `3 matched · 2 delivered ·
1 failed`.

### D5 — Retry state is visually distinct from failure

A delivery with `status: "pending"` and a future `next_retry_at` is *in
flight*, not broken. Render it as "Attempt 3 — next retry 14:32", visually
distinct from terminal `failed`. Conflating them causes false escalation, and
with a 72h retry envelope the in-flight window is long.

### D6 — Redeliver states its own semantics

The redeliver control carries the C2 caveat inline: replay reuses the original
event id, and a receiver that dedupes will discard it. Copy should say
recovery, not retry. Redeliver returns `202` and is async — the UI reflects
"queued", and the outcome appears in the deliveries feed, not in the button's
response.

### D7 — Events log is the escape hatch, not the front door

A standalone events log ships in phase 3, reachable from the webhooks section
rather than as a new top-level nav item. It carries the payload inspector
(`data`, pretty-printed, copyable) and redelivery.

**Why not lead with it.** A top-level "Events" page adds a fourth place to look
and maps to no question a user actually asks. Users ask about *emails* and
about *endpoints*; the event id is an implementation detail they encounter only
when someone hands them one. D1 and D2 put the answers where the questions
start. This ordering is the design's main opinion, and the thing most worth
disagreeing with.

### API additions (phase 2, all additive)

- **G1 — `event_id` on `WebhookDeliveryView`.** Without it there is no
  navigation from a delivery to its event or message; the drill-down path is
  broken at its most important hop. Must tolerate the C1 orphan case.
- **G2 — `status` filter on `GET /v1/events`.** The highest-value query —
  "show me everything that matched nothing" or "everything that failed" — is
  currently impossible server-side. Client-side filtering over a paged log is
  wrong past the first page.
- **G3 — health rollup.** Counts by status over a window, per webhook, so D3
  can show a real success rate without N+1.

All three are additive and clear the oasdiff compat gate
(`docs/api-compatibility-gate.md`). Each requires the full parity sweep: Go
handler, `make generate`, both SDKs, CLI, MCP, and — per the gap named in §1 —
the dashboard.

### Phasing

| Phase | Contents | API change |
|---|---|---|
| 1 | Webhook detail page + deliveries feed; list-row health from existing fields | None |
| 2 | G1, G2; message-view notifications section (D1) | Additive |
| 3 | Events log with payload inspector + redeliver; G3 health rollup | Additive |

Phase 1 closes the incident-shaped hole using only endpoints that exist today.

## 5. Edge cases and failure handling

| Case | Handling |
|---|---|
| **Orphaned delivery** (event 30d+ old, `410 Gone`) | Normal state per C1. Render the delivery; disable the event link with "Event expired (30-day retention)". Never an error toast. |
| **`no_match` event** | D4 — distinct state and label. |
| **Pending with `next_retry_at` in the past** | Worker lag or clock skew. Render "retry due" rather than a negative countdown. Never compute a duration that can go negative. |
| **Auto-disabled webhook** | Loud banner on detail, badge on list row. Explain *why* e2a disabled it and what re-enabling requires. |
| **`last_error` is hostile** | Untrusted text (A2). Render as plain text — never `dangerouslySetInnerHTML` — truncate at a fixed length with expand-on-demand. |
| **Deliveries exist, webhook deleted** | Delivery rows outlive the subscription. Detail page for a deleted webhook should 404 cleanly rather than render a half-page. |
| **Redeliver double-click** | Server dedupes within a short window; disable the control after the first `202` and reflect "queued". |
| **Redeliver to a disabled webhook** | Behavior unconfirmed — see open questions. Until settled, the control is hidden for disabled subscriptions rather than guessing. |
| **Very high delivery volume** | Cursor pagination throughout, `limit` capped at 100 by the API. No client-side "load all". |
| **Empty states** | "No deliveries yet" for a new webhook is very different from "no deliveries in this window" — do not share one string. |

**Fail closed:** any state the UI cannot confidently classify renders as
unknown with the raw status string shown, never as success. An observability
surface that reports green on unparsed data is worse than one that admits
confusion — that is exactly the failure mode of the unread badge, which
correctly rendered "nothing to report" while the underlying truth was
"something is consuming everything."

## 6. Scalability and extensibility notes

- **Grows in volume:** deliveries and events, both unbounded per account and
  both already cursor-paginated. Nothing in this design accumulates unbounded
  client state.
- **Grows in surface:** the event catalog is explicitly an open set
  (`docs/events.md`: "tolerate unknown values"). Every event-type rendering
  path must handle unknown types by displaying the raw string, not by throwing
  or omitting. The existing `EVENT_TYPES` constant in the webhooks page is a
  *curated picker* list, not an exhaustive catalog — reusing it for display
  would silently drop unknown types.
- **Deliberately narrow for v1:** the health window is a fixed 24h rather than
  a user-selectable range; no charting library is introduced.
- **Made easier later:** G1 + G2 are the joins any future cross-cutting view
  needs (per-agent event feeds, reconciliation UI, bulk replay). Landing them
  in phase 2 is what makes phase 3 small.

## 7. Verification strategy

**Seams.** Two, both boundaries callers actually cross:

1. **The wire→view parsing layer** (`web/src/app/components/onboarding/api.ts`,
   alongside `getInboxUnread` etc.). Every field this design reads is optional
   or open-set; parsing is where malformed and unknown values must be
   normalized. Unit-testable without React.
2. **Component render** for state classification — the mapping from
   `{status, attempts, next_retry_at, last_status_code}` to a rendered state.

There is deliberately no adapter seam between them: there is one API and one
renderer, and a seam nothing varies across is a hypothetical, not a real one.

**Required tests:**

- Classification table: delivered / failed / pending-retrying / pending-overdue
  / unknown-status → expected label and tone.
- `no_match` renders its own state, not `0 delivered`.
- Orphaned delivery renders with the event link disabled, no error.
- Unknown event type renders the raw string rather than throwing.
- Hostile `last_error` renders as text — assert no HTML injection.
- Scope display for all filter shapes (already covered by the shipped Scope
  column tests).

**Most likely regressions:** (a) treating an open-set enum as exhaustive and
crashing on a new event type; (b) an N+1 request pattern creeping into the list
row (D3 exists to prevent this — a test asserting request count on list render
is worth having); (c) negative durations from clock skew.

**Manual validation:** a genuinely failing endpoint. Point a webhook at a URL
returning 500 and confirm the retry sequence is legible end to end. Local
fixtures can produce delivered and no_match, but the retry-with-backoff path is
worth seeing live at least once.

## 8. Open questions

1. **Is `last_error` length-capped server-side?** (A2.) If not, the API can
   return arbitrarily large strings and the cap belongs server-side, not only
   in CSS.
2. **What does redeliver do against a disabled or auto-disabled webhook?**
   Silently drop, error, or deliver anyway? Determines whether the control is
   shown, disabled, or shown-with-warning.
3. **Should the 30-day event retention move to 90 to match deliveries?** A4
   assumes not, and C1 is handled — but if orphaned deliveries prove confusing
   in practice, aligning retention removes the state entirely.
4. **What window defines "healthy"?** 24h is proposed. 7d is steadier for
   low-volume accounts, where 24h may legitimately contain zero deliveries.
5. **Should the events log be account-wide or agent-scoped by default?** For a
   250-agent account an account-wide firehose may be unusable as a default
   view.
6. **Does `redeliver-since` (bulk) still want to exist?** Designed in the
   2026-06-01 doc, never shipped. Reconciliation currently means walking
   `GET /v1/events` and redelivering ids one at a time — workable over an API,
   painful in a UI.
7. **Should the PR template gain a web-dashboard row?** Out of scope here, but
   it is the mechanism that caused this gap and will cause the next one.
