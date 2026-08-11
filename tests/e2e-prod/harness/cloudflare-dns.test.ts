import { test } from "node:test";
import assert from "node:assert/strict";
import { CloudflareDnsClient, type CloudflareDnsRecordRef } from "./cloudflare-dns.ts";
import { DnsDeleteError } from "./domain-fixture-cleanup.ts";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), {
  status,
  headers: { "content-type": "application/json" },
});

test("tracks a deterministic descriptor before an ambiguous create failure", async () => {
  const tracked: CloudflareDnsRecordRef[] = [];
  const client = new CloudflareDnsClient("zone", "token", async () => {
    throw new Error("connection reset after commit");
  });
  await assert.rejects(
    client.create({ type: "TXT", name: "_verify.fixture.test", content: "token" }, tracked, "temporary"),
    /connection reset/,
  );
  assert.deepEqual(tracked, [{ type: "TXT", name: "_verify.fixture.test", content: "token", comment: "temporary" }]);
});

test("an unresolved descriptor is listed by exact type/name then deleted", async () => {
  const calls: Array<{ url: string; method?: string }> = [];
  const client = new CloudflareDnsClient("zone", "token", async (input, init) => {
    const url = String(input);
    calls.push({ url, method: init?.method });
    if (url.includes("dns_records?")) return json({ success: true, result: [{ id: "record-1" }] });
    return json({ success: true, result: { id: "record-1" } });
  });
  await client.delete({ type: "MX", name: "fixture.test" });
  assert.match(calls[0]!.url, /type=MX/);
  assert.match(calls[0]!.url, /name=fixture/);
  assert.deepEqual(calls.map((call) => call.method), [undefined, "DELETE"]);
});

test("delete rejects a 2xx Cloudflare failure envelope", async () => {
  const client = new CloudflareDnsClient("zone", "token", async () => json({ success: false, errors: [{ code: 1000 }] }));
  await assert.rejects(
    client.delete({ type: "TXT", name: "fixture.test", id: "record-1" }),
    (error: unknown) => error instanceof DnsDeleteError && !error.retryable,
  );
});

test("definitive create rejection disarms descriptor and preserves existing record", async () => {
	const tracked: CloudflareDnsRecordRef[] = [];
	const calls: string[] = [];
	const client = new CloudflareDnsClient("zone", "token", async (input) => {
		calls.push(String(input));
		return json({ success: false, errors: [{ code: 81057, message: "record already exists" }] }, 400);
	});
	await assert.rejects(
		client.create({ type: "TXT", name: "existing.fixture.test", content: "new-value" }, tracked, "temporary"),
		/HTTP 400/,
	);
	assert.deepEqual(tracked, []);
	assert.equal(calls.length, 1, "cleanup has no descriptor with which to delete the pre-existing record");
});

test("ambiguous create lookup retries when Cloudflare has not converged", async () => {
	const client = new CloudflareDnsClient("zone", "token", async () => json({ success: true, result: [] }));
	await assert.rejects(
		client.delete({ type: "TXT", name: "lagging.fixture.test", content: "token", comment: "temporary" }),
		(error: unknown) => error instanceof DnsDeleteError && error.retryable,
	);
});

test("ambiguous create lookup filters by run comment", async () => {
	const deleted: string[] = [];
	const client = new CloudflareDnsClient("zone", "token", async (input, init) => {
		const url = String(input);
		if (url.includes("dns_records?")) {
			return json({ success: true, result: [
				{ id: "foreign", content: "old", comment: "someone else" },
				{ id: "ours", content: "token", comment: "temporary" },
			] });
		}
		if (init?.method === "DELETE") deleted.push(url.split("/").at(-1)!);
		return json({ success: true, result: {} });
	});
	await client.delete({ type: "TXT", name: "shared.fixture.test", content: "token", comment: "temporary" });
	assert.deepEqual(deleted, ["ours"]);
});

test("ambiguous create lookup tolerates canonicalized content when the run comment matches", async () => {
	const deleted: string[] = [];
	const client = new CloudflareDnsClient("zone", "token", async (input, init) => {
		const url = String(input);
		if (url.includes("dns_records?")) {
			return json({ success: true, result: [
				{ id: "foreign", content: "10 old.example.test", comment: "another run" },
				{ id: "ours", content: "10 canonicalized.example.test.", comment: "run-123" },
			] });
		}
		if (init?.method === "DELETE") deleted.push(url.split("/").at(-1)!);
		return json({ success: true, result: {} });
	});
	await client.delete({
		type: "MX",
		name: "bounce.fixture.test",
		content: "10 canonicalized.example.test",
		comment: "run-123",
	});
	assert.deepEqual(deleted, ["ours"]);
});
