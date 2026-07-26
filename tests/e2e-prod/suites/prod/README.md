# Prod-only suites

Suites here verify behavior that **staging structurally cannot produce**, so they
run only when the target is production.

## Why a directory, not a runtime skip

The obvious implementation is a guard at the top of each test — "if not prod,
`return`". Do not do that. A test that silently returns is indistinguishable
from a test that passed, and this repo has already been bitten by exactly that:
`get_message`'s happy path used to `return` when the inbox was empty, so the
suite reported 18/18 green while never once exercising the tool.

Selection is therefore by **file glob**, not by runtime branch:

| Script | Runs | Target |
|---|---|---|
| `npm test` | `suites/*.test.ts` | staging (and any non-prod deployment) |
| `npm run test:prod` | `suites/*.test.ts` **+** `suites/prod/*.test.ts` | production |

On staging these files are never loaded, so they cannot report a false pass.
On prod they are loaded and must genuinely pass. There is no third state.

## What belongs here

Only things staging cannot do. As of writing, staging cannot:

- receive mail over a real external **MX** (it has none; a distinct agent→agent
  send egresses via SES and fails)
- exercise **SES delivery feedback** — the `e2a-staging-smtp` IAM policy denies
  `ses:SendRawEmail` to the bounce/complaint simulators, so `email.delivered`,
  `email.bounced`, `email.complained` and the suppression events they cause can
  never fire there
- provision a real **SES sending identity** (DKIM) for `domain.sending_verified`
  / `domain.sending_failed`
- serve the prod-only marketing routes (e.g. `/pricing`)

If staging *can* do it, the test belongs in `suites/`, not here — a prod-only
test that didn't need to be prod-only just narrows where it runs.

## Requirements

These run against the real service with a real account, so:

- **Self-provisioning.** Create every fixture you need and clean it up. No
  manual setup steps — a suite that needs a human to prepare state cannot be
  scheduled later.
- **Use the dedicated conformance account**, never a real one. `E2E_ALLOW_PROD=1`
  is required by `harness/env.ts` before any prod target is accepted.
- **Mind persistent state.** Bounce/complaint simulator sends create real
  suppression entries on the account. That is the point (a real bounce is the
  only black-box way to create one), but it is durable — clean up what you can
  and document what you can't.
