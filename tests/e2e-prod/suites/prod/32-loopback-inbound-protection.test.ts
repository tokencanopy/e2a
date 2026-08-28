import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../../harness/client.ts";
import { uniqueSlug, uniqueSubject } from "../../harness/fixtures.ts";
import { fail, info, writeReport } from "../../harness/report.ts";

// PROD-ONLY REGRESSION for the 1.3.0 loopback-screening bypass (OSS #681).
//
// THE BUG: before 1.3.0, a self-send's INBOUND leg (internal/agent's
// performSelfSend) wrote the inbound row with a zero-value screening verdict —
// it never ran internal/inboundscreen.EvaluateLoopback at all. So an agent's
// inbound_gate_policy and inbound_scan were silently IGNORED whenever the
// agent emailed itself: a `review` or `block` gate that would hold/quarantine
// a real SMTP-delivered message let a self-send through completely
// undetected. The fix (1.3.0, currently running in prod per release.env)
// routes the loopback inbound leg through the exact same EvaluateLoopback the
// SMTP relay path uses (internal/inboundscreen.EvaluateLoopback /
// internal/loopback.LoopbackGate).
//
// WHY THIS NEEDS TO BE A PROD-ONLY TEST: the fix shipped to production having
// been verified only against DEFAULT (open/allow) protection — i.e. against
// exactly the posture the bug was invisible under, since an open gate never
// flags anything self-send or not. This suite is the first conformance check
// with a genuinely NON-DEFAULT inbound_gate_policy against the self-send path.
// tests/e2e-prod's own suites/21-webhook-events.test.ts already exercises
// self-send loopback with an inbound `allowlist` gate — but only its
// action=flag branch (deliver + annotate; email.received still fires). That
// is the SHALLOWEST of the four gate actions and the one closest to "no
// change happened" from the outside. `review` and `block` are the outcomes
// where the OLD bug and the NEW fix produce visibly, structurally different
// results (delivered-as-normal-unread vs. hidden-pending_review-hold /
// accept-then-quarantine) — this suite covers `review`, the clearest of the
// two: a genuine hold with email.received SUPPRESSED and a real reviewer
// queue entry, versus the pre-fix silent pass-through.
//
// LoopbackGate (internal/inboundscreen/inboundscreen.go) hardcodes the
// self-send's sender-authentication facts as resolvable=true, dmarc="pass" —
// the strongest possible self-authentication — so the ONLY way to make the
// gate flag a self-send is a policy/allowlist MISMATCH, not a DMARC/auth
// failure. Configuring inbound.gate.policy="allowlist" with an allowlist that
// does NOT include the agent's own address is exactly that: the agent's own
// address is never trivially a member of its own non-matching allowlist, so
// gate.Flagged=true and the configured action (review) is applied — same as
// the relay would do for a real inbound message with a mismatched sender.
//
// This suite is NOT prod-only because staging can't run it (staging COULD
// run the identical scenario) — it belongs here because it is a direct,
// deliberate live-prod verification of a fix that reached prod unverified
// against this exact posture; see the task brief that requested it.
const SUITE = "prod/32-loopback-inbound-protection";
const client = new ApiClient();

interface SendResult {
  message_id?: string;
  status?: string;
}
interface EventView {
  id: string;
  type: string;
  created_at: string;
  agent_email?: string;
  message_id?: string;
  data: Record<string, unknown>;
}
interface PageEventView {
  items: EventView[];
  next_cursor: string | null;
}
interface ReviewView {
  id: string;
  agent_email: string;
  direction: string;
  subject: string;
  review_status: string;
  flagged?: boolean;
  flag_reason?: string;
  created_at: string;
}
interface PageReviewView {
  items: ReviewView[];
  next_cursor: string | null;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const sinceNow = () => new Date(Date.now() - 5000).toISOString();

async function createAgent(label: string): Promise<string> {
  const slug = uniqueSlug(label);
  const c = await client.post<{ email: string }>("/v1/agents", {
    body: { email: `${slug}@${client.env.sharedDomain}`, name: `loopback-regression ${label}` },
  });
  assert.equal(c.status, 201, `create agent failed: ${c.status} ${c.raw.slice(0, 200)}`);
  return c.body!.email;
}

async function delAgent(email: string): Promise<void> {
  await client.delete(`/v1/agents/${encodeURIComponent(email)}?confirm=DELETE&permanent=true`);
}

// pollEvent mirrors suites/21-webhook-events.test.ts's helper (kept local —
// this file intentionally avoids touching shared harness/ files while a
// sibling agent works an adjacent prod-only area).
async function pollEvent(
  params: { type: string; agentEmail: string; since: string },
  match: (e: EventView) => boolean,
  timeoutMs = 15000,
): Promise<EventView | null> {
  const deadline = Date.now() + timeoutMs;
  let delay = 400;
  while (Date.now() < deadline) {
    const r = await client.get<PageEventView>("/v1/events", {
      query: { type: params.type, agent_email: params.agentEmail, since: params.since, limit: 50 },
    });
    if (r.status === 200 && r.body?.items) {
      const found = r.body.items.find(match);
      if (found) return found;
    }
    await sleep(delay);
    delay = Math.min(Math.floor(delay * 1.5), 2500);
  }
  return null;
}

// assertEventAbsent polls the SAME window a real event would appear in and
// confirms it never does. Loopback events publish synchronously inside the
// same HTTP request/transaction as the self-send (internal/agent/selfsend.go
// publishLoopbackEventsTx) — so by the time POST .../messages has already
// returned, email.received would already be committed if it were going to
// fire at all. This bounded re-poll guards only against clock skew between
// this harness and the server, not against genuine async delay.
async function assertEventAbsent(params: { type: string; agentEmail: string; since: string }, subject: string, windowMs = 6000): Promise<void> {
  const deadline = Date.now() + windowMs;
  while (Date.now() < deadline) {
    const r = await client.get<PageEventView>("/v1/events", {
      query: { type: params.type, agent_email: params.agentEmail, since: params.since, limit: 50 },
    });
    if (r.status === 200 && r.body?.items?.some((e) => e.data.subject === subject)) {
      fail(SUITE, "received-event-not-suppressed", `${params.type} for subject ${JSON.stringify(subject)} appeared even though the inbound leg was held for review — the hold did NOT suppress delivery (the exact 1.3.0 regression)`);
      assert.fail(`${params.type} must be suppressed while the inbound leg is held for review`);
    }
    await sleep(500);
  }
}

test("1.3.0 regression: self-send with a NON-DEFAULT inbound review gate is genuinely screened (hold, not silent pass-through)", async () => {
  const email = await createAgent("loopback-review");
  try {
    // Configure a NON-DEFAULT inbound posture: allowlist gate whose allowlist
    // does NOT include the agent's own address, action=review. Full replace
    // (PUT is a full-replace resource) — outbound/holds stay at their
    // documented defaults (open/flag, 604800s TTL / reject-on-expiry).
    const prot = await client.put(`/v1/agents/${encodeURIComponent(email)}/protection`, {
      body: {
        inbound: { gate: { policy: "allowlist", action: "review", allowlist: ["nobody-else@example.com"] }, scan: {} },
        outbound: { gate: {}, scan: {} },
        holds: {},
      },
    });
    assert.equal(prot.status, 200, `configure non-default inbound review gate failed: ${prot.status} ${prot.raw.slice(0, 200)}`);

    // Sanity: the config actually landed as requested (guards against a
    // silently-ignored PUT masking the very bypass this test targets).
    const got = await client.get<{ inbound: { gate: { policy: string; action: string; allowlist: string[] } } }>(
      `/v1/agents/${encodeURIComponent(email)}/protection`,
    );
    assert.equal(got.status, 200);
    assert.equal(got.body?.inbound.gate.policy, "allowlist", "GET reflects the configured inbound gate policy");
    assert.equal(got.body?.inbound.gate.action, "review", "GET reflects the configured inbound gate action");
    assert.ok(!got.body?.inbound.gate.allowlist.includes(email), "the agent's own address is deliberately NOT on its own inbound allowlist");

    const subject = uniqueSubject("loopback review regression");
    const since = sinceNow();

    // The self-send itself: a single recipient equal to the agent's own
    // address, no CC/BCC — internal/loopback.IsSelfSend's exact predicate.
    const send = await client.post<SendResult>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      body: { to: [email], subject, text: "1.3.0 loopback-screening regression probe — must be held, not delivered" },
    });
    // The OUTBOUND leg of a loopback ALWAYS completes synchronously — only
    // the agent's OWN outbound gate (unconfigured here, default open) could
    // hold the outbound draft, and the inbound-leg hold this test configures
    // is orthogonal to it. A 200/sent here is expected and is NOT the
    // regression signal; the regression signal is what happens to the
    // INBOUND copy, asserted below.
    assert.equal(send.status, 200, `self-send loopback expected 200, got ${send.status}: ${send.raw.slice(0, 200)}`);
    assert.equal(send.body?.status, "sent", "outbound leg of a loopback self-send always resolves to sent regardless of the inbound gate");
    const outboundMessageId = send.body!.message_id!;
    assert.ok(outboundMessageId, "send response carries the outbound message id");

    // --- email.sent: always fires for the delivered outbound leg ---
    const sentEvent = await pollEvent({ type: "email.sent", agentEmail: email, since }, (e) => e.data.subject === subject);
    assert.ok(sentEvent, `email.sent for subject ${JSON.stringify(subject)} must appear within 15s (outbound leg always delivers)`);

    // --- THE REGRESSION SIGNAL: email.received must be SUPPRESSED ---
    // Pre-1.3.0 bug: performSelfSend wrote the inbound row with a zero-value
    // verdict, so email.received fired unconditionally regardless of the
    // agent's inbound_gate_policy — the message reached the inbox as a
    // normal unread item as if no gate existed at all. Post-fix: the inbound
    // leg runs the same gate evaluation the relay uses, gate.Flagged=true
    // (address not on allowlist) escalates to the configured action=review,
    // Hold=true, and email.received is suppressed
    // (internal/agent/selfsend.go publishLoopbackEventsTx: `if !screenRes.Hold`).
    await assertEventAbsent({ type: "email.received", agentEmail: email, since }, subject);

    // --- email.review_requested fires instead (the hold's own event) ---
    const reviewEvent = await pollEvent({ type: "email.review_requested", agentEmail: email, since }, (e) => e.data.subject === subject);
    assert.ok(reviewEvent, `email.review_requested for subject ${JSON.stringify(subject)} must appear within 15s — the inbound leg must be genuinely held`);
    assert.equal(reviewEvent!.data.direction, "inbound", "review_requested payload reports the inbound direction");

    // --- the held message surfaces in the account review queue as a real,
    // resolvable pending_review hold — not merely "an event fired" ---
    let held: ReviewView | undefined;
    const deadline = Date.now() + 15000;
    while (Date.now() < deadline && !held) {
      const list = await client.get<PageReviewView>("/v1/reviews");
      assert.equal(list.status, 200, `listReviews expected 200, got ${list.status}: ${list.raw.slice(0, 200)}`);
      held = list.body?.items.find((v) => v.agent_email === email && v.direction === "inbound" && v.subject === subject);
      if (!held) await sleep(500);
    }
    assert.ok(held, `the self-send's inbound leg must appear in the account review queue (direction=inbound, subject=${JSON.stringify(subject)})`);
    assert.equal(held!.review_status, "pending_review", "held inbound self-send is pending_review, not silently delivered");
    assert.notEqual(held!.id, outboundMessageId, "the held INBOUND message id is distinct from the delivered OUTBOUND message id — both legs are real, separate rows");

    // Full detail via getReview confirms the same facts server-side.
    const detail = await client.get<{ id: string; direction: string; review_status: string; subject: string }>(`/v1/reviews/${held!.id}`);
    assert.equal(detail.status, 200, `getReview expected 200, got ${detail.status}: ${detail.raw.slice(0, 200)}`);
    assert.equal(detail.body?.direction, "inbound");
    assert.equal(detail.body?.review_status, "pending_review");
    assert.equal(detail.body?.subject, subject);

    info(
      SUITE,
      "loopback-review-hold-confirmed",
      `self-send inbound leg genuinely held: outbound msg=${outboundMessageId} sent, inbound review id=${held!.id} pending_review, ` +
        `email.received suppressed, email.review_requested fired (evt=${reviewEvent!.id}) — the 1.3.0 fix holds under a non-default policy`,
    );

    // Clean up the hold itself (reject: dropped, never reaches the inbox —
    // matches the documented reject semantics for an inbound hold) so the
    // account review queue doesn't accumulate a stale entry across runs.
    const reject = await client.post(`/v1/reviews/${held!.id}/reject`, { body: { reason: "e2e loopback-regression cleanup" } });
    if (![200, 404, 409].includes(reject.status)) {
      fail(SUITE, "reject-hold-failed", `reject of held review ${held!.id} returned ${reject.status}: ${reject.raw.slice(0, 200)} — MANUAL CLEANUP MAY BE NEEDED`);
    }
  } finally {
    await delAgent(email);
  }
});

after(async () => {
  await writeReport(`./reports/${SUITE}.json`);
});
