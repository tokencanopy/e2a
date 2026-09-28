import { test, after } from "node:test";
import assert from "node:assert/strict";
import { ApiClient } from "../harness/client.ts";
import { HttpMcpClient, callTool, type McpToolResult } from "../harness/mcp.ts";
import { info, writeReport } from "../harness/report.ts";

// Black-box MCP conformance for the external-sending-access request tools
// (beta) against the DEPLOYED streamable-HTTP /mcp server — the MCP analogue
// of suite 39 (REST), so mcp_coverage_gate.py credits both tools.
//
// SAFETY / IDEMPOTENCE: the conformance account is an internal-class account,
// which the rule never binds, so the server sends NO operator notification for
// its requests. Filing never grants access, and while a request is pending a
// resubmit returns the same request — reruns converge on one pending request
// and never approach the 3-per-30-days cap.
//
// A deployment that does not enable the control answers 501 not_implemented
// (isError); that is recorded as info and the checks are skipped — the tools
// then stay uncovered and the coverage gate reports it, which is correct.
const SUITE = "40-mcp-sending-access";
const apiClient = new ApiClient();
const mcp = new HttpMcpClient(apiClient.env.mcpUrl, apiClient.env.apiKey);

interface SendingAccessRequestView {
  id: string;
  state: string;
  use_case: string;
  recipients: string;
  expected_daily_volume: number;
  created_at: string;
}

const form = {
  use_case: "conformance suite probe: verifies the request intake contract",
  recipients: "no recipients; this request exists only for automated conformance",
  expected_daily_volume: 1,
};

function text(r: McpToolResult): string {
  return r.content?.find((c) => c.type === "text")?.text ?? "";
}

after(async () => {
  await mcp.stop();
  writeReport(`./reports/${SUITE}.json`);
});

test("mcp-sending-access: tools/list advertises both tools, request is marked mutating", async () => {
  const list = await mcp.call<{ tools: Array<{ name: string; _meta?: Record<string, unknown>; annotations?: { readOnlyHint?: boolean } }> }>("tools/list");
  const req = list.tools.find((t) => t.name === "request_sending_access");
  const get = list.tools.find((t) => t.name === "get_sending_access_request");
  assert.ok(req, "request_sending_access is advertised");
  assert.ok(get, "get_sending_access_request is advertised");
  assert.equal(req!._meta?.["e2a/mutating"], true, "request_sending_access is mutating");
  assert.equal(get!._meta?.["e2a/mutating"], false, "get_sending_access_request is a read");
  assert.equal(get!.annotations?.readOnlyHint, true, "get_sending_access_request is readOnlyHint");
});

test("mcp-sending-access: request_sending_access → get_sending_access_request", async (t) => {
  const filed = await callTool(mcp, "request_sending_access", form);
  if (filed.isError && /not_implemented/.test(text(filed))) {
    info(SUITE, "request_sending_access", "501 not_implemented: external sending access is not enabled on this deployment");
    t.skip("external sending access disabled on this deployment");
    return;
  }
  assert.equal(filed.isError, undefined, `request_sending_access isError: ${text(filed).slice(0, 300)}`);
  const view = JSON.parse(text(filed)) as SendingAccessRequestView;
  assert.ok(view.id, "request id present");
  assert.equal(typeof view.state, "string", "state is a string (open set)");
  assert.equal(typeof view.expected_daily_volume, "number", "expected_daily_volume is snake_case on the MCP wire");

  const latest = await callTool(mcp, "get_sending_access_request", {});
  assert.equal(latest.isError, undefined, `get_sending_access_request isError: ${text(latest).slice(0, 300)}`);
  const got = JSON.parse(text(latest)) as SendingAccessRequestView;
  assert.equal(got.id, view.id, "latest request is the one request_sending_access returned");
});
