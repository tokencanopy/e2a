import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { uniqueSlug } from "../harness/fixtures.ts";
import { writeReport } from "../harness/report.ts";

// Limit/quota ENFORCEMENT can only be exercised against a STANDARD-class,
// low-cap account. The main conformance account is internal-class → unmetered,
// rate-limit-exempt, and huge caps by construction, so a green conformance run
// does NOT attest that enforcement works. This suite uses a SEPARATE opt-in
// account (E2A_QUOTA_API_KEY / E2A_QUOTA_AGENT_EMAIL, provisioned by
// `e2a-prober seed-conformance`) to close that gap. It skips cleanly when the
// quota account isn't provisioned. Cleanup is inline (per-account, via q) rather
// than the shared cleanup harness, which is single-client.
const SUITE = "15-quota-enforcement";
const base = new ApiClient();
const q = base.env.quotaApiKey
  ? new ApiClient({
      ...base.env,
      apiKey: base.env.quotaApiKey,
      primaryAgentEmail: base.env.quotaAgentEmail ?? base.env.primaryAgentEmail,
    })
  : null;
const skip = q ? false : "E2A_QUOTA_API_KEY not set (standard-class quota account not provisioned)";

interface LimitErr {
  error?: { code?: string; details?: { resource?: string; limit?: number; current?: number } };
}

test("quota: agent-count cap is enforced (402 limit_exceeded)", { skip }, async () => {
  // Create slug agents until the account's max_agents cap trips. The cap is a
  // 402 limit_exceeded carrying resource + current/limit in details.
  const created: string[] = [];
  let capped: { status: number; body: LimitErr | null; raw: string } | null = null;
  try {
    for (let i = 0; i < 20; i++) {
      const slug = uniqueSlug("quota");
      const r = await q!.post<{ email: string }>("/v1/agents", {
        body: { email: `${slug}@${q!.env.sharedDomain}`, name: "quota-cap" },
      });
      if (r.status === 201) {
        created.push(r.body!.email);
        continue;
      }
      capped = r as { status: number; body: LimitErr | null; raw: string };
      break;
    }
    assert.ok(capped, `expected the agent cap to trip within 20 creates (created ${created.length})`);
    assert.equal(capped.status, 402, `agent cap → 402 limit_exceeded, got ${capped.status}: ${capped.raw.slice(0, 200)}`);
    assert.equal(capped.body?.error?.code, "limit_exceeded");
    assert.equal(capped.body?.error?.details?.resource, "agents");
    assert.equal(typeof capped.body?.error?.details?.limit, "number", "limit_exceeded carries the numeric cap");
    assert.ok(created.length >= 1, "at least one create succeeded before the cap");
  } finally {
    for (const email of created) {
      await q!.delete(`/v1/agents/${encodeURIComponent(email)}?confirm=DELETE&permanent=true`);
    }
  }
});

test("quota: domain-count cap is enforced (402 limit_exceeded)", { skip }, async () => {
  // The quota account owns no domain initially (agents live on the shared
  // domain), so max_domains=1 → the 2nd register trips the cap. RFC-2606
  // .example.com names can never verify against real DNS — fine, we only test
  // the register-count cap.
  const created: string[] = [];
  let capped: { status: number; body: LimitErr | null; raw: string } | null = null;
  try {
    for (let i = 0; i < 5; i++) {
      const domain = `q-${uniqueSlug("d").replace(/[^a-z0-9-]/g, "")}.example.com`;
      const r = await q!.post<{ domain: string }>("/v1/domains", { body: { domain } });
      if (r.status === 201) {
        created.push(r.body!.domain);
        continue;
      }
      capped = r as { status: number; body: LimitErr | null; raw: string };
      break;
    }
    assert.ok(capped, `expected the domain cap to trip (created ${created.length})`);
    assert.equal(capped.status, 402, `domain cap → 402, got ${capped.status}: ${capped.raw.slice(0, 200)}`);
    assert.equal(capped.body?.error?.code, "limit_exceeded");
    assert.equal(capped.body?.error?.details?.resource, "domains");
  } finally {
    for (const domain of created) {
      await q!.delete(`/v1/domains/${encodeURIComponent(domain)}?confirm=DELETE`);
    }
  }
});

after(async () => {
  await writeReport(`./reports/${SUITE}.json`);
});
