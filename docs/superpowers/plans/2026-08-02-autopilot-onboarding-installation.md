# Conversational Autopilot Onboarding and Installation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn `/autopilot` into a one-question-at-a-time policy interview that defaults to a safe Customer Support agent, proves the selected boundary, and installs only after the exact user confirmation.

**Architecture:** The Markdown skill owns conversation, education, recommendations, and explicit consent. A deterministic shell/Node executor owns discovery, policy validation, read-only preflight, transactional server configuration, credential storage, service installation, status, and rollback. The versioned non-secret policy and plain-English summary are shown before any mutation.

**Tech Stack:** Agent plugin Markdown, POSIX shell wrapper, Node.js 18+ modules from the daemon plan, e2a CLI JSON commands, JSON Schema, launchd/systemd, Node built-in tests and plugin validation.

## Global Constraints

- The interview asks exactly one substantive question at a time and explains invalid answers before repeating that question.
- Customer Support is recommended and fully guided; Coding/Repository and Custom are advanced.
- The first questions are inbox identity, confirmed account owner email, and the job the agent should solve.
- Default inbound posture for Customer Support admits DMARC-aligned authenticated customers to restricted support capabilities and holds unauthenticated mail for review.
- Exact-address rules mean exact normalized From after aligned DMARC; they authenticate domain use for the message, not the individual human mailbox controller.
- Default outbound posture reviews every send/reply/forward, rejects holds on expiry, and treats `pending_review` as successful human handoff rather than delivery or retry.
- Owner CC is default-on; disabling it requires an explicit warning and confirmation.
- No account/server mutation, credential mint, or service installation occurs before exact confirmation: `Install and start autopilot`.
- Read-only discovery, local policy generation, and sandbox preflight may run before confirmation.
- There is no legacy installation detection, import, migration, backup, or compatibility flow.
- No unsandboxed mode; Windows is unsupported in this release.
- Use only synthetic public examples and fixtures.

## File Map

- Replace `plugins/e2a/skills/autopilot/SKILL.md`: conversational control flow and safety contract.
- Create `plugins/e2a/skills/autopilot/references/interview.md` and `references/policy-summary.md`.
- Create profile guides under `plugins/e2a/skills/autopilot/profiles/`.
- Replace `plugins/e2a/skills/autopilot/autopilot.sh`: stable executor commands.
- Create `plugins/e2a/skills/autopilot/install.mjs`: discovery, preflight, transactional install, rollback, status.
- Create onboarding/installer contract tests under `plugins/e2a/skills/autopilot/test/`.
- Remove obsolete prototype files `autopilot.env.example`, `lib.sh`, and `headless-settings.claude.example.json` after their useful content is represented by policy/profile docs.
- Modify plugin manifests/marketplaces and `plugins/e2a/README.md` for the new release.

---

### Task 1: Encode the Conversational Interview Contract

**Files:**
- Replace: `plugins/e2a/skills/autopilot/SKILL.md`
- Create: `plugins/e2a/skills/autopilot/references/interview.md`
- Create: `plugins/e2a/skills/autopilot/test/onboarding-contract.test.mjs`

**Interfaces:**
- Produces an ordered interview state machine whose output is policy schema version 1.
- Consumes executor commands `discover --json`, `preflight --policy <path> --json`, and `install --policy <path> --confirm "Install and start autopilot"`.

- [ ] **Step 1: Write failing skill-contract tests**

Parse `SKILL.md` and `interview.md` to require this order:

```text
select/create inbox -> confirm owner -> define job -> select profile ->
authorize inbound -> choose outbound review -> choose owner CC ->
define execution boundary -> show summary -> run preflight -> exact confirmation
```

Assert the docs require one question per turn, repeat invalid questions, state all safe defaults, name both warned opt-outs, explain DMARC/address limitations, define `pending_review`, and prohibit install before the exact confirmation string.

- [ ] **Step 2: Run the contract test**

Run: `node --test plugins/e2a/skills/autopilot/test/onboarding-contract.test.mjs`

Expected: FAIL because the prototype skill does not encode the approved sequence and confirmation barrier.

- [ ] **Step 3: Rewrite `SKILL.md` as a concise controller**

The main skill must:

1. run read-only discovery;
2. ask only the next unanswered question;
3. store answers in a draft policy;
4. defer profile detail to the selected profile file;
5. render machine and human summaries;
6. run read-only preflight;
7. ask for `Install and start autopilot`; and
8. invoke install with `--confirm "Install and start autopilot"` only when the reply exactly confirms that phrase.

Any other response leaves generated files local and the service uninstalled.

- [ ] **Step 4: Write the complete question and validation reference**

For every question, specify accepted value shape, recommended answer, warning copy, policy field, and retry copy. Include account-owner confirmation and owner email as distinct from the agent inbox.

- [ ] **Step 5: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/onboarding-contract.test.mjs`

Expected: PASS.

```bash
git add plugins/e2a/skills/autopilot/SKILL.md plugins/e2a/skills/autopilot/references/interview.md plugins/e2a/skills/autopilot/test/onboarding-contract.test.mjs
git commit -m "feat(plugin): make autopilot onboarding conversational"
```

### Task 2: Define Customer Support, Coding, and Custom Capability Profiles

**Files:**
- Create: `plugins/e2a/skills/autopilot/profiles/customer-support.md`
- Create: `plugins/e2a/skills/autopilot/profiles/coding-repository.md`
- Create: `plugins/e2a/skills/autopilot/profiles/custom.md`
- Modify: `plugins/e2a/skills/autopilot/test/onboarding-contract.test.mjs`

**Interfaces:**
- Produces policy templates that may narrow, but never silently widen, capabilities.
- Customer Support local capabilities are exactly `e2a.current_message`, `e2a.current_thread`, `e2a.reply`, and `e2a.escalate`.

- [ ] **Step 1: Add failing capability-ceiling tests**

Assert Customer Support excludes source code, general host filesystem, browser/cloud credentials, account/review administration, payment/refund, and customer-account mutation. Require refund, legal, security, account change, private data, and uncertain answer to escalate. Assert operator identity never changes mounts/tools/network dynamically.

Assert Coding uses an isolated worktree/disposable clone and cannot write the primary checkout by default. Assert Custom begins with no data/tool/network/write grants and records each selection explicitly.

- [ ] **Step 2: Run the onboarding contract test**

Run: `node --test plugins/e2a/skills/autopilot/test/onboarding-contract.test.mjs`

Expected: FAIL because profile files do not exist.

- [ ] **Step 3: Write the Customer Support guided profile**

Collect knowledge-base mounts, product context, tone, supported categories, escalation categories, external actions, service hours, and response expectations. Default to authenticated customer intake, review-held replies, owner CC, and fixed owner escalation.

- [ ] **Step 4: Write advanced profiles**

Coding produces a patch/commit/draft-PR artifact for review from an isolated job checkout. Custom presents explicit empty permission categories and rejects natural-language instructions as capability grants.

- [ ] **Step 5: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/onboarding-contract.test.mjs`

Expected: PASS.

```bash
git add plugins/e2a/skills/autopilot/profiles plugins/e2a/skills/autopilot/test/onboarding-contract.test.mjs
git commit -m "docs(plugin): define autopilot task profiles"
```

### Task 3: Implement Read-Only Discovery and Policy Preflight

**Files:**
- Replace: `plugins/e2a/skills/autopilot/autopilot.sh`
- Create: `plugins/e2a/skills/autopilot/install.mjs`
- Create: `plugins/e2a/skills/autopilot/test/install.test.mjs`

**Interfaces:**
- Produces commands:
  - `autopilot.sh discover --json`
  - `autopilot.sh preflight --policy <absolute-path> --json`
  - `autopilot.sh install --policy <absolute-path> --confirm "Install and start autopilot"`
  - `autopilot.sh status --agent <email> --json`
  - `autopilot.sh logs --agent <email>`
  - `autopilot.sh stop --agent <email>`
  - `autopilot.sh uninstall --agent <email>`
- Discovery output contains authenticated scope, account owner, candidate inboxes, platform/service manager, and detected runtimes, but no credential.

- [ ] **Step 1: Write failing command and no-mutation tests**

Inject fake e2a/service/runtime dependencies. Assert `discover` calls `whoami --json` and, only under account scope, `agents list --json`. Assert `preflight` validates policy, runs adapter/escape probes, and atomically writes an owner-only receipt containing the exact policy digest and check results, but never calls protection replacement, key creation, or service installation. Assert each command rejects unknown flags and relative policy paths.

- [ ] **Step 2: Run installer tests**

Run: `node --test plugins/e2a/skills/autopilot/test/install.test.mjs`

Expected: FAIL because `install.mjs` and the command surface do not exist.

- [ ] **Step 3: Implement the shell dispatcher safely**

Use `set -eu`, resolve its own directory, and `exec node "$SCRIPT_DIR/install.mjs" "$@"`. Do not source env files, use `eval`, interpolate commands, or accept secrets as arguments.

- [ ] **Step 4: Implement discovery and preflight**

Parse JSON only, validate account scope for setup, confirm the policy agent belongs to the account, detect `launchd` on Darwin and user `systemd` on Linux, report Windows unsupported, detect runtime versions, and run the daemon plan’s full sandbox probes. Return structured JSON with `ok`, policy SHA-256 digest, receipt path, checks, warnings, and remediation; omit secrets and message data.

- [ ] **Step 5: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/install.test.mjs`

Expected: PASS for discovery/preflight cases.

```bash
git add plugins/e2a/skills/autopilot/autopilot.sh plugins/e2a/skills/autopilot/install.mjs plugins/e2a/skills/autopilot/test/install.test.mjs
git commit -m "feat(plugin): add autopilot discovery and preflight"
```

### Task 4: Render the Exact Policy and Plain-English Summary

**Files:**
- Create: `plugins/e2a/skills/autopilot/references/policy-summary.md`
- Create: `plugins/e2a/skills/autopilot/examples/customer-support.policy.json`
- Modify: `plugins/e2a/skills/autopilot/SKILL.md`
- Modify: `plugins/e2a/skills/autopilot/test/onboarding-contract.test.mjs`

**Interfaces:**
- Produces a schema-v1 policy compatible with `validatePolicy` from the daemon plan.
- Produces a summary containing job, sender lanes, outbound posture, owner visibility, mounts/write boundary, network, runtime/status, sandbox, service manager, and experimental warnings.

- [ ] **Step 1: Add failing golden-summary tests**

Use `owner@example.test` and `support@agents.example.test`. Assert review-all, reject-on-expiry, `suppress_notifications: false`, required owner CC, authenticated-customer lane, held unauthenticated lane, fixed escalation, no unsupported capabilities, and exact pre-install confirmation text. Add separate golden cases for owner-CC opt-out, recipient allowlist, no-review opt-out, and experimental runtime.

- [ ] **Step 2: Run contract tests**

Run: `node --test plugins/e2a/skills/autopilot/test/onboarding-contract.test.mjs`

Expected: FAIL because policy/summary guidance and example do not exist.

- [ ] **Step 3: Write the schema-valid example and deterministic summary rules**

The example must validate against `policy.schema.json`. Warning sections must remain present in both machine and human summaries; do not hide them after the user acknowledges them.

- [ ] **Step 4: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/{policy,onboarding-contract}.test.mjs`

Expected: PASS.

```bash
git add plugins/e2a/skills/autopilot/references/policy-summary.md plugins/e2a/skills/autopilot/examples/customer-support.policy.json plugins/e2a/skills/autopilot/SKILL.md plugins/e2a/skills/autopilot/test/onboarding-contract.test.mjs
git commit -m "docs(plugin): render autopilot policy before consent"
```

### Task 5: Install Transactionally and Roll Back Partial Failure

**Files:**
- Modify: `plugins/e2a/skills/autopilot/install.mjs`
- Modify: `plugins/e2a/skills/autopilot/test/install.test.mjs`

**Interfaces:**
- Consumes CLI commands from the server-protection plan: `protection replace`, `protection revision`, `keys create --agent`, and existing `keys delete <key-id>`.
- Consumes `installService` and daemon state-path helpers.
- Produces an owner-only secret file containing the dedicated agent key and no account key.
- Produces `buildProtectionRequest(policy): ProtectionConfigRequest` with a complete, deterministic server posture.

- [ ] **Step 1: Write failing mutation-order tests**

Assert install refuses unless `--confirm` equals `Install and start autopilot` and a passing preflight receipt created within the last 30 minutes matches the exact policy SHA-256 digest, platform, runtime executable path/version, and canonical mount paths. Pin this order:

```text
read current full protection -> replace complete protection -> read final revision ->
mint agent key -> atomically write secret/policy -> install service -> verify active
```

Assert the final revision is written into `server_protection.expected_revision` before service start.

Pin these exact protection mappings:

```text
Customer Support inbound: open + require_authenticated=true + action=review
Coding/Custom exact operator inbound: allowlist(addresses) + require_authenticated=true + action=review
Coding/Custom domain operator inbound: domain(domains) + require_authenticated=true + action=review
Review all outbound: allowlist([]) + action=review
Recipient allowlist outbound: allowlist(unique(addresses + required_cc)) + action=review
No outbound review: open + action=flag
All postures: holds.ttl_seconds=604800, holds.on_expiry=reject, holds.suppress_notifications=false, required_cc=[owner] unless explicitly opted out
```

Including `required_cc` in the recipient allowlist is mandatory because server composition inserts those addresses before the recipient gate; omitting them would turn every otherwise-authorized send into a review hold.

- [ ] **Step 2: Write failing rollback matrix tests**

For failure after each mutation, assert:

- protection-write failure leaves service absent and creates no secret;
- key-mint failure restores the prior protection;
- secret-write or service-install failure revokes the new key and restores protection;
- active verification failure stops/uninstalls the service, revokes the key, and restores protection;
- rollback failure returns manual remediation with identifiers but no secret.

- [ ] **Step 3: Run installer tests**

Run: `node --test plugins/e2a/skills/autopilot/test/install.test.mjs`

Expected: FAIL because transactional install is not implemented.

- [ ] **Step 4: Implement install with in-memory rollback state**

Keep the prior protection only in process memory; do not create a legacy-style backup file. Write secret/policy through mode-0600 temp+rename. Pass only the agent state directory to the service. After successful health verification, clear in-memory secret and rollback material.

- [ ] **Step 5: Implement explicit status/stop/uninstall behavior**

Status compares expected/current revision without showing full server policy. Stop preserves state. Uninstall stops/removes the service and revokes the dedicated key when possible, then removes only the resolved agent-specific runtime state after enumerating exact targets; it never touches unrelated agents.

- [ ] **Step 6: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/install.test.mjs`

Expected: PASS.

```bash
git add plugins/e2a/skills/autopilot/install.mjs plugins/e2a/skills/autopilot/test/install.test.mjs
git commit -m "feat(plugin): install autopilot transactionally"
```

### Task 6: Remove Prototype Artifacts and Gate the New-Install-Only Contract

**Files:**
- Delete: `plugins/e2a/skills/autopilot/autopilot.env.example`
- Delete: `plugins/e2a/skills/autopilot/lib.sh`
- Delete: `plugins/e2a/skills/autopilot/headless-settings.claude.example.json`
- Modify: `plugins/e2a/skills/autopilot/test/onboarding-contract.test.mjs`
- Modify: `scripts/validate-plugin.mjs`

**Interfaces:**
- Ensures policy JSON and generated isolated runtime settings replace prototype env/headless configuration.

- [ ] **Step 1: Add failing legacy-language and file-absence tests**

Scan the autopilot skill/runtime for legacy install/import/migrate/backup/reactivate behavior and assert the three obsolete files do not exist. Permit ordinary database migration references outside this plugin; scope the scan to `plugins/e2a/skills/autopilot`.

- [ ] **Step 2: Run plugin tests**

Run: `node --test plugins/e2a/skills/autopilot/test/onboarding-contract.test.mjs && node scripts/validate-plugin.mjs`

Expected: FAIL while prototype files and behavior remain.

- [ ] **Step 3: Remove obsolete files and tighten validation**

Delete the files only after all required settings are represented in schema/profile/generated adapter configuration. Make the validator require the new policy schema, profile docs, executor, daemon entry point, and test directory.

- [ ] **Step 4: Run tests and commit**

Run: `node --test plugins/e2a/skills/autopilot/test/*.test.mjs && node scripts/validate-plugin.mjs`

Expected: PASS.

```bash
git add -A plugins/e2a/skills/autopilot scripts/validate-plugin.mjs
git commit -m "refactor(plugin): replace prototype autopilot install"
```

### Task 7: Release the Updated Plugin Contract

**Files:**
- Modify: `plugins/e2a/.claude-plugin/plugin.json`
- Modify: `plugins/e2a/.codex-plugin/plugin.json`
- Modify: `plugins/e2a/.cursor-plugin/plugin.json`
- Modify: `.claude-plugin/marketplace.json`
- Modify: `.cursor-plugin/marketplace.json`
- Modify: `plugins/e2a/README.md`
- Modify: `.github/workflows/test.yml`

**Interfaces:**
- Bumps the e2a plugin from `0.6.0` to `0.7.0` consistently.
- Adds required onboarding/daemon test commands to plugin CI.

- [ ] **Step 1: Update plugin metadata and documentation**

Set every version-bearing manifest/marketplace entry to `0.7.0`. Update the skill tree and describe Customer Support as recommended, secure defaults, supported versus experimental runtimes, macOS/Linux service support, and exact confirmation barrier.

- [ ] **Step 2: Extend plugin CI**

Keep existing commands and add:

```yaml
- name: Test autopilot onboarding and runtime
  run: node --test plugins/e2a/skills/autopilot/test/*.test.mjs
```

- [ ] **Step 3: Run the complete plugin gate**

Run: `node --test plugins/e2a/skills/autopilot/test/*.test.mjs`

Run: `node --test scripts/plugin-agent-guidance.test.mjs`

Run: `node scripts/validate-plugin.mjs`

Expected: PASS and validator reports four plugin skills at version `0.7.0`.

- [ ] **Step 4: Run repository regression gates**

Run: `make test-unit && npm run build --workspace @e2a/sdk && npm test --workspace @e2a/cli`

Expected: PASS.

- [ ] **Step 5: Inspect and commit**

Run: `git diff --check && git status --short`

Expected: only approved autopilot, plugin metadata/docs, CI, and prerequisite surface files are changed; no secret or customer data appears.

```bash
git add plugins/e2a/.claude-plugin/plugin.json plugins/e2a/.codex-plugin/plugin.json plugins/e2a/.cursor-plugin/plugin.json .claude-plugin/marketplace.json .cursor-plugin/marketplace.json plugins/e2a/README.md .github/workflows/test.yml
git commit -m "chore(plugin): release policy-first autopilot 0.7.0"
```
