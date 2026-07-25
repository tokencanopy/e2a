import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../../harness/client.ts";
import { info, writeReport } from "../../harness/report.ts";

// PROD-ONLY, READ-ONLY: the billing sidecar's plan/quota catalog endpoint,
// GET /api/billing/plan (billing/internal/api/handlers.go handleGetPlan).
// This talks to the SAME LIVE Stripe account the prod billing sidecar uses
// for real checkout/portal sessions, so this suite touches NOTHING mutating —
// no checkout session, no portal session, no subscription, nothing under
// /api/billing/checkout or /api/billing/portal. GET only.
//
// Why this belongs under suites/prod/ rather than suites/ alongside the
// existing 13-billing-surface.test.ts CSRF/shape checks: the billing sidecar
// is a hosted-prod-only component (e2a-ops' billing/) that does not run on
// staging at all (staging has no billing sidecar container), so this
// endpoint is structurally unreachable there — see suites/prod/README.md's
// "what belongs here" list, which this suite extends.
//
// AUTH SHAPE: billing/internal/api/session.go's AuthenticateRequest reads
// ONLY a session cookie (`e2a_session` — set by the Next.js dashboard's OAuth
// login flow), never a Bearer API key. An e2e-prod harness that only carries
// API keys therefore has NO way to mint a valid session cookie and can never
// exercise the 200 catalog+current-plan body from outside a real browser
// OAuth round-trip — that gap is documented explicitly below rather than
// faked. What IS fully black-box-provable, and asserted here: the endpoint
// exists, is GET, and correctly rejects an unauthenticated (no-cookie)
// request with 401 — the same negative-space CSRF/authn discipline
// 13-billing-surface.test.ts already applies to /api/billing/checkout and
// /api/billing/portal.
const SUITE = "31-billing-plan";
const client = new ApiClient();
const siteClient = new ApiClient(client.env, undefined, client.env.siteUrl);

test("billing: GET /api/billing/plan without a session cookie returns 401 (not authenticated)", async () => {
  // apiKey:null strips the Authorization header the harness would otherwise
  // send; this endpoint never reads Bearer auth at all (cookie-only), but
  // stripping it keeps the request unambiguously "no credentials of any kind".
  const r = await siteClient.get("/api/billing/plan", { apiKey: null });
  assert.equal(r.status, 401, `GET /api/billing/plan with no session cookie expected 401, got ${r.status}: ${r.raw.slice(0, 300)}`);
  // handleGetPlan's authenticate() shim writes http.Error(w, "not authenticated", 401)
  // — a plain-text body, not a JSON envelope (this endpoint predates the v1
  // ErrorEnvelope convention and is intentionally out of the v1 contract).
  assert.ok(
    r.raw.toLowerCase().includes("not authenticated") || r.raw.length === 0,
    `expected a plain "not authenticated" body (or empty), got: ${r.raw.slice(0, 200)}`,
  );
});

test("billing: GET /api/billing/plan with a bearer API key (not a session cookie) still 401s", async () => {
  // Sends the real e2a API key as Bearer auth — proves the endpoint doesn't
  // accidentally accept API-key auth as a session-cookie substitute (it must
  // not: API keys are account/agent-scoped for the v1 surface, not a login
  // session, and accepting them here would let any API-key holder read
  // Stripe-adjacent billing state through the wrong auth mechanism).
  const r = await siteClient.get("/api/billing/plan");
  assert.equal(r.status, 401, `GET /api/billing/plan with a Bearer API key (no session cookie) expected 401, got ${r.status}: ${r.raw.slice(0, 300)}`);
});

test("billing: GET /api/billing/plan response shape — documented, not exercisable black-box (recorded finding)", async () => {
  // The 200 shape (PlanInfo: {catalog: PlanEntry[], current: CurrentState})
  // is defined in billing/internal/api/handlers.go and is the SSOT the
  // pricing page must stay manually in sync with (AGENTS.md). It cannot be
  // black-box-verified from this API-key-only harness because the sidecar's
  // authenticate() shim accepts only a live user_sessions cookie minted by
  // the dashboard's OAuth login — there is no service-account / API-key path
  // to it, by design (billing/internal/api/session.go AuthenticateRequest).
  // Recorded as an explicit, honest gap rather than silently omitted or faked
  // with a stubbed cookie.
  info(
    SUITE,
    "plan-200-shape-not-exercisable",
    "GET /api/billing/plan's 200 body (PlanInfo: catalog[] + current) requires a live dashboard OAuth session cookie " +
      "(billing/internal/api/session.go AuthenticateRequest is cookie-only, no Bearer/API-key path) — not obtainable from " +
      "an API-key-only e2e-prod harness. Only the auth boundary (401 unauthenticated, asserted above) is black-box " +
      "verifiable here; the catalog/current shape is covered by billing's own Go unit tests (billing/internal/api/handlers_test.go) instead.",
  );
});

after(async () => {
  await writeReport(`./reports/${SUITE}.json`);
});
