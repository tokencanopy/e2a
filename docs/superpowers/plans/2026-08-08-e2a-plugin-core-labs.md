# e2a Core and Labs Plugin Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split experimental autonomous workflows into `e2a-labs` and ship three focused, safe developer skills in the stable e2a plugin.

**Architecture:** The stable `plugins/e2a` package remains the only owner of the hosted e2a MCP registration and contains four focused skills. A sibling `plugins/e2a-labs` package contains Agentify, Autopilot, and Tether, depends on core where the client supports dependencies, and never registers a duplicate MCP server. Repository tests enforce skill ownership, manifest/version consistency, routing contracts, confirmation boundaries, and the known Labs security fixes.

**Tech Stack:** Markdown skills, JSON plugin/marketplace manifests, Node.js 22 built-in test runner, Bash, Python 3 with PyYAML for Agentify validation, GitHub Actions, Claude Code CLI 2.1.226 for strict manifest validation.

## Global Constraints

- Core plugin version is `0.7.0`; Labs starts at `0.1.0`.
- Core contains exactly `e2a`, `e2a-setup`, `e2a-integrate`, and `e2a-doctor`.
- Labs contains exactly `agentify`, `autopilot`, and `tether`.
- `plugins/e2a/.mcp.json` is the only plugin-owned e2a MCP registration; Labs must not contain `.mcp.json` or an `mcpServers` manifest field.
- Claude and Codex marketplaces list core and Labs; Cursor lists core only until it supports this skill-delivery path.
- Interactive MCP setup uses OAuth and never requests an API key.
- TypeScript/JavaScript and Python use official SDKs; other server-side languages use REST/OpenAPI; no unofficial SDK is invented.
- Diagnosis is MCP-first and read-only; every state-changing repair requires confirmation.
- One complete DNS diff receives one confirmation before provider-assisted writes.
- No real customer or non-public production-derived data may enter source, fixtures, logs, screenshots, commits, or reviews. Use `.test`, `.invalid`, `example.com`, and fictional IDs.
- Do not add a backend API, MCP tool, domain-purchase flow, or required CLI dependency.
- Preserve unrelated working-tree changes and commit only files named by the active task.

---

### Task 1: Enforce Agentify's unattended permission boundary

**Files:**
- Create: `plugins/e2a/skills/agentify/test/permission-contract.test.mjs`
- Modify: `plugins/e2a/skills/agentify/test/run.sh`
- Modify: `plugins/e2a/skills/agentify/templates/workflows/feedback-triage.yml.tmpl`
- Modify: `plugins/e2a/skills/agentify/templates/workflows/feedback-comms.yml.tmpl`
- Modify: `plugins/e2a/skills/agentify/templates/workflows/feedback-fix.yml.tmpl`
- Modify: `plugins/e2a/skills/agentify/references/security-invariants.md`

**Interfaces:**
- Consumes: Claude Code permission semantics: `dontAsk` denies unapproved calls, `--tools` limits built-ins, `--strict-mcp-config` excludes unrelated MCP servers.
- Produces: A deterministic workflow contract that later moves unchanged with Agentify into Labs.

- [ ] **Step 1: Write the failing permission-contract test**

Create a dependency-free Node test that loads all three workflow templates and pins the security-bearing flags:

```js
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

const workflow = (name) => readFile(
  new URL(`../templates/workflows/${name}`, import.meta.url),
  "utf8",
);

test("triage and comms use deny-by-default tool permissions", async () => {
  for (const name of ["feedback-triage.yml.tmpl", "feedback-comms.yml.tmpl"]) {
    const source = await workflow(name);
    assert.doesNotMatch(source, /--permission-mode\s+bypassPermissions/);
    assert.match(source, /--permission-mode\s+dontAsk/);
    assert.match(source, /--tools\s+"Bash,Read"/);
    assert.match(source, /--strict-mcp-config/);
    assert.doesNotMatch(source, /--allowedTools[\s\\]+(?:"[^"]+"\s+)*"Bash"(?:\s|\\)/);
    assert.doesNotMatch(source, /"Bash\((?:curl|wget|env|printenv):\*\)"/);
  }
});

test("the fix lane exposes only its documented built-ins", async () => {
  const source = await workflow("feedback-fix.yml.tmpl");
  assert.doesNotMatch(source, /--permission-mode\s+bypassPermissions/);
  assert.match(source, /--permission-mode\s+dontAsk/);
  assert.match(source, /--tools\s+"Bash,Edit,Write,Read,Glob,Grep"/);
  assert.match(source, /--strict-mcp-config/);
});
```

- [ ] **Step 2: Run the new test and verify the current workflows fail**

Run: `node --test plugins/e2a/skills/agentify/test/permission-contract.test.mjs`

Expected: FAIL because the workflows still contain `bypassPermissions` and do not restrict built-in tools.

- [ ] **Step 3: Make triage and comms deny by default**

For both CLI invocations, use this flag order after `--mcp-config`:

```bash
--strict-mcp-config \
--permission-mode dontAsk \
--tools "Bash,Read" \
--allowedTools \
```

Keep only the existing narrow `Bash(script-prefix:*)`, `Bash(gh ...:*)`, `Read`, and named e2a read tools in `--allowedTools`. Keep explicit `--disallowedTools` as defense in depth. Rewrite the comments to state that `dontAsk` makes unmatched calls fail and `--tools` removes unneeded built-ins; do not describe `--allowedTools` as an exclusive allowlist.

- [ ] **Step 4: Restrict the fix lane's built-in tool set**

In the action's `claude_args`, replace `bypassPermissions` with:

```text
--strict-mcp-config
--permission-mode dontAsk
--tools "Bash,Edit,Write,Read,Glob,Grep"
--allowedTools Bash Edit Write Read Glob Grep
```

Document that the fix lane intentionally retains arbitrary repository-local Bash for build/test work, runs without deploy or production credentials, and ends at a human-reviewed PR. Do not claim Bash subcommands are structurally constrained there.

- [ ] **Step 5: Update the security invariant and run Agentify tests**

Add an invariant stating:

```markdown
Unattended lanes use `dontAsk`, an explicit built-in `--tools` set, and a
strict MCP configuration. Triage and comms auto-approve only named Bash
prefixes and e2a tools. The fix lane intentionally retains general repository
Bash, but receives no deploy or production credentials and cannot merge its PR.
```

Add `run node test/permission-contract.test.mjs` to `test/run.sh` after the addon bridge tests.

Run: `bash plugins/e2a/skills/agentify/test/run.sh`

Expected: `AGENTIFY TESTS: ALL PASS`.

- [ ] **Step 6: Commit the permission fix**

```bash
git add plugins/e2a/skills/agentify
git commit -m "fix(agentify): enforce unattended tool boundaries"
```

### Task 2: Anchor Agentify conversation dedup to the trusted footer

**Files:**
- Modify: `plugins/e2a/skills/agentify/templates/scripts/ticket_card.sh`
- Modify: `plugins/e2a/skills/agentify/templates/workflows/feedback-triage.yml.tmpl`
- Modify: `plugins/e2a/skills/agentify/templates/workflows/feedback-comms.yml.tmpl`
- Modify: `plugins/e2a/skills/agentify/templates/runtime-skill/triage.md`
- Modify: `plugins/e2a/skills/agentify/templates/runtime-skill/ticket-card.md`
- Modify: `plugins/e2a/skills/agentify/references/security-invariants.md`

**Interfaces:**
- Consumes: `AUTOREPO_BOT_LOGIN`, `AUTOREPO_MARKER`, and a conversation ID.
- Produces: `_matches_comms_footer(bot, marker, conversation_id)`, which succeeds only when the last nonblank line is the exact trusted footer.

- [ ] **Step 1: Add failing pure-logic self-tests**

Extend `ticket_card.sh _selftest` with JSON issue bodies that cover a genuine footer and a forged value inside quoted content:

```bash
good_body=$'summary\n```text\nuser data\n```\n<!-- acme-feedback comms:conv_target -->'
forged_body=$'summary\n```text\ncomms:conv_target\n```\n<!-- acme-feedback comms:conv_other -->'
good_issue="$(jq -n --arg body "$good_body" '{author:{login:"bot[bot]"},body:$body}')"
forged_issue="$(jq -n --arg body "$forged_body" '{author:{login:"bot[bot]"},body:$body}')"
printf '%s' "$good_issue" | _matches_comms_footer "bot[bot]" "acme-feedback" "conv_target" \
  || { echo "FAIL exact comms footer rejected"; fail=1; }
printf '%s' "$forged_issue" | _matches_comms_footer "bot[bot]" "acme-feedback" "conv_target" \
  && { echo "FAIL quoted comms value accepted"; fail=1; }
```

- [ ] **Step 2: Run the self-test and verify the helper is missing**

Run: `bash plugins/e2a/skills/agentify/templates/scripts/ticket_card.sh _selftest`

Expected: FAIL because `_matches_comms_footer` is not defined.

- [ ] **Step 3: Implement exact footer matching**

Add this pure helper before the GitHub-backed operations:

```bash
_matches_comms_footer() { # stdin={author,body}; $1=bot $2=marker $3=conversation
  jq -e --arg bot "$1" --arg marker "$2" --arg conv "$3" '
    (.author.login == $bot) and
    ((.body | split("\n") | map(select(test("\\S"))) | last)
      == ("<!-- " + $marker + " comms:" + $conv + " -->"))'
}
```

Require `AUTOREPO_MARKER` alongside the bot identity in `find-by-comms`, then replace the unanchored `contains` expression with:

```bash
gh issue view "$n" -R "$repo" --json author,body \
  | _matches_comms_footer "$AUTOREPO_BOT_LOGIN" "$AUTOREPO_MARKER" "$conv"
```

- [ ] **Step 4: Export and document the marker contract**

In the triage and comms config-parsing steps, add:

```bash
echo "AUTOREPO_MARKER=$(yq -r '.marker' "$CFG")"
```

Update the runtime and security references to say the exact last nonblank footer is trusted only when both bot author and configured marker match.

- [ ] **Step 5: Run the full Agentify suite**

Run: `bash plugins/e2a/skills/agentify/test/run.sh`

Expected: all script self-tests, YAML validation, and lane assertions pass.

- [ ] **Step 6: Commit the dedup fix**

```bash
git add plugins/e2a/skills/agentify
git commit -m "fix(agentify): anchor conversation dedup footer"
```

### Task 3: Make Agentify configuration rendering YAML-safe

**Files:**
- Create: `plugins/e2a/skills/agentify/test/render-config.test.py`
- Modify: `plugins/e2a/skills/agentify/agentify-render.sh`
- Modify: `plugins/e2a/skills/agentify/test/run.sh`

**Interfaces:**
- Consumes: arbitrary Unicode `ANS_*` scalar values gathered by the Agentify interview.
- Produces: `_yaml_dq_content(value) -> escaped text safe inside an existing YAML double-quoted scalar`.

- [ ] **Step 1: Write the failing renderer regression test**

Create a Python test using the already-required PyYAML dependency:

```python
#!/usr/bin/env python3
import os
import pathlib
import subprocess
import tempfile
import yaml

root = pathlib.Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory() as tmp:
    env = os.environ | {
        "ANS_PRODUCT_NAME": 'Acme "Widget"\nOperations',
        "ANS_OWNER": "acme",
        "ANS_REPO": "widget",
        "ANS_MARKER": "acme-feedback",
        "ANS_REVIEWER_LOGIN": "dev",
        "ANS_BOT_LOGIN": "acme-bot[bot]",
        "ANS_SUPPORT_ADDRESS": "support+agent@acme.test",
        "ANS_FIX_GATE_MODE": "hitl",
        "ANS_APPROVER_ADDRESS": "owner@acme.test",
        "ANS_VERIFY_SETUP_SCRIPT": r"scripts\verify.sh",
    }
    subprocess.run(["bash", str(root / "agentify-render.sh"), "--to", tmp], env=env, check=True)
    config = yaml.safe_load(pathlib.Path(tmp, "autonomous-repo.config.yml").read_text())
    assert config["product_name"] == 'Acme "Widget"\nOperations'
    assert config["verify_setup_script"] == r"scripts\verify.sh"
    assert config["repo"] == "acme/widget"

    bad = env | {"ANS_MARKER": "acme\nINJECTED=value"}
    rejected = subprocess.run(
        ["bash", str(root / "agentify-render.sh"), "--to", tmp, "--force"],
        env=bad,
    )
    assert rejected.returncode == 2
```

- [ ] **Step 2: Run it and verify YAML parsing fails**

Run: `python3 plugins/e2a/skills/agentify/test/render-config.test.py`

Expected: FAIL with a YAML parser error caused by the embedded quote.

- [ ] **Step 3: Serialize substitutions for YAML double-quoted scalars**

Replace `_esc`-only substitution with these helpers:

```bash
_yaml_dq_content() {
  python3 -c 'import json,sys
s=json.dumps(sys.argv[1], ensure_ascii=False)
print(s[1:-1])' "$1"
}

_sed_replacement() {
  _yaml_dq_content "$1" | sed -e 's/[\\&|]/\\&/g'
}
```

Use `_sed_replacement` for every `ANS_*` replacement. Keep the template's existing surrounding double quotes so the escaped content is always one YAML scalar. Preserve the existing unfilled-placeholder rejection.

Before rendering, reject CR/LF in values later exported through `GITHUB_ENV`, and constrain structural fields:

```bash
_single_line() {
  case "$2" in
    *$'\n'*|*$'\r'*) echo "agentify: $1 must be one line" >&2; exit 2 ;;
  esac
}

for name in ANS_OWNER ANS_REPO ANS_MARKER ANS_REVIEWER_LOGIN ANS_BOT_LOGIN \
  ANS_SUPPORT_ADDRESS ANS_FIX_GATE_MODE ANS_APPROVER_ADDRESS ANS_VERIFY_SETUP_SCRIPT; do
  _single_line "$name" "${!name:-}"
done
[[ "${ANS_OWNER:-}" =~ ^[A-Za-z0-9_.-]+$ ]] || { echo "agentify: invalid owner" >&2; exit 2; }
[[ "${ANS_REPO:-}" =~ ^[A-Za-z0-9_.-]+$ ]] || { echo "agentify: invalid repo" >&2; exit 2; }
[[ "${ANS_MARKER:-}" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || { echo "agentify: invalid marker" >&2; exit 2; }
[[ "${ANS_FIX_GATE_MODE:-}" =~ ^(auto|hitl)$ ]] || { echo "agentify: fix gate must be auto or hitl" >&2; exit 2; }
```

- [ ] **Step 4: Add the regression to the Agentify runner**

After `run bash agentify-render.sh _selftest`, add:

```bash
run python3 test/render-config.test.py
```

- [ ] **Step 5: Run renderer and full Agentify tests**

Run: `python3 plugins/e2a/skills/agentify/test/render-config.test.py`

Run: `bash plugins/e2a/skills/agentify/test/run.sh`

Expected: both pass; the rendered YAML retains quotes, newlines, backslashes, and Unicode as scalar data.

- [ ] **Step 6: Commit the renderer fix**

```bash
git add plugins/e2a/skills/agentify
git commit -m "fix(agentify): serialize rendered YAML values"
```

### Task 4: Make Tether expiry parsing timezone-safe and fail-closed

**Files:**
- Modify: `plugins/e2a/skills/tether/lib.sh`
- Modify: `plugins/e2a/skills/tether/tether.sh`

**Interfaces:**
- Produces: `t_parse_until(value) -> normalized UTC RFC3339 string | INVALID`.
- Preserves: `t_remaining_seconds` returns `2147483647` only when no expiry exists; malformed stored expiry returns `-1` and is treated as expired.

- [ ] **Step 1: Add failing expiry self-tests**

Add these checks in the duration-parser section of `tether.sh _selftest`:

```bash
ck "naive --until rejected" "$(t_parse_until '2026-08-08T18:00:00')" "INVALID"
ck "Z --until normalized" "$(t_parse_until '2026-08-08T18:00:00Z')" "2026-08-08T18:00:00Z"
ck "offset --until normalized" "$(t_parse_until '2026-08-08T11:00:00-07:00')" "2026-08-08T18:00:00Z"
badf=/tmp/tether-selftest-bad-expiry.json
printf '{"expires_at":"not-a-date"}' > "$badf"
badrem="$(TETHER_STATE="$badf" t_remaining_seconds)"
ck "malformed stored expiry is expired" "$badrem" "-1"
rm -f "$badf"
```

- [ ] **Step 2: Run the self-test and verify failure**

Run: `bash plugins/e2a/skills/tether/tether.sh _selftest`

Expected: FAIL because `t_parse_until` is missing and malformed state still returns the unbounded sentinel.

- [ ] **Step 3: Implement explicit-offset parsing**

Add to `lib.sh` beside the duration helpers:

```bash
t_parse_until() {
  python3 -c 'import datetime,sys
try:
    raw=sys.argv[1]
    value=datetime.datetime.fromisoformat(raw.replace("Z", "+00:00"))
    if value.tzinfo is None or value.utcoffset() is None:
        raise ValueError("timezone required")
    utc=value.astimezone(datetime.timezone.utc).replace(microsecond=0)
    print(utc.isoformat().replace("+00:00", "Z"))
except Exception:
    print("INVALID")' "$1"
}
```

Change `start --until` to call `t_parse_until "$untilarg"`. In `t_remaining_seconds`, keep the empty-state sentinel but print `-1` on parse, timezone, or subtraction errors.

- [ ] **Step 4: Run Tether and shell syntax checks**

Run: `bash plugins/e2a/skills/tether/tether.sh _selftest`

Run: `bash -n plugins/e2a/skills/tether/lib.sh plugins/e2a/skills/tether/tether.sh`

Expected: all expiry cases and existing self-tests pass.

- [ ] **Step 5: Commit the expiry fix**

```bash
git add plugins/e2a/skills/tether/lib.sh plugins/e2a/skills/tether/tether.sh
git commit -m "fix(tether): require timezone-aware expiry"
```

### Task 5: Extract the experimental e2a-labs package

**Files:**
- Create: `plugins/e2a-labs/.claude-plugin/plugin.json`
- Create: `plugins/e2a-labs/.codex-plugin/plugin.json`
- Create: `plugins/e2a-labs/README.md`
- Create: `plugins/e2a-labs/assets/icon.svg`
- Move: `plugins/e2a/skills/agentify/` -> `plugins/e2a-labs/skills/agentify/`
- Move: `plugins/e2a/skills/autopilot/` -> `plugins/e2a-labs/skills/autopilot/`
- Move: `plugins/e2a/skills/tether/` -> `plugins/e2a-labs/skills/tether/`
- Create: `scripts/plugin-packaging.test.mjs`
- Modify: `scripts/validate-plugin.mjs`
- Modify: `scripts/plugin-agent-guidance.test.mjs`
- Modify: `.github/workflows/agentify-test.yml`
- Modify: `.github/workflows/agentify-lane-fixtures.yml`
- Modify: `AGENTS.md`
- Modify: `docs/design/autonomous-repo-framework.md`

**Interfaces:**
- Produces: two independently versioned plugin definitions, with core as the sole MCP owner.
- Produces: validator descriptors `PLUGIN_DEFS` keyed by `e2a` and `e2a-labs`.

- [ ] **Step 1: Write the failing package-ownership test**

Create `scripts/plugin-packaging.test.mjs`:

```js
import assert from "node:assert/strict";
import { access, readdir, readFile } from "node:fs/promises";
import { test } from "node:test";

const skillNames = async (plugin) => (await readdir(`plugins/${plugin}/skills`, { withFileTypes: true }))
  .filter((entry) => entry.isDirectory())
  .map((entry) => entry.name)
  .sort();

test("experimental workflows live only in e2a-labs", async () => {
  assert.deepEqual(await skillNames("e2a-labs"), ["agentify", "autopilot", "tether"]);
  const core = await skillNames("e2a");
  assert.ok(core.includes("e2a"));
  for (const experimental of ["agentify", "autopilot", "tether"]) {
    assert.ok(!core.includes(experimental), `${experimental} leaked into core`);
  }
});

test("only core registers the e2a MCP server", async () => {
  await access("plugins/e2a/.mcp.json");
  await assert.rejects(access("plugins/e2a-labs/.mcp.json"));
  for (const client of [".claude-plugin", ".codex-plugin"]) {
    const labs = JSON.parse(await readFile(`plugins/e2a-labs/${client}/plugin.json`, "utf8"));
    assert.equal(labs.mcpServers, undefined);
  }
});
```

- [ ] **Step 2: Run it and verify Labs is missing**

Run: `node --test scripts/plugin-packaging.test.mjs`

Expected: FAIL because `plugins/e2a-labs` does not exist.

- [ ] **Step 3: Move the three complete skill directories**

Use history-preserving moves:

```bash
mkdir -p plugins/e2a-labs/skills plugins/e2a-labs/assets
git mv plugins/e2a/skills/agentify plugins/e2a-labs/skills/agentify
git mv plugins/e2a/skills/autopilot plugins/e2a-labs/skills/autopilot
git mv plugins/e2a/skills/tether plugins/e2a-labs/skills/tether
cp plugins/e2a/assets/icon.svg plugins/e2a-labs/assets/icon.svg
```

- [ ] **Step 4: Add Labs manifests and prerequisite messaging**

The Claude manifest must contain `name: e2a-labs`, `version: 0.1.0`, no `icon`, no `mcpServers`, and:

```json
"dependencies": [{ "name": "e2a", "version": "^0.7.0" }]
```

The Codex manifest must point `skills` to `./skills/`, omit `mcpServers`, and describe core e2a as a prerequisite. The README must include Claude and Codex installation commands, state that Labs is experimental, and state that core supplies the MCP connection. Do not create a Cursor manifest.

- [ ] **Step 5: Generalize the validator to both plugin definitions**

Replace the single `PLUGIN_DIR`/canonical version model with descriptors shaped as:

```js
const PLUGIN_DEFS = [
  {
    name: "e2a",
    dir: join(ROOT, "plugins", "e2a"),
    clients: [".claude-plugin", ".codex-plugin", ".cursor-plugin"],
    ownsMcp: true,
  },
  {
    name: "e2a-labs",
    dir: join(ROOT, "plugins", "e2a-labs"),
    clients: [".claude-plugin", ".codex-plugin"],
    ownsMcp: false,
  },
];
```

Validate each definition's version independently, validate every skill directory, require MCP ownership only for core, and fail if a non-owner contains `.mcp.json` or any manifest has `mcpServers`.

- [ ] **Step 6: Update every path-sensitive test and workflow**

Change Agentify workflow path filters and commands to `plugins/e2a-labs/skills/agentify/**`. Change the Tether invariant in `plugin-agent-guidance.test.mjs` to `plugins/e2a-labs/skills/tether/tether.sh`. Update `AGENTS.md` and the maintained autonomous-repo design to point at the Labs path. Leave historical implementation plans unchanged. Update comments that claim all skills live under core.

- [ ] **Step 7: Run package, validator, and moved skill suites**

Run: `node --test scripts/plugin-packaging.test.mjs scripts/plugin-agent-guidance.test.mjs`

Run: `node scripts/validate-plugin.mjs`

Run: `bash plugins/e2a-labs/skills/agentify/test/run.sh`

Run: `node --test plugins/e2a-labs/skills/autopilot/test/*.test.mjs`

Run: `bash plugins/e2a-labs/skills/tether/tether.sh _selftest`

Expected: all pass and the validator reports both plugin versions separately.

- [ ] **Step 8: Commit the package extraction**

```bash
git add plugins/e2a plugins/e2a-labs scripts .github/workflows/agentify-test.yml .github/workflows/agentify-lane-fixtures.yml AGENTS.md docs/design/autonomous-repo-framework.md
git commit -m "refactor(plugin): move autonomous workflows to labs"
```

### Task 6: Add the e2a-setup skill

**Files:**
- Create: `plugins/e2a/skills/e2a-setup/SKILL.md`
- Create: `plugins/e2a/skills/e2a-setup/references/clients.md`
- Create: `plugins/e2a/skills/e2a-setup/references/custom-domains.md`
- Modify: `plugins/e2a/docs/setup.md`
- Modify: `web/public/setup.md` (generated mirror)
- Modify: `web/public/llms-full.txt` (generated corpus)
- Create: `scripts/plugin-core-skills.test.mjs`

**Interfaces:**
- Consumes: the current client's tool registry and e2a `whoami`, `list_agents`, `create_agent`, `list_messages`, `register_domain`, `get_domain`, and `verify_domain` tools.
- Produces: a readiness summary containing MCP state, credential scope, selected inbox, read verification, and custom-domain capability state.

- [ ] **Step 1: Write the failing setup contract test**

Create `scripts/plugin-core-skills.test.mjs` with a `section` helper and these assertions:

```js
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";

const read = (path) => readFile(path, "utf8");
const section = (source, heading) =>
  source.split(`## ${heading}\n`)[1]?.split(/\n## /)[0] ?? "";

test("e2a-setup bootstraps MCP, inboxes, and optional custom domains", async () => {
  const source = await read("plugins/e2a/skills/e2a-setup/SKILL.md");
  assert.match(source, /^name: e2a-setup$/m);
  assert.match(source, /register.*https:\/\/api\.e2a\.dev\/mcp/i);
  assert.match(source, /plugin.*(?:disabled|reload).*duplicate/is);
  assert.match(source, /OAuth/i);
  assert.match(source, /never ask.*API key/i);
  assert.match(source, /whoami/);
  assert.match(source, /confirm.*full.*address/i);
  assert.match(source, /agents\.e2a\.dev/);
  assert.match(source, /custom domain/i);
  assert.match(source, /one confirmation.*complete.*DNS diff/is);
  assert.match(source, /Cloudflare API MCP/i);
  assert.match(source, /GoDaddy.*gddy/is);
  assert.match(source, /GoDaddy MCP.*read-only/is);
  const report = section(source, "Completion report");
  for (const term of ["MCP", "scope", "inbox", "domain", "read"]) {
    assert.match(report, new RegExp(term, "i"));
  }
});
```

- [ ] **Step 2: Run it and verify the skill is absent**

Run: `node --test scripts/plugin-core-skills.test.mjs`

Expected: FAIL with `ENOENT` for `plugins/e2a/skills/e2a-setup/SKILL.md`.

- [ ] **Step 3: Write the focused setup skill**

Use this exact frontmatter and top-level section contract:

```markdown
---
name: e2a-setup
description: "Use when a user wants to connect or authorize the e2a MCP server, select or create an agent inbox, verify first-run readiness, or set up a custom email domain. Guides native client OAuth, shared-domain onboarding, and confirmed DNS-provider assistance without changing application code."
---

# Set up e2a

## Boundaries
## MCP bootstrap
## Select or create the inbox
## Choose shared or custom domain
## Verify readiness
## Completion report
```

The body must encode the approved setup sequence: check disabled/reload state before manual registration, confirm before client-config writes, use OAuth, call MCP `whoami`, select by scope, confirm the complete address before `create_agent`, perform a harmless `list_messages`, explicitly ask shared versus custom domain, and never send a test without a user-selected recipient.

- [ ] **Step 4: Add focused client and domain references**

`clients.md` must give the canonical Claude, Codex, and manual remote-MCP flows and explain the self-bootstrap boundary. `custom-domains.md` must define the shared-domain fast path, the exact register -> DNS -> verify sequence, one full-diff confirmation, Cloudflare API MCP guidance, GoDaddy `gddy` guidance, manual fallback, propagation handling, and separate inbound/outbound capability reporting. Link the official Cloudflare and GoDaddy documentation used in the approved design.

- [ ] **Step 5: Connect the canonical setup guide and regenerate hosted docs**

Add a short callout near the top of `plugins/e2a/docs/setup.md`: Claude and Codex plugin users can invoke `e2a-setup`; manual MCP clients should continue through the document. Do not claim Cursor receives the skill.

Run: `node scripts/sync-agent-docs.mjs`

Expected: `web/public/setup.md` becomes byte-identical to the canonical guide and `web/public/llms-full.txt` includes the updated setup section.

- [ ] **Step 6: Run setup and manifest contracts**

Run: `node --test scripts/plugin-core-skills.test.mjs scripts/plugin-packaging.test.mjs`

Run: `node scripts/validate-plugin.mjs`

Expected: setup contract passes; packaging still allows the growing stable skill set until Task 9 pins exactly four.

- [ ] **Step 7: Commit setup**

```bash
git add plugins/e2a/skills/e2a-setup plugins/e2a/docs/setup.md web/public/setup.md web/public/llms-full.txt scripts/plugin-core-skills.test.mjs
git commit -m "feat(plugin): add guided e2a setup skill"
```

### Task 7: Add the language-aware e2a-integrate skill

**Files:**
- Create: `plugins/e2a/skills/e2a-integrate/SKILL.md`
- Create: `plugins/e2a/skills/e2a-integrate/references/integration-modes.md`
- Create: `plugins/e2a/skills/e2a-integrate/references/sdk-recipes.md`
- Create: `plugins/e2a/skills/e2a-integrate/references/rest-openapi.md`
- Create: `plugins/e2a/skills/e2a-integrate/references/webhooks-and-tests.md`
- Modify: `plugins/e2a/docs/sdk.md`
- Modify: `web/public/sdk.md` (generated mirror)
- Modify: `web/public/llms-full.txt` (generated corpus)
- Modify: `scripts/plugin-core-skills.test.mjs`

**Interfaces:**
- Consumes: repository language/framework/package-manager/test-runner evidence and the requested send, webhook, or polling behavior.
- Produces: one application-owned e2a adapter boundary, server-only configuration contract, verified webhook handler when requested, and synthetic tests.

- [ ] **Step 1: Append the failing integration contract test**

```js
test("e2a-integrate is language-aware and security-complete", async () => {
  const source = await read("plugins/e2a/skills/e2a-integrate/SKILL.md");
  assert.match(source, /^name: e2a-integrate$/m);
  assert.match(source, /outbound.*webhook.*polling/is);
  assert.match(source, /TypeScript.*Python.*official.*SDK/is);
  assert.match(source, /REST.*OpenAPI/is);
  assert.match(source, /never invent.*SDK/i);
  assert.match(source, /application-owned.*boundary/i);
  assert.match(source, /server-only.*credential/i);
  assert.match(source, /signature verification.*before/i);
  assert.match(source, /idempotent/i);
  assert.match(source, /synthetic/i);
  assert.match(source, /live smoke test.*separate/is);
});
```

- [ ] **Step 2: Run it and verify the skill is absent**

Run: `node --test scripts/plugin-core-skills.test.mjs`

Expected: FAIL with `ENOENT` for the integration skill.

- [ ] **Step 3: Write the integration decision flow**

Use this frontmatter and section contract:

```markdown
---
name: e2a-integrate
description: "Use when adding e2a email capabilities to an application or codebase: outbound sending, inbound signed webhooks, REST polling, or SDK integration. Inspects the existing language and framework, uses official TypeScript/Python SDKs or idiomatic REST/OpenAPI, adds tests, and keeps credentials server-side."
---

# Integrate e2a into an application

## Determine the integration mode
## Inspect the existing codebase
## Select the supported client surface
## Build one application boundary
## Secure inbound webhooks
## Test and verify
## Completion report
```

Require the implementer to ask only when send/receive intent is materially ambiguous, preserve the repository's framework and module conventions, and never create production credentials or place live addresses in fixtures.

- [ ] **Step 4: Write the four implementation references**

Pin these decisions:

- `integration-modes.md`: send-only, signed-webhook, polling, and combined decision table.
- `sdk-recipes.md`: `@e2a/sdk` for TypeScript/JavaScript and `e2a` for Python; use their high-level clients and repository-native dependency commands.
- `rest-openapi.md`: use `https://api.e2a.dev/v1/openapi.yaml`, bearer auth on the server, explicit timeout/error handling, idempotency keys for retried writes, and no generated-client hand edits.
- `webhooks-and-tests.md`: verify signatures before parsing/dispatch, retain raw request bytes, deduplicate events using durable application state when available, use `.test` fixtures, and separate unit verification from an explicitly approved live smoke test.

- [ ] **Step 5: Connect the canonical SDK guide and regenerate hosted docs**

Add a short callout near the top of `plugins/e2a/docs/sdk.md`: Claude and Codex plugin users can invoke `e2a-integrate` to apply the guide to the current repository. Keep the SDK guide usable without the plugin.

Run: `node scripts/sync-agent-docs.mjs`

Expected: `web/public/sdk.md` is byte-identical to the canonical SDK guide and `web/public/llms-full.txt` contains the callout.

- [ ] **Step 6: Run core skill contracts**

Run: `node --test scripts/plugin-core-skills.test.mjs`

Run: `node scripts/validate-plugin.mjs`

Expected: setup and integration contracts pass.

- [ ] **Step 7: Commit integration**

```bash
git add plugins/e2a/skills/e2a-integrate plugins/e2a/docs/sdk.md web/public/sdk.md web/public/llms-full.txt scripts/plugin-core-skills.test.mjs
git commit -m "feat(plugin): add application integration skill"
```

### Task 8: Add the MCP-first e2a-doctor skill

**Files:**
- Create: `plugins/e2a/skills/e2a-doctor/SKILL.md`
- Create: `plugins/e2a/skills/e2a-doctor/references/diagnostic-matrix.md`
- Create: `plugins/e2a/skills/e2a-doctor/references/guided-repairs.md`
- Modify: `scripts/plugin-core-skills.test.mjs`

**Interfaces:**
- Consumes: symptom plus relevant e2a MCP reads; optionally consumes `e2a doctor --json` only when an already-authenticated CLI is the relevant surface.
- Produces: ranked findings `{cause, evidence, impact, remediation}` and individually confirmed guided repairs.

- [ ] **Step 1: Append the failing Doctor contract test**

```js
test("e2a-doctor is MCP-first, read-first, and repair-capable", async () => {
  const source = await read("plugins/e2a/skills/e2a-doctor/SKILL.md");
  assert.match(source, /^name: e2a-doctor$/m);
  assert.match(source, /MCP-first/i);
  assert.match(source, /read-only.*diagnos/is);
  for (const tool of [
    "whoami", "get_protection", "list_agent_suppressions", "get_domain",
    "list_webhook_deliveries", "get_message_lifecycle",
  ]) assert.match(source, new RegExp(tool));
  assert.match(source, /ranked.*evidence/i);
  assert.match(source, /confirmation.*each.*state-changing repair/is);
  assert.match(source, /e2a doctor --json/);
  assert.match(source, /never.*install.*CLI.*solely/is);
  assert.match(source, /accepted.*scheduled.*pending_review.*do not retry/is);
});
```

- [ ] **Step 2: Run it and verify the skill is absent**

Run: `node --test scripts/plugin-core-skills.test.mjs`

Expected: FAIL with `ENOENT` for the Doctor skill.

- [ ] **Step 3: Write the MCP-first diagnostic flow**

Use this frontmatter and section contract:

```markdown
---
name: e2a-doctor
description: "Use when an existing e2a MCP connection, inbox, custom domain, protection policy, webhook, or message delivery is failing or unclear. Diagnoses read-only through MCP first, ranks evidence-backed causes, and offers individually confirmed repairs; uses the CLI doctor only when CLI or self-hosted diagnostics are specifically relevant."
---

# Diagnose and repair e2a

## Start from the symptom
## Read-only MCP diagnosis
## Rank findings
## Offer guided repairs
## CLI-assisted branch
## Verify and report
```

State that absent permissions produce explicit skipped checks, not false passes. Preserve accepted/scheduled/pending-review no-retry behavior.

- [ ] **Step 4: Add diagnostic and repair references**

`diagnostic-matrix.md` must map these symptoms to the minimum reads: MCP/auth -> `whoami`; inbox access -> `get_agent`/`list_agents`; holds -> `get_protection`/`list_pending_messages`; suppressions -> agent/account suppression lists; domain -> `get_domain` plus read-only public DNS; webhook -> `get_webhook`/`list_webhook_deliveries`; message -> `get_message_lifecycle`. Separate configuration failure, authorization failure, async pending state, and transient service failure.

`guided-repairs.md` must require confirmation before reauthorization, `create_agent`, `update_protection`, suppression changes, webhook mutation/test/redelivery, `verify_domain`, or DNS writes. One confirmed full DNS diff is one repair. Every accepted repair is followed by the narrowest relevant read-only verification.

- [ ] **Step 5: Run core skill contracts and validator**

Run: `node --test scripts/plugin-core-skills.test.mjs`

Run: `node scripts/validate-plugin.mjs`

Expected: all three new skill contracts pass.

- [ ] **Step 6: Commit Doctor**

```bash
git add plugins/e2a/skills/e2a-doctor scripts/plugin-core-skills.test.mjs
git commit -m "feat(plugin): add MCP-first doctor skill"
```

### Task 9: Narrow the e2a operating skill and lock routing boundaries

**Files:**
- Modify: `plugins/e2a/skills/e2a/SKILL.md`
- Modify: `scripts/plugin-agent-guidance.test.mjs`
- Modify: `scripts/plugin-core-skills.test.mjs`
- Modify: `scripts/plugin-packaging.test.mjs`

**Interfaces:**
- Consumes: the three new stable skill contracts.
- Produces: four non-overlapping stable trigger descriptions and an `e2a` skill focused on email operation.

- [ ] **Step 1: Write failing ownership and routing assertions**

Change the core ownership expectation to:

```js
assert.deepEqual(await skillNames("e2a"), [
  "e2a", "e2a-doctor", "e2a-integrate", "e2a-setup",
]);
```

Add this routing contract:

```js
test("stable skill descriptions separate setup, integration, operation, and diagnosis", async () => {
  const sources = Object.fromEntries(await Promise.all(
    ["e2a", "e2a-setup", "e2a-integrate", "e2a-doctor"].map(async (name) => [
      name,
      await read(`plugins/e2a/skills/${name}/SKILL.md`),
    ]),
  ));
  assert.match(sources["e2a-setup"], /connect|authorize|create.*inbox/i);
  assert.match(sources["e2a-integrate"], /application|codebase|SDK|webhook/i);
  assert.match(sources.e2a, /read|send|reply|forward/i);
  assert.match(sources["e2a-doctor"], /failing|diagnos|delivery/i);
  assert.doesNotMatch(sources.e2a.match(/^description:.*$/m)[0], /integrating.*software/i);
});
```

- [ ] **Step 2: Run routing tests and verify the broad e2a description fails**

Run: `node --test scripts/plugin-core-skills.test.mjs scripts/plugin-packaging.test.mjs`

Expected: the exact four-skill ownership passes, but routing fails because the current `e2a` description also claims software integration.

- [ ] **Step 3: Narrow e2a to operation**

Replace its description with:

```yaml
description: "Use when operating an already-connected e2a inbox over MCP: reading, composing, sending, replying, forwarding, handling attachments, managing contacts/outreach, scheduling mail, or using templates. Teaches correct threading, conversation correlation, concise multipart composition, and accepted/pending-review no-retry behavior."
```

Remove the `First run: connect and verify e2a`, `Add a custom domain`, `Receive mail in your own backend`, and `Integrating e2a into software` sections. Replace them near `How this fits` with direct handoffs to `e2a-setup`, `e2a-integrate`, and `e2a-doctor`. Keep operational protection, triage, composition, send/reply, scheduling, templates, contacts, gotchas, and no-retry guidance.

- [ ] **Step 4: Retarget existing guidance tests**

Move MCP-bootstrap assertions from the e2a skill test to the e2a-setup contract. Keep composition, outreach, tool-count, protection, and pending-review assertions against `e2a/SKILL.md`. Update test names so failures identify the owning skill.

- [ ] **Step 5: Run all stable skill and guidance tests**

Run: `node --test scripts/plugin-agent-guidance.test.mjs scripts/plugin-core-skills.test.mjs scripts/plugin-packaging.test.mjs`

Run: `node scripts/validate-plugin.mjs`

Expected: all pass; core owns exactly four skills and each description has one primary job.

- [ ] **Step 6: Commit the routing cleanup**

```bash
git add plugins/e2a/skills/e2a/SKILL.md scripts/plugin-agent-guidance.test.mjs scripts/plugin-core-skills.test.mjs scripts/plugin-packaging.test.mjs
git commit -m "refactor(plugin): focus stable skill routing"
```

### Task 10: Publish both packages and enforce release freshness

**Files:**
- Modify: `plugins/e2a/.claude-plugin/plugin.json`
- Modify: `plugins/e2a/.codex-plugin/plugin.json`
- Modify: `plugins/e2a/.cursor-plugin/plugin.json`
- Modify: `plugins/e2a/README.md`
- Modify: `plugins/e2a-labs/README.md`
- Modify: `.claude-plugin/marketplace.json`
- Modify: `.agents/plugins/marketplace.json`
- Modify: `.cursor-plugin/marketplace.json`
- Modify: `scripts/validate-plugin.mjs`
- Create: `scripts/check-plugin-version-bump.mjs`
- Create: `scripts/check-plugin-version-bump.test.mjs`
- Modify: `.github/workflows/test.yml`

**Interfaces:**
- Produces: published core `0.7.0`, Labs `0.1.0`, strict client exposure rules, dynamic MCP-count validation, and a PR/push version-bump gate.

- [ ] **Step 1: Write failing manifest/count assertions**

Extend `plugin-packaging.test.mjs` to assert:

```js
const claudeMarket = JSON.parse(await readFile(".claude-plugin/marketplace.json", "utf8"));
const codexMarket = JSON.parse(await readFile(".agents/plugins/marketplace.json", "utf8"));
const cursorMarket = JSON.parse(await readFile(".cursor-plugin/marketplace.json", "utf8"));
assert.deepEqual(claudeMarket.plugins.map((p) => p.name).sort(), ["e2a", "e2a-labs"]);
assert.deepEqual(codexMarket.plugins.map((p) => p.name).sort(), ["e2a", "e2a-labs"]);
assert.deepEqual(cursorMarket.plugins.map((p) => p.name), ["e2a"]);
assert.equal(JSON.parse(await readFile("plugins/e2a/.claude-plugin/plugin.json", "utf8")).version, "0.7.0");
assert.equal(JSON.parse(await readFile("plugins/e2a-labs/.claude-plugin/plugin.json", "utf8")).version, "0.1.0");
```

Run: `node --test scripts/plugin-packaging.test.mjs`

Expected: FAIL because marketplaces and core version still represent the old package.

- [ ] **Step 2: Write the failing version-bump unit tests**

Create `scripts/check-plugin-version-bump.test.mjs` before the implementation module:

```js
import assert from "node:assert/strict";
import { test } from "node:test";
import { changedPlugins, unchangedVersions } from "./check-plugin-version-bump.mjs";

test("maps changed files to independently versioned plugins", () => {
  assert.deepEqual(changedPlugins(["plugins/e2a/skills/e2a/SKILL.md"]), ["e2a"]);
  assert.deepEqual(changedPlugins(["plugins/e2a-labs/skills/tether/lib.sh"]), ["e2a-labs"]);
  assert.deepEqual(changedPlugins([
    "plugins/e2a/skills/e2a/SKILL.md",
    "plugins/e2a-labs/skills/tether/lib.sh",
  ]).sort(), ["e2a", "e2a-labs"]);
});

test("requires bumps only for plugins that existed in the base", () => {
  assert.deepEqual(unchangedVersions({
    changed: ["e2a", "e2a-labs"],
    baseVersions: { e2a: "0.6.0" },
    currentVersions: { e2a: "0.6.0", "e2a-labs": "0.1.0" },
  }), ["e2a"]);
});
```

Run: `node --test scripts/check-plugin-version-bump.test.mjs`

Expected: FAIL with `ERR_MODULE_NOT_FOUND` because the implementation module does not exist.

- [ ] **Step 3: Implement the pure version-bump seam and CLI**

Create `check-plugin-version-bump.mjs` exporting:

```js
export function changedPlugins(paths) {
  return [...new Set(paths.flatMap((path) =>
    path.startsWith("plugins/e2a-labs/") ? ["e2a-labs"]
      : path.startsWith("plugins/e2a/") ? ["e2a"] : [],
  ))];
}

export function unchangedVersions({ changed, baseVersions, currentVersions }) {
  return changed.filter((name) =>
    baseVersions[name] !== undefined && baseVersions[name] === currentVersions[name],
  );
}
```

Extend the tests to prove a Labs-only change requires only Labs and a move requires both. The CLI entry point accepts one base revision, uses `git diff --name-only <base> -- plugins/e2a plugins/e2a-labs`, reads base manifests with `git show`, and exits nonzero listing every unchanged plugin.

- [ ] **Step 4: Run the version tests**

Run: `node --test scripts/check-plugin-version-bump.test.mjs`

Expected: PASS for the pure change/version matrix.

- [ ] **Step 5: Update core and Labs manifests and marketplace exposure**

Set all core client manifests and applicable marketplace metadata to `0.7.0`; keep Labs at `0.1.0`. Advertise the canonical 78-tool count or avoid a hard-coded number in descriptive copy. Remove `icon` from the Claude core manifest while retaining Codex `composerIcon` and Cursor `logo`. Add Labs entries to Claude and Codex only. Confirm the Labs Claude dependency is `e2a ^0.7.0` and neither Labs manifest declares MCP.

- [ ] **Step 6: Rewrite installation and migration documentation**

Update the core README tree to show four stable skills and link Labs. The Labs README must include the moved-skill migration commands and clearly label all three workflows experimental. State that installing core supplies MCP and that Cursor receives the core MCP configuration/docs but not Labs skills.

- [ ] **Step 7: Validate advertised tool counts dynamically**

In `validate-plugin.mjs`, read `mcp/tool-names.v1.json`. For every manifest or marketplace description containing `/\b(\d+) MCP tools\b/`, compare the captured value to the canonical array length and fail with the exact file on mismatch. Descriptions without a numeric claim remain valid.

- [ ] **Step 8: Add strict validation and version checks to CI**

Change the plugin job checkout to `fetch-depth: 0`, install the pinned validator, and run both packages:

```yaml
env:
  CLAUDE_CODE_VERSION: "2.1.226"
steps:
  - uses: actions/checkout@v7
    with:
      fetch-depth: 0
  - uses: actions/setup-node@v7
    with:
      node-version: "22"
  - run: npm install --global "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}"
  - run: node --test scripts/plugin-agent-guidance.test.mjs scripts/plugin-core-skills.test.mjs scripts/plugin-packaging.test.mjs scripts/check-plugin-version-bump.test.mjs
  - run: node scripts/validate-plugin.mjs
  - run: claude plugin validate --strict plugins/e2a
  - run: claude plugin validate --strict plugins/e2a-labs
```

Add conditional PR and push steps invoking `check-plugin-version-bump.mjs` with the base SHA, following the existing MCP compatibility job's base-selection pattern.

- [ ] **Step 9: Run the complete local verification matrix**

Run:

```bash
node --test scripts/plugin-agent-guidance.test.mjs scripts/plugin-core-skills.test.mjs scripts/plugin-packaging.test.mjs scripts/check-plugin-version-bump.test.mjs
node scripts/validate-plugin.mjs
claude plugin validate --strict plugins/e2a
claude plugin validate --strict plugins/e2a-labs
node --test scripts/sync-agent-docs.test.mjs
node scripts/sync-agent-docs.mjs --check
bash plugins/e2a-labs/skills/agentify/test/run.sh
node --test plugins/e2a-labs/skills/autopilot/test/*.test.mjs
bash plugins/e2a-labs/skills/tether/tether.sh _selftest
git diff --check
```

Expected: every command passes. If the Autopilot suite cannot create sockets or loopback listeners in the implementation sandbox, rerun that same command with the required sandbox approval and record that the unrestricted run passes.

- [ ] **Step 10: Inspect the final diff for public-data and scope safety**

Run:

```bash
git status --short
git diff --stat
git diff -- plugins/e2a plugins/e2a-labs scripts .claude-plugin .agents/plugins .cursor-plugin .github/workflows/test.yml .github/workflows/agentify-test.yml .github/workflows/agentify-lane-fixtures.yml
```

Confirm no unrelated files are staged, no customer identifiers appear, Labs has no MCP registration, Cursor has no Labs entry, and every user-facing move has migration guidance.

- [ ] **Step 11: Commit distribution and gates**

```bash
git add plugins/e2a plugins/e2a-labs scripts/validate-plugin.mjs scripts/plugin-agent-guidance.test.mjs scripts/plugin-core-skills.test.mjs scripts/plugin-packaging.test.mjs scripts/check-plugin-version-bump.mjs scripts/check-plugin-version-bump.test.mjs .claude-plugin/marketplace.json .agents/plugins/marketplace.json .cursor-plugin/marketplace.json .github/workflows/test.yml .github/workflows/agentify-test.yml .github/workflows/agentify-lane-fixtures.yml
git commit -m "release(plugin): split stable core and experimental labs"
```
