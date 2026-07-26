import { recordToolCall, recordToolList } from "./mcp-coverage.ts";

interface JsonRpcRequest {
  jsonrpc: "2.0";
  id: number;
  method: string;
  params?: unknown;
}

// Feed the MCP tool-coverage recorder from the single JSON-RPC choke point, so
// coverage is recorded whether a suite goes through callTool() or calls
// `mcp.call("tools/call", …)` directly. Never throws: coverage is advisory and
// must not be able to fail a suite.
function recordMcpResult(method: string, params: unknown, result: unknown): void {
  try {
    if (method === "tools/list") {
      const tools = (result as { tools?: Array<{ name?: string }> })?.tools;
      if (Array.isArray(tools)) {
        recordToolList(tools.map((t) => t?.name).filter((n): n is string => typeof n === "string"));
      }
      return;
    }
    if (method === "tools/call") {
      const name = (params as { name?: string })?.name;
      if (typeof name === "string") {
        recordToolCall(name, (result as { isError?: boolean })?.isError);
      }
    }
  } catch {
    /* advisory only */
  }
}

interface JsonRpcResponse<T = unknown> {
  jsonrpc: "2.0";
  id: number;
  result?: T;
  error?: { code: number; message: string; data?: unknown };
}

/**
 * The single MCP surface the suites depend on: one JSON-RPC round-trip.
 * The streamable-HTTP client implements it so callTool stays decoupled from
 * transport details.
 */
export interface McpRpcClient {
  call<T = unknown>(method: string, params?: unknown, timeoutMs?: number): Promise<T>;
}

/**
 * MCP client that talks to a DEPLOYED streamable-HTTP `/mcp` server over
 * plain `fetch` — no child process, no `@modelcontextprotocol/sdk` (this
 * package is intentionally zero-dependency). This exercises the same image
 * that ships to prod/staging, not a locally-built stdio binary.
 *
 * The server is a **stateless** transport (`sessionIdGenerator: undefined`
 * in mcp/src/http-server.ts): there is no `initialize` handshake and no
 * `Mcp-Session-Id` — a bare `tools/list` / `tools/call` dispatches on its
 * own. The user's Bearer is forwarded to the e2a backend as-is. The SDK
 * answers a POST as an SSE stream (`text/event-stream`) unless JSON
 * responses are enabled (they aren't), so we accept both framings and parse
 * accordingly — mirroring the Go prober's `mcpCall` (internal/selftest).
 */
export class HttpMcpClient implements McpRpcClient {
  private nextId = 1;
  private readonly url: string;
  private readonly apiKey: string;

  // Plain field assignment (not constructor parameter properties) — the suites
  // run under `node --test` strip-only mode, which rejects param properties.
  constructor(url: string, apiKey: string) {
    this.url = url;
    this.apiKey = apiKey;
  }

  async call<T = unknown>(method: string, params?: unknown, timeoutMs = 15_000): Promise<T> {
    const id = this.nextId++;
    const body: JsonRpcRequest = { jsonrpc: "2.0", id, method, params };
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), timeoutMs);
    let resp: Response;
    try {
      resp = await fetch(this.url, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${this.apiKey}`,
          "Content-Type": "application/json",
          // Streamable-HTTP requires the client to accept both framings.
          Accept: "application/json, text/event-stream",
        },
        body: JSON.stringify(body),
        signal: ctrl.signal,
      });
    } catch (err) {
      const reason = (err as Error)?.name === "AbortError" ? `timed out after ${timeoutMs}ms` : String(err);
      throw new Error(`MCP call ${method} failed: ${reason} (POST ${this.url})`);
    } finally {
      clearTimeout(timer);
    }
    const raw = await resp.text();
    if (!resp.ok) {
      throw new Error(`MCP ${method}: HTTP ${resp.status}. body: ${raw.slice(0, 500)}`);
    }
    const env = parseJsonRpcEnvelope<T>(raw, resp.headers.get("content-type") ?? "");
    if (!env) {
      throw new Error(`MCP ${method}: no JSON-RPC message in response. body: ${raw.slice(0, 500)}`);
    }
    if (env.error) {
      throw new Error(`MCP ${method} error ${env.error.code}: ${env.error.message}`);
    }
    recordMcpResult(method, params, env.result);
    return env.result as T;
  }

  // No process/socket to tear down — present so suites can call it uniformly.
  async stop(): Promise<void> {}
}

/**
 * Decode a JSON-RPC message from an MCP streamable-HTTP response, accepting
 * either a bare JSON body or an SSE stream. For SSE, walk each event's
 * `data:` lines (successive `data:` lines within one event join with `\n`)
 * and return the first that decodes to a message carrying a `jsonrpc` key
 * (ignoring pings / other event types). Mirrors the prober's Go parser.
 */
function parseJsonRpcEnvelope<T>(raw: string, contentType: string): JsonRpcResponse<T> | null {
  if (contentType.includes("text/event-stream")) {
    const body = raw.replace(/\r\n/g, "\n");
    for (const event of body.split("\n\n")) {
      const dataLines: string[] = [];
      for (const line of event.split("\n")) {
        if (line.startsWith("data:")) dataLines.push(line.slice(5).replace(/^ /, ""));
      }
      if (dataLines.length === 0) continue;
      let msg: JsonRpcResponse<T>;
      try {
        msg = JSON.parse(dataLines.join("\n"));
      } catch {
        continue;
      }
      if (msg && (msg as { jsonrpc?: unknown }).jsonrpc) return msg;
    }
    return null;
  }
  try {
    return JSON.parse(raw) as JsonRpcResponse<T>;
  } catch {
    return null;
  }
}

export interface McpToolCall {
  name: string;
  arguments?: Record<string, unknown>;
}

export interface McpToolResult {
  content?: Array<{ type: string; text?: string }>;
  isError?: boolean;
}

export async function callTool(c: McpRpcClient, name: string, args?: Record<string, unknown>): Promise<McpToolResult> {
  return c.call<McpToolResult>("tools/call", { name, arguments: args ?? {} });
}
