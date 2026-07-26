import { test, after } from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { ApiClient } from "../harness/client.ts";
import { uniqueSlug, uniqueSubject } from "../harness/fixtures.ts";
import { writeReport, info } from "../harness/report.ts";

// PRODUCTION-ONLY: a genuine agent-to-agent send over the REAL wire — SES
// egress, a real external MX hop back into e2a's own inbound SMTP listener —
// as opposed to the internal loopback shortcut (internal/loopback/, taken
// only when IsSelfSend(req, agentEmail) is true: a single To recipient that
// IS the sender's own address, no Cc/Bcc). Staging cannot exercise this at
// all: a distinct agent→agent send there egresses via SES and fails (no
// external MX for agents-staging.e2a.dev to route back through) — see this
// directory's README. Production's agents.e2a.dev has a live MX
// (_dc-mx.<hex>.agents.e2a.dev, INFRASTRUCTURE.md) that resolves back to the
// same VM's real SMTP listener, so a distinct-agent send genuinely round-trips
// through SES and back in, and is the only way to black-box prove the wire
// path (as opposed to the loopback's internal short-circuit) actually works.
//
// Distinguishing the two paths: loopback unconditionally hardcodes
// envelope_from=nil and authentication=nil on every event it builds
// (internal/loopback/screening_events.go; internal/agent/selfsend.go sets
// EnvelopeFrom: nil explicitly) — it never touches the SMTP listener. A real
// wire delivery's envelope_from is populated from the actual SMTP MAIL FROM
// the inbound listener observed (internal/relay/server.go: Mail(from) →
// extractEmail → EmailReceivedData.EnvelopeFrom). For a SHARED-domain agent
// (agents.e2a.dev, not a customer's own sending-verified domain), sender.go's
// useOwnAddressFrom is false, so e2a itself composes the header From address
// as the e2a-owned relay address (agent@send.e2a.dev in prod), NOT the
// sending agent's own address (live-probed 2026-07-25 — this initially
// surprised an assumption that header_from would be the agent's own address,
// which only holds for a customer's sending-verified domain). SES then
// separately rewrites the wire envelope Return-Path under its OWN
// custom-MAIL-FROM subdomain (mail.send.e2a.dev) to a VERP-style bounce
// token distinct from what e2a submitted. So header_from and envelope_from
// differ from EACH OTHER, but both are non-null and both are categorically
// different from the sending agent's address — a combination loopback can
// never produce (its header_from IS the agent's own address, and its
// envelope_from is always null) — which is exactly what this suite asserts.
//
// Webhook delivery is proved to the same bar suites/21-webhook-events.test.ts
// established: event-scoped fanout (delivery_status.matched_webhooks>=1) AND
// webhook-scoped delivery attempts>=1 (our fresh webhook's HTTP leg actually
// fired — the delivery worker signs the payload per internal/webhook/
// subscriber_deliverer.go's X-E2A-Signature: t=<unix>,v1=<hmac-sha256> scheme
// before dispatch). This suite does not itself decode that header — doing so
// black-box would require an internet-reachable HTTPS capture endpoint this
// harness does not provision (attempting to stand one up against a
// convenience third-party capture service was refused by this environment's
// own network policy; deploying dedicated capture infra is a bigger, separate
// decision than writing this test) — so "HMAC verified" here means what it
// means everywhere else in this suite: a real signed delivery attempt ran,
// proved the same dual-assertion way. Flagged explicitly rather than silently
// assumed; a follow-up with a real capture endpoint would tighten this to an
// actual byte-level signature check.
const SUITE = "29-inbound-mx";
const client = new ApiClient();

const EVENT_COVERAGE_DIR = fileURLToPath(new URL("../../reports/event-coverage/", import.meta.url));
const verifiedEventTypes = new Set<string>();

interface EventView {
  id: string;
  type: string;
  schema_version: string;
  created_at: string;
  status: string;
  data: Record<string, unknown>;
  agent_email?: string;
  message_id?: string;
  delivery_status?: { matched_webhooks?: number };
}
interface PageEventView {
  items: EventView[];
  next_cursor: string | null;
}
interface WebhookDeliveryView {
  id: string;
  type: string;
  status: string;
  attempts: number;
}
interface PageWebhookDeliveryView {
  items: WebhookDeliveryView[];
  next_cursor: string | null;
}
interface CreateWebhookResponse {
  id: string;
  url: string;
  events: string[];
  signing_secret: string;
}
interface SendResult {
  status?: string;
  message_id?: string;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const sinceNow = () => new Date(Date.now() - 5000).toISOString();

async function createAgent(label: string): Promise<string> {
  const slug = uniqueSlug(label);
  const c = await client.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${client.env.sharedDomain}`, name: `inbound-mx ${label}` },
  });
  if (c.status !== 201 || !c.body?.email) {
    throw new Error(`create agent failed: ${c.status} ${c.raw.slice(0, 200)}`);
  }
  return c.body.email;
}
async function delAgent(email: string): Promise<void> {
  await client.delete(`/v1/agents/${encodeURIComponent(email)}?confirm=DELETE`);
}
async function createHook(events: string[]): Promise<CreateWebhookResponse> {
  const r = await client.post<CreateWebhookResponse>("/v1/webhooks", {
    body: { url: "https://example.com/e2e-prod-inbound-mx", events, description: `e2e-prod ${uniqueSlug("whmx")}` },
  });
  assert.equal(r.status, 201, `create webhook expected 201, got ${r.status}: ${r.raw.slice(0, 200)}`);
  return r.body!;
}
async function delHook(id: string): Promise<void> {
  await client.delete(`/v1/webhooks/${encodeURIComponent(id)}?confirm=DELETE`);
}

async function pollEvent(
  params: { type: string; agentId: string; since: string },
  match: (e: EventView) => boolean,
  timeoutMs = 30000,
): Promise<EventView | null> {
  const deadline = Date.now() + timeoutMs;
  let delay = 500;
  while (Date.now() < deadline) {
    const r = await client.get<PageEventView>("/v1/events", {
      query: { type: params.type, agent_email: params.agentId, since: params.since, limit: 50 },
    });
    if (r.status === 200 && r.body?.items) {
      const found = r.body.items.find(match);
      if (found) return found;
    }
    await sleep(delay);
    delay = Math.min(Math.floor(delay * 1.5), 3000);
  }
  return null;
}
async function pollEventFanout(eventId: string, timeoutMs = 20000): Promise<number | null> {
  const deadline = Date.now() + timeoutMs;
  let delay = 500;
  while (Date.now() < deadline) {
    const r = await client.get<EventView>(`/v1/events/${eventId}`);
    const n = r.body?.delivery_status?.matched_webhooks ?? 0;
    if (r.status === 200 && n >= 1) return n;
    await sleep(delay);
    delay = Math.min(Math.floor(delay * 1.5), 3000);
  }
  return null;
}
async function pollDelivery(webhookId: string, eventType: string, timeoutMs = 20000): Promise<WebhookDeliveryView | null> {
  const deadline = Date.now() + timeoutMs;
  let delay = 500;
  while (Date.now() < deadline) {
    const r = await client.get<PageWebhookDeliveryView>(`/v1/webhooks/${webhookId}/deliveries`);
    if (r.status === 200 && r.body?.items) {
      const found = r.body.items.find((d) => d.type === eventType && d.attempts >= 1);
      if (found) return found;
    }
    await sleep(delay);
    delay = Math.min(Math.floor(delay * 1.5), 3000);
  }
  return null;
}

test("real wire round trip: agent A → agent B egresses via SES and re-enters over the real inbound MX", async () => {
  const agentA = await createAgent("mxa");
  const agentB = await createAgent("mxb");
  const hook = await createHook(["email.sent", "email.received"]);
  const since = sinceNow();
  try {
    const subject = uniqueSubject("real wire round trip");
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(agentA)}/messages`, {
      body: { to: [agentB], subject, text: "genuine agent-to-agent send over the real inbound MX" },
    });
    // Distinct recipient (agentB != agentA) → NOT a self-send: this is a real
    // external send, accepted for async delivery (202), not the loopback's
    // synchronous 200.
    assert.equal(send.status, 202, `distinct-agent send expected 202 accepted, got ${send.status}: ${send.raw.slice(0, 200)}`);
    assert.equal(send.body?.status, "accepted", "a real (non-loopback) send is accepted for async delivery");
    const sentMessageId = send.body!.message_id!;
    assert.ok(sentMessageId?.startsWith("msg_"), "send returns a msg_ id");

    // ---- Leg 1: the message actually egresses via SES (email.sent on A) ----
    const sentEv = await pollEvent({ type: "email.sent", agentId: agentA, since }, (e) =>
      e.message_id === sentMessageId || e.data.message_id === sentMessageId,
    );
    assert.ok(sentEv, `email.sent event for ${sentMessageId} must appear within 30s (proves SES egress)`);
    const sentFanout = await pollEventFanout(sentEv!.id);
    assert.ok(sentFanout, `email.sent event ${sentEv!.id} must fan out to >=1 webhook`);
    const sentDel = await pollDelivery(hook.id, "email.sent");
    assert.ok(sentDel, "a delivery attempt for email.sent must appear on our webhook");
    info(SUITE, "email.sent", `egress confirmed: evt=${sentEv!.id} fanned to ${sentFanout} webhook(s), whd=${sentDel!.id} attempts=${sentDel!.attempts}`);
    verifiedEventTypes.add("email.sent");

    // ---- Leg 2: the message re-enters via the REAL inbound MX (email.received on B) ----
    // Correlated by the unique subject (the inbound copy's message_id is
    // distinct from the outbound send's, same as the self-send tests in
    // 21-webhook-events.test.ts).
    const receivedEv = await pollEvent({ type: "email.received", agentId: agentB, since }, (e) => e.data.subject === subject);
    assert.ok(receivedEv, `email.received event for subject ${JSON.stringify(subject)} must appear within 30s (proves the real MX round trip)`);
    assert.equal(receivedEv!.data.direction, "inbound", "received payload carries direction=inbound");
    assert.equal(receivedEv!.data.delivered_to, agentB, "received payload's delivered_to is agent B");

    // THE distinguishing assertion: loopback unconditionally nils both
    // envelope_from and authentication (screening_events.go / selfsend.go).
    // A real wire delivery populates both from the actual inbound SMTP
    // session the listener observed.
    //
    // Empirically (live-probed 2026-07-25, production): for a SHARED-domain
    // agent (not a customer's own sending-verified domain), sender.go's
    // useOwnAddressFrom is false, so the header From address falls back to
    // the e2a-owned relay address e2a itself composed
    // (`agent@<outbound_smtp.from_domain>` — prod: agent@send.e2a.dev) — NOT
    // the sending agent's own address (an initial assumption that the header
    // From stays the agent's own address was wrong; it only holds for a
    // customer's sending-verified domain). SES then independently rewrites
    // the wire envelope Return-Path under its OWN custom-MAIL-FROM subdomain
    // (INFRASTRUCTURE.md: "custom MAIL FROM at mail.send.e2a.dev") to a
    // VERP-style bounce token, e.g.
    // `010f...@mail.send.e2a.dev` — SES's own generated address, distinct
    // from what e2a submitted. So header_from and envelope_from are NOT
    // equal to each other on the wire, but BOTH are non-null and BOTH are
    // categorically different from the sending agent's own address — a
    // combination loopback can never produce (its header_from IS the
    // agent's own address, and its envelope_from is always null).
    const envelopeFrom = receivedEv!.data.envelope_from;
    const headerFrom = receivedEv!.data.header_from;
    assert.ok(
      typeof envelopeFrom === "string" && envelopeFrom.length > 0,
      `envelope_from must be populated for a real wire delivery (loopback always nils it); got ${JSON.stringify(envelopeFrom)}`,
    );
    assert.ok(
      typeof headerFrom === "string" && headerFrom.length > 0,
      `header_from must be populated; got ${JSON.stringify(headerFrom)}`,
    );
    assert.notEqual(
      String(envelopeFrom).toLowerCase(),
      agentA.toLowerCase(),
      "envelope_from is SES's own return-path, not the sending agent's address — proves this transited SES, not the loopback shortcut",
    );
    assert.notEqual(
      String(headerFrom).toLowerCase(),
      agentA.toLowerCase(),
      "header_from is e2a's own relay address for a shared-domain send, not the sending agent's own address (that equality only holds for a customer's sending-verified domain)",
    );
    // Assert the SHAPE, not a literal domain. What proves the wire path is that
    // envelope_from is a VERP bounce token under SES's custom MAIL FROM
    // subdomain — NOT that the domain is the production one. Hardcoding
    // send.e2a.dev made this suite fail on staging purely because its relay is
    // send-staging.e2a.dev, even though the round trip completed correctly.
    //   prod:    ...@mail.send.e2a.dev
    //   staging: ...@mail.send-staging.e2a.dev
    // The distinguishing property against a loopback short-circuit is the same
    // either way: a loopback never leaves e2a, so envelope_from would be the
    // agent's own address rather than a provider-generated return-path.
    assert.match(
      String(envelopeFrom).toLowerCase(),
      /^[^@]+@mail\.send(-[a-z0-9-]+)?\.e2a\.dev$/,
      `envelope_from should be SES's VERP return-path under the deployment's custom MAIL FROM subdomain (mail.send.e2a.dev on prod, mail.send-staging.e2a.dev on staging) — that is what proves the message left via SES and re-entered over the MX rather than short-circuiting through loopback. Got ${JSON.stringify(envelopeFrom)}`,
    );
    // Shape, not a literal domain — same reasoning as envelope_from above. The
    // relay's from_domain is deployment-specific (send.e2a.dev on prod,
    // send-staging.e2a.dev on staging); what matters is that header_from is the
    // RELAY's address and not the sending agent's, which the assert.equal above
    // already pins.
    assert.match(
      String(headerFrom).toLowerCase(),
      /^[^@]+@send(-[a-z0-9-]+)?\.e2a\.dev$/,
      `header_from should be the deployment's own outbound relay address (agent@send.e2a.dev on prod, agent@send-staging.e2a.dev on staging), got ${JSON.stringify(headerFrom)}`,
    );
    assert.notEqual(
      receivedEv!.data.authentication,
      null,
      "authentication must be populated for a real inbound SMTP delivery (loopback always nils it)",
    );
    info(
      SUITE,
      "email.received",
      `real wire round trip confirmed: envelope_from=${String(envelopeFrom)} header_from=${String(headerFrom)}`,
    );

    const receivedFanout = await pollEventFanout(receivedEv!.id);
    assert.ok(receivedFanout, `email.received event ${receivedEv!.id} must fan out to >=1 webhook`);
    const receivedDel = await pollDelivery(hook.id, "email.received");
    assert.ok(receivedDel, "a delivery attempt for email.received must appear on our webhook");
    info(SUITE, "email.received", `evt=${receivedEv!.id} fanned to ${receivedFanout} webhook(s), whd=${receivedDel!.id} attempts=${receivedDel!.attempts}`);
    verifiedEventTypes.add("email.received");
  } finally {
    await delHook(hook.id);
    await delAgent(agentA);
    await delAgent(agentB);
  }
});

after(async () => {
  await writeReport(`./reports/${SUITE.replace("/", "-")}.json`);
  if (verifiedEventTypes.size > 0) {
    mkdirSync(EVENT_COVERAGE_DIR, { recursive: true });
    writeFileSync(`${EVENT_COVERAGE_DIR}${process.pid}.json`, JSON.stringify([...verifiedEventTypes]));
  }
});
