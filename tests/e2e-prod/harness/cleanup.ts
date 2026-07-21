import type { ApiClient } from "./client.ts";

type Kind = "agent" | "domain";

interface Tracked {
  kind: Kind;
  id: string;
}

export interface CleanupResult {
  /** Distinct fixtures in this pass's snapshot. track() dedupes, so 1 per fixture. */
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
  /**
   * Floor for the delay between retries; grows linearly per attempt. A server
   * `Retry-After` wins when it is larger, because internal/ratelimit guarantees
   * a whole-second, sliding-window-aware value — retrying before it elapses is
   * guaranteed to 429 again and just adds load to an already-throttled account.
   */
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
// internal/ratelimit rounds Retry-After up to a whole second, so a sub-second
// floor cannot outlast even the shortest 429 window.
const DEFAULT_BACKOFF_MS = 1_000;
// Teardown must not hang on a long sliding window (a per-minute limit can
// report Retry-After: 60). Wait at most this long, then take the next attempt.
const MAX_BACKOFF_MS = 15_000;

export function track(kind: Kind, id: string): void {
  // Every teardown path in this harness is an `after()` hook, and `after()`
  // does not run when the process dies abnormally — Ctrl-C on a long prod
  // sweep, an unhandled rejection, an uncaught throw outside a test. Anything
  // tracked at that moment leaks a REAL production resource, so arm a
  // process-level net the first time a suite tracks anything.
  armSafetyNet();
  // Dedupe: suites now track a requested identity before the create, which can
  // name an id some other call site already tracked. Without this, one 5xx on
  // a duplicated id gets reported as LEAKED even though its twin deleted fine.
  if (tracked.some((t) => t.kind === kind && t.id === id)) return;
  tracked.push({ kind, id });
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
    let waitMs = backoffMs * attempt;
    try {
      const res = await client.delete(path);
      if (TERMINAL_OK.has(res.status)) return null;
      reason = `HTTP ${res.status}: ${res.raw.slice(0, 200)}`;
      retryable = isRetryableStatus(res.status);
      waitMs = Math.max(waitMs, retryAfterMs(res.headers));
    } catch (e) {
      // A thrown request is a transport failure (DNS, socket reset, abort).
      // The DELETE may well have reached the server, but we cannot know — and
      // this DELETE is idempotent, so retrying is always safe.
      // errMessage(), not `(e as Error).message`: a rejection carrying a
      // non-object (null, a string) would otherwise throw a TypeError out of
      // this catch, out of cleanup()'s loop, and abandon every fixture after
      // this one — the exact all-or-nothing failure this function exists to
      // prevent.
      reason = errMessage(e);
      retryable = true;
    }
    if (!retryable || attempt === attempts) break;
    await sleep(Math.min(waitMs, MAX_BACKOFF_MS));
  }
  return reason;
}

function errMessage(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}

function retryAfterMs(headers: Record<string, string>): number {
  const raw = headers["retry-after"];
  if (!raw) return 0;
  const secs = Number(raw);
  // Delta-seconds only; the HTTP-date form is legal but this API never emits it.
  return Number.isFinite(secs) && secs > 0 ? secs * 1000 : 0;
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

/** Seams so the net is testable without actually killing the test process. */
export interface SafetyNetDeps {
  resolveClient: () => Promise<ApiClient>;
  exit: (code: number) => void;
  write: (line: string) => void;
}

// The net's own sweep must not crawl: defaultClient inherits E2E_RPS (default
// 1 req/s), which on a large registry means a minute-plus of silence after
// Ctrl-C — long enough that an operator presses it again. Its DELETEs are the
// last requests the process makes, so a higher rate costs nothing downstream.
const NET_RPS = 5;

const defaultDeps: SafetyNetDeps = {
  resolveClient: async () => {
    // Imported lazily so this module stays free of env-loading side effects at
    // import time (loadEnv() throws when the API key is unset).
    const { ApiClient } = await import("./client.ts");
    return new ApiClient(undefined, NET_RPS);
  },
  exit: (code) => process.exit(code),
  write: (line) => process.stderr.write(line),
};

let deps: SafetyNetDeps = defaultDeps;
let safetyNetArmed = false;
let safetyNetRunning = false;

/**
 * Arm process-level handlers that run a best-effort cleanup pass when the
 * process is about to die without reaching its `after()` hooks. Idempotent;
 * called automatically from track().
 *
 * The handlers always re-establish a non-zero exit, so a suite that crashed
 * still reports failure. Note the deliberate trade: handling uncaughtException
 * means node:test loses its "which test generated this" attribution, so the
 * cause (with stack) is written to stderr first. Deleting a real production
 * agent is worth more than one diagnostic line.
 */
export function armSafetyNet(): void {
  if (safetyNetArmed) return;
  safetyNetArmed = true;
  // `on`, not `once`: a second SIGINT must still be caught. With `once` the
  // handler is consumed by the first signal and an impatient second Ctrl-C
  // hits Node's default terminate, killing the sweep mid-flight and taking
  // the leak report with it — strictly worse than not sweeping at all.
  process.on("SIGINT", () => void runSafetyNet("SIGINT", 130));
  process.on("SIGTERM", () => void runSafetyNet("SIGTERM", 143));
  process.on("uncaughtException", (e) => void runSafetyNet(`uncaughtException: ${e?.stack ?? e}`, 1));
  process.on("unhandledRejection", (e) => void runSafetyNet(`unhandledRejection: ${String(e)}`, 1));
}

/** Test-only: drop the handlers so a unit test cannot fire a real sweep. */
export function disarmSafetyNet(): void {
  process.removeAllListeners("SIGINT");
  process.removeAllListeners("SIGTERM");
  process.removeAllListeners("uncaughtException");
  process.removeAllListeners("unhandledRejection");
  safetyNetArmed = false;
  safetyNetRunning = false;
}

/** Test-only: swap the net's client/exit/stderr seams. Pass nothing to restore. */
export function setSafetyNetDeps(d?: Partial<SafetyNetDeps>): void {
  deps = d ? { ...defaultDeps, ...d } : defaultDeps;
}

export async function runSafetyNet(cause: string, exitCode: number): Promise<void> {
  if (safetyNetRunning) {
    // Second signal while the first sweep is in flight. Don't start a second
    // concurrent pass (it would double the request pressure on an already
    // throttled account) — report what is still outstanding and go now, since
    // the operator has asked twice.
    deps.write(`[e2e-prod] teardown net interrupted (${cause}) — ${tracked.length} fixture(s) NOT deleted\n`);
    for (const t of tracked) deps.write(`[e2e-prod]   LEAKED ${t.kind} ${t.id}\n`);
    deps.exit(exitCode);
    return;
  }
  safetyNetRunning = true;
  // Print the cause first — handling uncaughtException suppresses Node's own
  // crash report, and a silently-swallowed crash is worse than a leaked fixture.
  deps.write(`\n[e2e-prod] teardown net fired (${cause})\n`);
  if (tracked.length > 0) {
    deps.write(`[e2e-prod] deleting ${tracked.length} tracked fixture(s) before exit — Ctrl-C again to abandon them\n`);
    try {
      // One attempt each: the process is already dying and may be on a signal
      // deadline, so a full retry budget risks being killed mid-sweep.
      const r = await cleanup(await deps.resolveClient(), { attempts: 1 });
      deps.write(`[e2e-prod] teardown net: ${r.succeeded} deleted, ${r.failed.length} LEAKED\n`);
      for (const f of r.failed) {
        deps.write(`[e2e-prod]   LEAKED ${f.kind} ${f.id}: ${f.reason}\n`);
      }
    } catch (e) {
      deps.write(`[e2e-prod] teardown net failed: ${errMessage(e)}\n`);
      for (const t of tracked) deps.write(`[e2e-prod]   LEAKED ${t.kind} ${t.id}\n`);
    }
  }
  deps.exit(exitCode);
}
