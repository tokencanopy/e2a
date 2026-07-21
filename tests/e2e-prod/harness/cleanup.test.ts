// Unit tests for the teardown registry. These NEVER touch production: the
// only thing cleanup() calls on its client is `delete(path)`, so a fake stands
// in for ApiClient and no network request is made. Deliberately NOT under
// `suites/` — `npm test` globs `suites/*.test.ts` and runs against live prod.
// Run these with `npm run test:harness`.
import { test, beforeEach, after } from "node:test";
import assert from "node:assert/strict";
import type { ApiClient, RawResponse } from "./client.ts";
import {
  cleanup,
  track,
  untrack,
  getTracked,
  disarmSafetyNet,
  setSafetyNetDeps,
  runSafetyNet,
} from "./cleanup.ts";

/** A scripted response: an HTTP status, a thrown value, or a status + headers. */
type Outcome = number | Error | unknown | { status: number; headers: Record<string, string> };

/** A DELETE recorder whose per-path responses are scripted by the test. */
function fakeClient(script: (path: string, attempt: number) => Outcome): {
  client: ApiClient;
  calls: string[];
} {
  const calls: string[] = [];
  const attempts = new Map<string, number>();
  const client = {
    delete(path: string): Promise<RawResponse> {
      calls.push(path);
      const n = (attempts.get(path) ?? 0) + 1;
      attempts.set(path, n);
      const outcome = script(path, n);
      const shaped =
        typeof outcome === "number"
          ? { status: outcome, headers: {} as Record<string, string> }
          : outcome && typeof outcome === "object" && "status" in outcome
            ? (outcome as { status: number; headers: Record<string, string> })
            : null;
      // Anything that isn't a status shape is a rejection — including non-Error
      // values, which is the point of the isolation test below.
      if (shaped === null) return Promise.reject(outcome);
      return Promise.resolve({
        status: shaped.status,
        ok: shaped.status < 400,
        headers: shaped.headers,
        body: null,
        raw: "",
        latencyMs: 0,
      });
    },
  } as unknown as ApiClient;
  return { client, calls };
}

const noSleep = () => Promise.resolve();

function drainTracked(): void {
  for (const t of [...getTracked()]) untrack(t.kind, t.id);
}

beforeEach(() => {
  // The registry is module-level state shared across tests in this file.
  drainTracked();
  // track() arms the real process-level net, whose default client is built
  // from the ambient E2A_API_KEY. A Ctrl-C mid-run would otherwise fire real
  // DELETEs at production for these synthetic ids. Disarm it; the tests that
  // exercise the net re-enter it deliberately through injected deps.
  disarmSafetyNet();
  setSafetyNetDeps();
});

after(() => {
  disarmSafetyNet();
  setSafetyNetDeps();
});

test("cleanup deletes every tracked fixture and empties the registry", async () => {
  track("agent", "a@x.dev");
  track("agent", "b@x.dev");
  track("domain", "d.x.dev");
  const { client, calls } = fakeClient(() => 204);

  const r = await cleanup(client, { sleep: noSleep });

  assert.equal(r.attempted, 3);
  assert.equal(r.succeeded, 3);
  assert.deepEqual(r.failed, []);
  assert.equal(getTracked().length, 0);
  assert.equal(calls.length, 3);
  // Reverse order (newest fixture first) is the existing teardown contract.
  assert.match(calls[0]!, /d\.x\.dev/);
  assert.match(calls[0]!, /confirm=DELETE/);
});

test("a permanently failing fixture does not block the remaining ones", async () => {
  track("agent", "first@x.dev");
  track("agent", "doomed@x.dev");
  track("agent", "last@x.dev");
  // 400 is non-retryable, so this is one attempt and a hard give-up.
  const { client } = fakeClient((path) => (path.includes("doomed") ? 400 : 204));

  const r = await cleanup(client, { sleep: noSleep });

  assert.equal(r.attempted, 3);
  assert.equal(r.succeeded, 2, "the other two fixtures must still be deleted");
  assert.equal(r.failed.length, 1);
  assert.equal(r.failed[0]!.id, "doomed@x.dev");
  // Only the failure stays tracked, so the safety net can report it as leaked.
  assert.deepEqual(
    getTracked().map((t) => t.id),
    ["doomed@x.dev"],
  );
});

test("a thrown request does not block the remaining fixtures", async () => {
  track("agent", "first@x.dev");
  track("agent", "boom@x.dev");
  track("agent", "last@x.dev");
  const { client } = fakeClient((path) =>
    path.includes("boom") ? new Error("socket hang up") : 204,
  );

  const r = await cleanup(client, { attempts: 2, sleep: noSleep });

  assert.equal(r.succeeded, 2);
  assert.equal(r.failed.length, 1);
  assert.match(r.failed[0]!.reason, /socket hang up/);
});

test("a transient 429 is retried and the fixture is still deleted", async () => {
  track("agent", "ratelimited@x.dev");
  // This is the production failure mode: cleanup runs at the tail of a suite,
  // when the run's rate-limit budget is most depleted. Pre-fix, this single
  // 429 abandoned a real agent permanently.
  const { client, calls } = fakeClient((_path, attempt) => (attempt === 1 ? 429 : 204));

  const r = await cleanup(client, { sleep: noSleep });

  assert.equal(r.succeeded, 1);
  assert.deepEqual(r.failed, []);
  assert.equal(calls.length, 2, "should have retried exactly once");
  assert.equal(getTracked().length, 0);
});

test("5xx is retried; a non-retryable status is not", async () => {
  track("agent", "flaky@x.dev");
  const flaky = fakeClient((_p, attempt) => (attempt < 3 ? 503 : 200));
  const r1 = await cleanup(flaky.client, { attempts: 3, sleep: noSleep });
  assert.equal(r1.succeeded, 1);
  assert.equal(flaky.calls.length, 3);

  drainTracked();
  track("agent", "hard@x.dev");
  const hard = fakeClient(() => 422);
  const r2 = await cleanup(hard.client, { attempts: 3, sleep: noSleep });
  assert.equal(r2.failed.length, 1);
  assert.equal(hard.calls.length, 1, "422 must not consume the retry budget");
});

test("retries are bounded and the give-up reason names the last status", async () => {
  track("agent", "downforever@x.dev");
  const { client, calls } = fakeClient(() => 500);

  const r = await cleanup(client, { attempts: 3, sleep: noSleep });

  assert.equal(calls.length, 3);
  assert.equal(r.succeeded, 0);
  assert.match(r.failed[0]!.reason, /HTTP 500/);
});

test("backoff grows per attempt", async () => {
  track("agent", "slow@x.dev");
  const slept: number[] = [];
  const { client } = fakeClient(() => 429);

  await cleanup(client, {
    attempts: 3,
    backoffMs: 100,
    sleep: (ms) => {
      slept.push(ms);
      return Promise.resolve();
    },
  });

  // Two waits for three attempts — none after the final one.
  assert.deepEqual(slept, [100, 200]);
});

test("404 and 403 count as success (already gone / anti-enumeration)", async () => {
  track("agent", "gone@x.dev");
  track("agent", "notours@x.dev");
  const { client } = fakeClient((path) => (path.includes("gone") ? 404 : 403));

  const r = await cleanup(client, { sleep: noSleep });

  assert.equal(r.succeeded, 2);
  assert.equal(getTracked().length, 0);
});

test("tracking the same identity twice registers it once", async () => {
  // Over-tracking is the safe direction — suites now track a requested identity
  // before the create, which can duplicate an id tracked elsewhere — but a
  // duplicate registry entry makes one 5xx report a LEAK for a fixture whose
  // twin deleted fine. track() dedupes so the report stays truthful.
  track("agent", "dupe@x.dev");
  track("agent", "dupe@x.dev");
  assert.equal(getTracked().length, 1, "duplicate track must not double-register");
  const { client, calls } = fakeClient(() => 204);

  const r = await cleanup(client, { sleep: noSleep });

  assert.equal(r.attempted, 1);
  assert.equal(r.succeeded, 1);
  assert.equal(calls.length, 1, "one fixture, one DELETE");
  assert.equal(getTracked().length, 0);
});

test("kind is part of identity — same id under two kinds stays two fixtures", async () => {
  track("agent", "x.dev");
  track("domain", "x.dev");
  assert.equal(getTracked().length, 2);
  const { client, calls } = fakeClient(() => 204);

  await cleanup(client, { sleep: noSleep });

  assert.equal(calls.length, 2);
  assert.ok(calls.some((c) => c.startsWith("/v1/agents/")));
  assert.ok(calls.some((c) => c.startsWith("/v1/domains/")));
});

test("a rejection carrying a non-Error does not abandon the remaining fixtures", async () => {
  // Regression guard: `(e as Error).message` on a null rejection throws a
  // TypeError out of the catch, out of cleanup()'s loop, and skips every
  // fixture queued behind it — silently converting a per-fixture failure into
  // an all-or-nothing one. `first` is deleted LAST (reverse order), so it is
  // the one that goes missing when isolation breaks.
  track("agent", "first@x.dev");
  track("agent", "nonerror@x.dev");
  track("agent", "last@x.dev");
  const { client } = fakeClient((path) => (path.includes("nonerror") ? null : 204));

  const r = await cleanup(client, { attempts: 1, sleep: noSleep });

  assert.equal(r.succeeded, 2, "both healthy fixtures must still be deleted");
  assert.equal(r.failed.length, 1);
  assert.equal(r.failed[0]!.id, "nonerror@x.dev");
  assert.deepEqual(
    getTracked().map((t) => t.id),
    ["nonerror@x.dev"],
  );
});

test("a 429 Retry-After longer than the backoff floor is honored, and capped", async () => {
  // internal/ratelimit rounds Retry-After up to a whole second of the remaining
  // sliding window. Retrying on a sub-second backoff is guaranteed to 429 again
  // and just adds load to an account that is already throttled.
  track("agent", "throttled@x.dev");
  const slept: number[] = [];
  const { client } = fakeClient((_p, attempt) =>
    attempt === 1
      ? { status: 429, headers: { "retry-after": "4" } }
      : { status: 429, headers: { "retry-after": "600" } },
  );

  const r = await cleanup(client, {
    attempts: 3,
    backoffMs: 1_000,
    sleep: (ms) => {
      slept.push(ms);
      return Promise.resolve();
    },
  });

  // 4s beats the 1s floor; 600s is clamped so teardown cannot hang for 10 min.
  assert.deepEqual(slept, [4_000, 15_000]);
  assert.equal(r.failed.length, 1);
});

test("a Retry-After shorter than the backoff floor does not shorten the wait", async () => {
  track("agent", "throttled@x.dev");
  const slept: number[] = [];
  const { client } = fakeClient(() => ({ status: 429, headers: { "retry-after": "0" } }));

  await cleanup(client, {
    attempts: 2,
    backoffMs: 1_000,
    sleep: (ms) => {
      slept.push(ms);
      return Promise.resolve();
    },
  });

  assert.deepEqual(slept, [1_000]);
});

test("cleanup on an empty registry is a no-op", async () => {
  const { client, calls } = fakeClient(() => 204);
  const r = await cleanup(client, { sleep: noSleep });
  assert.deepEqual(r, { attempted: 0, succeeded: 0, failed: [] });
  assert.equal(calls.length, 0);
});

// --- abnormal-exit safety net --------------------------------------------
//
// These drive runSafetyNet() directly with injected client/exit/stderr seams,
// so nothing kills this process and no request leaves it. They deliberately do
// NOT install signal handlers.

/** Captures the net's exit code and stderr, and hands it a scripted client. */
function netHarness(script: (path: string, attempt: number) => Outcome) {
  const { client, calls } = fakeClient(script);
  const exits: number[] = [];
  const lines: string[] = [];
  setSafetyNetDeps({
    resolveClient: () => Promise.resolve(client),
    exit: (code) => exits.push(code),
    write: (line) => lines.push(line),
  });
  return { calls, exits, out: () => lines.join("") };
}

test("safety net deletes tracked fixtures and exits non-zero", async () => {
  track("agent", "doomed@x.dev");
  track("domain", "doomed.x.dev");
  const net = netHarness(() => 204);

  await runSafetyNet("SIGINT", 130);

  assert.equal(net.calls.length, 2, "both fixtures must be deleted before exit");
  assert.equal(getTracked().length, 0);
  assert.deepEqual(net.exits, [130], "the signal's exit code must be preserved");
  assert.match(net.out(), /teardown net fired \(SIGINT\)/);
  assert.match(net.out(), /2 deleted, 0 LEAKED/);
});

test("safety net names every fixture it could not delete", async () => {
  track("agent", "stuck@x.dev");
  const net = netHarness(() => 500);

  await runSafetyNet("uncaughtException: boom", 1);

  assert.deepEqual(net.exits, [1], "a crash must still exit non-zero");
  assert.match(net.out(), /uncaughtException: boom/, "the crash cause must survive");
  assert.match(net.out(), /LEAKED agent stuck@x\.dev: HTTP 500/);
});

test("safety net exits non-zero even with nothing tracked", async () => {
  const net = netHarness(() => 204);
  await runSafetyNet("SIGTERM", 143);
  assert.equal(net.calls.length, 0);
  assert.deepEqual(net.exits, [143]);
});

test("safety net survives a client that cannot even be constructed", async () => {
  track("agent", "orphan@x.dev");
  const exits: number[] = [];
  const lines: string[] = [];
  setSafetyNetDeps({
    // loadEnv() throws when E2A_API_KEY is unset — the net must still report.
    resolveClient: () => Promise.reject(new Error("No API key found")),
    exit: (code) => exits.push(code),
    write: (line) => lines.push(line),
  });

  await runSafetyNet("SIGINT", 130);

  assert.deepEqual(exits, [130]);
  assert.match(lines.join(""), /teardown net failed: No API key found/);
  assert.match(lines.join(""), /LEAKED agent orphan@x\.dev/);
});

test("a second signal during a sweep reports the remainder instead of hanging", async () => {
  // Regression guard for `process.once`: the first signal consumed the handler,
  // so an impatient second Ctrl-C hit Node's default terminate and killed the
  // sweep mid-flight — losing both the deletes and the leak report.
  track("agent", "slow@x.dev");
  const exits: number[] = [];
  const lines: string[] = [];
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  setSafetyNetDeps({
    resolveClient: async () => {
      await gate;
      return fakeClient(() => 204).client;
    },
    exit: (code) => exits.push(code),
    write: (line) => lines.push(line),
  });

  const first = runSafetyNet("SIGINT", 130);
  // Second signal lands while the first sweep is still awaiting its client.
  await runSafetyNet("SIGINT", 130);

  assert.deepEqual(exits, [130], "the second signal must exit immediately");
  assert.match(lines.join(""), /teardown net interrupted \(SIGINT\)/);
  assert.match(lines.join(""), /LEAKED agent slow@x\.dev/);
  // And it must not have started a competing sweep.
  assert.equal(lines.join("").match(/teardown net fired/g)?.length, 1);

  release();
  await first;
});

test("a second cleanup pass retries what the first one failed", async () => {
  track("agent", "recoverable@x.dev");
  const down = fakeClient(() => 500);
  const r1 = await cleanup(down.client, { attempts: 1, sleep: noSleep });
  assert.equal(r1.failed.length, 1);
  assert.equal(getTracked().length, 1, "a failed fixture stays tracked");

  const up = fakeClient(() => 204);
  const r2 = await cleanup(up.client, { attempts: 1, sleep: noSleep });
  assert.equal(r2.succeeded, 1);
  assert.equal(getTracked().length, 0);
});
