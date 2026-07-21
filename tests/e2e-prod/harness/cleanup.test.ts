// Unit tests for the teardown registry. These NEVER touch production: the
// only thing cleanup() calls on its client is `delete(path)`, so a fake stands
// in for ApiClient and no network request is made. Deliberately NOT under
// `suites/` — `npm test` globs `suites/*.test.ts` and runs against live prod.
// Run these with `npm run test:harness`.
import { test, beforeEach } from "node:test";
import assert from "node:assert/strict";
import type { ApiClient, RawResponse } from "./client.ts";
import { cleanup, track, untrack, getTracked } from "./cleanup.ts";

/** A DELETE recorder whose per-path responses are scripted by the test. */
function fakeClient(script: (path: string, attempt: number) => number | Error): {
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
      if (outcome instanceof Error) return Promise.reject(outcome);
      return Promise.resolve({
        status: outcome,
        ok: outcome < 400,
        headers: {},
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

test("tracking the same identity twice is idempotent under teardown", async () => {
  // Over-tracking is the safe direction: suites now track a requested identity
  // before the create, which can duplicate an id already tracked elsewhere.
  track("agent", "dupe@x.dev");
  track("agent", "dupe@x.dev");
  const { client, calls } = fakeClient((_p, attempt) => (attempt === 1 ? 204 : 404));

  const r = await cleanup(client, { sleep: noSleep });

  assert.equal(r.succeeded, 2);
  assert.equal(calls.length, 2);
  assert.equal(getTracked().length, 0, "both registry entries must be cleared");
});

test("cleanup on an empty registry is a no-op", async () => {
  const { client, calls } = fakeClient(() => 204);
  const r = await cleanup(client, { sleep: noSleep });
  assert.deepEqual(r, { attempted: 0, succeeded: 0, failed: [] });
  assert.equal(calls.length, 0);
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
