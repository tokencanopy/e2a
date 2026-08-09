import assert from "node:assert/strict";
import path from "node:path";
import test from "node:test";

import {
  buildLaunchdDefinition,
  buildSystemdDefinition,
  serviceCommands,
  serviceIdentity,
} from "../service.mjs";

const paths = {
  runnerPath: "/opt/e2a/autopilot/runner.mjs",
  nodePath: "/usr/local/bin/node",
  policyPath: "/home/operator/.local/share/e2a-autopilot/example/policy.json",
  secretsPath: "/home/operator/.local/share/e2a-autopilot/example/secrets.json",
  stateRoot: "/home/operator/.local/share/e2a-autopilot/example/state",
  stdoutPath: "/home/operator/.local/share/e2a-autopilot/example/logs/autopilot.log",
  stderrPath: "/home/operator/.local/share/e2a-autopilot/example/logs/autopilot.err.log",
};

test("service identity is deterministic and contains no mailbox", () => {
  const one = serviceIdentity("support@example.test");
  const two = serviceIdentity("support@example.test");
  assert.deepEqual(one, two);
  assert.match(one.slug, /^agent-[a-f0-9]{16}$/);
  assert.doesNotMatch(JSON.stringify(one), /support|example\.test/);
});

test("launchd definition contains paths but no credentials", () => {
  const definition = buildLaunchdDefinition({
    ...paths,
    label: "dev.e2a.autopilot.agent-deadbeefdeadbeef",
  });

  assert.match(definition, /E2A_AUTOPILOT_POLICY_PATH/);
  assert.match(definition, /KeepAlive/);
  assert.doesNotMatch(definition, /e2a_agt_|forwardToken|apiKey/);
  assert.match(definition, new RegExp(path.basename(paths.runnerPath)));
});

test("systemd definition contains paths but no credentials", () => {
  const definition = buildSystemdDefinition(paths);

  assert.match(definition, /Restart=on-failure/);
  assert.match(definition, /E2A_AUTOPILOT_SECRETS_PATH/);
  assert.doesNotMatch(definition, /e2a_agt_|forwardToken|apiKey/);
});

test("service lifecycle commands never place secrets on the command line", () => {
  const launchd = serviceCommands({
    manager: "launchd",
    action: "start",
    servicePath: "/Users/operator/Library/LaunchAgents/dev.e2a.autopilot.agent-test.plist",
    identity: { launchdLabel: "dev.e2a.autopilot.agent-test" },
    uid: 501,
  });
  assert.deepEqual(launchd, [
    ["launchctl", ["bootstrap", "gui/501", "/Users/operator/Library/LaunchAgents/dev.e2a.autopilot.agent-test.plist"]],
  ]);

  const systemd = serviceCommands({
    manager: "systemd",
    action: "start",
    servicePath: "/home/operator/.config/systemd/user/e2a-autopilot-agent-test.service",
    identity: { systemdUnit: "e2a-autopilot-agent-test.service" },
  });
  assert.deepEqual(systemd, [
    ["systemctl", ["--user", "daemon-reload"]],
    ["systemctl", ["--user", "enable", "--now", "e2a-autopilot-agent-test.service"]],
  ]);
  assert.doesNotMatch(JSON.stringify({ launchd, systemd }), /e2a_agt_|forwardToken/);
});
