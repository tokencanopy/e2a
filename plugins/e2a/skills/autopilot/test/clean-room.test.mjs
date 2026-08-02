import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { AutopilotDaemon } from "../daemon.mjs";
import { installAutopilot, prepareAutopilotInstall } from "../installer.mjs";
import { loadInstallation, localStatus } from "../operator.mjs";
import { JobSpool } from "../spool.mjs";
import { AutopilotSupervisor } from "../supervisor.mjs";

const helperPath = path.resolve(import.meta.dirname, "..", "job-tool.mjs");

function runHelper(command, invocation, input = "") {
  return new Promise((resolve) => {
    const child = spawn(process.execPath, [helperPath, command], {
      env: invocation.env,
      stdio: ["pipe", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (chunk) => { stdout += chunk; });
    child.stderr.on("data", (chunk) => { stderr += chunk; });
    child.on("close", (code) => resolve({ code: code ?? -1, timedOut: false, stdout, stderr }));
    child.stdin.end(input);
  });
}

test("clean-room install, authorized screening release, reply, restart, dedupe, and status", async () => {
  const home = mkdtempSync(path.join(tmpdir(), "autopilot-clean-room-"));
  const policy = {
    version: 1,
    task: {
      profile: "customer-support",
      objective: "Answer routine synthetic support requests.",
      instructions: "Use the synthetic handbook and escalate billing.",
      replyMode: "submit-for-review",
    },
    mailbox: { agentEmail: "support@example.test", ownerEmail: "owner@example.test" },
    inbound: {
      mode: "addresses",
      addresses: ["buyer@customer.test"],
      domains: [],
      fallback: "review",
    },
    outbound: { requireReview: true, ccOwner: true },
    screening: { promptInjection: true },
    runtime: {
      adapter: "codex",
      command: "/opt/synthetic/bin/codex",
      workdir: "/srv/synthetic-support",
      sandbox: "custom",
    },
    service: { manager: "foreground" },
    acknowledgements: ["custom_sandbox_acknowledged"],
  };
  const openProtection = {
    inbound: { gate: { policy: "open", allowlist: [], action: "flag" }, scan: { sensitivity: "off" } },
    outbound: { gate: { policy: "open", allowlist: [], action: "flag" }, scan: { sensitivity: "off" } },
    holds: { ttl_seconds: 604800, on_expiry: "reject", suppress_notifications: false },
  };
  const setup = {
    current: structuredClone(openProtection),
    async preflight() {
      return {
        deploymentUrl: "https://e2a.example.test",
        apiBaseUrl: "https://e2a.example.test",
        cliCommand: process.execPath,
        cliBaseArgs: ["/opt/synthetic/e2a.mjs"],
      };
    },
    async getProtection() { return structuredClone(this.current); },
    async createAgentKey() { return { id: "key_clean_room", key: "e2a_agt_clean_room" }; },
    async replaceProtection(_agentEmail, next) {
      this.current = structuredClone(next);
      return structuredClone(next);
    },
    async revokeKey() {},
  };

  const prepared = await prepareAutopilotInstall({ policy, setup });
  const installation = await installAutopilot({
    policy,
    confirmation: prepared.planDigest,
    prepared,
    home,
    setup,
    skipExecutableChecks: true,
  });
  assert.equal(setup.current.inbound.gate.action, "review");
  assert.equal(setup.current.outbound.gate.action, "review");
  assert.equal(setup.current.holds.suppress_notifications, false);

  const spool = new JobSpool(path.join(installation.paths.stateRoot, "jobs"));
  const released = JSON.stringify({
    id: "msg_released_after_review",
    header_from: "buyer@customer.test",
    verified_domain: "customer.test",
  }) + "\n";
  const daemon = new AutopilotDaemon({
    policy,
    secrets: { apiKey: "e2a_agt_clean_room", forwardToken: "synthetic", deploymentUrl: "https://e2a.example.test" },
    spool,
    supervisor: { async runNextJob() { return { state: "idle" }; } },
    cli: { command: process.execPath, baseArgs: ["/opt/synthetic/e2a.mjs"] },
    reconcileStatePath: path.join(installation.paths.stateRoot, "reconcile.json"),
    execFileImpl(_command, _args, _options, callback) { callback(null, released); },
  });
  const firstReconcile = await daemon.reconcile();
  assert.deepEqual(firstReconcile, { checked: 1, enqueued: 1, refused: 0, deduplicated: 0 });

  spool.claimNext();
  const restartedSpool = new JobSpool(path.join(installation.paths.stateRoot, "jobs"));
  assert.equal(restartedSpool.recoverRunning(), 1);
  assert.equal(restartedSpool.promoteReadyRetries(), 1);

  const replies = [];
  const supervisor = new AutopilotSupervisor({
    policy,
    spool: restartedSpool,
    stateRoot: installation.paths.stateRoot,
    helperPath,
    mail: {
      async getMessage(id) {
        return {
          id,
          conversation_id: "conv_synthetic",
          header_from: "buyer@customer.test",
          reply_to: [],
        };
      },
      async getThread(id) { return [{ id, conversation_id: "conv_synthetic" }]; },
      async reply(messageId, input) {
        replies.push({ messageId, ...input });
        return { message_id: "msg_reply_synthetic", status: "pending_review" };
      },
    },
    runtimeExecutor: async (invocation) => {
      const reply = await runHelper("reply", invocation, "Here is the synthetic answer.\n");
      assert.equal(reply.code, 0, reply.stderr);
      return runHelper("complete", invocation, "Answered and submitted for review.\n");
    },
  });
  const handled = await supervisor.runNextJob();
  assert.equal(
    handled.state,
    "done",
    JSON.stringify(restartedSpool.list(handled.state === "retry" ? "retry" : "dead")),
  );
  assert.equal(replies.length, 1);
  assert.deepEqual(replies[0].cc, ["owner@example.test"]);
  assert.match(replies[0].idempotencyKey, /^autopilot-[a-f0-9]{64}$/);

  const secondReconcile = await daemon.reconcile();
  assert.deepEqual(secondReconcile, { checked: 1, enqueued: 0, refused: 0, deduplicated: 1 });
  const status = localStatus(loadInstallation({ agentEmail: policy.mailbox.agentEmail, home }));
  assert.deepEqual(status.jobs, { pending: 0, running: 0, retry: 0, done: 1, dead: 0 });
});
