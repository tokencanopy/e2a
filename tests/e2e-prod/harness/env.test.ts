import { test } from "node:test";
import assert from "node:assert/strict";
import {
  assertProductionOptIn,
  isProductionTarget,
  resolveSinkEmail,
  resolveSiteUrl,
  type ProdEnv,
} from "./env.ts";

test("resolveSiteUrl prefers an explicit site URL and removes its trailing slash", () => {
  assert.equal(
    resolveSiteUrl("https://api.e2a.dev/", "https://console.example.test/"),
    "https://console.example.test",
  );
});

test("resolveSiteUrl maps the canonical production API origin to the production site", () => {
  assert.equal(resolveSiteUrl("https://api.e2a.dev/"), "https://e2a.dev");
});

test("resolveSiteUrl maps the staging API origin to the staging site", () => {
  assert.equal(
    resolveSiteUrl("https://api-staging.e2a.dev/"),
    "https://staging.e2a.dev",
  );
});

test("resolveSiteUrl keeps other API targets as the site target", () => {
  assert.equal(
    resolveSiteUrl("https://self-hosted.example.test/base/"),
    "https://self-hosted.example.test/base",
  );
});

test("resolveSinkEmail returns the explicit non-empty sink", () => {
  assert.equal(resolveSinkEmail(" sink@example.test "), "sink@example.test");
});

test("resolveSinkEmail rejects a missing or empty sink", () => {
  assert.throws(
    () => resolveSinkEmail(),
    /E2E_SINK_EMAIL.*required/i,
  );
  assert.throws(
    () => resolveSinkEmail("   "),
    /E2E_SINK_EMAIL.*required/i,
  );
});

test("isProductionTarget recognizes the hosted production origins", () => {
  for (const url of [
    "https://e2a.dev",
    "https://e2a.dev/",
    "https://api.e2a.dev",
    "https://API.E2A.DEV/v1",
    "https://www.e2a.dev",
  ]) {
    assert.equal(isProductionTarget(url), true, `${url} should be production`);
  }
});

test("isProductionTarget does not flag staging, localhost, or self-hosted targets", () => {
  for (const url of [
    "https://api-staging.e2a.dev",
    "https://staging.e2a.dev",
    "http://localhost:8080",
    "https://self-hosted.example.test",
    // A lookalike host must not be treated as production, but equally must not
    // be mistaken for one — it is simply somebody else's deployment.
    "https://e2a.dev.evil.example",
    "not a url",
  ]) {
    assert.equal(isProductionTarget(url), false, `${url} should not be production`);
  }
});

test("assertProductionOptIn refuses a production target without the explicit opt-in", () => {
  assert.throws(
    () => assertProductionOptIn("https://e2a.dev", undefined),
    /Refusing to run the destructive e2e-prod suite against production/,
  );
  // Anything other than the exact opt-in value is still a refusal.
  assert.throws(() => assertProductionOptIn("https://api.e2a.dev", "true"), /E2E_ALLOW_PROD=1/);
  assert.throws(() => assertProductionOptIn("https://api.e2a.dev", "0"), /E2E_ALLOW_PROD=1/);
});

test("assertProductionOptIn allows production with the explicit opt-in, and non-production always", () => {
  assert.doesNotThrow(() => assertProductionOptIn("https://e2a.dev", "1"));
  assert.doesNotThrow(() => assertProductionOptIn("https://api-staging.e2a.dev", undefined));
  assert.doesNotThrow(() => assertProductionOptIn("http://localhost:8080", undefined));
});

test("loadEnv fails closed when no target deployment is configured", async () => {
  const saved = { ...process.env };
  for (const k of ["E2A_URL", "E2A_API_URL", "E2A_API_KEY", "E2A_AGENT_EMAIL", "E2A_PRIMARY_AGENT"]) {
    delete process.env[k];
  }
  process.env.E2E_SINK_EMAIL = "sink@example.test";
  // HOME is redirected so a real ~/.e2a/config.json on the developer's machine
  // cannot supply api_url and mask the failure this test is asserting.
  process.env.HOME = "/nonexistent-home-for-e2e-prod-env-test";
  try {
    const { loadEnv } = await import(`./env.ts?no-target=${Date.now()}`);
    assert.throws(() => loadEnv(), /No target deployment/);
  } finally {
    process.env = saved;
  }
});

test("ApiClient can override its base URL without changing existing constructor arguments", async () => {
  const originalApiUrl = process.env.E2A_URL;
  const originalApiKey = process.env.E2A_API_KEY;
  const originalAgentEmail = process.env.E2A_AGENT_EMAIL;
  const originalSinkEmail = process.env.E2E_SINK_EMAIL;
  // client.ts calls loadEnv() at module scope, so the target must be explicit.
  // This test previously set no E2A_URL and silently inherited the old
  // https://e2a.dev default — exactly the footgun the guard removes.
  process.env.E2A_URL = "https://api.example.test";
  process.env.E2A_API_KEY = "test-key";
  process.env.E2A_AGENT_EMAIL = "primary@agents.example.test";
  process.env.E2E_SINK_EMAIL = "sink@example.test";
  const { ApiClient } = await import("./client.ts");
  const env: ProdEnv = {
    apiUrl: "https://api.example.test",
    siteUrl: "https://site.example.test",
    apiKey: "test-key",
    primaryAgentEmail: "primary@agents.example.test",
    sinkEmail: "primary@agents.example.test",
    sharedDomain: "agents.example.test",
    mcpUrl: "https://api.example.test/mcp",
    allowStress: false,
    cleanupMode: "always",
    rateLimitRps: 1,
  };
  const originalFetch = globalThis.fetch;
  let requestedUrl = "";
  globalThis.fetch = async (input) => {
    requestedUrl = String(input);
    return new Response("not found", { status: 404 });
  };

  try {
    const client = new ApiClient(env, 1_000_000, env.siteUrl);
    await client.get("/pricing", { apiKey: null });
  } finally {
    globalThis.fetch = originalFetch;
    if (originalApiUrl === undefined) delete process.env.E2A_URL;
    else process.env.E2A_URL = originalApiUrl;
    if (originalApiKey === undefined) delete process.env.E2A_API_KEY;
    else process.env.E2A_API_KEY = originalApiKey;
    if (originalAgentEmail === undefined) delete process.env.E2A_AGENT_EMAIL;
    else process.env.E2A_AGENT_EMAIL = originalAgentEmail;
    if (originalSinkEmail === undefined) delete process.env.E2E_SINK_EMAIL;
    else process.env.E2E_SINK_EMAIL = originalSinkEmail;
  }

  assert.equal(requestedUrl, "https://site.example.test/pricing");
});
