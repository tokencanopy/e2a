import { writeSync } from "node:fs";
import type { ApiClient } from "./client.ts";

type Kind = "agent" | "domain";

export interface CleanupFixture {
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
  /**
   * Total DELETE attempts per fixture before giving up. Cleanup runs at the
   * tail of a suite, when the run's rate-limit budget is most depleted and a
   * 429 is most likely, so a single-shot pass abandons real production
   * fixtures for reasons that would clear on a second try.
   */
  attempts?: number;
	/**
	 * Attempt budget for the two known transient 409s. send_in_progress may
	 * legally remain fresh for ten minutes, so the normal three-attempt budget
	 * is not a meaningful cleanup guarantee. Defaults to 49 (>10 minutes of
	 * the capped backoff); explicitly setting attempts also caps this unless
	 * this option is provided.
	 */
	conflictAttempts?: number;
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

const tracked: CleanupFixture[] = [];

// A DELETE landing on one of these is terminal-good: the fixture is gone, or
// A not-found response is the delete endpoints' anti-enumeration result for
// both absent and not-owned resources. A 403 is a real scope/access failure and
// therefore MUST remain tracked rather than being misreported as deletion.
const TERMINAL_OK = new Set([200, 204, 404]);

const DEFAULT_ATTEMPTS = 3;
// 49 attempts yield at least 615 seconds of capped waits before the final
// attempt, exceeding the server's ten-minute active-send lease.
const DEFAULT_CONFLICT_ATTEMPTS = 49;
// internal/ratelimit rounds Retry-After up to a whole second, so a sub-second
// floor cannot outlast even the shortest 429 window.
const DEFAULT_BACKOFF_MS = 1_000;
// Teardown must not hang on a long sliding window (a per-minute limit can
// report Retry-After: 60). Wait at most this long, then take the next attempt.
const MAX_BACKOFF_MS = 15_000;

export function track(kind: Kind, id: string): void {
  // Arm the leak reporter the first time a suite tracks anything, so a fixture
  // that outlives teardown announces itself (see the reporter section below).
  armLeakReporter();
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
  return cleanupFixtures(client, [...tracked], opts);
}

/** Delete only the named fixtures, while keeping the shared leak registry honest. */
export async function cleanupFixtures(
  client: ApiClient,
  fixtures: readonly CleanupFixture[],
  opts: CleanupOpts = {},
): Promise<CleanupResult> {
	const attempts = Math.max(1, opts.attempts ?? DEFAULT_ATTEMPTS);
	const conflictAttempts = Math.max(
		attempts,
		opts.conflictAttempts ?? (opts.attempts === undefined ? DEFAULT_CONFLICT_ATTEMPTS : attempts),
	);
  const backoffMs = opts.backoffMs ?? DEFAULT_BACKOFF_MS;
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));

  const failed: CleanupResult["failed"] = [];
  let succeeded = 0;
  // Snapshot up front: the loop mutates `tracked` via untrack().
  const batch = [...fixtures].reverse();
  for (const t of batch) {
    // Each fixture is fully isolated — its own try/catch AND its own retry
    // budget — so neither a hard failure nor an exhausted retry on one fixture
    // can stop the remaining ones from being deleted.
		const reason = await deleteWithRetry(client, t, attempts, conflictAttempts, backoffMs, sleep);
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
	t: CleanupFixture,
	attempts: number,
	conflictAttempts: number,
  backoffMs: number,
  sleep: (ms: number) => Promise<void>,
): Promise<string | null> {
  const path = pathFor(t);
  let reason = "no attempt made";
	const maxAttempts = Math.max(attempts, conflictAttempts);
	// The budget is monotonic: once any attempt observes a known transient 409,
	// the fixture has entered the (up to ten-minute) conflict wait and keeps the
	// larger budget for the rest of its attempts. A 429/5xx/transport blip
	// interleaved with the conflict responses consumes one attempt — it must not
	// shrink the budget back to `attempts` and abandon the fixture mid-wait.
	let budget = attempts;
	for (let attempt = 1; attempt <= maxAttempts; attempt++) {
		let retryable: boolean;
    let waitMs = backoffMs * attempt;
    try {
      const res = await client.delete(path);
      if (TERMINAL_OK.has(res.status)) return null;
			reason = `HTTP ${res.status}: ${res.raw.slice(0, 200)}`;
			retryable = isRetryableResponse(res.status, res.raw);
			if (isRetryableConflictResponse(res.status, res.raw)) budget = Math.max(budget, conflictAttempts);
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
		if (!retryable || attempt >= budget) break;
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

// 429 is the realistic one — cleanup competes with the suite's own traffic.
// Permanent agent deletion can also race an active send or an earlier purge;
// those two machine codes are explicitly retryable, unlike arbitrary 409s.
function isRetryableResponse(status: number, raw: string): boolean {
	if (status === 429 || status === 408 || status >= 500) return true;
	return isRetryableConflictResponse(status, raw);
}

function isRetryableConflictResponse(status: number, raw: string): boolean {
	if (status !== 409) return false;
  try {
    const parsed = JSON.parse(raw) as { code?: unknown; error?: { code?: unknown } };
    const code = parsed.code ?? parsed.error?.code;
    return code === "send_in_progress" || code === "purge_in_progress";
  } catch {
    return false;
  }
}

function pathFor(t: CleanupFixture): string {
  // Destructive deletes require ?confirm=DELETE (the API's irreversible-op
  // guard). Cleanup is always intentional, so we always confirm.
  //
  // Agents additionally need permanent=true. A plain confirmed DELETE only
  // moves an agent to the trash: messages_deleted is 0, bodies stay stored,
  // and the row keeps counting against usage.storage_bytes until the janitor
  // purges it at the end of the trash-retention window (30 days). Fixtures
  // are throwaway by definition and nothing ever restores them, so leaving
  // them to age out just accumulates tombstones on the test account — one
  // suite run at a time, faster than they expire.
  //
  // That accumulation is not only untidy: past ~7k agents on one account, the
  // account-scoped metrics queries stop being able to use their index and
  // fall back to sequential scans of the whole messages/transitions tables,
  // which is what made the staging metrics gate time out. Purging here keeps
  // the test account's agent count proportional to a single run.
  switch (t.kind) {
    case "agent":
      return `/v1/agents/${encodeURIComponent(t.id)}?confirm=DELETE&permanent=true`;
    case "domain":
      return `/v1/domains/${encodeURIComponent(t.id)}?confirm=DELETE`;
  }
}

export function getTracked(): readonly CleanupFixture[] {
  return tracked;
}

// --- abnormal-exit leak reporter -----------------------------------------
//
// The reliable teardown path is the per-suite `after()` hook calling cleanup()
// (with the retry/isolation above). `after()` does NOT run when the process
// dies without finishing normally: a module-level throw, an unhandled
// rejection, or a Ctrl-C. Anything still tracked at that point is a REAL
// leaked production resource.
//
// This reporter deliberately does NOT try to DELETE on the way out. An earlier
// version did, and it was a trap: `npm test` runs each suite in its own child
// process (Node's `--test-isolation=process` default), so a terminal Ctrl-C
// signals the whole process group — the node:test RUNNER parent has no signal
// handler, exits in milliseconds, and tears the child down long before an
// async, rate-limited DELETE sweep could finish. A net that deletes 1 of N
// fixtures while claiming to be a safety net is worse than an honest reporter.
// Deleting on crash needs a supervisor ABOVE the child (a wrapper that traps
// the signal and reaps), which is out of scope for this module.
//
// So instead we surface the leak synchronously: print every still-tracked id
// on the way out, in a form a human can act on. That directly serves the
// manual-deletion follow-up leaked fixtures require, and it cannot hang, cannot
// double-delete, and cannot stomp the runner's own handlers.

/** Test seams: swap stderr and exit so a unit test never writes to a real fd. */
export interface ReporterDeps {
  write: (line: string) => void;
  exit: (code: number) => void;
}

const defaultReporterDeps: ReporterDeps = {
  // writeSync(2), not process.stderr.write: an async write during the 'exit'
  // event can be truncated, and on Ctrl-C the runner may already have torn
  // down the pipe — an EPIPE thrown here escapes the 'exit' listener as an
  // uncaught exception, masks the exit code, and buries the leak report.
  write: (line) => {
    try {
      writeSync(2, line);
    } catch {
      // stderr is gone; the exit path must never throw.
    }
  },
  exit: (code) => process.exit(code),
};

let reporterDeps: ReporterDeps = defaultReporterDeps;
let reporterArmed = false;
let reported = false;

// A signal fires no `process.on("exit")`, so translate the common ones into an
// explicit exit — which DOES run the exit handler, and thus the reporter. Kept
// as named references so disarm can remove exactly these, never the runner's.
// SIGHUP matters for runs over SSH: a terminal close default-terminates
// without an 'exit' event, so without a handler no report would print.
const onExit = () => reportLeaks("exit");
const onSigint = () => reporterDeps.exit(130);
const onSigterm = () => reporterDeps.exit(143);
const onSighup = () => reporterDeps.exit(129);

/** Idempotent; called automatically from track(). */
export function armLeakReporter(): void {
  if (reporterArmed) return;
  reporterArmed = true;
  process.on("exit", onExit);
  process.on("SIGINT", onSigint);
  process.on("SIGTERM", onSigterm);
  process.on("SIGHUP", onSighup);
}

/**
 * Test-only: remove exactly the handlers armLeakReporter added. Uses `off`
 * with retained references, NOT removeAllListeners — the latter would also
 * tear out node:test's own uncaughtException/rejection handlers and silently
 * break test attribution.
 */
export function disarmLeakReporter(): void {
  process.off("exit", onExit);
  process.off("SIGINT", onSigint);
  process.off("SIGTERM", onSigterm);
  process.off("SIGHUP", onSighup);
  reporterArmed = false;
  reported = false;
}

/** Test-only: swap the write/exit seams. Pass nothing to restore the defaults. */
export function setReporterDeps(d?: Partial<ReporterDeps>): void {
  reporterDeps = d ? { ...defaultReporterDeps, ...d } : defaultReporterDeps;
}

/**
 * Synchronously print every still-tracked fixture. Runs at process exit (and
 * is idempotent via `reported`, since a signal handler's explicit exit re-runs
 * the exit handler). No-op when the registry is empty — a suite whose `after()`
 * cleaned up prints nothing.
 */
export function reportLeaks(cause: string): void {
  if (reported || tracked.length === 0) return;
  reported = true;
  reporterDeps.write(
    `\n[e2e-prod] ${tracked.length} fixture(s) still tracked at ${cause} — teardown did not run to completion:\n` +
      `[e2e-prod] (a tracked identity whose create never succeeded may appear here; its manual delete will 404, harmlessly)\n`,
  );
  for (const t of tracked) {
    reporterDeps.write(`[e2e-prod]   LEAKED ${t.kind} ${t.id} — delete manually\n`);
  }
}
