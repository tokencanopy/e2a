import { loadEnv, type ProdEnv } from "./env.ts";
import { recordRequest } from "./coverage.ts";
import { recordTarget } from "./target.ts";

export interface RawResponse<T = unknown> {
  status: number;
  ok: boolean;
  headers: Record<string, string>;
  body: T | null;
  raw: string;
  latencyMs: number;
}

export interface RequestOpts {
  apiKey?: string | null;
  headers?: Record<string, string>;
  body?: unknown;
  query?: Record<string, string | number | undefined>;
  expect?: number | number[];
}

export class RateLimiter {
  private lastTickMs = 0;
  private readonly rps: number;
  constructor(rps: number) {
    this.rps = rps;
  }
  async tick(): Promise<void> {
    const interval = 1000 / this.rps;
    const now = Date.now();
    const wait = Math.max(0, this.lastTickMs + interval - now);
    if (wait > 0) await new Promise((r) => setTimeout(r, wait));
    this.lastTickMs = Date.now();
  }
}

export class ApiClient {
  readonly env: ProdEnv;
  readonly limiter: RateLimiter;
  private readonly baseUrl: string;

  constructor(env?: ProdEnv, rpsOverride?: number, baseUrlOverride?: string) {
    this.env = env ?? loadEnv();
    this.limiter = new RateLimiter(rpsOverride ?? this.env.rateLimitRps);
    this.baseUrl = baseUrlOverride ?? this.env.apiUrl;
    // Record which deployment this process's requests actually target — the
    // event/operation coverage gates read this to decide whether the
    // production-only allowlist entries are required or merely allowlisted.
    // See target.ts for why this must be derived here, not from a flag.
    recordTarget(this.env.apiUrl);
  }

  async request<T = unknown>(method: string, path: string, opts: RequestOpts = {}): Promise<RawResponse<T>> {
    await this.limiter.tick();
    const url = new URL(path, this.baseUrl);
    if (opts.query) {
      for (const [k, v] of Object.entries(opts.query)) {
        if (v !== undefined) url.searchParams.set(k, String(v));
      }
    }
    const headers: Record<string, string> = {
      Accept: "application/json",
      ...(opts.headers ?? {}),
    };
    const key = opts.apiKey === null ? null : opts.apiKey ?? this.env.apiKey;
    if (key) headers.Authorization = `Bearer ${key}`;
    let body: string | undefined;
    if (opts.body !== undefined) {
      body = typeof opts.body === "string" ? opts.body : JSON.stringify(opts.body);
      headers["Content-Type"] = headers["Content-Type"] ?? "application/json";
    }
    const t0 = performance.now();
    // redirect: "manual" stops fetch from transparently following 3xx
    // responses. The default `"follow"` makes tests assert against
    // whatever the redirect target replies — which silently defeats
    // CSRF-discipline checks like "/api/billing/checkout via GET must
    // be rejected" if the endpoint ever started returning 302 →
    // Stripe (fetch would follow to Stripe and the test would assert
    // against Stripe's response, not ours). We test API endpoints, so
    // a 3xx from any of our routes is a real signal that callers must
    // see, not transparently swallow.
    const res = await fetch(url, { method, headers, body, redirect: "manual" });
    // Record the exercised operation for the coverage gate — only on a 2xx (see
    // coverage.ts), so negative/rejected probes don't over-claim coverage.
    recordRequest(method, url.pathname, res.status);
    const raw = await res.text();
    const latencyMs = performance.now() - t0;
    let parsed: T | null = null;
    if (raw.length > 0) {
      try {
        parsed = JSON.parse(raw) as T;
      } catch {
        parsed = null;
      }
    }
    const out: RawResponse<T> = {
      status: res.status,
      ok: res.ok,
      headers: Object.fromEntries(res.headers.entries()),
      body: parsed,
      raw,
      latencyMs,
    };
    if (opts.expect !== undefined) {
      const expected = Array.isArray(opts.expect) ? opts.expect : [opts.expect];
      if (!expected.includes(res.status)) {
        throw new Error(
          `${method} ${url.pathname}: expected ${expected.join("|")}, got ${res.status}. Body: ${raw.slice(0, 400)}`,
        );
      }
    }
    return out;
  }

  get<T = unknown>(path: string, opts: RequestOpts = {}) {
    return this.request<T>("GET", path, opts);
  }
  post<T = unknown>(path: string, opts: RequestOpts = {}) {
    return this.request<T>("POST", path, opts);
  }
  patch<T = unknown>(path: string, opts: RequestOpts = {}) {
    return this.request<T>("PATCH", path, opts);
  }
  put<T = unknown>(path: string, opts: RequestOpts = {}) {
    return this.request<T>("PUT", path, opts);
  }
  delete<T = unknown>(path: string, opts: RequestOpts = {}) {
    return this.request<T>("DELETE", path, opts);
  }
}

export const defaultClient = new ApiClient();
