# Email-agent evaluation suite

This directory contains a synthetic, single-turn evaluation suite. Keep the
target and actor values in environment variables, use only dedicated test
mailboxes, and review each expected recipient before running an evaluation.

The `results/` directory is intentionally ignored. Add multipart fixtures or
attachment metadata beneath `fixtures/` when a case needs them.
