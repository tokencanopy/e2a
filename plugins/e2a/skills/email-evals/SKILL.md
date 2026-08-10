---
name: email-evals
description: Author and safely run deterministic email-agent evaluation suites with dedicated e2a test agents. Use when a user wants to define synthetic email cases, validate a suite, inspect its dry-run plan, run it after approval, or regrade changed assertions.
---

# Email evals

## Gather the suite one answer at a time

Ask one logical question at a time; do not dump a questionnaire. Ask exactly one prompt, wait for the answer, then advance. Conditionally omit a later prompt only when an earlier answer proves its field irrelevant; never bundle prompts.

<!-- email-evals:authoring-prompts:start -->
1. <!-- email-evals:field=use-case --> **use case** — Ask: “What is the synthetic use case?”
2. <!-- email-evals:field=existing-target-runtime --> **existing target runtime** — Ask: “Does the existing target runtime already run?”
3. <!-- email-evals:field=dedicated-actor-environment-name --> **dedicated actor environment name** — Ask: “What is the dedicated actor environment name?”
4. <!-- email-evals:field=dedicated-target-environment-name --> **dedicated target environment name** — Ask: “What is the dedicated target environment name?”
5. <!-- email-evals:field=expected-action --> **expected action** — Ask: “What is the expected action?”
6. <!-- email-evals:field=exact-allowed-recipients --> **exact allowed recipients** — Ask: “Which exact allowed recipients are required?”
7. <!-- email-evals:field=sender --> **sender** — Ask: “What is the sender?”
8. <!-- email-evals:field=reply-to --> **Reply-To** — Ask: “What is the Reply-To expectation?”
9. <!-- email-evals:field=thread --> **thread** — Ask: “What is the thread expectation?”
10. <!-- email-evals:field=subject --> **subject** — Ask: “What is the subject expectation?”
11. <!-- email-evals:field=required-facts --> **required facts** — Ask: “What required facts apply?”
12. <!-- email-evals:field=forbidden-patterns --> **forbidden patterns** — Ask: “What forbidden patterns apply?”
13. <!-- email-evals:field=attachments --> **attachments** — Ask: “What attachments are expected?”
14. <!-- email-evals:field=timeout --> **timeout** — Ask: “What timeout applies?”
15. <!-- email-evals:field=lifecycle --> **lifecycle** — Ask: “What lifecycle outcome is required?”
<!-- email-evals:authoring-prompts:end -->

This skill does not build or start the target agent runtime.

Refuse real customer messages, identifiers, customer domains, or production-derived fixtures. Immediately propose a synthetic replacement, such as a fictional order identifier and a `.test` mailbox. Keep every case and attachment synthetic.

Do not scaffold until these answers are sufficient to make deterministic assertions.

## Contain the evaluation before setup

Never change e2a protection. Tell the user to configure containment separately, in the same dedicated account for e2a, with an account-scoped API key:

- actor: `allowlist/block [target]`
- target: `allowlist/block [actor, probes...]`

Use dedicated actor and target agents only. Do not infer, broaden, or repair protection settings; surface protection failures during validation instead.

## Scaffold, edit, and validate

After gathering sufficient answers, scaffold the suite:

```sh
plugins/e2a/skills/email-evals/email-evals.sh scaffold --root <suite-root> --name <suite-name> --target-env <target-env> --actor-env <actor-env>
```

Edit the generated `suite.yaml` and case YAML files to reflect the answers. The
credential name is fixed outside suite authority: export only
`E2A_EVAL_API_KEY`. YAML interpolation is limited to the documented actor,
target, and probe mailbox fields; never interpolate names, actions, timing,
subjects, bodies, patterns, or attachment metadata.

Then verify the checked-in, single-file plugin runtime bundle and validate the
suite. Setup performs no package installation and never copies or executes
JavaScript or dependencies beneath the suite root:

```sh
plugins/e2a/skills/email-evals/email-evals.sh setup --root <suite-root>
plugins/e2a/skills/email-evals/email-evals.sh validate --suite <suite-root>/suite.yaml
```

Show the complete alias-only dry-run plan, its `approvalDigest`, and any
protection failures. Validation is the preflight gate; do not run a suite
while it reports a protection or capability failure. Treat the digest as an
opaque value; it binds the resolved identities, origin, containment posture,
stimuli, assertions, recipients, and execution limits without revealing them.

The public `https://api.e2a.dev` origin is the default. If the suite names a
custom/local origin, the operator must independently authorize the exact
origin on every command with `--trusted-origin <origin>`; cleartext origins
are restricted to loopback. Never infer this flag from suite content.

## Request approval immediately before sending

Ask for explicit user approval immediately before `run`, because it sends real email between the dedicated agents. Do not treat earlier authoring answers as approval.

Only after that approval, run:

```sh
plugins/e2a/skills/email-evals/email-evals.sh run --suite <suite-root>/suite.yaml --approval-digest <approvalDigest-from-validate>
```

If the suite, resolved mailbox identities, custom origin, protection posture,
or plan changed since validation, `run` fails closed. Validate again, show the
new complete plan, and request fresh approval; never substitute or guess a
digest.

## Inspect and iterate

Read the generated `report.md`. Summarize deterministic failures without hiding errors, then propose the smallest case/agent change that addresses each failure.

When only assertions changed, use `regrade` instead of `run`; regrade performs no sends:

```sh
plugins/e2a/skills/email-evals/email-evals.sh regrade --suite <suite-root>/suite.yaml --run <run-dir>
```

Regrade accepts a changed full-suite digest only when the execution digest is
unchanged. It always uses the current validated assertions and rejects changes
to sending, correlation, timing, containment, actor, target, or origin inputs.
The output root also contains the private mode-`0600`
`.email-evals-artifact-auth-key`, which authenticates redaction-loss metadata
independently of the rotatable API key. Keep that hidden file with its run
directories; never publish it or place it inside a suite. Regrade fails closed
for redacted runs if this authentication root is missing or replaced.
If body evidence required configured-pattern redaction, the forbidden-pattern
set must remain identical (reordering is allowed); changing that set fails
closed because arbitrary new regexes cannot be evaluated against erased text.

## V0 limits

Keep claims within the launch slice: no semantic judge, no deep HTML equivalence, no scheduled-send proof, and no full review/bounce/complaint matrix. Do not promise simulator, model, or data-generator features.
