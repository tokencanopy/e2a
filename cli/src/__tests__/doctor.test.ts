import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { E2AError } from "@e2a/sdk/v1";
import type { DoctorCheck, DoctorReport } from "../commands/doctor.js";

const mockLoadConfig = vi.fn();
vi.mock("../config.js", () => ({
  loadConfig: (...args: unknown[]) => mockLoadConfig(...args),
}));

const mockCreateClient = vi.fn();
vi.mock("../sdk.js", () => ({
  createClient: (...args: unknown[]) => mockCreateClient(...args),
}));

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

function baseConfig(overrides: Record<string, unknown> = {}) {
  return {
    api_key: "e2a_acct_testkey",
    api_url: "https://e2a.dev",
    agent_email: "bot@agents.e2a.dev",
    shared_domain: "agents.e2a.dev",
    key_scope: "account",
    ...overrides,
  };
}

function makeAccount(overrides: Record<string, unknown> = {}) {
  return {
    user: { id: "usr_1", email: "owner@example.com" },
    scope: "account",
    planCode: "free",
    upgradeUrl: "https://e2a.dev/upgrade",
    limits: { maxAgents: 5, maxDomains: 1, maxMessagesMonth: 1000, maxStorageBytes: 0 },
    usage: { agents: 2, domains: 1, messagesMonth: 17, storageBytes: 0 },
    ...overrides,
  };
}

function makeAgent(overrides: Record<string, unknown> = {}) {
  return {
    email: "bot@agents.e2a.dev",
    name: "Bot",
    domain: "agents.e2a.dev",
    registeredDomain: "agents.e2a.dev",
    domainVerified: true,
    createdAt: new Date("2026-07-01T00:00:00Z"),
    ...overrides,
  };
}

function makeDomain(overrides: Record<string, unknown> = {}) {
  return {
    domain: "acme.com",
    verified: true,
    verificationToken: "e2a-verify-tok123",
    agentCount: 1,
    createdAt: new Date("2026-07-01T00:00:00Z"),
    sendingStatus: "verified",
    sendingRamp: { status: "complete" },
    dnsRecords: [
      {
        type: "TXT",
        name: "acme.com",
        value: "e2a-verify-tok123",
        priority: null,
        purpose: "ownership",
        status: "verified",
      },
      {
        type: "MX",
        name: "acme.com",
        value: "smtp.e2a.dev",
        priority: 10,
        purpose: "inbound_mx",
        status: "verified",
      },
      {
        type: "TXT",
        name: "sel1._domainkey.acme.com",
        value: "v=DKIM1; k=rsa; p=MIIBIjAN",
        priority: null,
        purpose: "dkim",
        status: "verified",
      },
      {
        type: "MX",
        name: "mail.acme.com",
        value: "feedback-smtp.us-east-1.amazonses.com",
        priority: 10,
        purpose: "mail_from_mx",
        status: "verified",
      },
      {
        type: "TXT",
        name: "mail.acme.com",
        value: "v=spf1 include:amazonses.com ~all",
        priority: null,
        purpose: "mail_from_spf",
        status: "verified",
      },
    ],
    ...overrides,
  };
}

function makeWebhook(overrides: Record<string, unknown> = {}) {
  return {
    id: "wh_1",
    url: "https://hooks.example.com/e2a",
    description: "",
    events: ["email.received"],
    filters: {},
    enabled: true,
    createdAt: new Date("2026-07-01T00:00:00Z"),
    lastDeliveredAt: new Date("2026-07-22T00:00:00Z"),
    ...overrides,
  };
}

function makeDelivery(overrides: Record<string, unknown> = {}) {
  return {
    id: "whd_1",
    type: "email.received",
    status: "delivered",
    attempts: 1,
    createdAt: new Date("2026-07-22T00:00:00Z"),
    lastAttemptAt: new Date("2026-07-22T00:00:01Z"),
    lastStatusCode: 200,
    ...overrides,
  };
}

async function* pager<T>(items: T[]): AsyncGenerator<T> {
  for (const item of items) yield item;
}

/**
 * Traps any access to a method the fake doesn't define — the fakes define
 * ONLY read methods, so doctor reaching for create/update/delete/test/verify
 * (or anything else mutating) explodes loudly in every test, not just the
 * dedicated read-only test.
 */
function readOnly<T extends object>(name: string, obj: T): T {
  return new Proxy(obj, {
    get(target, prop, receiver) {
      if (typeof prop === "string" && !(prop in target)) {
        throw new Error(`doctor accessed a non-read method: ${name}.${prop}`);
      }
      return Reflect.get(target, prop, receiver);
    },
  });
}

function fakeClient(overrides: Record<string, unknown> = {}) {
  return readOnly("client", {
    account: readOnly("account", { get: vi.fn(async () => makeAccount()) }),
    agents: readOnly("agents", { get: vi.fn(async () => makeAgent()) }),
    domains: readOnly("domains", {
      list: vi.fn(() => pager([makeDomain()])),
      get: vi.fn(async () => makeDomain()),
    }),
    webhooks: readOnly("webhooks", {
      list: vi.fn(() => pager([makeWebhook()])),
      deliveries: vi.fn(() => pager([makeDelivery()])),
    }),
    ...overrides,
  });
}

// DNS answers matching makeDomain()'s prescribed records, all present.
function healthyDNS() {
  return {
    txt: {
      "acme.com": ["e2a-verify-tok123", "v=spf1 include:_spf.google.com ~all"],
      "sel1._domainkey.acme.com": ["v=DKIM1; k=rsa; p=MIIBIjAN"],
      "mail.acme.com": ["v=spf1 include:amazonses.com ~all"],
      "_dmarc.acme.com": ["v=DMARC1; p=quarantine"],
    } as Record<string, string[]>,
    mx: {
      "acme.com": [{ exchange: "smtp.e2a.dev", priority: 10 }],
      "mail.acme.com": [{ exchange: "feedback-smtp.us-east-1.amazonses.com", priority: 10 }],
    } as Record<string, Array<{ exchange: string; priority: number }>>,
  };
}

interface FakeIOOptions {
  dns?: ReturnType<typeof healthyDNS>;
  http?: Record<string, { status: number; body: string }>;
  env?: Record<string, string>;
  httpError?: string;
}

function fakeIO(opts: FakeIOOptions = {}) {
  const dns = opts.dns ?? healthyDNS();
  const http: Record<string, { status: number; body: string }> = {
    "https://e2a.dev/v1/info": {
      status: 200,
      body: JSON.stringify({
        version: "1.0.0",
        shared_domain: "agents.e2a.dev",
        slug_registration_enabled: true,
        public_url: "https://e2a.dev",
      }),
    },
    "https://api.e2a.dev/mcp": { status: 405, body: "{}" },
    "https://api.e2a.dev/.well-known/oauth-protected-resource": {
      status: 200,
      body: JSON.stringify({
        resource: "https://api.e2a.dev/mcp",
        authorization_servers: ["https://api.e2a.dev"],
        scopes_supported: ["agent", "account"],
      }),
    },
    ...opts.http,
  };
  return {
    now: () => new Date("2026-07-23T12:00:00.000Z"),
    cliVersion: () => "0.0.0-test",
    env: (name: string) => opts.env?.[name],
    httpGet: vi.fn(async (url: string) => {
      if (opts.httpError) throw new Error(opts.httpError);
      const res = http[url];
      if (!res) throw new Error(`connect ECONNREFUSED (${url})`);
      return res;
    }),
    resolveTxt: vi.fn(async (name: string) => dns.txt[name] ?? []),
    resolveMx: vi.fn(async (name: string) => dns.mx[name] ?? []),
  };
}

function check(report: DoctorReport, id: string, target?: string): DoctorCheck {
  const found = report.checks.filter(
    (c) => c.id === id && (target === undefined || c.target === target),
  );
  expect(found.length, `expected exactly one check ${id}`).toBe(1);
  return found[0];
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe("doctor", () => {
  beforeEach(() => {
    mockLoadConfig.mockReturnValue(baseConfig());
    mockCreateClient.mockReturnValue(fakeClient());
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  describe("healthy run", () => {
    it("passes every applicable check and exits 0", async () => {
      const { runDoctor, EXIT_HEALTHY } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      expect(report.schema).toBe("e2a.doctor/v1");
      expect(report.status).toBe("healthy");
      expect(report.exit_code).toBe(EXIT_HEALTHY);
      expect(report.summary.fail).toBe(0);
      expect(report.summary.warn).toBe(0);

      expect(check(report, "cli.config").status).toBe("pass");
      expect(check(report, "api.reachability").status).toBe("pass");
      expect(check(report, "api.auth").status).toBe("pass");
      expect(check(report, "agent.access").status).toBe("pass");
      expect(check(report, "domain.ownership").status).toBe("pass");
      expect(check(report, "domain.mx").status).toBe("pass");
      expect(check(report, "domain.dkim").status).toBe("pass");
      expect(check(report, "domain.mailfrom_mx").status).toBe("pass");
      expect(check(report, "domain.spf").status).toBe("pass");
      expect(check(report, "domain.dmarc").status).toBe("pass");
      expect(check(report, "domain.sending").status).toBe("pass");
      expect(check(report, "mcp.reachability").status).toBe("pass");
      expect(check(report, "mcp.auth_metadata").status).toBe("pass");
      expect(check(report, "webhook.config").status).toBe("pass");
      expect(check(report, "webhook.delivery").status).toBe("pass");
      // No safe non-delivering probe exists — always an explicit skip.
      expect(check(report, "webhook.reachability").status).toBe("skip");
      expect(check(report, "webhook.reachability").reason_code).toBe("no_safe_probe");
    });

    it("never calls a mutating endpoint or sends mail", async () => {
      // The fake client defines ONLY read methods behind a Proxy that throws
      // on any other property access (send/create/delete/test/verify/...),
      // so a full run completing IS the proof no mutating method was touched.
      const client = fakeClient();
      mockCreateClient.mockReturnValue(client);
      const io = fakeIO();
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, io);
      expect(report.status).toBe("healthy");

      // And the raw HTTP probes never hit the side-effecting endpoints.
      for (const call of io.httpGet.mock.calls) {
        expect(String(call[0])).not.toContain("/test");
        expect(String(call[0])).not.toContain("/verify");
      }
    });

    it("never leaks the API key into the serialized report", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());
      expect(JSON.stringify(report)).not.toContain("e2a_acct_testkey");
    });
  });

  describe("auth failures (exit 4)", () => {
    it("fails cli.config when no API key is configured", async () => {
      mockLoadConfig.mockReturnValue(baseConfig({ api_key: "" }));
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const c = check(report, "cli.config");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("no_api_key");
      expect(report.exit_code).toBe(4);
      expect(report.status).toBe("failed");
      // Authenticated checks are skipped, not failed.
      expect(check(report, "api.auth").status).toBe("skip");
      expect(check(report, "agent.access").status).toBe("skip");
      // Unauthenticated checks still run.
      expect(check(report, "api.reachability").status).toBe("pass");
    });

    it("fails api.auth on a rejected key", async () => {
      const client = fakeClient();
      client.account.get = vi.fn(async () => {
        throw new E2AError({
          code: "unauthorized", message: "bad key", status: 401, retryable: false,
        });
      });
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const c = check(report, "api.auth");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("unauthorized");
      expect(report.exit_code).toBe(4);
    });

    it("fails agent.access with forbidden for a foreign agent-bound key", async () => {
      const client = fakeClient();
      client.agents.get = vi.fn(async () => {
        throw new E2AError({
          code: "forbidden", message: "forbidden", status: 403, retryable: false,
        });
      });
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const c = check(report, "agent.access");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("forbidden");
      expect(report.exit_code).toBe(4);
    });
  });

  describe("configuration failures (exit 9)", () => {
    it("fails agent.access when the agent does not exist", async () => {
      const client = fakeClient();
      client.agents.get = vi.fn(async () => {
        throw new E2AError({
          code: "not_found", message: "no such agent", status: 404, retryable: false,
        });
      });
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const c = check(report, "agent.access");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("agent_not_found");
      expect(c.remediation).toBeTruthy();
      expect(report.exit_code).toBe(9);
    });

    it("fails domain.mx when the live MX record is missing", async () => {
      const dns = healthyDNS();
      dns.mx["acme.com"] = [];
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ dns }));

      const c = check(report, "domain.mx");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("record_missing");
      expect(c.remediation).toContain("smtp.e2a.dev");
      expect(report.exit_code).toBe(9);
    });

    it("fails domain.mx with record_mismatch when a different MX is published", async () => {
      const dns = healthyDNS();
      dns.mx["acme.com"] = [{ exchange: "mail.other.com", priority: 5 }];
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ dns }));

      const c = check(report, "domain.mx");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("record_mismatch");
      expect((c.evidence as Record<string, unknown>).found).toEqual(["mail.other.com"]);
    });

    it("fails domain.ownership when the TXT token is absent", async () => {
      const dns = healthyDNS();
      dns.txt["acme.com"] = ["v=spf1 include:_spf.google.com ~all"];
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ dns }));

      const c = check(report, "domain.ownership");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("record_missing");
      expect(report.exit_code).toBe(9);
    });

    it("fails domain.sending when SES reports failure", async () => {
      const client = fakeClient();
      const domain = makeDomain({ sendingStatus: "failed", sendingError: "DKIM verification failed" });
      client.domains.list = vi.fn(() => pager([domain]));
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const c = check(report, "domain.sending");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("sending_failed");
      expect((c.evidence as Record<string, unknown>).sending_error).toBe("DKIM verification failed");
      expect(report.exit_code).toBe(9);
    });

    it("fails domain.registered when --domain names an unregistered domain", async () => {
      const client = fakeClient();
      client.domains.get = vi.fn(async () => {
        throw new E2AError({
          code: "not_found", message: "not found", status: 404, retryable: false,
        });
      });
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({ domain: "missing.example" }, fakeIO());

      const c = check(report, "domain.registered");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("not_registered");
      expect(report.exit_code).toBe(9);
    });

    it("fails webhook.config when a webhook was auto-disabled", async () => {
      const client = fakeClient();
      client.webhooks.list = vi.fn(() =>
        pager([
          makeWebhook({
            enabled: false,
            autoDisabledAt: new Date("2026-07-20T00:00:00Z"),
            autoDisabledReason: "HTTP 404",
          }),
        ]),
      );
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const c = check(report, "webhook.config");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("webhook_auto_disabled");
      // The concrete failure the breaker observed rides along as evidence —
      // the first question anyone debugging this asks.
      expect(c.evidence).toMatchObject({ auto_disabled_reason: "HTTP 404" });
      expect(report.exit_code).toBe(9);
    });
  });

  describe("transient connectivity failures (exit 1)", () => {
    it("fails api.reachability when the deployment is unreachable", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ httpError: "connect ECONNREFUSED" }));

      const c = check(report, "api.reachability");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("connection_failed");
      expect(report.exit_code).toBe(1);
      // API-dependent checks skip rather than pile on failures.
      expect(check(report, "api.auth").status).toBe("skip");
      expect(check(report, "agent.access").status).toBe("skip");
    });

    it("treats a 5xx from /v1/info as transient", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor(
        {},
        fakeIO({ http: { "https://e2a.dev/v1/info": { status: 503, body: "unavailable" } } }),
      );

      const c = check(report, "api.reachability");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("http_error");
      expect(report.exit_code).toBe(1);
    });

    it("treats a 404 from /v1/info as a configuration failure (wrong E2A_URL)", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor(
        {},
        fakeIO({ http: { "https://e2a.dev/v1/info": { status: 404, body: "not here" } } }),
      );

      expect(check(report, "api.reachability").status).toBe("fail");
      expect(report.exit_code).toBe(9);
    });

    it("fails domain DNS checks transiently when the resolver errors", async () => {
      const io = fakeIO();
      io.resolveMx = vi.fn(async () => {
        throw new Error("queryMx ETIMEOUT acme.com");
      });
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, io);

      const c = check(report, "domain.mx");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("dns_lookup_failed");
      expect(report.exit_code).toBe(1);
    });

    it("prefers auth over config over transient when mixed", async () => {
      // Unauthorized key + broken DNS: exit must be 4, not 1/9.
      const client = fakeClient();
      client.account.get = vi.fn(async () => {
        throw new E2AError({
          code: "unauthorized", message: "bad key", status: 401, retryable: false,
        });
      });
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());
      expect(report.exit_code).toBe(4);
    });
  });

  describe("exit precedence ladder (each rung individually)", () => {
    it("auth beats config: forbidden agent + missing MX exits 4", async () => {
      const client = fakeClient();
      client.agents.get = vi.fn(async () => {
        throw new E2AError({
          code: "forbidden", message: "forbidden", status: 403, retryable: false,
        });
      });
      mockCreateClient.mockReturnValue(client);
      const dns = healthyDNS();
      dns.mx["acme.com"] = []; // config-class failure alongside the auth one
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ dns }));

      expect(check(report, "agent.access").status).toBe("fail");
      expect(check(report, "domain.mx").status).toBe("fail");
      expect(report.exit_code).toBe(4);
    });

    it("config beats transient: missing MX + DNS timeout exits 9", async () => {
      const dns = healthyDNS();
      dns.mx["acme.com"] = []; // config-class failure
      const io = fakeIO({ dns });
      const realResolveMx = io.resolveMx.getMockImplementation()!;
      io.resolveMx = vi.fn(async (name: string) => {
        if (name === "mail.acme.com") throw new Error("queryMx ETIMEOUT"); // transient
        return realResolveMx(name);
      });
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, io);

      expect(check(report, "domain.mx").reason_code).toBe("record_missing");
      expect(check(report, "domain.mailfrom_mx").reason_code).toBe("dns_lookup_failed");
      expect(report.exit_code).toBe(9);
    });

    it("transient beats warn: DNS timeout + disabled webhook exits 1", async () => {
      const client = fakeClient();
      client.webhooks.list = vi.fn(() => pager([makeWebhook({ enabled: false })])); // warn
      mockCreateClient.mockReturnValue(client);
      const io = fakeIO();
      const realResolveMx = io.resolveMx.getMockImplementation()!;
      io.resolveMx = vi.fn(async (name: string) => {
        if (name === "mail.acme.com") throw new Error("queryMx ETIMEOUT"); // transient
        return realResolveMx(name);
      });
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, io);

      expect(check(report, "webhook.config").status).toBe("warn");
      expect(report.exit_code).toBe(1);
    });
  });

  describe("hardening (review findings)", () => {
    it("a malformed --mcp-url never crashes runDoctor and classifies as config", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({ mcpUrl: "api.e2a.dev/mcp" }, fakeIO());

      expect(check(report, "mcp.reachability").reason_code).toBe("invalid_mcp_url");
      expect(check(report, "mcp.auth_metadata").reason_code).toBe("invalid_mcp_url");
      expect(report.exit_code).toBe(9);
    });

    it("doctor rejects a malformed --mcp-url as a usage error (exit 2)", async () => {
      const mockExit = vi.spyOn(process, "exit").mockImplementation(() => {
        throw new Error("process.exit");
      });
      const mockStderr = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
      const { doctor } = await import("../commands/doctor.js");
      await expect(doctor({ mcpUrl: "not a url" }, fakeIO())).rejects.toThrow("process.exit");
      expect(mockExit).toHaveBeenCalledWith(2);
      mockExit.mockRestore();
      mockStderr.mockRestore();
    });

    it("HTTP 404 at the MCP URL is a routing failure, not a pass", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor(
        {},
        fakeIO({ http: { "https://api.e2a.dev/mcp": { status: 404, body: "" } } }),
      );

      const c = check(report, "mcp.reachability");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("not_routed");
      expect(report.exit_code).toBe(9);
    });

    it("fails mcp.reachability transiently when the endpoint is unreachable", async () => {
      const io = fakeIO();
      const realHttpGet = io.httpGet.getMockImplementation()!;
      io.httpGet = vi.fn(async (url: string) => {
        if (url === "https://api.e2a.dev/mcp") throw new Error("connect ETIMEDOUT");
        return realHttpGet(url);
      });
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, io);

      expect(check(report, "mcp.reachability").reason_code).toBe("connection_failed");
      expect(report.exit_code).toBe(1);
    });

    it("warns on invalid MCP auth metadata (no authorization_servers)", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor(
        {},
        fakeIO({
          http: {
            "https://api.e2a.dev/.well-known/oauth-protected-resource": { status: 200, body: "{}" },
          },
        }),
      );

      expect(check(report, "mcp.auth_metadata").reason_code).toBe("metadata_invalid");
    });

    it("a trailing slash on the deployment URL still detects hosted and probes cleanly", async () => {
      mockLoadConfig.mockReturnValue(baseConfig({ api_url: "https://e2a.dev/" }));
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      expect(check(report, "api.reachability").status).toBe("pass");
      expect(check(report, "mcp.reachability").status).toBe("pass");
      expect(report.deployment_url).toBe("https://e2a.dev");
    });

    it("accepts a legitimately extended SPF record (inclusion, not equality)", async () => {
      const dns = healthyDNS();
      dns.txt["mail.acme.com"] = ["v=spf1 include:amazonses.com include:_spf.corp.example ~all"];
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ dns }));

      expect(check(report, "domain.spf").status).toBe("pass");
    });

    it("flags an SPF record that lost the prescribed include as a mismatch", async () => {
      const dns = healthyDNS();
      dns.txt["mail.acme.com"] = ["v=spf1 include:other.example ~all"];
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ dns }));

      const c = check(report, "domain.spf");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("record_mismatch");
    });

    it("treats a 200 non-JSON /v1/info body as a config failure (wrong E2A_URL)", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor(
        {},
        fakeIO({ http: { "https://e2a.dev/v1/info": { status: 200, body: "<html>welcome</html>" } } }),
      );

      const c = check(report, "api.reachability");
      expect(c.reason_code).toBe("unexpected_response");
      expect(report.exit_code).toBe(9);
    });

    it("warns on a plain-HTTP webhook URL", async () => {
      const client = fakeClient();
      client.webhooks.list = vi.fn(() => pager([makeWebhook({ url: "http://hooks.example.com/e2a" })]));
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      expect(check(report, "webhook.config").reason_code).toBe("insecure_url");
      expect(report.exit_code).toBe(8);
    });

    it("skips webhook.delivery when no delivery has completed yet", async () => {
      const client = fakeClient();
      client.webhooks.deliveries = vi.fn(() => pager([]));
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      expect(check(report, "webhook.delivery").reason_code).toBe("no_deliveries");
    });

    it("reports the key source as E2A_API_KEY when set via environment", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ env: { E2A_API_KEY: "e2a_acct_envkey" } }));

      const c = check(report, "cli.config");
      expect((c.evidence as Record<string, unknown>).key_source).toBe("E2A_API_KEY");
    });

    it("warns when the domain list is truncated at the cap", async () => {
      const client = fakeClient();
      const many = Array.from({ length: 51 }, (_, i) => makeDomain({ domain: `d${i}.example` }));
      client.domains.list = vi.fn(() => pager(many));
      mockCreateClient.mockReturnValue(client);
      const io = fakeIO();
      // Domain names beyond acme.com have no DNS fixtures; resolve empty is fine —
      // this test only asserts the truncation warning exists.
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, io);

      const truncation = report.checks.find((c) => c.reason_code === "too_many_domains");
      expect(truncation?.status).toBe("warn");
    });
  });

  describe("warnings (exit 8)", () => {
    it("warns on a missing DMARC record without failing the run", async () => {
      const dns = healthyDNS();
      delete dns.txt["_dmarc.acme.com"];
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ dns }));

      const c = check(report, "domain.dmarc");
      expect(c.status).toBe("warn");
      expect(c.reason_code).toBe("no_dmarc_record");
      expect(c.remediation).toContain("_dmarc.acme.com");
      expect(report.status).toBe("warnings");
      expect(report.exit_code).toBe(8);
    });

    it("warns when sending status is still pending", async () => {
      const client = fakeClient();
      client.domains.list = vi.fn(() => pager([makeDomain({ sendingStatus: "pending" })]));
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      expect(check(report, "domain.sending").status).toBe("warn");
      expect(report.exit_code).toBe(8);
    });

    it("warns when a webhook is manually disabled", async () => {
      const client = fakeClient();
      client.webhooks.list = vi.fn(() => pager([makeWebhook({ enabled: false })]));
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const c = check(report, "webhook.config");
      expect(c.status).toBe("warn");
      expect(c.reason_code).toBe("webhook_disabled");
      expect(report.exit_code).toBe(8);
    });

    it("warns when recent webhook deliveries are failing", async () => {
      const client = fakeClient();
      client.webhooks.deliveries = vi.fn(() =>
        pager([makeDelivery({ status: "failed", lastError: "connection refused", lastStatusCode: 0, attempts: 5 })]),
      );
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const c = check(report, "webhook.delivery");
      expect(c.status).toBe("warn");
      expect(c.reason_code).toBe("deliveries_failing");
      expect((c.evidence as Record<string, unknown>).last_error).toBe("connection refused");
    });

    it("warns when MCP auth metadata is missing", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor(
        {},
        fakeIO({
          http: {
            "https://api.e2a.dev/.well-known/oauth-protected-resource": { status: 404, body: "" },
          },
        }),
      );

      const c = check(report, "mcp.auth_metadata");
      expect(c.status).toBe("warn");
      expect(c.reason_code).toBe("metadata_missing");
    });
  });

  describe("skips", () => {
    it("skips agent.access when no agent is selected anywhere", async () => {
      mockLoadConfig.mockReturnValue(baseConfig({ agent_email: "" }));
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const c = check(report, "agent.access");
      expect(c.status).toBe("skip");
      expect(c.reason_code).toBe("no_agent_selected");
      // A skip alone never degrades the run.
      expect(report.exit_code).toBe(0);
    });

    it("skips domain and webhook checks for an agent-scoped key", async () => {
      const client = fakeClient();
      client.account.get = vi.fn(async () =>
        makeAccount({ scope: "agent", agentEmail: "bot@agents.e2a.dev" }),
      );
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      expect(check(report, "domain.registered").reason_code).toBe("requires_account_scope");
      expect(check(report, "webhook.config").reason_code).toBe("requires_account_scope");
      expect(report.exit_code).toBe(0);
    });

    it("skips domain record checks when only the shared domain is in use", async () => {
      const client = fakeClient();
      client.domains.list = vi.fn(() => pager([]));
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const c = check(report, "domain.registered");
      expect(c.status).toBe("skip");
      expect(c.reason_code).toBe("no_domains");
      expect(report.exit_code).toBe(0);
    });

    it("skips DKIM when the server has not provisioned a selector yet", async () => {
      const client = fakeClient();
      const domain = makeDomain();
      (domain.dnsRecords as Array<{ purpose: string }>) = (
        domain.dnsRecords as Array<{ purpose: string }>
      ).filter((r) => !["dkim", "mail_from_mx", "mail_from_spf"].includes(r.purpose));
      client.domains.list = vi.fn(() => pager([domain]));
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      expect(check(report, "domain.dkim").reason_code).toBe("not_prescribed");
      expect(check(report, "domain.spf").reason_code).toBe("not_prescribed");
      expect(check(report, "domain.mailfrom_mx").reason_code).toBe("not_prescribed");
    });

    it("skips MCP checks for a self-hosted deployment without --mcp-url", async () => {
      mockLoadConfig.mockReturnValue(baseConfig({ api_url: "https://mail.selfhosted.io" }));
      const { runDoctor } = await import("../commands/doctor.js");
      const io = fakeIO({
        http: {
          "https://mail.selfhosted.io/v1/info": {
            status: 200,
            body: JSON.stringify({ version: "1.0.0", shared_domain: "", slug_registration_enabled: false }),
          },
        },
      });
      const report = await runDoctor({}, io);

      expect(check(report, "mcp.reachability").reason_code).toBe("no_mcp_url");
      expect(check(report, "mcp.auth_metadata").reason_code).toBe("no_mcp_url");
    });

    it("probes an explicit --mcp-url", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const io = fakeIO({
        http: {
          "https://mcp.selfhosted.io/mcp": { status: 405, body: "{}" },
          "https://mcp.selfhosted.io/.well-known/oauth-protected-resource": {
            status: 200,
            body: JSON.stringify({ authorization_servers: ["https://mcp.selfhosted.io"], scopes_supported: ["agent"] }),
          },
        },
      });
      const report = await runDoctor({ mcpUrl: "https://mcp.selfhosted.io/mcp" }, io);

      expect(check(report, "mcp.reachability").status).toBe("pass");
      expect(check(report, "mcp.auth_metadata").status).toBe("pass");
    });

    it("skips webhook checks when none are configured", async () => {
      const client = fakeClient();
      client.webhooks.list = vi.fn(() => pager([]));
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const c = check(report, "webhook.config");
      expect(c.status).toBe("skip");
      expect(c.reason_code).toBe("no_webhooks");
    });
  });

  describe("smtp.config (self-hosted outbound visibility)", () => {
    it("reports outbound SMTP env config without exposing credentials", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor(
        {},
        fakeIO({
          env: {
            E2A_OUTBOUND_SMTP_HOST: "email-smtp.us-east-1.amazonaws.com",
            E2A_OUTBOUND_SMTP_PORT: "587",
            E2A_OUTBOUND_SMTP_FROM_DOMAIN: "acme.com",
            E2A_OUTBOUND_SMTP_USERNAME: "AKIA_SECRET_USER",
            E2A_OUTBOUND_SMTP_PASSWORD: "supersecret",
          },
        }),
      );

      const c = check(report, "smtp.config");
      expect(c.status).toBe("pass");
      const evidence = c.evidence as Record<string, unknown>;
      expect(evidence.host).toBe("email-smtp.us-east-1.amazonaws.com");
      expect(evidence.credentials).toBe("set");
      const serialized = JSON.stringify(report);
      expect(serialized).not.toContain("supersecret");
      expect(serialized).not.toContain("AKIA_SECRET_USER");
    });

    it("skips when no outbound SMTP env is visible", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const c = check(report, "smtp.config");
      expect(c.status).toBe("skip");
      expect(c.reason_code).toBe("not_visible");
    });

    it("warns on partial outbound SMTP config", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor(
        {},
        fakeIO({ env: { E2A_OUTBOUND_SMTP_HOST: "localhost" } }),
      );

      const c = check(report, "smtp.config");
      expect(c.status).toBe("warn");
      expect(c.reason_code).toBe("smtp_partial");
    });
  });

  describe("output", () => {
    let mockStdout: ReturnType<typeof vi.spyOn>;
    let mockExitCode: number | undefined;

    beforeEach(() => {
      mockStdout = vi.spyOn(process.stdout, "write").mockImplementation(() => true);
      mockExitCode = process.exitCode as number | undefined;
    });

    afterEach(() => {
      mockStdout.mockRestore();
      process.exitCode = mockExitCode;
    });

    it("human output uses pass/warn/fail lines with remediation", async () => {
      const dns = healthyDNS();
      delete dns.txt["_dmarc.acme.com"];
      dns.mx["acme.com"] = [];
      const { doctor } = await import("../commands/doctor.js");
      await doctor({}, fakeIO({ dns }));

      const output = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
      expect(output).toMatch(/^pass {2}cli\.config/m);
      expect(output).toMatch(/^fail {2}domain\.mx/m);
      expect(output).toMatch(/^warn {2}domain\.dmarc/m);
      expect(output).toContain("fix:");
      expect(output).toMatch(/exit 9/);
      expect(process.exitCode).toBe(9);
    });

    it("--json emits the versioned report and nothing else", async () => {
      const { doctor } = await import("../commands/doctor.js");
      await doctor({ json: true }, fakeIO());

      const output = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
      const parsed = JSON.parse(output);
      expect(parsed.schema).toBe("e2a.doctor/v1");
      expect(parsed.exit_code).toBe(0);
      expect(process.exitCode).toBe(0);
    });
  });

  describe("hardening (review round 2)", () => {
    it("accepts a DKIM record with extra tags and reordering (p= payload match)", async () => {
      const dns = healthyDNS();
      // Same key, different tag order plus extra tags — the server's own
      // verifier (classifyDKIM) accepts this; doctor must too.
      dns.txt["sel1._domainkey.acme.com"] = ["k=rsa; s=email; t=s; v=DKIM1; p=MIIBIjAN"];
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ dns }));

      expect(check(report, "domain.dkim").status).toBe("pass");
    });

    it("flags a DKIM record with a different p= payload as a mismatch", async () => {
      const dns = healthyDNS();
      dns.txt["sel1._domainkey.acme.com"] = ["v=DKIM1; k=rsa; p=TRUNCATEDKEY"];
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ dns }));

      const c = check(report, "domain.dkim");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("record_mismatch");
      expect(report.exit_code).toBe(9);
    });

    it("accepts an ownership token embedded in a larger TXT record (substring, like the server)", async () => {
      const dns = healthyDNS();
      dns.txt["acme.com"] = ["registrar-wrapped e2a-verify-tok123 suffix"];
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ dns }));

      expect(check(report, "domain.ownership").status).toBe("pass");
    });

    it("matches SPF case-insensitively (like the server)", async () => {
      const dns = healthyDNS();
      dns.txt["mail.acme.com"] = ["V=SPF1 INCLUDE:AMAZONSES.COM ~ALL"];
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ dns }));

      expect(check(report, "domain.spf").status).toBe("pass");
    });

    it("checks each domain against its own records (per-domain isolation)", async () => {
      const client = fakeClient();
      const beta = makeDomain({
        domain: "beta.io",
        verificationToken: "e2a-verify-beta",
        dnsRecords: [
          { type: "TXT", name: "beta.io", value: "e2a-verify-beta", priority: null, purpose: "ownership", status: "pending" },
          { type: "MX", name: "beta.io", value: "smtp.e2a.dev", priority: 10, purpose: "inbound_mx", status: "pending" },
        ],
      });
      client.domains.list = vi.fn(() => pager([makeDomain(), beta]));
      mockCreateClient.mockReturnValue(client);
      const dns = healthyDNS();
      dns.txt["beta.io"] = ["e2a-verify-beta"];
      dns.mx["beta.io"] = []; // beta's MX missing; acme's intact
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ dns }));

      expect(check(report, "domain.mx", "acme.com").status).toBe("pass");
      expect(check(report, "domain.mx", "beta.io").status).toBe("fail");
      expect(check(report, "domain.ownership", "beta.io").status).toBe("pass");
      expect(report.exit_code).toBe(9);
    });

    it("skips domain.sending when the identity is not provisioned (self-hosted)", async () => {
      const client = fakeClient();
      client.domains.list = vi.fn(() => pager([makeDomain({ sendingStatus: "none" })]));
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      expect(check(report, "domain.sending").reason_code).toBe("sending_not_provisioned");
    });

    it("a DMARC lookup error is a warn, never a fail (advisory contract)", async () => {
      const io = fakeIO();
      const realResolveTxt = io.resolveTxt.getMockImplementation()!;
      io.resolveTxt = vi.fn(async (name: string) => {
        if (name.startsWith("_dmarc.")) throw new Error("queryTxt ETIMEOUT");
        return realResolveTxt(name);
      });
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, io);

      const c = check(report, "domain.dmarc");
      expect(c.status).toBe("warn");
      expect(c.reason_code).toBe("dns_lookup_failed");
      expect(report.exit_code).toBe(8);
    });

    it("fails the run transiently when the delivery-history API errors", async () => {
      const client = fakeClient();
      client.webhooks.deliveries = vi.fn(() => {
        throw new E2AError({
          code: "internal_error", message: "boom", status: 500, retryable: true,
        });
      });
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      expect(check(report, "webhook.delivery").reason_code).toBe("connection_failed");
      expect(report.exit_code).toBe(1);
    });

    it("classifies a connection-level agent lookup failure as transient", async () => {
      const client = fakeClient();
      client.agents.get = vi.fn(async () => {
        throw new E2AError({
          code: "connection_error", message: "socket hang up", status: 0, retryable: true,
        });
      });
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      expect(check(report, "agent.access").reason_code).toBe("connection_failed");
      expect(report.exit_code).toBe(1);
    });

    it("warns when the webhook list is truncated at the cap", async () => {
      const client = fakeClient();
      const many = Array.from({ length: 51 }, (_, i) => makeWebhook({ id: `wh_${i}` }));
      client.webhooks.list = vi.fn(() => pager(many));
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const truncation = report.checks.find((c) => c.reason_code === "too_many_webhooks");
      expect(truncation?.status).toBe("warn");
    });

    it("survives a malformed domain object and still emits the full report", async () => {
      const client = fakeClient();
      const broken = { domain: "broken.example", verified: true, sendingStatus: "verified" };
      client.domains.list = vi.fn(() =>
        pager([broken as unknown as ReturnType<typeof makeDomain>, makeDomain()]),
      );
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const internal = report.checks.find((c) => c.id === "doctor.internal");
      expect(internal?.reason_code).toBe("internal_error");
      // The healthy domain and all later phases still reported.
      expect(check(report, "domain.mx", "acme.com").status).toBe("pass");
      expect(check(report, "smtp.config").status).toBe("skip");
    });

    it("strips basic-auth userinfo from webhook targets in the report", async () => {
      const client = fakeClient();
      client.webhooks.list = vi.fn(() =>
        pager([makeWebhook({ url: "https://user:hunter2@hooks.example.com/e2a" })]),
      );
      mockCreateClient.mockReturnValue(client);
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      expect(check(report, "webhook.config").target).toBe("https://hooks.example.com/e2a");
      expect(JSON.stringify(report)).not.toContain("hunter2");
    });

    it("fails api.reachability as config when E2A_URL is not an absolute http(s) URL", async () => {
      mockLoadConfig.mockReturnValue(baseConfig({ api_url: "e2a.dev" }));
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());

      const c = check(report, "api.reachability");
      expect(c.reason_code).toBe("invalid_deployment_url");
      expect(report.exit_code).toBe(9);
    });

    it("treats 429 from /v1/info as transient, not config", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor(
        {},
        fakeIO({ http: { "https://e2a.dev/v1/info": { status: 429, body: "slow down" } } }),
      );

      expect(check(report, "api.reachability").reason_code).toBe("http_error");
      expect(report.exit_code).toBe(1);
    });

    it("treats a 5xx from the MCP endpoint as transient, not a pass", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor(
        {},
        fakeIO({ http: { "https://api.e2a.dev/mcp": { status: 502, body: "" } } }),
      );

      const c = check(report, "mcp.reachability");
      expect(c.status).toBe("fail");
      expect(c.reason_code).toBe("http_error");
      expect(report.exit_code).toBe(1);
    });

    it("normalizes a trailing-slash E2A_URL into the SDK client too", async () => {
      mockLoadConfig.mockReturnValue(baseConfig({ api_url: "https://e2a.dev/" }));
      const { runDoctor } = await import("../commands/doctor.js");
      await runDoctor({}, fakeIO());

      const [, configArg] = mockCreateClient.mock.calls[0] as [unknown, { api_url: string }];
      expect(configArg.api_url).toBe("https://e2a.dev");
    });

    it("strips terminal control characters from human output", async () => {
      const client = fakeClient();
      client.webhooks.list = vi.fn(() =>
        pager([makeWebhook({ id: "wh_\u001b[31mevil\u001b[0m" })]),
      );
      mockCreateClient.mockReturnValue(client);
      const mockStdout = vi.spyOn(process.stdout, "write").mockImplementation(() => true);
      const savedExit = process.exitCode as number | undefined;
      const { doctor } = await import("../commands/doctor.js");
      await doctor({}, fakeIO());
      const output = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
      mockStdout.mockRestore();
      process.exitCode = savedExit;

      expect(output).not.toContain("\u001b");
      expect(output).toContain("wh_[31mevil[0m"); // escapes gone, text kept  
    });
  });

  describe("golden fixtures (schema lock)", () => {
    // The fixtures lock the e2a.doctor/v1 schema: any field rename, removal,
    // or shape change fails here. To regenerate after an INTENTIONAL additive
    // change: UPDATE_DOCTOR_FIXTURES=1 npx vitest run src/__tests__/doctor.test.ts
    async function assertGolden(report: unknown, name: string) {
      const { readFileSync, writeFileSync } = await import("node:fs");
      const path = new URL(`./fixtures/${name}`, import.meta.url);
      if (process.env.UPDATE_DOCTOR_FIXTURES) {
        writeFileSync(path, JSON.stringify(report, null, 2) + "\n");
        return;
      }
      expect(report).toEqual(JSON.parse(readFileSync(path, "utf-8")));
    }

    it("healthy hosted run matches the golden fixture", async () => {
      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO());
      await assertGolden(report, "doctor-healthy.json");
    });

    it("mixed-failure run matches the golden fixture", async () => {
      mockLoadConfig.mockReturnValue(baseConfig({ agent_email: "" }));
      const client = fakeClient();
      client.domains.list = vi.fn(() =>
        pager([makeDomain({ sendingStatus: "pending" })]),
      );
      client.webhooks.list = vi.fn(() => pager([makeWebhook({ enabled: false })]));
      mockCreateClient.mockReturnValue(client);
      const dns = healthyDNS();
      dns.mx["acme.com"] = [{ exchange: "mail.other.com", priority: 5 }];
      delete dns.txt["_dmarc.acme.com"];

      const { runDoctor } = await import("../commands/doctor.js");
      const report = await runDoctor({}, fakeIO({ dns }));
      await assertGolden(report, "doctor-mixed.json");
    });
  });
});
