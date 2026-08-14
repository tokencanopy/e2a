import { test, beforeEach, after } from "node:test";
import assert from "node:assert/strict";
import type { ApiClient, RawResponse } from "./client.ts";
import { cleanupDomainFixture, DnsDeleteError } from "./domain-fixture-cleanup.ts";
import { disarmLeakReporter, getTracked, setReporterDeps, track, untrack } from "./cleanup.ts";

function fakeClient(statusFor: (path: string) => number | { status: number; raw: string }, calls: string[]): ApiClient {
  return {
    delete(path: string): Promise<RawResponse> {
      calls.push(path);
      const r = statusFor(path);
      const { status, raw } = typeof r === "number" ? { status: r, raw: "" } : r;
      return Promise.resolve({
        status,
        ok: status < 400,
        headers: {},
        body: null,
        raw,
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
    {
      domain: "run.example.test",
      agent: "bot@run.example.test",
      dnsRecords: [
        { id: "dns-ownership", type: "TXT", name: "_verify.run.example.test" },
        { id: "dns-mail-from", type: "MX", name: "bounce.run.example.test" },
      ],
    },
		async (record) => {
			calls.push(`dns:${record.id}`);
    },
    { sleep: async () => {} },
  );

  assert.deepEqual(result.failed, []);
  assert.deepEqual(result.dnsFailed, []);
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
    {
      domain: "run.example.test",
      agent: "bot@run.example.test",
      dnsRecords: [
        { id: "dns-ownership", type: "TXT", name: "_verify.run.example.test" },
        { id: "dns-mail-from", type: "MX", name: "bounce.run.example.test" },
      ],
    },
		async (record) => {
			calls.push(`dns:${record.id}`);
    },
    { attempts: 1, sleep: async () => {} },
  );

  assert.equal(result.failed.length, 1);
  assert.equal(result.failed[0]!.kind, "domain");
  assert.equal(calls.some((call) => call.startsWith("dns:")), false, "DNS must remain while the identity is stranded");
  assert.deepEqual(getTracked(), [{ kind: "domain", id: "run.example.test" }]);
});

test("pending sending-identity teardown preserves DNS and keeps the fixture tracked", async () => {
	// The API delete can succeed while SES teardown continues asynchronously
	// (sending_teardown:"pending" — provider outage/throttle at delete time).
	// Removing DNS then would strand the still-live identity in a failing
	// state: the exact failure this module exists to prevent. DNS must be
	// preserved and reported until the identity is actually gone.
	const calls: string[] = [];
	const client = fakeClient(
		(path) =>
			path.startsWith("/v1/domains/")
				? { status: 200, raw: JSON.stringify({ deleted: true, domain: "run.example.test", sending_teardown: "pending" }) }
				: 204,
		calls,
	);
	track("domain", "run.example.test");
	track("agent", "bot@run.example.test");

	const result = await cleanupDomainFixture(
		client,
		{
			domain: "run.example.test",
			agent: "bot@run.example.test",
			dnsRecords: [
				{ id: "dns-ownership", type: "TXT", name: "_verify.run.example.test" },
				{ id: "dns-mail-from", type: "MX", name: "bounce.run.example.test" },
			],
		},
		async (record) => {
			calls.push(`dns:${record.id}`);
		},
		{ sleep: async () => {} },
	);

	assert.deepEqual(result.failed, [], "the API delete itself succeeded");
	assert.equal(calls.some((call) => call.startsWith("dns:")), false, "DNS must remain while provider teardown is pending");
	assert.equal(result.dnsFailed.length, 2, "retained records are reported, not silently kept");
	assert.match(result.dnsFailed[0]!.reason, /teardown.*pending/i);
	assert.deepEqual(getTracked(), [{ kind: "domain", id: "run.example.test" }], "the fixture stays tracked for follow-up");
});

test("confirmed sending-identity teardown proceeds to DNS removal", async () => {
	const calls: string[] = [];
	const client = fakeClient(
		(path) =>
			path.startsWith("/v1/domains/")
				? { status: 200, raw: JSON.stringify({ deleted: true, domain: "run.example.test", sending_teardown: "confirmed" }) }
				: 204,
		calls,
	);
	track("domain", "run.example.test");

	const result = await cleanupDomainFixture(
		client,
		{ domain: "run.example.test", dnsRecords: [{ id: "dns-ownership", type: "TXT", name: "_verify.run.example.test" }] },
		async (record) => {
			calls.push(`dns:${record.id}`);
		},
		{ sleep: async () => {} },
	);

	assert.deepEqual(result.failed, []);
	assert.deepEqual(result.dnsFailed, []);
	assert.deepEqual(calls.slice(-1), ["dns:dns-ownership"]);
	assert.deepEqual(getTracked(), []);
});

test("fixture-scoped cleanup cannot detach an older fixture from its DNS", async () => {
  const calls: string[] = [];
  const client = fakeClient(() => 204, calls);
  track("domain", "old.example.test");
  track("domain", "current.example.test");

  const result = await cleanupDomainFixture(
    client,
    { domain: "current.example.test", dnsRecords: [{ id: "dns-current", type: "TXT", name: "current.example.test" }] },
		async (record) => {
			calls.push(`dns:${record.id}`);
    },
    { sleep: async () => {} },
  );

  assert.deepEqual(result.failed, []);
  assert.deepEqual(getTracked(), [{ kind: "domain", id: "old.example.test" }]);
  assert.equal(calls.some((call) => call.includes("old.example.test")), false);
  assert.deepEqual(calls.slice(-1), ["dns:dns-current"]);
});

test("DNS deletes retry independently and aggregate failures", async () => {
  const calls: string[] = [];
  const attempts = new Map<string, number>();
  const client = fakeClient(() => 204, calls);
  track("domain", "run.example.test");

  const result = await cleanupDomainFixture(
    client,
		{
			domain: "run.example.test",
			dnsRecords: ["transport", "rate", "forbidden", "healthy"].map((id) => ({ id, type: "TXT", name: `${id}.run.example.test` })),
		},
		async (record) => {
			const id = record.id!;
			calls.push(`dns:${id}`);
      const n = (attempts.get(id) ?? 0) + 1;
      attempts.set(id, n);
      if (id === "transport") throw new Error("socket reset");
      if (id === "rate" && n === 1) throw new DnsDeleteError("HTTP 429", true);
      if (id === "forbidden") throw new DnsDeleteError("HTTP 403", false);
    },
    { attempts: 2, sleep: async () => {} },
  );

  assert.deepEqual(result.failed, []);
  assert.deepEqual(
    result.dnsFailed.map((failure) => failure.id),
    ["transport", "forbidden"],
  );
  assert.equal(attempts.get("transport"), 2, "transport failures are retryable");
  assert.equal(attempts.get("rate"), 2, "explicit transient status is retryable");
  assert.equal(attempts.get("forbidden"), 1, "non-retryable status stops immediately");
  assert.equal(attempts.get("healthy"), 1, "later DNS records are still attempted");
	assert.deepEqual(getTracked(), [{ kind: "domain", id: "run.example.test" }], "DNS failure keeps an abnormal-exit marker armed");
});
