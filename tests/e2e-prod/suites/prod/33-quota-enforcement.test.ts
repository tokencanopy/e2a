import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../../harness/client.ts";
import { uniqueSlug } from "../../harness/fixtures.ts";
import { fail, info, writeReport } from "../../harness/report.ts";

// PROD-ONLY quota-cap enforcement, driven with the dedicated STANDARD-class
// account (E2A_QUOTA_API_KEY / E2A_QUOTA_AGENT_EMAIL: 5 agents / 1 domain).
// This is genuinely prod-shaped work even though staging's e2e harness has an
// analogous suite (15-quota-enforcement.test.ts) against its own staging
// quota account: 402 limit_exceeded enforcement is plan-code/billing-catalog
// behavior (internal/limits + billing/internal/plans.go), and the whole point
// of this exercise is proving it fires for real against the LIVE prod plan
// catalog with the account actually provisioned for prod, not inferring it
// from staging. Assertion depth here goes beyond 15's: every LimitExceededDetails
// field (resource, limit, current, plan_code) is checked, matching the exact
// wire shape from internal/httpapi/errors.go.
//
// The main conformance account (E2A_API_KEY) is internal-class: unmetered,
// rate-limit-exempt, huge caps — it can NEVER produce a 402 limit_exceeded, so
// it cannot stand in for this suite. E2A_QUOTA_API_KEY is required; its
// absence is a hard failure (not a silent skip) because this env is
// self-provisioned for exactly this purpose per the run's setup contract.
//
// RE-RUNNABLE: every created agent/domain is deleted in a `finally`, so a
// second consecutive run starts from the same empty baseline. Do not leave
// the account AT its cap — the whole point of the cleanup discipline is that
// this suite can run indefinitely without manual intervention.
const SUITE = "prod/33-quota-enforcement";
const base = new ApiClient();

if (!base.env.quotaApiKey) {
  throw new Error(
    "E2A_QUOTA_API_KEY is required for the prod-only quota-enforcement suite (standard-class, 5 agents/1 domain). " +
      "This is a hard failure, not a skip: the prod run is provisioned with this account specifically for quota enforcement.",
  );
}
const q = new ApiClient({
  ...base.env,
  apiKey: base.env.quotaApiKey,
  primaryAgentEmail: base.env.quotaAgentEmail ?? base.env.primaryAgentEmail,
});

interface LimitExceededBody {
  error?: {
    code?: string;
    message?: string;
    details?: {
      resource?: string;
      limit?: number;
      current?: number;
      plan_code?: string;
      upgrade_url?: string;
    };
  };
}

function assertLimitExceededShape(
  r: { status: number; body: LimitExceededBody | null; raw: string },
  expectedResource: string,
): void {
  assert.equal(r.status, 402, `expected 402 limit_exceeded, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.equal(r.body?.error?.code, "limit_exceeded", `error.code must be limit_exceeded: ${r.raw.slice(0, 200)}`);
  const d = r.body?.error?.details;
  assert.ok(d, `error.details must be present on limit_exceeded: ${r.raw.slice(0, 200)}`);
  assert.equal(d!.resource, expectedResource, `error.details.resource must be "${expectedResource}"`);
  assert.equal(typeof d!.limit, "number", `error.details.limit must be a number, got ${JSON.stringify(d!.limit)}`);
  assert.equal(typeof d!.current, "number", `error.details.current must be a number, got ${JSON.stringify(d!.current)}`);
  assert.ok(
    d!.current! >= d!.limit!,
    `error.details.current (${d!.current}) should be >= limit (${d!.limit}) at the moment the cap tripped`,
  );
  // plan_code is `omitempty` in the OpenAPI schema (LimitExceededDetails), so a
  // standard-class account SHOULD carry one, but tolerate its absence rather
  // than hard-failing on an explicitly optional field — record it either way.
  if (typeof d!.plan_code === "string" && d!.plan_code.length > 0) {
    info(SUITE, `${expectedResource}-plan-code`, `plan_code="${d!.plan_code}"`);
  } else {
    fail(SUITE, `${expectedResource}-missing-plan-code`, `error.details.plan_code missing/empty on a standard-class 402 — clients can't show "you're on plan X, upgrade to Y": ${r.raw.slice(0, 300)}`);
  }
}

test("prod quota: agent-count cap (5) is enforced with full LimitExceededDetails, and is re-runnable", async () => {
  const created: string[] = [];
  try {
    let capped: { status: number; body: LimitExceededBody | null; raw: string } | null = null;
    for (let i = 0; i < 12; i++) {
      const slug = uniqueSlug("pquota-agent");
      const r = await q.post<{ email: string }>("/v1/agents", {
        body: { email: `${slug}@${q.env.sharedDomain}`, name: "prod-quota-agent-cap" },
      });
      if (r.status === 201) {
        assert.ok(r.body?.email, "created agent has an email");
        created.push(r.body!.email);
        continue;
      }
      capped = r as { status: number; body: LimitExceededBody | null; raw: string };
      break;
    }
    assert.ok(capped, `expected the agent cap to trip within 12 creates (created ${created.length} — is the quota account's plan/cap misconfigured?)`);
    assertLimitExceededShape(capped!, "agents");
    assert.ok(created.length >= 1 && created.length <= 5, `at least one create should succeed before a 5-agent cap trips (created ${created.length})`);
    info(SUITE, "agent-cap-trip", `cap tripped after ${created.length} successful creates`);
  } finally {
    for (const email of created) {
      const d = await q.delete(`/v1/agents/${encodeURIComponent(email)}?confirm=DELETE&permanent=true`);
      if (![200, 204, 404].includes(d.status)) {
        fail(SUITE, "agent-cleanup-failed", `delete agent ${email} returned ${d.status}: ${d.raw.slice(0, 200)} — MANUAL CLEANUP MAY BE NEEDED (would strand the account at cap on the next run)`);
      }
    }
  }
});

test("prod quota: domain-count cap (1) is enforced with full LimitExceededDetails, and is re-runnable", async () => {
  // The quota account's agents live on the shared domain, so it owns zero
  // domains at baseline — max_domains=1 means the SECOND register trips the
  // cap. RFC-2606 .example.com names can never verify against real DNS, which
  // is fine: this only exercises the register-count cap, never verification.
  const created: string[] = [];
  try {
    let capped: { status: number; body: LimitExceededBody | null; raw: string } | null = null;
    for (let i = 0; i < 4; i++) {
      const domain = `pq-${uniqueSlug("d").replace(/[^a-z0-9-]/g, "")}.example.com`;
      const r = await q.post<{ domain: string }>("/v1/domains", { body: { domain } });
      if (r.status === 201) {
        assert.ok(r.body?.domain, "registered domain echoes its name");
        created.push(r.body!.domain);
        continue;
      }
      capped = r as { status: number; body: LimitExceededBody | null; raw: string };
      break;
    }
    assert.ok(capped, `expected the domain cap to trip within 4 registers (created ${created.length})`);
    assertLimitExceededShape(capped!, "domains");
    assert.equal(created.length, 1, `exactly one domain register should succeed before a 1-domain cap trips (created ${created.length})`);
    info(SUITE, "domain-cap-trip", `cap tripped after ${created.length} successful register(s)`);
  } finally {
    // No agents were ever created on these domains, so plain delete (no
    // domain_has_agents risk) — but still domains-only, agents-before-domains
    // ordering is moot here since there are no agents in this test.
    for (const domain of created) {
      const d = await q.delete(`/v1/domains/${encodeURIComponent(domain)}?confirm=DELETE`);
      if (![200, 204, 404].includes(d.status)) {
        fail(SUITE, "domain-cleanup-failed", `delete domain ${domain} returned ${d.status}: ${d.raw.slice(0, 200)} — MANUAL CLEANUP MAY BE NEEDED (would strand the account at cap on the next run)`);
      }
    }
  }
});

test("prod quota: account left at baseline — quota account owns no leftover agents/domains from this suite", async () => {
  // Belt-and-suspenders re-runnability check: list what the quota account
  // actually owns right now and confirm none of OUR run's slugs linger (a
  // failed cleanup above would already have recorded a `fail`, but this
  // catches a cleanup that silently no-op'd, e.g. a 200 that didn't actually
  // delete). No account-global assertion ("exactly N") — only checks for the
  // run's own slug prefixes.
  const agents = await q.get<{ items: Array<{ email: string }> }>("/v1/agents", { query: { limit: 100 } });
  const domains = await q.get<{ items: Array<{ domain: string }> }>("/v1/domains", { query: { limit: 100 } });
  const leakedAgents = (agents.body?.items ?? []).filter((a) => a.email.includes("pquota-agent"));
  const leakedDomains = (domains.body?.items ?? []).filter((d) => d.domain.startsWith("pq-"));
  if (leakedAgents.length > 0) {
    fail(SUITE, "leaked-agents", `quota account still owns ${leakedAgents.length} agent(s) from this suite: ${leakedAgents.map((a) => a.email).join(", ")}`);
  }
  if (leakedDomains.length > 0) {
    fail(SUITE, "leaked-domains", `quota account still owns ${leakedDomains.length} domain(s) from this suite: ${leakedDomains.map((d) => d.domain).join(", ")}`);
  }
  assert.equal(leakedAgents.length, 0, "no leftover quota-suite agents");
  assert.equal(leakedDomains.length, 0, "no leftover quota-suite domains");
});

after(async () => {
  await writeReport(`./reports/${SUITE}.json`);
});
