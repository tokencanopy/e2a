#!/usr/bin/env node
// Ergonomic-surface coverage gate — the TS-SDK analogue of
// tests/e2e-prod/mcp_coverage_gate.py.
//
// Every method the runtime-introspected ergonomic surface exposes (see
// introspect.ts — the denominator is the BUILT, INSTANTIATED client's own
// prototype graph, not a grep over client.ts) must be exercised by the live
// e2e suite (test/e2e*.test.ts) with an assertion on the result, or be listed
// in ALLOWLIST below with a one-line justification.
//
// Inputs: reports/*.json shards written by recorder.ts, each
//   {"advertised": [...method ids...], "covered": [...method ids...]}.
//
// Usage: node test/coverage/gate.mjs [--reports DIR]
// Exit 0 = every method covered or allowlisted; 1 = coverage gap;
// 2 = usage/IO error (including "no shards", which must never read as a pass).
import { readdirSync, readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));

// Methods the live suite intentionally does NOT exercise, with the reason.
// Keep this SHORT and justified — every entry is coverage knowingly forgone.
//
// NOTE on `client.meta` (PromiseMetaApi) and every resource's internal `.api`
// field: these do NOT appear here. They are excluded from the DENOMINATOR
// itself by introspect.ts (a node whose constructor name matches the
// generated layer's `Promise<Tag>Api` convention is never walked), not
// allowlisted — there is nothing to forgo, because they were never counted as
// ergonomic surface in the first place. See introspect.ts's module comment
// for why `private` can't be used as that signal and what is used instead.
const ALLOWLIST = {
  "account.delete": [
    "irreversible: cascades the entire account (agents, domains, messages,",
    "keys). There is no black-box way to mint a throwaway ACCOUNT (only",
    "throwaway agents/domains/keys within one) to test this against, and this",
    "gate runs against the one live staging account it shares with every other",
    "SDK/CLI/webhook coverage suite. Mirrors tests/e2e-prod's own deleteAccount",
    "placeholder (suites/19-account.test.ts).",
  ].join(" "),
  "account.suppressions.delete": [
    "an ACCOUNT-level suppression (distinct from the per-agent",
    "agents.*Suppression* surface, which IS covered) can only be created by a",
    "real SES bounce/complaint — there is no direct create endpoint. Per",
    "AGENTS.md's documented staging limitations, the e2a-staging-smtp SES IAM",
    "policy denies ses:SendRawEmail to the bounce/complaint simulator",
    "addresses, so no account-level suppression can ever exist on staging to",
    "delete. account.suppressions.list IS covered (asserts the envelope shape).",
  ].join(" "),
};

function loadShards(reportsDir) {
  const advertised = new Set();
  const covered = new Set();
  let shardCount = 0;
  let entries;
  try {
    entries = readdirSync(reportsDir).filter((f) => f.endsWith(".json")).sort();
  } catch {
    return { advertised, covered, shardCount: 0 };
  }
  for (const name of entries) {
    const raw = readFileSync(join(reportsDir, name), "utf8");
    const shard = JSON.parse(raw);
    if (shard && typeof shard === "object") {
      for (const id of shard.advertised ?? []) advertised.add(id);
      for (const id of shard.covered ?? []) covered.add(id);
      shardCount += 1;
    }
  }
  return { advertised, covered, shardCount };
}

function main() {
  const args = process.argv.slice(2);
  const flagIdx = args.indexOf("--reports");
  const reportsDir = flagIdx >= 0 && args[flagIdx + 1] ? args[flagIdx + 1] : join(HERE, "reports");

  const { advertised, covered, shardCount } = loadShards(reportsDir);

  if (shardCount === 0 || advertised.size === 0) {
    console.error(
      `coverage_gate: no shard directory (or no shards) at ${reportsDir}. ` +
        "Run the live suite first (`npm run test:live`, with E2A_TEST_* creds set); " +
        "an absent run is not a pass.",
    );
    return 2;
  }

  const allowlistKeys = Object.keys(ALLOWLIST);
  const stale = allowlistKeys.filter((k) => !advertised.has(k));
  if (stale.length > 0) {
    console.error(
      `coverage_gate: allowlist entries the introspected surface no longer exposes ` +
        `(renamed/removed?): ${JSON.stringify(stale)}`,
    );
    return 2;
  }

  const unadvertised = [...covered].filter((id) => !advertised.has(id)).sort();

  const missing = [...advertised].filter((id) => !covered.has(id));
  const allowlisted = missing.filter((id) => allowlistKeys.includes(id)).sort();
  const uncovered = missing.filter((id) => !allowlistKeys.includes(id)).sort();

  console.log(`Shards               : ${shardCount}`);
  console.log(`Advertised methods   : ${advertised.size}`);
  console.log(`Covered              : ${[...covered].filter((id) => advertised.has(id)).length}`);
  console.log(`Allowlisted          : ${allowlisted.length}${allowlisted.length ? " " + JSON.stringify(allowlisted) : ""}`);
  if (unadvertised.length > 0) {
    console.log(`Called but not advertised (recorder/walker disagree): ${JSON.stringify(unadvertised)}`);
  }

  if (uncovered.length > 0) {
    console.error(`\nUNCOVERED (${uncovered.length}):`);
    for (const id of uncovered) console.error(`  ${id}`);
    console.error(
      "\nEvery method the runtime-introspected ergonomic surface exposes needs a " +
        "live test that calls it and asserts on the RESULT, or an ALLOWLIST entry " +
        "with a justification in test/coverage/gate.mjs.",
    );
    return 1;
  }

  console.log("\nPASS: every introspected ergonomic method is exercised or allowlisted.");
  return 0;
}

process.exit(main());
