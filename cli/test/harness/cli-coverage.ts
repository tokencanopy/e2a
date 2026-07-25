import { writeFileSync, mkdirSync } from "node:fs";
import { fileURLToPath } from "node:url";

// CLI command-coverage recorder — the CLI analogue of
// tests/e2e-prod/harness/mcp-coverage.ts, which does the same job for MCP
// tools/list.
//
// The denominator is NOT derived from source (grepping bin/e2a.ts's switch
// statement, or its USAGE string, from a file on disk). It is whatever the
// BUILT BINARY advertises on `--help` when actually invoked — captured live
// during the e2e run by parseHelpCommands() below, from the real stdout of a
// spawned `dist/bin/e2a.js --help`. This mirrors the exact reasoning that
// caught the MCP-side drift (58 grepped vs 60 advertised): the binary's own
// advertisement is the only honest catalog, because it's exactly what a real
// user invoking `e2a --help` sees they can run — a source grep can drift the
// moment a command is documented without being wired (or vice versa).
//
// A command counts as covered only when an invocation of it returned a
// *documented success* — not merely that the binary ran and exited zero on
// some incidental usage. Callers decide this per test and call
// recordCovered() only after asserting real output/behavior, exactly as
// suites call recordToolCall() only after checking `isError !== true`.
//
// Shards live in test/reports/cli-coverage/ — analogous to
// tests/e2e-prod/reports/mcp-coverage/, kept out of cli/coverage/ (the
// Vitest --coverage output dir) so the two can never collide.
const CLI_COVERAGE_DIR = fileURLToPath(new URL("../reports/cli-coverage/", import.meta.url));

const covered = new Set<string>();
const advertised = new Set<string>();
let installed = false;

/** Write the shard now. Safe to call more than once (idempotent overwrite). */
function flush(): void {
  if (covered.size === 0 && advertised.size === 0) return;
  try {
    mkdirSync(CLI_COVERAGE_DIR, { recursive: true });
    writeFileSync(
      `${CLI_COVERAGE_DIR}${process.pid}.json`,
      JSON.stringify({ advertised: [...advertised], covered: [...covered] }),
    );
  } catch {
    /* best-effort: coverage is advisory and must never fail a suite */
  }
}

function installFlush(): void {
  if (installed) return;
  installed = true;
  // Best-effort net for a plain `node --test`-style consumer (matching
  // mcp-coverage.ts): 'exit' does not fire on SIGKILL, so a force-killed run
  // contributes no shard and its commands read UNCOVERED — a safe
  // false-FAIL, never a false-PASS, because the shard dir is cleared before
  // each run (see package.json's test:e2e:coverage) so no stale shard can
  // mask the loss.
  //
  // This is NOT sufficient under vitest, though: the e2e suite runs inside a
  // worker (thread or fork) whose lifecycle 'exit' does not reliably line up
  // with — and may never fire relative to — the outer vitest process, so
  // e2e.test.ts also calls flushCliCoverage() explicitly from its own
  // afterAll(). Both paths write the exact same shard shape, so whichever
  // fires first is a no-op duplicate of whichever fires second.
  process.on("exit", flush);
}

/** Explicit flush for runners (vitest) where 'exit' isn't a reliable hook. */
export function flushCliCoverage(): void {
  flush();
}

/**
 * Parse the top-level command catalog out of `e2a --help`'s stdout.
 *
 * Every genuine top-level command line in USAGE (cli/src/bin/e2a.ts) is
 * indented by EXACTLY two spaces before `e2a <command>` — e.g.
 * "  e2a doctor [options]" or "  e2a agents create <email> [--name <n>]".
 * Deeper-indented option/continuation lines (8+ spaces, e.g.
 * "        --agent <email>") and the later "Options:"/"Exit codes:"
 * sections (which reference `e2a` only inside prose, at 17+ spaces of
 * indent, e.g. "e2a send -h — always before any network call") never match
 * this pattern, so only real command entries are picked up. Multi-line
 * subcommand groups (agents/keys/protection/messages each get 2-3 lines)
 * naturally dedupe via the Set.
 */
export function parseHelpCommands(helpText: string): string[] {
  const names = new Set<string>();
  for (const m of helpText.matchAll(/^ {2}e2a (\S+)/gm)) {
    names.add(m[1]);
  }
  return [...names].sort();
}

/** Record the live command catalog the binary advertised on `--help`. */
export function recordAdvertised(names: readonly string[]): void {
  for (const n of names) advertised.add(n);
  installFlush();
}

/** Record one top-level command as genuinely exercised (output/exit asserted). */
export function recordCovered(name: string): void {
  covered.add(name);
  installFlush();
}
