import { test, before, after, afterEach } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { cleanup, track } from "../harness/cleanup.ts";
import { HttpMcpClient, callTool, type McpToolResult } from "../harness/mcp.ts";
import { isEventsLogDisabled } from "../harness/event-capability.ts";
import { uniqueSlug, uniqueSubject } from "../harness/fixtures.ts";
import { fail, info, writeReport } from "../harness/report.ts";

// Black-box MCP conformance for the webhooks + events tool surface (11 tools)
// against the DEPLOYED streamable-HTTP /mcp server. This is the MCP analogue
// of suites 16-webhooks.test.ts (REST webhook CRUD) and 21-webhook-events.test.ts
// (REST event emission) — same domain, same server-side semantics, but every
// call here goes through tools/call so the mcp_coverage_gate.py recorder
// credits the tool.
//
// Tools exercised (exactly 11, per the coverage batch):
//   create_webhook, get_webhook, list_webhooks, update_webhook, delete_webhook,
//   rotate_webhook_secret, test_webhook, list_webhook_deliveries,
//   list_events, get_event, redeliver_event
//
// Shapes verified against mcp/src/tools/webhooks.ts + events.ts (the MCP tool
// registrations) and cross-checked against the REST WebhookView/EventView
// shapes in 16-webhooks.test.ts / 21-webhook-events.test.ts — the MCP layer's
// toMcpOutput() snake-cases the SDK's camelCase fields back to the same REST
// vocabulary, so the JSON text content matches those interfaces field-for-field.
//
// Coverage note: `test_webhook` delivers a REAL synthetic event to the
// configured URL (bypassing filter matching); we do not depend on an external
// request-bin sink — the dummy https://example.com target 404/405s the POST,
// which still proves the delivery ATTEMPT ran (asserted via
// list_webhook_deliveries), matching the "assert the API's own response plus
// the resulting delivery record" guidance rather than requiring a 2xx from a
// sink we don't control.
//
// `redeliver_event` needs a prior delivered event to replay honestly, so this
// suite drives one real email.sent emission (no HITL hold, real send to the
// SES simulator) exactly like 21-webhook-events.test.ts, then replays THAT
// event. The event-log capability probe + skip semantics are copied verbatim
// from harness/event-capability.ts (501 + events_log_disabled ONLY) — no
// looser skip is invented, and a capability-disabled target legitimately
// leaves list_events/get_event/redeliver_event uncovered rather than faking it.
const SUITE = "26-mcp-webhooks";
const apiClient = new ApiClient();
const mcp = new HttpMcpClient(apiClient.env.mcpUrl, apiClient.env.apiKey);

const SIMULATOR = "success@simulator.amazonses.com";

interface WebhookView {
  id: string;
  url: string;
  description?: string;
  events: string[];
  filters?: Record<string, unknown>;
  enabled: boolean;
  created_at: string;
  last_delivered_at?: string;
  auto_disabled_at?: string;
  signing_secret?: string;
}
interface ListWebhooksResult {
  webhooks: WebhookView[];
  next_cursor?: string;
}
interface DeleteWebhookResult {
  deleted: boolean;
  id: string;
}
interface RotateSecretResult {
  signing_secret: string;
  previous_secret_expires_at: string;
}
interface TestWebhookResult {
  delivery_id: string;
}
interface WebhookDeliveryView {
  id: string;
  type: string;
  status: string;
  attempts: number;
  next_retry_at?: string;
  created_at: string;
  last_status_code?: number;
  last_error?: string;
}
interface ListWebhookDeliveriesResult {
  deliveries: WebhookDeliveryView[];
  next_cursor?: string;
}
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
interface ListEventsResult {
  events: EventView[];
  next_cursor?: string;
}
interface RedeliverResult {
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

const REQUIRED_TOOLS = [
  "create_webhook",
  "get_webhook",
  "list_webhooks",
  "update_webhook",
  "delete_webhook",
  "rotate_webhook_secret",
  "test_webhook",
  "list_webhook_deliveries",
  "list_events",
  "get_event",
  "redeliver_event",
];

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function extractText(r: McpToolResult): string {
  return r.content?.find((c) => c.type === "text")?.text ?? "";
}

function parseOk<T>(r: McpToolResult, label: string): T {
  assert.equal(r.isError, undefined, `${label} isError: ${extractText(r).slice(0, 300)}`);
  const text = extractText(r);
  assert.ok(text, `${label} returned text content`);
  return JSON.parse(text) as T;
}

async function deleteHook(id: string): Promise<void> {
  // Best-effort: the delete_webhook happy path is asserted explicitly in the
  // CRUD test, which already deletes the hook — this is only a cleanup net
  // for tests that create a hook and exit early on assertion failure.
  await callTool(mcp, "delete_webhook", { id, confirm: true }).catch(() => {});
}

async function createAgent(label: string): Promise<string> {
  const slug = uniqueSlug(label);
  const c = await apiClient.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${apiClient.env.sharedDomain}`, name: `mcp webhooks ${label}` },
  });
  if (c.status !== 201 || !c.body?.email) {
    throw new Error(`create agent failed: ${c.status} ${c.raw.slice(0, 200)}`);
  }
  track("agent", c.body.email);
  return c.body.email;
}

// pollWebhookDelivery: poll list_webhook_deliveries (via MCP) until a
// delivery matching `match` appears, or the bounded window elapses.
async function pollWebhookDelivery(
  hookId: string,
  match: (d: WebhookDeliveryView) => boolean,
  timeoutMs = 15000,
): Promise<WebhookDeliveryView | null> {
  const deadline = Date.now() + timeoutMs;
  let delay = 500;
  while (Date.now() < deadline) {
    const r = await callTool(mcp, "list_webhook_deliveries", { id: hookId, limit: 50 });
    if (!r.isError) {
      const parsed = JSON.parse(extractText(r)) as ListWebhookDeliveriesResult;
      const found = parsed.deliveries?.find(match);
      if (found) return found;
    }
    await sleep(delay);
    delay = Math.min(Math.floor(delay * 1.5), 3000);
  }
  return null;
}

// pollEvent: poll list_events (via MCP) until an event matching `match`
// appears, or the bounded window elapses. Mirrors 21-webhook-events.test.ts's
// REST pollEvent, but driven through the MCP tool so this suite's own
// tools/call traffic is what the coverage recorder sees.
async function pollEvent(
  params: { type: string; agentEmail: string; since: string },
  match: (e: EventView) => boolean,
  timeoutMs = 15000,
): Promise<EventView | null> {
  const deadline = Date.now() + timeoutMs;
  let delay = 500;
  while (Date.now() < deadline) {
    const r = await callTool(mcp, "list_events", {
      type: params.type,
      agent_email: params.agentEmail,
      since: params.since,
      limit: 50,
    });
    if (!r.isError) {
      const parsed = JSON.parse(extractText(r)) as ListEventsResult;
      const found = parsed.events?.find(match);
      if (found) return found;
    }
    await sleep(delay);
    delay = Math.min(Math.floor(delay * 1.5), 3000);
  }
  return null;
}

before(async () => {
  info(SUITE, "transport", `MCP over HTTP -> ${apiClient.env.mcpUrl}`);
});

afterEach(async () => {
  await cleanup(apiClient);
});

after(async () => {
  await mcp.stop();
  await cleanup(apiClient);
  writeReport(`./reports/${SUITE}.json`);
});

test("mcp-webhooks: tools/list advertises all 11 target tools", async () => {
  const list = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  const names = new Set(list.tools.map((t) => t.name));
  const missing = REQUIRED_TOOLS.filter((n) => !names.has(n));
  assert.deepEqual(missing, [], `deployed MCP server must advertise every target tool; missing: ${missing.join(", ")}`);
});

// create_webhook + get_webhook + list_webhooks + update_webhook +
// rotate_webhook_secret + test_webhook + list_webhook_deliveries +
// delete_webhook — the full webhook-subscriber CRUD round-trip over MCP.
test("mcp-webhooks: full webhook CRUD round-trip via MCP", async () => {
  const url = `https://example.com/e2e-mcp-webhook-${uniqueSlug("wh")}`;

  // create_webhook
  const created = await parseOk<WebhookView>(
    await callTool(mcp, "create_webhook", {
      url,
      events: ["email.received", "email.sent"],
      description: `e2e mcp ${uniqueSlug("wh")}`,
    }),
    "create_webhook",
  );
  assert.ok(created.id?.startsWith("wh_"), `create_webhook id has wh_ prefix: ${created.id}`);
  assert.equal(created.url, url);
  assert.deepEqual(created.events, ["email.received", "email.sent"]);
  assert.equal(created.enabled, true, "new webhook defaults to enabled");
  assert.ok(
    created.signing_secret?.startsWith("whsec_"),
    `create_webhook returns a whsec_ signing_secret: ${created.signing_secret}`,
  );
  const id = created.id;

  try {
    // get_webhook
    const got = await parseOk<WebhookView>(await callTool(mcp, "get_webhook", { id }), "get_webhook");
    assert.equal(got.id, id);
    assert.equal(got.url, url);
    assert.equal(got.enabled, true);
    assert.equal(got.signing_secret, undefined, "get_webhook must NOT return signing_secret");

    // list_webhooks
    const listed = await parseOk<ListWebhooksResult>(await callTool(mcp, "list_webhooks", {}), "list_webhooks");
    assert.ok(Array.isArray(listed.webhooks), "list_webhooks.webhooks is an array");
    const inList = listed.webhooks.find((w) => w.id === id);
    assert.ok(inList, `created webhook ${id} is present in list_webhooks`);
    assert.equal(inList!.signing_secret, undefined, "list_webhooks items must NOT carry signing_secret");

    // update_webhook — partial update; events is a full replace when present.
    const updated = await parseOk<WebhookView>(
      await callTool(mcp, "update_webhook", {
        id,
        description: "e2e mcp updated",
        events: ["email.received", "email.sent", "email.failed"],
      }),
      "update_webhook",
    );
    assert.equal(updated.description, "e2e mcp updated");
    assert.deepEqual(updated.events, ["email.received", "email.sent", "email.failed"]);
    assert.equal(updated.enabled, true, "update_webhook left enabled untouched (not passed)");

    // rotate_webhook_secret — new secret, distinct from the created one.
    const rotated = await parseOk<RotateSecretResult>(
      await callTool(mcp, "rotate_webhook_secret", { id }),
      "rotate_webhook_secret",
    );
    assert.ok(
      rotated.signing_secret?.startsWith("whsec_"),
      `rotate_webhook_secret returns a whsec_ secret: ${rotated.signing_secret}`,
    );
    assert.notEqual(rotated.signing_secret, created.signing_secret, "rotated secret differs from the original");
    assert.ok(
      typeof rotated.previous_secret_expires_at === "string" && rotated.previous_secret_expires_at.length > 0,
      "rotate_webhook_secret returns previous_secret_expires_at (grace window)",
    );

    // test_webhook — fires a synthetic event at the (unreachable, but valid
    // HTTPS/public) target. Does not depend on the sink responding 2xx.
    const tested = await parseOk<TestWebhookResult>(
      await callTool(mcp, "test_webhook", { id, type: "email.received" }),
      "test_webhook",
    );
    assert.ok(
      typeof tested.delivery_id === "string" && tested.delivery_id.length > 0,
      `test_webhook returns a delivery_id: ${tested.delivery_id}`,
    );

    // list_webhook_deliveries — the delivery test_webhook scheduled must
    // surface here (proves the tool + the resulting delivery record, per the
    // "don't depend on an external sink" guidance).
    const delivery = await pollWebhookDelivery(id, (d) => d.id === tested.delivery_id);
    assert.ok(delivery, `test_webhook's delivery ${tested.delivery_id} must appear in list_webhook_deliveries`);
    assert.equal(delivery!.type, "email.received");
    info(SUITE, "test_webhook", `delivery=${delivery!.id} status=${delivery!.status} attempts=${delivery!.attempts} last_status=${delivery!.last_status_code}`);
  } finally {
    // delete_webhook — DESTRUCTIVE; requires confirm:true.
    const deleted = await parseOk<DeleteWebhookResult>(
      await callTool(mcp, "delete_webhook", { id, confirm: true }),
      "delete_webhook",
    );
    assert.equal(deleted.deleted, true, "delete_webhook returns deleted:true");
    assert.equal(deleted.id, id, "delete_webhook echoes the webhook id");

    // Confirm it is actually gone.
    const gone = await callTool(mcp, "get_webhook", { id });
    if (!gone.isError) {
      fail(SUITE, "delete-webhook-not-gone", `get_webhook still succeeds for deleted ${id}`);
    }
  }
});

// delete_webhook without confirm:true must be rejected by the tool's own
// guard (never reaches the API) — a cheap extra proof the destructive guard
// works, using a throwaway hook so the CRUD test above stays the sole owner
// of the "real" delete path.
test("mcp-webhooks: delete_webhook without confirm:true is rejected", async () => {
  const url = `https://example.com/e2e-mcp-webhook-${uniqueSlug("whguard")}`;
  const created = await parseOk<WebhookView>(
    await callTool(mcp, "create_webhook", { url, events: ["email.received"] }),
    "create_webhook (guard fixture)",
  );
  try {
    const r = await callTool(mcp, "delete_webhook", { id: created.id, confirm: false });
    assert.equal(r.isError, true, "delete_webhook must reject confirm:false");
  } finally {
    await deleteHook(created.id);
  }
});

// list_events + get_event + redeliver_event — driven by one real email.sent
// emission (no HITL hold, real send to the SES simulator), exactly the
// trigger 21-webhook-events.test.ts uses on the REST side. The event-log
// capability probe below is copied verbatim from that suite: skip ONLY on
// the exact 501 events_log_disabled signal, never on a looser heuristic, and
// never treat a connectivity failure as a skip.
let eventsSkip: string | false = false;
try {
  const probe = await apiClient.get("/v1/events", { query: { limit: 1 } });
  if (isEventsLogDisabled(probe.status, probe.body)) {
    eventsSkip = "event-log capability disabled on this target (events_log_disabled)";
  }
} catch {
  // Probe couldn't reach the target — do NOT skip; let the tests run and
  // surface the real connectivity error instead of masking an outage.
}

test(
  "mcp-webhooks: list_events / get_event / redeliver_event round-trip via a real send",
  { skip: eventsSkip },
  async () => {
    const email = await createAgent("mcpev");
    const url = `https://example.com/e2e-mcp-webhook-events-${uniqueSlug("wh")}`;
    const hook = await parseOk<WebhookView>(
      await callTool(mcp, "create_webhook", { url, events: ["email.sent"] }),
      "create_webhook (events fixture)",
    );
    const since = new Date(Date.now() - 5000).toISOString();
    try {
      // Real (no-hold) send to the SES simulator, exactly like
      // 21-webhook-events.test.ts's email.sent emission test.
      const send = await apiClient.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
        body: { to: [SIMULATOR], subject: uniqueSubject("mcp emit sent"), text: "real send via MCP events suite" },
      });
      assert.equal(send.status, 202, `real send expected 202 accepted, got ${send.status}: ${send.raw.slice(0, 200)}`);
      assert.equal(send.body?.status, "accepted", "no-hold agent send is accepted for async delivery");
      const messageId = send.body!.message_id!;
      assert.ok(messageId?.startsWith("msg_"), "send returns a msg_ id");

      // list_events (via MCP), filtered — polled until the emitted event lands.
      const ev = await pollEvent({ type: "email.sent", agentEmail: email, since }, (e) =>
        e.message_id === messageId || e.data.message_id === messageId,
      );
      assert.ok(ev, `email.sent event for ${messageId} must appear via list_events within 15s`);
      assert.ok(ev!.id?.startsWith("evt_"), `event id has evt_ prefix: ${ev!.id}`);
      assert.equal(ev!.type, "email.sent");
      assert.equal(ev!.agent_email, email, "event.agent_email is the sending agent");

      // get_event — single-event fetch by id.
      const got = await parseOk<EventView>(await callTool(mcp, "get_event", { event_id: ev!.id }), "get_event");
      assert.equal(got.id, ev!.id, "get_event echoes the requested id");
      assert.equal(got.type, "email.sent");

      // Nonexistent event id -> isError.
      const missGet = await callTool(mcp, "get_event", { event_id: `evt_nonexistent_${Date.now()}` });
      if (!missGet.isError) {
        fail(SUITE, "get-event-bogus-not-error", "get_event with a nonexistent id did not surface as error");
      }

      // redeliver_event — replay to our fresh webhook by id. Uses the SAME
      // envelope id as the original (documented, non-dedup-safe replay
      // semantics); we only assert the tool's own response shape plus the
      // resulting delivery record, not a receiver-side outcome.
      const redelivered = await parseOk<RedeliverResult>(
        await callTool(mcp, "redeliver_event", { event_id: ev!.id, webhook_id: hook.id }),
        "redeliver_event",
      );
      assert.equal(redelivered.event_id, ev!.id, "redeliver_event echoes the event id");
      assert.ok(
        typeof redelivered.status === "string" && redelivered.status.length > 0,
        "redeliver_event returns a status",
      );
      const newDeliveryIds = [
        redelivered.delivery_id,
        ...(redelivered.deliveries ?? []).map((d) => d.delivery_id),
      ].filter((x): x is string => typeof x === "string" && x.length > 0);
      assert.ok(newDeliveryIds.length >= 1, `redeliver_event returns at least one delivery id: ${JSON.stringify(redelivered)}`);

      const requeued = await pollWebhookDelivery(hook.id, (d) => newDeliveryIds.includes(d.id));
      assert.ok(requeued, `redelivered delivery ${newDeliveryIds[0]} must appear in list_webhook_deliveries`);
      info(
        SUITE,
        "redeliver_event",
        `event=${ev!.id} webhook=${hook.id} -> delivery=${requeued!.id} status=${requeued!.status} attempts=${requeued!.attempts}`,
      );

      // Nonexistent event id -> isError.
      const missRedeliver = await callTool(mcp, "redeliver_event", { event_id: `evt_nonexistent_${Date.now()}` });
      if (!missRedeliver.isError) {
        fail(SUITE, "redeliver-event-bogus-not-error", "redeliver_event with a nonexistent id did not surface as error");
      }
    } finally {
      await deleteHook(hook.id);
    }
  },
);
