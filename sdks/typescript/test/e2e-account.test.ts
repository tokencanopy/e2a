/**
 * Live ergonomic coverage: client.account.* and its sub-resources
 * (client.account.suppressions.*, client.account.apiKeys.*). See
 * test/e2e.test.ts's header for the coverage-gate contract this suite
 * participates in.
 *
 * This file owns: account.get, account.export, account.suppressions.list,
 * account.apiKeys.create, account.apiKeys.list, account.apiKeys.delete.
 *
 * account.delete and account.suppressions.delete are NOT exercised here — see
 * test/coverage/gate.mjs's ALLOWLIST for the justification (destructive /
 * un-creatable-on-staging, respectively; mirrors
 * tests/e2e-prod/suites/19-account.test.ts's own documented-skip stance on
 * deleteAccount).
 */
import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { E2AClient, CreateAPIKeyRequestScopeEnum } from "../src/v1/index.js";
import { walkErgonomicSurface } from "./coverage/introspect.js";
import { recordSurface, recordCovered, flushCoverage } from "./coverage/recorder.js";
import { loadLiveEnv } from "./coverage/helpers.js";

const env = loadLiveEnv();

describe.skipIf(!env)("ts sdk live e2e: account", () => {
  let client: E2AClient;

  beforeAll(() => {
    client = new E2AClient({ apiKey: env!.apiKey, baseUrl: env!.baseUrl });
    recordSurface(walkErgonomicSurface(client));
  });

  afterAll(() => {
    flushCoverage();
  });

  it("get() returns AccountView with identity, plan caps, and usage", async () => {
    const account = await client.account.get();
    expect(account.user.id.length).toBeGreaterThan(0);
    expect(account.user.email).toContain("@");
    expect(["account", "agent"]).toContain(account.scope);
    expect(account.limits.maxAgents).toBeGreaterThan(0);
    expect(account.usage.agents).toBeGreaterThanOrEqual(0);
    recordCovered("account.get");
  });

  // Explicit timeout: the export walks every section of the account's data,
  // so its cost scales with account population and with whatever load the
  // environment is under — in the release pipeline this suite runs right
  // after the conformance battery, against a still-draining staging DB, and
  // vitest's 5s default proved marginal there (2026-08-28: two pipeline
  // failures at exactly the default). Same rule as the grouped-metrics test
  // below: do not "tidy" this back to the default.
  it("export() returns a UserExport with the required sections", async () => {
    const exported = await client.account.export();
    expect(exported.generatedAt).toBeTruthy();
    expect(exported.schemaVersion).toBeTruthy();
    expect(exported.user).toBeTruthy();
    expect(Array.isArray(exported.domains)).toBe(true);
    expect(Array.isArray(exported.agents)).toBe(true);
    expect(Array.isArray(exported.apiKeys)).toBe(true);
    recordCovered("account.export");
  }, 30_000);

  it("suppressions.list returns the envelope shape (possibly empty)", async () => {
    const list = await client.account.suppressions.list().toArray({ limit: 100 });
    expect(Array.isArray(list)).toBe(true);
    for (const s of list) {
      expect(s.address).toContain("@");
      expect(s.source).toBeTruthy();
      expect(s.createdAt).toBeTruthy();
    }
    recordCovered("account.suppressions.list");
  });

  it("apiKeys.create → list shows it → apiKeys.delete (new key only) → gone", async () => {
    const name = `sdk-coverage-${Date.now()}`;
    const created = await client.account.apiKeys.create({ name, scope: CreateAPIKeyRequestScopeEnum.Account });
    expect(created.key.startsWith(created.keyPrefix)).toBe(true);
    expect(created.name).toBe(name);
    expect(created.scope).toBe("account");
    // Guard: never touch the key we authenticate with.
    expect(created.key).not.toBe(env!.apiKey);
    recordCovered("account.apiKeys.create");

    let deleted = false;
    try {
      const list = await client.account.apiKeys.list({ limit: 50 }).toArray({ limit: 200 });
      expect(list.some((k) => k.id === created.id)).toBe(true);
      recordCovered("account.apiKeys.list");

      const del = await client.account.apiKeys.delete(created.id);
      expect(del.deleted).toBe(true);
      expect(del.id).toBe(created.id);
      deleted = true;
      recordCovered("account.apiKeys.delete");

      const after = await client.account.apiKeys.list({ limit: 50 }).toArray({ limit: 200 });
      expect(after.some((k) => k.id === created.id)).toBe(false);
    } finally {
      if (!deleted) await client.account.apiKeys.delete(created.id).catch(() => {});
    }
  });
});
