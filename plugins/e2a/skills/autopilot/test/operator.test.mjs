import assert from "node:assert/strict";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import {
  controlService,
  loadInstallation,
  localStatus,
  uninstallAutopilot,
} from "../operator.mjs";
import { installationPaths } from "../installer.mjs";

function fixture(manager = "launchd") {
  const home = mkdtempSync(path.join(tmpdir(), "autopilot-operator-"));
  const policy = {
    version: 1,
    task: { profile: "customer-support", objective: "Answer routine support.", instructions: "Escalate billing.", replyMode: "submit-for-review" },
    mailbox: { agentEmail: "support@example.test", ownerEmail: "owner@example.test" },
    inbound: { mode: "addresses", addresses: ["buyer@customer.test"], domains: [], fallback: "review" },
    outbound: { requireReview: true, ccOwner: true },
    screening: { promptInjection: true },
    runtime: { adapter: "codex", command: "/usr/local/bin/codex", workdir: "/srv/support", sandbox: "custom" },
    service: { manager },
    acknowledgements: ["custom_sandbox_acknowledged"],
  };
  const paths = installationPaths(policy, home);
  for (const directory of [paths.root, paths.stateRoot, paths.logsRoot]) {
    mkdirSync(directory, { recursive: true, mode: 0o700 });
    chmodSync(directory, 0o700);
  }
  for (const state of ["pending", "running", "retry", "done", "dead"]) {
    mkdirSync(path.join(paths.stateRoot, "jobs", state), { recursive: true, mode: 0o700 });
  }
  writeFileSync(paths.policyPath, `${JSON.stringify(policy)}\n`, { mode: 0o600 });
  writeFileSync(paths.secretsPath, `${JSON.stringify({
    version: 1,
    apiKey: "e2a_agt_synthetic",
    keyId: "key_synthetic",
    forwardToken: "synthetic_forward_capability",
    cliCommand: "/usr/local/bin/node",
    cliBaseArgs: ["/opt/e2a/dist/bin/e2a.js"],
    deploymentUrl: "https://e2a.example.test",
    apiBaseUrl: "https://api.e2a.example.test",
  })}\n`, { mode: 0o600 });
  writeFileSync(paths.installPath, `${JSON.stringify({ version: 1, serviceManager: manager, servicePath: paths.servicePath })}\n`, { mode: 0o600 });
  if (paths.servicePath) {
    mkdirSync(path.dirname(paths.servicePath), { recursive: true });
    writeFileSync(paths.servicePath, "synthetic service\n", { mode: 0o600 });
  }
  writeFileSync(path.join(paths.stateRoot, "jobs", "pending", "message-1.json"), "{}\n");
  writeFileSync(path.join(paths.stateRoot, "jobs", "dead", "message-2.json"), "{}\n");
  return { home, policy, paths };
}

test("status reports durable job counts without reading message content", () => {
  const files = fixture();
  const installed = loadInstallation({ agentEmail: files.policy.mailbox.agentEmail, home: files.home });
  const status = localStatus(installed);

  assert.equal(status.installed, true);
  assert.deepEqual(status.jobs, { pending: 1, running: 0, retry: 0, done: 0, dead: 1 });
  assert.equal(status.manager, "launchd");
});

test("service control executes the generated manager command", () => {
  const files = fixture("systemd");
  const installed = loadInstallation({ agentEmail: files.policy.mailbox.agentEmail, home: files.home });
  const calls = [];
  controlService(installed, "start", {
    uid: 1000,
    execFileSyncImpl(command, args) {
      calls.push([command, args]);
    },
  });
  assert.deepEqual(calls.map(([command, args]) => [command, args.slice(0, 2)]), [
    ["systemctl", ["--user", "daemon-reload"]],
    ["systemctl", ["--user", "enable"]],
  ]);
});

test("confirmed uninstall stops, revokes, removes service, and archives local state", async () => {
  const files = fixture();
  const calls = [];
  const setup = {
    async preflight(agentEmail) { calls.push(["preflight", agentEmail]); },
    async revokeKey(keyId) { calls.push(["revokeKey", keyId]); },
  };
  const result = await uninstallAutopilot({
    agentEmail: files.policy.mailbox.agentEmail,
    confirmation: "DELETE",
    home: files.home,
    setup,
    control(_installed, action) { calls.push(["control", action]); },
    now: () => new Date("2026-08-02T12:34:56.000Z"),
  });

  assert.deepEqual(calls, [
    ["control", "stop"],
    ["preflight", "support@example.test"],
    ["revokeKey", "key_synthetic"],
  ]);
  assert.equal(existsSync(files.paths.servicePath), false);
  assert.equal(existsSync(files.paths.root), false);
  assert.equal(existsSync(result.archivePath), true);
  assert.match(result.archivePath, /\.uninstalled-20260802T123456Z$/);
  assert.match(readFileSync(path.join(result.archivePath, "policy.json"), "utf8"), /support@example\.test/);
});
