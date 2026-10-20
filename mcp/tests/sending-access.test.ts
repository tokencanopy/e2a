import { describe, expect, it, vi } from "vitest";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { E2AError } from "@e2a/sdk/v1";
import type { McpClient } from "../src/client.js";
import { buildServer } from "../src/server.js";
import { ADMIN_TOOLS, RUNTIME_TOOLS } from "../src/tools/tiers.js";
import { MUTATING_META_KEY, MUTATING_TOOLS, NON_MUTATING_TOOLS, TOOL_OPERATIONS } from "../src/tools/mutating.js";

// External sending access (beta) over MCP: get_sending_access_request (read)
// and request_sending_access (write → mutating, refused for a read-only
// account). Every value is synthetic.

const pending = {
  id: "esar_test",
  state: "pending",
  useCase: "order confirmations for customers who signed up on example.com",
  recipients: "our own signed-up customers",
  expectedDailyVolume: 50,
  createdAt: new Date("2026-01-01T00:00:00Z"),
};

function stubClient(overrides: Partial<Record<"getSendingAccessRequest" | "requestSendingAccess", unknown>> = {}, scope: "account" | "agent" = "account") {
  return {
    agentEmail: undefined,
    scope,
    getSendingAccessRequest: vi.fn(async () => pending),
    requestSendingAccess: vi.fn(async () => pending),
    ...overrides,
  } as unknown as McpClient & {
    getSendingAccessRequest: ReturnType<typeof vi.fn>;
    requestSendingAccess: ReturnType<typeof vi.fn>;
  };
}

async function connect(stub: McpClient): Promise<Client> {
  const server = buildServer({ client: stub, version: "0.0.0-test" });
  const [a, b] = InMemoryTransport.createLinkedPair();
  await server.connect(a);
  const client = new Client({ name: "test", version: "0.0.0" });
  await client.connect(b);
  return client;
}

describe("sending access MCP tools", () => {
  it("are classified: request mutating, get read-only, both admin tier, mapped to their operations", () => {
    expect(MUTATING_TOOLS.has("request_sending_access")).toBe(true);
    expect(NON_MUTATING_TOOLS.has("request_sending_access")).toBe(false);
    expect(NON_MUTATING_TOOLS.has("get_sending_access_request")).toBe(true);
    expect(TOOL_OPERATIONS.request_sending_access).toEqual(["createSendingAccessRequest"]);
    expect(TOOL_OPERATIONS.get_sending_access_request).toEqual(["getSendingAccessRequest"]);
    for (const name of ["request_sending_access", "get_sending_access_request"]) {
      expect(ADMIN_TOOLS.has(name)).toBe(true);
      expect(RUNTIME_TOOLS.has(name)).toBe(false);
    }
  });

  it("advertise the mutating flag and annotations over tools/list; agent scope sees neither", async () => {
    const acct = await connect(stubClient());
    const { tools } = await acct.listTools();
    const req = tools.find((t) => t.name === "request_sending_access");
    const get = tools.find((t) => t.name === "get_sending_access_request");
    expect(req?._meta?.[MUTATING_META_KEY]).toBe(true);
    expect(get?._meta?.[MUTATING_META_KEY]).toBe(false);
    expect(get?.annotations?.readOnlyHint).toBe(true);
    expect(req?.annotations?.readOnlyHint).not.toBe(true);
    expect(req?.inputSchema.required?.sort()).toEqual(["expected_daily_volume", "recipients", "use_case"]);

    const agent = await connect(stubClient({}, "agent"));
    const agentNames = new Set((await agent.listTools()).tools.map((t) => t.name));
    expect(agentNames.has("request_sending_access")).toBe(false);
    expect(agentNames.has("get_sending_access_request")).toBe(false);
  });

  it("tell an agent to file once, not retry, and that the decision arrives by email", async () => {
    const { tools } = await (await connect(stubClient())).listTools();
    const req = tools.find((t) => t.name === "request_sending_access")!.description ?? "";
    expect(req).toMatch(/ONCE/);
    expect(req).toMatch(/do not retry/i);
    expect(req).toContain("rate_limited");
    expect(req).toMatch(/emailed to the account owner/);
    expect(req).toContain("available_unlocks");
    const get = tools.find((t) => t.name === "get_sending_access_request")!.description ?? "";
    expect(get).toMatch(/emailed to the account owner/);
    expect(get).toContain("available_unlocks");
    // The send tools point at the request tool and no longer promise an
    // unconditional unlock.
    for (const name of ["send_message", "reply_to_message", "forward_message", "send_email"]) {
      const d = tools.find((t) => t.name === name)!.description ?? "";
      expect(d, name).toContain("request_sending_access");
      expect(d, name).toContain("available_unlocks");
      expect(d, name).not.toMatch(/recover by verifying a sending domain/);
    }
    const whoami = tools.find((t) => t.name === "whoami")!.description ?? "";
    expect(whoami).toContain("available_unlocks");
  });

  it("request_sending_access forwards the snake_case form and returns the request in wire shape", async () => {
    const stub = stubClient();
    const client = await connect(stub);
    const res = await client.callTool({
      name: "request_sending_access",
      arguments: { use_case: pending.useCase, recipients: pending.recipients, expected_daily_volume: 50 },
    });
    expect(res.isError).toBeFalsy();
    expect(stub.requestSendingAccess).toHaveBeenCalledWith({
      useCase: pending.useCase,
      recipients: pending.recipients,
      expectedDailyVolume: 50,
    });
    const body = JSON.parse((res.content as Array<{ text: string }>)[0].text);
    expect(body).toMatchObject({ id: "esar_test", state: "pending", expected_daily_volume: 50 });
  });

  it("request_sending_access rejects out-of-range input before calling the API", async () => {
    const stub = stubClient();
    const client = await connect(stub);
    for (const args of [
      { use_case: "", recipients: "x", expected_daily_volume: 1 },
      { use_case: "x", recipients: "x", expected_daily_volume: 0 },
      { use_case: "x", recipients: "x", expected_daily_volume: 1.5 },
      { use_case: "x", recipients: "x", expected_daily_volume: 1, account_id: "usr_other" },
    ]) {
      const res = await client.callTool({ name: "request_sending_access", arguments: args });
      expect(res.isError, JSON.stringify(args)).toBe(true);
    }
    expect(stub.requestSendingAccess).not.toHaveBeenCalled();
  });

  it("a read-only account's request surfaces account_read_only (not retryable)", async () => {
    const stub = stubClient({
      requestSendingAccess: vi.fn(async () => {
        throw new E2AError({ code: "account_read_only", message: "account is read-only", status: 403, retryable: false });
      }),
    });
    const client = await connect(stub);
    const res = await client.callTool({
      name: "request_sending_access",
      arguments: { use_case: "x", recipients: "y", expected_daily_volume: 1 },
    });
    expect(res.isError).toBe(true);
    expect(res.structuredContent).toMatchObject({ code: "account_read_only", retryable: false, status: 403 });
  });

  it("get_sending_access_request returns the latest request", async () => {
    const stub = stubClient();
    const client = await connect(stub);
    const res = await client.callTool({ name: "get_sending_access_request", arguments: {} });
    expect(res.isError).toBeFalsy();
    expect(stub.getSendingAccessRequest).toHaveBeenCalledTimes(1);
    expect(JSON.parse((res.content as Array<{ text: string }>)[0].text)).toMatchObject({ id: "esar_test", state: "pending" });
  });
});
