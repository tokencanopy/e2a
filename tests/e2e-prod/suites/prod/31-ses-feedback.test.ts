import { test, after } from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { ApiClient } from "../../harness/client.ts";
import { uniqueSlug, uniqueSubject } from "../../harness/fixtures.ts";
import { writeReport, info } from "../../harness/report.ts";

// PRODUCTION-ONLY: real Amazon SES delivery feedback (SES → SNS →
// /webhooks/ses), which staging's e2a-staging-smtp IAM policy blocks by
// denying ses:SendRawEmail to the mailbox-simulator's bounce@/complaint@
// addresses (see this directory's README and event_coverage_gate.py's
// STAGING_ONLY_ALLOWLIST). This suite is EXPLICITLY authorized to send real
// mail to the AWS-purpose-built simulator addresses
// (success@/bounce@/complaint@simulator.amazonses.com) — they exist for
// exactly this, and do not touch sender reputation.
//
// internal/delivery/ses.go's ParseSESNotification classifies what each
// simulator address produces:
//   success@simulator.amazonses.com  → Delivery                (email.delivered)
//   bounce@simulator.amazonses.com   → Bounce, type=Permanent   (email.bounced,
//                                       hard=true → auto-suppressed)
//   complaint@simulator.amazonses.com → Complaint               (email.complained,
//                                       always Suppress=true)
// Only a PERMANENT bounce or a complaint auto-suppresses
// (internal/delivery/consumer.go); both fire domain.suppression_added
// (account-scoped despite the event-name prefix — EventSuppressionAdded is
// literal "domain.suppression_added" in that file). This is the ONLY
// black-box way to create a real account suppression (no createSuppression
// API), which is what makes deleteSuppression's happy path exercisable here
// for the first time — see coverage_gate.py's STAGING_ONLY_ALLOWLIST entry,
// retired from the always-allowlisted set by this suite.
//
// agent.suppression_added is a DIFFERENT mechanism entirely — read
// internal/delivery/consumer.go closely and it NEVER fires that event; only
// the manual createAgentSuppression API and the unsubscribe-token flow do
// (internal/agent/events_api.go's AgentSuppressionAddedHook, called from
// internal/httpapi/agent_suppressions.go and internal/httpapi/unsubscribe.go
// only). It was allowlisted in event_coverage_gate.py under the same
// blanket "needs a real bounce" reasoning as domain.suppression_added, which
// doesn't actually apply to it — a real bounce/complaint NEVER produces an
// agent-scoped suppression, on staging OR production. It's verified here via
// the manual-create path (already black-box testable anywhere, per
// suites/24-agent-suppressions.test.ts) rather than a real bounce, so this
// one test would pass unchanged on staging too; it lives here because no
// staging suite currently wires a webhook to it (see event_coverage_gate.py's
// entry for the full note) — a reasonable follow-up left for later so this
// change stays scoped to what a prod-only differential suite is for.
//
// Verification is the same dual assertion suites/21-webhook-events.test.ts
// established: event-scoped fanout (delivery_status.matched_webhooks>=1) AND
// webhook-scoped delivery attempts>=1 on our own fresh webhook.
//
// Feedback is ASYNCHRONOUS (SES → SNS → our webhook endpoint) and can take
// tens of seconds; polls below use a generous budget (2 minutes) with capped
// backoff. A timeout is a hard assertion failure, never a skip.
const SUITE = "prod/31-ses-feedback";
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
interface SuppressionView {
  address: string;
  source: string;
  created_at: string;
  reason?: string;
}
interface PageSuppressionView {
  items: SuppressionView[];
  next_cursor: string | null;
}
interface DeleteSuppressionResult {
  deleted: boolean;
  address: string;
}
interface AgentSuppressionView {
  agent_email: string;
  address: string;
  source: string;
  created_at: string;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const sinceNow = () => new Date(Date.now() - 5000).toISOString();

const SUCCESS_SIM = "success@simulator.amazonses.com";
const BOUNCE_SIM = "bounce@simulator.amazonses.com";
const COMPLAINT_SIM = "complaint@simulator.amazonses.com";

async function createAgent(label: string): Promise<string> {
  const slug = uniqueSlug(label);
  const c = await client.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${client.env.sharedDomain}`, name: `ses-feedback ${label}` },
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
    body: { url: "https://example.com/e2e-prod-ses-feedback", events, description: `e2e-prod ${uniqueSlug("whsf")}` },
  });
  assert.equal(r.status, 201, `create webhook expected 201, got ${r.status}: ${r.raw.slice(0, 200)}`);
  return r.body!;
}
async function delHook(id: string): Promise<void> {
  await client.delete(`/v1/webhooks/${encodeURIComponent(id)}?confirm=DELETE`);
}

// Best-effort un-suppress. Unlike the ASSERTED deleteSuppression call inside
// each test (which is real coverage of that operation's happy path), this
// swallows every outcome — a 404 just means the address wasn't suppressed.
//
// It runs BOTH before the send and in the finally, and that pairing is the
// point. These simulator addresses are shared, account-scoped state: once
// suppressed, every later send to them is refused with 422
// recipient_suppressed. A failure anywhere between the send and the asserted
// delete used to leak the suppression, and because the very next run then
// failed at its own send — before reaching any cleanup — the leak re-armed
// itself. One red run permanently wedged the suite until someone deleted the
// suppression by hand. Observed on the 2026-07-26 production run: bounce@ and
// complaint@ were still suppressed from the previous night's failed run.
//
// The finally sweep stops a failure from leaking; the pre-send clear lets an
// already-wedged account heal on its own next run. Neither alone is enough.
async function clearSuppression(address: string): Promise<void> {
  try {
    await client.delete(`/v1/account/suppressions/${encodeURIComponent(address)}?confirm=DELETE`);
  } catch {
    // Network/transport failure during best-effort cleanup — never mask the
    // real test failure this finally block is unwinding from.
  }
}

// Generous async budget: SES → SNS → /webhooks/ses feedback has no
// documented SLA and observably takes anywhere from a few seconds to over a
// minute. A timeout returns null, which every caller turns into a hard
// assert.ok(..., "... must appear within Nms") failure — never a skip.
async function pollEvent(
  params: { type: string; agentId?: string; since: string },
  match: (e: EventView) => boolean,
  timeoutMs = 120000,
): Promise<EventView | null> {
  const deadline = Date.now() + timeoutMs;
  let delay = 1000;
  while (Date.now() < deadline) {
    const r = await client.get<PageEventView>("/v1/events", {
      query: { type: params.type, agent_email: params.agentId, since: params.since, limit: 50 },
    });
    if (r.status === 200 && r.body?.items) {
      const found = r.body.items.find(match);
      if (found) return found;
    }
    await sleep(delay);
    delay = Math.min(Math.floor(delay * 1.5), 5000);
  }
  return null;
}
async function pollEventFanout(eventId: string, timeoutMs = 30000): Promise<number | null> {
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
async function pollDelivery(webhookId: string, eventType: string, timeoutMs = 30000): Promise<WebhookDeliveryView | null> {
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

async function assertDualDelivery(hookId: string, eventType: string, ev: EventView): Promise<void> {
  const fanout = await pollEventFanout(ev.id);
  assert.ok(fanout, `event ${ev.id} (${eventType}) must fan out (matched_webhooks>=1) within 30s`);
  const del = await pollDelivery(hookId, eventType);
  assert.ok(del, `a delivery attempt for ${eventType} must appear on webhook ${hookId}`);
  assert.ok(del!.attempts >= 1, `delivery attempted (attempts=${del!.attempts})`);
  info(SUITE, eventType, `evt=${ev.id} fanned to ${fanout} webhook(s); whd=${del!.id} attempts=${del!.attempts}`);
  verifiedEventTypes.add(eventType);
}

// ---- email.delivered: a real send to the SES success simulator ----
test("emit: email.delivered — a real delivery-confirmed send emits the event and attempts a delivery", async () => {
  const email = await createAgent("delivered");
  const hook = await createHook(["email.delivered"]);
  const since = sinceNow();
  try {
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [SUCCESS_SIM], subject: uniqueSubject("emit delivered"), text: "real send for delivery confirmation" },
    });
    assert.equal(send.status, 202, `real send expected 202 accepted, got ${send.status}: ${send.raw.slice(0, 200)}`);
    const messageId = send.body!.message_id!;

    const ev = await pollEvent({ type: "email.delivered", agentId: email, since }, (e) =>
      e.message_id === messageId || e.data.message_id === messageId,
    );
    assert.ok(ev, `email.delivered event for ${messageId} must appear within 120s (real SES→SNS feedback)`);
    assert.equal(ev!.data.direction, "outbound", "delivered payload carries direction=outbound");
    assert.equal(ev!.data.delivered_to, SUCCESS_SIM, "delivered payload's delivered_to is the simulator recipient");
    assert.equal(ev!.data.agent_email, email, "delivered payload's agent_email is the sending agent");

    await assertDualDelivery(hook.id, "email.delivered", ev!);
  } finally {
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- email.bounced + domain.suppression_added: a real hard bounce ----
test("emit: email.bounced — a real permanent bounce emits the event, auto-suppresses, and is deletable", async () => {
  const email = await createAgent("bounced");
  const hook = await createHook(["email.bounced", "domain.suppression_added"]);
  const since = sinceNow();
  try {
    // Heal a suppression leaked by an earlier failed run before it refuses
    // this send with 422 recipient_suppressed. See clearSuppression's note.
    await clearSuppression(BOUNCE_SIM);
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [BOUNCE_SIM], subject: uniqueSubject("emit bounced"), text: "real send to trigger a hard bounce" },
    });
    assert.equal(send.status, 202, `real send expected 202 accepted, got ${send.status}: ${send.raw.slice(0, 200)}`);
    const messageId = send.body!.message_id!;

    // ---- email.bounced ----
    const bouncedEv = await pollEvent({ type: "email.bounced", agentId: email, since }, (e) =>
      e.message_id === messageId || e.data.message_id === messageId,
    );
    assert.ok(bouncedEv, `email.bounced event for ${messageId} must appear within 120s (real SES→SNS feedback)`);
    assert.equal(bouncedEv!.data.direction, "outbound", "bounced payload carries direction=outbound");
    assert.equal(bouncedEv!.data.delivered_to, BOUNCE_SIM, "bounced payload's delivered_to is the simulator recipient");
    assert.equal(bouncedEv!.data.bounce_type, "permanent", "the mailbox simulator's bounce@ address produces a PERMANENT (hard) bounce");
    await assertDualDelivery(hook.id, "email.bounced", bouncedEv!);

    // ---- domain.suppression_added (account-scoped; a hard bounce auto-suppresses) ----
    // Account-scoped events carry no agent_email (internal/delivery/consumer.go
    // leaves AgentID empty for the suppression FiredEvent), so correlate by the
    // triggering message_id instead of an agent_email filter.
    const suppEv = await pollEvent({ type: "domain.suppression_added", since }, (e) => e.data.message_id === messageId, 30000);
    assert.ok(suppEv, `domain.suppression_added event for ${messageId} must appear within 30s of the bounce`);
    assert.equal(suppEv!.data.address, BOUNCE_SIM, "suppression payload's address is the bounced recipient");
    assert.equal(suppEv!.data.source, "bounce", "suppression payload's source is bounce");
    await assertDualDelivery(hook.id, "domain.suppression_added", suppEv!);

    // ---- listSuppressions shows it ----
    const list = await client.get<PageSuppressionView>("/v1/account/suppressions", { query: { limit: 100 } });
    assert.equal(list.status, 200, `listSuppressions expected 200, got ${list.status}`);
    const listed = list.body!.items.find((s) => s.address === BOUNCE_SIM);
    assert.ok(listed, `${BOUNCE_SIM} must appear in listSuppressions after the real bounce`);
    assert.equal(listed!.source, "bounce", "listed suppression's source is bounce");

    // ---- deleteSuppression: previously allowlisted in coverage_gate.py as
    // "no happy path black-box" — a real bounce is exactly what makes this
    // exercisable. Clean up so the shared simulator address isn't left
    // suppressed on this account. ----
    const del = await client.delete<DeleteSuppressionResult>(
      `/v1/account/suppressions/${encodeURIComponent(BOUNCE_SIM)}?confirm=DELETE`,
    );
    assert.equal(del.status, 200, `deleteSuppression expected 200, got ${del.status}: ${del.raw.slice(0, 200)}`);
    assert.equal(del.body?.deleted, true, "deletion object has deleted:true");
    assert.equal(del.body?.address, BOUNCE_SIM, "deletion object echoes the un-suppressed address");

    const afterDel = await client.get<PageSuppressionView>("/v1/account/suppressions", { query: { limit: 100 } });
    assert.ok(
      !afterDel.body!.items.some((s) => s.address === BOUNCE_SIM),
      `${BOUNCE_SIM} must no longer be suppressed after cleanup`,
    );
    info(SUITE, "deleteSuppression", `real bounce suppression created and deleted cleanly for ${BOUNCE_SIM}`);
  } finally {
    await clearSuppression(BOUNCE_SIM);
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- email.complained + domain.suppression_added: a real complaint ----
test("emit: email.complained — a real complaint emits the event, auto-suppresses, and is deletable", async () => {
  const email = await createAgent("complained");
  const hook = await createHook(["email.complained", "domain.suppression_added"]);
  const since = sinceNow();
  try {
    // Heal a suppression leaked by an earlier failed run before it refuses
    // this send with 422 recipient_suppressed. See clearSuppression's note.
    await clearSuppression(COMPLAINT_SIM);
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [COMPLAINT_SIM], subject: uniqueSubject("emit complained"), text: "real send to trigger a complaint" },
    });
    assert.equal(send.status, 202, `real send expected 202 accepted, got ${send.status}: ${send.raw.slice(0, 200)}`);
    const messageId = send.body!.message_id!;

    // ---- email.complained ----
    const complainedEv = await pollEvent({ type: "email.complained", agentId: email, since }, (e) =>
      e.message_id === messageId || e.data.message_id === messageId,
    );
    assert.ok(complainedEv, `email.complained event for ${messageId} must appear within 120s (real SES→SNS feedback)`);
    assert.equal(complainedEv!.data.direction, "outbound", "complained payload carries direction=outbound");
    assert.equal(complainedEv!.data.delivered_to, COMPLAINT_SIM, "complained payload's delivered_to is the simulator recipient");
    await assertDualDelivery(hook.id, "email.complained", complainedEv!);

    // ---- domain.suppression_added (a complaint ALWAYS auto-suppresses) ----
    const suppEv = await pollEvent({ type: "domain.suppression_added", since }, (e) => e.data.message_id === messageId, 30000);
    assert.ok(suppEv, `domain.suppression_added event for ${messageId} must appear within 30s of the complaint`);
    assert.equal(suppEv!.data.address, COMPLAINT_SIM, "suppression payload's address is the complained recipient");
    assert.equal(suppEv!.data.source, "complaint", "suppression payload's source is complaint");
    await assertDualDelivery(hook.id, "domain.suppression_added", suppEv!);

    // ---- clean up: un-suppress so the shared simulator address is left usable ----
    const del = await client.delete<DeleteSuppressionResult>(
      `/v1/account/suppressions/${encodeURIComponent(COMPLAINT_SIM)}?confirm=DELETE`,
    );
    assert.equal(del.status, 200, `deleteSuppression expected 200, got ${del.status}: ${del.raw.slice(0, 200)}`);
    assert.equal(del.body?.deleted, true, "deletion object has deleted:true");

    const afterDel = await client.get<PageSuppressionView>("/v1/account/suppressions", { query: { limit: 100 } });
    assert.ok(
      !afterDel.body!.items.some((s) => s.address === COMPLAINT_SIM),
      `${COMPLAINT_SIM} must no longer be suppressed after cleanup`,
    );
    info(SUITE, "deleteSuppression", `real complaint suppression created and deleted cleanly for ${COMPLAINT_SIM}`);
  } finally {
    await clearSuppression(COMPLAINT_SIM);
    await delHook(hook.id);
    await delAgent(email);
  }
});

// ---- agent.suppression_added: manual create (NOT a real-bounce artifact — see module doc) ----
test("emit: agent.suppression_added — a manual agent-scoped block emits the event and attempts a delivery", async () => {
  const email = await createAgent("agentsupp");
  const hook = await createHook(["agent.suppression_added"]);
  const since = sinceNow();
  const address = `blocked-${uniqueSlug("rcpt")}@example.com`;
  try {
    const create = await client.post<AgentSuppressionView>(`/v1/agents/${encodeURIComponent(email)}/suppressions`, {
      body: { address, reason: "e2e-prod agent.suppression_added emission" },
    });
    assert.equal(create.status, 200, `createAgentSuppression expected 200, got ${create.status}: ${create.raw.slice(0, 200)}`);
    assert.equal(create.body?.source, "manual", "API-created block has source=manual");

    const ev = await pollEvent({ type: "agent.suppression_added", agentId: email, since }, (e) => e.data.address === address, 30000);
    assert.ok(ev, `agent.suppression_added event for ${address} must appear within 30s`);
    assert.equal(ev!.data.agent_email, email, "agent-suppression payload's agent_email is the owning agent");
    assert.equal(ev!.data.address, address, "agent-suppression payload's address is the blocked recipient");
    assert.equal(ev!.data.source, "manual", "agent-suppression payload's source is manual");
    await assertDualDelivery(hook.id, "agent.suppression_added", ev!);

    // Clean up the agent-scoped block (API-allowed; cf. deleteAgentSuppression
    // in suites/24-agent-suppressions.test.ts).
    const del = await client.delete(`/v1/agents/${encodeURIComponent(email)}/suppressions/${encodeURIComponent(address)}?confirm=DELETE`);
    assert.equal(del.status, 200, `deleteAgentSuppression expected 200, got ${del.status}: ${del.raw.slice(0, 200)}`);
  } finally {
    await delHook(hook.id);
    await delAgent(email);
  }
});

after(async () => {
  await writeReport(`./reports/${SUITE.replace("/", "-")}.json`);
  if (verifiedEventTypes.size > 0) {
    mkdirSync(EVENT_COVERAGE_DIR, { recursive: true });
    writeFileSync(`${EVENT_COVERAGE_DIR}${process.pid}.json`, JSON.stringify([...verifiedEventTypes]));
  }
});
