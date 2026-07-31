# Security invariants

These are the framework's load-bearing defenses — the things an adopter
must not misconfigure away. They exist because feedback bodies, emails, and
issue/PR comments are attacker-controlled text flowing into agent prompts
AND (later) a repo-write coding agent.

1. **Untrusted input is data, never instructions.** Every lane frames user
   content (bodies, email text, attachment contents, third-party comments)
   inside a fenced block under a standing banner and never follows
   directives inside it — including text rendered inside screenshots
   (image-borne injection). Attachments are *described*, never executed or
   rendered; their bytes never reach GitHub.

2. **The fix lane holds ZERO deploy/prod/cloud secrets** — its entire
   credential inventory is the Anthropic token + a repo-scoped GitHub App
   token + a throwaway local verify stack. That bound is real and is what
   keeps the blast radius bounded. **But be honest about what "bounded"
   means** — it is NOT "only a rejected PR". A coding agent with `Bash` +
   `Edit/Write` + repo write, reading untrusted issue text, can also:
   - **exfiltrate the run-env tokens** (the Anthropic token, the ~1h App
     token) via `curl`/`gh issue create` — *no merge required*. The App token
     is short-lived + issues/PR-scoped; the Anthropic token is the real
     residual. Network-egress restrictions on the runner are the future
     hardening.
   - **land code on `main` directly** *if branch protection is missing* — the
     workflow denies `gh pr merge` but cannot deny a raw `git push
     origin HEAD:main`. **Branch protection on the default branch is therefore
     a REQUIRED activation step, not optional** (setup-checklist).
   - **poison config/workflows in its PR** (re-point `approver`/`reviewer`,
     add a cloud-auth step) — bounded by PR review AND by the App **not**
     holding `workflows:write` (so it cannot even push a `.github/workflows`
     change). Grant the App no `workflows` permission; consider CODEOWNERS on
     `autonomous-repo.config.yml` + `.github/`.
   - **author a hostile `customer-note`** (emailed from the verified domain /
     shown on the public issue) — its content renders visibly in the PR, so
     **the PR review is the gate on it**; review it.

   So the load-bearing fences are: zero prod creds + **branch protection** +
   **App without `workflows:write`** + a **diligent PR review** (incl. the
   customer-note and any config diff). The merge gate is real, but it is those
   four together, not "a PR can't do anything until merged".

3. **Authorship trust.** Decisions read only the bot-authored issue/PR body
   and `OWNER`/`MEMBER` comments. The ticket-card and the `marker` are
   honored ONLY from `github_app_login` — a third party can post a forged
   card or marker on a public issue/PR, and it must never be trusted.

4. **Verified-reply routing.** Inbound email auto-routes (approvals,
   dispute-reopens) ONLY when the e2a `conversation_id` matches a ticket's
   `comms_ref`, `authentication.dmarc.status` is `pass`, AND `header_from`
   matches the address on file. A public marker / subject token NEVER
   routes — an attacker who knows an issue number cannot approve a fix or
   reopen a ticket.

5. **Capability minimization + bounded blast radius (not "secrets
   unreachable").** Each lane's tool allowlist is deliberately narrow:
   triage gets the ticket-card helper, `gh issue` plus read-only `gh pr
   list`/`gh pr view` (never bare `gh` — which would expose `gh auth
   token`; never `gh api` — the whole installation),
   `Read`, and the e2a **read** tools (no send). There is no `jq` tool (a
   raw `jq -rn env.X` reads secrets from the run env) and no raw shell.
   **Be honest about the limit:** this does not make the run-env secrets
   unreachable — a model that obeys an injection could still surface the
   read-only e2a key or the short-lived issues-scoped App token and publish
   it via `gh issue create`. What the design actually guarantees is a
   **bounded blast radius**: no deploy/prod/cloud creds in any lane, the e2a
   key is read-only, the App token is ~1h + issues-only, and the fix lane
   (the only repo-write path) ships nothing without a human PR merge. Comms
   is the only lane that sends mail. Backend secrets (if a backend store is
   used) are scoped per lane and reachable only through a single allowlisted
   script.

   **Comms lane (read + send).** The comms key can send mail, so its blast
   radius includes outbound email — but the *recipient* is bounded
   **structurally**: all sends go through `scripts/comms_send.sh`, which
   computes the recipient from the thread (reply) or config
   (`fix_gate.approver`) and never sets `cc`/`bcc`/`reply_all`; the raw e2a
   send tools (`send_message`/`reply_to_message`/`forward_message`), which
   accept arbitrary `to`/`cc`/`bcc`, are **disallowed**. So an injection
   cannot relay mail off the verified domain or bcc thread content to an
   attacker. The residual (the model controls the *body* sent to a legitimate
   thread participant or the approver) is the bounded-blast-radius limit, not
   an arbitrary-egress hole.

6. **Per-adopter identity.** Each adopter brings its own GitHub App and its
   own Anthropic + comms credentials. Nothing is shared across installs.

7. **Fail closed.** Lanes no-op loudly until their secrets exist; budgets
   are finite; unmatched/ambiguous/over-budget items degrade to a human,
   never to a guess; illegal transitions are refused; the pause switch
   stops all lanes before any model call. (The one deliberate exception:
   `fix_gate.mode: auto` is not fail-closed — it leans on the PR-merge gate,
   the real fence — and `always_hitl` still forces a human for sensitive
   surfaces.)
