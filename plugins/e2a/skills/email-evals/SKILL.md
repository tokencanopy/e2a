---
name: email-evals
description: Author and safely run deterministic email-agent evaluation suites with dedicated e2a test agents. Use when a user wants to define synthetic email cases, validate a suite, inspect its dry-run plan, run it after approval, or regrade changed assertions.
---

# Email evals

## Gather the suite one answer at a time

Ask one logical question at a time; do not dump a questionnaire. Ask, in order:

1. Ask for the use case.
2. Ask whether the target runtime already exists and is running. This skill does not build or start the target agent runtime.
3. Ask for the dedicated actor and target environment-variable names.
4. Ask for the expected action and exact allowed recipients.
5. Ask for the sender and Reply-To expectations.
6. Ask for the thread and subject expectations.
7. Ask for required facts and forbidden patterns.
8. Ask for attachments, timeout, and lifecycle expectations.

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
plugins/e2a/skills/email-evals/email-evals.sh scaffold --root <suite-root> --name <suite-name> --target-env <target-env> --actor-env <actor-env> --api-key-env <api-key-env>
```

Edit the generated `suite.yaml` and case YAML files to reflect the answers. Then install the pinned suite-local runtime and validate it:

```sh
plugins/e2a/skills/email-evals/email-evals.sh setup --root <suite-root>
plugins/e2a/skills/email-evals/email-evals.sh validate --suite <suite-root>/suite.yaml
```

Show the complete alias-only dry-run plan and protection failures. Validation is the preflight gate; do not run a suite while it reports a protection or capability failure.

## Request approval immediately before sending

Ask for explicit user approval immediately before `run`, because it sends real email between the dedicated agents. Do not treat earlier authoring answers as approval.

Only after that approval, run:

```sh
plugins/e2a/skills/email-evals/email-evals.sh run --suite <suite-root>/suite.yaml
```

## Inspect and iterate

Read the generated `report.md`. Summarize deterministic failures without hiding errors, then propose the smallest case/agent change that addresses each failure.

When only assertions changed, use `regrade` instead of `run`; regrade performs no sends:

```sh
plugins/e2a/skills/email-evals/email-evals.sh regrade --suite <suite-root>/suite.yaml --run <run-dir>
```

## V0 limits

Keep claims within the launch slice: no semantic judge, no deep HTML equivalence, no scheduled-send proof, and no full review/bounce/complaint matrix. Do not promise simulator, model, or data-generator features.
