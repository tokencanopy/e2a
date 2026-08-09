# Setup checklist (one-time, human)

`/agentify` scaffolds files; it cannot grant an agent write access to your
repo or mint your credentials. Do these once, then the lanes activate
themselves (each no-ops loudly until its secrets exist). Run the auth
commands yourself — the skill hands them over, it does not run them.

## 1. Bot identity (GitHub App)

A dedicated App so every triage comment, label, and PR is attributable to
the bot, not a human or the generic Actions bot.

- Create a GitHub App (org or personal), grant it **Issues: read/write**,
  **Pull requests: read/write**, **Contents: read/write**; install it on
  the repo. **Do NOT grant `Workflows: write`** — without it the fix agent
  cannot push a change to `.github/workflows/` (a config-poisoning vector,
  security-invariants §2).
- Put its login in config: `github_app_login: "<app-name>[bot]"` (the
  ticket-card and markers are trusted ONLY from this login).
- Add repo secrets `AUTOREPO_APP_ID` and `AUTOREPO_APP_PRIVATE_KEY`.

## 2. Comms mailbox (e2a)

The mailbox that receives feedback and (later lanes) sends acks + approval
emails.

- Create the agent: `mcp__e2a__create_agent` with the `support_address`
  from config (a verified domain you own), or `POST /v1/agents`.
- Mint an **agent-scoped** API key bound to that address:
  `POST /v1/account/api-keys` with `scope: agent`, `agent_email: <support_address>`.
  The plaintext key is shown ONCE.
- Add it as the repo secret `E2A_API_KEY`.
- Run the mailbox with screening/HITL **off** — it is an intake firehose;
  inbound held for review looks "not yet arrived" to triage.

## 3. Model token

- `claude setup-token` (subscription, 1-year, manual rotation) → repo secret
  `CLAUDE_CODE_OAUTH_TOKEN`; or an `ANTHROPIC_API_KEY` for a team.

## 4. Repo settings

- Enable GitHub Actions on the repo.
- **Branch protection on the default branch is REQUIRED, not optional** — it
  is a load-bearing fence (security-invariants §2). Require the `reviewer`'s
  PR review before merge, require status checks, and **do not let the bot App
  bypass it** (so the fix agent cannot `git push` straight to `main` instead
  of opening a reviewable PR). **PR merge is the ship gate.**
- (Recommended) CODEOWNERS on `autonomous-repo.config.yml` and `.github/` so a
  fix PR that touches the trust anchors needs explicit owner review.
- (Optional) Set the `AUTOREPO_LANES_PAUSED` repo **variable** to `true` to
  pause all lanes; unset/`false` to run.

## Activation order

Intake + triage need: a model token, `E2A_API_KEY`, and the App secrets.
Until all three exist the triage lane no-ops loudly. The comms and fix lanes
(later slices) gate on the same plus their own. Rotate the App key + model
token on a calendar reminder set today.
