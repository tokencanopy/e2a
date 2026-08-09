---
name: email-evals
description: Author and safely run deterministic email-agent evaluation suites with dedicated e2a test agents. Use when a user wants to define synthetic email cases, validate a suite, inspect its dry-run plan, run it after approval, or regrade changed assertions.
---

# Email evals

## Gather the suite one answer at a time

Ask one logical question at a time; do not dump a questionnaire. Ask exactly one prompt, wait for the answer, then advance. Conditionally omit a later prompt only when an earlier answer proves its field irrelevant; never bundle prompts.

<!-- email-evals:authoring-prompts:start -->
1. **use case** — Ask: “What is the synthetic use case?”
2. **existing target runtime** — Ask: “Does the existing target runtime already run?”
3. **dedicated actor environment name** — Ask: “What is the dedicated actor environment name?”
4. **dedicated target environment name** — Ask: “What is the dedicated target environment name?”
5. **expected action** — Ask: “What is the expected action?”
6. **exact allowed recipients** — Ask: “Which exact allowed recipients are required?”
7. **sender** — Ask: “What is the sender?”
8. **Reply-To** — Ask: “What is the Reply-To expectation?”
9. **thread** — Ask: “What is the thread expectation?”
10. **subject** — Ask: “What is the subject expectation?”
11. **required facts** — Ask: “What required facts apply?”
12. **forbidden patterns** — Ask: “What forbidden patterns apply?”
13. **attachments** — Ask: “What attachments are expected?”
14. **timeout** — Ask: “What timeout applies?”
15. **lifecycle** — Ask: “What lifecycle outcome is required?”
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
