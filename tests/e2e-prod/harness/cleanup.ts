import type { ApiClient } from "./client.ts";

type Kind = "agent" | "domain";

interface Tracked {
  kind: Kind;
  id: string;
}

export interface CleanupResult {
  attempted: number;
  succeeded: number;
  failed: Array<{ kind: Kind; id: string; reason: string }>;
}

export interface CleanupOpts {
  force?: boolean;
  /**
   * Total DELETE attempts per fixture before giving up. Cleanup runs at the
   * tail of a suite, when the run's rate-limit budget is most depleted and a
   * 429 is most likely, so a single-shot pass abandons real production
   * fixtures for reasons that would clear on a second try.
   */
  attempts?: number;
  /** Base delay between retries; grows linearly per attempt. */
  backoffMs?: number;
  /** Injectable so unit tests don't burn real wall-clock time. */
  sleep?: (ms: number) => Promise<void>;
}

const tracked: Tracked[] = [];

// A DELETE landing on one of these is terminal-good: the fixture is gone, or
// was never ours to begin with (403 == "not owned"/already deleted under
// anti-enumeration semantics). Stop retrying and untrack.
const TERMINAL_OK = new Set([200, 204, 404, 403]);

const DEFAULT_ATTEMPTS = 3;
const DEFAULT_BACKOFF_MS = 500;

export function track(kind: Kind, id: string): void {
  tracked.push({ kind, id });
  // Every teardown path in this harness is an `after()` hook, and `after()`
  // does not run when the process dies abnormally — Ctrl-C on a long prod
  // sweep, an unhandled rejection, an uncaught throw outside a test. Anything
  // tracked at that moment leaks a REAL production resource, so arm a
  // process-level net the first time a suite tracks anything.
  armSafetyNet();
}

export function untrack(kind: Kind, id: string): void {
  const i = tracked.findIndex((t) => t.kind === kind && t.id === id);
  if (i >= 0) tracked.splice(i, 1);
}

export async function cleanup(client: ApiClient, opts: CleanupOpts = {}): Promise<CleanupResult> {
  const attempts = Math.max(1, opts.attempts ?? DEFAULT_ATTEMPTS);
  const backoffMs = opts.backoffMs ?? DEFAULT_BACKOFF_MS;
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));

  const failed: CleanupResult["failed"] = [];
  let succeeded = 0;
  // Snapshot up front: the loop mutates `tracked` via untrack().
  const batch = [...tracked].reverse();
  for (const t of batch) {
    // Each fixture is fully isolated — its own try/catch AND its own retry
    // budget — so neither a hard failure nor an exhausted retry on one fixture
    // can stop the remaining ones from being deleted.
    const reason = await deleteWithRetry(client, t, attempts, backoffMs, sleep);
    if (reason === null) {
      succeeded++;
      untrack(t.kind, t.id);
    } else {
      failed.push({ ...t, reason });
    }
  }
  return { attempted: batch.length, succeeded, failed };
}

/** Returns null once the fixture is gone, or a human-readable reason on give-up. */
async function deleteWithRetry(
  client: ApiClient,
  t: Tracked,
  attempts: number,
  backoffMs: number,
  sleep: (ms: number) => Promise<void>,
): Promise<string | null> {
  const path = pathFor(t);
  let reason = "no attempt made";
  for (let attempt = 1; attempt <= attempts; attempt++) {
    let retryable: boolean;
    try {
      const res = await client.delete(path);
      if (TERMINAL_OK.has(res.status)) return null;
      reason = `HTTP ${res.status}: ${res.raw.slice(0, 200)}`;
      retryable = isRetryableStatus(res.status);
    } catch (e) {
      // A thrown request is a transport failure (DNS, socket reset, abort).
      // The DELETE may well have reached the server, but we cannot know — and
      // this DELETE is idempotent, so retrying is always safe.
      reason = (e as Error).message;
      retryable = true;
    }
    if (!retryable || attempt === attempts) break;
    await sleep(backoffMs * attempt);
  }
  return reason;
}

// 429 is the realistic one — cleanup competes with the suite's own traffic
// against the agent rate limit. 408 and 5xx are transient by definition.
function isRetryableStatus(status: number): boolean {
  return status === 429 || status === 408 || status >= 500;
}

function pathFor(t: Tracked): string {
  // Destructive deletes require ?confirm=DELETE (the API's irreversible-op
  // guard). Cleanup is always intentional, so we always confirm.
  switch (t.kind) {
    case "agent":
      return `/v1/agents/${encodeURIComponent(t.id)}?confirm=DELETE`;
    case "domain":
      return `/v1/domains/${encodeURIComponent(t.id)}?confirm=DELETE`;
  }
}

export function getTracked(): readonly Tracked[] {
  return tracked;
}

// --- abnormal-exit safety net --------------------------------------------

let safetyNetArmed = false;
let safetyNetRunning = false;

/**
 * Arm process-level handlers that run a best-effort cleanup pass when the
 * process is about to die without reaching its `after()` hooks. Idempotent;
 * called automatically from track().
 *
 * The handlers always re-establish a non-zero exit, so a suite that crashed
 * still reports failure — the net changes what gets deleted, never whether
 * the run is considered green.
 */
export function armSafetyNet(): void {
  if (safetyNetArmed) return;
  safetyNetArmed = true;
  process.once("SIGINT", () => void runSafetyNet("SIGINT", 130));
  process.once("SIGTERM", () => void runSafetyNet("SIGTERM", 143));
  process.once("uncaughtException", (e) => void runSafetyNet(`uncaughtException: ${e?.stack ?? e}`, 1));
  process.once("unhandledRejection", (e) => void runSafetyNet(`unhandledRejection: ${String(e)}`, 1));
}

async function runSafetyNet(cause: string, exitCode: number): Promise<void> {
  if (safetyNetRunning) return;
  safetyNetRunning = true;
  // Print the cause first — handling uncaughtException suppresses Node's own
  // crash report, and a silently-swallowed crash is worse than a leaked fixture.
  process.stderr.write(`\n[e2e-prod] teardown net fired (${cause})\n`);
  if (tracked.length > 0) {
    process.stderr.write(`[e2e-prod] deleting ${tracked.length} tracked fixture(s) before exit\n`);
    try {
      // Imported lazily so this module stays free of env-loading side effects
      // at import time (loadEnv() throws when the API key is unset).
      const { defaultClient } = await import("./client.ts");
      // One attempt each: the process is already dying and may be on a signal
      // deadline, so a full retry budget risks being killed mid-sweep.
      const r = await cleanup(defaultClient, { attempts: 1 });
      process.stderr.write(`[e2e-prod] teardown net: ${r.succeeded} deleted, ${r.failed.length} LEAKED\n`);
      for (const f of r.failed) {
        process.stderr.write(`[e2e-prod]   LEAKED ${f.kind} ${f.id}: ${f.reason}\n`);
      }
    } catch (e) {
      process.stderr.write(`[e2e-prod] teardown net failed: ${(e as Error).message}\n`);
      for (const t of tracked) {
        process.stderr.write(`[e2e-prod]   LEAKED ${t.kind} ${t.id}\n`);
      }
    }
  }
  process.exit(exitCode);
}
