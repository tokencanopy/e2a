import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { Resolver } from "node:dns/promises";
import { mkdirSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { ApiClient } from "../harness/client.ts";
import { isProductionTarget } from "../harness/env.ts";
import { cleanupDomainFixture } from "../harness/domain-fixture-cleanup.ts";
import { CloudflareDnsClient, cloudflareFixtureComment, type CloudflareDnsRecordRef } from "../harness/cloudflare-dns.ts";
import { track } from "../harness/cleanup.ts";
import { uniqueSlug } from "../harness/fixtures.ts";
import { writeReport, info, warn, fail } from "../harness/report.ts";

// The full custom-domain lifecycle PLUS the real SES sending identity (DKIM +
// custom MAIL FROM) — the one axis suites/22-domain-lifecycle.test.ts (which
// this suite's DNS machinery deliberately mirrors) does NOT cover.
//
// THIS SUITE USED TO LIVE IN suites/prod/ AND ONLY RUN AGAINST PRODUCTION.
// That is why a real sender-identity regression shipped to prod on
// 2026-08-16: config.staging.yaml omitted `sender_identity`, so the whole
// subsystem was dark on staging, and this was the ONLY suite exercising it —
// nightly, non-blocking, prod-only. e2a-ops PR #318 gave staging its own
// `sender_identity.ses_region` plus a dedicated AWS IAM user whose policy
// Deny-fences every mutating SES action to `identity/*.staging.trymnexa.com`.
// Staging can now execute this path for real, so this suite now lives here
// in suites/ instead: `npm test` (staging) glob-excludes suites/prod/, so a
// suite has to live here to gate anything on that target. `npm run test:prod`
// runs suites/ AND suites/prod/, so production coverage is unaffected by the
// move — see suites/prod/README.md for why file-glob selection, not a
// runtime skip, is this repo's convention for "prod can, staging can't".
//
// ENVIRONMENT-AWARE FIXTURE NAMING — the coupling most likely to break this
// suite in a confusing way. See fixtureDomainSuffix() below: staging MUST
// register domains under <slug>.staging.trymnexa.com, prod keeps
// <slug>.trymnexa.com. Get this wrong and the staging IAM fence denies the
// SES provisioning call — the suite fails closed (safely — nothing
// mutates outside the fenced namespace), but the error reads like a code
// bug, not a naming mismatch. Keep the derivation obvious and in one place.
//
// SEQUENCE:
//   1. register  -> returns ALL applicable DNS records up front (deterministic
//      per internal/httpapi/domains.go domainView): ownership TXT, inbound_mx
//      MX, dkim TXT (per-domain keypair minted at claim time), and — only
//      when the deployment has SESRegion configured (both prod and staging,
//      post e2a-ops PR #318) — mail_from_mx MX + mail_from_spf TXT for the
//      bounce.<domain> subdomain.
//   2. publish ALL FIVE records in the isolated zone.
//   3. wait for the ownership TXT + inbound MX to be PUBLICLY resolvable
//      (8.8.8.8) before the first verify — the same negative-DNS-cache trap
//      22-domain-lifecycle.test.ts documents (verify's live net.LookupTXT/MX
//      caches a miss for the zone's SOA minimum, ~30min on trymnexa.com,
//      unrecoverable within the run). This trap is specific to the server's
//      OWN verify probe; it does NOT apply to the DKIM/MAIL FROM records
//      below, which SES's reconciler checks via AWS's own DNS lookups on its
//      own schedule (internal/senderidentity), not our verify endpoint.
//   4. verifyDomain -> verified=true, which auto-enqueues SES sending-identity
//      provisioning (internal/httpapi/domains.go enqueueSenderProvision).
//   5. HARD GATE (~60s budget): sending_status must leave "none". A missing
//      IAM action, an AccessDenied, or a domain name that falls outside the
//      staging fence's identity/*.staging.trymnexa.com scope all present
//      IDENTICALLY here: internal/senderidentity/worker.go's
//      syncProviderIdentityWithInspection returns the raw provider error
//      without ever calling store.SetSendingStatus, so the domain sits at
//      the "none" default forever. This is deterministic and fast — it does
//      NOT depend on AWS's own async verification timing — and it is the
//      assertion that would have caught the 2026-08-16 incident.
//   6. LONGER, ALSO-FAILING GATE (~600s budget): sending_status must reach
//      "verified" and domain.sending_verified must be observed via
//      listEvents. This DOES depend on AWS's own async DKIM/MAIL-FROM
//      verification timing, so — unlike the hard gate above — it is
//      legitimately more exposed to flakiness under AWS-side variance. If it
//      proves flaky in practice, ONLY this assertion may be downgraded back
//      to a non-fatal warn() (see the DOWNGRADE NOTE at its call site) — the
//      hard gate is fully independent and keeps catching the actual
//      incident class on its own.
//   7. create a custom-domain agent, confirming domain_verified=true.
//   8. teardown: agent BEFORE domain (domain_has_agents guard), then all 5
//      Cloudflare records only after the API confirms SES deletion. Failed
//      API cleanup preserves DNS (cleanupDomainFixture's own tradeoff — see
//      its module doc). Teardown is then VERIFIED, not assumed: a follow-up
//      GET must 404 the domain and the Cloudflare API must agree every
//      record is actually gone — a 200 from either API is not proof by
//      itself. This matters because it wasn't proof in practice: production
//      currently carries 8 stale dl-*.trymnexa.com fixtures dating to
//      2026-08-08 (a different suite, 22-domain-lifecycle.test.ts, and a
//      different — non-staging — namespace, but the same underlying
//      cleanupDomainFixture code path and the same "trust the response"
//      gap this suite now closes for itself).
//
// PRE-RUN SWEEPER: staging runs this suite on every promotion (far more
// often than prod's nightly cadence), so a crashed run's leftovers — which
// skip the `finally` teardown above entirely (see harness/cleanup.ts's
// abnormal-exit leak reporter doc) — compound fast. sweepStaleStagingFixtures()
// below reaps them at suite start, strictly scoped to fixture-shaped domains
// under .staging.trymnexa.com; see its own doc for the layered safety
// argument (regex shape, environment check, age floor, and the IAM fence
// itself as a backstop: DeleteEmailIdentity is allowed in-namespace, so
// staging can reap its own identities but never prod's).
const SUITE = "35-domain-sending-identity";
const client = new ApiClient();

// Event-type coverage recorder for event_coverage_gate.py, same shard
// contract suites/21-webhook-events.test.ts uses (a JSON array of verified
// event-type strings, one file per pid under reports/event-coverage/) —
// deliberately self-contained here rather than a shared harness/ addition,
// mirroring that suite's own reasoning (see its module doc). This suite is
// the ONLY place domain.sending_verified / domain.sending_failed can ever be
// recorded, since they require a real custom-domain SES sending identity
// that no other e2e-prod suite provisions.
const EVENT_COVERAGE_DIR = fileURLToPath(new URL("../reports/event-coverage/", import.meta.url));
const verifiedEventTypes = new Set<string>();

const CF_TOKEN = process.env.CLOUDFLARE_API_TOKEN;
const CF_ZONE = process.env.CLOUDFLARE_ZONE_ID;
const CF_ZONE_NAME = process.env.CLOUDFLARE_ZONE_NAME;
const skip =
  CF_TOKEN && CF_ZONE && CF_ZONE_NAME
    ? false
    : "CLOUDFLARE_API_TOKEN + CLOUDFLARE_ZONE_ID + CLOUDFLARE_ZONE_NAME not set (isolated conformance DNS zone)";

const cfDns = new CloudflareDnsClient(CF_ZONE ?? "", CF_TOKEN ?? "");
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

// Environment-aware fixture-domain suffix — THE central, obvious place this
// suite decides which Cloudflare zone-subtree a fixture domain lives under.
// Do NOT let this drift back to a bare `${uniqueSlug(...)}.${CF_ZONE_NAME}`
// (what this suite used before it ran on staging, and what the sibling
// suites/22-domain-lifecycle.test.ts — which never provisions a real SES
// sending identity, so it has nothing for the fence to deny — still uses
// today). The reason is the staging AWS IAM policy from e2a-ops PR #318,
// which Deny-fences every mutating SES action (CreateEmailIdentity,
// PutEmailIdentityDkimSigningAttributes, SetIdentityMailFromDomain,
// DeleteEmailIdentity, ...) to `identity/*.staging.trymnexa.com`. A fixture
// domain registered without that exact suffix on a staging run gets
// AccessDenied the instant SES provisioning runs — which reads exactly like
// a code bug, not a naming mismatch. Driven by the SAME apiUrl-derived
// signal (isProductionTarget) that already gates the destructive-prod
// opt-in and the event-coverage-gate's target detection — not a new,
// independently-settable flag.
function fixtureDomainSuffix(apiUrl: string, zoneName: string): string {
  return isProductionTarget(apiUrl) ? zoneName : `staging.${zoneName}`;
}

interface DNSRecordView {
  type: string;
  name: string;
  value: string;
  priority: number | null;
  purpose: string;
  status: string;
}
interface DomainView {
  domain: string;
  verified: boolean;
  dns_records: DNSRecordView[];
  sending_status: string;
  sending_error?: string;
  capabilities: { inbound: string; outbound: string };
  created_at: string;
}

// waitForPublicDns — see 22-domain-lifecycle.test.ts's identical helper for
// the full rationale (order-matters negative-cache trap). Duplicated here
// (not imported) so this file stays self-contained; both suites deliberately
// keep the same logic, not a shared abstraction, per this task's scope
// (avoid touching shared harness/ or the sibling suite while both are in
// flight independently).
async function waitForPublicDns(domain: string, txtValue: string, mxHost: string): Promise<boolean> {
  const r = new Resolver();
  r.setServers(["8.8.8.8"]);
  for (let i = 0; i < 60; i++) {
    let txtOk = false;
    let mxOk = false;
    try {
      const txts = await r.resolveTxt(domain);
      txtOk = txts.some((chunks) => chunks.join("").includes(txtValue));
    } catch {
      /* propagating */
    }
    try {
      const mxs = await r.resolveMx(domain);
      mxOk = mxs.some((m) => m.exchange.replace(/\.$/, "").toLowerCase() === mxHost.toLowerCase());
    } catch {
      /* propagating */
    }
    if (txtOk && mxOk) {
      info(SUITE, "dns", `ownership TXT + inbound MX public after ~${i * 3}s`);
      return true;
    }
    await sleep(3000);
  }
  return false;
}

// --- Pre-run sweeper --------------------------------------------------------
//
// Reaps leftover *.staging.trymnexa.com fixture domains from PREVIOUS
// crashed/killed runs. This is the part that actually prevents accumulation:
// this suite's own teardown (in the test's `finally` below) only recovers
// from ITS OWN failure — a hard kill (OOM, CI cancel) skips `finally`
// entirely, and harness/cleanup.ts's abnormal-exit leak reporter can only
// print on the way out, never delete (see its module doc for why). Staging
// runs this suite on every promotion — far more often than prod's nightly
// cadence — so leakage compounds fast without something that also recovers
// earlier runs' failures, not just this one's.
//
// Evidence this class of leak is real: production currently carries 8 stale
// dl-*.trymnexa.com fixtures dating to 2026-08-08. Those came from a
// different suite (22-domain-lifecycle.test.ts) in a different (non-staging)
// namespace, but through the exact same mechanism this suite's own teardown
// also uses — cleanupDomainFixture deliberately preserving DNS on an
// ambiguous API failure (a reasonable safety choice on its own, but one that
// accumulates garbage without a sweep on top of it).
//
// SAFETY — deliberately layered, not resting on any one check alone:
//   1. STAGING_FIXTURE_DOMAIN requires the exact .staging.trymnexa.com
//      suffix AND the uniqueSlug() shape (harness/fixtures.ts:
//      <prefix>-<6 hex>-<6 hex>). Prod's bare *.trymnexa.com fixtures, and
//      any real customer domain anywhere, never match this regex.
//   2. sweepStaleStagingFixtures() also refuses to run at all on a
//      production target — belt-and-suspenders, and it avoids scanning a
//      prod account's full domain list for zero benefit (nothing there can
//      match the regex above).
//   3. Only domains older than SWEEP_MIN_AGE_MS are touched — comfortably
//      longer than this suite's own worst-case single run, so a sibling
//      process's fixture that is still legitimately mid-run is never swept
//      out from under it.
//   4. Even if 1–3 all failed simultaneously, the staging AWS IAM policy
//      itself (e2a-ops PR #318) is a fourth, independent backstop:
//      DeleteEmailIdentity is allowed only in-namespace, so staging's own
//      server credentials cannot delete an SES identity outside
//      identity/*.staging.trymnexa.com no matter what this sweeper asks for.
const STAGING_FIXTURE_DOMAIN = /^[a-z][a-z0-9]*-[0-9a-f]{6}-[0-9a-f]{6}\.staging\.trymnexa\.com$/;
const SWEEP_MIN_AGE_MS = 3 * 60 * 60 * 1000; // 3h; this suite's own worst case is well under 15min end to end.
const SWEEP_MAX_PAGES = 50; // bound the /v1/domains walk on a very large account rather than hang.

async function sweepStaleStagingFixtures(): Promise<void> {
  if (isProductionTarget(client.env.apiUrl)) return; // see SAFETY #2 above.

  const cutoff = Date.now() - SWEEP_MIN_AGE_MS;
  const stale: DomainView[] = [];
  let cursor: string | undefined;
  let pages = 0;
  try {
    do {
      const res = await client.get<{ items: DomainView[]; next_cursor?: string | null }>("/v1/domains", {
        query: { cursor, limit: 100 },
      });
      if (res.status !== 200 || !res.body) {
        warn(SUITE, "sweep-list-failed", `GET /v1/domains returned HTTP ${res.status} while sweeping; skipping the sweep this run`);
        return;
      }
      for (const d of res.body.items) {
        if (STAGING_FIXTURE_DOMAIN.test(d.domain) && new Date(d.created_at).getTime() < cutoff) {
          stale.push(d);
        }
      }
      cursor = res.body.next_cursor ?? undefined;
      pages++;
    } while (cursor && pages < SWEEP_MAX_PAGES);
    if (cursor) {
      warn(SUITE, "sweep-truncated", `stopped the sweep scan after ${SWEEP_MAX_PAGES} pages with more domains remaining; some stale fixtures may not have been reaped this run`);
    }
  } catch (e) {
    warn(SUITE, "sweep-error", `error listing domains while sweeping: ${e instanceof Error ? e.message : String(e)}`);
    return;
  }

  if (stale.length === 0) {
    info(SUITE, "sweep", "no stale staging fixture domains found");
    return;
  }
  info(SUITE, "sweep", `found ${stale.length} stale staging fixture domain(s) older than ${SWEEP_MIN_AGE_MS / 3_600_000}h; reaping`);

  for (const d of stale) {
    try {
      // Reconstruct the DNS refs from the domain's OWN current dns_records
      // (register/get return them deterministically — see internal/httpapi/
      // domains.go domainView), so CloudflareDnsClient's exact type+name(+
      // content) match can find them without needing this run's own
      // in-memory tracking (which a crashed prior run never left behind).
      const dnsRecords: CloudflareDnsRecordRef[] = d.dns_records
        .filter((r) => r.type === "TXT" || r.type === "MX")
        .map((r) => ({ type: r.type, name: r.name, content: r.value }));
      // Same fixture shape this suite itself always creates (step 7 below);
      // a 404 on a stale run that never got that far is fine — see
      // harness/cleanup.ts's identical note on why a not-found delete is
      // harmless.
      const result = await cleanupDomainFixture(
        client,
        { domain: d.domain, agent: `bot@${d.domain}`, dnsRecords },
        (record) => cfDns.delete(record),
      );
      if (result.failed.length > 0 || result.dnsFailed.length > 0) {
        warn(
          SUITE,
          "sweep-incomplete",
          `stale domain ${d.domain} did not fully tear down (${result.failed.length} API fixture(s), ${result.dnsFailed.length} DNS record(s) survived) — left for a later sweep or manual cleanup`,
          { failed: result.failed, dnsFailed: result.dnsFailed },
        );
        continue;
      }
      // VERIFY, not assume — same discipline as this run's own teardown below.
      const check = await client.get(`/v1/domains/${encodeURIComponent(d.domain)}`);
      if (check.status !== 404) {
        warn(SUITE, "sweep-not-verified", `stale domain ${d.domain} still resolves (HTTP ${check.status}) after sweep teardown reported success`);
        continue;
      }
      info(SUITE, "swept", `reaped stale staging fixture domain ${d.domain} (created ${d.created_at})`);
    } catch (e) {
      warn(SUITE, "sweep-error", `error reaping stale domain ${d.domain}: ${e instanceof Error ? e.message : String(e)}`);
    }
  }
}

before(async () => {
  if (skip) return;
  await sweepStaleStagingFixtures();
});

test("domain lifecycle + SES sending identity: register -> DNS (incl. DKIM/MAIL FROM) -> verify -> sending_status hard gate -> [best-effort, also-failing] sending_status verified -> custom-domain agent -> teardown (verified)", { skip }, async () => {
  const domain = `${uniqueSlug("dsi")}.${fixtureDomainSuffix(client.env.apiUrl, CF_ZONE_NAME!)}`;
  const dnsRecords: CloudflareDnsRecordRef[] = [];
  let agentEmail: string | undefined;
  track("domain", domain);
  try {
    // 1. register
    const reg = await client.post<DomainView>("/v1/domains", { body: { domain } });
    assert.equal(reg.status, 201, `register ${domain}: ${reg.raw.slice(0, 200)}`);

    const records = reg.body?.dns_records ?? [];
    const ownership = records.find((r) => r.purpose === "ownership" && r.type === "TXT");
    const inboundMx = records.find((r) => r.purpose === "inbound_mx" && r.type === "MX");
    const dkim = records.find((r) => r.purpose === "dkim" && r.type === "TXT");
    const mailFromMx = records.find((r) => r.purpose === "mail_from_mx" && r.type === "MX");
    const mailFromSpf = records.find((r) => r.purpose === "mail_from_spf" && r.type === "TXT");

    assert.ok(ownership, "register returns an ownership TXT record");
    assert.ok(inboundMx, "register returns an inbound MX record");
    assert.ok(dkim, "register returns a dkim TXT record (per-domain DKIM keypair minted at claim time)");
    assert.ok(mailFromMx, "register returns a mail_from_mx MX record (SESRegion is configured)");
    assert.ok(mailFromSpf, "register returns a mail_from_spf TXT record (SESRegion is configured)");

    // 2. publish ALL FIVE records in the isolated zone.
    const comment = cloudflareFixtureComment("domain-sending-identity", domain);
    await cfDns.create({ type: "TXT", name: ownership!.name, content: ownership!.value }, dnsRecords, comment);
    await cfDns.create({ type: "MX", name: inboundMx!.name, content: inboundMx!.value, priority: inboundMx!.priority ?? 10 }, dnsRecords, comment);
    await cfDns.create({ type: "TXT", name: dkim!.name, content: dkim!.value }, dnsRecords, comment);
    await cfDns.create({ type: "MX", name: mailFromMx!.name, content: mailFromMx!.value, priority: mailFromMx!.priority ?? 10 }, dnsRecords, comment);
    await cfDns.create({ type: "TXT", name: mailFromSpf!.name, content: mailFromSpf!.value }, dnsRecords, comment);

    // 3. wait for ownership TXT + inbound MX to be publicly visible BEFORE
    //    the first verify (negative-cache trap; see module doc).
    const propagated = await waitForPublicDns(domain, ownership!.value, inboundMx!.value);
    assert.ok(propagated, "ownership TXT + inbound MX became publicly resolvable within ~180s");
    await sleep(5000); // margin for the VM resolver's PoP to catch up to 8.8.8.8

    // 4. verifyDomain HAPPY PATH.
    let verified = false;
    for (let i = 0; i < 20; i++) {
      const v = await client.post<{ verified: boolean }>(`/v1/domains/${domain}/verify`);
      if (v.status === 200 && v.body?.verified) {
        verified = true;
        info(SUITE, "verify", `domain verified after ~${i * 3}s`);
        break;
      }
      await sleep(3000);
    }
    assert.ok(verified, "domain reached verified=true after ownership TXT + inbound MX were published and propagated");

    const afterVerify = await client.get<DomainView>(`/v1/domains/${domain}`);
    assert.equal(afterVerify.status, 200);
    assert.equal(afterVerify.body?.verified, true, "GET domain reflects verified=true");
    assert.equal(afterVerify.body?.capabilities.inbound, "verified", "capabilities.inbound restates verified=true");

    const sinceStart = new Date().toISOString();
    let status = afterVerify.body!.sending_status;
    const SENDING_STATUS_POLL_MS = 5000;

    // 5. HARD GATE (~60s budget): sending_status must leave "none". See the
    //    module doc's step 5 for exactly why this is deterministic and why
    //    it is the check that would have caught the 2026-08-16 incident.
    const SENDING_LEAVES_NONE_BUDGET_MS = 60 * 1000;
    {
      const deadline = Date.now() + SENDING_LEAVES_NONE_BUDGET_MS;
      while (Date.now() < deadline && status === "none") {
        await sleep(SENDING_STATUS_POLL_MS);
        const poll = await client.get<DomainView>(`/v1/domains/${domain}`);
        if (poll.status === 200 && poll.body) status = poll.body.sending_status;
      }
      assert.notEqual(
        status,
        "none",
        `sending_status stayed "none" for ${SENDING_LEAVES_NONE_BUDGET_MS / 1000}s after verify — the SES sending-identity provisioning call never ran. ` +
          `An AccessDenied, a missing IAM action, or a fixture domain outside the staging IAM fence's identity/*.staging.trymnexa.com scope all present ` +
          `exactly this way (internal/senderidentity/worker.go's syncProviderIdentityWithInspection returns the raw provider error without ever calling ` +
          `store.SetSendingStatus). This is the assertion that would have caught the 2026-08-16 production incident.`,
      );
      info(SUITE, "sending-status-left-none", `sending_status reached "${status}" within ${SENDING_LEAVES_NONE_BUDGET_MS / 1000}s of verify (hard gate)`);
    }

    // 6. LONGER, ALSO-FAILING GATE (~600s budget — ~5x the ~110s observed in
    //    production run 32186793272): sending_status must reach "verified"
    //    and domain.sending_verified must be observed. Unlike the hard gate
    //    above, this depends on AWS's own async DKIM/MAIL-FROM verification
    //    timing, not just our own code path.
    const SENDING_VERIFIED_BUDGET_MS = 600 * 1000;
    {
      const deadline = Date.now() + SENDING_VERIFIED_BUDGET_MS;
      while (Date.now() < deadline && status !== "verified" && status !== "failed") {
        await sleep(SENDING_STATUS_POLL_MS);
        const poll = await client.get<DomainView>(`/v1/domains/${domain}`);
        if (poll.status === 200 && poll.body) status = poll.body.sending_status;
      }

      if (status === "failed") {
        // domain.sending_failed stays non-fatal here AND in
        // event_coverage_gate.py's ALWAYS_ALLOWLIST — no suite anywhere
        // induces a real AWS-classified failure on purpose, so reaching
        // "failed" is unexpected but not something to assert against without
        // a way to force it deliberately.
        const finalDomain = await client.get<DomainView>(`/v1/domains/${domain}`);
        warn(
          SUITE,
          "sending-identity-failed",
          `domain ${domain} sending_status=failed within the budget (sending_error=${JSON.stringify(finalDomain.body?.sending_error)}) — ` +
            `NOT treated as a suite failure (the hard gate above already proved provisioning ran); investigate sending_error before removing domain.sending_failed's allowlist entry.`,
        );
        const events = await client.get<{ items: Array<{ type: string; data: Record<string, unknown> }> }>("/v1/events", {
          query: { type: "domain.sending_failed", limit: 50, since: sinceStart },
        });
        const evt = events.body?.items?.find((e) => (e.data as { domain?: string }).domain === domain);
        if (evt) {
          info(SUITE, "sending-failed-event-confirmed", `domain.sending_failed event observed for ${domain} — remove its event_coverage_gate.py ALLOWLIST entry`);
          verifiedEventTypes.add("domain.sending_failed");
        }
      } else {
        // DOWNGRADE NOTE: if AWS-side DKIM/MAIL-FROM verification timing
        // makes ONLY this assertion flaky in practice, it is safe to turn
        // this assert.equal (and the event assert.ok below) into warn()
        // calls without losing incident-class coverage — the hard gate above
        // is fully independent and already proves the provisioning calls
        // themselves succeeded, which is the actual thing the 2026-08-16
        // incident got wrong. Do not downgrade the hard gate.
        assert.equal(
          status,
          "verified",
          `sending_status did not reach "verified" (still "${status}") within the ${SENDING_VERIFIED_BUDGET_MS / 1000}s budget. ` +
            `This run's own provisioning calls succeeded (the hard gate above passed), so this most likely reflects real AWS-side DKIM/MAIL-FROM ` +
            `verification latency beyond the ~5x-observed budget rather than a code regression — see the DOWNGRADE NOTE above.`,
        );
        const finalDomain = await client.get<DomainView>(`/v1/domains/${domain}`);
        assert.equal(finalDomain.body?.sending_status, "verified");
        assert.equal(finalDomain.body?.capabilities.outbound, "verified", "capabilities.outbound restates sending_status=verified");
        info(SUITE, "sending-identity-verified", `domain ${domain} reached sending_status=verified within the ${SENDING_VERIFIED_BUDGET_MS / 1000}s budget`);

        const events = await client.get<{ items: Array<{ type: string; data: Record<string, unknown> }> }>("/v1/events", {
          query: { type: "domain.sending_verified", limit: 50, since: sinceStart },
        });
        const evt = events.body?.items?.find((e) => (e.data as { domain?: string }).domain === domain);
        assert.ok(
          evt,
          `sending_status reached "verified" but no domain.sending_verified event was found via listEvents for ${domain} — ` +
            `domain.sending_verified is no longer allowlisted in event_coverage_gate.py, and this suite is its only source of real emission.`,
        );
        info(SUITE, "sending-verified-event-confirmed", `domain.sending_verified event observed for ${domain}`);
        verifiedEventTypes.add("domain.sending_verified");
      }
    }

    // 7. custom-domain agent, regardless of the sending-identity outcome
    //    above (inbound verification, not sending, is what create-agent needs).
    agentEmail = `bot@${domain}`;
    track("agent", agentEmail);
    const ag = await client.post<{ email: string; domain_verified: boolean }>("/v1/agents", {
      body: { email: agentEmail, name: "sending-identity bot" },
    });
    assert.equal(ag.status, 201, `create custom-domain agent: ${ag.raw.slice(0, 200)}`);
    assert.equal(ag.body?.domain_verified, true, "custom-domain agent reports domain_verified=true on create");
  } finally {
    // 8. teardown — permanent agent purge first, then the transactional
    //    domain delete (commits the durable teardown job; a best-effort
    //    immediate deprovision usually confirms provider absence before the
    //    200, with async convergence as the guarantee), then DNS. If API
    //    teardown fails, retain DNS so a still-live provider identity does
    //    not lose verification.
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

    // VERIFY, not assume — a 200/204 from either API is not proof by itself.
    // Production currently carries 8 stale dl-*.trymnexa.com fixtures dating
    // to 2026-08-08 that slipped through exactly this "trust the response"
    // gap (a different suite, a different namespace, the same underlying
    // cleanupDomainFixture path). Confirm both halves independently.
    if (result.failed.length === 0) {
      const stillThere = await client.get(`/v1/domains/${encodeURIComponent(domain)}`);
      if (stillThere.status !== 404) {
        fail(
          SUITE,
          "domain-not-actually-gone",
          `GET /v1/domains/${domain} returned HTTP ${stillThere.status} after teardown reported success — the SES sending identity may still exist`,
          { status: stillThere.status },
        );
      }
    }
    if (result.dnsFailed.length === 0) {
      for (const record of dnsRecords) {
        let stillExists: boolean;
        try {
          stillExists = await cfDns.exists(record);
        } catch (e) {
          warn(SUITE, "dns-verify-error", `could not verify removal of ${record.type} ${record.name}: ${e instanceof Error ? e.message : String(e)}`);
          continue;
        }
        if (stillExists) {
          fail(SUITE, "dns-not-actually-removed", `${record.type} ${record.name} still resolves via the Cloudflare API after teardown reported success`, record);
        }
      }
    }
  }
});

after(async () => {
  if (verifiedEventTypes.size > 0) {
    mkdirSync(EVENT_COVERAGE_DIR, { recursive: true });
    writeFileSync(`${EVENT_COVERAGE_DIR}${process.pid}.json`, JSON.stringify([...verifiedEventTypes]));
  }
  await writeReport(`./reports/${SUITE}.json`);
});
