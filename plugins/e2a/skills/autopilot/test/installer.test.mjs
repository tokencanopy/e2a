import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, readdirSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { installAutopilot, installationPaths } from "../installer.mjs";
import { planDigest } from "../policy.mjs";

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
    runtime: { adapter: "codex", command: runtime, workdir, sandbox: "native" },
    service: { manager: "launchd" },
    acknowledgements: [],
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
        apiBaseUrl: "https://api.e2a.example.test",
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
  return { home, policy, current, calls, setup };
}

test("confirmed install verifies remote policy then writes private local state", async () => {
  const files = fixture();
  const result = await installAutopilot({
    policy: files.policy,
    confirmation: planDigest(files.policy),
    home: files.home,
    setup: files.setup,
    skipExecutableChecks: true,
  });

  assert.equal(result.installed, true);
  assert.equal(result.started, false);
  assert.equal(statSync(result.paths.root).mode & 0o777, 0o700);
  assert.equal(statSync(result.paths.policyPath).mode & 0o777, 0o600);
  assert.equal(statSync(result.paths.secretsPath).mode & 0o777, 0o600);
  assert.equal(statSync(result.paths.runtimeRoot).mode & 0o777, 0o700);
  assert.equal(statSync(result.paths.runnerPath).mode & 0o777, 0o600);
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

  await assert.rejects(
    installAutopilot({
      policy: files.policy,
      confirmation: planDigest(files.policy),
      home: files.home,
      setup: files.setup,
      skipExecutableChecks: true,
      writeServiceDefinition() {
        throw new Error("synthetic service write failure");
      },
    }),
    /synthetic service write failure/,
  );

  const names = files.calls.map(([name]) => name);
  assert.deepEqual(names.slice(-2), ["replaceProtection", "revokeKey"]);
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
  await assert.rejects(
    installAutopilot({
      policy: files.policy,
      confirmation: "0".repeat(64),
      home: files.home,
      setup: files.setup,
      skipExecutableChecks: true,
    }),
    /confirmation digest does not match/i,
  );
  assert.deepEqual(files.calls, []);
});
