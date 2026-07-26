// Coverage recorder — the TS-SDK analogue of tests/e2e-prod/harness/mcp-coverage.ts.
//
// `recordSurface` is called once per test file (in `beforeAll`) with the full
// runtime-introspected denominator (see introspect.ts) — idempotent, since
// every file walks the SAME client shape, so it's safe for many files to call
// it. `recordCovered` is called explicitly, by hand, right after a test's
// assertions on the RESULT of a call succeed — there is no single transport
// chokepoint analogous to the MCP client's tools/call wrapper to hook this
// automatically, so "covered" here means something slightly stronger than the
// MCP gate's "isError !== true": it means the call succeeded AND the test
// asserted the returned data was what was expected. A test that throws before
// reaching its recordCovered() call correctly leaves that id unrecorded.
//
// Shards flush to test/coverage/reports/, one file per shard (filename = pid +
// a random suffix, robust to whichever pool Vitest uses). Flushing is
// EXPLICIT (`flushCoverage`, called from every file's own `afterAll`) rather
// than relying solely on a process 'exit' handler — Vitest's worker pools
// (threads/forks) do not reliably fire a Node 'exit' event on the module
// realm coverage state lives in before the worker is torn down, which was
// observed to silently drop every shard. The 'exit' handler stays installed
// as a defense-in-depth fallback; it is not the primary mechanism.
// gate.mjs sums every shard under reports/; an empty/missing directory there
// is a hard failure, never a silent pass.
import { writeFileSync, mkdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { randomBytes } from "node:crypto";

const REPORTS_DIR = fileURLToPath(new URL("./reports/", import.meta.url));
const SHARD_ID = `${process.pid}-${randomBytes(4).toString("hex")}`;

const advertised = new Set<string>();
const covered = new Set<string>();
let installed = false;

function writeShard(): void {
  if (advertised.size === 0 && covered.size === 0) return;
  try {
    mkdirSync(REPORTS_DIR, { recursive: true });
    writeFileSync(
      `${REPORTS_DIR}${SHARD_ID}.json`,
      JSON.stringify({ advertised: [...advertised], covered: [...covered] }),
    );
  } catch {
    /* best-effort: coverage bookkeeping must never fail a suite */
  }
}

function installFlush(): void {
  if (installed) return;
  installed = true;
  process.on("exit", writeShard);
}

/** Record the full runtime-introspected ergonomic surface (the denominator). */
export function recordSurface(ids: readonly string[]): void {
  for (const id of ids) advertised.add(id);
  installFlush();
}

/** Record one method id as genuinely covered — call AFTER asserting on the
 *  call's result, never merely after a call that didn't throw. */
export function recordCovered(id: string): void {
  covered.add(id);
  installFlush();
}

/** Explicitly flush this file's shard now. Call from every test file's own
 *  `afterAll` — see the module comment for why this can't be left to a
 *  process 'exit' handler alone under Vitest's worker pools. Idempotent. */
export function flushCoverage(): void {
  writeShard();
}
