import { writeFileSync, mkdirSync } from "node:fs";
import { fileURLToPath } from "node:url";

// MCP tool-coverage recorder — the MCP analogue of coverage.ts, which does the
// same job for the typed /v1 OpenAPI operations.
//
// The denominator is NOT derived from source. It is whatever the DEPLOYED server
// advertises on `tools/list`, captured live during the run. Grepping
// mcp/src/tools/*.ts for registerTool() would measure the repo instead of the
// thing under test, and would silently drift the moment a tool is registered
// conditionally, renamed, or gated behind a tier. The server's own advertisement
// is the only honest catalog: whatever a real MCP client can see is exactly what
// the suite is accountable for.
//
// A tool counts as covered only when a `tools/call` returned a result with
// isError !== true. An error result means the tool's handler rejected — the same
// reasoning as coverage.ts recording only 2xx: a rejected call proves the route
// exists, not that the tool works.
//
// Shards live in reports/mcp-coverage/ — a separate subdir from the API
// recorder's reports/coverage/ so the two gates can never read each other's
// files, and neither collides with the per-suite {findings} JSONs in reports/.
const MCP_COVERAGE_DIR = fileURLToPath(new URL("../reports/mcp-coverage/", import.meta.url));

const covered = new Set<string>();
const advertised = new Set<string>();
let installed = false;

function installFlush(): void {
  if (installed) return;
  installed = true;
  // Sync flush on 'exit', matching coverage.ts. 'exit' does not fire on
  // SIGKILL, so a force-killed suite contributes no shard and its tools read
  // UNCOVERED — a safe false-FAIL, never a false-PASS, because `pretest` clears
  // the directory so no stale shard can mask the loss.
  process.on("exit", () => {
    if (covered.size === 0 && advertised.size === 0) return;
    try {
      mkdirSync(MCP_COVERAGE_DIR, { recursive: true });
      writeFileSync(
        `${MCP_COVERAGE_DIR}${process.pid}.json`,
        JSON.stringify({ advertised: [...advertised], covered: [...covered] }),
      );
    } catch {
      /* best-effort: coverage is advisory and must never fail a suite */
    }
  });
}

/** Record the live tool catalog the server advertised on `tools/list`. */
export function recordToolList(names: readonly string[]): void {
  for (const n of names) advertised.add(n);
  installFlush();
}

/** Record one `tools/call`. Only a non-error result counts as covered. */
export function recordToolCall(name: string, isError: boolean | undefined): void {
  if (isError === true) return;
  covered.add(name);
  installFlush();
}
