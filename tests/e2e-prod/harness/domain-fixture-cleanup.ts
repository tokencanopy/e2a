import type { ApiClient } from "./client.ts";
import { cleanupFixtures, track, untrack, type CleanupOpts, type CleanupResult } from "./cleanup.ts";
import type { CloudflareDnsRecordRef } from "./cloudflare-dns.ts";

export interface DomainFixture {
  domain: string;
  agent?: string;
	dnsRecords: readonly CloudflareDnsRecordRef[];
}

export interface DomainFixtureCleanupResult extends CleanupResult {
  dnsFailed: Array<{ id: string; reason: string }>;
}

/** Typed HTTP failure from a DNS adapter; transport errors remain retryable. */
export class DnsDeleteError extends Error {
  readonly retryable: boolean;

  constructor(message: string, retryable: boolean) {
    super(message);
    this.name = "DnsDeleteError";
    this.retryable = retryable;
  }
}

/**
 * Tear down API resources before removing the DNS that keeps a verified
 * domain's sender identity valid. The shared cleanup registry owns resource
 * ordering, permanent agent purge, retries, and leak reporting.
 *
 * If any API resource survives — or the API reports the provider-side
 * sending-identity teardown as still pending (sending_teardown:"pending" on
 * the delete response) — preserve every DNS record. Removing DNS from
 * underneath a live provider identity strands it in a pending/failed state
 * and turns a cleanup failure into a delayed SES verification alert.
 */
export async function cleanupDomainFixture(
  client: ApiClient,
  fixture: DomainFixture,
	deleteDnsRecord: (record: CloudflareDnsRecordRef) => Promise<void>,
  opts: CleanupOpts = {},
): Promise<DomainFixtureCleanupResult> {
  const resources = [
    { kind: "domain" as const, id: fixture.domain },
    ...(fixture.agent ? [{ kind: "agent" as const, id: fixture.agent }] : []),
  ];
  const result = await cleanupFixtures(client, resources, opts);
  if (result.failed.length > 0) return { ...result, dnsFailed: [] };
	// cleanupFixtures has removed the API domain from the registry. Re-arm it
	// synchronously before the first DNS await so a signal/crash during DNS
	// teardown still reports the fixture. It is untracked only after every DNS
	// record is confirmed deleted.
	track("domain", fixture.domain);

	// The API delete committed, but the provider identity may still exist
	// (best-effort deprovision failed; async teardown continues). DNS must
	// outlive the identity, so DNS removal requires an EXPLICIT
	// sending_teardown:"confirmed" — every other outcome fails closed. In
	// particular, a retry of an already-deleted domain answers 404 with no
	// teardown state at all: once this process has seen the domain pending,
	// that 404 proves nothing and DNS stays. (A 404 with no pending history is
	// the never-provisioned/already-cleaned case and proceeds — requiring
	// 'confirmed' there would leak DNS for every fixture whose registration
	// never succeeded.) Retained records are reported and the fixture stays
	// tracked for manual follow-up once the identity is verifiably gone.
	const domainDelete = result.completed.find((c) => c.kind === "domain" && c.id === fixture.domain);
	if (!sendingTeardownAllowsDnsRemoval(fixture.domain, domainDelete?.raw ?? "")) {
		return {
			...result,
			dnsFailed: fixture.dnsRecords.map((record) => ({
				id: record.id ?? `${record.type} ${record.name}`,
				reason: "sending-identity teardown not confirmed at the provider; DNS retained until the identity is removed",
			})),
		};
	}

  const attempts = Math.max(1, opts.attempts ?? 3);
  const backoffMs = opts.backoffMs ?? 1_000;
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms)));
  const dnsFailed: DomainFixtureCleanupResult["dnsFailed"] = [];
	for (const record of fixture.dnsRecords) {
		const id = record.id ?? `${record.type} ${record.name}`;
    let reason = "no attempt made";
    for (let attempt = 1; attempt <= attempts; attempt++) {
      try {
				await deleteDnsRecord(record);
        reason = "";
        break;
      } catch (error) {
        reason = error instanceof Error ? error.message : String(error);
        const retryable = !(error instanceof DnsDeleteError) || error.retryable;
        if (!retryable || attempt === attempts) break;
        await sleep(Math.min(backoffMs * attempt, 15_000));
      }
    }
    if (reason !== "") dnsFailed.push({ id, reason });
  }

	if (dnsFailed.length === 0) untrack("domain", fixture.domain);
  return { ...result, dnsFailed };
}

// teardownPendingDomains remembers, per process, every domain whose delete
// reported sending_teardown:"pending". A later bodiless 404 retry carries no
// state and must not be read as safety.
const teardownPendingDomains = new Set<string>();

/** Test-only: clear the per-process pending-teardown memory. */
export function resetSendingTeardownMemory(): void {
	teardownPendingDomains.clear();
}

/**
 * DNS removal is allowed only on an explicit sending_teardown:"confirmed", or
 * on a bodiless response (404/204) for a domain never seen pending — the
 * never-provisioned case. Pending, malformed bodies, unexpected values, and
 * post-pending 404s all fail closed.
 */
function sendingTeardownAllowsDnsRemoval(domain: string, raw: string): boolean {
	if (raw === "") {
		return !teardownPendingDomains.has(domain);
	}
	try {
		const parsed = JSON.parse(raw) as { sending_teardown?: unknown };
		if (parsed.sending_teardown === "confirmed") {
			teardownPendingDomains.delete(domain);
			return true;
		}
		if (parsed.sending_teardown === "pending") {
			teardownPendingDomains.add(domain);
		}
		return false;
	} catch {
		return false;
	}
}
