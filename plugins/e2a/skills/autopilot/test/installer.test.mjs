import assert from "node:assert/strict";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import {
  installAutopilot,
  installationPaths,
  prepareAutopilotInstall,
  validateServiceManager,
} from "../installer.mjs";

function fixture() {
  const home = mkdtempSync(path.join(tmpdir(), "autopilot-install-"));
  const runtime = path.join(home, "bin", "codex");
  const workdir = path.join(home, "workspace");
  const cliCommand = path.join(home, "bin", "node");
  const policy = {
    version: 1,
    task: {
      profile: "customer-support",
      objective: "Resolve routine support requests.",
      instructions: "Use approved docs and escalate billing.",
      replyMode: "submit-for-review",
    },
    mailbox: {
      agentEmail: "support@example.test",
      ownerEmail: "owner@example.test",
    },
    inbound: {
      mode: "addresses",
      addresses: ["customer@buyer.test"],
      domains: [],
      fallback: "review",
    },
    outbound: { requireReview: true, ccOwner: true },
    screening: { promptInjection: true },
    runtime: { adapter: "codex", command: runtime, workdir, sandbox: "custom" },
    service: { manager: "launchd" },
    acknowledgements: ["custom_sandbox_acknowledged"],
  };
  const current = {
    inbound: {
      gate: { policy: "open", allowlist: [], action: "flag" },
      scan: { sensitivity: "off" },
    },
    outbound: {
      gate: { policy: "open", allowlist: [], action: "flag" },
      scan: { sensitivity: "off" },
    },
    holds: { ttl_seconds: 604800, on_expiry: "reject", suppress_notifications: false },
  };
  const calls = [];
  const setup = {
    async preflight(agentEmail) {
      calls.push(["preflight", agentEmail]);
      return {
        deploymentUrl: "https://e2a.example.test",
        apiBaseUrl: "https://e2a.example.test",
        cliCommand,
        cliBaseArgs: ["/opt/e2a/dist/bin/e2a.js"],
      };
    },
    async getProtection() {
      calls.push(["getProtection"]);
      return structuredClone(this.current ?? current);
    },
    async createAgentKey(agentEmail) {
      calls.push(["createAgentKey", agentEmail]);
      return { id: "key_synthetic", key: "e2a_agt_synthetic" };
    },
    async replaceProtection(_agentEmail, next) {
      calls.push(["replaceProtection", structuredClone(next)]);
      this.current = structuredClone(next);
      return structuredClone(next);
    },
    async revokeKey(id) {
      calls.push(["revokeKey", id]);
    },
  };
  const launchctl = path.join(home, "bin", "launchctl");
  mkdirSync(path.dirname(launchctl), { recursive: true });
  writeFileSync(launchctl, "#!/bin/sh\n", { mode: 0o700 });
  chmodSync(launchctl, 0o700);
  return {
    home,
    policy,
    current,
    calls,
    setup,
    installOptions: {
      platform: "darwin",
      environment: { PATH: path.dirname(launchctl) },
    },
  };
}

async function confirmed(files) {
  const prepared = await prepareAutopilotInstall({ policy: files.policy, setup: files.setup });
  return { prepared, confirmation: prepared.planDigest };
}

test("confirmed install verifies remote policy then writes private local state", async () => {
  const files = fixture();
  const confirmation = await confirmed(files);
  const result = await installAutopilot({
    policy: files.policy,
    ...confirmation,
    home: files.home,
    setup: files.setup,
    skipExecutableChecks: true,
    ...files.installOptions,
  });

  assert.equal(result.installed, true);
  assert.equal(result.started, false);
  assert.equal(statSync(result.paths.root).mode & 0o777, 0o700);
  assert.equal(statSync(result.paths.policyPath).mode & 0o777, 0o600);
  assert.equal(statSync(result.paths.secretsPath).mode & 0o777, 0o600);
  assert.equal(statSync(result.paths.runtimeRoot).mode & 0o777, 0o700);
  assert.equal(statSync(result.paths.runnerPath).mode & 0o777, 0o600);
  assert.equal(statSync(path.join(result.paths.stateRoot, "reconcile.json")).mode & 0o777, 0o600);
  assert.ok(readdirSync(result.paths.runtimeRoot).includes("job-tool.mjs"));
  assert.ok(!readdirSync(result.paths.runtimeRoot).includes("installer.mjs"));
  const secrets = JSON.parse(readFileSync(result.paths.secretsPath, "utf8"));
  assert.equal(secrets.apiKey, "e2a_agt_synthetic");
  assert.equal(secrets.keyId, "key_synthetic");
  const service = readFileSync(result.paths.servicePath, "utf8");
  assert.doesNotMatch(service, /e2a_agt_synthetic/);
  assert.match(service, new RegExp(result.paths.runnerPath.replaceAll("/", "\\/")));
  assert.deepEqual(files.calls.map(([name]) => name), [
    "preflight",
    "getProtection",
    "createAgentKey",
    "replaceProtection",
    "getProtection",
  ]);
});

test("failed local install restores protection, revokes key, and removes only new files", async () => {
  const files = fixture();
  files.setup.getProtection = async function getProtection() {
    files.calls.push(["getProtection"]);
    return structuredClone(this.current ?? files.current);
  };

  const confirmation = await confirmed(files);
  await assert.rejects(
    installAutopilot({
      policy: files.policy,
      ...confirmation,
      home: files.home,
      setup: files.setup,
      skipExecutableChecks: true,
      ...files.installOptions,
      writeServiceDefinition() {
        throw new Error("synthetic service write failure");
      },
    }),
    /synthetic service write failure/,
  );

  const names = files.calls.map(([name]) => name);
  assert.deepEqual(names.slice(-3), ["getProtection", "replaceProtection", "revokeKey"]);
  assert.equal(existsSync(path.join(files.home, ".local", "share", "e2a-autopilot")), true);
  const installRoot = installationPaths(files.policy, files.home).root;
  assert.equal(
    existsSync(installRoot),
    false,
    "rollback must not leave the attempted agent root",
  );
});

test("install refuses a stale or missing confirmation before setup mutation", async () => {
  const files = fixture();
  const prepared = await prepareAutopilotInstall({ policy: files.policy, setup: files.setup });
  await assert.rejects(
    installAutopilot({
      policy: files.policy,
      confirmation: "0".repeat(64),
      prepared,
      home: files.home,
      setup: files.setup,
      skipExecutableChecks: true,
      ...files.installOptions,
    }),
    /confirmation digest does not match/i,
  );
  assert.deepEqual(files.calls.map(([name]) => name), ["preflight", "getProtection"]);
});

test("commit-then-transport-error conditionally restores protection and revokes the key", async () => {
  const files = fixture();
  const prepared = await prepareAutopilotInstall({ policy: files.policy, setup: files.setup });
  let first = true;
  files.setup.replaceProtection = async function replaceProtection(_agentEmail, next) {
    files.calls.push(["replaceProtection", structuredClone(next)]);
    this.current = structuredClone(next);
    if (first) {
      first = false;
      throw new Error("synthetic lost response");
    }
    return structuredClone(next);
  };

  await assert.rejects(
    installAutopilot({
      policy: files.policy,
      confirmation: prepared.planDigest,
      prepared,
      home: files.home,
      setup: files.setup,
      skipExecutableChecks: true,
      ...files.installOptions,
    }),
    /synthetic lost response/,
  );

  assert.deepEqual(files.setup.current, files.current);
  assert.deepEqual(files.calls.map(([name]) => name).slice(-4), [
    "replaceProtection",
    "getProtection",
    "replaceProtection",
    "revokeKey",
  ]);
});

test("service manager compatibility is checked before remote mutation", () => {
  assert.throws(
    () => validateServiceManager("launchd", { platform: "linux", environment: { PATH: "" } }),
    /requires macOS/,
  );
  assert.throws(
    () => validateServiceManager("systemd", { platform: "darwin", environment: { PATH: "" } }),
    /requires Linux/,
  );
});
