# Ticket state machine (shared spec)

Product-neutral. Every `ticket_store` adapter honors the SAME transitions;
the runtime skill is the single source of truth, the store just persists.
In the **github** store, the authoritative status is the `status` field of
the ticket-card (see `ticket-card.md`); labels are the human-visible
projection of it, kept in sync on every transition.

## States

| status | meaning | github projection |
|---|---|---|
| `triaged` | classified, issue exists, not yet gated | open + `status:triaged` |
| `awaiting_approval` | hitl fix gate: approval email owed/pending | open + `status:awaiting-approval` |
| `in_progress` | a fix PR is open | open + `status:in-progress` |
| `shipped` | the fix PR merged (released) | closed, reason `completed` |
| `closed_duplicate` | folded into another ticket | closed, reason `duplicate` |
| `closed_wontfix` | human declined | closed, reason `not_planned` + `wontfix` |
| `closed_noise` | not actionable / a question | closed, reason `not_planned` |

## Forward edges

```
triaged ─(mode:auto, agent-fix applied by triage)──────────► in_progress ─► shipped
triaged ─(mode:hitl)─► awaiting_approval ─(approve)─► triaged+agent-fix ─► in_progress ─► shipped
                                          └(decline)─► closed_wontfix
triaged ─► closed_duplicate | closed_wontfix | closed_noise
```

On approval the ticket returns to `triaged` carrying the `agent-fix` label —
the fix lane consumes the label the same way it does an `auto`-mode label, so
there is one fix path, not two.

## Recovery edges (each exists because a specific actor needs it)

| edge | driver | trigger |
|---|---|---|
| `shipped → triaged` | comms lane | filer disputes the fix (verified reply) |
| `closed_duplicate → triaged` | comms lane *(deferred — follow-on with the dup-filer fan-out; no v0 driver)* | filer disputes the dup verdict (verified reply) |
| `closed_noise → received*` | comms lane *(deferred — follow-on with the dup-filer fan-out; no v0 driver)* | filer supplies substance (verified reply) |
| `triaged → awaiting_approval` | comms lane | fix_gate hitl: approval-request emailed to the approver |
| `awaiting_approval → triaged` | comms lane | approver **approves** (verified reply); `agent-fix` applied |
| `awaiting_approval → closed_wontfix` | comms lane | approver **declines** (verified reply); reason recorded |
| `in_progress → triaged` | triage sweep | fix PR closed unmerged — re-arms the gate |
| `in_progress → shipped` | release callback / triage sweep | PR merged (callback, or merged >24h repair) |
| `triaged → closed_wontfix` | triage sweep | human applied the `wontfix` label |

\* in the github store there is no separate `received` row; "re-enters
triage" means reopening the issue and clearing the close — see the store.

## Rules

1. **Transitions are requests, validated against this table.** An illegal
   edge is refused. In the github store the runtime skill enforces it
   before patching; a concurrent change that already moved the ticket is
   discovered by re-reading the ticket-card (treat "already where I wanted
   to go" as success, not an error).
2. **`duplicate_of` is one level deep.** The target must itself have
   `duplicate_of == null` and must not be the ticket itself. No chains.
3. **The fix lane never owns `→ shipped`.** Only the release callback (or
   the missed-callback triage sweep) drives it. The fix lane sets
   `in_progress` together with the PR number in one patch — a run that dies
   before the PR exists leaves the ticket `triaged` (self-healing), never a
   dangling `in_progress` with no PR.
4. **`awaiting_approval` exists only under `fix_gate.mode: hitl`.**
