import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

export interface ProdEnv {
  apiUrl: string;
  siteUrl: string;
  apiKey: string;
  primaryAgentEmail: string;
  sinkEmail: string;
  sharedDomain: string;
  // Deployed streamable-HTTP MCP endpoint the MCP suites target. Defaults to
  // `${apiUrl}/mcp` (Caddy routes /mcp to the co-versioned mcp-server image);
  // override with E2A_MCP_URL for an out-of-band host (e.g. an in-cluster URL).
  mcpUrl: string;
  // Optional separate STANDARD-class, low-cap account for enforcement tests
  // (limit + rate-limit). Absent → the enforcement suite skips, since the main
  // conformance account is internal-class and by construction exempt.
  quotaApiKey?: string;
  quotaAgentEmail?: string;
  allowStress: boolean;
  cleanupMode: "always" | "on_success" | "never";
  rateLimitRps: number;
}

function normalizeBaseUrl(value: string): string {
  return value.trim().replace(/\/+$/, "");
}

export function resolveSiteUrl(apiUrl: string, explicitSiteUrl?: string): string {
  if (explicitSiteUrl?.trim()) return normalizeBaseUrl(explicitSiteUrl);

  const normalizedApiUrl = normalizeBaseUrl(apiUrl);
  const apiOrigin = new URL(normalizedApiUrl).origin;
  if (apiOrigin === "https://api.e2a.dev") return "https://e2a.dev";
  if (apiOrigin === "https://api-staging.e2a.dev") return "https://staging.e2a.dev";
  return normalizedApiUrl;
}

// The hosted PRODUCTION origins. This suite is destructive by construction — it
// creates and deletes agents, domains, webhooks and templates, and defaults to
// `E2E_CLEANUP=always` — so running it against these hosts must be a deliberate,
// explicit act, never something an unconfigured `npm test` can stumble into.
// Self-hosted deployments are deliberately NOT listed: their operators are the
// intended audience for an unguarded run against their own instance.
const PRODUCTION_HOSTS = new Set(["e2a.dev", "www.e2a.dev", "api.e2a.dev"]);

export function isProductionTarget(apiUrl: string): boolean {
  try {
    return PRODUCTION_HOSTS.has(new URL(normalizeBaseUrl(apiUrl)).hostname.toLowerCase());
  } catch {
    // An unparseable URL is not a production URL; loadEnv surfaces the real
    // error when it tries to use it.
    return false;
  }
}

// assertProductionOptIn fails closed when the resolved target is a hosted
// production origin and the operator has not explicitly opted in. Named and
// exported so the check is unit-testable without mutating process.env.
export function assertProductionOptIn(apiUrl: string, allowProd = process.env.E2E_ALLOW_PROD): void {
  if (!isProductionTarget(apiUrl)) return;
  if (allowProd === "1") return;
  throw new Error(
    `Refusing to run the destructive e2e-prod suite against production (${apiUrl}).\n` +
      `This suite creates and deletes agents, domains, webhooks and templates, and cleans up with E2E_CLEANUP=always.\n` +
      `If you really mean to target production, set E2E_ALLOW_PROD=1 — and use a dedicated conformance account, never a real one.\n` +
      `To target staging instead: E2A_URL=https://api-staging.e2a.dev`,
  );
}

export function resolveSinkEmail(explicitSinkEmail?: string): string {
  const sinkEmail = explicitSinkEmail?.trim();
  if (!sinkEmail) {
    throw new Error("E2E_SINK_EMAIL is required and must name a safe test sink; never use a real agent address");
  }
  return sinkEmail;
}

function readLocalConfig(): { api_key?: string; api_url?: string; agent_email?: string; shared_domain?: string } {
  try {
    const raw = readFileSync(join(homedir(), ".e2a", "config.json"), "utf-8");
    return JSON.parse(raw);
  } catch {
    return {};
  }
}

// pickEnv returns the first non-empty env var from `names`, with a
// one-shot stderr warning when a non-canonical (i.e. non-first) name
// is what actually carries the value. Mirrors the dual-read pattern
// in cli/src/config.ts and mcp/src/config.ts so all three surfaces
// drift on the same migration schedule.
const warned = new Set<string>();
function pickEnv(canonical: string, ...legacy: string[]): string | undefined {
  if (process.env[canonical]) return process.env[canonical];
  for (const name of legacy) {
    const v = process.env[name];
    if (v) {
      if (!warned.has(name)) {
        process.stderr.write(
          `[e2e-prod] ${name} is deprecated; rename it to ${canonical} (both names work today).\n`,
        );
        warned.add(name);
      }
      return v;
    }
  }
  return undefined;
}

export function loadEnv(): ProdEnv {
  const local = readLocalConfig();
  // No default target. This used to fall back to https://e2a.dev, which — combined
  // with the ~/.e2a/config.json api_key fallback below — meant an unconfigured
  // `npm test` ran the destructive suite against PRODUCTION using the operator's
  // own CLI credentials. The target is now always explicit.
  const apiUrl = pickEnv("E2A_URL", "E2A_API_URL") ?? local.api_url;
  if (!apiUrl) {
    throw new Error(
      "No target deployment. Set E2A_URL (e.g. https://api-staging.e2a.dev) or configure api_url in ~/.e2a/config.json.",
    );
  }
  assertProductionOptIn(apiUrl);
  const primaryAgentEmail = pickEnv("E2A_AGENT_EMAIL", "E2A_PRIMARY_AGENT") ?? local.agent_email ?? "";
  const env: ProdEnv = {
    apiUrl,
    siteUrl: resolveSiteUrl(apiUrl, process.env.E2A_SITE_URL),
    mcpUrl: "", // filled below once apiUrl is known
    apiKey: process.env.E2A_API_KEY ?? local.api_key ?? "",
    primaryAgentEmail,
    sinkEmail: resolveSinkEmail(process.env.E2E_SINK_EMAIL),
    sharedDomain: process.env.E2A_SHARED_DOMAIN ?? local.shared_domain ?? "agents.e2a.dev",
    quotaApiKey: process.env.E2A_QUOTA_API_KEY || undefined,
    quotaAgentEmail: process.env.E2A_QUOTA_AGENT_EMAIL || undefined,
    allowStress: process.env.E2E_PROD_STRESS === "1",
    cleanupMode: (process.env.E2E_CLEANUP as ProdEnv["cleanupMode"]) ?? "always",
    rateLimitRps: Number(process.env.E2E_RPS ?? "1"),
  };
  // Default the MCP endpoint to the API host's /mcp route; E2A_MCP_URL overrides.
  env.mcpUrl = process.env.E2A_MCP_URL || new URL("/mcp", env.apiUrl).toString();
  if (!env.apiKey) {
    throw new Error("No API key found. Set E2A_API_KEY or run `e2a login` first.");
  }
  if (!env.primaryAgentEmail) {
    throw new Error("No primary agent email. Set E2A_AGENT_EMAIL.");
  }
  return env;
}
