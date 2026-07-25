import { test, after } from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { ApiClient } from "../harness/client.ts";
import { isEventsLogDisabled } from "../harness/event-capability.ts";
import { uniqueSlug, uniqueSubject, holdAllOutbound } from "../harness/fixtures.ts";
import { writeReport, info } from "../harness/report.ts";

// Black-box conformance for REAL webhook-event EMISSION against a LIVE target.
// The suite runs wherever the target exposes the event-log capability. A single
// GET /v1/events probe detects deployments configured without that capability;
// connectivity failures are not treated as capability skips.
//
// Emission is proved for every event type across THREE correlated signals:
//   1. listEvents (GET /v1/events, filtered type + agent_email + since) — THIS
//      message's event row landed in the outbox/log (correlated by message_id).
//   2. the event's OWN delivery_status.matched_webhooks (GET /v1/events/{id}) —
//      EVENT-scoped proof THIS event fanned out to >=1 subscriber.
//   3. listWebhookDeliveries (GET /v1/webhooks/{id}/deliveries) — WEBHOOK-scoped
//      proof that OUR fresh webhook's HTTP delivery leg was ATTEMPTED. We assert
//      attempts>=1, NOT delivery success: this suite has no real webhook sink, so the
//      dummy target (example.com) 405s the POST. A 405 (or any last_status_code)
//      still proves the delivery leg ran; requiring a 2xx would test the sink, not
//      e2a. (2) and (3) are complementary — (2) is event-scoped but counts every
//      matching webhook; (3) is webhook-scoped but attempt-level — together they
//      close both the cross-suite and the "did the delivery worker run" gaps.
//
// Shapes/status verified against api/openapi.yaml (the drift-gated SSOT) AND
// curl-probed on live staging before these assertions were written (2026-07-10):
//   EventView     required {id,type,schema_version,created_at,status,data};
//                 optional agent_email, conversation_id, message_id, delivery_status.
//   PageEventView {items, next_cursor:string|null}.
//   RedeliverView required {event_id,status}; single-webhook replay also carries
//                 top-level delivery_id + webhook_id (status "pending"); bulk
//                 fan-out carries deliveries[] (status "scheduled").
//   WebhookDeliveryView required {id,type,status,attempts,next_retry_at,created_at}.
//
// Event types covered (HTTP-triggerable, per internal/webhookpub/event.go):
//   email.sent            — real send (no hold) to the SES simulator (SES 200).
//   email.review_requested  — hold-all-outbound BEFORE send → 202 pending_review.
//   email.review_rejected — reject a held message (clean; no send).
//   email.review_approved — approve a held message addressed to the simulator
//                           (approve→send succeeds; a non-simulator recipient can
//                           fail when the target uses an SES sandbox).
//   email.blocked         — outbound gate policy=allowlist action=block + a
//                           non-allowlisted recipient → the send is REFUSED
//                           (403 blocked_by_policy) and email.blocked fires.
//   email.received        — self-send loopback (agent mails itself). The inbound
//                           leg of a loopback is evaluated through the SAME
//                           agent-scoped inbound gate a real SMTP delivery would
//                           use (internal/inboundscreen.EvaluateLoopback /
//                           internal/loopback.LoopbackGate): sender = the
//                           agent's own address, resolvable, DMARC "pass" — so a
//                           default (open) inbound policy delivers straight
//                           through and fires email.received exactly as an SMTP
//                           roundtrip would (selfsend.go). No wire hop needed.
//   email.flagged          — self-send loopback with an inbound gate the
//                           self-sender itself doesn't satisfy: an allowlist
//                           gate with an EMPTY allowlist always flags a
//                           self-send (the agent's own address is never
//                           trivially a member of its own empty allowlist).
//                           action=flag means "deliver + annotate", so this is
//                           NOT the review-hold path — email.received still
//                           fires for the same inbound copy (screening_events.go).
//   email.failed           — a real send the provider PERMANENTLY refuses.
//                           Staging's e2a-staging-smtp IAM identity scopes
//                           ses:SendRawEmail to a small recipient allowlist (the
//                           mailbox simulator + a couple of test sinks); a send
//                           outside that scope gets a synchronous SMTP 554
//                           "Access denied ... not authorized to perform
//                           ses:SendRawEmail" (curl/live-probed 2026-07-25).
//                           internal/outbound.IsPermanentSMTPError treats any
//                           5xx as permanent, so the outbound worker's single
//                           attempt terminally fails the message and fires
//                           email.failed — deterministically, no retry wait. The
//                           recipient uses the RFC 2606 `.invalid` TLD so it
//                           stays unroutable even if that IAM scope ever widens.
//
// Event types ALLOWLISTED with reasons (structurally not producible on
// staging — see event_coverage_gate.py's ALLOWLIST, which is the enforced
// source of truth; the skip tests below exist only for human-readable
// visibility inside this suite's own output):
//   email.delivered/bounced/complained — async SES delivery-feedback (SNS);
//                     staging's e2a-staging-smtp IAM policy denies
//                     ses:SendRawEmail to the bounce/complaint simulator
//                     addresses, so this feedback cannot be produced there.
//   domain.suppression_added, agent.suppression_added — a suppression is
//                     created only by a real SES bounce/complaint (no
//                     createSuppression API), which inherits the same blocker.
//   domain.sending_verified/failed — need real SES sending-identity (DKIM)
//                     provisioning against a custom domain, verified async by
//                     AWS over minutes-to-hours; a throwaway shared-domain
//                     agent has no sending identity to provision.
// All seven are deferred to the planned prod-only differential suite, where
// the real infrastructure (a controlled custom domain, an unrestricted SES
// identity) exists to produce them honestly instead of being faked here.
//
// Ops exercised: listEvents (envelope + filters), getEvent (+404), redeliverEvent
// (re-queues a delivery; a new attempt appears). Every agent + webhook created is
// deleted inline in a finally (agent delete cascades to held messages; we also
// resolve holds explicitly). The shared cleanup harness is not used (it only
// tracks agents); this suite otherwise still avoids touching the shared harness/
// directory (see the event-coverage recorder below), consistent with that.
const SUITE = "21-webhook-events";
const client = new ApiClient();

// Event-type coverage recorder for event_coverage_gate.py. Mirrors the
// per-pid shard pattern harness/coverage.ts and harness/mcp-coverage.ts use,
// but is deliberately self-contained here (no harness/ edit) — there's no
// generic "an event type was verified" concept those two recorders model
// (one records exercised /v1 routes, the other advertised-vs-called MCP
// tools), so bolting a THIRD concern onto shared infra for a single suite
// isn't worth the coupling. A type is recorded ONLY after BOTH halves of the
// dual assertion below (event-scoped fanout AND webhook-scoped delivery
// attempt) pass — never on a bare "it showed up in listEvents", which would
// under-prove emission exactly the way a bare matched_webhooks count would
// (see the module doc above). Flushed once per suite-file process in
// `after`, alongside the existing per-suite report write.
const EVENT_COVERAGE_DIR = fileURLToPath(new URL("../reports/event-coverage/", import.meta.url));
const verifiedEventTypes = new Set<string>();
function recordVerified(type: string): void {
  verifiedEventTypes.add(type);
}

// Capability probe: deployments may disable the event log and report
// events_log_disabled. Skip only when both the 501 status and standard error code
// match; any other response proceeds so the tests surface it. Probe once at module
// load (top-level await; the runner waits for module eval to finish).
let skip: string | false = false;
try {
  const eventsProbe = await client.get("/v1/events", { query: { limit: 1 } });
  if (isEventsLogDisabled(eventsProbe.status, eventsProbe.body)) {
    skip = "event-log capability disabled on this target (events_log_disabled)";
  }
} catch {
  // Probe couldn't reach the target — do NOT skip. Let the tests run and surface
  // the real connectivity error rather than masking an outage as a clean skip.
}

// sinceNow returns a `since` filter with a few seconds of slack, so host/server
// clock skew (host clock ahead of the server's) can't place `since` after a
// just-emitted event's server-side created_at and hide it → false RED.
const sinceNow = () => new Date(Date.now() - 5000).toISOString();

// Real, deliverable recipient: SES accepts + 200s it and drops it (no real
// mailbox), so email.sent / review_approved actually reach the "sent" state.
const SIMULATOR = "success@simulator.amazonses.com";

interface EventView {
  id: string;
  type: string;
  schema_version: string;
  created_at: string;
  status: string;
  data: Record<string, unknown>;
  agent_email?: string;
  conversation_id?: string;
  message_id?: string;
  delivery_status?: { matched_webhooks?: number; delivered?: number; pending?: number; failed?: number };
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
  next_retry_at: string;
  created_at: string;
  last_status_code?: number;
  last_error?: string;
  last_attempt_at?: string;
}
interface PageWebhookDeliveryView {
  items: WebhookDeliveryView[];
  next_cursor: string | null;
}
interface CreateWebhookResponse {
  id: string;
  url: string;
  events: string[];
  enabled: boolean;
  signing_secret: string;
}
interface RedeliverView {
  event_id: string;
  status: string;
  delivery_id?: string;
  webhook_id?: string;
  deliveries?: Array<{ webhook_id?: string; delivery_id?: string; status?: string }>;
}
interface SendResult {
  status?: string;
  message_id?: string;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function createAgent(label: string, hold = false): Promise<string> {
  const slug = uniqueSlug(label);
  const c = await client.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${client.env.sharedDomain}`, name: `events ${label}` },
  });
  if (c.status !== 201 || !c.body?.email) {
    throw new Error(`create agent failed: ${c.status} ${c.raw.slice(0, 200)}`);
  }
  const email = c.body.email;
  if (hold) {
    const u = await holdAllOutbound(client, email);
    if (u.status !== 200) {
      await delAgent(email);
      throw new Error(`hold-all-outbound failed: ${u.status} ${u.raw.slice(0, 200)}`);
    }
  }
  return email;
}

async function createHook(events: string[]): Promise<CreateWebhookResponse> {
  const r = await client.post<CreateWebhookResponse>("/v1/webhooks", {
    // Dummy HTTPS target: passes the create-time HTTPS/SSRF guard, then 405s the
    // POST at delivery time — proving the delivery ATTEMPT without a real sink.
    body: { url: "https://example.com/e2e-webhook-events", events, description: `e2e ${uniqueSlug("whev")}` },
  });
  assert.equal(r.status, 201, `create webhook expected 201, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.ok(r.body?.id?.startsWith("wh_"), `webhook id has wh_ prefix: ${r.body?.id}`);
  return r.body!;
}

async function delAgent(email: string): Promise<void> {
  await client.delete(`/v1/agents/${encodeURIComponent(email)}?confirm=DELETE`);
}
async function delHook(id: string): Promise<void> {
  await client.delete(`/v1/webhooks/${encodeURIComponent(id)}?confirm=DELETE`);
}

// pollEvent: poll listEvents (filtered type+agent_email+since) until an event
// matching `match` appears, or the bounded window elapses. Backoff 500ms→3s.
async function pollEvent(
  params: { type: string; agentId: string; since: string },
  match: (e: EventView) => boolean,
  timeoutMs = 15000,
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

// pollDelivery: poll a webhook's deliveries until one for `eventType` with
// attempts>=1 appears (proving a delivery leg ran for that event). Optionally
// require a specific delivery id (used by the redeliver test).
async function pollDelivery(
  webhookId: string,
  eventType: string,
  opts: { deliveryId?: string } = {},
  timeoutMs = 15000,
): Promise<WebhookDeliveryView | null> {
  const deadline = Date.now() + timeoutMs;
  let delay = 500;
  while (Date.now() < deadline) {
    const r = await client.get<PageWebhookDeliveryView>(`/v1/webhooks/${webhookId}/deliveries`);
    if (r.status === 200 && r.body?.items) {
      const found = r.body.items.find(
        (d) => d.type === eventType && d.attempts >= 1 && (!opts.deliveryId || d.id === opts.deliveryId),
      );
      if (found) return found;
    }
    await sleep(delay);
    delay = Math.min(Math.floor(delay * 1.5), 3000);
  }
  return null;
}

// pollEventFanout: GET the specific event and poll until its OWN delivery_status
// shows it fanned out to >=1 subscriber. EVENT-scoped — the server counts
// webhook_subscriber_deliveries WHERE event_id = THIS (globally unique) event, so
// it proves THIS message's event fanned out and can't be satisfied by another
// suite's same-typed event. Caveat: it counts ALL matching webhooks in the account,
// not just ours, and the rows are inserted as status=pending (attempts=0) at
// ENQUEUE — so this alone proves neither "our webhook was matched" nor "a delivery
// attempt ran". Each emit test therefore ALSO asserts pollDelivery(hook.id) with
// attempts>=1: that endpoint is scoped to our fresh per-test webhook (ownership-
// checked, webhook-id path param) and only advances attempts once the HTTP leg
// fires. The pair — event-scoped fanout + webhook-scoped attempt — is what closes
// both the cross-suite and the "did the delivery worker actually run" gaps.
async function pollEventFanout(
  eventId: string,
  timeoutMs = 15000,
): Promise<NonNullable<EventView["delivery_status"]> | null> {
  const deadline = Date.now() + timeoutMs;
  let delay = 500;
  while (Date.now() < deadline) {
    const r = await client.get<EventView>(`/v1/events/${eventId}`);
    const ds = r.body?.delivery_status;
    if (r.status === 200 && ds && (ds.matched_webhooks ?? 0) >= 1) return ds;
    await sleep(delay);
    delay = Math.min(Math.floor(delay * 1.5), 3000);
  }
  return null;
}

function assertEventShape(e: EventView, expect: { type: string; agentId: string; messageId?: string }): void {
  // EventView required fields (openapi): id,type,schema_version,created_at,status,data.
  assert.ok(typeof e.id === "string" && e.id.startsWith("evt_"), `event id has evt_ prefix: ${e.id}`);
  assert.equal(e.type, expect.type, "event.type matches the triggered type");
  assert.ok(typeof e.schema_version === "string" && e.schema_version.length > 0, "schema_version is a non-empty string label");
  assert.ok(typeof e.created_at === "string" && e.created_at.length > 0, "created_at present");
  assert.ok(typeof e.status === "string" && e.status.length > 0, "status present");
  assert.ok(e.data && typeof e.data === "object", "data object present");
  // agent_email is optional in the schema but populated for these agent-scoped events.
  assert.equal(e.agent_email, expect.agentId, "event.agent_email is the triggering inbox");
  if (expect.messageId) {
    // Correlate to the triggering message: top-level message_id (populated on
    // the live target) OR data.message_id (always present in the payload).
    const dataMsg = e.data.message_id;
    assert.ok(
      e.message_id === expect.messageId || dataMsg === expect.messageId,
      `event correlates to message ${expect.messageId} (top-level=${e.message_id} data=${String(dataMsg)})`,
    );
  }
}

// ---- email.sent: a REAL send (no hold) to the SES simulator ----
test("emit: email.sent — real send emits the event and attempts a delivery", { skip }, async () => {
  const email = await createAgent("sent");
  const hook = await createHook(["email.sent"]);
  const since = sinceNow();
  try {
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [SIMULATOR], subject: uniqueSubject("emit sent"), text: "real send to SES simulator" },
    });
    // A no-hold send is durably accepted and delivered asynchronously → 202
    // accepted; the terminal "sent" arrives via the email.sent event polled
    // for below (or via wait=sent). Branch on body.status, not the HTTP code.
    assert.equal(send.status, 202, `real send expected 202 accepted, got ${send.status}: ${send.raw.slice(0, 200)}`);
    assert.equal(send.body?.status, "accepted", "no-hold agent send is accepted for async delivery");
    const messageId = send.body!.message_id!;
    assert.ok(messageId?.startsWith("msg_"), "send returns a msg_ id");

    const ev = await pollEvent({ type: "email.sent", agentId: email, since }, (e) =>
      e.message_id === messageId || e.data.message_id === messageId,
    );
    assert.ok(ev, `email.sent event for ${messageId} must appear in listEvents within 15s`);
    assertEventShape(ev!, { type: "email.sent", agentId: email, messageId });

    // Event-scoped: THIS event fanned out to >=1 subscriber (matched_webhooks
    // counts webhook_subscriber_deliveries WHERE event_id = this unique event).
    const fanout = await pollEventFanout(ev!.id);
    assert.ok(fanout, `event ${ev!.id} must fan out (matched_webhooks>=1) within 15s`);
    // Webhook-scoped: OUR fresh webhook's delivery leg actually RAN. The example.com
    // sink 405s the POST — attempts>=1 proves the leg fired (not delivery success).
    const del = await pollDelivery(hook.id, "email.sent");
    assert.ok(del, `a delivery ATTEMPT for email.sent must appear on webhook ${hook.id}`);
    assert.ok(del!.attempts >= 1, `delivery attempted (attempts=${del!.attempts})`);
    info(SUITE, "email.sent", `emitted evt=${ev!.id} fanned to ${fanout!.matched_webhooks} webhook(s); our webhook whd=${del!.id} attempts=${del!.attempts} last_status=${del!.last_status_code}`);
    recordVerified("email.sent");
  } finally {
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- email.review_requested: hold-all-outbound BEFORE send → 202 ----
test("emit: email.review_requested — held send emits the event and attempts a delivery", { skip }, async () => {
  const email = await createAgent("pending", true);
  const hook = await createHook(["email.review_requested"]);
  const since = sinceNow();
  let heldId: string | null = null;
  try {
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [SIMULATOR], subject: uniqueSubject("emit pending"), text: "held for review" },
    });
    assert.equal(send.status, 202, `held send expected 202 pending_review, got ${send.status}: ${send.raw.slice(0, 200)}`);
    assert.equal(send.body?.status, "pending_review", "gated send is held");
    heldId = send.body!.message_id!;
    assert.ok(heldId?.startsWith("msg_"), "held send returns a msg_ id");

    const ev = await pollEvent({ type: "email.review_requested", agentId: email, since }, (e) =>
      e.message_id === heldId || e.data.message_id === heldId,
    );
    assert.ok(ev, `email.review_requested event for ${heldId} must appear within 15s`);
    assertEventShape(ev!, { type: "email.review_requested", agentId: email, messageId: heldId! });
    // Payload is direction-aware (outbound HITL hold).
    assert.equal(ev!.data.direction, "outbound", "pending_review payload carries direction=outbound");

    const fanout = await pollEventFanout(ev!.id);
    assert.ok(fanout, `event ${ev!.id} must fan out (matched_webhooks>=1) within 15s`);
    const del = await pollDelivery(hook.id, "email.review_requested");
    assert.ok(del, `a delivery ATTEMPT for email.review_requested must appear on webhook ${hook.id}`);
    assert.ok(del!.attempts >= 1, `delivery attempted (attempts=${del!.attempts})`);
    info(SUITE, "email.review_requested", `emitted evt=${ev!.id} fanned to ${fanout!.matched_webhooks} webhook(s); our webhook whd=${del!.id} attempts=${del!.attempts}`);
    recordVerified("email.review_requested");
  } finally {
    // Resolve the hold explicitly (reject), then delete (delete cascades anyway).
    if (heldId) await client.post(`/v1/reviews/${heldId}/reject`, { body: { reason: "e2e pending-emit cleanup" } });
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- email.review_rejected: reject a held message (no send) ----
test("emit: email.review_rejected — rejecting a hold emits the event and attempts a delivery", { skip }, async () => {
  const email = await createAgent("reject", true);
  const hook = await createHook(["email.review_rejected"]);
  const since = sinceNow();
  try {
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [SIMULATOR], subject: uniqueSubject("emit reject"), text: "will be rejected" },
    });
    assert.equal(send.status, 202, `held send expected 202, got ${send.status}: ${send.raw.slice(0, 200)}`);
    const heldId = send.body!.message_id!;

    const reason = "e2e review_rejected emission";
    const rej = await client.post<{ status?: string; message_id?: string }>(`/v1/reviews/${heldId}/reject`, {
      body: { reason },
    });
    assert.equal(rej.status, 200, `reject expected 200, got ${rej.status}: ${rej.raw.slice(0, 200)}`);
    assert.equal(rej.body?.status, "review_rejected", "reject transitions to review_rejected");

    const ev = await pollEvent({ type: "email.review_rejected", agentId: email, since }, (e) =>
      e.message_id === heldId || e.data.message_id === heldId,
    );
    assert.ok(ev, `email.review_rejected event for ${heldId} must appear within 15s`);
    assertEventShape(ev!, { type: "email.review_rejected", agentId: email, messageId: heldId });
    // The event-data "why" field is `reason` (unified across events in #451,
    // renamed from `rejection_reason`). The RejectResultView response still
    // exposes `rejection_reason`; only the event payload was renamed.
    assert.equal(ev!.data.reason, reason, "payload echoes the rejection reason");

    const fanout = await pollEventFanout(ev!.id);
    assert.ok(fanout, `event ${ev!.id} must fan out (matched_webhooks>=1) within 15s`);
    const del = await pollDelivery(hook.id, "email.review_rejected");
    assert.ok(del, `a delivery ATTEMPT for email.review_rejected must appear on webhook ${hook.id}`);
    assert.ok(del!.attempts >= 1, `delivery attempted (attempts=${del!.attempts})`);
    info(SUITE, "email.review_rejected", `emitted evt=${ev!.id} fanned to ${fanout!.matched_webhooks} webhook(s); our webhook whd=${del!.id} attempts=${del!.attempts}`);
    recordVerified("email.review_rejected");
  } finally {
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- email.review_approved: approve a held message addressed to the simulator ----
test("emit: email.review_approved — approving a hold (to the simulator) emits the event and attempts a delivery", { skip }, async () => {
  const email = await createAgent("approve", true);
  const hook = await createHook(["email.review_approved"]);
  const since = sinceNow();
  let heldId: string | null = null;
  let resolved = false;
  try {
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      // Addressed to the simulator so approve→send succeeds when the target uses
      // an SES sandbox (a non-simulator/blackhole recipient can fail the send leg).
      body: { to: [SIMULATOR], subject: uniqueSubject("emit approve"), text: "will be approved + sent" },
    });
    assert.equal(send.status, 202, `held send expected 202, got ${send.status}: ${send.raw.slice(0, 200)}`);
    heldId = send.body!.message_id!;

    const ap = await client.post<SendResult>(`/v1/reviews/${heldId}/approve`, { body: {} });
    assert.ok(ap.status === 200 || ap.status === 202, `approve→send expected 200 terminal or 202 enqueued, got ${ap.status}: ${ap.raw.slice(0, 200)}`);
    assert.equal(ap.body?.status, ap.status === 202 ? "accepted" : "sent", "HTTP status matches the approval outcome");
    resolved = true;

    const ev = await pollEvent({ type: "email.review_approved", agentId: email, since }, (e) =>
      e.message_id === heldId || e.data.message_id === heldId,
    );
    assert.ok(ev, `email.review_approved event for ${heldId} must appear within 15s`);
    assertEventShape(ev!, { type: "email.review_approved", agentId: email, messageId: heldId! });
    assert.equal(ev!.data.direction, "outbound", "review_approved payload carries direction=outbound");

    const fanout = await pollEventFanout(ev!.id);
    assert.ok(fanout, `event ${ev!.id} must fan out (matched_webhooks>=1) within 15s`);
    const del = await pollDelivery(hook.id, "email.review_approved");
    assert.ok(del, `a delivery ATTEMPT for email.review_approved must appear on webhook ${hook.id}`);
    assert.ok(del!.attempts >= 1, `delivery attempted (attempts=${del!.attempts})`);
    info(SUITE, "email.review_approved", `emitted evt=${ev!.id} fanned to ${fanout!.matched_webhooks} webhook(s); our webhook whd=${del!.id} attempts=${del!.attempts}`);
    recordVerified("email.review_approved");
  } finally {
    // If approve didn't resolve the hold, reject it so nothing lingers.
    if (heldId && !resolved) {
      await client.post(`/v1/reviews/${heldId}/reject`, { body: { reason: "e2e approve-emit cleanup" } });
    }
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- email.blocked: outbound gate action=block refuses the send ----
test("emit: email.blocked — a gate-blocked send emits the event and attempts a delivery", { skip }, async () => {
  const email = await createAgent("blocked");
  // Block-all-outbound: same allowlist+empty-list gate as holdAllOutbound, but
  // with action=block — every recipient is unknown to the allowlist and every
  // send is REFUSED outright (vs. held for review). The /protection
  // sub-resource is a full replace (PUT), so send the complete shape.
  const prot = await client.put(`/v1/agents/${encodeURIComponent(email)}/protection`, {
    body: {
      inbound: { gate: {}, scan: {} },
      outbound: { gate: { policy: "allowlist", action: "block", allowlist: [] }, scan: {} },
      holds: {},
    },
  });
  if (prot.status !== 200) {
    await delAgent(email);
    throw new Error(`block-all-outbound protection PUT failed: ${prot.status} ${prot.raw.slice(0, 200)}`);
  }
  const hook = await createHook(["email.blocked"]);
  const since = sinceNow();
  try {
    const subject = uniqueSubject("emit blocked");
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [SIMULATOR], subject, text: "refused by the outbound gate" },
    });
    // An egress block REFUSES the send: 403 blocked_by_policy, and NO message
    // row is persisted. The event anchors to a stable server-derived soft-ref
    // id (msgblk_…) that the 403 body does NOT return — so the poll below
    // correlates on the unique subject instead of a message id.
    assert.equal(send.status, 403, `blocked send expected 403, got ${send.status}: ${send.raw.slice(0, 200)}`);
    assert.equal((send.body as { error?: { code?: string } })?.error?.code, "blocked_by_policy", "403 carries the blocked_by_policy code");

    const ev = await pollEvent({ type: "email.blocked", agentId: email, since }, (e) => e.data.subject === subject);
    assert.ok(ev, `email.blocked event for subject ${JSON.stringify(subject)} must appear in listEvents within 15s`);
    assertEventShape(ev!, { type: "email.blocked", agentId: email });
    // Beta payload: rowless soft-ref message id + the gate attribution.
    const blockedId = ev!.data.message_id;
    assert.ok(typeof blockedId === "string" && blockedId.startsWith("msgblk_"), `blocked payload carries the msgblk_ soft-ref id: ${String(blockedId)}`);
    assert.equal(ev!.data.direction, "outbound", "blocked payload carries direction=outbound");
    assert.equal(ev!.data.reason_source, "recipient_gate", "block is attributed to the recipient gate");
    assert.deepEqual(ev!.data.to, [SIMULATOR], "blocked payload echoes the refused recipient");

    const fanout = await pollEventFanout(ev!.id);
    assert.ok(fanout, `event ${ev!.id} must fan out (matched_webhooks>=1) within 15s`);
    const del = await pollDelivery(hook.id, "email.blocked");
    assert.ok(del, `a delivery ATTEMPT for email.blocked must appear on webhook ${hook.id}`);
    assert.ok(del!.attempts >= 1, `delivery attempted (attempts=${del!.attempts})`);
    info(SUITE, "email.blocked", `emitted evt=${ev!.id} fanned to ${fanout!.matched_webhooks} webhook(s); our webhook whd=${del!.id} attempts=${del!.attempts}`);
    recordVerified("email.blocked");
  } finally {
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- email.received: self-send loopback (own address) ----
// The inbound leg of a loopback self-send is evaluated through the SAME
// agent-scoped inbound gate a real SMTP delivery would use
// (internal/inboundscreen.EvaluateLoopback / internal/loopback.LoopbackGate):
// sender = the agent's own address, resolvable, DMARC "pass" — the strongest
// possible sender authentication. A fresh agent's default (open) inbound
// policy never flags/holds, so the self-send delivers straight through and the
// inbound copy fires email.received exactly as a real SMTP roundtrip would
// (buildLoopbackReceivedEvent in selfsend.go). The event's own message_id is
// the INBOUND copy's id — distinct from the send response's (outbound)
// message_id — so correlation here is by the unique subject, same pattern as
// email.blocked above.
test("emit: email.received — self-send loopback emits the event and attempts a delivery", { skip }, async () => {
  const email = await createAgent("received");
  const hook = await createHook(["email.received"]);
  const since = sinceNow();
  try {
    const subject = uniqueSubject("emit received");
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [email], subject, text: "self-send loopback for email.received" },
    });
    // Loopback is LOCAL delivery — terminal synchronously: 200 sent (not the
    // 202 accepted of an external/queued send).
    assert.equal(send.status, 200, `self-send loopback expected 200 sent, got ${send.status}: ${send.raw.slice(0, 200)}`);
    assert.equal(send.body?.status, "sent", "self-send loopback resolves synchronously to sent");

    const ev = await pollEvent({ type: "email.received", agentId: email, since }, (e) => e.data.subject === subject);
    assert.ok(ev, `email.received event for subject ${JSON.stringify(subject)} must appear in listEvents within 15s`);
    assertEventShape(ev!, { type: "email.received", agentId: email });
    assert.equal(ev!.data.direction, "inbound", "received payload carries direction=inbound");
    assert.equal(ev!.data.delivered_to, email, "received payload's delivered_to is the receiving agent");

    const fanout = await pollEventFanout(ev!.id);
    assert.ok(fanout, `event ${ev!.id} must fan out (matched_webhooks>=1) within 15s`);
    const del = await pollDelivery(hook.id, "email.received");
    assert.ok(del, `a delivery ATTEMPT for email.received must appear on webhook ${hook.id}`);
    assert.ok(del!.attempts >= 1, `delivery attempted (attempts=${del!.attempts})`);
    info(SUITE, "email.received", `emitted evt=${ev!.id} fanned to ${fanout!.matched_webhooks} webhook(s); our webhook whd=${del!.id} attempts=${del!.attempts}`);
    recordVerified("email.received");
  } finally {
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- email.flagged: self-send loopback with an inbound gate the self-sender doesn't satisfy ----
// LoopbackGate evaluates the agent's OWN inbound ingestion gate against its OWN
// address (the loopback sender). An allowlist gate with an EMPTY allowlist
// therefore always flags a self-send: the agent's own address is never
// trivially a member of its own empty allowlist. action=flag means "deliver +
// annotate" (screening_events.go: gate.Flagged && !res.Hold) — NOT the
// review-hold path — so email.received still fires for the same inbound copy;
// this test only asserts email.flagged.
test("emit: email.flagged — an inbound gate the self-sender doesn't satisfy emits the event and attempts a delivery", { skip }, async () => {
  const email = await createAgent("flagged");
  // Full replace (PUT): an allowlist inbound gate with an empty allowlist,
  // action=flag (deliver + annotate, never hold).
  const prot = await client.put(`/v1/agents/${encodeURIComponent(email)}/protection`, {
    body: {
      inbound: { gate: { policy: "allowlist", action: "flag", allowlist: [] }, scan: {} },
      outbound: { gate: {}, scan: {} },
      holds: {},
    },
  });
  if (prot.status !== 200) {
    await delAgent(email);
    throw new Error(`inbound flag-gate protection PUT failed: ${prot.status} ${prot.raw.slice(0, 200)}`);
  }
  const hook = await createHook(["email.flagged"]);
  const since = sinceNow();
  try {
    const subject = uniqueSubject("emit flagged");
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [email], subject, text: "self-send loopback for email.flagged" },
    });
    assert.equal(send.status, 200, `self-send loopback expected 200 sent, got ${send.status}: ${send.raw.slice(0, 200)}`);
    assert.equal(send.body?.status, "sent", "a flag (not review) gate never holds — self-send loopback still resolves synchronously to sent");

    const ev = await pollEvent({ type: "email.flagged", agentId: email, since }, (e) => e.data.subject === subject);
    assert.ok(ev, `email.flagged event for subject ${JSON.stringify(subject)} must appear in listEvents within 15s`);
    assertEventShape(ev!, { type: "email.flagged", agentId: email });
    assert.equal(ev!.data.direction, "inbound", "flagged payload carries direction=inbound");
    assert.equal(ev!.data.policy, "allowlist", "flagged payload echoes the triggering gate policy");
    assert.equal(ev!.data.reason, "sender not on the agent's inbound allowlist", "flagged payload carries the gate's non-match reason");

    const fanout = await pollEventFanout(ev!.id);
    assert.ok(fanout, `event ${ev!.id} must fan out (matched_webhooks>=1) within 15s`);
    const del = await pollDelivery(hook.id, "email.flagged");
    assert.ok(del, `a delivery ATTEMPT for email.flagged must appear on webhook ${hook.id}`);
    assert.ok(del!.attempts >= 1, `delivery attempted (attempts=${del!.attempts})`);
    info(SUITE, "email.flagged", `emitted evt=${ev!.id} fanned to ${fanout!.matched_webhooks} webhook(s); our webhook whd=${del!.id} attempts=${del!.attempts}`);
    recordVerified("email.flagged");
  } finally {
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- email.failed: a real send the provider PERMANENTLY refuses ----
// Staging's e2a-staging-smtp IAM identity scopes ses:SendRawEmail to a small
// recipient allowlist (the mailbox simulator + a couple of test sinks) —
// curl/live-probed 2026-07-25, a send outside that scope gets a SYNCHRONOUS
// SMTP 554 "Access denied: ... not authorized to perform `ses:SendRawEmail`
// ... identity/send-staging.e2a.dev". internal/outbound.IsPermanentSMTPError
// treats any 5xx as permanent, so the outbound worker's SINGLE attempt
// terminally fails the message (delivery.FailureSourceProvider,
// reason_code=submission.provider_rejected) and fires email.failed —
// deterministically, no retry wait needed. This is a REAL provider-classified
// permanent failure, just one caused by staging's SES identity being
// deliberately scoped for safety rather than by a bad mailbox. The recipient
// uses the RFC 2606 `.invalid` TLD so it stays unroutable even if that IAM
// scope ever widens.
const UNAUTHORIZED_RECIPIENT = "emit-failed-probe@nonexistent-e2e-events.invalid";
test("emit: email.failed — a provider-refused send emits the event and attempts a delivery", { skip }, async () => {
  const email = await createAgent("failed");
  const hook = await createHook(["email.failed"]);
  const since = sinceNow();
  try {
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [UNAUTHORIZED_RECIPIENT], subject: uniqueSubject("emit failed"), text: "provider-refused send" },
    });
    // The async pipeline always accepts first; the terminal failure arrives
    // via the email.failed event (or GET .../messages/{id}), not the HTTP status.
    assert.equal(send.status, 202, `send expected 202 accepted, got ${send.status}: ${send.raw.slice(0, 200)}`);
    assert.equal(send.body?.status, "accepted", "the async pipeline accepts first regardless of the eventual outcome");
    const messageId = send.body!.message_id!;

    const ev = await pollEvent({ type: "email.failed", agentId: email, since }, (e) =>
      e.message_id === messageId || e.data.message_id === messageId,
    );
    assert.ok(ev, `email.failed event for ${messageId} must appear in listEvents within 15s`);
    assertEventShape(ev!, { type: "email.failed", agentId: email, messageId });
    assert.equal(ev!.data.direction, "outbound", "failed payload carries direction=outbound");
    assert.equal(ev!.data.reason_code, "submission.provider_rejected", "failed payload carries the provider-rejected reason code");
    assert.ok(typeof ev!.data.reason === "string" && (ev!.data.reason as string).length > 0, "failed payload carries a non-empty reason diagnostic");
    assert.deepEqual(ev!.data.to, [UNAUTHORIZED_RECIPIENT], "failed payload echoes the refused recipient");

    const fanout = await pollEventFanout(ev!.id);
    assert.ok(fanout, `event ${ev!.id} must fan out (matched_webhooks>=1) within 15s`);
    const del = await pollDelivery(hook.id, "email.failed");
    assert.ok(del, `a delivery ATTEMPT for email.failed must appear on webhook ${hook.id}`);
    assert.ok(del!.attempts >= 1, `delivery attempted (attempts=${del!.attempts})`);
    info(SUITE, "email.failed", `emitted evt=${ev!.id} fanned to ${fanout!.matched_webhooks} webhook(s); our webhook whd=${del!.id} attempts=${del!.attempts} reason=${String(ev!.data.reason).slice(0, 80)}`);
    recordVerified("email.failed");
  } finally {
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- Events read API: listEvents envelope + filters ----
test("events: listEvents returns PageEventView envelope and honors type/agent_email/since/limit filters", { skip }, async () => {
  const email = await createAgent("list");
  const hook = await createHook(["email.sent"]);
  const since = sinceNow();
  try {
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [SIMULATOR], subject: uniqueSubject("emit list"), text: "for listEvents" },
    });
    assert.equal(send.status, 202, `real send expected 202 accepted, got ${send.status}: ${send.raw.slice(0, 200)}`);
    const messageId = send.body!.message_id!;
    // Ensure the event exists before asserting on the filtered list.
    const seed = await pollEvent({ type: "email.sent", agentId: email, since }, (e) =>
      e.message_id === messageId || e.data.message_id === messageId,
    );
    assert.ok(seed, "seed email.sent event present");

    // Full envelope shape (PageEventView: items + next_cursor:string|null, both required).
    const page = await client.get<PageEventView>("/v1/events", { query: { limit: 5 } });
    assert.equal(page.status, 200, `listEvents expected 200, got ${page.status}`);
    assert.ok(Array.isArray(page.body?.items), "items is an array");
    assert.ok(
      page.body!.next_cursor === null || typeof page.body!.next_cursor === "string",
      `next_cursor must be present as string|null, got ${JSON.stringify(page.body!.next_cursor)}`,
    );
    assert.ok(page.body!.items.length <= 5, "limit=5 clamps the page size");

    // type filter: every returned item is the requested type.
    const typed = await client.get<PageEventView>("/v1/events", {
      query: { type: "email.sent", agent_email: email, since },
    });
    assert.equal(typed.status, 200);
    assert.ok(typed.body!.items.length >= 1, "type+agent_email+since filter returns the seeded event");
    for (const e of typed.body!.items) {
      assert.equal(e.type, "email.sent", "type filter is honored");
      assert.equal(e.agent_email, email, "agent_email filter is honored");
    }

    // agent_email filter isolation: a bogus agent_email returns an empty page (not an error).
    const other = await client.get<PageEventView>("/v1/events", {
      query: { agent_email: `nonexistent-${Date.now()}@${client.env.sharedDomain}`, since },
    });
    assert.equal(other.status, 200, "agent_email filter with no matches returns 200");
    assert.equal(other.body!.items.length, 0, "unknown agent_email yields an empty page");

    // since filter: a future timestamp excludes everything.
    const future = new Date(Date.now() + 3_600_000).toISOString();
    const none = await client.get<PageEventView>("/v1/events", { query: { agent_email: email, since: future } });
    assert.equal(none.status, 200);
    assert.equal(none.body!.items.length, 0, "since=future excludes all events");
  } finally {
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- Events read API: getEvent (+ 404) ----
test("events: getEvent returns the EventView by evt_ id; nonexistent → 404", { skip }, async () => {
  const email = await createAgent("get");
  const hook = await createHook(["email.sent"]);
  const since = sinceNow();
  try {
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [SIMULATOR], subject: uniqueSubject("emit get"), text: "for getEvent" },
    });
    assert.equal(send.status, 202);
    const messageId = send.body!.message_id!;
    const ev = await pollEvent({ type: "email.sent", agentId: email, since }, (e) =>
      e.message_id === messageId || e.data.message_id === messageId,
    );
    assert.ok(ev, "seeded email.sent event present");

    const got = await client.get<EventView>(`/v1/events/${ev!.id}`);
    assert.equal(got.status, 200, `getEvent expected 200, got ${got.status}: ${got.raw.slice(0, 200)}`);
    assert.equal(got.body?.id, ev!.id, "getEvent echoes the requested id");
    assertEventShape(got.body!, { type: "email.sent", agentId: email, messageId });

    const miss = await client.get(`/v1/events/evt_nonexistent_${Date.now()}`);
    assert.equal(miss.status, 404, `getEvent nonexistent expected 404, got ${miss.status}: ${miss.raw.slice(0, 200)}`);
  } finally {
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- Events read API: redeliverEvent (re-queues a delivery) ----
test("events: redeliverEvent re-queues a delivery for the event; a new attempt appears", { skip }, async () => {
  const email = await createAgent("redeliver");
  const hook = await createHook(["email.sent"]);
  const since = sinceNow();
  try {
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [SIMULATOR], subject: uniqueSubject("emit redeliver"), text: "for redeliver" },
    });
    assert.equal(send.status, 202);
    const messageId = send.body!.message_id!;
    const ev = await pollEvent({ type: "email.sent", agentId: email, since }, (e) =>
      e.message_id === messageId || e.data.message_id === messageId,
    );
    assert.ok(ev, "seeded email.sent event present");
    // Wait for the original delivery so we can prove redeliver ADDS one.
    const first = await pollDelivery(hook.id, "email.sent");
    assert.ok(first, "original email.sent delivery attempt present before redeliver");

    // Single-webhook replay: RedeliverView carries event_id + status + top-level
    // delivery_id + webhook_id (probed status "pending"). requestBody is required.
    const rd = await client.post<RedeliverView>(`/v1/events/${ev!.id}/redeliver`, { body: { webhook_id: hook.id } });
    // Redeliver re-queues asynchronously → 202 Accepted (status "pending").
    assert.equal(rd.status, 202, `redeliver expected 202 accepted, got ${rd.status}: ${rd.raw.slice(0, 200)}`);
    assert.equal(rd.body?.event_id, ev!.id, "RedeliverView echoes the event id");
    assert.ok(typeof rd.body?.status === "string" && rd.body.status.length > 0, "RedeliverView has a status");
    // Collect the new delivery id(s) from either the single or bulk shape.
    const newIds = [
      rd.body?.delivery_id,
      ...(rd.body?.deliveries ?? []).map((d) => d.delivery_id),
    ].filter((x): x is string => typeof x === "string" && x.length > 0);
    assert.ok(newIds.length >= 1, `redeliver returns at least one new delivery id: ${JSON.stringify(rd.body)}`);
    assert.ok(newIds.includes(first!.id) === false, "redeliver id is distinct from the original delivery");

    // The re-queued delivery must surface in the webhook's deliveries.
    const requeued = await pollDelivery(hook.id, "email.sent", { deliveryId: newIds[0] });
    assert.ok(requeued, `re-queued delivery ${newIds[0]} must appear on webhook ${hook.id}`);
    assert.ok(requeued!.attempts >= 1, `re-queued delivery attempted (attempts=${requeued!.attempts})`);
    info(SUITE, "redeliverEvent", `event=${ev!.id} original whd=${first!.id} → redelivered whd=${newIds[0]} status=${rd.body?.status}`);

    // Redeliver of a nonexistent event → 404.
    const miss = await client.post(`/v1/events/evt_nonexistent_${Date.now()}/redeliver`, { body: {} });
    assert.equal(miss.status, 404, `redeliver nonexistent expected 404, got ${miss.status}: ${miss.raw.slice(0, 200)}`);
  } finally {
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- Negatives ----
test("events: unauthenticated listEvents / getEvent → 401", async () => {
  const list = await client.get("/v1/events", { apiKey: null });
  assert.equal(list.status, 401, `unauth listEvents expected 401, got ${list.status}`);
  const get = await client.get(`/v1/events/evt_whatever`, { apiKey: null });
  assert.equal(get.status, 401, `unauth getEvent expected 401, got ${get.status}`);
});

// ---- Documented skips: events whose trigger is out of this suite's reach ----
// These are NOT coverage gaps to be quietly ignored — each names WHY it can't
// be driven from this API-only battery on STAGING specifically, and where the
// coverage actually lives (the planned prod-only differential suite, which
// has the real infrastructure these need), so a future reader doesn't mistake
// a deliberate boundary for an oversight. event_coverage_gate.py's ALLOWLIST
// is the ENFORCED source of truth for these seven — these skip tests exist
// only so the reason also shows up in this suite's own human-readable output.
//
// email.delivered/bounced/complained — BLOCKED BY STAGING IAM SCOPE. These are
//   SES delivery-feedback events that arrive asynchronously via
//   SES→SNS→/webhooks/ses, normally triggerable via the SES mailbox
//   simulator's dedicated bounce@/complaint@ addresses — but staging's
//   e2a-staging-smtp IAM policy denies ses:SendRawEmail to exactly those
//   addresses (the same IAM scoping that email.failed above exploits to fail
//   an ordinary send), so this suite cannot even submit the message that
//   would trigger the feedback. Even without that block, the feedback's
//   unbounded async timeline would trade this gate's determinism for
//   flakiness — a second, independent reason this belongs in the prod-only
//   differential suite instead.
test("emit: email.delivered/bounced/complained — async SES delivery feedback blocked by staging's IAM scope", { skip: "SES delivery-feedback events require the mailbox simulator's bounce@/complaint@ addresses, which staging's e2a-staging-smtp IAM policy denies ses:SendRawEmail to; even unblocked, the async SES→SNS timeline is non-deterministic in a synchronous gate — deferred to the prod-only differential suite" }, () => {});
// domain.suppression_added, agent.suppression_added — INHERIT THE SAME BLOCKER.
//   A suppression is created only by a real SES bounce/complaint (no
//   createSuppression API — see coverage_gate.py's deleteSuppression
//   allowlist entry for the account-wide analogue of this same gap), so both
//   are unreachable for exactly the reason email.bounced/complained are above.
test("emit: domain.suppression_added, agent.suppression_added — need a real bounce/complaint, blocked the same way", { skip: "a suppression is created only by a real SES bounce/complaint (no createSuppression API); staging's IAM policy blocks producing that bounce/complaint (see email.bounced/complained above) — deferred to the prod-only differential suite" }, () => {});
// domain.sending_verified/failed — NEEDS REAL SES sending-identity (DKIM)
//   provisioning against a custom domain, which SES verifies asynchronously
//   over minutes-to-hours; a throwaway shared-domain e2e agent has no sending
//   identity to provision. Unlocking these means a dedicated custom-domain +
//   sending-identity fixture, not an in-suite tweak.
test("emit: domain.sending_verified/failed — need real SES sending-identity provisioning", { skip: "domain.sending_* events require real SES sending-identity (DKIM) provisioning on a custom domain, verified async over minutes-to-hours; a throwaway shared-domain agent has no sending identity — needs a dedicated custom-domain fixture — deferred to the prod-only differential suite" }, () => {});

after(async () => {
  await writeReport(`./reports/${SUITE}.json`);
  if (verifiedEventTypes.size > 0) {
    mkdirSync(EVENT_COVERAGE_DIR, { recursive: true });
    writeFileSync(`${EVENT_COVERAGE_DIR}${process.pid}.json`, JSON.stringify([...verifiedEventTypes]));
  }
});
