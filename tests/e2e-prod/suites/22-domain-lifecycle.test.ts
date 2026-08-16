import { test, after } from "node:test";
import assert from "node:assert/strict";
import { Resolver } from "node:dns/promises";
import { ApiClient } from "../harness/client.ts";
import { cleanupDomainFixture } from "../harness/domain-fixture-cleanup.ts";
import { CloudflareDnsClient, cloudflareFixtureComment, type CloudflareDnsRecordRef } from "../harness/cloudflare-dns.ts";
import { track } from "../harness/cleanup.ts";
import { uniqueSlug } from "../harness/fixtures.ts";
import { writeReport, fail, info } from "../harness/report.ts";

// The FULL custom-domain lifecycle — the one flow prod e2e structurally can't do
// because it needs REAL DNS: register → publish the verify records → verifyDomain
// HAPPY PATH → create a custom-domain agent → delete → tear down DNS.
//
// It runs against an ISOLATED Cloudflare zone (never prod e2a.dev), so the
// DNS:Edit token can't touch production records. Opt-in via three env vars,
// skips cleanly when absent:
//   CLOUDFLARE_API_TOKEN  — a DNS:Edit token scoped to the conformance zone ONLY
//   CLOUDFLARE_ZONE_ID    — that zone's id
//   CLOUDFLARE_ZONE_NAME  — that zone's apex (e.g. trymnexa.com); conformance
//                           domains are minted as <slug>.<zone-name>
//
// TWO NON-OBVIOUS FACTS the server's verify enforces (learned the hard way):
//  1. `verified` requires the ownership TXT *AND* the inbound MX record pointing
//     at mx-staging.e2a.dev — the TXT alone is NOT enough (see the server's
//     CheckDomainRecords / TestVerifyDomainMXMissing). We publish both.
//  2. The server does a live net.LookupTXT/LookupMX. If we call verify BEFORE the
//     records have propagated, its resolver caches the NEGATIVE for the zone's SOA
//     minimum (trymnexa.com = 1800s / 30min), which no in-test poll can outlast.
//     So we FIRST confirm public propagation via Google Public DNS (8.8.8.8 — the
//     same resolver the GCP VM forwards to) and only THEN trigger the server's
//     first verify. Order is load-bearing, not just an optimization.
const SUITE = "22-domain-lifecycle";
const client = new ApiClient();

const CF_TOKEN = process.env.CLOUDFLARE_API_TOKEN;
const CF_ZONE = process.env.CLOUDFLARE_ZONE_ID;
const CF_ZONE_NAME = process.env.CLOUDFLARE_ZONE_NAME;
const skip =
  CF_TOKEN && CF_ZONE && CF_ZONE_NAME
    ? false
    : "CLOUDFLARE_API_TOKEN + CLOUDFLARE_ZONE_ID + CLOUDFLARE_ZONE_NAME not set (isolated conformance DNS zone)";

const cfDns = new CloudflareDnsClient(CF_ZONE ?? "", CF_TOKEN ?? "");

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

// waitForPublicDns polls Google Public DNS (8.8.8.8) until BOTH the ownership TXT
// and the inbound MX resolve; the caller then waits a short margin before the
// server's first verify. Why order matters: the server does a live
// net.LookupTXT/LookupMX and negative-caches a miss for the zone's SOA minimum
// (trymnexa = 1800s / 30min), which no in-test poll can outlast — so the first
// verify MUST land after the records are visible to the server's resolver.
// IMPORTANT (not oversold): 8.8.8.8 is the closest public proxy for the GCP VM's
// Google-family resolver, but it is NOT the same cache — the VM resolves via
// 169.254.169.254, and Google Public DNS caches per-PoP — so a positive here does
// not PROVE the VM's resolver has it. This SHRINKS, not fully closes, the poison
// window; a rare cross-PoP lag shows up as a false RED that self-clears on retry
// (each run mints a NEW domain, so the poisoned name is never reused). Query
// 8.8.8.8 ONLY (not 1.1.1.1): a Cloudflare-positive / Google-lagging split would
// re-open exactly this window. Explicit-server Resolver so it never reads the
// local cache.
async function waitForPublicDns(domain: string, txtValue: string, mxHost: string): Promise<boolean> {
  const r = new Resolver();
  r.setServers(["8.8.8.8"]);
  // Budget ~180s: CF→public-resolver propagation for a FRESH record is highly
  // variable in practice (observed 12s on fast runs, 60s+ on slow ones). This must
  // comfortably exceed the slow tail — a timeout here is a false RED, and it's far
  // cheaper to wait than to fail the gate. (The subsequent verify is ~0s once this
  // returns, so the total stays modest on the common fast path.)
  for (let i = 0; i < 60; i++) {
    let txtOk = false;
    let mxOk = false;
    try {
      const txts = await r.resolveTxt(domain);
      txtOk = txts.some((chunks) => chunks.join("").includes(txtValue));
    } catch {
      /* NXDOMAIN/ENODATA while propagating — keep polling */
    }
    try {
      const mxs = await r.resolveMx(domain);
      mxOk = mxs.some((m) => m.exchange.replace(/\.$/, "").toLowerCase() === mxHost.toLowerCase());
    } catch {
      /* still propagating */
    }
    if (txtOk && mxOk) {
      info(SUITE, "dns", `TXT+MX public after ~${i * 3}s`);
      return true;
    }
    await sleep(3000);
  }
  return false;
}

test("domain lifecycle: register → DNS TXT+MX → verify (happy path) → custom-domain agent → teardown", { skip }, async () => {
  const domain = `${uniqueSlug("dl")}.${CF_ZONE_NAME}`;
  const dnsRecords: CloudflareDnsRecordRef[] = [];
  let agentEmail: string | undefined;
  track("domain", domain);
  try {
    // 1. register the throwaway domain → returns the records to publish.
    const reg = await client.post<{
      dns_records: Array<{ type: string; name: string; value: string; purpose: string; priority?: number | null }>;
    }>("/v1/domains", { body: { domain } });
    assert.equal(reg.status, 201, `register ${domain}: ${reg.raw.slice(0, 200)}`);
    const txt = reg.body?.dns_records?.find((r) => r.purpose === "ownership" && r.type === "TXT");
    const mx = reg.body?.dns_records?.find((r) => r.purpose === "inbound_mx" && r.type === "MX");
    assert.ok(txt, "register returns an ownership TXT record");
    assert.ok(mx, "register returns an inbound MX record");

    // 2. publish BOTH the ownership TXT and the inbound MX in the ISOLATED zone.
    //    `verified` requires the MX too, not just the TXT. Echo the MX priority
    //    the API returned rather than hardcoding it.
    const comment = cloudflareFixtureComment("domain-lifecycle", domain);
    await cfDns.create(
      { type: "TXT", name: txt!.name, content: txt!.value },
      dnsRecords,
      comment,
    );
    await cfDns.create(
      { type: "MX", name: mx!.name, content: mx!.value, priority: mx!.priority ?? 10 },
      dnsRecords,
      comment,
    );

    // 3. wait for BOTH records to be publicly visible BEFORE the first verify —
    //    otherwise the server negative-caches the miss for the SOA minimum (30min).
    const propagated = await waitForPublicDns(domain, txt!.value, mx!.value);
    assert.ok(propagated, "ownership TXT + inbound MX became publicly resolvable within ~180s");
    // Short margin so the VM's resolver PoP can catch up to 8.8.8.8 before the
    // FIRST verify — a premature miss negative-caches for 30min (unrecoverable).
    await sleep(5000);

    // 4. verifyDomain HAPPY PATH — the server's live lookup now finds both.
    let verified = false;
    for (let i = 0; i < 20; i++) {
      const v = await client.post<{ verified: boolean; mx?: string }>(`/v1/domains/${domain}/verify`);
      if (v.status === 200 && v.body?.verified) {
        verified = true;
        info(SUITE, "verify", `domain verified after ~${i * 3}s`);
        break;
      }
      await sleep(3000);
    }
    assert.ok(verified, "domain reached verified=true after TXT+MX were published and propagated");

    // 5. the domain now reads back verified, and a custom-domain agent can be
    //    created on it and is itself verified.
    const got = await client.get<{ verified: boolean }>(`/v1/domains/${domain}`);
    assert.equal(got.body?.verified, true, "GET domain reflects verified=true");

    agentEmail = `bot@${domain}`;
    track("agent", agentEmail);
    const ag = await client.post<{ email: string; domain_verified: boolean }>("/v1/agents", {
      body: { email: agentEmail, name: "lifecycle bot" },
    });
    assert.equal(ag.status, 201, `create custom-domain agent: ${ag.raw.slice(0, 200)}`);
    // The CREATE response must report the authoritative domain_verified up
    // front — createAgent reads back domains.verified after the INSERT rather
    // than returning a stale in-memory zero (OSS store.go createAgent). It must
    // agree with the READ path, which JOINs the live domain.verified.
    assert.equal(ag.body?.domain_verified, true, "custom-domain agent reports domain_verified=true on create");
    const gotAgent = await client.get<{ domain_verified: boolean }>(`/v1/agents/${encodeURIComponent(agentEmail)}`);
    assert.equal(gotAgent.status, 200, `read custom-domain agent: ${gotAgent.raw.slice(0, 200)}`);
    assert.equal(gotAgent.body?.domain_verified, true, "custom-domain agent reports domain_verified=true on read");
  } finally {
    // 6. teardown — the cleanup registry reverses creation order, so the
    //    agent is permanently purged before the domain delete transaction
    //    enqueues SES deprovisioning. Only then is it safe to remove DNS.
    const result = await cleanupDomainFixture(client, { domain, agent: agentEmail, dnsRecords }, (record) => cfDns.delete(record));
    if (result.failed.length > 0) {
      fail(
        SUITE,
        "resource-cleanup-failed",
        `preserved ${dnsRecords.length} DNS record(s) because ${result.failed.length} API fixture(s) survived teardown`,
        result.failed,
      );
    }
    if (result.dnsFailed.length > 0) {
      fail(SUITE, "dns-cleanup-failed", `${result.dnsFailed.length} DNS record(s) survived teardown`, result.dnsFailed);
    }
  }
});

test("domain verify NEGATIVE control: an unpublished domain does NOT verify (guards against a DNS short-circuit)", { skip }, async () => {
  // A domain with NO DNS published must NOT verify. If the server ever ran in a
  // non-production/dev mode, checkDomainRecords short-circuits to TXTFound:true /
  // MX:"found" with ZERO real lookups — which would silently turn the happy-path
  // test above into a tautology (green while the published DNS is never consulted).
  // This asserts the real DNS path is live. Uses its own throwaway domain that we
  // never verify for real, so poisoning its 30-min negative cache is harmless.
  const domain = `${uniqueSlug("dlneg")}.${CF_ZONE_NAME}`;
  track("domain", domain);
  try {
    const reg = await client.post("/v1/domains", { body: { domain } });
    assert.equal(reg.status, 201, `register ${domain}: ${reg.raw.slice(0, 200)}`);
    const v = await client.post<{ verified: boolean; mx?: string }>(`/v1/domains/${domain}/verify`);
    // The discriminating assertion: verified MUST be false. A dev-mode
    // short-circuit would report verified=true here and fail this test.
    assert.equal(v.body?.verified, false, `unpublished domain must report verified=false, got ${v.status} ${v.raw.slice(0, 160)}`);
    // Not-yet-verified is a normal 200 (branch on .verified), not a 412.
    assert.equal(v.status, 200, `unpublished domain verify → 200 with verified:false, got ${v.status}`);
  } finally {
    const result = await cleanupDomainFixture(client, { domain, dnsRecords: [] }, (record) => cfDns.delete(record));
    if (result.failed.length > 0) {
      fail(SUITE, "negative-control-cleanup-failed", "negative-control domain survived teardown", result.failed);
    }
  }
});

after(async () => {
  await writeReport(`./reports/${SUITE}.json`);
});
