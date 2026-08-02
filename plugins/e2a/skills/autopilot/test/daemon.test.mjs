import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { JobSpool } from "../spool.mjs";
import {
  AutopilotDaemon,
  buildCliEnvironment,
  buildListenerArgs,
  buildReconcileArgs,
  formatListenerStart,
  parseReconcileOutput,
  reconcileSummaries,
} from "../daemon.mjs";

const policy = {
  mailbox: { agentEmail: "support@example.test" },
  inbound: {
    mode: "addresses",
    addresses: ["customer@buyer.test"],
    domains: [],
    fallback: "review",
  },
};

test("listener command uses the existing CLI and its human log redacts the forward capability", () => {
  const args = buildListenerArgs({
    baseArgs: ["/opt/e2a/dist/bin/e2a.js"],
    agentEmail: "support@example.test",
    port: 8123,
    forwardToken: "synthetic_forward_capability",
  });

  assert.deepEqual(args, [
    "/opt/e2a/dist/bin/e2a.js",
    "listen",
    "--agent",
    "support@example.test",
    "--forward",
    "http://127.0.0.1:8123/hook",
    "--forward-token",
    "synthetic_forward_capability",
  ]);
  const display = formatListenerStart("/usr/local/bin/node", args);
  assert.match(display, /--forward-token \[redacted\]/);
  assert.doesNotMatch(display, /synthetic_forward_capability/);
});

test("CLI child environment excludes unrelated credentials", () => {
  const env = buildCliEnvironment(
    {
      PATH: "/usr/bin:/bin",
      HOME: "/home/operator",
      AWS_SECRET_ACCESS_KEY: "no",
      GITHUB_TOKEN: "no",
      ANTHROPIC_API_KEY: "no",
    },
    {
      apiKey: "e2a_agt_synthetic",
      agentEmail: "support@example.test",
      deploymentUrl: "https://e2a.example.test",
    },
  );

  assert.deepEqual(env, {
    PATH: "/usr/bin:/bin",
    HOME: "/home/operator",
    E2A_API_KEY: "e2a_agt_synthetic",
    E2A_AGENT_EMAIL: "support@example.test",
    E2A_URL: "https://e2a.example.test",
  });
});

test("reconcile command uses the existing unread inbound list", () => {
  assert.deepEqual(
    buildReconcileArgs({
      baseArgs: [],
      agentEmail: "support@example.test",
    }),
    [
      "messages",
      "list",
      "--agent",
      "support@example.test",
      "--direction",
      "inbound",
      "--read-status",
      "unread",
      "--json",
    ],
  );
});

test("reconcile output is all-or-nothing valid NDJSON", () => {
  const parsed = parseReconcileOutput(
    '{"id":"msg_1","headerFrom":"customer@buyer.test","verifiedDomain":"buyer.test"}\n' +
      '{"id":"msg_2","header_from":"other@buyer.test","verified_domain":"buyer.test"}\n',
  );
  assert.equal(parsed.length, 2);
  assert.throws(
    () => parseReconcileOutput('{"id":"msg_1"}\nnot-json\n'),
    /invalid NDJSON/i,
  );
});

test("reconciliation enqueues only messages allowed by the confirmed policy", () => {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-reconcile-"));
  const spool = new JobSpool(path.join(root, "jobs"));
  const result = reconcileSummaries({
    policy,
    spool,
    messages: [
      {
        id: "msg_allowed",
        headerFrom: "customer@buyer.test",
        verifiedDomain: "buyer.test",
      },
      {
        id: "msg_review",
        headerFrom: "other@buyer.test",
        verifiedDomain: "buyer.test",
      },
    ],
  });

  assert.deepEqual(result, { checked: 2, enqueued: 1, refused: 1, deduplicated: 0 });
  assert.equal(spool.list("pending")[0].messageId, "msg_allowed");
});

test("daemon schedules retry jobs for their durable availability time", async () => {
  let calls = 0;
  const daemon = new AutopilotDaemon({
    policy,
    secrets: {
      apiKey: "e2a_agt_synthetic",
      forwardToken: "synthetic",
      deploymentUrl: "https://e2a.example.test",
    },
    spool: { recoverRunning() {}, promoteReadyRetries() {} },
    supervisor: {
      async runNextJob() {
        calls += 1;
        if (calls === 1) {
          return {
            state: "retry",
            job: { messageId: "msg_retry", availableAt: Date.now() + 20 },
          };
        }
        return { state: "idle" };
      },
    },
    cli: { command: "/usr/bin/false", baseArgs: [] },
  });

  daemon.requestDrain();
  await new Promise((resolve) => setTimeout(resolve, 60));
  await daemon.stop();

  assert.ok(calls >= 3, `expected a scheduled retry drain, observed ${calls} calls`);
});
