import { randomBytes, randomUUID } from "node:crypto";
import {
  accessSync,
  chmodSync,
  constants,
  existsSync,
  mkdirSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { homedir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { normalizePolicy, planDigest, validatePolicy } from "./policy.mjs";
import {
  buildProtectionDocument,
  protectionMatchesPolicy,
} from "./setup.mjs";
import {
  buildLaunchdDefinition,
  buildSystemdDefinition,
  serviceIdentity,
} from "./service.mjs";

const pluginDirectory = path.dirname(fileURLToPath(import.meta.url));

function privateDirectory(directory) {
  mkdirSync(directory, { recursive: true, mode: 0o700 });
  chmodSync(directory, 0o700);
}

function atomicWrite(file, content, mode = 0o600) {
  const temporary = path.join(path.dirname(file), `.${path.basename(file)}.${randomUUID()}.tmp`);
  writeFileSync(temporary, content, { encoding: "utf8", flag: "wx", mode });
  renameSync(temporary, file);
  chmodSync(file, mode);
}

function taskMarkdown(policy) {
  return [
    "# Autopilot task",
    "",
    `Profile: ${policy.task.profile}`,
    "",
    "## Outcome",
    "",
    policy.task.objective,
    "",
    "## Instructions",
    "",
    policy.task.instructions || "Follow the configured profile and escalate when uncertain.",
    "",
    "## Mailbox boundary",
    "",
    "Operate only on the current job through the local Autopilot job gateway. Do not seek mailbox, e2a, shell, or unrelated filesystem credentials.",
    "",
  ].join("\n");
}

export function installationPaths(policy, home = homedir()) {
  const identity = serviceIdentity(policy.mailbox.agentEmail);
  const root = path.join(home, ".local", "share", "e2a-autopilot", identity.slug);
  const manager = policy.service.manager;
  const servicePath =
    manager === "launchd"
      ? path.join(home, "Library", "LaunchAgents", `${identity.launchdLabel}.plist`)
      : manager === "systemd"
        ? path.join(home, ".config", "systemd", "user", identity.systemdUnit)
        : null;
  return {
    root,
    policyPath: path.join(root, "policy.json"),
    taskPath: path.join(root, "task.md"),
    secretsPath: path.join(root, "secrets.json"),
    installPath: path.join(root, "install.json"),
    stateRoot: path.join(root, "state"),
    logsRoot: path.join(root, "logs"),
    stdoutPath: path.join(root, "logs", "autopilot.log"),
    stderrPath: path.join(root, "logs", "autopilot.err.log"),
    servicePath,
    identity,
  };
}

function checkExecutable(file, label) {
  if (!path.isAbsolute(file)) throw new Error(`${label} must be an absolute path.`);
  try {
    accessSync(file, constants.X_OK);
  } catch {
    throw new Error(`${label} is not executable: ${file}.`);
  }
}

function defaultWriteServiceDefinition({ file, content }) {
  mkdirSync(path.dirname(file), { recursive: true });
  atomicWrite(file, content, 0o600);
}

export async function installAutopilot({
  policy: policyInput,
  confirmation,
  setup,
  home = homedir(),
  nodePath = process.execPath,
  runnerPath = path.join(pluginDirectory, "runner.mjs"),
  skipExecutableChecks = false,
  writeServiceDefinition = defaultWriteServiceDefinition,
}) {
  const policy = normalizePolicy(policyInput);
  const errors = validatePolicy(policy);
  if (errors.length > 0) throw new Error(`Invalid Autopilot policy:\n- ${errors.join("\n- ")}`);
  const expectedDigest = planDigest(policy);
  if (confirmation !== expectedDigest) {
    throw new Error("Plan confirmation digest does not match the current policy. Run `autopilot plan` again.");
  }
  if (!setup) throw new Error("An e2a setup client is required.");

  const paths = installationPaths(policy, home);
  if (existsSync(paths.root)) throw new Error(`Autopilot is already installed at ${paths.root}.`);
  if (paths.servicePath && existsSync(paths.servicePath)) {
    throw new Error(`Autopilot service definition already exists at ${paths.servicePath}.`);
  }
  if (!skipExecutableChecks) {
    checkExecutable(policy.runtime.command, "Runtime command");
    checkExecutable(nodePath, "Node executable");
    if (!statSync(policy.runtime.workdir).isDirectory()) {
      throw new Error(`Task workspace is not a directory: ${policy.runtime.workdir}.`);
    }
  }

  let priorProtection;
  let createdKey;
  let remoteChanged = false;
  let localCreated = false;
  let serviceAttempted = false;
  try {
    const preflight = await setup.preflight(policy.mailbox.agentEmail);
    priorProtection = await setup.getProtection(policy.mailbox.agentEmail);
    const nextProtection = buildProtectionDocument(priorProtection, policy);
    createdKey = await setup.createAgentKey(policy.mailbox.agentEmail);
    await setup.replaceProtection(policy.mailbox.agentEmail, nextProtection);
    remoteChanged = true;
    const verified = await setup.getProtection(policy.mailbox.agentEmail);
    if (!protectionMatchesPolicy(verified, policy)) {
      throw new Error("e2a protection verification did not match the confirmed Autopilot policy.");
    }

    privateDirectory(paths.root);
    localCreated = true;
    privateDirectory(paths.stateRoot);
    privateDirectory(path.join(paths.stateRoot, "jobs"));
    privateDirectory(path.join(paths.stateRoot, "locks"));
    privateDirectory(paths.logsRoot);
    atomicWrite(paths.policyPath, `${JSON.stringify(policy, null, 2)}\n`);
    atomicWrite(paths.taskPath, taskMarkdown(policy));
    atomicWrite(
      paths.secretsPath,
      `${JSON.stringify(
        {
          version: 1,
          apiKey: createdKey.key,
          keyId: createdKey.id,
          forwardToken: randomBytes(32).toString("hex"),
          cliCommand: preflight.cliCommand,
          cliBaseArgs: preflight.cliBaseArgs,
          deploymentUrl: preflight.deploymentUrl,
          apiBaseUrl: preflight.apiBaseUrl,
        },
        null,
        2,
      )}\n`,
    );
    atomicWrite(
      paths.installPath,
      `${JSON.stringify(
        {
          version: 1,
          planDigest: expectedDigest,
          serviceManager: policy.service.manager,
          servicePath: paths.servicePath,
          installedAt: new Date().toISOString(),
        },
        null,
        2,
      )}\n`,
    );

    if (paths.servicePath) {
      const serviceValues = {
        nodePath,
        runnerPath,
        policyPath: paths.policyPath,
        secretsPath: paths.secretsPath,
        stateRoot: paths.stateRoot,
        stdoutPath: paths.stdoutPath,
        stderrPath: paths.stderrPath,
        pathValue: process.env.PATH,
      };
      const content =
        policy.service.manager === "launchd"
          ? buildLaunchdDefinition({ ...serviceValues, label: paths.identity.launchdLabel })
          : buildSystemdDefinition(serviceValues);
      serviceAttempted = true;
      writeServiceDefinition({ file: paths.servicePath, content });
    }

    return { installed: true, started: false, paths, planDigest: expectedDigest };
  } catch (error) {
    const rollbackErrors = [];
    if (serviceAttempted && paths.servicePath) {
      try {
        rmSync(paths.servicePath, { force: true });
      } catch (rollbackError) {
        rollbackErrors.push(`service cleanup failed: ${rollbackError.message}`);
      }
    }
    if (localCreated) {
      try {
        rmSync(paths.root, { recursive: true, force: true });
      } catch (rollbackError) {
        rollbackErrors.push(`local cleanup failed: ${rollbackError.message}`);
      }
    }
    if (remoteChanged && priorProtection) {
      try {
        await setup.replaceProtection(policy.mailbox.agentEmail, priorProtection);
      } catch (rollbackError) {
        rollbackErrors.push(`protection restore failed: ${rollbackError.message}`);
      }
    }
    if (createdKey?.id) {
      try {
        await setup.revokeKey(createdKey.id);
      } catch (rollbackError) {
        rollbackErrors.push(`credential revocation failed: ${rollbackError.message}`);
      }
    }
    if (rollbackErrors.length > 0) {
      throw new Error(`${error.message}\nROLLBACK INCOMPLETE:\n- ${rollbackErrors.join("\n- ")}`);
    }
    throw error;
  }
}
