import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { info, writeReport } from "../harness/report.ts";

// Black-box conformance for the beta delivery-metrics surface:
// getAgentMetrics (GET /v1/agents/{email}/metrics) and getAccountMetrics
// (GET /v1/metrics).
//
// Asserted on SHAPE and INVARIANTS, never on specific counts. These run
// against a shared deployment whose traffic is not this suite's to control, so
// any assertion on a particular number would be flaky by construction. The
// invariants below are the contract's actual promises and hold at any traffic
// level, including zero.
//
// This suite creates NOTHING. Metrics are read-only, and a suite that mints
// agents to get a clean fixture competes for the account's agent cap with
// every other suite in the run.
const SUITE = "37-metrics";
const client = new ApiClient();

interface Rates {
  delivered_rate: number | null;
  bounce_rate: number | null;
  complaint_rate: number | null;
  suppression_block_rate: number | null;
}
interface MetricsBase {
  start: string;
  end: string;
  messages_in_window: number;
  messages_with_lifecycle: number;
  reconstructed_observations: number;
  summary: Record<string, number>;
  rates: Rates;
}
interface AgentMetricsView extends MetricsBase {
  agent_email: string;
}
interface AccountMetricsView extends MetricsBase {
  agents: Array<{ agent_email: string; messages_in_window: number }> | null;
  agents_truncated: boolean;
}

/** Shared invariants both shapes must satisfy. */
function assertMetricsInvariants(m: MetricsBase, label: string) {
  assert.ok(
    Date.parse(m.start) < Date.parse(m.end),
    `${label}: window must be ordered (start < end)`,
  );

  // Ledger-coverage honesty: the aggregate reports how much of the window it
  // could actually see, so a gap is a visible number rather than a silent
  // undercount that would read as lost mail.
  assert.ok(m.messages_in_window >= 0, `${label}: messages_in_window >= 0`);
  assert.ok(
    m.messages_with_lifecycle <= m.messages_in_window,
    `${label}: messages_with_lifecycle (${m.messages_with_lifecycle}) must not exceed messages_in_window (${m.messages_in_window})`,
  );

  // The contract's sharpest promise: a zero denominator yields null, NEVER 0 —
  // 0% is indistinguishable from total delivery failure.
  for (const [k, v] of Object.entries(m.rates)) {
    if (v === null) continue;
    assert.ok(
      typeof v === "number" && v >= 0 && v <= 1,
      `${label}: rate ${k} must be null or within [0,1], got ${v}`,
    );
  }
  if (m.messages_in_window === 0) {
    assert.equal(m.rates.bounce_rate, null, `${label}: zero denominator must yield null bounce_rate, not 0`);
    assert.equal(m.rates.delivered_rate, null, `${label}: zero denominator must yield null delivered_rate, not 0`);
  }
}

test("agent metrics: default cohort window returns coherent counters", async () => {
  const email = client.env.primaryAgentEmail;
  const r = await client.get<AgentMetricsView>(`/v1/agents/${encodeURIComponent(email)}/metrics`);
  assert.equal(r.status, 200, `expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);

  const m = r.body!;
  assert.equal(m.agent_email, email, "response must echo the agent it describes");
  assertMetricsInvariants(m, "agent metrics");

  info(SUITE, "agent-default-window", `window ${m.start} → ${m.end}`, {
    messages_in_window: m.messages_in_window,
    messages_with_lifecycle: m.messages_with_lifecycle,
  });
});

test("agent metrics: an explicit window is honoured", async () => {
  const email = client.env.primaryAgentEmail;
  const end = new Date();
  const start = new Date(end.getTime() - 7 * 24 * 60 * 60 * 1000);

  const r = await client.get<AgentMetricsView>(
    `/v1/agents/${encodeURIComponent(email)}/metrics` +
      `?start=${encodeURIComponent(start.toISOString())}&end=${encodeURIComponent(end.toISOString())}`,
  );
  assert.equal(r.status, 200, `expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);

  // The server may normalise RFC 3339 precision, so compare instants rather
  // than strings.
  const m = r.body!;
  assert.ok(
    Math.abs(Date.parse(m.start) - start.getTime()) < 1000,
    `echoed start ${m.start} should match the requested ${start.toISOString()}`,
  );
  assert.ok(
    Math.abs(Date.parse(m.end) - end.getTime()) < 1000,
    `echoed end ${m.end} should match the requested ${end.toISOString()}`,
  );
  assertMetricsInvariants(m, "agent metrics (explicit window)");
});

test("account metrics: aggregates the account and groups by agent", async () => {
  const flat = await client.get<AccountMetricsView>("/v1/metrics");
  assert.equal(flat.status, 200, `expected 200, got ${flat.status}: ${flat.raw.slice(0, 200)}`);
  assertMetricsInvariants(flat.body!, "account metrics");

  // group_by is the only shape-changing parameter, so it is worth exercising.
  const grouped = await client.get<AccountMetricsView>("/v1/metrics?group_by=agent");
  assert.equal(grouped.status, 200, `expected 200, got ${grouped.status}: ${grouped.raw.slice(0, 200)}`);
  const g = grouped.body!;
  assertMetricsInvariants(g, "account metrics (group_by=agent)");

  // Per-agent rows are a partition of the account total, so they cannot claim
  // more messages than the whole they came from. Skipped when the response is
  // truncated, where the rows are deliberately a subset.
  if (!g.agents_truncated) {
    const perAgent = (g.agents ?? []).reduce((sum, a) => sum + a.messages_in_window, 0);
    assert.ok(
      perAgent <= g.messages_in_window,
      `per-agent total (${perAgent}) must not exceed the account total (${g.messages_in_window})`,
    );
  }

  info(SUITE, "account-group-by-agent", `${(g.agents ?? []).length} agent row(s)`, {
    truncated: g.agents_truncated,
    messages_in_window: g.messages_in_window,
  });
});

test("agent metrics: an unknown agent is rejected, not silently empty", async () => {
  const r = await client.get(`/v1/agents/${encodeURIComponent("no-such-agent@invalid.test")}/metrics`);
  assert.ok(
    r.status === 404 || r.status === 400,
    `an unknown agent must fail cleanly (404/400), got ${r.status}: zeroed metrics for a nonexistent agent would read as "your mail stopped"`,
  );
});

after(() => {
  writeReport(`reports/${SUITE}.json`);
});
