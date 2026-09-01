import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { uniqueSlug, uniqueSubject, holdAllOutbound } from "../harness/fixtures.ts";
import { fail, info, warn, writeReport } from "../harness/report.ts";

// Black-box conformance for the attachments surface (SSOT: api/openapi.yaml +
// the non-openapi raw download route). Two endpoints under test:
//   1. getAttachment  — GET /v1/agents/{email}/messages/{id}/attachments/{index}
//        (Huma/bearer; returns AttachmentView = metadata + short-lived
//         download_url + expires_at; ?inline=true adds base64 `data` for
//         attachments <= 256 KB, else 413). Verified against openapi shapes.
//   2. the raw DOWNLOAD route — GET .../attachments/{index}/download?token=...
//        (NOT in openapi; a chi route outside Huma. The capability TOKEN in the
//         URL authorizes the stream — no bearer. Errors are PLAIN TEXT via
//         http.Error, NOT the JSON error envelope — this is by design for the
//         raw route, pinned below, not a spec violation since it's off-contract.)
//
// Producing a message WITH a retrievable attachment on staging:
//   - Inbound-with-attachments is unavailable (no MX on staging).
//   - HELD outbound (holdAllOutbound) does NOT work: a pending_review draft has
//     NO raw_message, so `attachments` is [] and getAttachment 404s. This is a
//     deliberate server property (internal/httpapi/messages.go: "held drafts
//     (no raw_message) carry []"). We assert it below rather than assume it.
//   - REAL outbound to the SES simulator (success@simulator.amazonses.com)
//     WITHOUT holding DOES work: the send returns 202 "accepted" (a no-hold
//     send is async — the River worker delivers it and persists the sent MIME
//     afterwards), and getAttachment/download then operate on real bytes. This
//     is the happy path this suite uses; the setup polls the sent message to
//     tolerate the async worker. If a real send ever stops yielding a
//     retrievable attachment, the happy-path tests self-downgrade to a flagged
//     staging limitation (see ensureSentAttachment) rather than fail.

const SUITE = "20-attachments";
const client = new ApiClient();

// Bytes we send as the attachment; the download must round-trip these exactly.
const ATTACH_TEXT = "hello attachment world";
const ATTACH_B64 = Buffer.from(ATTACH_TEXT, "utf8").toString("base64");
const ATTACH_FILENAME = "hello.txt";
const ATTACH_CTYPE = "text/plain";

// Throwaway agents to delete in `after` (delete cascades to messages).
const createdAgents: string[] = [];

interface SentAttachment {
  agentEmail: string;
  messageId: string;
}

// ensureSentAttachment lazily provisions ONE throwaway agent, does a REAL send
// with an attachment to the SES simulator (no hold), and polls the sent message
// until its parsed `attachments[]` is populated. Memoized so every happy-path
// test shares the single fixture regardless of run order. Returns null (after
// recording a warn) if staging can't produce a retrievable attachment — the
// happy-path tests then self-skip and the limitation is flagged, per the brief.
let setupPromise: Promise<SentAttachment | null> | undefined;
function ensureSentAttachment(): Promise<SentAttachment | null> {
  if (!setupPromise) setupPromise = doSetup();
  return setupPromise;
}

async function doSetup(): Promise<SentAttachment | null> {
  const slug = uniqueSlug("attach");
  const agentEmail = `${slug}@${client.env.sharedDomain}`;
  const create = await client.post<{ email: string }>("/v1/agents", {
    body: { email: agentEmail, name: "attach fixture" },
  });
  if (create.status !== 201) {
    warn(SUITE, "setup", `could not create fixture agent (got ${create.status}); happy-path flagged`, create.raw.slice(0, 200));
    return null;
  }
  createdAgents.push(agentEmail);

  const send = await client.post<{ status: string; message_id: string }>(
    `/v1/agents/${encodeURIComponent(agentEmail)}/messages`,
    {
      body: {
        to: ["success@simulator.amazonses.com"],
        subject: uniqueSubject("attach"),
        text: "see attached",
        attachments: [{ filename: ATTACH_FILENAME, content_type: ATTACH_CTYPE, data: ATTACH_B64 }],
      },
    },
  );
  if ((send.status !== 200 && send.status !== 202) || !send.body?.message_id) {
    warn(SUITE, "setup", `real send with attachment did not return 200/202+message_id (got ${send.status}); happy-path flagged`, send.raw.slice(0, 200));
    return null;
  }
  const messageId = send.body.message_id;

  // A no-hold send is async (202 accepted): the worker persists the sent MIME
  // only after delivery, so poll generously before flagging a limitation.
  for (let attempt = 0; attempt < 24; attempt++) {
    const msg = await client.get<{ attachments?: Array<{ index: number }> }>(
      `/v1/agents/${encodeURIComponent(agentEmail)}/messages/${messageId}`,
    );
    if (msg.status === 200 && Array.isArray(msg.body?.attachments) && msg.body!.attachments.length > 0) {
      return { agentEmail, messageId };
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
  warn(
    SUITE,
    "setup",
    "STAGING LIMITATION: a real send accepted the attachment but the sent message never exposed a retrievable attachments[] within the poll window; happy-path (real retrieval) flagged, negatives still run",
  );
  return null;
}

// Build the fixture ONCE up front. When staging can't produce a retrievable
// attachment, the real-retrieval + capability-token tests SKIP (not silently
// pass) — so a broken fixture can never hide a regression in the authz coverage.
const fixture = await ensureSentAttachment();
const fxSkip = fixture ? false : "staging could not produce a retrievable attachment fixture";

// ── Happy path (real attachment retrieval) ───────────────────────────────────

test("getAttachment: returns AttachmentView (metadata + short-lived download_url)", { skip: fxSkip }, async () => {
  const s = fixture!;
  const r = await client.get<{
    index: number;
    filename?: string;
    content_type?: string;
    size_bytes: number;
    download_url: string;
    expires_at: string;
    data?: string;
  }>(`/v1/agents/${encodeURIComponent(s.agentEmail)}/messages/${s.messageId}/attachments/0`);

  assert.equal(r.status, 200, `expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
  // openapi AttachmentView required: index, size_bytes, download_url, expires_at.
  assert.equal(r.body?.index, 0, "index is the 0-based attachment index");
  assert.equal(typeof r.body?.size_bytes, "number", "size_bytes is int");
  assert.equal(r.body?.size_bytes, ATTACH_TEXT.length, "size_bytes matches sent bytes");
  assert.ok(typeof r.body?.download_url === "string" && r.body!.download_url.length > 0, "download_url present");
  assert.ok(r.body?.download_url?.includes("/attachments/0/download?token="), "download_url is the capability-token route");
  // expires_at is an RFC3339 date-time in the future (15-min TTL server-side).
  const exp = Date.parse(r.body!.expires_at);
  assert.ok(!Number.isNaN(exp), "expires_at is a parseable date-time");
  assert.ok(exp > Date.now(), "download_url has not already expired");
  // Optional metadata the server does populate for this send.
  assert.equal(r.body?.filename, ATTACH_FILENAME, "filename echoed");
  assert.equal(r.body?.content_type, ATTACH_CTYPE, "content_type echoed");
  // Without inline, bytes must NOT be present (the whole point of §6a #5).
  assert.ok(r.body?.data === undefined, "no inline `data` unless inline=true");
});

test("getAttachment: ?inline=true returns base64 data that round-trips", { skip: fxSkip }, async () => {
  const s = fixture!;
  const r = await client.get<{ data?: string; size_bytes: number }>(
    `/v1/agents/${encodeURIComponent(s.agentEmail)}/messages/${s.messageId}/attachments/0`,
    { query: { inline: "true" } },
  );
  assert.equal(r.status, 200, `expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.ok(typeof r.body?.data === "string", "inline=true adds base64 `data` for a small attachment");
  const decoded = Buffer.from(r.body!.data!, "base64").toString("utf8");
  assert.equal(decoded, ATTACH_TEXT, "inline data decodes back to the sent bytes");
});

test("getAttachment: out-of-range index returns 404 attachment_not_found", { skip: fxSkip }, async () => {
  const s = fixture!;
  const r = await client.get<{ error?: { code?: string } }>(
    `/v1/agents/${encodeURIComponent(s.agentEmail)}/messages/${s.messageId}/attachments/9`,
  );
  assert.equal(r.status, 404, `out-of-range index → 404, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.equal(r.body?.error?.code, "attachment_not_found", "error envelope carries attachment_not_found");
});

test("download: valid capability token streams the exact bytes", { skip: fxSkip }, async () => {
  const s = fixture!;
  const meta = await client.get<{ download_url: string }>(
    `/v1/agents/${encodeURIComponent(s.agentEmail)}/messages/${s.messageId}/attachments/0`,
  );
  assert.equal(meta.status, 200);
  // download_url is minted against the deployment's public base (staging.e2a.dev);
  // re-target it at the client's apiUrl (the local proxy). The token is an
  // HMAC over message|index|exp — host-independent — so this is faithful.
  const path = pathAndQuery(meta.body!.download_url);
  const dl = await client.get<never>(path, { apiKey: null });
  assert.equal(dl.status, 200, `valid-token download → 200, got ${dl.status}: ${dl.raw.slice(0, 200)}`);
  assert.equal(dl.raw, ATTACH_TEXT, "streamed body equals the sent attachment bytes");
  assert.ok(
    (dl.headers["content-type"] ?? "").startsWith(ATTACH_CTYPE),
    `content-type is the attachment's, got ${dl.headers["content-type"]}`,
  );
  assert.equal(dl.headers["x-content-type-options"], "nosniff", "download is served nosniff");
});

test("download: token bound to index 0 is rejected for index 1 (capability scoping)", { skip: fxSkip }, async () => {
  const s = fixture!;
  const meta = await client.get<{ download_url: string }>(
    `/v1/agents/${encodeURIComponent(s.agentEmail)}/messages/${s.messageId}/attachments/0`,
  );
  assert.equal(meta.status, 200);
  const path = pathAndQuery(meta.body!.download_url).replace("/attachments/0/download", "/attachments/1/download");
  const dl = await client.get(path, { apiKey: null });
  assert.equal(dl.status, 403, `index-swapped token → 403, got ${dl.status}: ${dl.raw.slice(0, 200)}`);
});

test("download: valid token is rejected when the {email} path names a different agent (path-agent binding)", { skip: fxSkip }, async () => {
  const s = fixture!;
  const meta = await client.get<{ download_url: string }>(
    `/v1/agents/${encodeURIComponent(s.agentEmail)}/messages/${s.messageId}/attachments/0`,
  );
  assert.equal(meta.status, 200);
  // The token binds message+index but NOT identity; the handler additionally
  // keys GetMessage by the path agent. Swap the path agent to a DIFFERENT owned
  // agent (the primary) — the message doesn't belong to it → 404 not-found.
  // NB: the minted path keeps the RAW email (Go url.PathEscape leaves `@`
  // unescaped), so swap on the raw address, not an encodeURIComponent form.
  const path = pathAndQuery(meta.body!.download_url).replace(
    `/agents/${s.agentEmail}/`,
    `/agents/${client.env.primaryAgentEmail}/`,
  );
  assert.ok(path.includes(`/agents/${client.env.primaryAgentEmail}/`), "path-agent swap applied");
  const dl = await client.get(path, { apiKey: null });
  assert.equal(dl.status, 404, `token replayed under a different agent path → 404, got ${dl.status}: ${dl.raw.slice(0, 200)}`);
});

// ── Held-draft has no retrievable attachment (documents WHY we real-send) ─────

test("getAttachment: a HELD (pending_review) draft carrying an attachment exposes NO attachment (404)", async () => {
  const slug = uniqueSlug("attach-held");
  const email = `${slug}@${client.env.sharedDomain}`;
  const create = await client.post<{ email: string }>("/v1/agents", {
    body: { email, name: "attach held" },
  });
  assert.equal(create.status, 201, `create expected 201, got ${create.status}: ${create.raw.slice(0, 200)}`);
  createdAgents.push(email);

  const hold = await holdAllOutbound(client, email);
  assert.equal(hold.status, 200, `hold-all-outbound expected 200, got ${hold.status}`);

  const send = await client.post<{ status: string; message_id: string }>(
    `/v1/agents/${encodeURIComponent(email)}/messages`,
    {
      body: {
        to: ["success@simulator.amazonses.com"],
        subject: uniqueSubject("held attach"),
        text: "held with attachment",
        attachments: [{ filename: ATTACH_FILENAME, content_type: ATTACH_CTYPE, data: ATTACH_B64 }],
      },
    },
  );
  assert.equal(send.status, 202, `held send expected 202 pending_review, got ${send.status}: ${send.raw.slice(0, 200)}`);
  const messageId = send.body!.message_id;

  // The draft carries no raw_message, so `attachments` is [] and index 0 404s.
  const msg = await client.get<{ attachments?: unknown[] }>(
    `/v1/agents/${encodeURIComponent(email)}/messages/${messageId}`,
  );
  assert.equal(msg.status, 200);
  assert.deepEqual(msg.body?.attachments, [], "held draft exposes an EMPTY attachments[] (no raw_message)");

  const att = await client.get<{ error?: { code?: string } }>(
    `/v1/agents/${encodeURIComponent(email)}/messages/${messageId}/attachments/0`,
  );
  assert.equal(att.status, 404, `getAttachment on a held draft → 404, got ${att.status}: ${att.raw.slice(0, 200)}`);

  // Resolve the hold so we don't leave a dangling pending_review item.
  const reject = await client.post(
    `/v1/reviews/${messageId}/reject`,
    { body: { reason: "e2e attachments suite cleanup" } },
  );
  assert.ok(reject.status === 200, `reject expected 200, got ${reject.status}: ${reject.raw.slice(0, 200)}`);
});

// ── Negative paths (no attachment fixture required) ──────────────────────────

test("getAttachment: nonexistent message returns 404 not_found", async () => {
  const r = await client.get<{ error?: { code?: string } }>(
    `/v1/agents/${encodeURIComponent(client.env.primaryAgentEmail)}/messages/msg_doesnotexist_${Date.now()}/attachments/0`,
  );
  assert.equal(r.status, 404, `nonexistent message → 404, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.equal(r.body?.error?.code, "not_found", "error envelope carries not_found");
});

test("getAttachment: unauthenticated returns 401", async () => {
  const r = await client.get(
    `/v1/agents/${encodeURIComponent(client.env.primaryAgentEmail)}/messages/msg_x/attachments/0`,
    { apiKey: null },
  );
  assert.equal(r.status, 401, `missing bearer → 401, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("getAttachment: unowned agent returns 404 not_found (anti-enumeration; matches resolveOwnedAgent)", async () => {
  const r = await client.get(
    `/v1/agents/${encodeURIComponent(`nobody-${Date.now()}@example.com`)}/messages/msg_x/attachments/0`,
  );
  assert.equal(r.status, 404, `unowned agent → 404 not_found, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("download: missing token returns 401", async () => {
  const r = await client.get(
    `/v1/agents/${encodeURIComponent(client.env.primaryAgentEmail)}/messages/msg_x/attachments/0/download`,
    { apiKey: null },
  );
  assert.equal(r.status, 401, `download with no token → 401, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("download: malformed token returns 403", async () => {
  const r = await client.get(
    `/v1/agents/${encodeURIComponent(client.env.primaryAgentEmail)}/messages/msg_x/attachments/0/download?token=garbage.abc`,
    { apiKey: null },
  );
  assert.equal(r.status, 403, `bad token → 403, got ${r.status}: ${r.raw.slice(0, 200)}`);
});

test("download: non-numeric / negative index returns 400", async () => {
  const email = encodeURIComponent(client.env.primaryAgentEmail);
  const bad = await client.get(
    `/v1/agents/${email}/messages/msg_x/attachments/abc/download?token=garbage.abc`,
    { apiKey: null },
  );
  assert.equal(bad.status, 400, `non-numeric index → 400, got ${bad.status}: ${bad.raw.slice(0, 200)}`);
  const neg = await client.get(
    `/v1/agents/${email}/messages/msg_x/attachments/-1/download?token=garbage.abc`,
    { apiKey: null },
  );
  assert.equal(neg.status, 400, `negative index → 400, got ${neg.status}: ${neg.raw.slice(0, 200)}`);
});

// pathAndQuery extracts a minted absolute download_url's path+query so it can be
// re-issued through the ApiClient (which resolves relative paths against apiUrl,
// i.e. the local proxy). The capability token is host-independent.
function pathAndQuery(absoluteUrl: string): string {
  const u = new URL(absoluteUrl);
  return u.pathname + u.search;
}

after(async () => {
  for (const email of createdAgents) {
    const del = await client.delete(`/v1/agents/${encodeURIComponent(email)}?confirm=DELETE&permanent=true`);
    if (!(del.status === 204 || del.status === 200)) {
      fail(SUITE, "cleanup", `failed to delete throwaway agent ${email} (got ${del.status})`, del.raw.slice(0, 200));
    }
  }
  info(SUITE, "cleanup", `deleted ${createdAgents.length} throwaway agent(s)`);
  // The raw download route returns PLAIN-TEXT errors (http.Error), not the JSON
  // error envelope the Huma surface uses — intentional for this off-openapi
  // capability route; recorded so a future contract sweep doesn't mis-flag it.
  info(SUITE, "download-route", "raw download route errors are plain-text (non-enveloped) by design; it is not part of openapi.yaml");
  await writeReport(`./reports/${SUITE}.json`);
});
