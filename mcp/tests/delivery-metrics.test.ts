import { describe, it, expect, vi, beforeEach } from "vitest";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { buildServer } from "../src/server.js";
import type { McpClient } from "../src/client.js";

// Delivery-metrics TOOLS (src/tools/metrics.ts) — not to be confused with
// metrics.test.ts, which covers the MCP server's own Prometheus registry.

function makeStub(scope: "account" | "agent" = "account") {
  return {
    agentEmail: "bot@agents.localhost",
    scope,
    getAgentMetrics: vi.fn(async () => ({
      agentEmail: "bot@agents.localhost",
      start: new Date("2026-07-09T00:00:00Z"),
      end: new Date("2026-08-08T00:00:00Z"),
      messagesInWindow: 10,
      messagesWithLifecycle: 4,
      reconstructedObservations: 0,
      summary: { accepted: 10, submitted: 8, delivered: 6 },
      rates: { deliveredRate: 0.6, bounceRate: null, complaintRate: null, suppressionBlockRate: 0 },
      counters: [],
    })),
    getAccountMetrics: vi.fn(async () => ({
      start: new Date("2026-07-09T00:00:00Z"),
      end: new Date("2026-08-08T00:00:00Z"),
      messagesInWindow: 10,
      messagesWithLifecycle: 10,
      reconstructedObservations: 0,
      summary: { accepted: 10, submitted: 8, delivered: 6 },
      rates: { deliveredRate: 0.6, bounceRate: null, complaintRate: null, suppressionBlockRate: 0 },
      counters: [],
      agents: [],
      agentsTruncated: false,
    })),
  } as unknown as McpClient;
}

async function connect(stub: McpClient): Promise<Client> {
  const server = buildServer({ client: stub, version: "0.0.0-test" });
  const client = new Client({ name: "test", version: "0" }, { capabilities: {} });
  const [ct, st] = InMemoryTransport.createLinkedPair();
  await Promise.all([server.connect(st), client.connect(ct)]);
  return client;
}

function payloadOf(res: unknown): Record<string, any> {
  return JSON.parse(((res as { content: Array<{ text: string }> }).content)[0]!.text);
}

describe("MCP delivery-metrics tools", () => {
  let stub: McpClient;
  let client: Client;

  beforeEach(async () => {
    stub = makeStub();
    client = await connect(stub);
  });

  it("get_agent_metrics has an exact schema and is marked beta", async () => {
    const tool = (await client.listTools()).tools.find((t) => t.name === "get_agent_metrics")!;
    expect(tool.title).toContain("(beta)");
    expect(tool.description).toContain("Beta:");
    const schema = tool.inputSchema as { properties: Record<string, unknown> };
    expect(Object.keys(schema.properties).sort()).toEqual(["email", "end", "start"]);
  });

  it("get_account_metrics pins its enumerated options", async () => {
    const tool = (await client.listTools()).tools.find((t) => t.name === "get_account_metrics")!;
    const schema = tool.inputSchema as { properties: Record<string, { const?: string; enum?: string[] }> };
    expect(Object.keys(schema.properties).sort()).toEqual(["bucket", "end", "group_by", "start"]);
    const groupBy = schema.properties.group_by;
    expect(groupBy.const ?? groupBy.enum?.[0]).toBe("agent");
    const bucket = schema.properties.bucket;
    expect(bucket.const ?? bucket.enum?.[0]).toBe("day");
  });

  it("forwards bucket=day to the account rollup", async () => {
    await client.callTool({ name: "get_account_metrics", arguments: { bucket: "day" } });
    expect(stub.getAccountMetrics).toHaveBeenCalledWith({ bucket: "day" });
  });

  // These descriptions carry caveats a model cannot infer from the numbers: a
  // window still settling, and a null rate that is not a zero.
  it("states the settling window and the null-rate meaning in both descriptions", async () => {
    const { tools } = await client.listTools();
    for (const name of ["get_agent_metrics", "get_account_metrics"]) {
      const desc = tools.find((t) => t.name === name)!.description!;
      expect(desc, `${name} must warn about late feedback`).toContain("72 hours");
      expect(desc, `${name} must explain null rates`).toContain("null — never 0");
      expect(desc, `${name} must disclaim inbox placement`).toContain("does NOT claim inbox placement");
    }
  });

  it("forwards the agent selector and parsed window", async () => {
    await client.callTool({
      name: "get_agent_metrics",
      arguments: { email: "other@agents.localhost", start: "2026-07-01T00:00:00Z", end: "2026-07-08T00:00:00Z" },
    });
    const call = (stub.getAgentMetrics as unknown as { mock: { calls: [{ start: Date; end: Date }, string][] } }).mock.calls[0];
    expect(call[1]).toBe("other@agents.localhost");
    expect(call[0].start.toISOString()).toBe("2026-07-01T00:00:00.000Z");
    expect(call[0].end.toISOString()).toBe("2026-07-08T00:00:00.000Z");
  });

  it("forwards group_by to the account rollup", async () => {
    await client.callTool({ name: "get_account_metrics", arguments: { group_by: "agent" } });
    expect(stub.getAccountMetrics).toHaveBeenCalledWith({ groupBy: "agent" });
  });

  it("omits the window entirely when not supplied, rather than inventing one", async () => {
    await client.callTool({ name: "get_agent_metrics", arguments: {} });
    expect(stub.getAgentMetrics).toHaveBeenCalledWith({}, undefined);
  });

  // An Invalid Date serializes to null and would silently widen the read to
  // the 30-day default, so a bad timestamp has to fail loudly instead.
  it("rejects an unparseable timestamp instead of silently defaulting the window", async () => {
    const res = await client.callTool({
      name: "get_agent_metrics",
      arguments: { start: "last tuesday" },
    });
    expect(res.isError).toBe(true);
    expect(JSON.stringify(res.content)).toContain("not a valid RFC 3339");
    expect(stub.getAgentMetrics).not.toHaveBeenCalled();
  });

  it("surfaces the ledger coverage gap in the payload", async () => {
    const payload = payloadOf(await client.callTool({ name: "get_agent_metrics", arguments: {} }));
    expect(payload.messages_in_window).toBe(10);
    expect(payload.messages_with_lifecycle).toBe(4);
  });

  it("keeps a null rate null on the wire", async () => {
    const payload = payloadOf(await client.callTool({ name: "get_agent_metrics", arguments: {} }));
    expect(payload.rates.bounce_rate).toBeNull();
    expect(payload.rates.suppression_block_rate).toBe(0);
  });

  // Tier gating: an agent reads its own numbers; the rollup is account admin.
  it("exposes get_agent_metrics but not get_account_metrics to an agent scope", async () => {
    const agentClient = await connect(makeStub("agent"));
    const names = new Set((await agentClient.listTools()).tools.map((t) => t.name));
    expect(names.has("get_agent_metrics")).toBe(true);
    expect(names.has("get_account_metrics")).toBe(false);
  });
});
