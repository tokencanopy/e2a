import { describe, expect, it, vi } from "vitest";
import { ConfigError, loadConfig, logJson } from "../src/bin/http.js";

// Minimal env that satisfies both no-default requirements (E2A_API_URL and
// MCP_ALLOWED_HOSTS), so tests that exercise an unrelated field don't also
// have to reason about the fail-closed backend/host checks.
const VALID_ENV = {
  E2A_API_URL: "https://api.example.test",
  MCP_ALLOWED_HOSTS: "mcp.example.test",
};

describe("bin/http loadConfig", () => {
  it("throws ConfigError when no backend URL is configured", () => {
    // No default to the operator's api.e2a.dev: forwarding a self-hoster's
    // users' bearer tokens to someone else's deployment must be a loud
    // startup failure, not a silent fallback.
    expect(() => loadConfig({})).toThrowError(ConfigError);
    expect(() => loadConfig({})).toThrow(/E2A_API_URL/);
  });

  it("throws ConfigError when MCP_ALLOWED_HOSTS and MCP_PUBLIC_URL are both unset", () => {
    // No default host allowlist either: it must not silently 421 every
    // request against the operator's api.e2a.dev allowlist.
    expect(() => loadConfig({ E2A_API_URL: "https://api.example.test" })).toThrowError(ConfigError);
    expect(() => loadConfig({ E2A_API_URL: "https://api.example.test" })).toThrow(/MCP_ALLOWED_HOSTS/);
  });

  it("derives the allowed-hosts default from MCP_PUBLIC_URL's host", () => {
    const cfg = loadConfig({
      E2A_API_URL: "https://api.example.test",
      MCP_PUBLIC_URL: "https://mcp.example.test:8443/",
    });
    expect(cfg.allowedHosts).toEqual(["mcp.example.test"]);
  });

  it("prefers explicit MCP_ALLOWED_HOSTS over the MCP_PUBLIC_URL-derived default", () => {
    const cfg = loadConfig({
      E2A_API_URL: "https://api.example.test",
      MCP_PUBLIC_URL: "https://mcp.example.test",
      MCP_ALLOWED_HOSTS: "other.example.test",
    });
    expect(cfg.allowedHosts).toEqual(["other.example.test"]);
  });

  it("rejects an unparseable MCP_PUBLIC_URL used for the allowed-hosts default", () => {
    expect(() =>
      loadConfig({ E2A_API_URL: "https://api.example.test", MCP_PUBLIC_URL: "not-a-url" }),
    ).toThrowError(ConfigError);
    expect(() =>
      loadConfig({ E2A_API_URL: "https://api.example.test", MCP_PUBLIC_URL: "not-a-url" }),
    ).toThrow(/MCP_PUBLIC_URL/);
  });

  it("parses valid values (canonical E2A_API_URL)", () => {
    const cfg = loadConfig({
      PORT: "8080",
      E2A_API_URL: "https://api.staging.example.test",
      MCP_ALLOWED_HOSTS: "mcp.example.test,mcp-staging.example.test",
      MCP_SESSION_IDLE_MS: "60000",
      MCP_MAX_SESSIONS: "100",
      MCP_RESOLVE_TIMEOUT_MS: "2500",
    });
    expect(cfg).toEqual({
      port: 8080,
      baseUrl: "https://api.staging.example.test",
      allowedHosts: ["mcp.example.test", "mcp-staging.example.test"],
      sessionIdleMs: 60_000,
      maxSessions: 100,
      resolveTimeoutMs: 2500,
      trustProxy: "loopback",
    });
  });

  it.each([["E2A_URL"], ["E2A_BASE_URL"]])(
    "falls back to %s when E2A_API_URL is unset (structured deprecation log)",
    (legacy) => {
      const lines: string[] = [];
      const warn = vi.spyOn(process.stderr, "write").mockImplementation((chunk) => {
        lines.push(String(chunk));
        return true;
      });
      const cfg = loadConfig({
        [legacy]: "https://legacy.example.com",
        MCP_ALLOWED_HOSTS: "mcp.example.test",
      });
      expect(cfg.baseUrl).toBe("https://legacy.example.com");
      // The deprecation notice is emitted as one structured JSON line that
      // Cloud Logging can parse — severity + event + a human-readable message.
      const entry = JSON.parse(lines.at(-1)!);
      expect(entry).toMatchObject({ severity: "WARNING", event: "e2a_api_url_legacy_name" });
      expect(entry.message).toContain(legacy);
      warn.mockRestore();
    },
  );

  it("prefers canonical E2A_API_URL over both legacy names", () => {
    const cfg = loadConfig({
      E2A_API_URL: "https://canonical.example.com",
      E2A_URL: "https://deployment-root.example.com",
      E2A_BASE_URL: "https://legacy.example.com",
      MCP_ALLOWED_HOSTS: "mcp.example.test",
    });
    expect(cfg.baseUrl).toBe("https://canonical.example.com");
  });

  it("rejects non-numeric PORT", () => {
    expect(() => loadConfig({ PORT: "abc" })).toThrowError(ConfigError);
    expect(() => loadConfig({ PORT: "abc" })).toThrow(/PORT/);
  });

  it("rejects negative PORT", () => {
    expect(() => loadConfig({ PORT: "-1" })).toThrowError(ConfigError);
  });

  it("rejects port over 65535", () => {
    expect(() => loadConfig({ PORT: "70000" })).toThrowError(ConfigError);
  });

  it("rejects MCP_MAX_SESSIONS=0", () => {
    expect(() => loadConfig({ ...VALID_ENV, MCP_MAX_SESSIONS: "0" })).toThrowError(ConfigError);
    expect(() => loadConfig({ ...VALID_ENV, MCP_MAX_SESSIONS: "0" })).toThrow(/MCP_MAX_SESSIONS/);
  });

  it("rejects non-integer MCP_SESSION_IDLE_MS", () => {
    expect(() => loadConfig({ ...VALID_ENV, MCP_SESSION_IDLE_MS: "3.14" })).toThrowError(ConfigError);
  });

  it("defaults MCP_RESOLVE_TIMEOUT_MS to 5000", () => {
    expect(loadConfig(VALID_ENV).resolveTimeoutMs).toBe(5000);
  });

  it.each([["0"], ["-100"], ["abc"], ["3.14"]])(
    "rejects invalid MCP_RESOLVE_TIMEOUT_MS=%s",
    (raw) => {
      expect(() => loadConfig({ ...VALID_ENV, MCP_RESOLVE_TIMEOUT_MS: raw })).toThrowError(ConfigError);
      expect(() => loadConfig({ ...VALID_ENV, MCP_RESOLVE_TIMEOUT_MS: raw })).toThrow(
        /MCP_RESOLVE_TIMEOUT_MS/,
      );
    },
  );

  it("rejects empty MCP_ALLOWED_HOSTS after filtering", () => {
    // "," and ", ,," both filter down to []. Must fail loudly to avoid
    // a silent broken-but-running deploy.
    expect(() =>
      loadConfig({ E2A_API_URL: "https://api.example.test", MCP_ALLOWED_HOSTS: "," }),
    ).toThrowError(ConfigError);
    expect(() =>
      loadConfig({ E2A_API_URL: "https://api.example.test", MCP_ALLOWED_HOSTS: ",  ,  ," }),
    ).toThrowError(ConfigError);
  });

  it("accepts a single allowed host with whitespace padding", () => {
    const cfg = loadConfig({ ...VALID_ENV, MCP_ALLOWED_HOSTS: "  mcp.example.test  " });
    expect(cfg.allowedHosts).toEqual(["mcp.example.test"]);
  });

  it("allows port 0 (OS-assigned)", () => {
    const cfg = loadConfig({ ...VALID_ENV, PORT: "0" });
    expect(cfg.port).toBe(0);
  });

  it("defaults E2A_TRUST_PROXY to loopback", () => {
    expect(loadConfig(VALID_ENV).trustProxy).toBe("loopback");
  });

  it("parses E2A_TRUST_PROXY booleans", () => {
    expect(loadConfig({ ...VALID_ENV, E2A_TRUST_PROXY: "true" }).trustProxy).toBe(true);
    expect(loadConfig({ ...VALID_ENV, E2A_TRUST_PROXY: "false" }).trustProxy).toBe(false);
  });

  it("parses a bare integer E2A_TRUST_PROXY as a hop count", () => {
    // Express reads a numeric *string* as a subnet, so it must become a
    // real number to mean "trust N hops".
    expect(loadConfig({ ...VALID_ENV, E2A_TRUST_PROXY: "1" }).trustProxy).toBe(1);
  });

  it("passes through a preset / subnet E2A_TRUST_PROXY verbatim", () => {
    expect(loadConfig({ ...VALID_ENV, E2A_TRUST_PROXY: "uniquelocal" }).trustProxy).toBe("uniquelocal");
    expect(loadConfig({ ...VALID_ENV, E2A_TRUST_PROXY: "10.0.0.0/8" }).trustProxy).toBe("10.0.0.0/8");
  });
});

describe("logJson", () => {
  it("emits a single-line JSON object with severity, event, message and fields", () => {
    const lines: string[] = [];
    const spy = vi.spyOn(process.stderr, "write").mockImplementation((chunk) => {
      lines.push(String(chunk));
      return true;
    });
    logJson("INFO", "listening", "e2a-mcp-http listening on :3000", {
      port: 3000,
      allowedHosts: ["api.e2a.dev"],
    });
    spy.mockRestore();

    expect(lines).toHaveLength(1);
    // Exactly one line: trailing newline, no embedded newlines (so Cloud
    // Logging treats it as a single structured entry).
    expect(lines[0].endsWith("\n")).toBe(true);
    expect(lines[0].trimEnd()).not.toContain("\n");
    expect(JSON.parse(lines[0])).toEqual({
      severity: "INFO",
      event: "listening",
      message: "e2a-mcp-http listening on :3000",
      port: 3000,
      allowedHosts: ["api.e2a.dev"],
    });
  });
});
