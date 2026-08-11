import { test, beforeEach, after } from "node:test";
import assert from "node:assert/strict";
import type { ApiClient, RawResponse } from "./client.ts";
import { cleanupDomainFixture } from "./domain-fixture-cleanup.ts";
import { disarmLeakReporter, getTracked, setReporterDeps, track, untrack } from "./cleanup.ts";

function fakeClient(statusFor: (path: string) => number, calls: string[]): ApiClient {
  return {
    delete(path: string): Promise<RawResponse> {
      calls.push(path);
      const status = statusFor(path);
      return Promise.resolve({
        status,
        ok: status < 400,
        headers: {},
        body: null,
        raw: "",
        latencyMs: 0,
      });
    },
  } as unknown as ApiClient;
}

function drainTracked(): void {
  for (const fixture of [...getTracked()]) untrack(fixture.kind, fixture.id);
}

beforeEach(() => {
  drainTracked();
  disarmLeakReporter();
  setReporterDeps();
});

after(() => {
  drainTracked();
  disarmLeakReporter();
  setReporterDeps();
});

test("verified-domain cleanup purges the agent and domain before removing DNS", async () => {
  const calls: string[] = [];
  const client = fakeClient(() => 204, calls);
  track("domain", "run.example.test");
  track("agent", "bot@run.example.test");

  const result = await cleanupDomainFixture(
    client,
    ["dns-ownership", "dns-mail-from"],
    async (id) => {
      calls.push(`dns:${id}`);
    },
    { sleep: async () => {} },
  );

  assert.deepEqual(result.failed, []);
  assert.match(calls[0]!, /^\/v1\/agents\//);
  assert.match(calls[0]!, /permanent=true/);
  assert.match(calls[1]!, /^\/v1\/domains\//);
  assert.deepEqual(calls.slice(2), ["dns:dns-ownership", "dns:dns-mail-from"]);
});

test("verified-domain cleanup preserves DNS when API resource teardown fails", async () => {
  const calls: string[] = [];
  const client = fakeClient((path) => (path.startsWith("/v1/domains/") ? 500 : 204), calls);
  track("domain", "run.example.test");
  track("agent", "bot@run.example.test");

  const result = await cleanupDomainFixture(
    client,
    ["dns-ownership", "dns-mail-from"],
    async (id) => {
      calls.push(`dns:${id}`);
    },
    { attempts: 1, sleep: async () => {} },
  );

  assert.equal(result.failed.length, 1);
  assert.equal(result.failed[0]!.kind, "domain");
  assert.equal(calls.some((call) => call.startsWith("dns:")), false, "DNS must remain while the identity is stranded");
  assert.deepEqual(getTracked(), [{ kind: "domain", id: "run.example.test" }]);
});
