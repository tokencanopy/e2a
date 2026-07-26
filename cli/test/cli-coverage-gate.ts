#!/usr/bin/env node
/**
 * CLI command-coverage gate.
 *
 * Every top-level command the BUILT BINARY advertises on `--help` must be
 * exercised (and asserted successful) by test/e2e.test.ts, or this gate
 * fails — the CLI analogue of tests/e2e-prod/mcp_coverage_gate.py, which
 * does the same job for MCP tools/list.
 *
 * SCOPE / denominator choice: the catalog comes from parsing the live
 * `dist/bin/e2a.js --help` output (captured by test/harness/cli-coverage.ts
 * during the e2e run), NOT from grepping cli/src/bin/e2a.ts's switch
 * statement or its USAGE string as source text. A source grep measures the
 * repo instead of the shipped artifact — it would go quietly wrong the
 * moment a command's `case` and its USAGE line drift apart (either one
 * added without the other). This is the same reasoning that caught the
 * MCP-side drift: grepping mcp/src/tools/*.ts for registerTool() found 58
 * tools while the deployed server actually advertised 60. The binary's own
 * advertisement — what a real `e2a --help` invocation shows a user they can
 * run — is the only honest catalog.
 *
 * Inputs:
 *   test/reports/cli-coverage/*.json : shards written by
 *   test/harness/cli-coverage.ts, each {"advertised": [...], "covered": [...]}.
 *
 * "Covered" means a suite invoked the command and asserted its OUTPUT or
 * EXIT CODE, per test/harness/cli-coverage.ts's recordCovered() contract —
 * not merely that the binary ran and exited without a crash.
 *
 * Usage: node cli-coverage-gate.ts [--reports DIR]
 * Exit 0 = every advertised command covered (or allowlisted);
 * Exit 1 = coverage gap;
 * Exit 2 = usage/IO error (including "no shards", which must never read as
 * a pass — an absent run is not a pass).
 */
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const HERE = path.dirname(fileURLToPath(import.meta.url));

// Commands the black-box e2e suite intentionally does NOT drive to a
// successful invocation, with the reason. Keep this SHORT and justified —
// every entry is coverage we knowingly forgo.
const ALLOWLIST: Record<string, string> = {
  login:
    "opens an interactive browser OAuth flow (spawns a local HTTP callback " +
    "server and waits up to 2 minutes for a human to complete a browser " +
    "redirect) — there is no headless way to drive it to a successful " +
    "invocation in CI/e2e without a real browser and a human. Its " +
    "non-interactive PREFLIGHT failure path (unreachable E2A_URL: fails " +
    "fast with a clear error, exit 1, before opening any browser or " +
    "blocking) IS exercised in e2e.test.ts ('login fails fast against an " +
    "unreachable API, before opening a browser') as a defense-in-depth " +
    "check on real code the interactive flow shares — but that is a " +
    "failure-mode assertion, not a successful login, so per this gate's " +
    "success-only coverage rule it does not count as 'covered'.",
};

interface Shard {
  advertised?: string[];
  covered?: string[];
}

function loadShards(reportsDir: string): { advertised: Set<string>; covered: Set<string>; shardCount: number } {
  const advertised = new Set<string>();
  const covered = new Set<string>();
  const files = readdirSync(reportsDir)
    .filter((f) => f.endsWith(".json"))
    .sort();
  for (const f of files) {
    const raw = readFileSync(path.join(reportsDir, f), "utf8");
    const shard = JSON.parse(raw) as Shard;
    for (const n of shard.advertised ?? []) advertised.add(n);
    for (const n of shard.covered ?? []) covered.add(n);
  }
  return { advertised, covered, shardCount: files.length };
}

function main(): number {
  const args = process.argv.slice(2);
  const idx = args.indexOf("--reports");
  const reportsDir = idx !== -1 && args[idx + 1] ? args[idx + 1] : path.join(HERE, "reports", "cli-coverage");

  if (!existsSync(reportsDir)) {
    console.error(
      `cli_coverage_gate: no shard directory at ${reportsDir}. ` +
        "Run the e2e suite first (`npm run test:e2e --workspace @e2a/cli` against staging); " +
        "an absent run is not a pass.",
    );
    return 2;
  }

  const { advertised, covered, shardCount } = loadShards(reportsDir);

  if (shardCount === 0 || advertised.size === 0) {
    console.error(
      "cli_coverage_gate: no --help denominator was recorded. " +
        "At least one e2e test must call `e2a --help` and record its output via recordAdvertised().",
    );
    return 2;
  }

  // An allowlist entry that no longer names an advertised command is a
  // silent hole (e.g. the command was renamed/removed) — fail loudly so the
  // allowlist can't drift stale.
  const stale = Object.keys(ALLOWLIST).filter((name) => !advertised.has(name));
  if (stale.length > 0) {
    console.error(
      `cli_coverage_gate: allowlist entries the binary does not advertise (renamed/removed?): ${JSON.stringify(stale)}`,
    );
    return 2;
  }

  // A covered command the binary never advertised means the recorder and
  // the catalog disagree — report it, but it cannot mask a real gap.
  const unadvertised = [...covered].filter((n) => !advertised.has(n)).sort();

  const missing = [...advertised].filter((n) => !covered.has(n));
  const allowlisted = missing.filter((n) => n in ALLOWLIST).sort();
  const uncovered = missing.filter((n) => !(n in ALLOWLIST)).sort();

  console.log(`Shards             : ${shardCount}`);
  console.log(`Advertised commands: ${advertised.size} ${JSON.stringify([...advertised].sort())}`);
  console.log(`Covered            : ${[...covered].filter((n) => advertised.has(n)).length}`);
  console.log(`Allowlisted        : ${allowlisted.length} ${allowlisted.length ? JSON.stringify(allowlisted) : ""}`);
  if (unadvertised.length > 0) {
    console.log(`Called but not advertised: ${JSON.stringify(unadvertised)}`);
  }

  if (uncovered.length > 0) {
    console.error(`\nUNCOVERED (${uncovered.length}):`);
    for (const name of uncovered) console.error(`  ${name}`);
    console.error(
      "\nEvery advertised e2a command needs an e2e test that invokes it and asserts a real " +
        "success (output or exit code). Add a test, or add an ALLOWLIST entry with a justification.",
    );
    return 1;
  }

  console.log("\nPASS: every advertised e2a command is exercised.");
  return 0;
}

process.exit(main());
