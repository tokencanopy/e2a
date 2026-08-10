# Email-agent evaluation suite

This directory contains a synthetic, single-turn evaluation suite. Keep the
target and actor values in the named environment variables and provide the
credential only as `E2A_EVAL_API_KEY`. Use only dedicated test mailboxes and
review each expected recipient before running an evaluation. Suite files are
data only; setup verifies the checked-in single-file runtime bundle and never
installs, creates, or executes JavaScript beneath this directory.

The public API origin is the default. A suite that names a custom origin is
accepted only when the operator repeats that exact origin with
`--trusted-origin` on validate, run, and regrade.

Validation prints an alias-only plan and an `approvalDigest`. A live run must
repeat that exact digest with `--approval-digest`; any plan, identity,
containment, or origin change requires validation and approval again.

The `results/` directory is intentionally ignored. Add multipart fixtures or
attachment metadata beneath `fixtures/` when a case needs them.
