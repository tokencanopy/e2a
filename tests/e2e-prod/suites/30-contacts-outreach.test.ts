import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { ApiClient, type RawResponse } from "../harness/client.ts";
import { isEventsLogDisabled } from "../harness/event-capability.ts";
import { uniqueSlug } from "../harness/fixtures.ts";
import { track, cleanup } from "../harness/cleanup.ts";
import { HttpMcpClient, callTool } from "../harness/mcp.ts";
import { writeReport, info, warn } from "../harness/report.ts";

// Contacts + outreach (beta) conformance — the live coverage owner for the 11
// contacts operationIds, the 11 contacts MCP tools, and the contact.due event,
// none of which any other suite exercises. Shapes verified against
// api/openapi.yaml (the drift-gated SSOT) before these assertions were written.
//
// Section 1 — REST (account key), all 11 operationIds on their 2xx paths:
//   createContact (201 + Location; duplicate → 409 conflict),
//   getContact (ETag header, "…32-hex…" strong validator), updateContact
//   (If-Match 200 → new ETag; stale If-Match → 412 precondition_failed),
//   listContacts (cursor pagination with no overlap; source=manual filter;
//   bogus source → 400 invalid_filter), importContacts (batch_id + per-row
//   statuses incl. duplicate_in_batch; import_batch_id list filter),
//   deleteImportBatch (contacts_deleted/contacts_retained counts; batch
//   contact then 404s), deleteContact (200; get → 404 contact_not_found),
//   and the per-agent engagement quad listEngagements / getEngagement /
//   upsertEngagement (201 enrol + ETag, If-Match 200 update) /
//   deleteEngagement (200; engagement 404s, the account-level CONTACT
//   survives). Per-agent scoping is asserted by upserting the SAME contact
//   address under two agents and showing the stages stay independent.
//   Scoping probe: an agent-scoped API key (minted via POST
//   /v1/account/api-keys, scope=agent — the pattern 19-account establishes)
//   is 403 forbidden on the account-level contacts surface.
//
// Section 2 — MCP. All 11 contact tools called successfully (isError !==
// true is what mcp_coverage_gate.py counts) with FRESH addresses disjoint
// from section 1, asserting on the parsed tool-result content, not merely
// the absence of an error.
//
// Section 3 — contact.due. A fresh engagement with next_action_at in the
// PAST is picked up by the deployment's contactdue sweep (River periodic,
// 5-minute interval + RunOnStart, internal/contactdue/contactdue.go), which
// emits one contact.due event per due engagement. Proved to the same
// dual-assertion bar 21-webhook-events established: (1) the event's own
// delivery_status.matched_webhooks >= 1 AND (2) OUR fresh webhook's
// deliveries list an entry for contact.due with attempts >= 1 (the
// example.com sink 405s the POST — an ATTEMPT is asserted, never delivery
// success). The listEvents poll budget is ~8 minutes to straddle two sweep
// intervals. IMPORTANT: contact.due is a wake-up signal delivered to
// subscribed webhooks so an EXTERNAL agent runtime can act on it — this
// suite asserts the signal is emitted and fanned out; it does NOT claim the
// event launches an agent runtime.
//
// Cleanup: agents go through the shared harness (track/cleanup); contacts,
// engagements, import batches and webhooks are tracked in local sets/arrays
// and deleted best-effort in `after` — each delete individually wrapped so
// one failure (or a mid-section test abort) can't strand the rest, and
// 404/403 count as success (already gone). Coverage recording is automatic:
// ApiClient records each 2xx for coverage_gate.py, HttpMcpClient records
// each successful tools/call for mcp_coverage_gate.py, and the verified
// contact.due type is flushed to reports/event-coverage/<pid>.json below —
// the same inline shard pattern as 21-webhook-events (deliberately not
// harness infra; see that suite's module doc for why).
const SUITE = "30-contacts-outreach";
const client = new ApiClient();
const mcp = new HttpMcpClient(client.env.mcpUrl, client.env.apiKey);

const EVENT_COVERAGE_DIR = fileURLToPath(new URL("../reports/event-coverage/", import.meta.url));
const verifiedEventTypes = new Set<string>();

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
// A `since` filter with slack for host/server clock skew (same reasoning as
// 21-webhook-events): a `since` AFTER the event's server-side created_at
// would hide a just-emitted event → false RED.
const sinceNow = () => new Date(Date.now() - 5000).toISOString();

// ---------------------------------------------------------------------------
// Wire types (subset of api/openapi.yaml schemas the assertions read).
// ---------------------------------------------------------------------------
interface ErrorEnvelope {
  error?: { code?: string; message?: string };
}
interface ContactView {
  address: string;
  display_name: string;
  metadata: Record<string, unknown>;
  source: string;
  import_batch_id?: string;
  created_at: string;
  updated_at: string;
}
interface PageContactView {
  items: ContactView[];
  next_cursor: string | null;
}
interface ContactEngagementView {
  agent_email: string;
  address: string;
  stage: string;
  next_action_at: string | null;
  metadata: Record<string, unknown>;
  replied: boolean;
  suppressed: boolean;
  contact: { address: string; display_name: string; metadata: Record<string, unknown> };
  created_at: string;
  updated_at: string;
}
interface PageContactEngagementView {
  items: ContactEngagementView[];
  next_cursor: string | null;
}
interface ContactImportResult {
  batch_id: string;
  created: number;
  updated: number;
  skipped: number;
  failed: number;
  results: Array<{ index: number; status: string; code?: string; address?: string }>;
}
interface DeleteImportBatchResult {
  deleted: boolean;
  batch_id: string;
  contacts_deleted: number;
  contacts_retained: number;
  engagements_deleted: number;
}
interface EventView {
  id: string;
  type: string;
  schema_version: string;
  created_at: string;
  status: string;
  data: Record<string, unknown>;
  agent_email?: string;
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
  last_status_code?: number;
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

// ETags on this surface are strong validators: a quoted 32-hex digest
// (internal/httpapi/contacts.go contactETag / engagements.go engagementETag —
// sha256 truncated to 16 bytes, hex-encoded, quoted).
const ETAG_RE = /^"[0-9a-f]{32}"$/;

// ---------------------------------------------------------------------------
// Local resource tracking. The shared harness cleanup only knows agents and
// domains, so contacts/engagements/batches/webhooks are tracked here and
// deleted best-effort in `after`. Every address comes from uniqueSlug(), so
// nothing collides across runs even when a prior run's cleanup failed.
// ---------------------------------------------------------------------------
const trackedContacts = new Set<string>();
const trackedBatches = new Set<string>();
const trackedHooks = new Set<string>();
const trackedEngagements = new Set<string>(); // "<agent>\n<address>"

function trackEngagement(agent: string, address: string): void {
  trackedEngagements.add(`${agent}\n${address}`);
  // An upsert auto-creates the account-level contact when absent, so the
  // address always needs contact cleanup too.
  trackedContacts.add(address);
}
function untrackEngagement(agent: string, address: string): void {
  trackedEngagements.delete(`${agent}\n${address}`);
}

async function freshAgent(label: string): Promise<string> {
  const email = `${uniqueSlug(label)}@${client.env.sharedDomain}`;
  const r = await client.post<{ email: string }>("/v1/agents", {
    body: { email, name: `contacts ${label}` },
    expect: 201,
  });
  track("agent", email);
  return r.body!.email;
}

async function createContact(address: string, displayName?: string): Promise<void> {
  const r = await client.post<ContactView>("/v1/contacts", {
    body: { address, ...(displayName ? { display_name: displayName } : {}) },
    expect: 201,
  });
  trackedContacts.add(r.body!.address);
}

// ---------------------------------------------------------------------------
// Section 1 — REST contacts + engagements (account key).
// ---------------------------------------------------------------------------

test("createContact: 201 + Location; a duplicate create → 409 conflict", async () => {
  const address = `${uniqueSlug("ctdup")}@example.com`;
  const r = await client.post<ContactView>("/v1/contacts", {
    body: { address, display_name: "Coverage Contact", metadata: { origin: "suite-30" } },
  });
  assert.equal(r.status, 201, `createContact expected 201, got ${r.status}: ${r.raw.slice(0, 200)}`);
  trackedContacts.add(r.body!.address);
  assert.equal(r.body?.address, address, "ContactView echoes the canonical address");
  assert.equal(r.body?.source, "manual", "a direct create has source=manual");
  assert.equal(r.body?.display_name, "Coverage Contact");
  // RFC 3986 allows '@' unescaped in a path segment (pchar includes '@'), so
  // the server may legally emit either "/v1/contacts/a@b.com" or the
  // percent-encoded form. Compare the DECODED trailing segment instead of
  // demanding one spelling (prod emits the raw '@' via Go's url.PathEscape).
  const location = r.headers.location ?? "";
  const locationAddress = decodeURIComponent(location.slice(location.lastIndexOf("/") + 1));
  assert.ok(
    location.includes("/v1/contacts/") && locationAddress === address,
    `Location header names the new contact: ${r.headers.location}`,
  );

  // The address is canonicalized before storage, so a second create of the
  // same address is a conflict, not a second contact.
  const dup = await client.post<ErrorEnvelope>("/v1/contacts", { body: { address } });
  assert.equal(dup.status, 409, `duplicate create expected 409, got ${dup.status}: ${dup.raw.slice(0, 200)}`);
  assert.equal(dup.body?.error?.code, "conflict", "duplicate create carries error.code=conflict");
});

test("getContact/updateContact: ETag + If-Match optimistic concurrency (200 → stale 412)", async () => {
  const address = `${uniqueSlug("ctetag")}@example.com`;
  await createContact(address, "Original Name");

  const got = await client.get<ContactView>(`/v1/contacts/${encodeURIComponent(address)}`, { expect: 200 });
  const etag = got.headers.etag;
  assert.ok(etag && ETAG_RE.test(etag), `getContact returns a quoted 32-hex ETag, got ${JSON.stringify(etag)}`);
  assert.equal(got.body?.display_name, "Original Name");

  // A conditional write with the CURRENT validator applies and moves the ETag.
  const upd = await client.patch<ContactView>(`/v1/contacts/${encodeURIComponent(address)}`, {
    headers: { "If-Match": etag! },
    body: { display_name: "Renamed Contact" },
  });
  assert.equal(upd.status, 200, `PATCH with current ETag expected 200, got ${upd.status}: ${upd.raw.slice(0, 200)}`);
  assert.equal(upd.body?.display_name, "Renamed Contact", "the conditional write applied");
  const etag2 = upd.headers.etag;
  assert.ok(etag2 && ETAG_RE.test(etag2), `PATCH returns a fresh ETag, got ${JSON.stringify(etag2)}`);
  assert.notEqual(etag2, etag, "an accepted write moves the validator");

  // The same (now stale) validator must NOT silently overwrite.
  const stale = await client.patch<ErrorEnvelope>(`/v1/contacts/${encodeURIComponent(address)}`, {
    headers: { "If-Match": etag! },
    body: { display_name: "Stale Writer" },
  });
  assert.equal(stale.status, 412, `PATCH with stale ETag expected 412, got ${stale.status}: ${stale.raw.slice(0, 200)}`);
  assert.equal(stale.body?.error?.code, "precondition_failed", "stale write carries error.code=precondition_failed");

  const after = await client.get<ContactView>(`/v1/contacts/${encodeURIComponent(address)}`, { expect: 200 });
  assert.equal(after.body?.display_name, "Renamed Contact", "the stale write did not apply");
});

test("listContacts: cursor pagination without overlap; source filter; invalid source → 400 invalid_filter", async () => {
  // Three manual contacts for the pagination walk, one import-created contact
  // the source=manual filter must EXCLUDE.
  const paged = [`${uniqueSlug("ctpg1")}@example.com`, `${uniqueSlug("ctpg2")}@example.com`, `${uniqueSlug("ctpg3")}@example.com`];
  for (const a of paged) await createContact(a);
  const imported = `${uniqueSlug("ctpgi")}@example.com`;
  const imp = await client.post<ContactImportResult>("/v1/contacts/import", {
    body: { contacts: [{ address: imported }] },
    expect: 200,
  });
  trackedBatches.add(imp.body!.batch_id);
  trackedContacts.add(imported);

  // Walk with limit=2 until all three are seen (cap pages so a broken cursor
  // can't loop forever). Pages may also contain survivors of earlier runs —
  // the assertion is on OUR three: each seen exactly once, never twice.
  const seen = new Map<string, number>();
  let cursor: string | null = null;
  let pages = 0;
  do {
    const page: RawResponse<PageContactView> = await client.get<PageContactView>("/v1/contacts", {
      query: { limit: 2, ...(cursor ? { cursor } : {}) },
      expect: 200,
    });
    pages++;
    assert.ok(page.body!.items.length <= 2, "limit=2 clamps the page size");
    for (const item of page.body!.items) {
      assert.ok(!seen.has(item.address), `pagination overlap: ${item.address} appeared on two pages`);
      seen.set(item.address, 1);
    }
    cursor = page.body!.next_cursor;
  } while (cursor && pages < 8 && !paged.every((a) => seen.has(a)));
  for (const a of paged) assert.ok(seen.has(a), `paginated listing reached ${a}`);

  // source=manual returns the manual contact and not the import-created one.
  const manualOnly = await client.get<PageContactView>("/v1/contacts", {
    query: { source: "manual", limit: 100 },
    expect: 200,
  });
  assert.ok(manualOnly.body!.items.some((c) => c.address === paged[0]), "source=manual returns the manual contact");
  assert.ok(!manualOnly.body!.items.some((c) => c.address === imported), "source=manual excludes the import-created contact");
  for (const c of manualOnly.body!.items) assert.equal(c.source, "manual", "source filter is honored on every row");

  // A value outside the known provenance vocabulary is rejected, not ignored.
  const bogus = await client.get<ErrorEnvelope>("/v1/contacts", { query: { source: "bogus" } });
  assert.equal(bogus.status, 400, `invalid source expected 400, got ${bogus.status}: ${bogus.raw.slice(0, 200)}`);
  assert.equal(bogus.body?.error?.code, "invalid_filter", "invalid source carries error.code=invalid_filter");
});

test("importContacts: per-row outcomes incl. duplicate_in_batch; import_batch_id list filter", async () => {
  const a1 = `${uniqueSlug("ctim1")}@example.com`;
  const a2 = `${uniqueSlug("ctim2")}@example.com`;
  const a3 = `${uniqueSlug("ctim3")}@example.com`;
  const r = await client.post<ContactImportResult>("/v1/contacts/import", {
    body: {
      contacts: [
        { address: a1, display_name: "Import One" },
        { address: a2 },
        { address: a3 },
        { address: a1 }, // duplicated in-batch: exercises the per-row skip path
      ],
    },
  });
  assert.equal(r.status, 200, `importContacts expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
  const result = r.body!;
  assert.ok(typeof result.batch_id === "string" && result.batch_id.length > 0, "import returns a batch_id");
  trackedBatches.add(result.batch_id);
  for (const a of [a1, a2, a3]) trackedContacts.add(a);

  assert.equal(result.created, 3, "three fresh rows created");
  assert.equal(result.skipped, 1, "the in-batch duplicate is skipped");
  assert.equal(result.failed, 0, "no row failed");
  assert.equal(result.results.length, 4, "one result entry per submitted row, in request order");
  assert.deepEqual(
    result.results.map((x) => x.status),
    ["created", "created", "created", "skipped"],
    "per-row statuses",
  );
  assert.equal(result.results[3].code, "duplicate_in_batch", "the skipped row names why");

  // The batch filter lists exactly this batch's contacts.
  const batchList = await client.get<PageContactView>("/v1/contacts", {
    query: { import_batch_id: result.batch_id, limit: 100 },
    expect: 200,
  });
  assert.deepEqual(
    batchList.body!.items.map((c) => c.address).sort(),
    [a1, a2, a3].sort(),
    "import_batch_id filter returns exactly the batch's contacts",
  );
  for (const c of batchList.body!.items) {
    assert.equal(c.source, "import", "batch contacts have source=import");
    assert.equal(c.import_batch_id, result.batch_id, "the row carries its batch id");
  }
});

test("deleteImportBatch: reversal counts; a batch contact then 404s", async () => {
  const b1 = `${uniqueSlug("ctdel1")}@example.com`;
  const b2 = `${uniqueSlug("ctdel2")}@example.com`;
  const imp = await client.post<ContactImportResult>("/v1/contacts/import", {
    body: { contacts: [{ address: b1 }, { address: b2 }] },
    expect: 200,
  });
  const batchId = imp.body!.batch_id;
  trackedBatches.add(batchId);
  trackedContacts.add(b1);
  trackedContacts.add(b2);

  const del = await client.delete<DeleteImportBatchResult>(
    `/v1/contacts/imports/${encodeURIComponent(batchId)}?confirm=DELETE`,
  );
  assert.equal(del.status, 200, `deleteImportBatch expected 200, got ${del.status}: ${del.raw.slice(0, 200)}`);
  assert.equal(del.body?.deleted, true);
  assert.equal(del.body?.batch_id, batchId);
  // Neither contact has correspondence history, so both are removed.
  assert.equal(del.body?.contacts_deleted, 2, "both untouched batch contacts removed");
  assert.equal(del.body?.contacts_retained, 0, "nothing retained (no correspondence history)");
  assert.equal(del.body?.engagements_deleted, 0, "the import enrolled no engagements");
  trackedBatches.delete(batchId);
  trackedContacts.delete(b1);
  trackedContacts.delete(b2);

  const gone = await client.get<ErrorEnvelope>(`/v1/contacts/${encodeURIComponent(b1)}`);
  assert.equal(gone.status, 404, `batch contact after reversal expected 404, got ${gone.status}: ${gone.raw.slice(0, 200)}`);
  assert.equal(gone.body?.error?.code, "contact_not_found");
});

test("engagements: enrol (201+ETag) → If-Match update (200) → filters → per-agent isolation → delete", async () => {
  const agentA = await freshAgent("enga");
  const agentB = await freshAgent("engb");
  const address = `${uniqueSlug("cteng")}@example.com`;
  const future = new Date(Date.now() + 3_600_000).toISOString();
  const base = `/v1/agents/${encodeURIComponent(agentA)}/contacts`;

  // First enrolment → 201 + ETag + Location; the contact is auto-created.
  const enrol = await client.put<ContactEngagementView>(`${base}/${encodeURIComponent(address)}`, {
    body: { stage: "prospect", next_action_at: future },
  });
  assert.equal(enrol.status, 201, `first upsertEngagement expected 201, got ${enrol.status}: ${enrol.raw.slice(0, 200)}`);
  trackEngagement(agentA, address);
  const etag = enrol.headers.etag;
  assert.ok(etag && ETAG_RE.test(etag), `201 upsert returns an ETag, got ${JSON.stringify(etag)}`);
  assert.ok(enrol.headers.location?.includes(`/v1/agents/`), `201 upsert returns a Location: ${enrol.headers.location}`);
  assert.equal(enrol.body?.stage, "prospect");
  assert.equal(enrol.body?.agent_email, agentA);
  assert.equal(enrol.body?.replied, false, "a fresh engagement has not been replied to");

  // Conditional update with the current validator → 200 (update, not create).
  const upd = await client.put<ContactEngagementView>(`${base}/${encodeURIComponent(address)}`, {
    headers: { "If-Match": etag! },
    body: { stage: "nurture" },
  });
  assert.equal(upd.status, 200, `If-Match upsert expected 200, got ${upd.status}: ${upd.raw.slice(0, 200)}`);
  assert.equal(upd.body?.stage, "nurture", "the conditional update applied");
  assert.ok(upd.headers.etag && ETAG_RE.test(upd.headers.etag) && upd.headers.etag !== etag, "the update moves the ETag");
  // An omitted field is left unchanged. Compare parsed instants, not strings:
  // the server may normalize RFC 3339 precision differently from toISOString.
  assert.ok(
    upd.body?.next_action_at && Math.abs(Date.parse(upd.body.next_action_at) - Date.parse(future)) < 1000,
    `next_action_at preserved (sent ${future}, got ${upd.body?.next_action_at})`,
  );

  // getEngagement: 200 + ETag + the embedded contact.
  const got = await client.get<ContactEngagementView>(`${base}/${encodeURIComponent(address)}`, { expect: 200 });
  assert.ok(got.headers.etag && ETAG_RE.test(got.headers.etag), "getEngagement returns an ETag");
  assert.equal(got.body?.contact?.address, address, "the engagement embeds the contact");

  // listEngagements filters: stage match / non-match, replied=false / true.
  const byStage = await client.get<PageContactEngagementView>(base, { query: { stage: "nurture" }, expect: 200 });
  assert.ok(byStage.body!.items.some((e) => e.address === address), "stage=nurture lists the engagement");
  const wrongStage = await client.get<PageContactEngagementView>(base, { query: { stage: "prospect" }, expect: 200 });
  assert.ok(!wrongStage.body!.items.some((e) => e.address === address), "stage=prospect no longer matches after the update");
  const unreplied = await client.get<PageContactEngagementView>(base, { query: { replied: "false" }, expect: 200 });
  assert.ok(unreplied.body!.items.some((e) => e.address === address), "replied=false lists the fresh engagement");
  const replied = await client.get<PageContactEngagementView>(base, { query: { replied: "true" }, expect: 200 });
  assert.ok(!replied.body!.items.some((e) => e.address === address), "replied=true excludes it");

  // Per-agent scoping: the SAME contact address under a second agent is an
  // independent engagement — B's stage must not leak into A's view.
  const enrolB = await client.put<ContactEngagementView>(
    `/v1/agents/${encodeURIComponent(agentB)}/contacts/${encodeURIComponent(address)}`,
    { body: { stage: "qualify" } },
  );
  assert.equal(enrolB.status, 201, `agent B enrolment expected 201, got ${enrolB.status}: ${enrolB.raw.slice(0, 200)}`);
  trackEngagement(agentB, address);
  const listA = await client.get<PageContactEngagementView>(base, { expect: 200 });
  assert.equal(
    listA.body!.items.find((e) => e.address === address)?.stage,
    "nurture",
    "agent A's engagement is untouched by agent B's enrolment",
  );
  const listB = await client.get<PageContactEngagementView>(`/v1/agents/${encodeURIComponent(agentB)}/contacts`, { expect: 200 });
  assert.equal(listB.body!.items.find((e) => e.address === address)?.stage, "qualify", "agent B sees its own stage");

  // deleteEngagement: the engagement goes, the account-level CONTACT survives.
  const del = await client.delete<{ deleted: boolean; address: string }>(
    `${base}/${encodeURIComponent(address)}?confirm=DELETE`,
  );
  assert.equal(del.status, 200, `deleteEngagement expected 200, got ${del.status}: ${del.raw.slice(0, 200)}`);
  assert.equal(del.body?.deleted, true);
  assert.equal(del.body?.address, address);
  untrackEngagement(agentA, address);

  const gone = await client.get<ErrorEnvelope>(`${base}/${encodeURIComponent(address)}`);
  assert.equal(gone.status, 404, `deleted engagement expected 404, got ${gone.status}: ${gone.raw.slice(0, 200)}`);
  assert.equal(gone.body?.error?.code, "engagement_not_found");
  const contact = await client.get<ContactView>(`/v1/contacts/${encodeURIComponent(address)}`, { expect: 200 });
  assert.equal(contact.body?.address, address, "the account-level contact survives un-enrolment");
  const stillB = await client.get<ContactEngagementView>(
    `/v1/agents/${encodeURIComponent(agentB)}/contacts/${encodeURIComponent(address)}`,
    { expect: 200 },
  );
  assert.equal(stillB.body?.stage, "qualify", "agent B's engagement survives A's un-enrolment");
});

test("deleteContact: 200 deletion object; the contact then 404s", async () => {
  const address = `${uniqueSlug("ctrm")}@example.com`;
  await createContact(address);
  const del = await client.delete<{ deleted: boolean; address: string }>(
    `/v1/contacts/${encodeURIComponent(address)}?confirm=DELETE`,
  );
  assert.equal(del.status, 200, `deleteContact expected 200, got ${del.status}: ${del.raw.slice(0, 200)}`);
  assert.equal(del.body?.deleted, true);
  assert.equal(del.body?.address, address);
  trackedContacts.delete(address);

  const gone = await client.get<ErrorEnvelope>(`/v1/contacts/${encodeURIComponent(address)}`);
  assert.equal(gone.status, 404, `deleted contact expected 404, got ${gone.status}: ${gone.raw.slice(0, 200)}`);
  assert.equal(gone.body?.error?.code, "contact_not_found");
});

test("scoping: an agent-scoped API key is 403 forbidden on the account contacts surface", async () => {
  // Minting an agent-scoped key is the established pattern (19-account):
  // POST /v1/account/api-keys with scope=agent + agent_email.
  const agent = await freshAgent("ctkey");
  const mint = await client.post<{ id: string; key: string; scope: string }>("/v1/account/api-keys", {
    body: { name: uniqueSlug("ctkey"), scope: "agent", agent_email: agent },
  });
  assert.equal(mint.status, 201, `createApiKey(scope=agent) expected 201, got ${mint.status}: ${mint.raw.slice(0, 200)}`);
  const keyId = mint.body!.id;
  try {
    const list = await client.get<ErrorEnvelope>("/v1/contacts", { apiKey: mint.body!.key });
    assert.equal(list.status, 403, `agent-scoped listContacts expected 403, got ${list.status}: ${list.raw.slice(0, 200)}`);
    assert.equal(list.body?.error?.code, "forbidden", "the scope ceiling carries error.code=forbidden");
    const create = await client.post<ErrorEnvelope>("/v1/contacts", {
      apiKey: mint.body!.key,
      body: { address: `${uniqueSlug("ctsc")}@example.com` },
    });
    assert.equal(create.status, 403, `agent-scoped createContact expected 403, got ${create.status}: ${create.raw.slice(0, 200)}`);
  } finally {
    await client.delete(`/v1/account/api-keys/${encodeURIComponent(keyId)}?confirm=DELETE`);
  }
});

// ---------------------------------------------------------------------------
// Section 2 — MCP contact tools. Fresh addresses (mcp- prefix), disjoint from
// section 1, sequenced so the deletes run last. Each call asserts on the
// parsed tool-result content; isError !== true is what the coverage gate
// counts, and a bare no-error call would prove nothing about the payload.
// ---------------------------------------------------------------------------

before(async () => {
  info(SUITE, "transport", `MCP over HTTP → ${client.env.mcpUrl}`);
  // Feeds the mcp-coverage denominator (tools/list) even if a later test
  // short-circuits.
  const list = await mcp.call<{ tools: Array<{ name: string }> }>("tools/list");
  info(SUITE, "tools-list", `server advertises ${list.tools.length} tools`);
});

function extractText(r: { content?: Array<{ type: string; text?: string }> }): string {
  return r.content?.find((c) => c.type === "text")?.text ?? "";
}

function parseJson<T>(r: { content?: Array<{ type: string; text?: string }> }): T {
  return JSON.parse(extractText(r)) as T;
}

let mcpAgentPromise: Promise<string> | undefined;
function mcpAgent(): Promise<string> {
  mcpAgentPromise ??= freshAgent("mcpct");
  return mcpAgentPromise;
}

test("mcp-contacts: create/get/update/list/delete_contact lifecycle", async () => {
  const address = `${uniqueSlug("mcpct1")}@example.com`;

  const created = await callTool(mcp, "create_contact", { address, display_name: "MCP Contact" });
  assert.equal(created.isError, undefined, `create_contact isError: ${extractText(created).slice(0, 200)}`);
  const createdView = parseJson<ContactView>(created);
  assert.equal(createdView.address, address, "create_contact echoes the canonical address");
  trackedContacts.add(address);

  const got = await callTool(mcp, "get_contact", { address });
  assert.equal(got.isError, undefined, `get_contact isError: ${extractText(got).slice(0, 200)}`);
  const gotView = parseJson<ContactView & { etag?: string }>(got);
  assert.equal(gotView.address, address);
  assert.equal(gotView.display_name, "MCP Contact");
  assert.ok(gotView.etag && ETAG_RE.test(gotView.etag), `get_contact surfaces the ETag, got ${JSON.stringify(gotView.etag)}`);

  const renamed = await callTool(mcp, "update_contact", { address, display_name: "MCP Renamed", if_match: gotView.etag });
  assert.equal(renamed.isError, undefined, `update_contact isError: ${extractText(renamed).slice(0, 200)}`);
  assert.equal(parseJson<ContactView>(renamed).display_name, "MCP Renamed", "update_contact applies the new name");

  const listed = await callTool(mcp, "list_contacts", { source: "manual", limit: 100 });
  assert.equal(listed.isError, undefined, `list_contacts isError: ${extractText(listed).slice(0, 200)}`);
  const page = parseJson<{ contacts?: ContactView[] }>(listed);
  assert.ok(page.contacts?.some((c) => c.address === address), "list_contacts includes the created contact");

  const deleted = await callTool(mcp, "delete_contact", { address });
  assert.equal(deleted.isError, undefined, `delete_contact isError: ${extractText(deleted).slice(0, 200)}`);
  assert.deepEqual(parseJson<{ deleted?: boolean; address?: string }>(deleted), { deleted: true, address }, "delete_contact result shape");
  trackedContacts.delete(address);

  const gone = await client.get(`/v1/contacts/${encodeURIComponent(address)}`);
  assert.equal(gone.status, 404, "the contact is gone via REST after the MCP delete");
});

test("mcp-contacts: import_contacts then delete_contact_import reverses it", async () => {
  const a1 = `${uniqueSlug("mcpim1")}@example.com`;
  const a2 = `${uniqueSlug("mcpim2")}@example.com`;

  const imported = await callTool(mcp, "import_contacts", {
    contacts: [{ address: a1, display_name: "MCP Import One" }, { address: a2 }],
  });
  assert.equal(imported.isError, undefined, `import_contacts isError: ${extractText(imported).slice(0, 200)}`);
  const result = parseJson<ContactImportResult>(imported);
  assert.ok(result.batch_id, "import_contacts returns a batch_id");
  assert.equal(result.created, 2, "both rows created");
  trackedBatches.add(result.batch_id);
  trackedContacts.add(a1);
  trackedContacts.add(a2);

  const reversed = await callTool(mcp, "delete_contact_import", { batch_id: result.batch_id });
  assert.equal(reversed.isError, undefined, `delete_contact_import isError: ${extractText(reversed).slice(0, 200)}`);
  const receipt = parseJson<DeleteImportBatchResult>(reversed);
  assert.equal(receipt.deleted, true);
  assert.equal(receipt.contacts_deleted, 2, "both untouched batch contacts removed");
  trackedBatches.delete(result.batch_id);
  trackedContacts.delete(a1);
  trackedContacts.delete(a2);
});

test("mcp-contacts: set/get/list/delete_outreach_contact lifecycle", async () => {
  const agent = await mcpAgent();
  const address = `${uniqueSlug("mcpout")}@example.com`;
  const future = new Date(Date.now() + 3_600_000).toISOString();

  const enrolled = await callTool(mcp, "set_outreach_contact", {
    email: agent,
    address,
    stage: "prospect",
    next_action_at: future,
  });
  assert.equal(enrolled.isError, undefined, `set_outreach_contact isError: ${extractText(enrolled).slice(0, 200)}`);
  const view = parseJson<ContactEngagementView>(enrolled);
  assert.equal(view.agent_email, agent);
  assert.equal(view.address, address);
  assert.equal(view.stage, "prospect");
  trackEngagement(agent, address);

  const got = await callTool(mcp, "get_outreach_contact", { email: agent, address });
  assert.equal(got.isError, undefined, `get_outreach_contact isError: ${extractText(got).slice(0, 200)}`);
  const gotView = parseJson<ContactEngagementView & { etag?: string }>(got);
  assert.equal(gotView.stage, "prospect");
  assert.ok(gotView.etag && ETAG_RE.test(gotView.etag), `get_outreach_contact surfaces the ETag, got ${JSON.stringify(gotView.etag)}`);
  assert.equal(gotView.contact?.address, address, "the outreach record embeds the contact");

  const listed = await callTool(mcp, "list_outreach_contacts", { email: agent, stage: "prospect" });
  assert.equal(listed.isError, undefined, `list_outreach_contacts isError: ${extractText(listed).slice(0, 200)}`);
  const page = parseJson<{ contacts?: ContactEngagementView[] }>(listed);
  assert.ok(page.contacts?.some((e) => e.address === address), "list_outreach_contacts includes the enrolment");

  const removed = await callTool(mcp, "delete_outreach_contact", { email: agent, address });
  assert.equal(removed.isError, undefined, `delete_outreach_contact isError: ${extractText(removed).slice(0, 200)}`);
  assert.deepEqual(parseJson<{ deleted?: boolean; address?: string }>(removed), { deleted: true, address }, "delete_outreach_contact result shape");
  untrackEngagement(agent, address);

  const gone = await client.get(`/v1/agents/${encodeURIComponent(agent)}/contacts/${encodeURIComponent(address)}`);
  assert.equal(gone.status, 404, "the engagement is gone via REST after the MCP delete");
});

// ---------------------------------------------------------------------------
// Section 3 — contact.due emission (dual-assertion, mirroring
// 21-webhook-events). Capability probe identical to that suite's: skip only on
// a genuine events_log_disabled, never on a connectivity failure.
// ---------------------------------------------------------------------------

let skip: string | false = false;
try {
  const eventsProbe = await client.get("/v1/events", { query: { limit: 1 } });
  if (isEventsLogDisabled(eventsProbe.status, eventsProbe.body)) {
    skip = "event-log capability disabled on this target (events_log_disabled)";
  }
} catch {
  // Probe couldn't reach the target — do NOT skip; let the test surface the
  // real connectivity error rather than masking an outage as a clean skip.
}

// pollEvent mirrors 21-webhook-events' helper, with two parameterizable
// knobs the 5-minute contactdue sweep cadence needs: a long overall budget
// and a slower backoff cap (polling every 3s for 8 minutes would just be
// rate-limit noise).
async function pollEvent(
  params: { type: string; agentId: string; since: string },
  match: (e: EventView) => boolean,
  timeoutMs = 15000,
  maxDelayMs = 3000,
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
    delay = Math.min(Math.floor(delay * 1.5), maxDelayMs);
  }
  return null;
}

// pollEventFanout: EVENT-scoped proof THIS event fanned out to >=1 subscriber.
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

// pollDelivery: WEBHOOK-scoped proof OUR webhook's HTTP delivery leg ran
// (attempts>=1 — the example.com sink 405s the POST, which still proves the
// leg fired; delivery SUCCESS would test the sink, not e2a).
async function pollDelivery(webhookId: string, eventType: string, timeoutMs = 15000): Promise<WebhookDeliveryView | null> {
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

test("emit: contact.due — a past-due engagement emits the event and attempts a delivery", { skip }, async () => {
  const agent = await freshAgent("cdue");
  const address = `${uniqueSlug("cdue")}@example.com`;
  // Dummy HTTPS target: passes the create-time HTTPS/SSRF guard, then 405s the
  // POST at delivery time — proving the delivery ATTEMPT without a real sink.
  // filters.agent_emails scopes the subscription to exactly our fresh agent.
  const hookRes = await client.post<CreateWebhookResponse>("/v1/webhooks", {
    body: {
      url: "https://example.com/e2e-contacts-outreach",
      events: ["contact.due"],
      description: `e2e ${uniqueSlug("whcd")}`,
      filters: { agent_emails: [agent] },
    },
  });
  assert.equal(hookRes.status, 201, `create webhook expected 201, got ${hookRes.status}: ${hookRes.raw.slice(0, 200)}`);
  const hook = hookRes.body!;
  trackedHooks.add(hook.id);
  const since = sinceNow();
  try {
    // Arm the wake-up: next_action_at in the PAST, so the very next sweep
    // (5-minute River periodic, RunOnStart) claims it.
    const enrol = await client.put<ContactEngagementView>(
      `/v1/agents/${encodeURIComponent(agent)}/contacts/${encodeURIComponent(address)}`,
      { body: { stage: "follow-up", next_action_at: new Date(Date.now() - 10_000).toISOString() } },
    );
    assert.equal(enrol.status, 201, `upsertEngagement expected 201, got ${enrol.status}: ${enrol.raw.slice(0, 200)}`);
    trackEngagement(agent, address);

    // The sweep claims next_action_at <= now in batches of 200 and emits one
    // contact.due per engagement; budget ~8 minutes to straddle two intervals
    // (plus fan-out) without being so tight that a just-missed sweep flakes.
    const ev = await pollEvent(
      { type: "contact.due", agentId: agent, since },
      (e) => e.data.address === address,
      8 * 60 * 1000,
      20000,
    );
    assert.ok(ev, `contact.due event for ${address} must appear in listEvents within ~8 minutes (two sweep intervals)`);
    assert.ok(ev!.id.startsWith("evt_"), `event id has evt_ prefix: ${ev!.id}`);
    assert.equal(ev!.type, "contact.due");
    assert.equal(ev!.agent_email, agent, "event.agent_email is the enrolled agent");
    assert.equal(ev!.data.agent_email, agent, "payload carries the agent");
    assert.equal(ev!.data.stage, "follow-up", "payload carries the engagement stage");

    // Event-scoped: THIS event fanned out to >=1 subscriber.
    const fanout = await pollEventFanout(ev!.id, 30000);
    assert.ok(fanout, `event ${ev!.id} must fan out (matched_webhooks>=1) within 30s`);
    // Webhook-scoped: OUR fresh webhook's delivery leg actually RAN.
    const del = await pollDelivery(hook.id, "contact.due", 30000);
    assert.ok(del, `a delivery ATTEMPT for contact.due must appear on webhook ${hook.id}`);
    assert.ok(del!.attempts >= 1, `delivery attempted (attempts=${del!.attempts})`);
    info(SUITE, "contact.due", `emitted evt=${ev!.id} fanned to ${fanout!.matched_webhooks} webhook(s); our webhook whd=${del!.id} attempts=${del!.attempts} last_status=${del!.last_status_code}`);
    verifiedEventTypes.add("contact.due");
  } finally {
    // Each delete is individually guarded and the resource is untracked only
    // AFTER its delete succeeds: a throw here must neither skip the remaining
    // deletes nor strand the resource for the `after` fallback (whose
    // 404-tolerance makes a double-delete harmless).
    try {
      await client.delete(`/v1/webhooks/${encodeURIComponent(hook.id)}?confirm=DELETE`);
      trackedHooks.delete(hook.id);
    } catch { /* after() retries via trackedHooks */ }
    try {
      await client.delete(`/v1/agents/${encodeURIComponent(agent)}/contacts/${encodeURIComponent(address)}?confirm=DELETE`);
      untrackEngagement(agent, address);
    } catch { /* after() retries via trackedEngagements */ }
    try {
      await client.delete(`/v1/contacts/${encodeURIComponent(address)}?confirm=DELETE`);
      trackedContacts.delete(address);
    } catch { /* after() retries via trackedContacts */ }
  }
});

// ---------------------------------------------------------------------------
// Teardown: best-effort deletion of every survivor, each delete wrapped so one
// failure can't abort the rest (404/403 = already gone = success). Runs even
// when tests abort mid-section. Then the shared agent cleanup, the suite
// report, and the event-coverage shard (only verified types — recorded after
// BOTH halves of the dual assertion passed).
// ---------------------------------------------------------------------------
after(async () => {
  await mcp.stop();
  const failures: string[] = [];
  for (const key of [...trackedEngagements]) {
    const [agent, address] = key.split("\n");
    try {
      const r = await client.delete(
        `/v1/agents/${encodeURIComponent(agent)}/contacts/${encodeURIComponent(address)}?confirm=DELETE`,
      );
      if (![200, 404, 403].includes(r.status)) failures.push(`engagement ${agent}/${address}: HTTP ${r.status}`);
    } catch (e) {
      failures.push(`engagement ${agent}/${address}: ${(e as Error).message}`);
    }
  }
  for (const batch of [...trackedBatches]) {
    try {
      const r = await client.delete(`/v1/contacts/imports/${encodeURIComponent(batch)}?confirm=DELETE`);
      if (![200, 404, 403].includes(r.status)) failures.push(`import batch ${batch}: HTTP ${r.status}`);
    } catch (e) {
      failures.push(`import batch ${batch}: ${(e as Error).message}`);
    }
  }
  for (const address of [...trackedContacts]) {
    try {
      const r = await client.delete(`/v1/contacts/${encodeURIComponent(address)}?confirm=DELETE`);
      if (![200, 404, 403].includes(r.status)) failures.push(`contact ${address}: HTTP ${r.status}`);
    } catch (e) {
      failures.push(`contact ${address}: ${(e as Error).message}`);
    }
  }
  for (const id of [...trackedHooks]) {
    try {
      const r = await client.delete(`/v1/webhooks/${encodeURIComponent(id)}?confirm=DELETE`);
      if (![200, 404, 403].includes(r.status)) failures.push(`webhook ${id}: HTTP ${r.status}`);
    } catch (e) {
      failures.push(`webhook ${id}: ${(e as Error).message}`);
    }
  }
  if (failures.length) warn(SUITE, "cleanup", `${failures.length} survivor delete(s) failed`, failures);
  const r = await cleanup(client);
  if (r.failed.length) warn(SUITE, "cleanup", `shared agent cleanup failed ${r.failed.length}`, r.failed);
  await writeReport(`./reports/${SUITE}.json`);
  if (verifiedEventTypes.size > 0) {
    mkdirSync(EVENT_COVERAGE_DIR, { recursive: true });
    writeFileSync(`${EVENT_COVERAGE_DIR}${process.pid}.json`, JSON.stringify([...verifiedEventTypes]));
  }
});
