import { test, after } from "node:test";
import assert from "node:assert/strict";
import { Resolver } from "node:dns/promises";
import { mkdirSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { ApiClient } from "../../harness/client.ts";
import { cleanupDomainFixture } from "../../harness/domain-fixture-cleanup.ts";
import { track } from "../../harness/cleanup.ts";
import { uniqueSlug } from "../../harness/fixtures.ts";
import { writeReport, info, warn, fail } from "../../harness/report.ts";

// PROD-ONLY: the full custom-domain lifecycle PLUS the real SES sending
// identity (DKIM + custom MAIL FROM) — the one axis suites/22-domain-lifecycle.test.ts
// (which this suite's DNS machinery deliberately mirrors) does NOT cover.
// Per suites/prod/README.md, provisioning a real SES sending identity is
// exactly the kind of thing staging structurally cannot do (no unrestricted
// SES identity to provision against), which is why domain.sending_verified /
// domain.sending_failed sit in event_coverage_gate.py's ALLOWLIST today.
//
// Runs against the ISOLATED trymnexa.com Cloudflare zone (never prod e2a.dev),
// opt-in via the same three env vars 22-domain-lifecycle.test.ts uses, and
// skips cleanly when absent:
//   CLOUDFLARE_API_TOKEN / CLOUDFLARE_ZONE_ID / CLOUDFLARE_ZONE_NAME
//
// SEQUENCE:
//   1. register  -> returns ALL applicable DNS records up front (deterministic
//      per internal/httpapi/domains.go domainView): ownership TXT, inbound_mx
//      MX, dkim TXT (per-domain keypair minted at claim time), and — only
//      when the deployment has SESRegion configured (prod does) —
//      mail_from_mx MX + mail_from_spf TXT for the bounce.<domain> subdomain.
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
//   5. poll GET /v1/domains/{domain} for sending_status over a BOUNDED,
//      generous budget. AWS SES identity verification is asynchronous and can
//      legitimately take much longer than any single test run's budget — if
//      it doesn't complete, this is reported plainly (info/warn, never a
//      `fail`) and the event_coverage_gate.py allowlist entries are left in
//      place. This suite does NOT fake completion.
//   6. create a custom-domain agent, confirming domain_verified=true.
//   7. teardown: agent BEFORE domain (domain_has_agents guard), then all 5
//      Cloudflare records unconditionally.
const SUITE = "prod/35-domain-sending-identity";
const client = new ApiClient();

// Event-type coverage recorder for event_coverage_gate.py, same shard
// contract suites/21-webhook-events.test.ts uses (a JSON array of verified
// event-type strings, one file per pid under reports/event-coverage/) —
// deliberately self-contained here rather than a shared harness/ addition,
// mirroring that suite's own reasoning (see its module doc). This suite is
// the ONLY place domain.sending_verified / domain.sending_failed can ever be
// recorded, since they require a real custom-domain SES sending identity
// that no other e2e-prod suite provisions.
const EVENT_COVERAGE_DIR = fileURLToPath(new URL("../../reports/event-coverage/", import.meta.url));
const verifiedEventTypes = new Set<string>();

const CF_TOKEN = process.env.CLOUDFLARE_API_TOKEN;
const CF_ZONE = process.env.CLOUDFLARE_ZONE_ID;
const CF_ZONE_NAME = process.env.CLOUDFLARE_ZONE_NAME;
const skip =
  CF_TOKEN && CF_ZONE && CF_ZONE_NAME
    ? false
    : "CLOUDFLARE_API_TOKEN + CLOUDFLARE_ZONE_ID + CLOUDFLARE_ZONE_NAME not set (isolated conformance DNS zone)";

const CF_API = "https://api.cloudflare.com/client/v4";
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

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
}

async function cfCreateRecord(rec: { type: string; name: string; content: string; priority?: number }): Promise<string> {
  const res = await fetch(`${CF_API}/zones/${CF_ZONE}/dns_records`, {
    method: "POST",
    headers: { Authorization: `Bearer ${CF_TOKEN}`, "Content-Type": "application/json" },
    body: JSON.stringify({ ...rec, ttl: 60, comment: "e2a conformance domain-sending-identity (temporary)" }),
  });
  const j = (await res.json()) as { success: boolean; result?: { id: string }; errors?: unknown };
  if (!j.success || !j.result?.id) throw new Error(`CF ${rec.type} ${rec.name} create failed: ${JSON.stringify(j.errors)}`);
  return j.result.id;
}

async function cfDeleteRecord(id: string): Promise<void> {
  let res: Response;
  try {
    res = await fetch(`${CF_API}/zones/${CF_ZONE}/dns_records/${id}`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${CF_TOKEN}` },
    });
  } catch (e) {
    warn(SUITE, "cf-cleanup", `CF record ${id} delete threw — MANUAL CLEANUP NEEDED: ${String(e)}`);
    return;
  }
  if (!res.ok) {
    warn(SUITE, "cf-cleanup", `CF record ${id} delete failed HTTP ${res.status} — MANUAL CLEANUP NEEDED`);
  }
}

// waitForPublicDns — see 22-domain-lifecycle.test.ts's identical helper for
// the full rationale (order-matters negative-cache trap). Duplicated here
// (not imported) so this file stays self-contained; both suites deliberately
// keep the same logic, not a shared abstraction, per this task's scope
// (avoid touching shared harness/ or the sibling suite while both prod-only
// suites are in flight).
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

// Bounded budget for the ASYNC SES sending-identity poll. Generous but finite
// — AWS SES DKIM/MAIL-FROM verification can legitimately run well past this
// window; the task brief explicitly calls for an honest, bounded attempt
// rather than blocking indefinitely or faking success.
const SENDING_STATUS_BUDGET_MS = 5 * 60 * 1000; // 5 minutes
const SENDING_STATUS_POLL_MS = 15000;

test("domain lifecycle + SES sending identity: register -> DNS (incl. DKIM/MAIL FROM) -> verify -> [best-effort] sending_status -> custom-domain agent -> teardown", { skip }, async () => {
  const domain = `${uniqueSlug("dsi")}.${CF_ZONE_NAME}`;
  const dnsIds: string[] = [];
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
    assert.ok(mailFromMx, "register returns a mail_from_mx MX record (SESRegion is configured in prod)");
    assert.ok(mailFromSpf, "register returns a mail_from_spf TXT record (SESRegion is configured in prod)");

    // 2. publish ALL FIVE records in the isolated zone.
    dnsIds.push(await cfCreateRecord({ type: "TXT", name: ownership!.name, content: ownership!.value }));
    dnsIds.push(await cfCreateRecord({ type: "MX", name: inboundMx!.name, content: inboundMx!.value, priority: inboundMx!.priority ?? 10 }));
    dnsIds.push(await cfCreateRecord({ type: "TXT", name: dkim!.name, content: dkim!.value }));
    dnsIds.push(await cfCreateRecord({ type: "MX", name: mailFromMx!.name, content: mailFromMx!.value, priority: mailFromMx!.priority ?? 10 }));
    dnsIds.push(await cfCreateRecord({ type: "TXT", name: mailFromSpf!.name, content: mailFromSpf!.value }));

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

    // 5. BEST-EFFORT: poll for the async SES sending identity to resolve.
    //    verifyDomain's success already enqueued provisioning
    //    (enqueueSenderProvision) — the DKIM + MAIL FROM records are already
    //    published, so the reconciler can find them whenever AWS gets to it.
    const sinceStart = new Date().toISOString();
    let finalStatus = afterVerify.body!.sending_status;
    const deadline = Date.now() + SENDING_STATUS_BUDGET_MS;
    while (Date.now() < deadline && finalStatus !== "verified" && finalStatus !== "failed") {
      await sleep(SENDING_STATUS_POLL_MS);
      const poll = await client.get<DomainView>(`/v1/domains/${domain}`);
      if (poll.status === 200 && poll.body) {
        finalStatus = poll.body.sending_status;
      }
    }

    if (finalStatus === "verified") {
      const finalDomain = await client.get<DomainView>(`/v1/domains/${domain}`);
      assert.equal(finalDomain.body?.sending_status, "verified");
      assert.equal(finalDomain.body?.capabilities.outbound, "verified", "capabilities.outbound restates sending_status=verified");
      info(SUITE, "sending-identity-verified", `domain ${domain} reached sending_status=verified within the ${SENDING_STATUS_BUDGET_MS / 1000}s budget`);

      // domain.sending_verified is real, live-verifiable — look for it.
      const events = await client.get<{ items: Array<{ type: string; data: Record<string, unknown> }> }>("/v1/events", {
        query: { type: "domain.sending_verified", limit: 50, since: sinceStart },
      });
      const evt = events.body?.items?.find((e) => (e.data as { domain?: string }).domain === domain);
      if (evt) {
        info(SUITE, "sending-verified-event-confirmed", `domain.sending_verified event observed for ${domain}`);
        verifiedEventTypes.add("domain.sending_verified");
      } else {
        // domain.sending_verified is no longer allowlisted in
        // event_coverage_gate.py — this suite is what verifies it. So not
        // finding the event here is not merely informational: the gate will
        // report it UNVERIFIED and fail the run. Said plainly so the gate
        // failure that follows is not a surprise.
        warn(SUITE, "sending-verified-event-not-observed", `sending_status reached verified but no domain.sending_verified event was found in listEvents for ${domain} — the event coverage gate will FAIL, since this suite is its only source`);
      }
    } else if (finalStatus === "failed") {
      const finalDomain = await client.get<DomainView>(`/v1/domains/${domain}`);
      warn(
        SUITE,
        "sending-identity-failed",
        `domain ${domain} sending_status=failed within the budget (sending_error=${JSON.stringify(finalDomain.body?.sending_error)}) — ` +
          `domain.sending_failed may be verifiable; investigate sending_error before removing its allowlist entry. NOT treated as a suite failure — DNS/SES timing is outside this suite's control.`,
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
      warn(
        SUITE,
        "sending-identity-pending",
        `domain ${domain} sending_status still "${finalStatus}" after the ${SENDING_STATUS_BUDGET_MS / 1000}s budget. AWS SES sending-identity ` +
          `(DKIM + custom MAIL FROM) verification is asynchronous and can legitimately take much longer than this budget. ` +
          `domain.sending_verified / domain.sending_failed remain in event_coverage_gate.py's ALLOWLIST — not faked, not removed.`,
      );
    }

    // 6. custom-domain agent, regardless of the sending-identity outcome
    //    above (inbound verification, not sending, is what create-agent needs).
    const agentEmail = `bot@${domain}`;
    track("agent", agentEmail);
    const ag = await client.post<{ email: string; domain_verified: boolean }>("/v1/agents", {
      body: { email: agentEmail, name: "sending-identity bot" },
    });
    assert.equal(ag.status, 201, `create custom-domain agent: ${ag.raw.slice(0, 200)}`);
    assert.equal(ag.body?.domain_verified, true, "custom-domain agent reports domain_verified=true on create");
  } finally {
    // 7. teardown — permanent agent purge first, then transactional domain +
    //    SES deprovision enqueue, then DNS. If API teardown fails, retain DNS
    //    so a still-live provider identity does not lose verification.
    const result = await cleanupDomainFixture(client, dnsIds, cfDeleteRecord);
    if (result.failed.length > 0) {
      fail(
        SUITE,
        "resource-cleanup-failed",
        `preserved ${dnsIds.length} DNS record(s) because ${result.failed.length} API fixture(s) survived teardown`,
        result.failed,
      );
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
