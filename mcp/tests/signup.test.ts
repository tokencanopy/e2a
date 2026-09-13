import { describe, expect, it, vi } from "vitest";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { buildAgentSignupServer } from "../src/tools/signup.js";

describe("public agent signup MCP tools", () => {
  it("creates and verifies an agent without curl or a pre-existing credential", async () => {
    const provider = {
      create: vi.fn(async () => ({
        id: "as_1",
        inbox: "build-bot@agents.example.test",
        humanEmail: "owner@example.test",
        displayName: "Build Bot",
        status: "pending",
        apiKey: "e2a_agt_signup",
      })),
      verify: vi.fn(async () => ({
        id: "as_1",
        inbox: "build-bot@agents.example.test",
        humanEmail: "owner@example.test",
        displayName: "Build Bot",
        status: "verified",
        reviewOutbound: true,
      })),
    };
    const server = buildAgentSignupServer(provider);
    const client = new Client({ name: "signup-test", version: "0" });
    const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
    await server.connect(serverTransport);
    await client.connect(clientTransport);

    const tools = await client.listTools();
    expect(tools.tools.map((tool) => tool.name).sort()).toEqual([
      "signup_agent",
      "verify_agent_signup",
    ]);

    const created = await client.callTool({
      name: "signup_agent",
      arguments: {
        human_email: "owner@example.test",
        display_name: "Build Bot",
        note_to_human: "For the build queue.",
        harness: "codex",
        current_api_key: "e2a_agt_current",
      },
    });
    expect(created.isError).not.toBe(true);
    expect(provider.create).toHaveBeenCalledWith({
      humanEmail: "owner@example.test",
      displayName: "Build Bot",
      noteToHuman: "For the build queue.",
      harness: "codex",
      currentApiKey: "e2a_agt_current",
    });

    const verified = await client.callTool({
      name: "verify_agent_signup",
      arguments: { api_key: "e2a_agt_signup", code: "123456", review_outbound: true },
    });
    expect(verified.isError).not.toBe(true);
    expect(provider.verify).toHaveBeenCalledWith("e2a_agt_signup", {
      code: "123456",
      reviewOutbound: true,
    });

    await client.close();
    await server.close();
  });
});
