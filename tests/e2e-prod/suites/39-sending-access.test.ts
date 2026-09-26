import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { info, writeReport } from "../harness/report.ts";

// Black-box conformance for the external-sending-access request intake
// (beta): createSendingAccessRequest + getSendingAccessRequest.
//
// SAFETY / IDEMPOTENCE: filing a request never grants access, and while one is
// pending a resubmit returns the SAME pending request (200) without creating
// another. There is no customer delete; reruns converge on the single pending
// request of the conformance account (the first run gets 201, every later run
// 200), so the per-account request cap (3 per 30 days) is never approached.
//
// A deployment that does not enable the control answers 501 not_implemented;
// that is recorded as info and the checks are skipped (the operation then
// stays uncovered and the coverage gate reports it — which is correct).
const SUITE = "39-sending-access";
const client = new ApiClient();

interface SendingAccessRequestView {
  id: string;
  state: string;
  use_case: string;
  recipients: string;
  expected_daily_volume: number;
  created_at: string;
  decided_at?: string;
}

const form = {
  use_case: "conformance suite probe: verifies the request intake contract",
  recipients: "no recipients; this request exists only for automated conformance",
  expected_daily_volume: 1,
};

test("createSendingAccessRequest → getSendingAccessRequest (idempotent pending request)", async (t) => {
  const create = await client.post<SendingAccessRequestView>("/v1/account/sending-access/request", { body: form });
  if (create.status === 501) {
    info(SUITE, "createSendingAccessRequest", "501 not_implemented: external sending access is not enabled on this deployment");
    t.skip("external sending access disabled on this deployment");
    return;
  }
  assert.ok(create.status === 201 || create.status === 200,
    `createSendingAccessRequest expected 201 (new) or 200 (existing pending), got ${create.status}: ${create.raw.slice(0, 200)}`);
  const filed = create.body!;
  assert.ok(filed.id, "request id present");
  assert.equal(typeof filed.state, "string", "state is a string (open set)");
  assert.equal(typeof filed.expected_daily_volume, "number", "expected_daily_volume is a number");
  assert.ok(!Number.isNaN(Date.parse(filed.created_at)), `created_at is a timestamp: ${filed.created_at}`);

  // A second submission while the first is pending returns the SAME request
  // with 200, never a second one. (If support already decided it, a new one
  // is filed as an appeal — still a 2xx.)
  const again = await client.post<SendingAccessRequestView>("/v1/account/sending-access/request", { body: form });
  assert.ok(again.status === 200 || again.status === 201, `resubmit expected 2xx, got ${again.status}: ${again.raw.slice(0, 200)}`);
  if (filed.state === "pending") {
    assert.equal(again.status, 200, "resubmit while pending answers 200");
    assert.equal(again.body!.id, filed.id, "resubmit returns the existing pending request");
  }

  const latest = await client.get<SendingAccessRequestView>("/v1/account/sending-access/request");
  assert.equal(latest.status, 200, `getSendingAccessRequest expected 200, got ${latest.status}: ${latest.raw.slice(0, 200)}`);
  assert.equal(latest.body!.id, again.body!.id, "latest request is the one just returned");
});

test("sending-access request: invalid volume is a 422 invalid_request", async () => {
  const r = await client.post<{ error?: { code?: string } }>("/v1/account/sending-access/request", {
    body: { ...form, expected_daily_volume: 0 },
  });
  if (r.status === 501) return;
  assert.equal(r.status, 422, `expected 422, got ${r.status}: ${r.raw.slice(0, 200)}`);
  assert.equal(r.body?.error?.code, "invalid_request");
});

after(async () => {
  await writeReport(`./reports/${SUITE}.json`);
});
