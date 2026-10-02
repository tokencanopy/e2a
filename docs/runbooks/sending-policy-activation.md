# Sending-policy activation and readiness

These are operator-only server commands. They do not enable sending protection
by themselves. Runtime defaults remain unchanged, and hosted deployment,
rollback compatibility, and activation approvals belong to the deployment
runbook. Do not change the running server's config merely to prepare a future
policy generation.

## Review a complete policy file

`-sending-protection-policy-file` accepts a complete runtime-policy JSON object
(not a server YAML config), up to 64 KiB. Unknown fields, duplicate object keys, invalid values,
trailing content, and non-regular files are rejected. Config defaults and
environment overrides never alter the file's policy. Read/validation errors do
not echo file paths or contents.

Inspect using the same binary and trust roots as the target deployment:

```sh
e2a -config config.yaml -sending-protection-policy \
  -sending-protection-policy-file reviewed-policy.json
```

The existing `config_policy_*` fields describe the local config. With a file,
`candidate_policy_sha256` and `candidate_policy_canonical` describe the actual
activation candidate. Review that canonical payload and hash as well as the
stored generation. Inspection does not modify policy or audit rows.

Activate the exact reviewed file:

```sh
e2a -config config.yaml -activate-sending-protection-policy \
  -sending-protection-policy-file reviewed-policy.json \
  -expected-generation 0 \
  -expected-policy-sha256 "$REVIEWED_POLICY_SHA256" \
  -reason "Carry current admission policy into database source"
```

Use the generation actually inspected; `0` above illustrates the first
activation. A mismatched candidate hash, stale generation, absent/mismatched
selected recipient commitment, or missing trust roots for enabled controls
rejects the operation without policy/audit writes. Normal startup migrations
still run before operator commands. Successful activation writes the next
generation and audit event atomically. After a lost response, inspect before
retrying. Without the file flag, inspection and activation retain their
existing config-payload behavior. The file flag is invalid with other commands.

## Preserve admission rules when switching sources

The migration-seeded generation zero predates the external-sending admission
gate. A deployment using that gate must activate a reviewed generation carrying
its current effective admission mode, cohort cutoff, and unlock set **before**
selecting database source. In an operator-approval-only deployment, preserve
`mode: enforce` and `unlocks: [operator_approval]`; copying only the mode would
restore the older default unlock routes. Carry all other effective controls and
limits as well. This preparation need not enable budgets or trust progression.

Register the selected recipient commitment explicitly with
`-register-sending-protection-operator-recipients` before activation. Readiness
never registers or repairs anything. Inspect the stored generation/hash and
compare effective behavior on every serving and rollback-capable slot. Keep the
same admission rules in the config fallback so changing the source back does
not remove the gate. A healthy readiness result proves validity, not that a
policy matches the operator's intended rollout: generation/hash and admission
carryover checks remain required deployment gates.

## Database-source readiness

In database-source mode the existing background readiness probe reads policy
and the selected permanent recipient commitment in one consistent read-only
transaction. It validates the supported schema, canonical hash, policy fields,
and exact agreement between the selected logical version, local commitment
key, and registry commitment. Both trust roots must be loaded. A missing,
unreadable, corrupt, or mismatched state returns HTTP 503 with the fixed reason
`sending protection policy unavailable`; raw errors and row contents are never
returned. It does not fall back to config or another recipient version.

This check uses the monitor's dedicated database connection, not the shared
application pool, and performs no database work on the `/readyz` request path.
It shares the existing 2-second probe interval and 5-second probe timeout.
Policy failures bypass the connectivity failure grace period once observed;
recovery takes effect on the next successful probe. Ordinary connectivity
failures keep their existing 15-second grace, and draining still overrides
readiness immediately. `/api/health` remains shallow liveness.

Config-source deployments do not depend on stored policy or registry rows for
readiness. This preserves the existing self-host defaults and the explicit
config-source rollback path.
