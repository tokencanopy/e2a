import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { cleanup, track, untrack } from "../harness/cleanup.ts";
import { info, warn, writeReport } from "../harness/report.ts";
import { uniqueSlug, SINK_EMAIL } from "../harness/fixtures.ts";

const client = new ApiClient();
const SUITE = "09-postfix";

after(async () => {
  const r = await cleanup(client);
  if (r.failed.length) warn(SUITE, "cleanup", `failed ${r.failed.length}`, r.failed);
  writeReport(`./reports/09-postfix.json`);
  assert.equal(r.failed.length, 0, `final cleanup failed for ${r.failed.length} resources`);
});

const MAX_CLEANUP_RETRY_MS = 2_000;

async function deleteStressProbeAgent(email: string): Promise<void> {
  const path = `/v1/agents/${encodeURIComponent(email)}?confirm=DELETE&permanent=true`;
  let result = await client.delete(path);
  if (result.status === 429) {
    const retryAfterSeconds = Number(result.headers["retry-after"]);
    const requestedMs = Number.isFinite(retryAfterSeconds) && retryAfterSeconds >= 0
      ? retryAfterSeconds * 1_000
      : 1_000;
    await new Promise((resolve) => setTimeout(resolve, Math.min(requestedMs, MAX_CLEANUP_RETRY_MS)));
    result = await client.delete(path);
  }
  if (result.status === 200 || result.status === 204 || result.status === 404) {
    untrack("agent", email);
    return;
  }
  assert.fail(`stress-probe cleanup failed for ${email}: HTTP ${result.status} ${result.raw.slice(0, 200)}`);
}

test("postfix #4: GET nonexistent path returns 404 JSON error envelope", async () => {
  const r = await client.get("/v1/this/does/not/exist");
  assert.equal(r.status, 404, `expected 404, got ${r.status}`);
  const ct = r.headers["content-type"] ?? "";
  // Unknown paths resolve to the standard structured error envelope
  // (application/json {error:{code}}), same as every other 4xx — not a bare
  // text/plain 404. The point — a bounded, non-empty error, never empty/500 — holds.
  assert.ok(ct.startsWith("application/json"), `expected application/json, got "${ct}"`);
  assert.ok(r.raw.trim().length > 0, "body should be non-empty");
  info(SUITE, "404-shape", `Content-Type: "${ct}", text: "${r.raw.trim()}"`);
});

test("postfix #4: wrong-method on /info returns 405 JSON error", async () => {
  // A known path with an unsupported method resolves to 405 method-not-allowed
  // with the standard application/json error envelope (consistent with the
  // error-contract suite: post-info=405, put-messages=405, delete-messages=405).
  // The point of this test — a bounded, non-empty error, never empty/500 — holds.
  const r = await client.post("/v1/info", { body: {} });
  assert.equal(r.status, 405, `expected 405, got ${r.status}`);
  const ct = r.headers["content-type"] ?? "";
  assert.ok(ct.startsWith("application/json"), `expected application/json, got "${ct}"`);
  assert.ok(r.raw.trim().length > 0, "body should be non-empty");
  info(SUITE, "wrong-method-shape", `Content-Type: "${ct}", text: "${r.raw.trim()}"`);
});

test("postfix #4: wrong-method on /messages returns 405 JSON error", async () => {
  // See the /info case above — wrong method on a known path resolves to 405
  // method-not-allowed with an application/json error envelope.
  const email = client.env.primaryAgentEmail;
  const r = await client.put(`/v1/agents/${encodeURIComponent(email)}/messages`, { body: {} });
  assert.equal(r.status, 405, `expected 405, got ${r.status}`);
  const ct = r.headers["content-type"] ?? "";
  assert.ok(ct.startsWith("application/json"), `expected application/json, got "${ct}"`);
});

test("postfix #6: GET /agents/{email} is case-insensitive (lowercase + uppercase match)", async () => {
  const email = client.env.primaryAgentEmail;
  const lower = await client.get(`/v1/agents/${encodeURIComponent(email.toLowerCase())}`);
  const upper = await client.get(`/v1/agents/${encodeURIComponent(email.toUpperCase())}`);
  assert.equal(lower.status, 200, "lowercase form should resolve");
  assert.equal(upper.status, 200, `uppercase form should also resolve, got ${upper.status}: ${upper.raw.slice(0, 200)}`);
  if (lower.body && upper.body) {
    // Both should resolve to the same canonical agent.
    const lEmail = (lower.body as { email?: string }).email;
    const uEmail = (upper.body as { email?: string }).email;
    assert.equal(lEmail, uEmail, "both case forms should resolve to the same canonical email");
  }
});

test("postfix #6: GET /agents/{email} with mixed-case still matches", async () => {
  const email = client.env.primaryAgentEmail;
  const local = email.split("@")[0];
  const domain = email.split("@")[1];
  const mixed = local.toUpperCase() + "@" + domain.toLowerCase();
  const r = await client.get(`/v1/agents/${encodeURIComponent(mixed)}`);
  assert.equal(r.status, 200, `mixed-case email should match, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("postfix #7: /send with CRLF in subject is rejected at the API (400)", async () => {
  const r = await client.post(`/v1/agents/${encodeURIComponent(client.env.primaryAgentEmail)}/messages`, {
    body: {
      to: [SINK_EMAIL],
      subject: "Hello\r\nBcc: attacker@evil.com",
      text: "x",
    },
  });
  assert.equal(r.status, 400, `expected 400 (CRLF rejected), got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.ok(/CR|LF|\\r|\\n|newline|line/i.test(r.raw), `expected error mentioning CR/LF, got: ${r.raw.slice(0, 200)}`);
  info(SUITE, "crlf-rejected", `text: "${r.raw.trim()}"`);
});

test("postfix #7: bare LF in subject is also rejected (no carriage return)", async () => {
  const r = await client.post(`/v1/agents/${encodeURIComponent(client.env.primaryAgentEmail)}/messages`, {
    body: {
      to: [SINK_EMAIL],
      subject: "Hello\nX-Smuggled: yes",
      text: "x",
    },
  });
  assert.equal(r.status, 400, `expected 400 for bare LF, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("postfix #1 #2: /agents 429 includes Retry-After header (active probe — does NOT send mail)", async (t) => {
  if (!client.env.allowStress) {
    t.skip("set E2E_PROD_STRESS=1 to run the 25-attempt registration rate-limit probe");
    return;
  }
  // Agent creation is a pure CRUD op; failing creates don't fan out to SMTP.
  // Probe until we see a 429 OR exhaust 25 attempts.
  let saw429 = false;
  let retryAfter: string | undefined;
  for (let i = 0; i < 25; i++) {
    const requestedEmail = `${uniqueSlug(`pf${i}`)}@${client.env.sharedDomain}`;
    track("agent", requestedEmail);
    const r = await client.post<{ email?: string }>("/v1/agents", {
      body: { email: requestedEmail, name: "pf" },
    });
    if (r.status !== 201) untrack("agent", requestedEmail);
    if (r.status === 429) {
      saw429 = true;
      retryAfter = r.headers["retry-after"];
      break;
    }
    if (r.status === 201) {
      assert.equal(r.body?.email, requestedEmail, `successful stress-probe create returned an unexpected email: ${r.raw.slice(0, 200)}`);
      await deleteStressProbeAgent(requestedEmail);
    }
  }
  if (!saw429) {
    info(SUITE, "no-429-hit", "did not hit /agents rate limit in 25 attempts — limit higher than expected");
    return;
  }
  assert.ok(retryAfter, "429 must carry Retry-After header");
  const secs = Number(retryAfter);
  assert.ok(!Number.isNaN(secs) && secs > 0, `Retry-After should be positive seconds, got "${retryAfter}"`);
  info(SUITE, "retry-after-ok", `429 carries Retry-After: ${retryAfter}s — fix landed`);
});
