# Webhook health notifications, UI enable/disable, and the disabled-delivery snooze cap

Status: design · Owner: backend + web · Related: `internal/webhook/auto_disable.go`,
`internal/identity/webhooks.go`, `internal/webhookdelivery/worker.go`,
`web/src/app/(app)/webhooks/`, `internal/hitlnotify` (pattern precedent)

## Problem statement

When e2a auto-disables a customer's webhook for chronic delivery failure, **the customer
is never told**, and once told they have **no way to act on it from the dashboard**. A
production incident makes both concrete.

A customer's endpoint began returning `HTTP 404` to every POST on 2026-08-03. The breaker
tripped on 2026-08-04 19:35 PT. Between those points and today the account owner received
no signal of any kind. The evidence, from prod:

| status | last_error | count | created |
|---|---|---|---|
| delivered | — | 56 | 07-06 → 07-28 |
| failed | HTTP 404 | 50 | 07-06 → 08-03 |
| failed | HTTP 500 | 2 | 07-16 |
| **pending** | HTTP 404 | **128** | 08-03 → 08-04 |

Three distinct defects:

1. **No notification.** The only surfaces carrying the auto-disabled state are the
   dashboard (if you happen to look) and the API. Delivery had already stopped for a full
   day before the breaker even fired, and nothing reached the owner at any point.
2. **No dashboard remedy.** The webhooks UI is read-only apart from create / rotate-secret
   / delete. The detail page renders a banner that tells the user to leave: *"Fix the
   endpoint, then re-enable it through the API after the five-minute cooldown."* The CLI
   has no `webhooks` command at all, so the only recovery path is a hand-written HTTP call.
3. **128 deliveries are stuck pending forever.** Once a webhook is disabled, the delivery
   worker snoozes rather than failing (`worker.go:278-282`), **uncapped**. Those rows will
   wake hourly until `expires_at = 2026-11-01` — roughly 270,000 no-op job wakeups — and
   show the customer `pending` for three months on deliveries that will never be attempted.

Defect 3 is created *by* fixing defect 1: a "delivery has stopped" email that contradicts a
dashboard reading `pending` is worse than no email. They must ship together.

## Goals and non-goals

### Goals

- **G1.** The account owner is emailed when a webhook's deliveries start failing —
  *measurably before* the breaker trips, not only after.
- **G2.** The account owner is emailed when e2a auto-disables a webhook, with the concrete
  failure reason and re-enable instructions.
- **G3.** A user can see and change enabled/disabled state entirely from the dashboard.
- **G4.** A delivery against a disabled webhook reaches a truthful terminal state in
  bounded time.
- **G5.** Both emails come from a **replyable support address** rather than a `noreply` —
  a customer whose integration just broke should be able to hit reply. On the hosted
  service that is `support@team.tokencanopy.com`; the address is configuration, so
  self-hosters get a working default instead of the operator's address (Part 4).

### Measurable success criteria

- **SC1.** For an endpoint that starts hard-failing every request, the first warning email
  is enqueued **within 10 minutes** of the first failed attempt (one 5-minute sweep plus
  slack). Today: never.
- **SC2.** Every auto-disable transition produces exactly one email — no duplicates across
  sweeps, replicas, or restarts.
- **SC3.** Zero deliveries remain `pending` more than 24h after their webhook is disabled.
  Today: up to the full 90-day retention.
- **SC4.** Re-enabling a webhook requires no API call and no docs lookup.

### Non-goals

- Per-user notification preferences or an opt-out. These are operational service alerts
  about the customer's own broken integration, in the same class as the existing HITL
  approval mail, which has no opt-out either. Revisit if volume ever justifies it.
- Webhook-health alerting to *us* (Cloud Monitoring). Separate concern, separate surface.
- Changing the auto-disable thresholds themselves (10 failed / 72h). Out of scope; the
  warning is what fixes the "too late" problem, not a lower disable bar.
- CLI `webhooks` commands. Real gap, but independent of this work.

## Relevant context and constraints

### Reuse: `internal/hitlnotify` is the precedent, not a new system

The HITL approval email solved this exact shape and its design doc
(`docs/design/hitl-notify-river.md`) records why the naive version failed: it was a
detached `go func()` that composed and SMTP-sent inline, so **a crash or SMTP outage
between the state change and the send lost the notification forever**. It was rebuilt as
three layers on River, with the job enqueued *in the same transaction* as the state change.

This design copies that structure rather than inventing one. Concretely reusable:

- `internal/hitlnotify/jobs.go` — the `Jobs` registrar with two-phase wiring
  (`SetEnqueuer` / `SetDeliverer`) and late-binding `Deliver`, so a job enqueued during the
  startup window retries instead of hitting a nil deliverer.
- `outbound.SMTPRelay.SendOnce` + `IsPermanentSMTPError` / `IsConnectionError`
  (`internal/outbound/smtp_relay.go:100,129,147`) for the send and its error triage.
- `jobs.QueueNotify` already exists (`internal/jobs/queues.go:28`) with a small pool,
  deliberately isolated so notification backlog can't starve customer outbound delivery.
- `store.GetUserByID(ctx, agent.UserID)` → `owner.Email` for addressing.
- A fixed no-reply local part so clients thread the mail (HITL uses `hitl-noreply`).

### The sweep is not currently transactional

`Store.AutoDisableFailingWebhooks` (`internal/identity/webhooks.go:558`) is a bare
`pool.Query` running one bulk `UPDATE … RETURNING id`. To enqueue a job atomically per
disabled webhook it must move to a tx. This is the one structural change on the backend.

Its `AND enabled = true` predicate is what gives us SC2 for free: a webhook can only be
returned by the sweep on the transition, never on a subsequent pass.

### `snoozeCount` already exists

`internal/webhookdelivery/worker.go:169` reads River's per-job snooze counter out of job
metadata. It exists today to gate a latency SLI. The snooze cap needs exactly this value,
so G4 requires no new bookkeeping.

### The UI already classifies health correctly

`web/src/lib/webhooks.ts` has `classifyWebhookHealth` returning
`auto_disabled | disabled | never_delivered | stale | active`, with labels and tones
centralised so list and detail cannot disagree. `auto_disabled` is deliberately checked
before `enabled` because "e2a switched this off for you" is a louder fact than "you
switched it off". **G3's display half is already done** — the list in the screenshot
correctly reads `no deliveries in 7d`. Only the *mutation* is missing.

### Assumptions

- **A1.** The platform SMTP relay is configured in prod. If it is not, notifications
  degrade to logs — the notifier is nil-guarded exactly as `hitlnotify` is.
- **A2.** Emailing the account owner about their own endpoint's URL and status code leaks
  nothing across a tenancy boundary.
- **A3.** One email per webhook per degradation episode is the right volume. Accounts with
  many endpoints failing at once are rare; if that changes, batching is the follow-up.
- **A4.** Warning on *attempt-level* failures (below) is acceptable even though a single
  transient 500 followed by a successful retry could produce a warning. Debounced by
  threshold + cooldown; see edge cases.

## Proposed design

### Part 1 — warn early, on attempts, not terminal failures

The single most important design decision here. The obvious implementation is to warn at a
lower count of the *same* signal the breaker uses (terminal `failed` rows). **That would
not have helped this customer**, and it is worth being precise about why.

A delivery only reaches terminal `failed` after exhausting all 8 attempts across the frozen
`retryBackoffs` envelope — **29h21m** (`worker.go:34-42`). So a warning keyed on terminal
failures cannot fire sooner than ~29h after the endpoint breaks, no matter how low the
threshold. The customer broke on 08-03 and the first terminal failure was not until 08-04.

Instead the warn condition reads **first-attempt failures**, which land within seconds:

> In the last `WarnWindow` (24h), at least `WarnThreshold` (5) delivery rows for this
> webhook have recorded a failed attempt (`attempts >= 1 AND last_error IS NOT NULL`),
> **and** zero rows reached `delivered`, **and** the webhook is `enabled`, **and**
> `warn_notified_at IS NULL`.

Against the real incident: 42 rows were created on 08-03, each failing its first attempt
immediately. The condition is satisfied on the **next 5-minute sweep** — satisfying SC1
and giving the customer a day of warning before the breaker fires.

This rides the existing 5-minute maintenance sweep (`MaintenanceJobs`, `maintenance.go`) as
a second pass; no new schedule.

### Part 2 — the two emails

New package `internal/webhooknotify`, mirroring `hitlnotify`'s three files:

```
notifier.go   compose + SendOnce (owner lookup, plain-text + HTML bodies)
jobs.go       jobs.Registrar; EnqueueWebhookNotifyTx(ctx, tx, webhookID, kind)
worker.go     river.Worker on QueueNotify; re-reads state, guards, delivers
```

Job args carry `WebhookID` and `Kind ∈ {warning, disabled}` — one worker, two templates,
because the guards and delivery/error triage are identical and the bodies differ only in
copy and severity.

**Control flow, disable path:**

```
maintenance sweep (5 min, QueueMaintenance)
  BEGIN
    UPDATE webhooks SET enabled=false, auto_disabled_at=now()
      WHERE <existing predicate> AND enabled = true
      RETURNING id
    for each id: InsertTx(webhook_notify{id, disabled}, QueueNotify)
  COMMIT
```

The `RETURNING`-driven enqueue inside the same tx is what makes SC2 hold: the row cannot
be disabled without its job, and cannot be returned twice.

**Control flow, warn path:** same sweep, second pass —

```
  BEGIN
    UPDATE webhooks SET warn_notified_at=now()
      WHERE <warn condition> AND warn_notified_at IS NULL
      RETURNING id
    for each id: InsertTx(webhook_notify{id, warning}, QueueNotify)
  COMMIT
```

`warn_notified_at IS NULL` in the predicate is both the dedupe and the enqueue trigger, so
the same exactly-once argument applies.

**Worker guards** (mirroring `hitlnotify`'s, all → `nil`, no retry):

1. webhook row gone (deleted) — nothing to report;
2. `kind=disabled` but the row is `enabled` again — the user already fixed and re-enabled
   inside the window; the email is now misleading, so drop it;
3. `kind=warning` but the row is no longer `enabled` — it has since been disabled outright;
   the disable email supersedes it;
4. `kind=warning` but a delivery has since succeeded — recovered on its own.

Error triage on send follows `hitlnotify` exactly: permanent SMTP → `JobCancel` + log;
connection error → `JobSnooze`; transient → return error, River retries.

**Email content** (disabled variant): what happened, the endpoint URL, the concrete reason
(`last_error`, e.g. `HTTP 404`), how many failures over what window, that events are no
longer being delivered, and the re-enable path — now a dashboard link rather than a curl
command, because Part 3 makes that real. The warning variant is the same skeleton with
"we will disable this if it keeps failing" instead.

**What the disable email must NOT claim.** The tempting line is "your events are safe, just
replay them once you're fixed." That is only true up to the disable moment, and the
distinction matters enough to state precisely:

`webhook_events` rows survive 30 days and `POST /v1/events/{id}/redeliver` can replay them,
but replay is gated on `matched_webhook_ids` — a column **stored at fan-out time**
(`internal/agent/replay_api.go:27`). Fan-out matches only *enabled* subscribers, so an
event published after the disable never records this webhook, and replay refuses it with
`409 webhook was not among the originally-matched subscribers`. Bulk replay yields zero
deliveries for it.

So auto-disable is recoverable **backwards** and lossy **forwards**. The honest wording is:
delivery stopped at T; events queued before T can be replayed; events after T were never
queued for this endpoint. This is also why the *warning* email is the one that actually
protects data — by the time the disable email sends, the forward loss has already started.

Note also that replay is API-only (no dashboard, no CLI) and one event per call, so the
recovery it offers is real but hands-on.

### Part 3 — dashboard enable/disable

Server-side: nothing. `PATCH /v1/webhooks/{id}` with `{"enabled": bool}` already exists
(`updateWebhook`, `internal/httpapi/webhooks.go:267`), already clears `auto_disabled_at` on
re-enable, and already maps the cooldown to `409 webhook_cooldown`.

Client-side:

- **List row** — a toggle in the existing action cluster (beside Rotate secret / Delete),
  reusing that row's existing mutation plumbing.
- **Detail page** — replace the dead-end banner copy at
  `web/src/app/(app)/webhooks/detail/page.tsx:169-180` with a real **Re-enable** button.
- **409 handling** — `webhook_cooldown` must render as "auto-disabled moments ago; try
  again in a few minutes", never a raw error. Note the OpenAPI description explicitly warns
  *"SDKs do not automatically retry this code"*, so the UI must not either.
- **Optimistic-update discipline** — re-enable is exactly the action most likely to be
  taken against a stale page. Refetch rather than trusting local state.

**Prerequisite, already shipped separately:** the detail page was unreachable in prod —
Caddy's `/webhooks/*` third-party-callback matcher shadowed `/webhooks/detail` and returned
a plain-text 404. Fixed in e2a-ops PR #297. Part 3's detail-page work is untestable in prod
until that merges.

**One API addition** (additive, optional, backward compatible): `auto_disabled_reason` on
the webhook view — a short open-set string such as `HTTP 404`, derived from the last
terminal `last_error`. Without it the UI can say *that* we disabled the endpoint but not
*why*, which is the first question a user asks. Treated as an open set the client must
tolerate unknown values in, consistent with how `webhook_status` is documented on
`identity.Store`. Must carry no internal hostnames, IPs, or DB identifiers — the delivery
worker already sanitises `last_error` for this reason.

### Part 4 — the sender address (config, never a constant)

Both emails send from a **support address**, not a `noreply`. On the hosted service that
is `support@team.tokencanopy.com`.

**This must not be hardcoded in this repo.** The server is open source and self-hostable;
a constant here would make every self-host deployment try to send as the hosted operator's
support address — mail SES would reject outright, since the self-hoster does not own that
identity. The address is hosted configuration and belongs in the ops repo, matching the
existing split.

- **OSS side:** a new optional config key, `notifications.from_address`. When unset, fall
  back to the current `hitlnotify` behavior — a fixed local part on
  `cfg.OutboundSMTP.FromDomain` — so self-host works with zero configuration.
- **Ops side:** set it in `config.prod.yaml`, alongside the existing
  `from_domain: send.e2a.dev`.

Note the notifier's from-domain is a single global today (`cfg.OutboundSMTP.FromDomain`,
`cmd/e2a/main.go:571`) shared with the product outbound sender, so this is a genuinely new
knob rather than a value substitution.

**Feasibility is confirmed, not assumed.** `team.tokencanopy.com` is a registered e2a
domain with `sending_status: verified`, DKIM verified (`e2a202607._domainkey`), a verified
custom MAIL FROM (`bounce.team.tokencanopy.com` → `feedback-smtp.us-east-2.amazonses.com`),
and `sending_ramp: exempt`. SES in us-east-2 therefore already holds a sending identity for
it.

**Replies land somewhere real.** That domain's MX points at `mx.e2a.dev` and a
`support@team.tokencanopy.com` agent exists, so a customer replying to a webhook-failure
email arrives in an actual e2a inbox — the service dogfooding itself. Worth setting
`Reply-To` explicitly rather than relying on `From`, so a future change of sending identity
does not silently break the reply path.

**One implementation checkpoint:** confirm the server's runtime SES credentials (the scoped
`e2a-server` IAM user) are permitted to send with this From address — an IAM policy
condition on `ses:FromAddress` or `ses:FromDisplayName` would block it. Per the repo's
standing warning, verify against the **server's** credentials, not a laptop's; local creds
masking a missing container credential has caused a real prod failure before.

### Part 5 — cap the disabled-delivery snooze

```go
// worker.go, replacing the unconditional snooze
if !wh.Enabled {
    if snoozeCount(job) >= maxDisabledSnoozes {   // 24 → ~24h of grace
        w.emitAttempt("exhausted", "none", -1)
        w.subStore.TransitionSubscriberFailedIfPending(
            ctx, job.Args.DeliveryID, job.Attempt, "webhook disabled", 0)
        w.emitTerminal("webhook_disabled", deliveryScope(d))
        return nil
    }
    w.emitAttempt("skipped_disabled", "none", -1)
    return river.JobSnooze(disabledSnooze)
}
```

24 snoozes × 1h preserves the grace the current behavior was reaching for — a user
disabling an endpoint for maintenance loses nothing — while guaranteeing SC3. The existing
`TransitionSubscriberFailedIfPending` is already the conditional terminal write used
elsewhere in this worker, so a delivered row can never be clobbered.

`last_error = "webhook disabled"` is deliberately distinct from a transport error: it tells
the user the delivery stopped because of endpoint state, not because their server misbehaved
on that attempt.

### Alternatives considered

**Warn on terminal failures at a lower threshold (rejected).** Reuses the breaker's exact
signal — appealingly simple, one query shape. Rejected because the 29h21m retry envelope
puts a hard floor under how fast it can fire, which is precisely the failure this design
exists to fix. It would have warned this customer roughly a day late.

**Notify inline from the sweep, no River (rejected).** Fewer moving parts. Rejected on
recorded precedent: this is exactly the fire-and-forget shape `hitl-notify-river.md` was
written to remove, and it loses the notification on any crash or SMTP outage. Reintroducing
it for a *less* frequent, *more* consequential event would be a regression.

**Fail disabled deliveries immediately, no snooze (rejected).** Simplest and most honest.
Rejected because it destroys the maintenance-window buffer: a user toggling an endpoint off
for ten minutes would lose every in-flight delivery. The cap keeps the buffer and bounds it.

**Relabel `pending` → `paused` without changing semantics (rejected).** Fixes the
misleading status with no delivery-path change. Rejected because it leaves ~270k no-op
wakeups in place and adds a status value to the public API purely to describe an internal
stall — treating the symptom.

**A dedicated `webhook_notifications` table (rejected).** Full audit trail of what we sent.
Rejected as premature for two email kinds; two nullable columns on `webhooks` carry the
same dedupe guarantee. Revisit if notification kinds multiply.

### Schema

Migration `096_webhooks_health_notify.sql`, metadata-only, both nullable:

```sql
ALTER TABLE webhooks
  ADD COLUMN IF NOT EXISTS warn_notified_at   TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS auto_disable_reason TEXT;
```

`warn_notified_at` must be **cleared on successful delivery**, alongside the existing
`last_delivered_at` write, so a webhook that recovers and later degrades warns again rather
than staying permanently silent. This is the one easy-to-miss coupling in the change.

No cutover backfill is needed, unlike migration 057's: nothing has been notified before, so
there is no pre-existing state to suppress. The currently-auto-disabled production webhook
will *not* retroactively generate an email — the sweep only enqueues on transition, and
that row is already `enabled = false`. That customer needs a manual note; see open questions.

## Edge cases and failure handling

| Case | Handling |
|---|---|
| Owner has no email on record | `hitlnotify` returns an error and logs; same here — permanent, `JobCancel`, no retry loop |
| Webhook deleted between sweep and send | Worker guard 1 → `nil` |
| User re-enables before the disable email sends | Guard 2 drops it — no "we disabled your webhook" for a working one |
| Endpoint recovers after warning enqueued | Guard 4 drops it |
| Warn and disable both fire in one sweep | Warn pass runs first and its `enabled` predicate excludes rows the disable pass just turned off; only the disable email sends |
| Flapping endpoint (fails, recovers, fails) | `warn_notified_at` cleared on success → warns again. Bounded by `WarnThreshold` within `WarnWindow` |
| SMTP outage during a mass disable | `QueueNotify` isolates it; connection errors snooze; customer outbound delivery unaffected |
| Two server replicas sweep concurrently | The `UPDATE … WHERE enabled = true … RETURNING` is atomic; only one tx observes the transition |
| Sweep tx commits, process dies before River runs the job | Job is committed and durable — River picks it up. This is the whole point of the same-tx enqueue |
| Delivery succeeds on the attempt after the snooze cap fires | `TransitionSubscriberFailedIfPending` is conditional on still-pending; a delivered row is never clobbered |
| User re-enables with 128 rows snoozed | They all wake within the hour and POST. Intended — but see open question Q3 on the burst |

**Fail-closed choices:** an unknown/absent notification kind does not send; a missing owner
email cancels rather than retries forever; the snooze cap writes a terminal state rather
than leaving the row ambiguous.

## Scalability and extensibility notes

- **Sweep cost.** Two aggregate scans of `webhook_subscriber_deliveries` per 5 minutes,
  both grouped by `webhook_id` over a bounded window. The existing disable query already
  has this shape. The warn query filters on `attempts >= 1 AND last_error IS NOT NULL`,
  which is not covered by an index today — **verify the plan against a production-sized
  table before merging**; add a partial index if it seats.
- **Email volume** scales with failing endpoints, not with traffic, and is capped at one
  per webhook per episode by the two dedupe columns.
- **Deliberately narrow for v1:** two notification kinds, one recipient (the account
  owner), no batching, no preferences. The `Kind` field in the job args is the seam where a
  third kind lands without new plumbing.
- **What this makes easier later:** a `webhook.auto_disabled` *event* on the customer's own
  webhook feed, notification preferences, and digesting many failing endpoints into one
  email — all extend the notifier rather than restructure it.
- **What stays hard on purpose:** the 29h21m retry envelope is GA-frozen and untouched here.

## Verification strategy

Seams, fewest that carry real risk:

1. **The sweep's transactional boundary** (`AutoDisableFailingWebhooks`, DB-backed test).
   The highest-risk change — a bulk statement becoming a tx with enqueues. Assert: exactly
   one job per transition; a second sweep over the same state enqueues zero; a rolled-back
   tx leaves the webhook enabled *and* no job.
2. **The warn condition** (DB-backed, table-driven). This is where SC1 lives. Assert it
   fires on first-attempt failures, does not fire when any delivery succeeded, does not
   re-fire while `warn_notified_at` is set, and re-arms after a success clears it.
3. **The worker's guards** (unit, fake deliverer — `hitlnotify/worker_test.go` is the
   template). Each of the four guards, plus the three error-triage branches.
4. **The snooze cap** (unit, driving `Work` directly with a synthetic job at snooze counts
   23 / 24). Assert terminal write happens exactly at the boundary and that an
   already-delivered row is not clobbered.
5. **The UI toggle** (component test). Enable→disable→enable round trip, and the 409
   cooldown rendering as copy rather than an error.

Not worth a seam: the email bodies. Assert the reason string and the re-enable link are
present; do not snapshot the prose.

**Most likely regressions:** (a) forgetting to clear `warn_notified_at` on success, leaving
every recovered webhook permanently unwarnable — caught by seam 2's re-arm case; (b) the
tx conversion accidentally widening the sweep's predicate and disabling healthy webhooks —
caught by seam 1; (c) the snooze cap firing against rows whose webhook was re-enabled in
the meantime.

**Manual validation on staging:** register a webhook pointing at a deliberately 404-ing
endpoint, send inbound mail, confirm the warning email arrives within ~10 minutes, confirm
the toggle round-trips in the dashboard, and confirm pending deliveries terminate within
the cap rather than hanging. Note staging *can* exercise this fully — `email.received` is
inbound and the staging MX round trip works.

## Open questions

- **Q1. Warn threshold and window.** Proposed 5 failures / 24h. 5 is low enough to fire on
  the real incident within one sweep and high enough that a single blip does not mail
  anyone. Wants one sanity check against real delivery volume before it is frozen.
- **Q2. Snooze cap value.** Proposed 24 (≈24h). Trades maintenance-window grace against how
  long a delivery may sit `pending`. If the answer is "no one disables an endpoint for a
  whole day", 6–8 is defensible and tightens SC3.
- **Q3. The 128 stranded deliveries on the live account.** Shipping the cap terminates them
  as `failed` on first wake after deploy, which is honest but final. Sharper than it first
  looks: because replay is gated on `matched_webhook_ids` (see Part 2), events published
  *after* the disable are not replayable to this endpoint at all — so those 128 rows, plus
  the 50 already terminally failed, are the **last recoverable events that account has**.
  Terminating them without contacting the customer first forecloses their only path back.
  **This is a product call, not a technical one**, and it should be made before this deploys.
- **Q7 (follow-up, separate issue). Should fan-out record would-have-matched-but-disabled
  subscribers in `matched_webhook_ids`?** It would make auto-disable a genuinely recoverable
  pause instead of a forward-lossy one, turning the 30-day retention into a real safety net.
  Out of scope here, but it is the change that would most reduce the cost of the breaker.
- **Q4. Should the warning email also fire for a manually-disabled webhook accumulating
  failures?** Currently no — the warn predicate requires `enabled`. Arguably a user who
  disabled an endpoint does not want mail about it. Assumed yes-skip; flagging in case.
- **Q5. Does `auto_disable_reason` need to survive re-enable?** Re-enabling clears
  `auto_disabled_at`; the reason should probably clear with it, or the UI will show a stale
  cause on a healthy webhook.
- **Q6. Sender brand: `support@team.tokencanopy.com` vs `support@agents.e2a.dev`.**
  Specified as the former, and it works. Flagging only because a customer who bought *e2a*
  receives mail about their e2a webhook from a *tokencanopy.com* address, which can read as
  unrelated or as phishing — the risk being that the one email telling them their
  integration is broken is the one they ignore. A `support@agents.e2a.dev` agent already
  exists ("e2a Support") if brand alignment turns out to matter more than company
  consolidation. Purely a positioning call; the design works either way since the address
  is configuration.

## Implementation notes (2026-08-08, as built)

The implementation follows this design; the deviations and resolved
decisions, so the doc describes what actually shipped:

- **Pass ordering.** The sweep runs the DISABLE pass first, then the warn
  pass — the ordering the edge-case table's own rationale describes ("its
  `enabled` predicate excludes rows the disable pass just turned off"),
  though its first clause said the reverse. With disable-first, a burst
  crossing both thresholds in one sweep enqueues only the disable email;
  worker guard 3 remains the backstop for warn jobs already enqueued in an
  earlier sweep. Pinned by
  `TestAutoDisableWorker_BurstCrossingBothThresholdsSendsOnlyDisable`.
- **Guard 4's mechanism.** "A delivery has since succeeded" is detected via
  the dedupe column itself: a successful delivery clears `warn_notified_at`
  (same tx as the `last_delivered_at` bump), so the worker drops any
  warning job whose webhook shows `warn_notified_at IS NULL`.
- **Q5 resolved: yes** — re-enable clears `auto_disable_reason` alongside
  `auto_disabled_at`, so a healthy webhook never shows a stale cause. The
  API field is `auto_disabled_reason` (additive, optional, open set); the
  column is `auto_disable_reason`.
- **Constants** are named and marked tunable, values still as proposed
  (unfrozen): `identity.WarnThreshold`/`identity.WarnWindow` (5 / 24h) and
  `webhookdelivery.MaxDisabledSnoozes` (24).
- **Sender address (final decision, resolves Q6 and supersedes Part 4's
  `support@team.tokencanopy.com`).** The hosted pair is
  `From: support@send.e2a.dev` with `Reply-To: support@agents.e2a.dev`,
  both set via ops config. Rationale: `agents.e2a.dev` is the shared agent
  domain and is NOT an SES sending identity (it publishes no SPF and no
  DMARC), so mail cannot be sent From it — `send.e2a.dev` is the domain
  with the real sending identity (`v=spf1 include:amazonses.com ~all`,
  `DMARC p=none`, MAIL FROM at `mail.send.e2a.dev`). This is exactly the
  `sent_as=relay` pattern shared-domain agent sends already use in
  production: From rewritten onto the relay domain, Reply-To carrying the
  real address. **Reply-To is load-bearing on the hosted service** —
  `send.e2a.dev` has no mailbox, so without it a reply goes nowhere; the
  `support@agents.e2a.dev` inbox is where replies land. In the OSS repo
  both stay pure configuration, never constants: `notifications.
  from_address` and the new `notifications.reply_to`
  (`E2A_NOTIFICATIONS_FROM_ADDRESS` / `E2A_NOTIFICATIONS_REPLY_TO`, each
  validated as a bare address). Unset from_address falls back to
  `webhooks-noreply@<outbound_smtp.from_domain>`; unset reply_to emits NO
  Reply-To header (replies follow From — the sane self-host default,
  since a self-hoster's from_address is typically a real mailbox and has
  no `agents.e2a.dev`). The notifier is gated on
  `outbound_smtp.from_domain` alone — without `http.public_url` the
  emails still send, with generic dashboard copy instead of a link.
- **Message-ID** is deterministic per EPISODE (webhook id + the warn stamp
  / disable timestamp), not per webhook as in hitlnotify — a webhook can
  legitimately warn again after recovering, and a webhook-keyed id would
  make Message-ID-deduping recipients swallow the second episode's email.
- **Snooze cap failure handling.** If the terminal write at the cap fails,
  the worker returns the error (River retries) instead of completing the
  job over an ambiguous row. The terminal transition emits a new
  `webhook_disabled` outcome on `e2a_webhook_delivery_terminal_total`.
- **The Part 4 SES checkpoint** (runtime credentials permitted to send as
  the configured From) is an ops-side pre-deploy verification; it cannot be
  checked from this repo.
- **DKIM (design correction).** Part 4's "mirror hitlnotify" hid a gap:
  `SMTPRelay.SendOnce` performs no DKIM signing (that lives in
  `outbound.Sender`), and an upstream like SES only signs identities IT
  manages — a BYODKIM custom `notifications.from_address` domain would
  have gone out with no DKIM leg at all, leaving the alert riding SPF
  alignment alone (spam on any SPF-breaking forward). The notifier now
  signs in-process for the From-header domain via the extracted
  `outbound.SignWithDKIM` (the same lookup + `internal/dkim` signing the
  Sender uses — one copy of the logic, no new crypto), applied after the
  Message-ID prepend so the signature covers the final header set.
  Fail-open: no stored key (the zero-config self-host path) sends
  unsigned, exactly as before — a missing key is never an error. With the
  final `support@send.e2a.dev` sender this has a real identity to align
  to: `send.e2a.dev` is the deployment's actual sending domain, so when a
  key exists the signature is DMARC-aligned with From.
