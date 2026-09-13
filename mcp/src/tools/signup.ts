import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { E2AClient, signupAgent } from "@e2a/sdk/v1";
import type {
  AgentSignupCreateResponse,
  AgentSignupRequest,
  AgentSignupView,
  VerifyAgentSignupRequest,
} from "@e2a/sdk/v1";
import { z } from "zod";
import { resolveServerVersion } from "../version.js";
import { runTool, strictInputSchema } from "./util.js";

export interface AgentSignupProvider {
  create(body: AgentSignupRequest): Promise<AgentSignupCreateResponse | object>;
  verify(apiKey: string, body: VerifyAgentSignupRequest): Promise<AgentSignupView | object>;
}

export function sdkAgentSignupProvider(baseUrl: string): AgentSignupProvider {
  return {
    create: (body) => signupAgent(body, { baseUrl }),
    verify: (apiKey, body) =>
      new E2AClient({ apiKey, baseUrl }).agentSignup.verify(body),
  };
}

export function registerAgentSignupTools(
  server: McpServer,
  provider: AgentSignupProvider,
): void {
  server.registerTool(
    "signup_agent",
    {
      title: "Create a provisional e2a agent",
      description:
        "Create an e2a inbox and one-time agent API key without an existing credential. A six-digit verification code is emailed to the named human. Save api_key from the result, then call verify_agent_signup. To repeat the same human_email + display_name, pass current_api_key; the successful request rotates it and resends the code.",
      annotations: {
        readOnlyHint: false,
        destructiveHint: false,
        idempotentHint: false,
        openWorldHint: true,
      },
      inputSchema: strictInputSchema({
        human_email: z.string().email().max(320).describe("Human who will receive the six-digit code and own the agent."),
        display_name: z.string().min(1).max(200).describe("Human-readable agent name; the inbox slug is derived from it."),
        note_to_human: z.string().max(2000).optional().describe("Optional explanation included in the verification email."),
        harness: z.string().max(100).optional().describe("Optional harness identifier, for example codex or a custom runtime name."),
        current_api_key: z.string().min(1).max(256).optional().describe("Current agent key, required to rotate and resend an existing pending signup."),
      }),
    },
    async (args) =>
      runTool(() =>
        provider.create({
          humanEmail: args.human_email,
          displayName: args.display_name,
          ...(args.note_to_human !== undefined ? { noteToHuman: args.note_to_human } : {}),
          ...(args.harness !== undefined ? { harness: args.harness } : {}),
          ...(args.current_api_key !== undefined ? { currentApiKey: args.current_api_key } : {}),
        }),
      ),
  );

  server.registerTool(
    "verify_agent_signup",
    {
      title: "Verify a provisional e2a agent",
      description:
        "Verify the provisional inbox using its one-time api_key and the six-digit code sent to the human. Set review_outbound to route later messages to recipients other than the human through e2a's existing review queue. While the request is still pending, the human can approve or reject it in the dashboard instead.",
      annotations: {
        readOnlyHint: false,
        destructiveHint: false,
        idempotentHint: false,
        openWorldHint: true,
      },
      inputSchema: strictInputSchema({
        api_key: z.string().min(1).describe("The one-time agent key returned by signup_agent. It is used only for this API call and is not returned."),
        code: z.string().regex(/^\d{6}$/).describe("Six-digit code sent to human_email."),
        review_outbound: z.boolean().optional().default(false).describe("Route non-human outbound through the existing human review queue after verification."),
      }),
    },
    async (args) =>
      runTool(() =>
        provider.verify(args.api_key, {
          code: args.code,
          reviewOutbound: args.review_outbound,
        }),
      ),
  );
}

/** Public, intentionally tiny MCP surface used before an agent has a key. */
export function buildAgentSignupServer(
  provider: AgentSignupProvider,
  version = resolveServerVersion(),
): McpServer {
  const server = new McpServer({ name: "e2a-mcp-server", version });
  registerAgentSignupTools(server, provider);
  return server;
}
