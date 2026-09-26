import express, { type Express, type NextFunction, type Request, type Response } from "express";
import cors from "cors";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { E2AClient } from "@e2a/sdk/v1";
import { buildServer, type ToolExecutionRecord } from "./server.js";
import { McpClient } from "./client.js";
import type { Scope } from "./tools/tiers.js";
import { ResolveCache, type ResolvedPrincipal } from "./resolve.js";
import { MetricsRegistry, type RouteLabel } from "./metrics.js";
import { correlationMiddleware, requestIdOf } from "./correlation.js";
import { defaultLogger, type Logger } from "./logging.js";
import { ReadinessProber, type ReadyzOptions } from "./readyz.js";
import {
  buildAgentSignupServer,
  sdkAgentSignupProvider,
  type AgentSignupProvider,
} from "./tools/signup.js";

export interface HttpServerOptions {
  /** Base URL of the e2a backend (Bearer is forwarded as-is). */
  baseUrl: string;
  /** Hostnames the SDK will accept; rejects anything else with 421. */
  allowedHosts: string[];
  /**
   * Externally reachable URL of this MCP server. When set, used verbatim
   * for the RFC 9728 protected-resource metadata `resource` field and
   * the `WWW-Authenticate: resource_metadata=...` value on 401s. When
   * unset (production default behind Caddy), we synthesize the URL from
   * the inbound request Host, forcing `https` for any non-loopback host
   * (a public MCP endpoint is always TLS-fronted — see #635). The
   * local-dev runbook sets this to `http://localhost:8765` so Claude
   * Code's OAuth probe can resolve the metadata over loopback http.
   */
  publicUrl?: string;
  /**
   * Externally reachable URL of the authorization server. Defaults to
   * baseUrl when unset — fine in prod where baseUrl is e2a.dev. In a
   * Docker compose setup the bearer-forwarding side (baseUrl) needs
   * the container-internal hostname (http://e2a:8080) while the OAuth
   * client needs the host-reachable URL (http://localhost:8080). Set
   * this to override what's advertised in protected-resource metadata
   * without changing where the SDK forwards bearer tokens.
  */
  authorizationServerUrl?: string;
  /**
   * Express `trust proxy` setting. Controls which hops' `X-Forwarded-*`
   * headers are honored when computing externally visible discovery URLs.
   * Defaults to `"loopback"`, matching the production Caddy topology.
   */
  trustProxy?: boolean | number | string;
  /** Optional shared resolution cache (defaults to a fresh one). */
  resolveCache?: ResolveCache;
  /** How long a resolved bearer→principal entry stays cached (ms). */
  resolveCacheTtlMs?: number;
  /** Hard cap on cached principals. */
  resolveCacheMaxEntries?: number;
  /**
   * Bound (ms) on the whoami scope-resolution probe, with at most one
   * retry. Default 5000. Without this a hung backend could hold a POST
   * for the SDK's 30s × 3-attempt worst case before failing closed.
   */
  resolveTimeoutMs?: number;
  /** Metrics registry for /metrics and the internal counters (defaults to
   *  a fresh one per app — inject for test isolation or to read back). */
  metrics?: MetricsRegistry;
  /** Structured log sink for request-scoped events (defaults to single-line
   *  JSON on stderr; the bin passes its logJson). */
  logger?: Logger;
  /** Readiness-probe seams for GET /readyz (timeout/cache/clock/fetcher). */
  readyz?: ReadyzOptions;
  /**
   * Test seam: override how the per-request E2AClient is built. The
   * default constructs `new E2AClient({ apiKey: bearer, baseUrl })`.
   *
   * The optional second arg carries the agent_email + scope resolved by the
   * per-bearer `whoami` prefetch (see {@link resolvePrincipal}). Tests that
   * want to exercise the prefetch path can pass it through to their stub;
   * tests that only care about the bearer can ignore it.
   */
  clientFactory?: (bearer: string, opts?: { agentEmail?: string; scope?: Scope }) => McpClient;
  /** Test/custom transport seam for the unauthenticated /mcp/signup tools. */
  agentSignupProvider?: AgentSignupProvider;
}

interface BuiltApp {
  app: Express;
  cache: ResolveCache;
  metrics: MetricsRegistry;
}

// Runtime bundles the per-app dependencies the route handlers share, so the
// middleware factories don't grow parallel parameter lists.
interface Runtime {
  opts: HttpServerOptions;
  cache: ResolveCache;
  metrics: MetricsRegistry;
  logger: Logger;
}

const DEFAULT_RESOLVE_TIMEOUT_MS = 5_000;

// Routes whose successful responses are counted in metrics but skipped by
// the access log (orchestrator probes and scrapers hit them every few
// seconds forever). Failures on these routes still log.
const QUIET_ROUTES: ReadonlySet<RouteLabel> = new Set(["healthz", "readyz", "metrics"]);

// classifyRoute maps a request path to its bounded metric/log label. Any
// path outside the known set collapses to "other" so cardinality stays fixed.
function classifyRoute(path: string): RouteLabel {
  if (path === "/mcp" || path === "/mcp/signup") return "mcp";
  if (path === "/healthz") return "healthz";
  if (path === "/readyz") return "readyz";
  if (path === "/metrics") return "metrics";
  if (path.startsWith("/.well-known/")) return "discovery";
  return "other";
}

/**
 * Build the Express app that hosts the MCP Streamable HTTP transport.
 *
 * Per design (.claude/design/mcp-system.md §4.2): this server is pure,
 * **stateless** transport. It forwards the user's Bearer to the e2a backend
 * unchanged (the backend dispatches on token prefix e2a_ vs ate2a_) and holds
 * no per-connection session state — every request builds a fresh server +
 * transport and dispatches independently. The only thing kept between requests
 * is a short-lived bearer→principal cache so a burst of tool calls doesn't pay
 * a `whoami` round-trip each. There is no session idle GC, so an idle client is
 * never disconnected; only the bearer's own expiry (handled by the client's
 * OAuth refresh) ever ends a connection.
 */
export function buildApp(opts: HttpServerOptions): BuiltApp {
  const cache =
    opts.resolveCache ??
    new ResolveCache({
      ttlMs: opts.resolveCacheTtlMs ?? 60_000,
      maxEntries: opts.resolveCacheMaxEntries ?? 500,
    });
  const metrics = opts.metrics ?? new MetricsRegistry();
  const logger = opts.logger ?? defaultLogger;
  const rt: Runtime = { opts, cache, metrics, logger };

  const app = express();
  // Set this before any route reads req.protocol. The default trusts only the
  // same-host production proxy, so public clients cannot spoof the scheme.
  app.set("trust proxy", opts.trustProxy ?? "loopback");
  // CORS open for v0.2; revisit when we have a real allowlist of MCP hosts.
  app.use(cors({ origin: "*", exposedHeaders: ["Mcp-Session-Id"] }));

  // Correlation first: every response (including 401/405/421/500) carries
  // the X-Request-Id this request is logged under.
  app.use(correlationMiddleware);

  // Per-request metric + structured access log, emitted when the response
  // finishes (covers every route and status, success or error). The route is
  // classified NOW, before any mounted middleware strips the mount prefix
  // from req.url (a 421 from the /mcp host guard would otherwise label as
  // "other" at finish time).
  //
  // Probe/scrape routes are metered but not access-logged on success: a 10s
  // healthcheck plus a 15s Prometheus scrape is ~14k INFO lines/day/replica
  // of pure noise (and real log-ingest cost). Failures (>=400) on those
  // routes still log — a 503 readyz is exactly what an operator greps for.
  app.use((req, res, next) => {
    const start = Date.now();
    const path = req.path;
    const route = classifyRoute(path);
    res.on("finish", () => {
      const durationMs = Date.now() - start;
      const statusClass = `${Math.floor(res.statusCode / 100)}xx`;
      metrics.incHttpRequest(route, statusClass);
      metrics.observeRequestDuration(route, durationMs / 1000);
      if (QUIET_ROUTES.has(route) && res.statusCode < 400) return;
      logger("INFO", "http_request", `${req.method} ${path} ${res.statusCode}`, {
        request_id: requestIdOf(res),
        route,
        method: req.method,
        status: res.statusCode,
        duration_ms: durationMs,
      });
    });
    next();
  });

  // Pure liveness only — never consults the backend.
  app.get("/healthz", (_req, res) => {
    res.json({ ok: true });
  });

  // Readiness: probes the backend API health (bounded + cached) so a
  // load balancer can tell "process alive" from "can actually serve".
  const prober = new ReadinessProber(opts.baseUrl, opts.readyz);
  app.get("/readyz", async (_req, res) => {
    const ok = await prober.check();
    metrics.incReadyzCheck(ok ? "ok" : "degraded");
    if (ok) {
      res.json({ ok: true, checks: { api: "ok" } });
      return;
    }
    // Match the failure-cache TTL: any sooner retry would hit the cached 503.
    res.setHeader("Retry-After", String(prober.cacheTtlSeconds));
    res.status(503).json({
      ok: false,
      checks: { api: "unreachable" },
      request_id: requestIdOf(res),
    });
  });

  // Prometheus scrape. Unauthenticated like /healthz: the exposition carries
  // only counters/gauges over closed label vocabularies — no bearer, user, or
  // request data.
  app.get("/metrics", (_req, res) => {
    res.type("text/plain; version=0.0.4; charset=utf-8").send(metrics.render());
  });

  // DNS rebinding protection. The SDK deprecated its in-transport allowlist
  // in favor of external middleware; we enforce it here. 421 Misdirected
  // Request is the spec-recommended status for "this server is not what
  // you asked for." Strip port before comparing so the same allowlist
  // entry works both for prod (`api.e2a.dev`) and tests on random ports
  // (`127.0.0.1:54321`).
  const allowedHosts = new Set(opts.allowedHosts.map((h) => h.toLowerCase()));
  app.use("/mcp", (req, res, next) => {
    const host = req.headers.host;
    if (!host) {
      res.status(421).end();
      return;
    }
    const bare = host.split(":")[0]!.toLowerCase();
    if (!allowedHosts.has(bare)) {
      res.status(421).end();
      return;
    }
    next();
  });

  // Spec-mandated discovery for hosts probing where the auth server lives.
  // Served unconditionally (even pre-OAuth) so clients don't get a confusing
  // 404 between v0.2 and v0.3. Validates Host against the allowlist to
  // avoid reflecting attacker-controlled hosts back in the `resource` URL.
  // Compute the public-facing URL of this MCP server. Three cases:
  //   1. publicUrl set explicitly: trust it verbatim (local-dev http,
  //      or any deployment behind a fronting proxy that knows its own
  //      external URL better than we do).
  //   2. unset + Host header present + Host in allowlist: synthesize the URL
  //      from Host, forcing https for non-loopback hosts while preserving the
  //      trusted request protocol for loopback development.
  //   3. unset + Host missing/disallowed: caller wrapped function
  //      rejects with 421 before reaching here.
  const resolveResourceUrl = (req: Request): string | null => {
    if (opts.publicUrl) {
      return opts.publicUrl.replace(/\/+$/, "");
    }
    const host = req.headers.host;
    if (!host) return null;
    const bare = host.split(":")[0]!.toLowerCase();
    if (!allowedHosts.has(bare)) return null;
    return `${discoveryScheme(req, host)}://${host}`;
  };

  app.get("/.well-known/oauth-protected-resource", (req, res) => {
    const resource = resolveResourceUrl(req);
    if (!resource) {
      res.status(421).end();
      return;
    }
    res.json({
      resource,
      authorization_servers: [opts.authorizationServerUrl ?? opts.baseUrl],
      // Scope vocabulary tracks the AS (Slice 5b): the lone "mcp" scope is
      // retired. MCP clients connect as public DCR clients; the consent screen
      // grants "agent" (single inbox) or "account" (workspace admin) when the
      // user opts in on an account-eligible client (loopback or https — see the
      // backend's accountEligibleRedirect). Both are advertised so a
      // spec-compliant client requests the full menu and the protected-resource
      // and AS metadata agree.
      scopes_supported: ["agent", "account"],
      bearer_methods_supported: ["header"],
    });
  });

  // Authenticate before reading a potentially large request body. The body
  // limit must clear the attachment contract: tools advertise 10 MB per file
  // and 25 MB combined, which base64-encode to ~34 MB plus the envelope.
  // Route-local parsing ensures a missing/revoked credential cannot spend that
  // parser budget. The fronting proxy remains the outer wire-size guard.
  const parseMcpJson = express.json({ limit: "40mb" });
  // Public bootstrap surface lives at a separate MCP endpoint so the primary
  // /mcp endpoint can keep returning its OAuth challenge when no bearer is
  // present. Its tools need no curl and expose only signup + code verification.
  // A small parser limit is sufficient because neither tool accepts content or
  // attachments.
  app.post(
    "/mcp/signup",
    express.json({ limit: "64kb" }),
    async (req, res) => {
      const provider = opts.agentSignupProvider ?? sdkAgentSignupProvider(opts.baseUrl);
      const server = buildAgentSignupServer(provider);
      await handleMcpServerRequest(req, res, server);
    },
  );
  app.post(
    "/mcp",
    authenticateClient(rt),
    parseMcpJson,
    async (req, res) => {
      await handleAuthenticatedClientRequest(req, res, rt);
    },
  );

  // Stateless transport: there is no standalone SSE stream and no session to
  // terminate, so the GET (SSE notifications) and DELETE (session teardown)
  // verbs the Streamable-HTTP spec defines for stateful servers don't apply.
  // Answer both with 405 + a JSON-RPC error, matching the SDK's stateless
  // reference server.
  app.get("/mcp", methodNotAllowed);
  app.delete("/mcp", methodNotAllowed);
  app.get("/mcp/signup", methodNotAllowed);
  app.delete("/mcp/signup", methodNotAllowed);

  // Terminal error handler (MUST be last; the 4-arg signature is how Express
  // identifies it). Without it, Express's default finalhandler dumps the error
  // stack — including absolute internal file paths — into the response body when
  // NODE_ENV !== "production" (bin/http never sets it), and never emits a
  // JSON-RPC error. Convert any post-route throw into a generic JSON-RPC
  // internal error; the detail is logged server-side only, never sent to the
  // client/model.
  app.use((err: unknown, req: Request, res: Response, next: NextFunction) => {
    const message = err instanceof Error ? err.message : String(err);
    const name = err instanceof Error ? err.name : typeof err;
    logger("ERROR", "terminal_error", `unhandled request error: ${message}`, {
      request_id: requestIdOf(res),
      error: name,
    });
    if (res.headersSent) {
      // The streaming transport already began writing; we can't reshape the
      // response, so let Express abort the connection.
      next(err);
      return;
    }
    res
      .status(500)
      .json(jsonRpcError(req.body, -32603, "internal server error", { request_id: requestIdOf(res) }));
  });

  return { app, cache, metrics };
}

async function handleMcpServerRequest(
  req: Request,
  res: Response,
  server: ReturnType<typeof buildAgentSignupServer>,
): Promise<void> {
  const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
  res.on("close", () => {
    void transport.close();
    void server.close();
  });
  try {
    await server.connect(transport);
    await transport.handleRequest(req, res, req.body);
  } catch (err) {
    await transport.close().catch(() => undefined);
    await server.close().catch(() => undefined);
    throw err;
  }
}

function methodNotAllowed(_req: Request, res: Response): void {
  res
    .status(405)
    .json(
      jsonRpcError(null, -32000, "Method not allowed: the e2a MCP server is stateless", {
        request_id: requestIdOf(res),
      }),
    );
}

// req.protocol honors X-Forwarded-Proto only for hops allowed by `trust proxy`.
function externalScheme(req: Request): string {
  return req.protocol === "https" ? "https" : "http";
}

// isLoopbackHost reports whether a Host header names a local-dev loopback
// address. Mirrors the bare-host extraction used by the allowlist check (port
// stripped; IPv6 literals aren't a supported deployment shape for this server).
function isLoopbackHost(host: string): boolean {
  const bare = host.split(":")[0]!.toLowerCase();
  return bare === "localhost" || bare === "127.0.0.1";
}

// discoveryScheme picks the scheme for synthesized discovery URLs (used only
// when publicUrl is unset). A public host is always TLS-fronted, so we force
// https there — this keeps the RFC 9728 `resource` identifier correct even when
// the fronting proxy's X-Forwarded-Proto isn't trusted (E2A_TRUST_PROXY
// misconfig, which silently downgraded prod to http; see #635). Loopback dev
// keeps deriving from the trusted request scheme so http://localhost still works
// (and local dev sets publicUrl explicitly anyway).
function discoveryScheme(req: Request, host: string): string {
  return isLoopbackHost(host) ? externalScheme(req) : "https";
}

// resourceMetadataURL returns the value the `WWW-Authenticate` header
// should advertise. Honors publicUrl when set (local-dev http), falls
// back to the synthesized scheme + Host otherwise.
function resourceMetadataURL(req: Request, opts: HttpServerOptions): string {
  const host = req.headers.host ?? "";
  const base = opts.publicUrl
    ? opts.publicUrl.replace(/\/+$/, "")
    : `${discoveryScheme(req, host)}://${host}`;
  return `${base}/.well-known/oauth-protected-resource`;
}

function bearerChallenge(req: Request, opts: HttpServerOptions): string {
  return `Bearer realm="e2a", resource_metadata="${resourceMetadataURL(req, opts)}"`;
}

class InvalidBearerError extends Error {
  constructor() {
    super("invalid bearer token");
    this.name = "InvalidBearerError";
  }
}

interface AuthenticatedContext {
  bearer: string;
  principal: ResolvedPrincipal;
}

function authenticateClient(
  rt: Runtime,
): (req: Request, res: Response, next: NextFunction) => Promise<void> {
  const { cache, opts, metrics, logger } = rt;
  return async (req, res, next) => {
    const requestId = requestIdOf(res);
    const bearer = extractBearer(req);
    if (!bearer) {
      res.setHeader("WWW-Authenticate", bearerChallenge(req, opts));
      res
        .status(401)
        .json(jsonRpcError(null, -32001, "missing bearer token", { request_id: requestId }));
      return;
    }

    // Resolve the credential's scope + bound agent before parsing JSON. A
    // cache hit skips the backend round-trip; revoked/expired tokens fail here.
    let principal = cache.get(bearer);
    if (principal) {
      metrics.incAuthResolution("cache_hit");
      logger("INFO", "auth_resolution", "bearer resolved from cache", {
        request_id: requestId,
        result: "cache_hit",
        duration_ms: 0,
        scope: principal.scope,
      });
    } else {
      const start = Date.now();
      let resolved: { value: ResolvedPrincipal; cacheable: boolean };
      try {
        resolved = await resolvePrincipal(opts, bearer);
      } catch (err) {
        if (err instanceof InvalidBearerError) {
          metrics.incAuthResolution("invalid");
          logger("WARNING", "auth_resolution", "bearer rejected by the backend", {
            request_id: requestId,
            result: "invalid",
            duration_ms: Date.now() - start,
          });
          res.setHeader(
            "WWW-Authenticate",
            `${bearerChallenge(req, opts)}, error="invalid_token"`,
          );
          res
            .status(401)
            .json(jsonRpcError(null, -32001, "invalid bearer token", { request_id: requestId }));
          return;
        }
        throw err;
      }
      const durationMs = Date.now() - start;
      principal = resolved.value;
      if (resolved.cacheable) {
        metrics.incAuthResolution("resolved");
        logger("INFO", "auth_resolution", "bearer resolved via whoami", {
          request_id: requestId,
          result: "resolved",
          duration_ms: durationMs,
          scope: principal.scope,
        });
        cache.set(bearer, principal);
      } else {
        // The previously silent fail-closed path: a non-auth backend failure
        // (5xx/timeout) now leaves an operable trace.
        metrics.incAuthResolution("fallback");
        logger("WARNING", "auth_resolution", "whoami probe failed; serving least-privilege fallback", {
          request_id: requestId,
          result: "fallback",
          duration_ms: durationMs,
          scope: principal.scope,
        });
      }
    }
    metrics.setResolveCacheEntries(cache.size());

    // The cold-path whoami probe is awaited, so the client may have
    // disconnected meanwhile. Do not hand an abandoned request to the parser.
    if (res.closed) return;

    res.locals.auth = { bearer, principal } satisfies AuthenticatedContext;
    next();
  };
}

async function handleAuthenticatedClientRequest(
  req: Request,
  res: Response,
  rt: Runtime,
): Promise<void> {
  const { opts, metrics, logger } = rt;
  const auth = res.locals.auth as AuthenticatedContext | undefined;
  if (!auth) throw new Error("missing authenticated MCP request context");
  const { bearer, principal } = auth;
  const requestId = requestIdOf(res);

  // Stateless: a fresh server + transport per request, torn down when the
  // response closes. The SDK forbids reusing a stateless transport across
  // requests (message-id collisions), and a fresh transport with
  // sessionIdGenerator=undefined skips all session/initialize gating, so a
  // bare tools/call dispatches without a prior initialize on this instance.
  const client = buildClient(opts, bearer, principal);
  const server = buildServer({
    client,
    onToolExecution: (rec: ToolExecutionRecord) => {
      metrics.incToolExecution(rec.tool, rec.outcome);
      logger(rec.outcome === "ok" ? "INFO" : "WARNING", "tool_execution", `tool ${rec.tool} ${rec.outcome}`, {
        request_id: requestId,
        tool: rec.tool,
        outcome: rec.outcome,
        duration_ms: rec.durationMs,
        ...(rec.errorCode ? { error_code: rec.errorCode } : {}),
      });
    },
  });
  const transport = new StreamableHTTPServerTransport({
    sessionIdGenerator: undefined,
  });
  res.on("close", () => {
    void transport.close();
    void server.close();
  });
  try {
    await server.connect(transport);
    await transport.handleRequest(req, res, req.body);
  } catch (err) {
    await transport.close().catch(() => undefined);
    await server.close().catch(() => undefined);
    throw err;
  }
}

/**
 * Construct the per-request E2AClient for an already-resolved principal.
 *
 * Why the agent-email default lives here and not in the SDK: the SDK is a thin
 * contract shared by CLI, browser, server, and Python users — auto-resolving an
 * agent on construction would be too magical for those callers. The MCP
 * transport has a clear notion of "the connecting credential," so pinning its
 * bound agent lets every tool call dispatch without forcing the LLM (or the
 * user) to pass `email` explicitly.
 */
function buildClient(
  opts: HttpServerOptions,
  bearer: string,
  principal: ResolvedPrincipal,
): McpClient {
  const { agentEmail, scope } = principal;
  if (opts.clientFactory) {
    // Preserve the no-arg-factory shape for callers (and tests) that don't
    // care about the resolved email/scope. Pass them through only when set.
    return agentEmail || scope
      ? opts.clientFactory(bearer, { ...(agentEmail ? { agentEmail } : {}), ...(scope ? { scope } : {}) })
      : opts.clientFactory(bearer);
  }
  return new McpClient(
    new E2AClient({ apiKey: bearer, baseUrl: opts.baseUrl }),
    agentEmail ?? "",
    // Fail CLOSED if scope is ever absent: "agent" is the least-privilege tier
    // (account = full admin surface). principal.scope is always set today, so
    // this only guards a future refactor — but a fail-open default here would
    // silently expose the admin surface.
    scope ?? "agent",
  );
}

/**
 * Resolve a bearer's scope + bound agent from whoami (GET /account), which the
 * backend scope-filters per credential (§6a). This drives both tool-tier gating
 * (server.ts) and the per-agent default:
 *   - agent scope   → pin the bound agent (whoami.agentEmail); the credential
 *     IS that agent. Surface = runtime tier.
 *   - account scope → no default agent (per §6a, explicit `email` required —
 *     the old single-agent auto-resolve is dropped). Surface = full.
 *
 * Throws {@link InvalidBearerError} on a 401 (revoked/expired/garbage token) so
 * the caller answers with the OAuth challenge. A non-auth failure (transient
 * backend hiccup) returns a least-privilege fallback marked `cacheable: false`
 * so it doesn't stick — the backend still enforces scope, and the next request
 * re-probes once the backend recovers.
 *
 * The probe is BOUNDED two ways (resolveTimeoutMs, default 5s): the race below
 * caps the total wait regardless of the client implementation, and the
 * default-path E2AClient is built with a per-attempt `timeoutMs` +
 * `maxRetries: 1` so it can't stretch to the SDK's 30s × 3-attempt worst case
 * on its own.
 */
export async function resolvePrincipal(
  opts: HttpServerOptions,
  bearer: string,
): Promise<{ value: ResolvedPrincipal; cacheable: boolean }> {
  const timeoutMs = opts.resolveTimeoutMs ?? DEFAULT_RESOLVE_TIMEOUT_MS;
  // The probe only calls whoami; its scope/agent don't matter. Build it bare
  // (factory(bearer) with no resolved opts) so the construction is
  // distinguishable from the final, resolved client.
  const probe = opts.clientFactory
    ? opts.clientFactory(bearer)
    : new McpClient(
        new E2AClient({
          apiKey: bearer,
          baseUrl: opts.baseUrl,
          timeoutMs,
          maxRetries: 1,
        }),
        "",
        "account",
      );
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    const me = await Promise.race([
      probe.whoami(),
      new Promise<never>((_resolve, reject) => {
        timer = setTimeout(
          () => reject(new Error(`whoami probe timed out after ${timeoutMs}ms`)),
          timeoutMs,
        );
      }),
    ]);
    const scope: Scope = me.scope === "account" ? "account" : "agent";
    const agentEmail = scope === "agent" && me.agentEmail ? me.agentEmail : undefined;
    return { value: { scope, ...(agentEmail ? { agentEmail } : {}) }, cacheable: true };
  } catch (err) {
    if (isUnauthorizedError(err)) {
      throw new InvalidBearerError();
    }
    // Fail-closed: least-privilege runtime tier, no default agent, not cached.
    return { value: { scope: "agent" }, cacheable: false };
  } finally {
    if (timer) clearTimeout(timer);
  }
}

function isUnauthorizedError(err: unknown): boolean {
  if (!err || typeof err !== "object") return false;
  const maybe = err as {
    status?: unknown;
    statusCode?: unknown;
    response?: { status?: unknown };
  };
  return (
    maybe.status === 401
    || maybe.statusCode === 401
    || maybe.response?.status === 401
  );
}

function extractBearer(req: Request): string | null {
  const auth = req.headers.authorization;
  if (!auth || typeof auth !== "string") return null;
  const m = /^Bearer\s+(.+)$/i.exec(auth);
  return m ? m[1]!.trim() : null;
}

function jsonRpcError(
  body: unknown,
  code: number,
  message: string,
  data?: Record<string, unknown>,
): Record<string, unknown> {
  // Preserve the request id when we can identify it; otherwise null.
  const id =
    body && typeof body === "object" && "id" in body
      ? (body as { id: unknown }).id
      : null;
  return { jsonrpc: "2.0", id, error: { code, message, ...(data ? { data } : {}) } };
}

/**
 * Start the HTTP server on `port`. Returns a closer that stops the listener —
 * wire it to SIGTERM in bin/http.ts. The server is stateless, so there are no
 * sessions to drain; closing the listener is the whole shutdown.
 *
 * NOTE on correlation: the request id is NOT forwarded to the backend on
 * whoami or tool calls — the @e2a/sdk client exposes no per-request header
 * hook (whoami goes through sdk.account.get(), which takes no options), and
 * this slice must not modify the SDK. If the SDK grows a header seam, thread
 * res.locals.requestId through buildClient/resolvePrincipal here.
 */
export async function startHttpServer(
  port: number,
  opts: HttpServerOptions,
): Promise<{ close: () => Promise<void>; port: number; metrics: MetricsRegistry }> {
  const { app, cache, metrics } = buildApp(opts);
  const server = app.listen(port);
  await new Promise<void>((resolve, reject) => {
    server.once("listening", resolve);
    server.once("error", reject);
  });
  const actualPort = (server.address() as { port: number }).port;
  return {
    port: actualPort,
    metrics,
    close: async () => {
      await new Promise<void>((resolve) => server.close(() => resolve()));
      cache.clear();
    },
  };
}
