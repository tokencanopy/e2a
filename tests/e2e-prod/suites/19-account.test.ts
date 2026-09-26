import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { isProductionTarget } from "../harness/env.ts";
import { info, writeReport } from "../harness/report.ts";

// Black-box conformance for the /v1/account surface (account ops in the
// drift-gated api/openapi.yaml SSOT): getAccount, listApiKeys, createApiKey,
// deleteApiKey, exportAccount, listSuppressions, deleteSuppression, and
// deleteAccount against a pipeline-minted disposable account.
//
// SAFETY (see the deleteAccount test + the createApiKey test):
//  - deleteAccount runs ONLY with E2A_DISPOSABLE_API_KEY, a throwaway account
//    the release pipeline mints per run, and only after proving that key is a
//    different account from the conformance account (E2A_API_KEY). It always
//    passes permanent=true, so no throwaway account is left in the trash.
//  - createApiKey mints a NEW key; only that new key is ever deleted. The
//    authenticating key (E2A_API_KEY) is never touched.
const SUITE = "19-account";
const client = new ApiClient();

interface AccountView {
  user: { id: string; email: string };
  scope: "account" | "agent";
  plan_code: string;
  agent_email?: string;
  upgrade_url: string;
  limits: { max_agents: number; max_domains: number; max_messages_month: number; max_storage_bytes: number };
  usage: { agents: number; domains: number; messages_month: number; storage_bytes: number };
}
interface ApiKeyView {
  id: string;
  name: string;
  key_prefix: string;
  scope: "account" | "agent";
  created_at: string;
  agent?: string;
  last_used_at?: string;
  expires_at?: string;
}
interface PageApiKeyView {
  items: ApiKeyView[];
  next_cursor: string | null;
}
interface CreateApiKeyResponse extends ApiKeyView {
  key: string;
}
interface SuppressionView {
  address: string;
  source: string;
  created_at: string;
  reason?: string;
  source_message_id?: string;
}
interface PageSuppressionView {
  items: SuppressionView[];
  next_cursor: string | null;
}
interface ErrorEnvelope {
  error?: { code?: string; message?: string; request_id?: string };
}

// ---------------------------------------------------------------------------
// getAccount — AccountView (whoami): identity + plan caps + usage.
// ---------------------------------------------------------------------------
test("getAccount: returns AccountView (user, scope, plan_code, limits, usage, upgrade_url)", async () => {
  const r = await client.get<AccountView>("/v1/account");
  assert.equal(r.status, 200, `getAccount expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
  const b = r.body!;
  // user{id,email} required
  assert.ok(b.user?.id, "user.id present");
  assert.ok(b.user?.email?.includes("@"), `user.email valid: ${b.user?.email}`);
  // scope enum
  assert.ok(b.scope === "account" || b.scope === "agent", `scope in {account,agent}: ${b.scope}`);
  assert.equal(typeof b.plan_code, "string", "plan_code is a string");
  assert.ok(b.plan_code.length > 0, "plan_code non-empty");
  assert.equal(typeof b.upgrade_url, "string", "upgrade_url present (may be empty)");
  // limits: all four max_* caps required + integer
  for (const k of ["max_agents", "max_domains", "max_messages_month", "max_storage_bytes"] as const) {
    assert.equal(typeof b.limits?.[k], "number", `limits.${k} is a number`);
    assert.ok(Number.isInteger(b.limits[k]), `limits.${k} is an integer`);
  }
  // usage: all four counters required + integer + non-negative
  for (const k of ["agents", "domains", "messages_month", "storage_bytes"] as const) {
    assert.equal(typeof b.usage?.[k], "number", `usage.${k} is a number`);
    assert.ok(b.usage[k] >= 0, `usage.${k} non-negative`);
  }
});

// ---------------------------------------------------------------------------
// listApiKeys — PageAPIKeyView envelope + the authenticating key is present.
// ---------------------------------------------------------------------------
test("listApiKeys: PageAPIKeyView envelope; the authenticating key is present", async () => {
  const r = await client.get<PageApiKeyView>("/v1/account/api-keys");
  assert.equal(r.status, 200, `listApiKeys expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.ok(Array.isArray(r.body?.items), "items is an array");
  assert.ok(
    r.body!.next_cursor === null || typeof r.body!.next_cursor === "string",
    "next_cursor is string|null (required by PageAPIKeyView)",
  );
  for (const k of r.body!.items) {
    // APIKeyView required: id, name, key_prefix, scope, created_at — and NO secret.
    assert.ok(k.id, "key.id present");
    assert.equal(typeof k.name, "string", "key.name is a string");
    assert.ok(k.key_prefix, "key.key_prefix present");
    assert.ok(k.scope === "account" || k.scope === "agent", `key.scope in {account,agent}: ${k.scope}`);
    assert.ok(k.created_at, "key.created_at present");
    assert.ok(!("key" in (k as object)), "list must NOT leak the secret key (metadata only)");
  }
  // The authenticating key must appear in its own list. key_prefix is a
  // non-secret leading slice of the full secret (e.g. e2a_acct_<hex7>), so the
  // env key must start with exactly one listed key's prefix.
  const mine = r.body!.items.find((k) => client.env.apiKey.startsWith(k.key_prefix));
  assert.ok(mine, "the E2A_API_KEY the suite authenticates with is present in listApiKeys");
});

// ---------------------------------------------------------------------------
// createApiKey → plaintext once → appears in list → deleteApiKey (NEW key
// only) → gone. NEVER deletes the authenticating key.
// ---------------------------------------------------------------------------
test("createApiKey → list shows it → deleteApiKey (new key only) → gone", async () => {
  const name = `conf-19-${Date.now()}`;
  const create = await client.post<CreateApiKeyResponse>("/v1/account/api-keys", {
    body: { name, scope: "account" },
  });
  assert.equal(create.status, 201, `createApiKey expected 201, got ${create.status}: ${create.raw.slice(0, 200)}`);
  const created = create.body!;
  // CreateAPIKeyResponse required: key (plaintext, once), id, name, key_prefix, scope, created_at.
  assert.ok(created.key, "plaintext key returned exactly once at creation");
  assert.ok(created.key.startsWith(created.key_prefix), "returned key begins with its key_prefix");
  assert.ok(created.id, "created.id present");
  assert.equal(created.name, name, "created.name echoes the request");
  assert.equal(created.scope, "account", "created.scope is account");

  // Guard: we must never delete the key we authenticate with.
  assert.notEqual(created.key, client.env.apiKey, "new key is distinct from the auth key");

  let deleted = false;
  try {
    // Present in the list now.
    const list = await client.get<PageApiKeyView>("/v1/account/api-keys");
    assert.equal(list.status, 200);
    assert.ok(list.body!.items.some((k) => k.id === created.id), "new key appears in listApiKeys");

    // Delete the NEW key (never the auth key).
    assert.ok(
      !client.env.apiKey.startsWith(created.key_prefix),
      "sanity: new key_prefix does not match the auth key before delete",
    );
    const del = await client.delete<{ deleted: boolean; id: string }>(
      `/v1/account/api-keys/${encodeURIComponent(created.id)}?confirm=DELETE`,
    );
    assert.equal(del.status, 200, `deleteApiKey expected 200 + deletion object, got ${del.status}: ${del.raw.slice(0, 200)}`);
    assert.equal(del.body?.deleted, true, "deletion object has deleted:true");
    assert.equal(del.body?.id, created.id, "deletion object echoes the key id");
    deleted = true;

    // Gone from the list.
    const after = await client.get<PageApiKeyView>("/v1/account/api-keys");
    assert.equal(after.status, 200);
    assert.ok(!after.body!.items.some((k) => k.id === created.id), "deleted key is gone from listApiKeys");
  } finally {
    // Self-clean: if an assertion aborted before the delete, revoke the throwaway.
    if (!deleted) await client.delete(`/v1/account/api-keys/${encodeURIComponent(created.id)}?confirm=DELETE`);
  }
});

test("deleteApiKey: unknown id returns 404 not_found", async () => {
  const r = await client.delete<ErrorEnvelope>("/v1/account/api-keys/apk_conf19doesnotexist000000000000?confirm=DELETE");
  assert.equal(r.status, 404, `unknown key delete → 404, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.equal(r.body?.error?.code, "not_found", "error envelope code=not_found");
});

// ---------------------------------------------------------------------------
// exportAccount — UserExport (GDPR right-of-access).
// ---------------------------------------------------------------------------
test("exportAccount: 200 UserExport with required sections + attachment header", async () => {
  const r = await client.get<Record<string, unknown>>("/v1/account/export");
  assert.equal(r.status, 200, `exportAccount expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
  const b = r.body!;
  // UserExport required: generated_at, schema_version, user, domains, agents,
  // api_keys, messages, suppressions, protection_events.
  for (const k of [
    "generated_at",
    "schema_version",
    "user",
    "domains",
    "agents",
    "api_keys",
    "messages",
    "suppressions",
    "protection_events",
  ]) {
    assert.ok(k in b, `export has required section '${k}'`);
  }
  // user is UserExportUser{id,email,name,created_at}.
  const user = b.user as { id?: string; email?: string; name?: string; created_at?: string };
  assert.ok(user?.id && user?.email, "export.user has id + email");
  // Array-typed sections must be arrays.
  for (const k of ["domains", "agents", "api_keys", "messages", "suppressions", "protection_events"]) {
    assert.ok(Array.isArray(b[k]), `export.${k} is an array`);
  }
  // Exported api_keys are metadata only — never the plaintext secret.
  for (const k of b.api_keys as Array<Record<string, unknown>>) {
    assert.ok(!("key" in k), "export.api_keys entries carry no plaintext secret");
  }
  // Downloadable dump: Content-Disposition attachment (spec documents the header).
  const cd = r.headers["content-disposition"];
  assert.ok(cd && /attachment/i.test(cd), `export sets Content-Disposition: attachment (got: ${cd ?? "absent"})`);
});

// ---------------------------------------------------------------------------
// listSuppressions — PageSuppressionView envelope (typically empty for a fresh acct).
// ---------------------------------------------------------------------------
test("listSuppressions: PageSuppressionView envelope {items, next_cursor}", async () => {
  const r = await client.get<PageSuppressionView>("/v1/account/suppressions", { query: { limit: 100 } });
  assert.equal(r.status, 200, `listSuppressions expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.ok(Array.isArray(r.body?.items), "items is an array");
  assert.ok(
    r.body!.next_cursor === null || typeof r.body!.next_cursor === "string",
    "next_cursor is string|null (required by PageSuppressionView)",
  );
  for (const s of r.body!.items) {
    // SuppressionView required: address, source, created_at.
    assert.ok(s.address?.includes("@"), `suppression.address valid: ${s.address}`);
    assert.ok(s.source, "suppression.source present");
    assert.ok(s.created_at, "suppression.created_at present");
  }
  if (r.body!.items.length === 0) {
    info(SUITE, "listSuppressions", "suppression list is empty (expected for the conformance account)");
  }
});

test("deleteSuppression: unknown address returns 404 not_found", async () => {
  const addr = `conf19-nonexistent-${Date.now()}@example.com`;
  const r = await client.delete<ErrorEnvelope>(`/v1/account/suppressions/${encodeURIComponent(addr)}?confirm=DELETE`);
  assert.equal(r.status, 404, `un-suppressing an unknown address → 404, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.equal(r.body?.error?.code, "not_found", "error envelope code=not_found");
});

// ---------------------------------------------------------------------------
// Unauthenticated access — every account op must reject with 401.
// ---------------------------------------------------------------------------
test("unauth: every account op rejects an unauthenticated caller with 401", async () => {
  // apiKey: null strips the Authorization header entirely.
  const noAuth = { apiKey: null as null };

  const cases: Array<{ label: string; run: () => Promise<{ status: number; raw: string }> }> = [
    { label: "GET /v1/account", run: () => client.get("/v1/account", noAuth) },
    { label: "GET /v1/account/api-keys", run: () => client.get("/v1/account/api-keys", noAuth) },
    {
      // NOTE: the server parses/validates the request body BEFORE the auth
      // check, so a POST with a MISSING/MALFORMED body 400s (validation) even
      // unauthenticated. To actually exercise the auth gate we send a
      // well-formed body — then the response is a clean 401. (Pinned quirk:
      // body-before-auth ordering; the 400 leaks no data, but the auth-first
      // contract is only observable with a valid body.)
      label: "POST /v1/account/api-keys",
      run: () => client.post("/v1/account/api-keys", { ...noAuth, body: { name: "unauth-probe", scope: "account" } }),
    },
    { label: "GET /v1/account/export", run: () => client.get("/v1/account/export", noAuth) },
    { label: "GET /v1/account/suppressions", run: () => client.get("/v1/account/suppressions", noAuth) },
    {
      // confirm=DELETE clears the required-param validation (which, like the
      // body-before-auth quirk above, runs before the auth gate) so the
      // response is a clean 401 rather than a 422.
      label: "DELETE /v1/account/api-keys/{id}",
      run: () => client.delete("/v1/account/api-keys/apk_unauth?confirm=DELETE", noAuth),
    },
    {
      label: "DELETE /v1/account/suppressions/{address}",
      run: () => client.delete("/v1/account/suppressions/unauth%40example.com?confirm=DELETE", noAuth),
    },
  ];

  for (const c of cases) {
    const r = await c.run();
    assert.equal(r.status, 401, `${c.label} unauthenticated → 401, got ${r.status}: ${r.raw.slice(0, 160)}`);
  }
});

// ---------------------------------------------------------------------------
// deleteAccount — exercised ONLY against a disposable account.
//
// DELETE /v1/account would destroy the account the whole conformance run
// authenticates as, and there is no black-box API to mint an account. The
// release pipeline therefore mints one throwaway account per run (the server's
// `-bootstrap-email` inside the staging container) and threads its
// account-scoped key in as E2A_DISPOSABLE_API_KEY. This test refuses to run
// with that key missing or equal to the conformance account, and always erases
// with permanent=true so the run leaves nothing in the trash. (The default
// trash receipt is covered per PR by the contract scenario
// account_delete_trash_receipt, where the database is disposable.)
// ---------------------------------------------------------------------------
const disposableKey = process.env.E2A_DISPOSABLE_API_KEY?.trim() ?? "";
const disposableSkip = disposableKey
  ? false
  : "E2A_DISPOSABLE_API_KEY is not set — deleteAccount runs only against a throwaway account the release pipeline mints per run " +
    "(-bootstrap-email with an @example.test address); the coverage gate allowlists deleteAccount while the key is absent";
if (disposableSkip) console.log(`[${SUITE}] skipping deleteAccount: ${disposableSkip}`);

interface DeleteAccountReceipt {
  deleted: boolean;
  mode?: string;
  purge_after?: string;
  user_deleted: boolean;
  agents_deleted: number;
}

interface DomainsPage {
  items: unknown[];
}

test("deleteAccount: DELETE /v1/account?permanent=true erases a disposable account and revokes its key", { skip: disposableSkip }, async () => {
  const env = client.env;
  // Defence in depth before a destructive call: the key must not be the
  // conformance key, must resolve to a DIFFERENT account whose email is the
  // synthetic @example.test pattern the pipeline mints, own no domains, and a
  // production target needs its own explicit opt-in.
  assert.notEqual(disposableKey, env.apiKey, "E2A_DISPOSABLE_API_KEY must not be the conformance key");
  if (isProductionTarget(env.apiUrl)) {
    assert.equal(
      process.env.E2E_ALLOW_DISPOSABLE_DELETE_PROD,
      "1",
      "refusing to erase an account on a production origin without E2E_ALLOW_DISPOSABLE_DELETE_PROD=1",
    );
  }
  const disposable = await client.request<AccountView>("GET", "/v1/account", { apiKey: disposableKey });
  assert.equal(disposable.status, 200, `disposable whoami expected 200, got ${disposable.status}: ${disposable.raw.slice(0, 200)}`);
  assert.equal(disposable.body!.scope, "account", "the disposable key must be account-scoped");
  assert.match(disposable.body!.user.email, /@example\.test$/i, "refusing to delete: the disposable account's email is not a synthetic @example.test address");
  const conformance = await client.get<AccountView>("/v1/account");
  assert.equal(conformance.status, 200);
  assert.notEqual(
    disposable.body!.user.id,
    conformance.body!.user.id,
    "refusing to delete: E2A_DISPOSABLE_API_KEY resolves to the conformance account",
  );
  const domains = await client.request<DomainsPage>("GET", "/v1/domains", { apiKey: disposableKey });
  assert.equal(domains.status, 200);
  assert.equal(domains.body!.items.length, 0, "refusing to delete: the disposable account owns domains");

  const r = await client.request<DeleteAccountReceipt>("DELETE", "/v1/account", {
    apiKey: disposableKey,
    query: { confirm: "DELETE", permanent: "true" },
  });
  assert.equal(r.status, 200, `deleteAccount expected 200, got ${r.status}: ${r.raw.slice(0, 200)}`);
  const b = r.body!;
  assert.equal(b.deleted, true, "deleted:true");
  assert.equal(b.mode, "permanent", "permanent=true reports mode permanent");
  assert.equal(b.user_deleted, true, "permanent erasure removes the user row");
  assert.equal(b.purge_after, undefined, "a permanent receipt carries no purge_after");
  info(SUITE, "deleteAccount", "erased the disposable account", { agents_deleted: b.agents_deleted });

  const after = await client.request("GET", "/v1/account", { apiKey: disposableKey });
  assert.equal(after.status, 401, `the erased account's key must stop authenticating, got ${after.status}`);
});

after(async () => {
  await writeReport(`./reports/${SUITE}.json`);
});
