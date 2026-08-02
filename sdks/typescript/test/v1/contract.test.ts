/**
 * TypeScript contract-test runner for scenarios.yaml.
 *
 * Runs against a live test server. Requires env vars:
 *   E2A_TEST_BASE_URL  — test server URL (e.g. http://localhost:8080)
 *   E2A_TEST_API_KEY   — valid API key for the test user
 *
 * The runner drives the server over raw HTTP (a thin scenario interpreter,
 * not the ergonomic client) plus {@link WSListener} for WebSocket steps.
 *
 * Setup steps that require direct store access (inject_message,
 * verify_domain as setup) cause the scenario to be skipped.
 *
 * NOTE: scenario `path`s are repointed from `/api/v1` to `/v1` as part of the
 * cross-language scenarios.yaml migration (tracked separately); this runner is
 * gated behind live-server env vars and is not part of the unit build.
 */
import { describe, it, expect, afterAll, vi } from "vitest";
import { Buffer } from "node:buffer";
import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve as pathResolve } from "node:path";
import { parse as yamlParse } from "yaml";
import { WSListener } from "../../src/v1/ws.js";
import { Seeder, seedEnabled, sweepLeaks } from "./seed.js";
import { ObjectSerializer } from "../../src/v1/generated/models/ObjectSerializer.js";
import type { PageMessageLifecycleTransition } from "../../src/v1/index.js";

// When the isolated CF zone + SMTP port are configured, store-dependent scenarios
// are SEEDED over the API (verified domains + real inbound) instead of skipped.
const SEED = seedEnabled();

it("parses the generated message lifecycle page contract", () => {
  const page = ObjectSerializer.deserialize(
    {
      items: [
        {
          id: "mlt_1",
          message_id: "msg_1",
          direction: "outbound",
          recipient: null,
          stage: "accepted",
          outcome: "accepted",
          reason_code: "acceptance.outbound_api",
          retryable: false,
          evidence: { source: "api", nested: { future: true } },
          correlation_ids: { request_id: "req_1", future_id: "future_1" },
          occurred_at: "2026-07-22T00:00:00Z",
          reconstructed: false,
        },
        {
          id: "mlt_recon_2",
          message_id: "msg_1",
          direction: "outbound",
          stage: "delivery",
          outcome: "delivered",
          reason_code: "delivery.recipient_server_accepted",
          retryable: false,
          evidence: {},
          correlation_ids: {},
          occurred_at: "2026-07-22T01:00:00Z",
          reconstructed: true,
        },
      ],
      next_cursor: null,
    },
    "PageMessageLifecycleTransition",
    "",
  ) as PageMessageLifecycleTransition;

  expect(page.items[0].recipient).toBeNull();
  expect(page.items[0].evidence.nested).toEqual({ future: true });
  expect(page.items[0].correlationIds.future_id).toBe("future_1");
  expect(page.items[1].recipient).toBeUndefined();
  expect(page.items[1].reconstructed).toBe(true);
  expect(page.items[1].reasonCode).toBe("delivery.recipient_server_accepted");
});

it("keeps the managed-unsubscribe scenario self-cleaning and lifecycle-observable", () => {
  const scenario = loadScenarios().find(
    (candidate) => candidate.name === "agent_suppression_and_managed_unsubscribe",
  );
  expect(scenario).toBeDefined();

  const held = scenario!.steps.find((step) => step.id === "managed_unsubscribe_send_held");
  expect(held?.capture).toEqual({ managed_message_id: "message_id" });

  const lifecycle = scenario!.steps.find((step) => step.id === "get_managed_message_lifecycle");
  expect(lifecycle?.path).toContain("{managed_message_id}/lifecycle");
  expect(lifecycle?.expect?.body_array_contains).toEqual({
    items: {
      message_id: "{managed_message_id}",
      direction: "outbound",
      stage: "review",
      outcome: "pending",
      reason_code: "review.hold_created",
      retryable: false,
      reconstructed: false,
    },
  });

  expect(scenario!.steps.map((step) => step.id)).not.toContain("delete_agent_permanently");
  expect(scenario!.cleanup?.map((step) => step.id)).toEqual([
    "delete_agent_permanently",
    "delete_domain",
  ]);
});

it("keeps the scheduled-send scenario self-cleaning and projection-complete", () => {
  const scenario = loadScenarios().find(
    (candidate) => candidate.name === "scheduled_send_fields",
  );
  expect(scenario).toBeDefined();
  expect(scenario!.setup?.[0]?.register_agent?.email).toBe(
    "scheduled-contract-{scenario_token}@agents.e2a.dev",
  );

  const steps = new Map(scenario!.steps.map((step) => [step.id, step]));
  const send = steps.get("schedule_send");
  expect(send?.body?.send_at).toBe("{future_rfc3339}");
  expect(send?.expect?.body_match).toEqual({
    status: "scheduled",
    scheduled_at: "{future_rfc3339}",
  });
  expect(send?.capture).toEqual({ scheduled_message_id: "message_id" });

  expect(steps.get("get_scheduled_message")?.expect?.body_match).toEqual({
    id: "{scheduled_message_id}",
    delivery_status: "accepted",
    scheduled_at: "{future_rfc3339}",
  });
  expect(steps.get("list_scheduled_message")?.expect?.body_array_contains).toEqual({
    items: {
      id: "{scheduled_message_id}",
      delivery_status: "accepted",
      scheduled_at: "{future_rfc3339}",
    },
  });
  expect(steps.get("trash_scheduled_message")?.expect?.body_match).toEqual({
    deleted: true,
    id: "{scheduled_message_id}",
  });
  expect(steps.get("restore_before_scheduled_at")?.expect?.body_match).toEqual({
    id: "{scheduled_message_id}",
    delivery_status: "accepted",
    scheduled_at: "{future_rfc3339}",
  });
  expect(scenario!.cleanup?.map((step) => step.id)).toEqual([
    "delete_scheduled_agent_permanently",
  ]);
});

it("runs cleanup in order after a primary failure without masking it", async () => {
  const paths: string[] = [];
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const path = new URL(String(input)).pathname;
    paths.push(path);
    if (path === "/fail") return new Response("primary", { status: 500 });
    if (path === "/cleanup-agent") return new Response("cleanup", { status: 500 });
    return new Response("", { status: 200 });
  });
  const scenario = {
    name: "failure_cleanup",
    description: "cleanup survives primary and cleanup failures",
    steps: [{ id: "fail", action: "request", method: "GET", path: "/fail", expect: { status: 200 } }],
    cleanup: [
      { id: "cleanup_agent", action: "request", method: "DELETE", path: "/cleanup-agent", expect: { status: 200 } },
      { id: "cleanup_domain", action: "request", method: "DELETE", path: "/cleanup-domain", expect: { status: 200 } },
    ],
  } satisfies Scenario;

  try {
    await expect(executeRunner(new Runner("https://contract.test", "key", scenario))).rejects.toThrow(
      /step fail: status/,
    );
    expect(paths).toEqual(["/fail", "/cleanup-agent", "/cleanup-domain"]);
  } finally {
    fetchMock.mockRestore();
  }
});

it("resolves future_rfc3339 once per scenario to a future UTC instant", () => {
  const before = Date.now();
  const runner = new Runner("https://contract.test", "key", {
    name: "future_timestamp",
    description: "dynamic timestamp parity",
    steps: [],
  });

  const first = runner.resolve("{future_rfc3339}");
  const second = runner.resolve("{future_rfc3339}");
  const parsed = Date.parse(first);

  expect(second).toBe(first);
  expect(Number.isNaN(parsed)).toBe(false);
  expect(parsed).toBeGreaterThanOrEqual(before + 4 * 60_000);
  expect(parsed).toBeLessThanOrEqual(Date.now() + 6 * 60_000);
  expect(first).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
});

it("assigns a unique lowercase-hex token to each scenario", () => {
  const first = new Runner("https://contract.test", "key", {
    name: "first",
    description: "unique dynamic placeholder",
    steps: [],
  });
  const second = new Runner("https://contract.test", "key", {
    name: "second",
    description: "unique dynamic placeholder",
    steps: [],
  });

  const firstToken = first.resolve("{scenario_token}");
  expect(firstToken).toMatch(/^[0-9a-f]{12}$/);
  expect(second.resolve("{scenario_token}")).not.toBe(firstToken);
});

it("surfaces cleanup failure when the scenario itself succeeds", async () => {
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response("cleanup", { status: 500 }),
  );
  const scenario = {
    name: "cleanup_failure",
    description: "cleanup failure is visible",
    steps: [],
    cleanup: [
      { id: "cleanup", action: "request", method: "DELETE", path: "/cleanup", expect: { status: 200 } },
    ],
  } satisfies Scenario;

  try {
    await expect(executeRunner(new Runner("https://contract.test", "key", scenario))).rejects.toThrow(
      /contract scenario cleanup failed/,
    );
  } finally {
    fetchMock.mockRestore();
  }
});

// ── Raw-bytes escape hatches (raw_body_base64 / headers_base64) ──
//
// Runner-level regressions, mirrored in tests/contract/runner_rawbytes_test.go
// and sdks/python/tests/test_contract.py. The extension exists because YAML is
// UTF-8 by definition and its \xNN escape denotes a CODEPOINT (\xFF is U+00FF →
// the perfectly valid two-byte C3 BF), so no scenario scalar can carry the
// ill-formed bytes the invalid_utf8_rejected scenario needs on the wire.

const RAW_INVALID_BODY = Buffer.concat([
  Buffer.from('{"address":"a'),
  Buffer.from([0xff]),
  Buffer.from('b@example.com"}'),
]);
const RAW_INVALID_HEADER = Buffer.from([0x6b, 0xff, 0x7a]); // k\xFFz

function isValidUtf8(bytes: Uint8Array): boolean {
  try {
    new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    return true;
  } catch {
    return false;
  }
}

it("puts raw_body_base64 and headers_base64 bytes on the wire verbatim", async () => {
  // A real socket, not a fetch mock: the whole question is whether undici's
  // header/body writer preserves bytes >= 0x80, which only the wire can answer.
  const { createServer } = await import("node:http");
  let gotBody = Buffer.alloc(0);
  let gotHeader = "";
  let gotContentType = "";
  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      gotBody = Buffer.concat(chunks);
      // Node parses header values as latin1, so this string's code units ARE
      // the received bytes.
      gotHeader = String(req.headers["idempotency-key"] ?? "");
      gotContentType = String(req.headers["content-type"] ?? "");
      res.writeHead(400, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: { code: "invalid_request" } }));
    });
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
  const port = (server.address() as { port: number }).port;

  const scenario = {
    name: "raw_bytes",
    description: "raw bytes reach the wire unmodified",
    steps: [
      {
        id: "raw",
        action: "request",
        method: "POST",
        path: "/v1/contacts",
        raw_body_base64: RAW_INVALID_BODY.toString("base64"),
        headers_base64: { "Idempotency-Key": RAW_INVALID_HEADER.toString("base64") },
        expect: { status: 400, body_match: { "error.code": "invalid_request" } },
      },
    ],
  } satisfies Scenario;

  try {
    await executeRunner(new Runner(`http://127.0.0.1:${port}`, "key", scenario));
  } finally {
    await new Promise<void>((r) => server.close(() => r()));
  }

  expect(Buffer.compare(gotBody, RAW_INVALID_BODY)).toBe(0);
  expect(Buffer.compare(Buffer.from(gotHeader, "latin1"), RAW_INVALID_HEADER)).toBe(0);
  expect(gotContentType).toBe("application/json");
  // Non-vacuity: the bytes that arrived must genuinely be ill-formed UTF-8,
  // otherwise the transport silently "fixed" them and the test proves nothing.
  expect(isValidUtf8(gotBody)).toBe(false);
  expect(isValidUtf8(Buffer.from(gotHeader, "latin1"))).toBe(false);
});

it("rejects a step declaring both body and raw_body_base64", () => {
  expect(() =>
    stepRawBody({
      id: "both",
      action: "request",
      body: { address: "a@example.com" },
      raw_body_base64: Buffer.from("{}").toString("base64"),
    }),
  ).toThrow(/mutually exclusive/);
});

it("asserts error-response bodies instead of skipping them on a 4xx", async () => {
  // Regression for the fork this runner used to have: non-2xx responses threw
  // out of RawApi.raw, so only the status was ever checked and every
  // body_match on an error envelope passed vacuously.
  const fetchMock = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValue(
      new Response(JSON.stringify({ error: { code: "something_else" } }), { status: 400 }),
    );
  const scenario = {
    name: "error_body",
    description: "error envelopes are asserted",
    steps: [
      {
        id: "rejected",
        action: "request",
        method: "GET",
        path: "/v1/webhooks/%FF",
        expect: { status: 400, body_match: { "error.code": "invalid_request" } },
      },
    ],
  } satisfies Scenario;
  try {
    await expect(executeRunner(new Runner("https://contract.test", "key", scenario))).rejects.toThrow(
      /body_match error\.code/,
    );
  } finally {
    fetchMock.mockRestore();
  }
});

it("keeps the invalid-UTF-8 scenario carrying genuinely ill-formed bytes", () => {
  const scenario = loadScenarios().find((c) => c.name === "invalid_utf8_rejected");
  expect(scenario).toBeDefined();
  const steps = new Map(scenario!.steps.map((step) => [step.id, step]));

  const body = stepRawBody(steps.get("body_rejected")!);
  expect(body, "body_rejected must declare raw_body_base64").toBeDefined();
  expect(isValidUtf8(new Uint8Array(body!)), "body_rejected payload must be ill-formed UTF-8").toBe(
    false,
  );

  const headerB64 = steps.get("header_rejected")!.headers_base64?.["Idempotency-Key"];
  expect(headerB64, "header_rejected must declare headers_base64").toBeDefined();
  expect(
    isValidUtf8(Buffer.from(headerB64!, "base64")),
    "header_rejected value must be ill-formed UTF-8",
  ).toBe(false);

  // Self-cleaning by construction: every step is a rejection (400) or a
  // not-created probe (404), so nothing exists to delete.
  expect(scenario!.cleanup).toBeUndefined();
  const statuses = [...new Set(scenario!.steps.map((step) => step.expect?.status))].sort();
  expect(statuses, "every step must be a 400 rejection or a 404 not-created probe").toEqual([
    400, 404,
  ]);
});

// Minimal raw-HTTP driver — the scenario runner needs a generic
// request(method, path, body) shim, not the ergonomic client surface.
class RawApiError extends Error {
  constructor(public readonly statusCode: number, message: string) {
    super(message);
    this.name = "RawApiError";
  }
}

class RawApi {
  constructor(
    private readonly apiKey: string,
    private readonly baseUrl: string,
  ) {}

  async raw(method: string, path: string, body?: unknown): Promise<Response> {
    const headers: Record<string, string> = { Authorization: `Bearer ${this.apiKey}` };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const resp = await fetch(`${this.baseUrl}${path}`, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    if (resp.status >= 400) throw new RawApiError(resp.status, await resp.text());
    return resp;
  }

  async registerDomain(input: { domain: string }): Promise<void> {
    await this.raw("POST", "/v1/domains", input);
  }

  async registerAgent(input: { email: string }): Promise<void> {
    await this.raw("POST", "/v1/agents", { email: input.email });
  }
}

// ── YAML schema ─────────────────────────────────────────────────

interface Scenario {
  name: string;
  description: string;
  auth_override?: string;
  setup?: SetupStep[];
  steps: Step[];
  cleanup?: Step[];
}

interface SetupStep {
  register_domain?: string;
  verify_domain?: string;
  register_agent?: { email: string };
  inject_message?: { agent_email: string; from: string; subject: string };
}

interface Step {
  id: string;
  action: string;
  method?: string;
  path?: string;
  body?: Record<string, unknown>;
  /** Raw request-body bytes, base64. Mutually exclusive with `body`; sent
   *  VERBATIM (never through JSON.stringify) so a scenario can transmit bytes
   *  YAML cannot spell — see the scenarios.yaml header. */
  raw_body_base64?: string;
  /** Raw request-header values, base64 per header name. Also verbatim. */
  headers_base64?: Record<string, string>;
  auth_override?: string;
  agent_email?: string;
  from?: string;
  subject?: string;
  verify_domain?: string;
  expect?: Expectation;
  /** After assertions, extract response json-paths into vars (Go-runner parity):
   *  { var_name: "json.path" } → resolvable later as {var_name}. */
  capture?: Record<string, string>;
}

interface Expectation {
  status?: number;
  body_contains?: string[];
  body_excludes?: string[];
  body_match?: Record<string, unknown>;
  body_array_contains?: Record<string, Record<string, unknown>>;
  fields_present?: string[];
  fields_absent?: string[];
  field_match?: Record<string, unknown>;
}

// ── Helpers ─────────────────────────────────────────────────────

function loadScenarios(): Scenario[] {
  const yamlPath = pathResolve(
    import.meta.dirname ?? ".",
    "../../../../tests/contract/scenarios.yaml",
  );
  const raw = readFileSync(yamlPath, "utf-8");
  const parsed = yamlParse(raw);
  return parsed.scenarios;
}

/** JSON-path evaluator: "agents[0].email", "agents.length" */
function jsonPathGet(obj: Record<string, unknown>, path: string): unknown {
  const parts = path.split(".");
  let current: unknown = obj;

  for (const part of parts) {
    if (part === "length") {
      return Array.isArray(current) ? current.length : undefined;
    }
    const bracketIdx = part.indexOf("[");
    if (bracketIdx !== -1) {
      const name = part.slice(0, bracketIdx);
      const arrIdx = parseInt(part.slice(bracketIdx + 1, -1), 10);
      const map = current as Record<string, unknown>;
      const arr = map?.[name];
      if (!Array.isArray(arr) || arrIdx >= arr.length) return undefined;
      current = arr[arrIdx];
    } else {
      const map = current as Record<string, unknown>;
      if (map == null || typeof map !== "object") return undefined;
      current = map[part];
    }
  }
  return current;
}

/** Cross-type value comparison (JSON number vs YAML int). */
function valuesEqual(jsonVal: unknown, yamlVal: unknown): boolean {
  if (typeof yamlVal === "number" && typeof jsonVal === "number") {
    return jsonVal === yamlVal;
  }
  if (typeof yamlVal === "boolean") return jsonVal === yamlVal;
  if (typeof yamlVal === "string") return jsonVal === yamlVal;
  return String(jsonVal) === String(yamlVal);
}

/** Decode a step's raw_body_base64, enforcing one body source per step.
 *  Returns an exact-length ArrayBuffer — the one BodyInit form fetch takes
 *  without any encoding step of its own. */
function stepRawBody(step: Step): ArrayBuffer | undefined {
  if (step.raw_body_base64 === undefined) return undefined;
  if (step.body !== undefined) {
    throw new Error(`step ${step.id}: body and raw_body_base64 are mutually exclusive`);
  }
  const decoded = Buffer.from(step.raw_body_base64, "base64");
  const out = new ArrayBuffer(decoded.byteLength);
  new Uint8Array(out).set(decoded);
  return out;
}

/**
 * Decode a base64 header value into the form undici puts on the wire
 * UNCHANGED. Node's HTTP writer serializes header values as latin1, so a
 * JS string whose code units are the raw bytes (Buffer#toString("latin1"))
 * is byte-exact on the wire — verified against a raw socket:
 * `Idempotency-Key: k\xffz`. A UTF-8 decode would silently turn 0xFF into
 * U+FFFD (or a two-byte C3 BF), which is exactly the laundering these
 * scenarios exist to catch.
 */
function decodeHeaderBytes(base64Value: string): string {
  return Buffer.from(base64Value, "base64").toString("latin1");
}

/** Extract agent email from a WS path like /v1/agents/bot@ws.test.dev/ws */
function extractEmailFromWSPath(path: string): string {
  const parts = path.replace(/^\/+/, "").split("/");
  for (let i = 0; i < parts.length; i++) {
    if (parts[i] === "agents" && i + 1 < parts.length) return parts[i + 1];
  }
  return "";
}

// ── Runner ──────────────────────────────────────────────────────

class Runner {
  private vars: Record<string, string> = {};
  private api: RawApi;
  private wsListener: WSListener | null = null;
  /** Buffer of notifications received from WSListener. */
  private wsMessages: string[] = [];
  private seeder: Seeder | null;
  /** Placeholder test domain → real minted+verified domain (seed mode only). */
  private domainSubs: Record<string, string> = {};

  constructor(
    private readonly baseUrl: string,
    private readonly apiKey: string,
    private readonly scenario: Scenario,
  ) {
    // One stable instant per scenario: request bodies and later expectations
    // must resolve to the exact same value, while never aging into the past.
    const future = new Date(Date.now() + 5 * 60_000);
    future.setUTCMilliseconds(0);
    this.vars.future_rfc3339 = future.toISOString().replace(".000Z", "Z");
    this.vars.scenario_token = randomBytes(6).toString("hex");
    this.api = new RawApi(apiKey, baseUrl);
    this.seeder = SEED ? new Seeder(baseUrl, apiKey) : null;
  }

  resolve(s: string): string {
    s = s.replaceAll("{base_url}", this.baseUrl);
    s = s.replaceAll("{api_key}", this.apiKey);
    for (const [k, v] of Object.entries(this.vars)) {
      s = s.replaceAll(`{${k}}`, v);
    }
    // In seed mode, scenarios' hardcoded placeholder domains (crud.test.dev, …)
    // are rewritten to the real minted+verified domains everywhere they appear
    // (bodies, paths, body_match expectations). Longest placeholder first so a
    // shorter one can't corrupt a prefix-sharing longer one (ws.test.dev vs
    // wsauth.test.dev) — latent today (one domain per scenario), cheap to anchor.
    for (const placeholder of Object.keys(this.domainSubs).sort((a, b) => b.length - a.length)) {
      s = s.replaceAll(placeholder, this.domainSubs[placeholder]);
    }
    return s;
  }

  resolveValue(v: unknown): unknown {
    if (typeof v === "string") return this.resolve(v);
    if (Array.isArray(v)) return v.map((item) => this.resolveValue(item));
    if (v && typeof v === "object") {
      const out: Record<string, unknown> = {};
      for (const [k, val] of Object.entries(v)) {
        out[k] = this.resolveValue(val);
      }
      return out;
    }
    return v;
  }

  /** Returns the auth override for a step, or undefined for default SDK auth. */
  private authOverride(step: Step): string | undefined {
    return step.auth_override ?? this.scenario.auth_override;
  }

  /** Runs setup. Returns true if setup needs store access we DON'T have (→ skip).
   *  In seed mode, store-dependent setup (verify_domain, inject_message) is
   *  satisfied over the API instead, and this always returns false. */
  async executeSetup(): Promise<boolean> {
    if (!this.scenario.setup) return false;

    for (const s of this.scenario.setup) {
      if (s.register_domain) {
        const placeholder = s.register_domain;
        if (this.seeder) {
          // Mint + register a real domain (not verified yet); rewrite the placeholder.
          this.domainSubs[placeholder] = await this.seeder.registerDomain(placeholder.split(".")[0]);
        } else {
          try {
            await this.api.registerDomain({ domain: this.resolve(placeholder) });
          } catch (err) {
            if (!(err instanceof RawApiError && err.statusCode === 409)) throw err;
          }
        }
        continue;
      }

      if (s.verify_domain) {
        if (!this.seeder) return true; // no store access → skip the scenario
        const placeholder = s.verify_domain;
        let real = this.domainSubs[placeholder];
        if (!real) {
          real = await this.seeder.registerDomain(placeholder.split(".")[0]);
          this.domainSubs[placeholder] = real;
        }
        await this.seeder.verifyDomain(real);
        continue;
      }

      if (s.register_agent) {
        const email = this.resolve(s.register_agent.email); // domainSubs applied
        try {
          await this.api.registerAgent({ email });
        } catch (err) {
          if (!(err instanceof RawApiError && err.statusCode === 409)) throw err;
        }
        this.vars["agent_email"] = email;
        continue;
      }

      if (s.inject_message) {
        if (!this.seeder) return true; // no store access → skip the scenario
        const im = s.inject_message;
        this.vars["injected_message_id"] = await this.seeder.injectMessage(
          this.resolve(im.agent_email),
          this.resolve(im.from),
          this.resolve(im.subject),
        );
        continue;
      }
    }
    return false;
  }

  async executeSteps(): Promise<void> {
    for (const step of this.scenario.steps) {
      switch (step.action) {
        case "request":
          await this.execRequest(step);
          break;
        case "inject_message":
          if (!this.seeder) throw new Error(`step ${step.id}: inject_message needs seeding (set the CF zone + SMTP env)`);
          this.vars["injected_message_id"] = await this.seeder.injectMessage(
            this.resolve(step.agent_email!),
            this.resolve(step.from!),
            this.resolve(step.subject!),
          );
          break;
        case "ws_connect":
          await this.execWSConnect(step);
          break;
        case "ws_reconnect":
          await this.execWSReconnect(step);
          break;
        case "ws_read":
          await this.execWSRead(step);
          break;
        case "verify_and_retry":
          if (!this.seeder) throw new Error(`step ${step.id}: verify_and_retry needs seeding (set the CF zone env)`);
          await this.execVerifyAndRetry(step);
          break;
        default:
          throw new Error(`step ${step.id}: unknown action ${step.action}`);
      }
    }
  }

  async cleanup(): Promise<void> {
    const errors: unknown[] = [];
    if (this.wsListener) {
      this.wsListener.close();
      this.wsListener = null;
    }
    for (const step of this.scenario.cleanup ?? []) {
      try {
        if (step.action !== "request") throw new Error(`cleanup step ${step.id}: only request actions are supported`);
        await this.execRequest(step);
      } catch (err) {
        errors.push(err);
      }
    }
    if (this.seeder) {
      try {
        await this.seeder.cleanup();
      } catch (err) {
        errors.push(err);
      }
    }
    if (errors.length) throw new AggregateError(errors, "contract scenario cleanup failed");
  }

  /** verify_and_retry: verify the (registered) domain, then run the request. */
  private async execVerifyAndRetry(step: Step): Promise<void> {
    const placeholder = step.verify_domain!;
    const real = this.domainSubs[placeholder];
    if (!real) throw new Error(`step ${step.id}: ${placeholder} was not registered in setup`);
    await this.seeder!.verifyDomain(real);
    await this.execRequest(step);
  }

  // ── Step executors ──────────────────────────────────────────

  private async execRequest(step: Step): Promise<void> {
    const path = this.resolve(step.path!);
    const jsonBody = step.body !== undefined ? this.resolveValue(step.body) : undefined;
    const rawBodyBytes = stepRawBody(step);
    const ex = step.expect;

    // One request path for every step (Go/Python-runner parity). It used to
    // fork: auth-override steps did their own fetch, everything else went
    // through RawApi.raw — which THROWS on >= 400, so the catch could only
    // check the status and had to return, silently skipping every body
    // assertion on an error response. Scenarios that assert an error envelope
    // (this file has many) were vacuous in TypeScript alone.
    const headers: Record<string, string> = {};
    const override = this.authOverride(step);
    if (override === undefined) {
      headers["Authorization"] = `Bearer ${this.apiKey}`;
    } else if (override !== "none") {
      headers["Authorization"] = this.resolve(override);
    }
    if (jsonBody !== undefined || rawBodyBytes !== undefined) {
      headers["Content-Type"] = "application/json";
    }
    for (const [name, encoded] of Object.entries(step.headers_base64 ?? {})) {
      headers[name] = decodeHeaderBytes(encoded);
    }

    const resp = await fetch(`${this.baseUrl}${path}`, {
      method: step.method,
      headers,
      // rawBodyBytes goes out untouched; only the YAML `body` is serialized.
      body: rawBodyBytes ?? (jsonBody !== undefined ? JSON.stringify(jsonBody) : undefined),
    });
    const status = resp.status;
    const rawBody = await resp.text();

    if (ex?.status) {
      expect(status, `step ${step.id}: status`).toBe(ex.status);
    }

    const hasBodyChecks = Boolean(
      ex?.body_contains?.length || ex?.body_match || ex?.body_excludes?.length || ex?.body_array_contains,
    );
    const hasCapture = Boolean(step.capture && Object.keys(step.capture).length);
    if (!hasBodyChecks && !hasCapture) return;

    const json = JSON.parse(rawBody) as Record<string, unknown>;

    for (const key of ex?.body_contains ?? []) {
      const resolved = this.resolve(key);
      expect(json, `step ${step.id}: body_contains ${resolved}`).toHaveProperty(resolved);
    }
    for (const key of ex?.body_excludes ?? []) {
      const resolved = this.resolve(key);
      expect(json, `step ${step.id}: body_excludes ${resolved}`).not.toHaveProperty(resolved);
    }
    if (ex?.body_match) {
      for (const [jsonPath, expected] of Object.entries(ex.body_match)) {
        const resolvedPath = this.resolve(jsonPath);
        const actual = jsonPathGet(json, resolvedPath);
        const resolvedExpected = this.resolveValue(expected);
        expect(
          valuesEqual(actual, resolvedExpected),
          `step ${step.id}: body_match ${resolvedPath} = ${JSON.stringify(actual)}, want ${JSON.stringify(resolvedExpected)}`,
        ).toBe(true);
      }
    }
    for (const [jsonPath, expectedFields] of Object.entries(ex?.body_array_contains ?? {})) {
      const resolvedPath = this.resolve(jsonPath);
      const items = jsonPathGet(json, resolvedPath);
      expect(Array.isArray(items), `step ${step.id}: body_array_contains ${resolvedPath} is an array`).toBe(true);
      const resolvedFields = this.resolveValue(expectedFields) as Record<string, unknown>;
      const found = (items as unknown[]).some((item) =>
        item !== null && typeof item === "object" && Object.entries(resolvedFields).every(
          ([field, expected]) => valuesEqual(jsonPathGet(item as Record<string, unknown>, field), expected),
        ),
      );
      expect(found, `step ${step.id}: body_array_contains ${resolvedPath} has a matching item`).toBe(true);
    }

    // Capture phase — AFTER assertions (Go-runner parity): extract a response
    // json-path into a var so later steps can reference it as {name}.
    for (const [name, srcPath] of Object.entries(step.capture ?? {})) {
      const val = jsonPathGet(json, this.resolve(srcPath));
      if (val === undefined) throw new Error(`step ${step.id}: capture path ${srcPath} not found in response`);
      this.vars[name] = String(val);
    }
  }

  private async execWSConnect(step: Step): Promise<void> {
    const path = this.resolve(step.path!);
    const email = extractEmailFromWSPath(path);

    this.wsMessages = [];
    const listener = new WSListener({
      apiKey: this.apiKey,
      agentEmail: email,
      baseUrl: this.baseUrl,
      reconnect: false,
    });

    listener.on("event", (notif) => {
      this.wsMessages.push(JSON.stringify(notif));
    });

    await new Promise<void>((resolve, reject) => {
      listener.on("open", resolve);
      listener.on("error", reject);
      listener.connect();
    });

    this.wsListener = listener;
  }

  private async execWSReconnect(step: Step): Promise<void> {
    if (this.wsListener) {
      this.wsListener.close();
      this.wsListener = null;
      // Brief pause for server to process disconnect
      await new Promise((r) => setTimeout(r, 100));
    }
    await this.execWSConnect(step);
  }

  private async execWSRead(step: Step): Promise<void> {
    if (!this.wsListener) throw new Error(`step ${step.id}: no WS connection`);

    // Wait for a buffered or incoming notification.
    let raw: string;
    if (this.wsMessages.length > 0) {
      raw = this.wsMessages.shift()!;
    } else {
      raw = await new Promise<string>((resolve, reject) => {
        const timeout = setTimeout(
          () => reject(new Error(`step ${step.id}: ws_read timeout`)),
          5000,
        );
        const handler = (notif: unknown) => {
          clearTimeout(timeout);
          resolve(JSON.stringify(notif));
        };
        this.wsListener!.once("event", handler);
      });
    }

    const notif = JSON.parse(raw) as Record<string, unknown>;
    const ex = step.expect;
    if (!ex) return;

    for (const field of ex.fields_present ?? []) {
      const resolved = this.resolve(field);
      expect(notif, `step ${step.id}: fields_present ${resolved}`).toHaveProperty(resolved);
    }
    for (const field of ex.fields_absent ?? []) {
      const resolved = this.resolve(field);
      expect(notif, `step ${step.id}: fields_absent ${resolved}`).not.toHaveProperty(resolved);
    }
    if (ex.field_match) {
      for (const [key, expected] of Object.entries(ex.field_match)) {
        const resolvedKey = this.resolve(key);
        const resolvedExpected = this.resolveValue(expected);
        // Dot-path lookup ("data.message_id") — WS frames are the versioned
        // event envelope, so scenario assertions address into `data`.
        const actual = resolvedKey
          .split(".")
          .reduce<unknown>((acc, part) => (acc as Record<string, unknown> | undefined)?.[part], notif);
        expect(
          valuesEqual(actual, resolvedExpected),
          `step ${step.id}: field_match ${resolvedKey} = ${JSON.stringify(actual)}, want ${JSON.stringify(resolvedExpected)}`,
        ).toBe(true);
      }
    }
  }
}

async function executeRunner(runner: Runner): Promise<void> {
  let primary: unknown;
  try {
    const skipped = await runner.executeSetup();
    if (!skipped) await runner.executeSteps();
  } catch (err) {
    primary = err;
  }
  try {
    await runner.cleanup();
  } catch (cleanupError) {
    if (primary === undefined) throw cleanupError;
  }
  if (primary !== undefined) throw primary;
}

// ── Scenarios that require store-level setup ────────────────────

const STORE_DEPENDENT_ACTIONS = new Set(["inject_message", "verify_and_retry"]);

function scenarioNeedsStore(sc: Scenario): boolean {
  if (sc.setup?.some((s) => s.inject_message || s.verify_domain)) return true;
  if (sc.steps.some((s) => STORE_DEPENDENT_ACTIONS.has(s.action))) return true;
  return false;
}

// ── Test entry point ────────────────────────────────────────────

const baseUrl = process.env.E2A_TEST_BASE_URL;
const apiKey = process.env.E2A_TEST_API_KEY;

// These scenarios assert account-GLOBAL collection state, which assumes an
// isolated/empty account — not the shared staging conformance account (it holds
// the shared domain + concurrent seeded domains + other suites' fresh agents):
//   agent_crud            — items.length:0 after delete
//   domain_crud           — items.length:1 on /v1/domains
//   placeholder_resolution — items[0].email == {agent_email} (i.e. "my agent is
//                            the account-newest"; races with concurrent suites)
// Their behavior is covered by the resource-scoped e2e-prod suites; the Go/local
// runner still covers them against a fresh DB. Skip regardless of seeding.
const ACCOUNT_GLOBAL = new Set(["agent_crud", "domain_crud", "placeholder_resolution"]);

describe.skipIf(!baseUrl || !apiKey)("Contract scenarios", () => {
  const scenarios = loadScenarios();

  for (const sc of scenarios) {
    // Store-dependent scenarios run when SEED supplies their preconditions over
    // the API; otherwise they skip. Account-global scenarios skip regardless.
    const skip = ACCOUNT_GLOBAL.has(sc.name) || (scenarioNeedsStore(sc) && !SEED);

    (skip ? it.skip : it)(
      sc.name,
      async () => {
        await executeRunner(new Runner(baseUrl!, apiKey!, sc));
      },
      // Seeding mints + CF-verifies a real domain (variable DNS propagation), so
      // seeded scenarios need a generous budget with headroom over the worst-case
      // ~270s (register + propagate + verify); non-seeded stay snappy.
      SEED ? 420_000 : 20_000,
    );
  }

  // Safety-net teardown: if any seeded scenario timed out (skipping its own
  // finally), sweep leftover seeded domains + CF records so nothing leaks into the
  // shared zone/account. No-op when seeding is off.
  afterAll(async () => {
    if (SEED) await sweepLeaks(baseUrl!, apiKey!);
  }, 120_000);
});
