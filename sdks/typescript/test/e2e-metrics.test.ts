/**
 * Live ergonomic coverage: the beta delivery-metrics surface. See
 * test/e2e.test.ts's header for the coverage-gate contract this suite
 * participates in.
 *
 * This file owns: messages.getMetrics, account.metrics.
 *
 * Both are asserted on SHAPE and INVARIANTS rather than on specific counts:
 * these run against a shared staging deployment whose traffic is not ours to
 * control, so any assertion on a particular number would be flaky by
 * construction. The invariants below are the contract's actual promises and
 * hold at any traffic level, including zero.
 *
 * This suite deliberately creates NOTHING. Metrics are read-only, and the
 * staging accounts these run against are Free-plan (3 agents). An earlier
 * draft created its own agent for isolation and pushed e2e-agents.test.ts over
 * the cap — two failures in a file this one never touches. Reading the env's
 * existing agent costs no quota and cannot destabilise a sibling suite.
 */
import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { E2AClient } from "../src/v1/index.js";
import { walkErgonomicSurface } from "./coverage/introspect.js";
import {
  recordSurface,
  recordCovered,
  flushCoverage,
} from "./coverage/recorder.js";
import { loadLiveEnv } from "./coverage/helpers.js";

const env = loadLiveEnv();

describe.skipIf(!env)("ts sdk live e2e: metrics (beta)", () => {
  let client: E2AClient;
  beforeAll(() => {
    client = new E2AClient({ apiKey: env!.apiKey, baseUrl: env!.baseUrl });
    recordSurface(walkErgonomicSurface(client));
  });

  afterAll(() => {
    flushCoverage();
  });

  it("messages.getMetrics() returns a cohort window with coherent counters", async () => {
    const m = await client.messages.getMetrics(env!.agentEmail);

    // The window is always present and ordered, whether or not it saw traffic.
    expect(m.start.getTime()).toBeLessThan(m.end.getTime());
    expect(m.agentEmail).toBe(env!.agentEmail);

    // Ledger-coverage honesty: the aggregate reports how much of the window it
    // could actually see, so a gap is a visible number rather than a silent
    // undercount that reads as lost mail.
    expect(m.messagesInWindow).toBeGreaterThanOrEqual(0);
    expect(m.messagesWithLifecycle).toBeGreaterThanOrEqual(0);
    expect(m.messagesWithLifecycle).toBeLessThanOrEqual(m.messagesInWindow);

    // The contract's sharpest promise: a zero denominator yields null — NOT 0,
    // which would be indistinguishable from "everything failed". Conditional
    // because this agent may legitimately have traffic on a shared staging
    // deployment; when it does, a rate must be a real number in [0,1].
    if (m.messagesInWindow === 0) {
      expect(m.rates.bounceRate).toBeNull();
      expect(m.rates.complaintRate).toBeNull();
      expect(m.rates.deliveredRate).toBeNull();
    } else {
      for (const r of [
        m.rates.deliveredRate,
        m.rates.bounceRate,
        m.rates.complaintRate,
      ]) {
        if (r !== null) {
          expect(r).toBeGreaterThanOrEqual(0);
          expect(r).toBeLessThanOrEqual(1);
        }
      }
    }

    recordCovered("messages.getMetrics");
  });

  it("messages.getMetrics() honours an explicit cohort window", async () => {
    const end = new Date();
    const start = new Date(end.getTime() - 7 * 24 * 60 * 60 * 1000);

    const m = await client.messages.getMetrics(env!.agentEmail, { start, end });

    // The server echoes the requested window (it may normalise precision, so
    // compare to the second rather than by identity).
    expect(Math.abs(m.start.getTime() - start.getTime())).toBeLessThan(1000);
    expect(Math.abs(m.end.getTime() - end.getTime())).toBeLessThan(1000);

    recordCovered("messages.getMetrics");
  });

  // Account-wide aggregation scans the shared account's cohort window, so its
  // runtime grows with staging data even without a per-agent breakdown. Keep
  // the live-test budget explicit rather than inheriting Vitest's 5s default.
  it("account.metrics() aggregates across the account", async () => {
    const flat = await client.account.metrics();

    expect(flat.start.getTime()).toBeLessThan(flat.end.getTime());
    expect(flat.messagesInWindow).toBeGreaterThanOrEqual(0);
    expect(flat.messagesWithLifecycle).toBeLessThanOrEqual(
      flat.messagesInWindow,
    );
    if (flat.messagesInWindow === 0) {
      expect(flat.rates.bounceRate).toBeNull();
    }

    recordCovered("account.metrics");
  }, 30_000);

  // Split from the flat read. group_by=agent adds a per-agent fan-out, so its
  // cost also scales with how many agents the account owns. Both account-wide
  // reads are environment-dependent and carry explicit budgets: they pass
  // quickly on a small account but can exceed Vitest's 5s default on staging.
  // Do not "tidy" either back to the default: it will pass locally and fail the
  // release pipeline.
  it("account.metrics({ groupBy: 'agent' }) partitions the account total", async () => {
    const grouped = await client.account.metrics({ groupBy: "agent" });
    expect(grouped.start.getTime()).toBeLessThan(grouped.end.getTime());

    // Per-agent rows are a partition of the account total, so they cannot
    // claim more messages than the whole they came from. Skipped when the
    // response is truncated, where the rows are deliberately a subset.
    if (!grouped.agentsTruncated) {
      const perAgentTotal = (grouped.agents ?? []).reduce(
        (sum, a) => sum + a.messagesInWindow,
        0,
      );
      expect(perAgentTotal).toBeLessThanOrEqual(grouped.messagesInWindow);
    }

    recordCovered("account.metrics");
  }, 30_000);
});
