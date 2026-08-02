import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { EventEmitter } from "node:events";

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

test("reconcile command scans all inbound mail from the durable cursor", () => {
  assert.deepEqual(
    buildReconcileArgs({
      baseArgs: [],
      agentEmail: "support@example.test",
      since: "2026-08-01T00:00:00.000Z",
    }),
    [
      "messages",
      "list",
      "--agent",
      "support@example.test",
      "--direction",
      "inbound",
      "--read-status",
      "all",
      "--since",
      "2026-08-01T00:00:00.000Z",
      "--json",
    ],
  );
});

test("successful reconcile advances its cursor after enqueuing a read message", async () => {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-reconcile-cursor-"));
  const cursorPath = path.join(root, "reconcile.json");
  writeFileSync(
    cursorPath,
    `${JSON.stringify({ version: 1, since: "2026-08-01T00:00:00.000Z" })}\n`,
    { mode: 0o600 },
  );
  const spool = new JobSpool(path.join(root, "jobs"));
  const calls = [];
  const daemon = new AutopilotDaemon({
    policy,
    secrets: {
      apiKey: "e2a_agt_synthetic",
      forwardToken: "synthetic",
      deploymentUrl: "https://e2a.example.test",
    },
    spool,
    supervisor: { runNextJob: async () => ({ state: "idle" }) },
    cli: { command: "/synthetic/e2a", baseArgs: [] },
    reconcileStatePath: cursorPath,
    now: () => Date.parse("2026-08-02T01:02:03.000Z"),
    execFileImpl(_command, args, _options, callback) {
      calls.push(args);
      callback(
        null,
        '{"id":"msg_read_after_failed_forward","headerFrom":"customer@buyer.test","verifiedDomain":"buyer.test","readStatus":"read"}\n',
      );
    },
  });

  const result = await daemon.reconcile();

  assert.equal(result.enqueued, 1);
  assert.ok(calls[0].includes("all"));
  assert.ok(calls[0].includes("2026-08-01T00:00:00.000Z"));
  assert.equal(
    JSON.parse(readFileSync(cursorPath, "utf8")).since,
    "2026-08-02T01:02:03.000Z",
  );
});

test("failed reconcile leaves the durable cursor unchanged", async () => {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-reconcile-cursor-"));
  const cursorPath = path.join(root, "reconcile.json");
  const original = { version: 1, since: "2026-08-01T00:00:00.000Z" };
  writeFileSync(cursorPath, `${JSON.stringify(original)}\n`, { mode: 0o600 });
  const daemon = new AutopilotDaemon({
    policy,
    secrets: {
      apiKey: "e2a_agt_synthetic",
      forwardToken: "synthetic",
      deploymentUrl: "https://e2a.example.test",
    },
    spool: new JobSpool(path.join(root, "jobs")),
    supervisor: { runNextJob: async () => ({ state: "idle" }) },
    cli: { command: "/synthetic/e2a", baseArgs: [] },
    reconcileStatePath: cursorPath,
    now: () => Date.parse("2026-08-02T01:02:03.000Z"),
    execFileImpl(_command, _args, _options, callback) {
      callback(null, "not-json\n");
    },
  });

  assert.equal(await daemon.reconcile(), null);
  assert.deepEqual(JSON.parse(readFileSync(cursorPath, "utf8")), original);
});

test("daemon schedules a persisted future retry on startup", async () => {
  const nextRetry = Date.now() + 20;
  let drains = 0;
  const daemon = new AutopilotDaemon({
    policy,
    secrets: {
      apiKey: "e2a_agt_synthetic",
      forwardToken: "synthetic",
      deploymentUrl: "https://e2a.example.test",
    },
    spool: {
      recoverRunning() {},
      promoteReadyRetries() {},
      nextRetryAt() { return nextRetry; },
    },
    supervisor: { async runNextJob() { drains += 1; return { state: "idle" }; } },
    cli: { command: "/usr/bin/false", baseArgs: [] },
  });

  daemon.schedulePersistedRetry();
  await new Promise((resolve) => setTimeout(resolve, 50));
  await daemon.stop();
  assert.ok(drains >= 1);
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

test("listener replacement exit is terminal instead of entering a restart fight", async () => {
  let spawns = 0;
  let child;
  const daemon = new AutopilotDaemon({
    policy,
    secrets: {
      apiKey: "e2a_agt_synthetic",
      forwardToken: "synthetic",
      deploymentUrl: "https://e2a.example.test",
    },
    spool: { recoverRunning() {}, promoteReadyRetries() {} },
    supervisor: { runNextJob: async () => ({ state: "idle" }) },
    cli: { command: "/synthetic/e2a", baseArgs: [] },
    spawnImpl() {
      spawns += 1;
      child = new EventEmitter();
      child.kill = () => {};
      return child;
    },
  });
  daemon.receiver = { port: 8123, close: async () => {} };

  daemon.spawnListener();
  child.emit("spawn");
  child.emit("exit", 5, null);
  await new Promise((resolve) => setTimeout(resolve, 20));
  await daemon.stop();

  assert.equal(spawns, 1);
  assert.equal(daemon.restartTimer, null);
});
