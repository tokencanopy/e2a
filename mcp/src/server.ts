import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { McpClient } from "./client.js";
import { registerMessageTools } from "./tools/messages.js";
import { registerAgentTools } from "./tools/agents.js";
import { registerDomainTools } from "./tools/domains.js";
import { registerReviewTools } from "./tools/review.js";
import { registerWebhookTools } from "./tools/webhooks.js";
import { registerEventTools } from "./tools/events.js";
import { registerTemplateTools } from "./tools/templates.js";
import { registerApiKeyTools } from "./tools/apikeys.js";
import { registerLegacyTools } from "./tools/legacy.js";
import { registerContactTools } from "./tools/contacts.js";
import { registerSuppressionTools } from "./tools/suppressions.js";
import { toolNamesForScope } from "./tools/tiers.js";
import { resolveServerVersion } from "./version.js";

export interface ToolExecutionRecord {
  tool: string;
  outcome: "ok" | "error";
  durationMs: number;
  /** Server-vocabulary error code from the isError result, when present. */
  errorCode?: string;
}

export interface BuildServerOptions {
  client: McpClient;
  version?: string;
  /**
   * Observability hook fired once per tool execution (both ok and isError
   * results, plus handler throws). The HTTP transport wires this to the
   * tool_execution metric + log event; in-process callers can ignore it.
   */
  onToolExecution?: (rec: ToolExecutionRecord) => void;
}

function isErrorResult(result: unknown): boolean {
  return !!result && typeof result === "object" && (result as { isError?: unknown }).isError === true;
}

function errorCodeOf(result: unknown): string | undefined {
  if (!isErrorResult(result)) return undefined;
  const code = (result as { structuredContent?: { code?: unknown } }).structuredContent?.code;
  return typeof code === "string" ? code : undefined;
}

// registerTool's last argument is always the handler (the overloads only
// vary whether a config object precedes it). Wrap it to time the call and
// classify the outcome without changing the handler's behavior or result.
function wrapToolHandlerArgs(
  name: string,
  args: unknown[],
  onToolExecution?: (rec: ToolExecutionRecord) => void,
): unknown[] {
  if (!onToolExecution) return args;
  const last = args.length - 1;
  if (last < 0 || typeof args[last] !== "function") return args;
  const handler = args[last] as (...a: unknown[]) => unknown;
  const wrapped = async (...callArgs: unknown[]) => {
    const start = Date.now();
    try {
      const result = await handler(...callArgs);
      const errorCode = errorCodeOf(result);
      onToolExecution({
        tool: name,
        outcome: isErrorResult(result) ? "error" : "ok",
        durationMs: Date.now() - start,
        ...(errorCode ? { errorCode } : {}),
      });
      return result;
    } catch (err) {
      onToolExecution({ tool: name, outcome: "error", durationMs: Date.now() - start });
      throw err;
    }
  };
  const copy = args.slice();
  copy[last] = wrapped;
  return copy;
}

// gateRegistration intercepts server.registerTool so a session only registers
// the tools its credential scope is allowed to see (§6a). One seam gates every
// tool — the per-resource register*Tools functions stay scope-agnostic, and the
// tier classification lives solely in tiers.ts. Tools outside the allowed set
// are silently skipped (not registered → not listed → not callable). This is a
// surface/decision-space optimization; the backend still enforces scope, so a
// skipped tool is never the security boundary.
function gateRegistration(
  server: McpServer,
  allowed: ReadonlySet<string>,
  onToolExecution?: (rec: ToolExecutionRecord) => void,
): void {
  const original = server.registerTool.bind(server) as (...a: unknown[]) => unknown;
  server.registerTool = ((name: string, ...rest: unknown[]) => {
    if (!allowed.has(name)) {
      // Skip: not wired into the server's tool list. Returns undefined; the
      // register*Tools callers don't use the return value.
      return undefined as unknown as ReturnType<McpServer["registerTool"]>;
    }
    return original(name, ...wrapToolHandlerArgs(name, rest, onToolExecution)) as ReturnType<
      McpServer["registerTool"]
    >;
  }) as McpServer["registerTool"];
}

export function buildServer({
  client,
  version = resolveServerVersion(),
  onToolExecution,
}: BuildServerOptions): McpServer {
  const server = new McpServer({
    name: "e2a",
    version,
  });
  // Scope-gate the surface to the connecting credential's tier (client.scope,
  // resolved at session init via whoami). account → full surface; agent →
  // runtime/inbox subset.
  gateRegistration(server, toolNamesForScope(client.scope), onToolExecution);
  registerMessageTools(server, client);
  registerAgentTools(server, client);
  registerDomainTools(server, client);
  registerReviewTools(server, client);
  registerWebhookTools(server, client);
  registerEventTools(server, client);
  registerTemplateTools(server, client);
  registerApiKeyTools(server, client);
  registerContactTools(server, client);
  registerSuppressionTools(server, client);
  registerLegacyTools(server, client);
  return server;
}
