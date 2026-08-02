import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { loadInstalledConfig } from "../config.mjs";

function fixture() {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-config-"));
  const policyPath = path.join(root, "policy.json");
  const secretsPath = path.join(root, "secrets.json");
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
    runtime: {
      adapter: "codex",
      command: "/usr/local/bin/codex",
      workdir: "/srv/autopilot/support",
      sandbox: "native",
    },
    service: { manager: "foreground" },
    acknowledgements: [],
  };
  const secrets = {
    version: 1,
    apiKey: "e2a_agt_synthetic",
    keyId: "key_synthetic",
    forwardToken: "synthetic_forward_capability",
    cliCommand: "/usr/local/bin/node",
    cliBaseArgs: ["/opt/e2a/dist/bin/e2a.js"],
    deploymentUrl: "https://e2a.example.test",
    apiBaseUrl: "https://api.e2a.example.test",
  };
  writeFileSync(policyPath, `${JSON.stringify(policy)}\n`, { mode: 0o600 });
  writeFileSync(secretsPath, `${JSON.stringify(secrets)}\n`, { mode: 0o600 });
  return { root, policyPath, secretsPath, policy, secrets };
}

test("loadInstalledConfig validates policy and owner-only credential storage", () => {
  const files = fixture();
  const loaded = loadInstalledConfig({
    policyPath: files.policyPath,
    secretsPath: files.secretsPath,
    stateRoot: path.join(files.root, "state"),
  });

  assert.equal(loaded.policy.mailbox.agentEmail, "support@example.test");
  assert.equal(loaded.secrets.apiKey, "e2a_agt_synthetic");
  assert.deepEqual(loaded.cli, {
    command: "/usr/local/bin/node",
    baseArgs: ["/opt/e2a/dist/bin/e2a.js"],
  });
  assert.equal(loaded.stateRoot, path.join(files.root, "state"));
});

test("loadInstalledConfig refuses a credential file readable by group or others", () => {
  const files = fixture();
  chmodSync(files.secretsPath, 0o640);

  assert.throws(
    () =>
      loadInstalledConfig({
        policyPath: files.policyPath,
        secretsPath: files.secretsPath,
        stateRoot: path.join(files.root, "state"),
      }),
    /must have mode 0600/,
  );
});

test("loadInstalledConfig refuses invalid policy and relative executable paths", () => {
  const files = fixture();
  files.policy.inbound.addresses = [];
  writeFileSync(files.policyPath, JSON.stringify(files.policy), { mode: 0o600 });
  assert.throws(
    () =>
      loadInstalledConfig({
        policyPath: files.policyPath,
        secretsPath: files.secretsPath,
        stateRoot: path.join(files.root, "state"),
      }),
    /Invalid Autopilot policy/,
  );

  files.policy.inbound.addresses = ["customer@buyer.test"];
  files.secrets.cliCommand = "node";
  writeFileSync(files.policyPath, JSON.stringify(files.policy), { mode: 0o600 });
  writeFileSync(files.secretsPath, JSON.stringify(files.secrets), { mode: 0o600 });
  assert.throws(
    () =>
      loadInstalledConfig({
        policyPath: files.policyPath,
        secretsPath: files.secretsPath,
        stateRoot: path.join(files.root, "state"),
      }),
    /CLI command must be an absolute path/,
  );
});

test("loadInstalledConfig rejects unknown credential fields", () => {
  const files = fixture();
  files.secrets.accountApiKey = "must-never-be-stored";
  writeFileSync(files.secretsPath, JSON.stringify(files.secrets), { mode: 0o600 });

  assert.throws(
    () =>
      loadInstalledConfig({
        policyPath: files.policyPath,
        secretsPath: files.secretsPath,
        stateRoot: path.join(files.root, "state"),
      }),
    /Unknown credential field: accountApiKey/,
  );
});
