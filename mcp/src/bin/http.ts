#!/usr/bin/env node
import { startHttpServer } from "../http-server.js";

interface BinConfig {
  port: number;
  baseUrl: string;
  allowedHosts: string[];
  sessionIdleMs: number;
  maxSessions: number;
  resolveTimeoutMs: number;
  publicUrl?: string;
  authorizationServerUrl?: string;
  trustProxy: boolean | number | string;
}

class ConfigError extends Error {}

type LogSeverity = "INFO" | "WARNING" | "ERROR";

// logJson emits one operational event as a single-line JSON object on
// stderr. GCE Cloud Logging parses a single-line JSON payload into
// structured `jsonPayload` fields and honors two special keys: `severity`
// (sets the entry's log level) and `message` (the human-readable summary
// shown in the console). Keeping each event on one line also means
// multi-line content like a stack trace in `error` stays a single log
// entry instead of being split into several fragmented ones. The result
// is both human-skimmable (via `message`) and queryable (filter on
// `severity`, `event`, or any structured field).
function logJson(
  severity: LogSeverity,
  event: string,
  message: string,
  fields: Record<string, unknown> = {},
): void {
  process.stderr.write(`${JSON.stringify({ severity, event, message, ...fields })}\n`);
}

function parsePositiveInt(name: string, raw: string, def: number): number {
  if (raw === "") return def;
  const n = Number(raw);
  if (!Number.isFinite(n) || !Number.isInteger(n) || n <= 0) {
    throw new ConfigError(`${name} must be a positive integer; got ${JSON.stringify(raw)}`);
  }
  return n;
}

function parsePort(name: string, raw: string, def: number): number {
  if (raw === "") return def;
  const n = Number(raw);
  // Port 0 is valid (OS-assigned), but reject NaN / negatives / >65535.
  if (!Number.isFinite(n) || !Number.isInteger(n) || n < 0 || n > 65535) {
    throw new ConfigError(`${name} must be 0..65535; got ${JSON.stringify(raw)}`);
  }
  return n;
}

// parseTrustProxy maps E2A_TRUST_PROXY to the value Express's `trust
// proxy` setting expects. Default "loopback": only a same-host proxy (the
// prod Caddy front, forwarding over localhost) is trusted for
// X-Forwarded-* headers. "true"/"false" become booleans; a bare integer
// is a hop count (Express reads a numeric *string* as a subnet, so it must
// be converted); anything else passes through as a preset name
// ("loopback"/"uniquelocal"/...) or a CSV of IPs/subnets.
function parseTrustProxy(raw: string): boolean | number | string {
  if (raw === "") return "loopback";
  if (raw === "true") return true;
  if (raw === "false") return false;
  if (/^\d+$/.test(raw)) return Number(raw);
  return raw;
}

// hostFromUrl extracts the bare hostname (no port) from a URL string, used to
// derive the MCP_ALLOWED_HOSTS default from MCP_PUBLIC_URL. Throws
// ConfigError on an unparseable URL so a typo'd MCP_PUBLIC_URL fails loudly
// instead of silently producing a useless allowlist.
function hostFromUrl(name: string, raw: string): string {
  try {
    return new URL(raw).hostname.toLowerCase();
  } catch {
    throw new ConfigError(`${name} is not a valid URL; got ${JSON.stringify(raw)}`);
  }
}

// parseHostList has NO literal-host fallback: defaulting to the operator's
// api.e2a.dev would silently 421 every request to a self-hosted deployment
// with a different host, with no body and no log line. When
// MCP_ALLOWED_HOSTS is unset, derive the allowlist from MCP_PUBLIC_URL's
// host if that's set (it already names this deployment's externally
// reachable host); otherwise there is no safe default and we fail closed.
function parseHostList(raw: string, publicUrl: string | undefined): string[] {
  if (raw !== "") {
    const list = raw.split(",").map((s) => s.trim()).filter(Boolean);
    if (list.length === 0) {
      throw new ConfigError(`MCP_ALLOWED_HOSTS resolved to an empty list (raw=${JSON.stringify(raw)})`);
    }
    return list;
  }
  if (publicUrl) {
    return [hostFromUrl("MCP_PUBLIC_URL", publicUrl)];
  }
  throw new ConfigError(
    "MCP_ALLOWED_HOSTS is required (or set MCP_PUBLIC_URL, whose host becomes the default allowlist) — " +
      "this server has no default Host allowlist, since defaulting to the operator's api.e2a.dev would 421 " +
      "every request to a self-hosted deployment with no body and no log line.",
  );
}

// resolveBaseUrl picks the API host this server talks to. Canonical is
// E2A_API_URL — the same concept the backend names with E2A_API_URL (its
// externally visible API base) and the SDKs read. This server is a pure API
// client, so it wants the API host, NOT the deployment root that the CLI's
// E2A_URL points at (that one also serves the dashboard).
//
// E2A_URL and E2A_BASE_URL are both legacy names this binary has shipped with;
// still accepted so existing deployment manifests keep working, with a stderr
// deprecation note. main() calls loadConfig exactly once, so the note is emitted
// once per process without needing a module-level guard to dedupe it.
//
// There is deliberately NO terminal fallback to "https://api.e2a.dev". This
// server forwards the caller's bearer token to `baseUrl` verbatim
// (`new E2AClient({ apiKey: bearer, baseUrl })` in http-server.ts) and also
// uses it for /readyz and RFC 9728 discovery. A self-hoster who forgets this
// var would otherwise have their users' credentials silently sent to the
// operator's production API instead of their own deployment — a loud
// failure here costs five minutes, a silent fallback costs a leaked
// credential.
function resolveBaseUrl(env: NodeJS.ProcessEnv): string {
  const canonical = env.E2A_API_URL;
  if (canonical) return canonical;
  for (const legacy of ["E2A_URL", "E2A_BASE_URL"] as const) {
    const v = env[legacy];
    if (!v) continue;
    logJson(
      "WARNING",
      "e2a_api_url_legacy_name",
      `${legacy} is deprecated; rename it to E2A_API_URL (the old names still work today).`,
    );
    return v;
  }
  throw new ConfigError(
    "E2A_API_URL is required (or the legacy E2A_URL / E2A_BASE_URL) — this server has no default backend API. " +
      "Falling back to the operator's api.e2a.dev would forward your users' bearer tokens to someone else's deployment.",
  );
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): BinConfig {
  const publicUrl = env.MCP_PUBLIC_URL || undefined;
  return {
    port: parsePort("PORT", env.PORT ?? "", 3000),
    baseUrl: resolveBaseUrl(env),
    allowedHosts: parseHostList(env.MCP_ALLOWED_HOSTS ?? "", publicUrl),
    sessionIdleMs: parsePositiveInt("MCP_SESSION_IDLE_MS", env.MCP_SESSION_IDLE_MS ?? "", 5 * 60_000),
    maxSessions: parsePositiveInt("MCP_MAX_SESSIONS", env.MCP_MAX_SESSIONS ?? "", 500),
    resolveTimeoutMs: parsePositiveInt("MCP_RESOLVE_TIMEOUT_MS", env.MCP_RESOLVE_TIMEOUT_MS ?? "", 5000),
    publicUrl,
    authorizationServerUrl: env.MCP_AUTHORIZATION_SERVER_URL || undefined,
    trustProxy: parseTrustProxy(env.E2A_TRUST_PROXY ?? ""),
  };
}

export { ConfigError, logJson };

async function main(): Promise<void> {
  let cfg: BinConfig;
  try {
    cfg = loadConfig();
  } catch (err) {
    if (err instanceof ConfigError) {
      logJson("ERROR", "config_error", `config error: ${err.message}`, { error: err.message });
      process.exit(2);
    }
    throw err;
  }

  const { close, port: bound } = await startHttpServer(cfg.port, {
    baseUrl: cfg.baseUrl,
    allowedHosts: cfg.allowedHosts,
    // The server is stateless; the legacy session knobs now size the
    // bearer→principal resolution cache (TTL + max entries). Env names are
    // kept so existing deploy manifests keep working.
    resolveCacheTtlMs: cfg.sessionIdleMs,
    resolveCacheMaxEntries: cfg.maxSessions,
    resolveTimeoutMs: cfg.resolveTimeoutMs,
    publicUrl: cfg.publicUrl,
    authorizationServerUrl: cfg.authorizationServerUrl,
    trustProxy: cfg.trustProxy,
    // Route all request-scoped events (http_request, auth_resolution,
    // tool_execution, terminal_error) through the same writer as the
    // process-lifecycle events.
    logger: logJson,
  });
  logJson("INFO", "listening", `e2a-mcp-http listening on :${bound}`, {
    port: bound,
    baseUrl: cfg.baseUrl,
    allowedHosts: cfg.allowedHosts,
  });

  // Graceful shutdown: stop accepting new connections, drain active
  // sessions, then exit. Hard ceiling at 30s to avoid hanging deploys.
  let closing = false;
  const shutdown = async (signal: NodeJS.Signals) => {
    if (closing) return;
    closing = true;
    logJson("INFO", "draining", `received ${signal}, draining...`, { signal });
    const drainTimeout = setTimeout(() => {
      logJson("ERROR", "drain_timeout", "drain timeout, forcing exit");
      process.exit(1);
    }, 30_000);
    drainTimeout.unref();
    try {
      await close();
      clearTimeout(drainTimeout);
      process.exit(0);
    } catch (err) {
      clearTimeout(drainTimeout);
      const message = err instanceof Error ? err.message : String(err);
      logJson("ERROR", "shutdown_error", `shutdown error: ${message}`, { error: message });
      process.exit(1);
    }
  };
  process.on("SIGTERM", () => void shutdown("SIGTERM"));
  process.on("SIGINT", () => void shutdown("SIGINT"));
}

// Only run main() when invoked as the entry point — keeps `loadConfig`
// importable from tests without spinning up the server.
const isMain = import.meta.url === `file://${process.argv[1]}`;
if (isMain) {
  main().catch((err) => {
    const message = err instanceof Error ? err.stack ?? err.message : String(err);
    logJson("ERROR", "fatal", `e2a-mcp-http fatal: ${message}`, { error: message });
    process.exit(1);
  });
}
