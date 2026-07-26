import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { cleanup, track } from "../harness/cleanup.ts";
import { HttpMcpClient, callTool } from "../harness/mcp.ts";
import { uniqueSlug, uniqueSubject } from "../harness/fixtures.ts";
import { info, warn, writeReport } from "../harness/report.ts";

// Coverage-gate fill: agents + messages + attachments, the largest single
// batch of the 4-way MCP tool-coverage split (see mcp_coverage_gate.py /
// harness/mcp-coverage.ts). This suite is the sole owner of:
//   get_agent, update_agent, delete_agent, restore_agent, get_protection,
//   update_protection, delete_message, restore_message, forward_message,
//   update_message_labels, get_message_lifecycle, get_conversation,
//   list_conversations, get_attachment, get_attachment_data
//
// Free-plan budget (3 agents / 1 domain / 3000 msgs-mo) is tight, so this
// file creates exactly ONE throwaway agent for the whole suite and reuses it
// end to end: identity/protection tools first, then a single self-send
// loopback message drives labels/conversations/lifecycle/forward/trash, then
// one real send (no hold) to the SES simulator produces a retrievable
// attachment (the same pattern 20-attachments.test.ts establishes — a HELD
// draft has no raw_message, so getAttachment 404s on it), and finally the
// agent itself goes through its own delete/restore cycle.
//
// Tests are intentionally sequential and share one lazily-created agent —
// mirrors 24-trash-lifecycle.test.ts's economy-of-agents convention — so run
// order matters here (this file is always run alone per the task brief, and
// `npm test` runs suites with --test-concurrency=1).

const SUITE = "27-mcp-agents-messages";
const apiClient = new ApiClient();
const mcp = new HttpMcpClient(apiClient.env.mcpUrl, apiClient.env.apiKey);
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

before(async () => {
  info(SUITE, "transport", `MCP over HTTP → ${apiClient.env.mcpUrl}`);
  // Feeds the mcp-coverage denominator (tools/list) even if every other test
  // in the file somehow short-circuits.
  const list = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  info(SUITE, "tools-list", `server advertises ${list.tools.length} tools`);
});

after(async () => {
  await mcp.stop();
  const r = await cleanup(apiClient);
  if (r.failed.length) warn(SUITE, "cleanup", `failed ${r.failed.length}`, r.failed);
  writeReport(`./reports/${SUITE}.json`);
});

function extractText(r: { content?: Array<{ type: string; text?: string }> }): string {
  return r.content?.find((c) => c.type === "text")?.text ?? "";
}

function parseJson<T>(r: { content?: Array<{ type: string; text?: string }> }): T {
  return JSON.parse(extractText(r)) as T;
}

// ── The one throwaway agent shared by the whole file ─────────────────────

let agentPromise: Promise<string> | undefined;
function agent(): Promise<string> {
  agentPromise ??= (async () => {
    const slug = uniqueSlug("mcpam");
    const email = `${slug}@${apiClient.env.sharedDomain}`;
    const created = await apiClient.post<{ email: string }>("/v1/agents", {
      body: { email, name: "mcp agents-messages batch" },
      expect: 201,
    });
    // Track before assertions so teardown owns cleanup even if the response
    // is malformed.
    track("agent", email);
    assert.equal(created.body?.email, email);
    return email;
  })();
  return agentPromise;
}

// ── get_agent / update_agent ──────────────────────────────────────────────

test("mcp-agents-msgs: get_agent returns the created agent's identity", async () => {
  const email = await agent();
  const r = await callTool(mcp, "get_agent", { email });
  assert.equal(r.isError, undefined, `get_agent isError: ${extractText(r).slice(0, 200)}`);
  const parsed = parseJson<{ email?: string; name?: string }>(r);
  assert.equal(parsed.email, email, "get_agent returns the requested agent");
  assert.equal(parsed.name, "mcp agents-messages batch", "get_agent echoes the display name set at creation");
});

test("mcp-agents-msgs: update_agent renames the agent", async () => {
  const email = await agent();
  const newName = `mcp batch renamed ${Date.now()}`;
  const r = await callTool(mcp, "update_agent", { email, name: newName });
  assert.equal(r.isError, undefined, `update_agent isError: ${extractText(r).slice(0, 200)}`);
  const parsed = parseJson<{ email?: string; name?: string }>(r);
  assert.equal(parsed.email, email);
  assert.equal(parsed.name, newName, "update_agent applies the new display name");

  // Confirm it stuck via a fresh read.
  const check = await callTool(mcp, "get_agent", { email });
  assert.equal(check.isError, undefined);
  assert.equal(parseJson<{ name?: string }>(check).name, newName, "renamed name persists across a fresh get_agent");
});

// ── get_protection / update_protection (beta) ─────────────────────────────

test("mcp-agents-msgs: get_protection reads the agent's protection posture", async () => {
  const email = await agent();
  const r = await callTool(mcp, "get_protection", { email });
  assert.equal(r.isError, undefined, `get_protection isError: ${extractText(r).slice(0, 200)}`);
  const parsed = parseJson<{
    inbound?: { gate?: { policy?: string } };
    outbound?: { gate?: { policy?: string } };
    holds?: { on_expiry?: string; ttl_seconds?: number };
  }>(r);
  assert.ok(parsed.inbound?.gate?.policy, "protection view carries an inbound gate policy");
  assert.ok(parsed.outbound?.gate?.policy, "protection view carries an outbound gate policy");
  // suppress_notifications is boolean `false`-omitted on the wire (only
  // present when true), so assert on a field that's always emitted instead.
  assert.ok(parsed.holds?.on_expiry, "protection view carries a holds.on_expiry");
  assert.equal(typeof parsed.holds?.ttl_seconds, "number", "holds.ttl_seconds is a number");
});

test("mcp-agents-msgs: update_protection read-modify-writes a single field", async () => {
  const email = await agent();
  // holds_suppress_notifications is a purely-cosmetic toggle (no gate/scan
  // change), so it can't perturb the later real-send/forward tests in this
  // file that depend on the default open outbound policy.
  const before = await callTool(mcp, "get_protection", { email });
  assert.equal(before.isError, undefined);
  const beforeCfg = parseJson<{ holds?: { suppress_notifications?: boolean } }>(before);
  const flipped = !beforeCfg.holds?.suppress_notifications;

  const r = await callTool(mcp, "update_protection", { email, holds_suppress_notifications: flipped });
  assert.equal(r.isError, undefined, `update_protection isError: ${extractText(r).slice(0, 200)}`);
  const parsed = parseJson<{ holds?: { suppress_notifications?: boolean } }>(r);
  assert.equal(parsed.holds?.suppress_notifications, flipped, "update_protection applies the requested field");

  // Read-modify-write: everything else should be untouched.
  const after = await callTool(mcp, "get_protection", { email });
  const afterCfg = parseJson<{ inbound?: { gate?: { policy?: string } }; outbound?: { gate?: { policy?: string } } }>(after);
  const beforeFull = parseJson<{ inbound?: { gate?: { policy?: string } }; outbound?: { gate?: { policy?: string } } }>(before);
  assert.equal(afterCfg.inbound?.gate?.policy, beforeFull.inbound?.gate?.policy, "unrelated inbound gate policy untouched");
  assert.equal(afterCfg.outbound?.gate?.policy, beforeFull.outbound?.gate?.policy, "unrelated outbound gate policy untouched");

  // Restore the original value so later tests in this file see the agent's
  // original posture (defensive; the field itself is inert either way).
  const restore = await callTool(mcp, "update_protection", { email, holds_suppress_notifications: !flipped });
  assert.equal(restore.isError, undefined);
});

// ── A self-send loopback message, shared by labels/conversations/lifecycle/forward/trash ──

interface LoopbackFixture {
  messageId: string;
  conversationId: string;
  subject: string;
}

let loopbackPromise: Promise<LoopbackFixture> | undefined;
function loopback(): Promise<LoopbackFixture> {
  loopbackPromise ??= (async () => {
    const email = await agent();
    const subject = uniqueSubject("mcp-batch-loopback");
    const send = await apiClient.post<{ status: string; message_id: string }>(
      `/v1/agents/${encodeURIComponent(email)}/messages`,
      { body: { to: [email], subject, text: "27-mcp-agents-messages loopback body" }, expect: [200, 202] },
    );
    assert.ok(send.body?.message_id, `loopback send did not return a message_id: ${send.raw.slice(0, 200)}`);

    for (let i = 0; i < 12; i++) {
      const list = await apiClient.get<{ items: Array<{ id: string; subject: string; conversation_id: string }> }>(
        `/v1/agents/${encodeURIComponent(email)}/messages`,
        { query: { direction: "inbound", read_status: "all", limit: 20 } },
      );
      const m = list.body?.items?.find((x) => x.subject === subject);
      if (m) return { messageId: m.id, conversationId: m.conversation_id, subject };
      await sleep(1500);
    }
    throw new Error(`loopback message "${subject}" never appeared for ${email}`);
  })();
  return loopbackPromise;
}

// ── update_message_labels ─────────────────────────────────────────────────

test("mcp-agents-msgs: update_message_labels adds then removes a label", async () => {
  const email = await agent();
  const { messageId } = await loopback();
  const label = "e2e-mcp-batch";

  const add = await callTool(mcp, "update_message_labels", { message_id: messageId, add_labels: [label], email });
  assert.equal(add.isError, undefined, `update_message_labels(add) isError: ${extractText(add).slice(0, 200)}`);
  const added = parseJson<{ message_id?: string; labels?: string[] }>(add);
  assert.equal(added.message_id, messageId);
  assert.ok(added.labels?.includes(label), `expected "${label}" in labels, got ${JSON.stringify(added.labels)}`);

  const remove = await callTool(mcp, "update_message_labels", { message_id: messageId, remove_labels: [label], email });
  assert.equal(remove.isError, undefined, `update_message_labels(remove) isError: ${extractText(remove).slice(0, 200)}`);
  const removed = parseJson<{ labels?: string[] }>(remove);
  assert.ok(!removed.labels?.includes(label), `expected "${label}" removed, got ${JSON.stringify(removed.labels)}`);
});

// ── list_conversations / get_conversation ─────────────────────────────────

test("mcp-agents-msgs: list_conversations surfaces the loopback thread", async () => {
  const email = await agent();
  const { conversationId } = await loopback();

  const r = await callTool(mcp, "list_conversations", { email });
  assert.equal(r.isError, undefined, `list_conversations isError: ${extractText(r).slice(0, 200)}`);
  const parsed = parseJson<{ conversations?: Array<{ id: string; message_count: number }> }>(r);
  const conv = parsed.conversations?.find((c) => c.id === conversationId);
  assert.ok(conv, `conversation ${conversationId} not found in list_conversations (${parsed.conversations?.length ?? 0} rows)`);
  assert.ok(conv!.message_count >= 1, "our conversation has at least the loopback message");
});

test("mcp-agents-msgs: get_conversation returns the loopback thread with its member message", async () => {
  const email = await agent();
  const { conversationId, messageId } = await loopback();

  const r = await callTool(mcp, "get_conversation", { conversation_id: conversationId, email });
  assert.equal(r.isError, undefined, `get_conversation isError: ${extractText(r).slice(0, 200)}`);
  const parsed = parseJson<{ id?: string; messages?: Array<{ id: string }>; participants?: string[] }>(r);
  assert.equal(parsed.id, conversationId);
  assert.ok(parsed.messages?.some((m) => m.id === messageId), "get_conversation includes our loopback message");
  assert.ok(parsed.participants?.includes(email), "participants includes the self-send agent");
});

// ── get_message_lifecycle (beta) ──────────────────────────────────────────

test("mcp-agents-msgs: get_message_lifecycle (beta) returns transitions for the loopback message", async () => {
  const email = await agent();
  const { messageId } = await loopback();

  const r = await callTool(mcp, "get_message_lifecycle", { message_id: messageId, email });
  assert.equal(r.isError, undefined, `get_message_lifecycle isError: ${extractText(r).slice(0, 200)}`);
  const parsed = parseJson<{ transitions?: Array<{ message_id: string; outcome: string; reason_code: string }> }>(r);
  assert.ok(Array.isArray(parsed.transitions) && parsed.transitions.length > 0, "at least one lifecycle transition recorded");
  for (const t of parsed.transitions!) {
    assert.equal(t.message_id, messageId, `transition message_id mismatch: ${JSON.stringify(t)}`);
  }
});

// ── forward_message ────────────────────────────────────────────────────────

test("mcp-agents-msgs: forward_message forwards the loopback message as a new thread", async () => {
  const email = await agent();
  const { messageId } = await loopback();

  const r = await callTool(mcp, "forward_message", {
    message_id: messageId,
    to: [email],
    text: "fwd comment from 27-mcp-agents-messages",
    email,
  });
  assert.equal(r.isError, undefined, `forward_message isError: ${extractText(r).slice(0, 200)}`);
  const parsed = parseJson<{ status?: string; message_id?: string }>(r);
  assert.ok(parsed.message_id?.startsWith("msg_"), `expected msg_ id, got "${parsed.message_id}"`);
  assert.ok(
    parsed.status === "accepted" || parsed.status === "sent" || parsed.status === "pending_review",
    `forward_message must be accepted, sent, or held, got "${parsed.status}"`,
  );
  if (parsed.status === "pending_review") {
    // Default protection is open (no gate hold expected), but resolve
    // defensively so no dangling review-queue item survives the suite.
    const rej = await apiClient.post(`/v1/reviews/${parsed.message_id}/reject`, {
      body: { reason: "27-mcp-agents-messages forward cleanup" },
    });
    assert.ok(rej.status === 200, `failed to reject held forward: ${rej.status} ${rej.raw.slice(0, 200)}`);
  }
});

// ── delete_message / restore_message ──────────────────────────────────────

test("mcp-agents-msgs: delete_message then restore_message round-trips the loopback message", async () => {
  const email = await agent();
  const { messageId } = await loopback();

  const del = await callTool(mcp, "delete_message", { message_id: messageId, email, confirm: true });
  assert.equal(del.isError, undefined, `delete_message isError: ${extractText(del).slice(0, 200)}`);
  const delParsed = parseJson<{ deleted?: boolean; id?: string }>(del);
  assert.deepEqual(delParsed, { deleted: true, id: messageId }, "delete_message result shape");

  // Confirm it dropped out of the live listing via the REST surface.
  const liveList = await apiClient.get<{ items: Array<{ id: string }> }>(
    `/v1/agents/${encodeURIComponent(email)}/messages`,
    { query: { direction: "inbound", read_status: "all", limit: 20 } },
  );
  assert.ok(!liveList.body?.items?.some((m) => m.id === messageId), "message absent from live listing after delete_message");

  const restore = await callTool(mcp, "restore_message", { message_id: messageId, email });
  assert.equal(restore.isError, undefined, `restore_message isError: ${extractText(restore).slice(0, 200)}`);
  const restoreParsed = parseJson<{ id?: string; deleted_at?: string | null }>(restore);
  assert.equal(restoreParsed.id, messageId);
  assert.equal(restoreParsed.deleted_at, undefined, "restored message view omits deleted_at");

  const liveAgain = await apiClient.get<{ items: Array<{ id: string }> }>(
    `/v1/agents/${encodeURIComponent(email)}/messages`,
    { query: { direction: "inbound", read_status: "all", limit: 20 } },
  );
  assert.ok(liveAgain.body?.items?.some((m) => m.id === messageId), "message back in live listing after restore_message");
});

// ── get_attachment / get_attachment_data ──────────────────────────────────
//
// A HELD (pending_review) draft has NO raw_message, so its attachments[] is
// empty and getAttachment 404s (see 20-attachments.test.ts). The default
// protection posture on our throwaway agent is open (never held), so a real
// no-hold send to the SES simulator is a reliable way to get retrievable
// attachment bytes without depending on external MX.

const ATTACH_TEXT = "hello mcp-agents-messages attachment";
const ATTACH_B64 = Buffer.from(ATTACH_TEXT, "utf8").toString("base64");
const ATTACH_FILENAME = "hello.txt";
const ATTACH_CTYPE = "text/plain";

interface AttachmentFixture {
  messageId: string;
}

let attachmentPromise: Promise<AttachmentFixture | null> | undefined;
function attachmentFixture(): Promise<AttachmentFixture | null> {
  attachmentPromise ??= (async () => {
    const email = await agent();
    const send = await apiClient.post<{ status: string; message_id: string }>(
      `/v1/agents/${encodeURIComponent(email)}/messages`,
      {
        body: {
          to: ["success@simulator.amazonses.com"],
          subject: uniqueSubject("mcp-batch-attach"),
          text: "see attached",
          attachments: [{ filename: ATTACH_FILENAME, content_type: ATTACH_CTYPE, data: ATTACH_B64 }],
        },
      },
    );
    if ((send.status !== 200 && send.status !== 202) || !send.body?.message_id) {
      warn(SUITE, "attachment-setup", `real send with attachment did not return 200/202+message_id (got ${send.status}); attachment tests flagged`, send.raw.slice(0, 200));
      return null;
    }
    const messageId = send.body.message_id;
    for (let attempt = 0; attempt < 24; attempt++) {
      const msg = await apiClient.get<{ attachments?: Array<{ index: number }> }>(
        `/v1/agents/${encodeURIComponent(email)}/messages/${messageId}`,
      );
      if (msg.status === 200 && Array.isArray(msg.body?.attachments) && msg.body!.attachments.length > 0) {
        return { messageId };
      }
      await sleep(1000);
    }
    warn(
      SUITE,
      "attachment-setup",
      "real send accepted the attachment but never exposed a retrievable attachments[] within the poll window; attachment tests flagged",
    );
    return null;
  })();
  return attachmentPromise;
}

test("mcp-agents-msgs: get_attachment returns metadata + a short-lived download_url", async () => {
  const email = await agent();
  const fixture = await attachmentFixture();
  assert.ok(fixture, "no attachment fixture available — a missing fixture is a broken test, not a reason to pass");
  const r = await callTool(mcp, "get_attachment", { message_id: fixture.messageId, attachment_index: 0, email });
  assert.equal(r.isError, undefined, `get_attachment isError: ${extractText(r).slice(0, 200)}`);
  const parsed = parseJson<{
    index?: number;
    filename?: string;
    content_type?: string;
    size_bytes?: number;
    download_url?: string;
    expires_at?: string;
    data?: string;
  }>(r);
  assert.equal(parsed.index, 0);
  assert.equal(parsed.filename, ATTACH_FILENAME);
  assert.equal(parsed.content_type, ATTACH_CTYPE);
  assert.equal(parsed.size_bytes, ATTACH_TEXT.length);
  assert.ok(parsed.download_url?.length, "download_url present");
  assert.ok(parsed.data === undefined, "no inline data unless inline:true was requested");
});

test("mcp-agents-msgs: get_attachment_data (legacy alias) returns inline base64 that round-trips", async () => {
  const email = await agent();
  const fixture = await attachmentFixture();
  assert.ok(fixture, "no attachment fixture available — a missing fixture is a broken test, not a reason to pass");
  const r = await callTool(mcp, "get_attachment_data", { message_id: fixture.messageId, attachment_index: 0, agent_email: email });
  assert.equal(r.isError, undefined, `get_attachment_data isError: ${extractText(r).slice(0, 200)}`);
  const parsed = parseJson<{ filename?: string; content_type?: string; size_bytes?: number; data?: string }>(r);
  assert.equal(parsed.filename, ATTACH_FILENAME);
  assert.equal(parsed.content_type, ATTACH_CTYPE);
  assert.ok(typeof parsed.data === "string" && parsed.data.length > 0, "inline base64 data present");
  const decoded = Buffer.from(parsed.data!, "base64").toString("utf8");
  assert.equal(decoded, ATTACH_TEXT, "get_attachment_data round-trips the exact sent bytes");
});

// ── delete_agent / restore_agent ──────────────────────────────────────────
// Runs LAST: everything above depends on the agent staying live.

test("mcp-agents-msgs: delete_agent then restore_agent round-trips the throwaway agent", async () => {
  const email = await agent();

  const del = await callTool(mcp, "delete_agent", { email, confirm: true });
  assert.equal(del.isError, undefined, `delete_agent isError: ${extractText(del).slice(0, 200)}`);
  const delParsed = parseJson<{ deleted?: boolean; email?: string }>(del);
  assert.equal(delParsed.deleted, true);
  assert.equal(delParsed.email, email);

  // Trashed agent 404s on a live GET (via REST, matching 24-trash-lifecycle).
  const getAfterDelete = await apiClient.get(`/v1/agents/${encodeURIComponent(email)}`);
  assert.equal(getAfterDelete.status, 404, `expected 404 for trashed agent, got ${getAfterDelete.status}`);

  const restore = await callTool(mcp, "restore_agent", { email });
  assert.equal(restore.isError, undefined, `restore_agent isError: ${extractText(restore).slice(0, 200)}`);
  const restoreParsed = parseJson<{ email?: string; deleted_at?: string | null }>(restore);
  assert.equal(restoreParsed.email, email);
  assert.equal(restoreParsed.deleted_at, undefined, "restored agent view omits deleted_at");

  const getAfterRestore = await apiClient.get(`/v1/agents/${encodeURIComponent(email)}`);
  assert.equal(getAfterRestore.status, 200, `expected 200 after restore, got ${getAfterRestore.status}`);

  // `after()`'s cleanup() re-trashes this tracked throwaway agent (soft
  // delete), matching every other suite's teardown-into-trash convention.
});
