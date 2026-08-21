/**
 * Live ergonomic coverage: client.domains.*. See test/e2e.test.ts's header
 * for the coverage-gate contract this suite participates in.
 *
 * This file owns: domains.create, domains.get, domains.list, domains.verify,
 * domains.delete.
 *
 * .example.com is reserved by RFC 2606 specifically for documentation/testing
 * — safe to register-then-fail-to-verify without colliding with real DNS
 * (mirrors tests/e2e-prod/suites/10-domains.test.ts's fakeDomain()).
 */
import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { E2AClient } from "../src/v1/index.js";
import { walkErgonomicSurface } from "./coverage/introspect.js";
import { recordSurface, recordCovered, flushCoverage } from "./coverage/recorder.js";
import { loadLiveEnv, runId } from "./coverage/helpers.js";

const env = loadLiveEnv();

function fakeDomain(label: string): string {
  return `e2e-sdk-${runId()}-${label}-${Math.random().toString(36).slice(2, 8)}.example.com`;
}

describe.skipIf(!env)("ts sdk live e2e: domains", () => {
  let client: E2AClient;

  beforeAll(() => {
    client = new E2AClient({ apiKey: env!.apiKey, baseUrl: env!.baseUrl });
    recordSurface(walkErgonomicSurface(client));
  });

  afterAll(() => {
    flushCoverage();
  });

  it(
    "create → get → list → verify(false, real DNS) → delete round-trip",
    async () => {
      const domain = fakeDomain("cov");
      const created = await client.domains.create({ domain });
      expect(created.domain).toBe(domain);
      expect(Array.isArray(created.dnsRecords)).toBe(true);
      expect(created.dnsRecords.length).toBeGreaterThan(0);
      recordCovered("domains.create");

      let cleaned = false;
      try {
        const got = await client.domains.get(domain);
        expect(got.domain).toBe(domain);
        expect(got.verified).toBe(false);
        recordCovered("domains.get");

        const list = await client.domains.list({ limit: 50 }).toArray({ limit: 200 });
        expect(list.some((d) => d.domain === domain)).toBe(true);
        recordCovered("domains.list");

        // Our fake .example.com domain has no matching DNS TXT/MX records, so
        // this MUST report verified:false — branching on the body, not the
        // status, per the documented contract (always 200).
        const verified = await client.domains.verify(domain);
        expect(verified.domain).toBe(domain);
        expect(verified.verified).toBe(false);
        recordCovered("domains.verify");

        const deleted = await client.domains.delete(domain);
        expect(deleted.deleted).toBe(true);
        expect(deleted.domain).toBe(domain);
        // With a sending-identity provider configured, the receipt starts
        // "pending" and is only "confirmed" when the in-request best-effort
        // deprovision proves provider absence. Staging's provider IAM policy
        // denies identity calls outside its own namespace, so this throwaway
        // .example.com domain can never reach "confirmed" there — both states
        // are contract-valid receipts for a domain that never verified.
        expect(["pending", "confirmed"]).toContain(deleted.sendingTeardown);
        cleaned = true;
        recordCovered("domains.delete");

        const afterList = await client.domains.list({ limit: 50 }).toArray({ limit: 200 });
        expect(afterList.some((d) => d.domain === domain)).toBe(false);
      } finally {
        if (!cleaned) await client.domains.delete(domain).catch(() => {});
      }
    },
    30_000,
  );
});
