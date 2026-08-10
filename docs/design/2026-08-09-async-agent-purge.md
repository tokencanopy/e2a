# Bounded permanent agent purge

Status: accepted
Related: `docs/design/trash-soft-delete.md`, `docs/design/async-message-pipeline.md`

## Problem

Permanent agent deletion currently removes every message and its cascade
children in one transaction. The transaction, lock set, WAL volume, River job
cancellation work, and pooled-connection hold time therefore grow without a
bound. The retention janitor uses the same unbounded shape.

Splitting the work into committed chunks solves that resource problem, but it
also removes the old all-or-nothing property. A safe chunked protocol must
prevent four failures:

- restoring an inbox after only part of it has been destroyed;
- a stale purge deleting a later inbox that reused the same email address;
- a message accepted between the last message chunk and parent deletion being
  removed only by the foreign-key cascade, bypassing job cancellation,
  storage accounting, and `messages_deleted`;
- the threshold decision or retention janitor reintroducing an unbounded
  transaction.

## Goals

- Bound every committed unit of database and River cancellation work.
- Keep `DELETE ...?permanent=true` synchronous and preserve its response shape.
- Make an interrupted purge resumable but irreversible.
- Bind every purge operation to one immutable incarnation.
- Delete messages explicitly while the agent row still exists.
- Free the email address only after the guarded drain is complete.

## Non-goals

- A general long-running-operation resource or background purge queue.
- Changes to account, domain, or import-batch deletion.
- A new success response or SDK-visible resource shape.
- Revalidating every inbound and outbound producer in this change. The final
  sealing transaction supplies the deletion-side correctness boundary.

## Schema and state

Add nullable `agent_identities.purge_token`.

- `NULL`: the agent is live or normally trashed and may be restored.
- non-`NULL`: permanent purge has been claimed and the row may only advance
  toward deletion.

The token is generated once while the owned agent row is locked. A retry adopts
the existing token; it does not replace it. Every chunk and the finalizer match
`id`, `user_id`, and the exact token. This makes the token the incarnation
identifier even though the primary key is the reusable email address.

A database constraint requires `purge_token IS NULL OR deleted_at IS NOT NULL`.
Claim also resets `deleted_at` to the current time. Together these are the
rolling-deploy safety rails: an older binary that attempts its legacy restore
(`deleted_at = NULL`) fails closed, while its retention janitor cannot
immediately select an old, already-expired trash row and run the legacy
unbounded purge. A binary rollback must not reissue permanent deletion against
a token-bearing row; the compatible recovery action is to roll forward and let
the token-aware janitor resume it.

Restore of a token-bearing row returns HTTP `409 purge_in_progress`. The token
is intentionally not exposed in the API.

## Permanent-delete flow

The HTTP handler carries the resolved agent's `created_at` into the store, so a
request delayed before claim cannot attach to a same-owner recreation at the
same address. `DeleteAgent` begins a transaction and locks that exact owned
agent row `FOR UPDATE`.
Under that lock it checks for a fresh provider-call lease, then uses bounded
existence probes to choose the deletion shape. The decision considers all work
that must be bounded:

- more than 5,000 total messages;
- more than 500 outbound messages whose River jobs must be canceled; or
- more than 5,000 contact engagements.

Each probe reads at most its limit plus one row. If total messages are within
the atomic bound, the transaction locks that entire bounded message set before
probing job state. This prevents an existing-message approval or reconciler
from creating jobs after classification. The count and decision are not
performed before the agent lock.

If every probe is within its bound, the existing atomic transaction cancels the
bounded jobs, explicitly deletes messages and engagements, deletes the agent,
and returns the exact message `RowsAffected` value.

Otherwise the same transaction resets `deleted_at`, creates or adopts
`purge_token`, and commits. The request then synchronously drains the claimed
incarnation in bounded transactions. A caller retry adopts the token and
continues the drain.

## Bounded drain

Every drain transaction locks and verifies the exact claimed row and refuses a
fresh provider-call lease. A partial index on `(agent_id, send_claimed_at)` for
active `sending` rows keeps that repeated guard proportional to active sends
instead of rescanning the shrinking inbox once per chunk.

1. Delete at most 500 outbound `accepted` or `sending` messages with a
   `send_job_id`, canceling exactly the returned River jobs in the same
   transaction. Repeat while rows are found.
2. Delete at most 5,000 remaining messages. The query excludes the job-bearing
   rows from step 1, so this path cannot accidentally perform 5,000 River
   cancellations. Repeat while rows are found.
3. Delete at most 5,000 contact engagements, scoped by both `user_id` and
   `agent_id`. Repeat while rows are found.

Missing row or token mismatch means this purge no longer owns a target. The
drain terminates immediately; it must not advance through later phases and
attach to a recreated row.

`messages_deleted` is the sum of message rows explicitly deleted by this
request. It remains an exact `RowsAffected` receipt.

## Final sealing transaction

A zero-row chunk is not completion. Between committed chunks, a producer that
resolved the previously-live agent may still insert a message: the foreign key
requires the parent row to exist but does not inspect `deleted_at`.

Completion therefore happens in one sealing transaction that:

1. locks and verifies the exact claimed agent row;
2. locks at most 500 residual messages without first classifying their state,
   then cancels every job visible on those locked rows and deletes them;
   if any were found, it commits and returns to the drain;
3. locks at most 5,000 engagement IDs and, if any are found, deletes those
   exact rows, commits, and returns to the drain; only an empty locked set is
   a valid completion signal because a concurrent update can change `ctid`;
4. only when both locked sets are empty, deletes the agent before releasing
   the row lock.

The parent-row lock conflicts with the foreign-key lock required by a new
message insert. A producer that arrives before the seal commits first and is
found by the guarded probe; one that arrives after the seal waits and then
fails its foreign-key check. Locking residual messages before inspecting their
job state similarly serializes an existing-message transition. There is no
zero-check-to-parent-delete or state-classification gap.

## Retention janitor

The janitor is a resumer of this same state machine, not a separate bulk-delete
implementation. For an expired normally-trashed row it locks the row, creates
a token, and drains it with the same bounds. For any row already bearing a
token, it adopts that token and resumes. One invocation still caps the number
of agents it completes.

This makes interruption and process restart ordinary retry cases. No fallback
path may scan or cancel all work for an agent in one transaction.

## Concurrency invariants

- A claimed purge can never restore.
- A stale purge can never operate on another token, even for the same owner and
  same email address.
- The final zero checks and parent deletion share one lock-holding transaction.
- Job-bearing messages are deleted only in cancellation-sized chunks.
- A parent delete never carries meaningful message cascades.
- A missing token ends the whole attempt; it is not interpreted as permission
  to continue into another phase.

## Verification

Required tests cover:

- restore returning `409 purge_in_progress` after a committed chunk;
- same-owner delete/recreate/delete ABA with two concurrent attempts;
- inbound insertion and outbound insertion plus River enqueue at the
  zero-to-final boundary;
- exact `messages_deleted` accounting for late rows;
- interruption followed by a request retry and by a janitor retry;
- bounded classifier behavior for total messages, cancellable jobs, and
  engagements;
- the fresh-send guard using its partial active-send index;
- job cancellation never exceeding 500 rows in a transaction;
- schema rollback safety: a claimed row cannot have `deleted_at` cleared.

The OpenAPI success shape is unchanged. Its restore documentation includes the
new `409 purge_in_progress` state.
