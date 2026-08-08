import { describe, expect, it, beforeEach, vi } from "vitest";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import type { McpClient } from "../src/client.js";
import { buildServer } from "../src/server.js";

// MCP tool tests for list_events / get_event / redeliver_event.
// Pattern mirrors tools.test.ts — in-memory transport, stub McpClient,
// assert wire shape passed to the wrapper + the tool result. The
// wrapper auto-paginates, so list_events returns a flat array (the
// cursor token is handled inside the SDK pager, not surfaced).

function makeStubClient(): McpClient {
  const stub = {
    agentEmail: "bot@example.com",
    scope: "account" as const,
    // Events methods on the wrapper. listEvents is cursor-paginated → Page.
    listEvents: vi.fn(async (_params?: Record<string, unknown>) => ({
      items: [
        {
          id: "evt_abc",
          type: "email.received",
          schemaVersion: 1,
          createdAt: "2026-06-01T12:00:00Z",
          status: "processed",
          data: { from: "alice@example.com" },
        },
      ],
      next_cursor: undefined,
    })),
    getEvent: vi.fn(async (id: string) => ({
      id,
      type: "email.received",
      schemaVersion: 1,
      createdAt: "2026-06-01T12:00:00Z",
      status: "processed",
      data: {},
      deliveryStatus: { matchedWebhooks: 1, delivered: 1, pending: 0, failed: 0 },
    })),
    redeliverEvent: vi.fn(async (id: string, webhookId?: string) => ({
      eventId: id,
      webhookId: webhookId ?? "",
      deliveryId: "whd_replay_xyz",
      status: "pending",
    })),
  };
  return stub as unknown as McpClient;
}

async function buildClient(stub: McpClient): Promise<Client> {
  const server = buildServer({ client: stub, version: "test" });
  const [serverTransport, clientTransport] = InMemoryTransport.createLinkedPair();
  await server.connect(serverTransport);
  const client = new Client({ name: "test", version: "1.0" });
  await client.connect(clientTransport);
  return client;
}

function parseToolResult(result: unknown): Record<string, unknown> {
  const r = result as { content: { type: string; text: string }[] };
  expect(r.content[0].type).toBe("text");
  return JSON.parse(r.content[0].text);
}

describe("MCP events tools", () => {
  let stub: McpClient;

  beforeEach(() => {
    stub = makeStubClient();
  });

  describe("list_events", () => {
    it("is registered", async () => {
      const client = await buildClient(stub);
      const { tools } = await client.listTools();
      const names = tools.map((t) => t.name);
      expect(names).toContain("list_events");
    });

    it("invokes client.listEvents with no params", async () => {
      const client = await buildClient(stub);
      const result = await client.callTool({ name: "list_events", arguments: {} });
      const parsed = parseToolResult(result);
      expect(parsed.events).toBeDefined();
      const events = parsed.events as Array<{ id: string }>;
      expect(events[0].id).toBe("evt_abc");
      expect(stub.listEvents).toHaveBeenCalledOnce();
    });

    it("surfaces next_cursor when more pages remain", async () => {
      stub.listEvents.mockResolvedValueOnce({ items: [{ id: "evt_p1" }], next_cursor: "c_next" });
      const client = await buildClient(stub);
      const parsed = parseToolResult(
        await client.callTool({ name: "list_events", arguments: {} }),
      );
      expect((parsed.events as Array<{ id: string }>)[0].id).toBe("evt_p1");
      expect(parsed.next_cursor).toBe("c_next");
    });

    it("forwards all filter params + cursor/limit to the wrapper", async () => {
      const client = await buildClient(stub);
      await client.callTool({
        name: "list_events",
        arguments: {
          type: "email.received",
          agent_email: "ag_x",
          conversation_id: "conv_y",
          message_id: "msg_z",
          since: "2026-06-01T00:00:00Z",
          until: "2026-06-02T00:00:00Z",
          limit: 25,
          cursor: "c_prev",
        },
      });
      expect(stub.listEvents).toHaveBeenCalledWith({
        type: "email.received",
        agentEmail: "ag_x",
        conversationId: "conv_y",
        messageId: "msg_z",
        since: "2026-06-01T00:00:00Z",
        until: "2026-06-02T00:00:00Z",
        limit: 25,
        cursor: "c_prev",
      });
    });
  });

  describe("get_event", () => {
    it("is registered", async () => {
      const client = await buildClient(stub);
      const { tools } = await client.listTools();
      expect(tools.map((t) => t.name)).toContain("get_event");
    });

    it("calls client.getEvent with the event_id", async () => {
      const client = await buildClient(stub);
      const result = await client.callTool({
        name: "get_event",
        arguments: { event_id: "evt_xyz" },
      });
      const parsed = parseToolResult(result);
      expect(parsed.id).toBe("evt_xyz");
      expect(stub.getEvent).toHaveBeenCalledWith("evt_xyz");
    });

    it("surfaces deliveryStatus in the response", async () => {
      const client = await buildClient(stub);
      const result = await client.callTool({
        name: "get_event",
        arguments: { event_id: "evt_xyz" },
      });
      const parsed = parseToolResult(result);
      const ds = parsed.delivery_status as Record<string, number>;
      expect(ds.delivered).toBe(1);
    });
  });

  describe("redeliver_event", () => {
    it("is registered", async () => {
      const client = await buildClient(stub);
      const { tools } = await client.listTools();
      expect(tools.map((t) => t.name)).toContain("redeliver_event");
    });

    it("targeted: forwards webhook_id to the wrapper", async () => {
      const client = await buildClient(stub);
      await client.callTool({
        name: "redeliver_event",
        arguments: { event_id: "evt_abc", webhook_id: "wh_target" },
      });
      expect(stub.redeliverEvent).toHaveBeenCalledWith("evt_abc", "wh_target");
    });

    it("fan-out: omits webhook_id when not provided", async () => {
      const client = await buildClient(stub);
      await client.callTool({
        name: "redeliver_event",
        arguments: { event_id: "evt_abc" },
      });
      expect(stub.redeliverEvent).toHaveBeenCalledWith("evt_abc", undefined);
    });

    it("returns the new delivery_id", async () => {
      const client = await buildClient(stub);
      const result = await client.callTool({
        name: "redeliver_event",
        arguments: { event_id: "evt_abc", webhook_id: "wh_x" },
      });
      const parsed = parseToolResult(result);
      expect(parsed.delivery_id).toBe("whd_replay_xyz");
    });
  });

  describe("tool catalog", () => {
    it("includes the 3 events tools in the additive v1 tool set — total 78", async () => {
      const client = await buildClient(stub);
      const { tools } = await client.listTools();
      const names = new Set(tools.map((t) => t.name));
      // The events tools add list_events/get_event/redeliver_event; the
      // full registered set (incl. the 8 beta template tools and the 3
      // api-key tools, the 3 trash-lifecycle tools, the beta message-lifecycle
      // diagnostic tool, contact/outreach and suppression tools, the two
      // delivery-metrics tools, and compatibility aliases) is 78 tools.
      expect(tools).toHaveLength(78);
      expect(names.has("list_events")).toBe(true);
      expect(names.has("get_event")).toBe(true);
      expect(names.has("redeliver_event")).toBe(true);
      // Existing tools should still be registered (re-curated names).
      expect(names.has("send_message")).toBe(true);
      expect(names.has("list_messages")).toBe(true);
      expect(names.has("whoami")).toBe(true);
    });
  });

  describe("concurrent invocations", () => {
    it("handles 20 concurrent list_events calls", async () => {
      const client = await buildClient(stub);
      const results = await Promise.all(
        Array.from({ length: 20 }, () =>
          client.callTool({ name: "list_events", arguments: {} }),
        ),
      );
      expect(results).toHaveLength(20);
      for (const r of results) {
        const parsed = parseToolResult(r);
        expect((parsed.events as unknown[]).length).toBe(1);
      }
      expect(stub.listEvents).toHaveBeenCalledTimes(20);
    });

    it("handles concurrent mix of all three tools", async () => {
      const client = await buildClient(stub);
      await Promise.all([
        client.callTool({ name: "list_events", arguments: { type: "email.received" } }),
        client.callTool({ name: "get_event", arguments: { event_id: "evt_a" } }),
        client.callTool({
          name: "redeliver_event",
          arguments: { event_id: "evt_b", webhook_id: "wh_x" },
        }),
        client.callTool({ name: "list_events", arguments: {} }),
        client.callTool({ name: "get_event", arguments: { event_id: "evt_c" } }),
      ]);
      expect(stub.listEvents).toHaveBeenCalledTimes(2);
      expect(stub.getEvent).toHaveBeenCalledTimes(2);
      expect(stub.redeliverEvent).toHaveBeenCalledTimes(1);
    });
  });
});
