import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { HttpMcpClient, callTool, type McpToolResult } from "../harness/mcp.ts";
import { info, writeReport } from "../harness/report.ts";

// Black-box MCP conformance for the beta delivery-metrics tools against the
// DEPLOYED streamable-HTTP /mcp server. This is the MCP analogue of suite
// 37-metrics.test.ts (REST) — same domain, same server-side semantics, but
// every call goes through tools/call so mcp_coverage_gate.py credits the tool.
//
// Tools exercised (both):
//   get_agent_metrics    (runtime tier)
//   get_account_metrics  (admin tier)
//
// The metrics surface shipped across four surfaces, each with its own live
// coverage gate: REST operationIds, the TS SDK ergonomic facade, the Python
// SDK facade, and these MCP tools. Three had coverage; this file closes the
// fourth.
//
// Asserted on SHAPE and INVARIANTS, never on specific counts — these run
// against a shared deployment whose traffic is not this suite's to control.
// The invariants are the contract's real promises and hold at any traffic
// level, including zero. Creates nothing: metrics are read-only, and minting
// an agent for a clean fixture competes for the account's agent cap with every
// other suite in the run.
const SUITE = "38-mcp-metrics";
const apiClient = new ApiClient();
const mcp = new HttpMcpClient(apiClient.env.mcpUrl, apiClient.env.apiKey);

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
  rates: Rates;
}
interface AgentMetricsResult extends MetricsBase {
  agent_email: string;
}
interface AccountMetricsResult extends MetricsBase {
  agents?: Array<{ agent_email: string; messages_in_window: number }> | null;
  agents_truncated?: boolean;
}

// Both tools must stay advertised: a silent removal would otherwise look like
// coverage passing, since an unadvertised tool is simply never counted.
const REQUIRED_TOOLS = ["get_agent_metrics", "get_account_metrics"];

function extractText(r: McpToolResult): string {
  return r.content?.find((c) => c.type === "text")?.text ?? "";
}

function parseOk<T>(r: McpToolResult, label: string): T {
  assert.equal(r.isError, undefined, `${label} isError: ${extractText(r).slice(0, 300)}`);
  const text = extractText(r);
  assert.ok(text.length > 0, `${label}: empty tool result`);
  return JSON.parse(text) as T;
}

function assertMetricsInvariants(m: MetricsBase, label: string) {
  assert.ok(Date.parse(m.start) < Date.parse(m.end), `${label}: window must be ordered`);
  assert.ok(m.messages_in_window >= 0, `${label}: messages_in_window >= 0`);
  assert.ok(
    m.messages_with_lifecycle <= m.messages_in_window,
    `${label}: messages_with_lifecycle (${m.messages_with_lifecycle}) must not exceed messages_in_window (${m.messages_in_window})`,
  );
  for (const [k, v] of Object.entries(m.rates ?? {})) {
    if (v === null) continue;
    assert.ok(
      typeof v === "number" && v >= 0 && v <= 1,
      `${label}: rate ${k} must be null or within [0,1], got ${v}`,
    );
  }
  // The contract's sharpest promise: a zero denominator yields null, NEVER 0 —
  // 0% is indistinguishable from total delivery failure.
  if (m.messages_in_window === 0) {
    assert.equal(m.rates.bounce_rate, null, `${label}: zero denominator must yield null, not 0`);
  }
}

test("mcp-metrics: tools/list advertises both metrics tools", async () => {
  const list = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  const names = new Set(list.tools.map((t) => t.name));
  const missing = REQUIRED_TOOLS.filter((n) => !names.has(n));
  assert.deepEqual(
    missing,
    [],
    `deployed MCP server must advertise every metrics tool; missing: ${missing.join(", ")}`,
  );
  info(SUITE, "tools-list", `server advertises ${list.tools.length} tools`);
});

test("mcp-metrics: get_agent_metrics returns a coherent cohort window", async () => {
  const email = apiClient.env.primaryAgentEmail;
  const r = await callTool(mcp, "get_agent_metrics", { email });
  const m = parseOk<AgentMetricsResult>(r, "get_agent_metrics");

  assert.equal(m.agent_email, email, "result must echo the agent it describes");
  assertMetricsInvariants(m, "get_agent_metrics");

  info(SUITE, "agent-metrics", `window ${m.start} → ${m.end}`, {
    messages_in_window: m.messages_in_window,
  });
});

test("mcp-metrics: get_account_metrics aggregates, and groups by agent", async () => {
  const flat = parseOk<AccountMetricsResult>(
    await callTool(mcp, "get_account_metrics", {}),
    "get_account_metrics",
  );
  assertMetricsInvariants(flat, "get_account_metrics");

  const grouped = parseOk<AccountMetricsResult>(
    await callTool(mcp, "get_account_metrics", { group_by: "agent" }),
    "get_account_metrics(group_by=agent)",
  );
  assertMetricsInvariants(grouped, "get_account_metrics(group_by=agent)");

  // Per-agent rows are a partition of the account total, so they cannot claim
  // more than the whole they came from. Skipped when truncated, where the rows
  // are deliberately a subset.
  if (!grouped.agents_truncated) {
    const perAgent = (grouped.agents ?? []).reduce((sum, a) => sum + a.messages_in_window, 0);
    assert.ok(
      perAgent <= grouped.messages_in_window,
      `per-agent total (${perAgent}) must not exceed the account total (${grouped.messages_in_window})`,
    );
  }

  info(SUITE, "account-metrics", `${(grouped.agents ?? []).length} agent row(s)`, {
    truncated: grouped.agents_truncated,
  });
});

test("mcp-metrics: an unknown agent is a tool error, not zeroed metrics", async () => {
  const r = await callTool(mcp, "get_agent_metrics", { email: "no-such-agent@invalid.test" });
  assert.equal(
    r.isError,
    true,
    "an unknown agent must surface as a tool error: zeroed metrics would read to an agent as \"my mail stopped\"",
  );
});

after(() => {
  writeReport(`reports/${SUITE}.json`);
});
