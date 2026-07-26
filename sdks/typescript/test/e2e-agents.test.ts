/**
 * Live ergonomic coverage: the rest of client.agents.* not already exercised
 * by test/e2e.test.ts (create/delete/list). See that file's header for the
 * coverage-gate contract this suite participates in.
 *
 * This file owns: agents.get, agents.update, agents.getProtection,
 * agents.replaceProtection, agents.restore, agents.test,
 * agents.listSuppressions, agents.createSuppression, agents.deleteSuppression.
 *
 * One throwaway agent per describe block, cleaned up in afterAll.
 */
import { describe, it, expect, beforeAll, afterAll } from "vitest";
import {
  E2AClient,
  ProtectionGateRequestPolicyEnum,
  ProtectionGateRequestActionEnum,
} from "../src/v1/index.js";
import { walkErgonomicSurface } from "./coverage/introspect.js";
import { recordSurface, recordCovered, flushCoverage } from "./coverage/recorder.js";
import { loadLiveEnv, uniqueSlug } from "./coverage/helpers.js";

const env = loadLiveEnv();

describe.skipIf(!env)("ts sdk live e2e: agents", () => {
  let client: E2AClient;
  const cleanup: Array<() => Promise<unknown>> = [];

  beforeAll(() => {
    client = new E2AClient({ apiKey: env!.apiKey, baseUrl: env!.baseUrl });
    recordSurface(walkErgonomicSurface(client));
  });

  afterAll(async () => {
    for (const fn of cleanup.splice(0)) {
      await fn().catch(() => {});
    }
    flushCoverage();
  });

  it("get / update / getProtection / replaceProtection round-trip on a fresh agent", async () => {
    const email = `${uniqueSlug("agtcov")}@${env!.sharedDomain}`;
    const created = await client.agents.create({ email, name: "coverage agents" });
    expect(created.email).toBe(email);
    cleanup.push(() => client.agents.delete(email));

    const got = await client.agents.get(email);
    expect(got.email).toBe(email);
    expect(got.name).toBe("coverage agents");
    expect(got.domain).toBe(env!.sharedDomain);
    recordCovered("agents.get");

    const newName = `agents coverage renamed ${Date.now()}`;
    const updated = await client.agents.update(email, { name: newName });
    expect(updated.email).toBe(email);
    expect(updated.name).toBe(newName);
    recordCovered("agents.update");

    // Confirm the rename actually persisted (not just echoed back).
    const reread = await client.agents.get(email);
    expect(reread.name).toBe(newName);

    const protection = await client.agents.getProtection(email);
    expect(protection.inbound.gate.policy).toBeTruthy();
    expect(protection.outbound.gate.policy).toBeTruthy();
    expect(protection.holds.onExpiry).toBeTruthy();
    recordCovered("agents.getProtection");

    // Wholesale replace (PUT semantics): flip the outbound gate to
    // allowlist+review with an explicit empty allowlist, then read it back.
    const replaced = await client.agents.replaceProtection(email, {
      inbound: { gate: {}, scan: {} },
      outbound: {
        gate: {
          policy: ProtectionGateRequestPolicyEnum.Allowlist,
          action: ProtectionGateRequestActionEnum.Review,
          allowlist: [],
        },
        scan: {},
      },
      holds: {},
    });
    expect(replaced.outbound.gate.policy).toBe("allowlist");
    expect(replaced.outbound.gate.action).toBe("review");
    recordCovered("agents.replaceProtection");

    const rereadProtection = await client.agents.getProtection(email);
    expect(rereadProtection.outbound.gate.policy).toBe("allowlist");
  });

  it("delete → restore brings a trashed agent back live", async () => {
    const email = `${uniqueSlug("agtrestore")}@${env!.sharedDomain}`;
    const created = await client.agents.create({ email, name: "coverage restore" });
    expect(created.email).toBe(email);
    cleanup.push(() => client.agents.delete(email, { permanent: true }));

    const del = await client.agents.delete(email);
    expect(del.deleted).toBe(true);
    expect(del.email).toBe(email);

    const restored = await client.agents.restore(email);
    expect(restored.email).toBe(email);
    expect(restored.deletedAt).toBeUndefined();
    recordCovered("agents.restore");

    // Restored agent is live again — a fresh get proves it's not still trashed.
    const got = await client.agents.get(email);
    expect(got.email).toBe(email);
  });

  it("test() sends a real self-test message through a fresh agent", async () => {
    const email = `${uniqueSlug("agttest")}@${env!.sharedDomain}`;
    await client.agents.create({ email, name: "coverage test()" });
    cleanup.push(() => client.agents.delete(email));

    const result = await client.agents.test(email);
    expect(result.messageId).toBeTruthy();
    expect(["sent", "accepted"]).toContain(result.status);
    recordCovered("agents.test");
  });

  it("createSuppression → listSuppressions → deleteSuppression round-trip", async () => {
    const email = `${uniqueSlug("agtsupp")}@${env!.sharedDomain}`;
    await client.agents.create({ email, name: "coverage suppressions" });
    cleanup.push(() => client.agents.delete(email));

    const address = `blocked-${uniqueSlug("addr")}@example.com`;
    const created = await client.agents.createSuppression(email, { address, reason: "coverage gate fixture" });
    expect(created.address).toBe(address);
    expect(created.agentEmail).toBe(email);
    recordCovered("agents.createSuppression");

    const list = await client.agents.listSuppressions(email).toArray({ limit: 50 });
    const mine = list.find((s) => s.address === address);
    expect(mine, `created suppression ${address} must appear in listSuppressions`).toBeTruthy();
    recordCovered("agents.listSuppressions");

    const deleted = await client.agents.deleteSuppression(email, address);
    expect(deleted.deleted).toBe(true);
    expect(deleted.address).toBe(address);
    recordCovered("agents.deleteSuppression");

    const after = await client.agents.listSuppressions(email).toArray({ limit: 50 });
    expect(after.some((s) => s.address === address)).toBe(false);
  });
});
