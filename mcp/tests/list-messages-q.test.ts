import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { McpClient } from "../src/client.js";
import { buildServer } from "../src/server.js";

function makeStubClient() {
  const client = new McpClient({} as never, "bot@example.com", "account");
  const listMessages = vi
    .spyOn(client, "listMessages")
    .mockResolvedValue({ items: [], next_cursor: undefined });
  return { client, listMessages };
}

async function connect(stub: McpClient): Promise<Client> {
  const server = buildServer({ client: stub, version: "0.0.0-test" });
  const client = new Client({ name: "list-messages-q-test", version: "0.0.0" });
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  await Promise.all([server.connect(serverTransport), client.connect(clientTransport)]);
  return client;
}

describe("list_messages q filter", () => {
  let stub: ReturnType<typeof makeStubClient>;
  let client: Client;

  beforeEach(async () => {
    stub = makeStubClient();
    client = await connect(stub.client);
  });

  it("documents only the v1 q fields", async () => {
    const { tools } = await client.listTools();
    const tool = tools.find((candidate) => candidate.name === "list_messages");
    const properties = (tool?.inputSchema as {
      properties?: Record<string, { description?: string }>;
    }).properties;
    const q = properties?.q;

    expect(q?.description).toContain("v1 fields: label, from, subject, created.");
    expect(q?.description).not.toContain("has");
  });

  it("accepts q at 500 Unicode code points and rejects 501", async () => {
    const accepted = ["a".repeat(500), "😀".repeat(500)];
    const rejected = ["a".repeat(501), "😀".repeat(501)];

    for (const q of accepted) {
      expect(Array.from(q)).toHaveLength(500);
      const result = await client.callTool({ name: "list_messages", arguments: { q } });
      expect(result.isError).not.toBe(true);
      expect(stub.listMessages).toHaveBeenLastCalledWith({ q });
    }

    for (const q of rejected) {
      expect(Array.from(q)).toHaveLength(501);
      const result = await client.callTool({ name: "list_messages", arguments: { q } });
      expect(result.isError).toBe(true);
      expect((result.content as Array<{ text: string }>)[0]?.text).toMatch(/500 Unicode code points/i);
    }
  });

  it("passes q through verbatim and omits it when absent", async () => {
    const q = "label:urgent OR (from:alerts AND NOT subject:newsletter) created>=2026-07-01";
    await client.callTool({ name: "list_messages", arguments: { q } });
    expect(stub.listMessages).toHaveBeenLastCalledWith({ q });

    stub.listMessages.mockClear();
    await client.callTool({ name: "list_messages", arguments: {} });
    const params = stub.listMessages.mock.calls[0]?.[0];
    expect(params).toEqual({});
    expect(params).not.toHaveProperty("q");
  });
});
