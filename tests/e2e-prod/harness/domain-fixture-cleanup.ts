import type { ApiClient } from "./client.ts";
import { cleanup, type CleanupOpts, type CleanupResult } from "./cleanup.ts";

/**
 * Tear down API resources before removing the DNS that keeps a verified
 * domain's sender identity valid. The shared cleanup registry owns resource
 * ordering, permanent agent purge, retries, and leak reporting.
 *
 * If any API resource survives, preserve every DNS record. Removing DNS from
 * underneath a live domain strands its provider identity in a pending/failed
 * state and turns a cleanup failure into a delayed SES verification alert.
 */
export async function cleanupDomainFixture(
  client: ApiClient,
  dnsRecordIds: readonly string[],
  deleteDnsRecord: (id: string) => Promise<void>,
  opts: CleanupOpts = {},
): Promise<CleanupResult> {
  const result = await cleanup(client, opts);
  if (result.failed.length > 0) return result;

  for (const id of dnsRecordIds) await deleteDnsRecord(id);
  return result;
}
