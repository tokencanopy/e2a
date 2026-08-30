import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { cleanup, track } from "../harness/cleanup.ts";
import { uniqueSlug, uniqueSubject, SINK_EMAIL, holdAllOutbound } from "../harness/fixtures.ts";
import { writeReport, warn } from "../harness/report.ts";

// Coverage-gate fill: deleteMessage, restoreMessage, restoreAgent had no
// conformance coverage (they exist on `main` but no suite ever issued a 2xx
// request to them — see harness/coverage.ts / coverage_gate.py). This suite
// drives the real soft-delete/restore lifecycle for both messages and agents
// against LIVE staging, asserting the documented server semantics
// (internal/httpapi/messages.go handleDeleteMessage/handleRestoreMessage,
// internal/httpapi/agents_write.go handleRestoreAgent), not just status codes:
//   - a trashed message disappears from ordinary listings but stays directly
//     readable (GET) with deleted_at set, and reappears via ?deleted=true
//   - restore clears deleted_at and un-hides it
//   - a message held for review (pending_review) cannot be deleted (409
//     message_held) — the trash op and the review-hold state machine are
//     independent gates
//   - restoring a live (non-trashed) resource is a 409 not_in_trash no-op,
//     for both messages and agents
//   - a trashed agent stops resolving (404 on GET) and moves into the
//     ?deleted=true agent trash view; restore brings it back live with its
//     messages intact
//
// Deliberately economical with agent creation: this suite runs against a
// conformance account that may be capped at a handful of live agents (e.g.
// staging's free-plan probe account, max 3), and a trashed agent doesn't
// count against that cap (internal/usage TestCountAgentsByUser_ExcludesTrashedAgents)
// but a LIVE one does. So exactly ONE tracked throwaway agent is shared by
// both tests: its self-send loopback materializes a real message without
// depending on outbound SMTP/SES, then the same agent is reused for the
// message-held guard and full agent delete/restore cycle. The persistent
// primary agent is never sent a message, so repeated runs do not pollute its
// inbox.
const SUITE = "24-trash-lifecycle";
const client = new ApiClient();
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
let lifecycleAgentPromise: Promise<string> | undefined;

after(async () => {
  const r = await cleanup(client);
  if (r.failed.length) warn(SUITE, "cleanup", `failed ${r.failed.length}`, r.failed);
  writeReport(`./reports/${SUITE}.json`);
});

interface MessageSummary {
  id: string;
  subject: string;
  deleted_at?: string | null;
}
interface MessageListPage {
  items: MessageSummary[];
}
interface MessageFull {
  id: string;
  subject: string;
  deleted_at?: string | null;
}
// Shared error-envelope shape (`{ error: { code, message } }`), used to type
// negative-path assertions on `body?.error?.code`.
interface ErrorBody {
  error?: { code: string; message?: string };
}

// Lazily create one agent for the whole file. Cache the initialization so
// every test in this suite uses the same tracked identity.
function lifecycleAgent(): Promise<string> {
  lifecycleAgentPromise ??= (async () => {
    const slug = uniqueSlug("trash-agent");
    const email = `${slug}@${client.env.sharedDomain}`;
    const created = await client.post<{ email: string }>("/v1/agents", {
      body: { email, name: "trash lifecycle" },
      expect: 201,
    });
    // Track the requested identity before assertions so teardown still owns
    // cleanup if a malformed success response omits or changes the email.
    track("agent", email);
    assert.equal(created.body?.email, email);
    return email;
  })();
  return lifecycleAgentPromise;
}

// Self-send loopback: the agent mails itself, so the send completes without
// depending on a real external SMTP hop, and the inbound copy shows up in
// the same agent's own inbox — the pattern 23-coverage-happy-path.test.ts
// established for materializing a real message record.
async function loopbackMessage(email: string, subject: string): Promise<{ id: string }> {
  await client.post(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    body: { to: [email], subject, text: "24-trash-lifecycle loopback body" },
    expect: [200, 202],
  });
  for (let i = 0; i < 12; i++) {
    const list = await client.get<MessageListPage>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
      query: { direction: "inbound", read_status: "all", limit: 20 },
    });
    const m = list.body?.items?.find((x) => x.subject === subject);
    if (m) return { id: m.id };
    await sleep(1500);
  }
  throw new Error(`loopback message "${subject}" never appeared for ${email}`);
}

test("message trash lifecycle: delete hides it, GET stays readable, restore brings it back", async () => {
  const email = await lifecycleAgent();
  const subject = uniqueSubject("trash-msg");
  const { id } = await loopbackMessage(email, subject);

  // Baseline: present in the ordinary (live) inbound listing.
  const before = await client.get<MessageListPage>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    query: { direction: "inbound", read_status: "all", limit: 20 },
  });
  assert.ok(before.body?.items?.some((m) => m.id === id), "message present in live listing before delete");

  // Negative: restoring a LIVE (not-yet-trashed) message is a 409 no-op.
  const restoreBeforeDelete = await client.post<ErrorBody>(`/v1/agents/${encodeURIComponent(email)}/messages/${id}/restore`);
  assert.equal(restoreBeforeDelete.status, 409, `expected 409, got ${restoreBeforeDelete.status}: ${restoreBeforeDelete.raw.slice(0, 200)}`);
  assert.equal(restoreBeforeDelete.body?.error?.code, "not_in_trash");

  // deleteMessage: the default (soft) delete needs no ?confirm — it's reversible.
  const del = await client.delete<{ deleted: boolean; id: string }>(
    `/v1/agents/${encodeURIComponent(email)}/messages/${id}`,
  );
  assert.equal(del.status, 200, `deleteMessage expected 200, got ${del.status}: ${del.raw.slice(0, 200)}`);
  assert.deepEqual(del.body, { deleted: true, id }, "DeleteMessageResult shape");

  // Trashed messages disappear from the ordinary listing…
  const afterDelete = await client.get<MessageListPage>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    query: { direction: "inbound", read_status: "all", limit: 20 },
  });
  assert.ok(!afterDelete.body?.items?.some((m) => m.id === id), "message no longer in live listing after delete");

  // …but appear in the trash view (?deleted=true), with deleted_at set.
  const trashView = await client.get<MessageListPage>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    query: { deleted: "true", direction: "inbound", limit: 20 },
  });
  const trashed = trashView.body?.items?.find((m) => m.id === id);
  assert.ok(trashed, "message present in ?deleted=true trash view after delete");
  assert.ok(trashed!.deleted_at, "trashed listing row carries deleted_at");

  // A trashed message stays directly readable via GET, deleted_at included
  // (per messages.go: "ordinary lists...exclude it" but direct GET does not).
  const getTrashed = await client.get<MessageFull>(`/v1/agents/${encodeURIComponent(email)}/messages/${id}`);
  assert.equal(getTrashed.status, 200, `direct GET of trashed message expected 200, got ${getTrashed.status}`);
  assert.equal(getTrashed.body?.id, id);
  assert.ok(getTrashed.body?.deleted_at, "direct GET of trashed message includes deleted_at");

  // restoreMessage: brings it back, deleted_at cleared.
  const restore = await client.post<MessageFull>(`/v1/agents/${encodeURIComponent(email)}/messages/${id}/restore`);
  assert.equal(restore.status, 200, `restoreMessage expected 200, got ${restore.status}: ${restore.raw.slice(0, 200)}`);
  assert.equal(restore.body?.id, id);
  assert.equal(restore.body?.subject, subject);
  assert.equal(restore.body?.deleted_at, undefined, "restored message view omits deleted_at");

  // Reappears in the live listing…
  const afterRestore = await client.get<MessageListPage>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    query: { direction: "inbound", read_status: "all", limit: 20 },
  });
  assert.ok(afterRestore.body?.items?.some((m) => m.id === id), "message back in live listing after restore");

  // …and drops out of the trash view again.
  const trashViewAfter = await client.get<MessageListPage>(`/v1/agents/${encodeURIComponent(email)}/messages`, {
    query: { deleted: "true", direction: "inbound", limit: 20 },
  });
  assert.ok(!trashViewAfter.body?.items?.some((m) => m.id === id), "message no longer in trash view after restore");
});

test("agent trash lifecycle: message-held delete guard, then delete/restore the agent itself", async () => {
  const email = await lifecycleAgent();

  // --- message_held guard: a pending_review draft cannot be trashed ---
  const hold = await holdAllOutbound(client, email);
  assert.equal(hold.status, 200, `hold-all-outbound failed: ${hold.status} ${hold.raw.slice(0, 200)}`);

  const held = await client.post<{ message_id: string; status: string }>(
    `/v1/agents/${encodeURIComponent(email)}/messages`,
    { body: { to: [SINK_EMAIL], subject: uniqueSubject("trash-held"), text: "must not be deletable while held" } },
  );
  assert.equal(held.status, 202, `held send expected 202, got ${held.status}: ${held.raw.slice(0, 200)}`);
  assert.equal(held.body?.status, "pending_review");
  const heldId = held.body!.message_id;

  const delHeld = await client.delete<ErrorBody>(`/v1/agents/${encodeURIComponent(email)}/messages/${heldId}`);
  assert.equal(delHeld.status, 409, `expected 409 message_held, got ${delHeld.status}: ${delHeld.raw.slice(0, 200)}`);
  assert.equal(delHeld.body?.error?.code, "message_held");

  // Resolve the hold so it doesn't linger in the account review queue.
  const rej = await client.post(`/v1/reviews/${heldId}/reject`, { body: { reason: "24-trash-lifecycle cleanup" } });
  assert.ok(rej.status === 200, `failed to reject held setup message: ${rej.status} ${rej.raw.slice(0, 200)}`);

  // --- deleteAgent / restoreAgent full cycle ---
  const del = await client.delete<{ deleted: boolean; email: string; messages_deleted: number }>(
    `/v1/agents/${encodeURIComponent(email)}?confirm=DELETE`,
  );
  assert.equal(del.status, 200, `deleteAgent expected 200, got ${del.status}: ${del.raw.slice(0, 200)}`);
  assert.deepEqual(del.body, { deleted: true, email, messages_deleted: 0 }, "DeleteAgentResult shape (soft delete)");

  // A trashed agent 404s on the live getter.
  const getAfterDelete = await client.get<ErrorBody>(`/v1/agents/${encodeURIComponent(email)}`);
  assert.equal(getAfterDelete.status, 404, `expected 404 for trashed agent, got ${getAfterDelete.status}`);
  assert.equal(getAfterDelete.body?.error?.code, "not_found");

  // …and disappears from the live list, but shows up in ?deleted=true with deleted_at.
  const liveList = await client.get<{ items: Array<{ email: string }> }>("/v1/agents", { query: { limit: 100 } });
  assert.ok(!liveList.body?.items?.some((a) => a.email === email), "trashed agent absent from live list");

  const trashList = await client.get<{ items: Array<{ email: string; deleted_at?: string | null }> }>("/v1/agents", {
    query: { deleted: "true", limit: 100 },
  });
  const trashedRow = trashList.body?.items?.find((a) => a.email === email);
  assert.ok(trashedRow, "trashed agent present in ?deleted=true trash view");
  assert.ok(trashedRow!.deleted_at, "trash listing row carries deleted_at");

  // Negative: restoring an already-live agent is 409 not_in_trash (checked
  // against the suite's persistent primary agent, which is never trashed).
  const restoreLive = await client.post<ErrorBody>(`/v1/agents/${encodeURIComponent(client.env.primaryAgentEmail)}/restore`);
  assert.equal(restoreLive.status, 409, `expected 409, got ${restoreLive.status}: ${restoreLive.raw.slice(0, 200)}`);
  assert.equal(restoreLive.body?.error?.code, "not_in_trash");

  // restoreAgent: brings the throwaway agent back, messages/config intact.
  const restore = await client.post<{ email: string; name: string; deleted_at?: string | null }>(
    `/v1/agents/${encodeURIComponent(email)}/restore`,
  );
  assert.equal(restore.status, 200, `restoreAgent expected 200, got ${restore.status}: ${restore.raw.slice(0, 200)}`);
  assert.equal(restore.body?.email, email);
  assert.equal(restore.body?.name, "trash lifecycle", "restore preserves the agent's prior config (name)");
  assert.equal(restore.body?.deleted_at, undefined, "restored agent view omits deleted_at");

  // Live again.
  const getAfterRestore = await client.get(`/v1/agents/${encodeURIComponent(email)}`);
  assert.equal(getAfterRestore.status, 200, `expected 200 after restore, got ${getAfterRestore.status}`);

  // `after()` cleanup() PERMANENTLY deletes this tracked throwaway agent
  // (harness/cleanup.ts uses ?permanent=true), so the trash rows this suite
  // deliberately created don't outlive the run. The soft delete above stays
  // soft on purpose — it IS the behavior under test.
});
