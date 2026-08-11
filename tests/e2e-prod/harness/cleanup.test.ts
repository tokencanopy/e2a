// Unit tests for the teardown registry. These NEVER touch production: the
// only thing cleanup() calls on its client is `delete(path)`, so a fake stands
// in for ApiClient and no network request is made. Deliberately NOT under
// `suites/` — `npm test` globs `suites/*.test.ts` and runs against live prod.
// Run these with `npm run test:unit` (CI's harness-tests job runs the same glob).
import { test, beforeEach, after } from "node:test";
import assert from "node:assert/strict";
import type { ApiClient, RawResponse } from "./client.ts";
import {
  cleanup,
  track,
  untrack,
  getTracked,
  armLeakReporter,
  disarmLeakReporter,
  setReporterDeps,
  reportLeaks,
} from "./cleanup.ts";

/** A scripted response: an HTTP status, a thrown value, or a status + headers. */
type Outcome = number | Error | unknown | { status: number; headers: Record<string, string>; raw?: string };

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
            ? (outcome as { status: number; headers: Record<string, string>; raw?: string })
            : null;
      // Anything that isn't a status shape is a rejection — including non-Error
      // values, which is the point of the isolation test below.
      if (shaped === null) return Promise.reject(outcome);
      return Promise.resolve({
        status: shaped.status,
        ok: shaped.status < 400,
        headers: shaped.headers,
        body: null,
        raw: shaped.raw ?? "",
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
  // track() arms the process-level leak reporter, whose signal handlers call
  // process.exit(). Disarm between tests so a real Ctrl-C during the run can't
  // fire it; the reporter tests re-arm deliberately through injected seams.
  disarmLeakReporter();
  setReporterDeps();
});

after(() => {
  disarmLeakReporter();
  setReporterDeps();
});

test("cleanup deletes every tracked fixture and empties the registry", async () => {
  track("agent", "a@x.test");
  track("agent", "b@x.test");
  track("domain", "d.x.test");
  const { client, calls } = fakeClient(() => 204);

  const r = await cleanup(client, { sleep: noSleep });

  assert.equal(r.attempted, 3);
  assert.equal(r.succeeded, 3);
  assert.deepEqual(r.failed, []);
  assert.equal(getTracked().length, 0);
  assert.equal(calls.length, 3);
  // Reverse order (newest fixture first) is the existing teardown contract.
  assert.match(calls[0]!, /d\.x\.test/);
  assert.match(calls[0]!, /confirm=DELETE/);
});

test("agent cleanup purges permanently rather than trashing", async () => {
  track("agent", "a@x.test");
  track("domain", "d.x.test");
  const { client, calls } = fakeClient(() => 204);

  await cleanup(client, { sleep: noSleep });

  // Without permanent=true an agent DELETE only moves the row to the trash:
  // messages_deleted is 0, bodies stay stored against usage.storage_bytes,
  // and the tombstone lingers for the 30-day retention window. Fixtures are
  // never restored, so trashing them just accumulates dead agents on the test
  // account faster than they expire — enough of them and the account-scoped
  // metrics queries lose their index and seq-scan the whole table.
  const agentCall = calls.find((c) => c.startsWith("/v1/agents/"));
  assert.ok(agentCall, "expected an agent DELETE");
  assert.match(agentCall, /permanent=true/);

  // Domains have no trash state, so they must NOT grow the parameter.
  const domainCall = calls.find((c) => c.startsWith("/v1/domains/"));
  assert.ok(domainCall, "expected a domain DELETE");
  assert.doesNotMatch(domainCall, /permanent=true/);
});

test("a permanently failing fixture does not block the remaining ones", async () => {
  track("agent", "first@x.test");
  track("agent", "doomed@x.test");
  track("agent", "last@x.test");
  // 400 is non-retryable, so this is one attempt and a hard give-up.
  const { client } = fakeClient((path) => (path.includes("doomed") ? 400 : 204));

  const r = await cleanup(client, { sleep: noSleep });

  assert.equal(r.attempted, 3);
  assert.equal(r.succeeded, 2, "the other two fixtures must still be deleted");
  assert.equal(r.failed.length, 1);
  assert.equal(r.failed[0]!.id, "doomed@x.test");
  // Only the failure stays tracked, so the safety net can report it as leaked.
  assert.deepEqual(
    getTracked().map((t) => t.id),
    ["doomed@x.test"],
  );
});

test("a thrown request does not block the remaining fixtures", async () => {
  track("agent", "first@x.test");
  track("agent", "boom@x.test");
  track("agent", "last@x.test");
  const { client } = fakeClient((path) =>
    path.includes("boom") ? new Error("socket hang up") : 204,
  );

  const r = await cleanup(client, { attempts: 2, sleep: noSleep });

  assert.equal(r.succeeded, 2);
  assert.equal(r.failed.length, 1);
  assert.match(r.failed[0]!.reason, /socket hang up/);
});

test("a transient 429 is retried and the fixture is still deleted", async () => {
  track("agent", "ratelimited@x.test");
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
  track("agent", "flaky@x.test");
  const flaky = fakeClient((_p, attempt) => (attempt < 3 ? 503 : 200));
  const r1 = await cleanup(flaky.client, { attempts: 3, sleep: noSleep });
  assert.equal(r1.succeeded, 1);
  assert.equal(flaky.calls.length, 3);

  drainTracked();
  track("agent", "hard@x.test");
  const hard = fakeClient(() => 422);
  const r2 = await cleanup(hard.client, { attempts: 3, sleep: noSleep });
  assert.equal(r2.failed.length, 1);
  assert.equal(hard.calls.length, 1, "422 must not consume the retry budget");
});

test("retries are bounded and the give-up reason names the last status", async () => {
  track("agent", "downforever@x.test");
  const { client, calls } = fakeClient(() => 500);

  const r = await cleanup(client, { attempts: 3, sleep: noSleep });

  assert.equal(calls.length, 3);
  assert.equal(r.succeeded, 0);
  assert.match(r.failed[0]!.reason, /HTTP 500/);
});

test("backoff grows per attempt", async () => {
  track("agent", "slow@x.test");
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

test("404 counts as success but 403 remains a tracked cleanup failure", async () => {
  track("agent", "gone@x.test");
  track("agent", "notours@x.test");
  const { client } = fakeClient((path) => (path.includes("gone") ? 404 : 403));

  const r = await cleanup(client, { sleep: noSleep });

  assert.equal(r.succeeded, 1);
  assert.equal(r.failed.length, 1);
  assert.equal(r.failed[0]!.id, "notours@x.test");
  assert.deepEqual(getTracked(), [{ kind: "agent", id: "notours@x.test" }]);
});

test("only transient permanent-delete conflict codes are retried", async () => {
  track("agent", "busy@x.test");
  const { client, calls } = fakeClient((_path, attempt) =>
    attempt < 3
      ? { status: 409, headers: {}, raw: JSON.stringify({ error: { code: attempt === 1 ? "send_in_progress" : "purge_in_progress" } }) }
      : 204,
  );

  const r = await cleanup(client, { attempts: 3, sleep: noSleep });

  assert.equal(r.succeeded, 1);
  assert.deepEqual(r.failed, []);
  assert.equal(calls.length, 3);

  drainTracked();
  track("agent", "conflict@x.test");
  const hard = fakeClient(() => ({ status: 409, headers: {}, raw: JSON.stringify({ error: { code: "address_in_trash" } }) }));
  const hardResult = await cleanup(hard.client, { attempts: 3, sleep: noSleep });
  assert.equal(hardResult.failed.length, 1);
  assert.equal(hard.calls.length, 1, "unrelated 409 conflicts must not be retried");
});

test("default conflict budget outlasts the normal three-attempt window", async () => {
	track("agent", "long-send@x.test");
	const { client, calls } = fakeClient((_path, attempt) =>
		attempt < 5
			? { status: 409, headers: {}, raw: JSON.stringify({ error: { code: "send_in_progress" } }) }
			: 204,
	);

	const r = await cleanup(client, { sleep: noSleep });

	assert.equal(r.succeeded, 1);
	assert.equal(calls.length, 5, "known 409 must not inherit the three-attempt transport budget");
});

test("default conflict waits out the full ten-minute send lease", async () => {
	track("agent", "lease-boundary@x.test");
	const waits: number[] = [];
	const { client, calls } = fakeClient((_path, attempt) =>
		attempt < 49
			? { status: 409, headers: {}, raw: JSON.stringify({ error: { code: "send_in_progress" } }) }
			: 204,
	);
	const result = await cleanup(client, { sleep: async (ms) => { waits.push(ms); } });
	assert.equal(result.succeeded, 1);
	assert.equal(calls.length, 49);
	assert.ok(waits.reduce((sum, ms) => sum + ms, 0) >= 600_000, "retry waits must exceed the 600s claim lease");
});

test("tracking the same identity twice registers it once", async () => {
  // Over-tracking is the safe direction — suites now track a requested identity
  // before the create, which can duplicate an id tracked elsewhere — but a
  // duplicate registry entry makes one 5xx report a LEAK for a fixture whose
  // twin deleted fine. track() dedupes so the report stays truthful.
  track("agent", "dupe@x.test");
  track("agent", "dupe@x.test");
  assert.equal(getTracked().length, 1, "duplicate track must not double-register");
  const { client, calls } = fakeClient(() => 204);

  const r = await cleanup(client, { sleep: noSleep });

  assert.equal(r.attempted, 1);
  assert.equal(r.succeeded, 1);
  assert.equal(calls.length, 1, "one fixture, one DELETE");
  assert.equal(getTracked().length, 0);
});

test("kind is part of identity — same id under two kinds stays two fixtures", async () => {
  track("agent", "x.test");
  track("domain", "x.test");
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
  track("agent", "first@x.test");
  track("agent", "nonerror@x.test");
  track("agent", "last@x.test");
  const { client } = fakeClient((path) => (path.includes("nonerror") ? null : 204));

  const r = await cleanup(client, { attempts: 1, sleep: noSleep });

  assert.equal(r.succeeded, 2, "both healthy fixtures must still be deleted");
  assert.equal(r.failed.length, 1);
  assert.equal(r.failed[0]!.id, "nonerror@x.test");
  assert.deepEqual(
    getTracked().map((t) => t.id),
    ["nonerror@x.test"],
  );
});

test("a 429 Retry-After longer than the backoff floor is honored, and capped", async () => {
  // internal/ratelimit rounds Retry-After up to a whole second of the remaining
  // sliding window. Retrying on a sub-second backoff is guaranteed to 429 again
  // and just adds load to an account that is already throttled.
  track("agent", "throttled@x.test");
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
  track("agent", "throttled@x.test");
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

// --- abnormal-exit leak reporter -----------------------------------------
//
// The reporter is intentionally synchronous and delete-free (see cleanup.ts
// for why an async delete-on-crash net is a trap under node:test's per-suite
// process isolation). These tests capture its stderr through an injected seam,
// so nothing is written to a real fd and the process is never killed.

function captureReporter() {
  const lines: string[] = [];
  const exits: number[] = [];
  setReporterDeps({ write: (l) => lines.push(l), exit: (c) => exits.push(c) });
  return { out: () => lines.join(""), exits };
}

test("armLeakReporter is idempotent — one listener per signal", () => {
  disarmLeakReporter();
  armLeakReporter();
  armLeakReporter();
  armLeakReporter();
  assert.equal(process.listenerCount("SIGINT"), 1);
  assert.equal(process.listenerCount("SIGTERM"), 1);
  assert.equal(process.listenerCount("SIGHUP"), 1);
  assert.equal(process.listenerCount("exit"), 1);
  disarmLeakReporter();
  assert.equal(process.listenerCount("SIGINT"), 0);
  assert.equal(process.listenerCount("SIGHUP"), 0);
  assert.equal(process.listenerCount("exit"), 0);
});

test("reportLeaks names every still-tracked fixture, once", () => {
  track("agent", "orphan@x.test");
  track("domain", "orphan.x.test");
  const cap = captureReporter();

  reportLeaks("exit");
  reportLeaks("exit"); // idempotent: a signal's exit re-runs the exit handler

  assert.match(cap.out(), /2 fixture\(s\) still tracked at exit/);
  assert.match(cap.out(), /LEAKED agent orphan@x\.test — delete manually/);
  assert.match(cap.out(), /LEAKED domain orphan\.x\.test — delete manually/);
  // Printed exactly once despite two calls.
  assert.equal(cap.out().match(/still tracked at exit/g)?.length, 1);
});

test("reportLeaks is silent when teardown emptied the registry", () => {
  track("agent", "cleaned@x.test");
  untrack("agent", "cleaned@x.test");
  const cap = captureReporter();

  reportLeaks("exit");

  assert.equal(cap.out(), "", "a clean run must print nothing on exit");
});

test("the SIGINT handler translates the signal into a non-zero exit", () => {
  disarmLeakReporter();
  const cap = captureReporter();
  armLeakReporter();

  // Fire the actual registered handler, not an internal — proves the signal is
  // wired to exit (which is what runs the exit-time reportLeaks).
  process.emit("SIGINT");
  process.emit("SIGTERM");
  process.emit("SIGHUP");

  assert.deepEqual(cap.exits, [130, 143, 129]);
  disarmLeakReporter();
});

test("disarm removes only our handlers, not a foreign one", () => {
  disarmLeakReporter();
  const foreign = () => {};
  process.on("SIGINT", foreign);
  armLeakReporter();
  assert.equal(process.listenerCount("SIGINT"), 2);

  disarmLeakReporter();

  // The foreign listener (a stand-in for node:test's own) must survive.
  assert.equal(process.listenerCount("SIGINT"), 1);
  assert.deepEqual(process.listeners("SIGINT"), [foreign]);
  process.off("SIGINT", foreign);
});

test("a second cleanup pass retries what the first one failed", async () => {
  track("agent", "recoverable@x.test");
  const down = fakeClient(() => 500);
  const r1 = await cleanup(down.client, { attempts: 1, sleep: noSleep });
  assert.equal(r1.failed.length, 1);
  assert.equal(getTracked().length, 1, "a failed fixture stays tracked");

  const up = fakeClient(() => 204);
  const r2 = await cleanup(up.client, { attempts: 1, sleep: noSleep });
  assert.equal(r2.succeeded, 1);
  assert.equal(getTracked().length, 0);
});
