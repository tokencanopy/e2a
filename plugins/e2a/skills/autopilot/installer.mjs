import { randomBytes, randomUUID } from "node:crypto";
import {
  accessSync,
  chmodSync,
  constants,
  existsSync,
  lstatSync,
  mkdirSync,
  readFileSync,
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
  normalizeProtection,
  protectionsEqual,
  protectionMatchesPolicy,
} from "./setup.mjs";
import {
  buildLaunchdDefinition,
  buildSystemdDefinition,
  serviceIdentity,
} from "./service.mjs";

const pluginDirectory = path.dirname(fileURLToPath(import.meta.url));
const RUNTIME_FILES = [
  "config.mjs",
  "daemon.mjs",
  "gateway.mjs",
  "job-tool.mjs",
  "lock.mjs",
  "mail-client.mjs",
  "policy.mjs",
  "runner.mjs",
  "runtime.mjs",
  "service.mjs",
  "spool.mjs",
  "supervisor.mjs",
];

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

function copyRuntimeBundle(sourceRoot, targetRoot) {
  privateDirectory(targetRoot);
  for (const name of RUNTIME_FILES) {
    atomicWrite(path.join(targetRoot, name), readFileSync(path.join(sourceRoot, name), "utf8"));
  }
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
    runtimeRoot: path.join(root, "runtime"),
    runnerPath: path.join(root, "runtime", "runner.mjs"),
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

function assertSafeParentChain(target, anchor) {
  const root = path.resolve(anchor);
  const parent = path.dirname(path.resolve(target));
  const relative = path.relative(root, parent);
  if (relative.startsWith("..") || path.isAbsolute(relative)) {
    throw new Error(`Refusing to write outside the installation home: ${target}.`);
  }
  let current = root;
  for (const part of relative.split(path.sep).filter(Boolean)) {
    current = path.join(current, part);
    if (!existsSync(current)) break;
    const info = lstatSync(current);
    if (info.isSymbolicLink() || !info.isDirectory()) {
      throw new Error(`Refusing to follow a non-directory or symlink parent: ${current}.`);
    }
  }
}

function findExecutable(name, environment = process.env) {
  for (const directory of String(environment.PATH || "").split(path.delimiter).filter(Boolean)) {
    const candidate = path.join(directory, name);
    try {
      accessSync(candidate, constants.X_OK);
      return candidate;
    } catch {
      // Continue through PATH.
    }
  }
  return null;
}

export function validateServiceManager(
  manager,
  { platform = process.platform, environment = process.env } = {},
) {
  if (manager === "foreground") return;
  if (manager === "launchd") {
    if (platform !== "darwin") throw new Error("launchd service mode requires macOS.");
    if (!findExecutable("launchctl", environment)) throw new Error("launchctl is not available on PATH.");
    return;
  }
  if (manager === "systemd") {
    if (platform !== "linux") throw new Error("systemd service mode requires Linux.");
    if (!findExecutable("systemctl", environment)) throw new Error("systemctl is not available on PATH.");
    return;
  }
  throw new Error(`Unsupported service manager: ${manager}.`);
}

export async function prepareAutopilotInstall({ policy: policyInput, setup }) {
  const policy = normalizePolicy(policyInput);
  const errors = validatePolicy(policy);
  if (errors.length > 0) throw new Error(`Invalid Autopilot policy:\n- ${errors.join("\n- ")}`);
  if (!setup) throw new Error("An e2a setup client is required.");
  const preparedAt = new Date().toISOString();
  const preflight = await setup.preflight(policy.mailbox.agentEmail);
  const priorProtection = normalizeProtection(
    await setup.getProtection(policy.mailbox.agentEmail),
  );
  const nextProtection = buildProtectionDocument(priorProtection, policy);
  const context = {
    ...preflight,
    priorProtection,
    nextProtection,
  };
  return {
    policy,
    preflight,
    priorProtection,
    nextProtection,
    context,
    preparedAt,
    planDigest: planDigest(policy, context),
  };
}

export async function installAutopilot({
  policy: policyInput,
  confirmation,
  setup,
  prepared,
  home = homedir(),
  nodePath = process.execPath,
  runtimeSourceRoot = pluginDirectory,
  skipExecutableChecks = false,
  writeServiceDefinition = defaultWriteServiceDefinition,
  platform = process.platform,
  environment = process.env,
}) {
  const policy = normalizePolicy(policyInput);
  const errors = validatePolicy(policy);
  if (errors.length > 0) throw new Error(`Invalid Autopilot policy:\n- ${errors.join("\n- ")}`);
  if (!setup) throw new Error("An e2a setup client is required.");

  const paths = installationPaths(policy, home);
  assertSafeParentChain(paths.root, home);
  if (paths.servicePath) assertSafeParentChain(paths.servicePath, home);
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
  validateServiceManager(policy.service.manager, { platform, environment });

  let priorProtection;
  let nextProtection;
  let preflight;
  let expectedDigest;
  let createdKey;
  let remoteMutationAttempted = false;
  let localCreated = false;
  let serviceAttempted = false;
  try {
    const preparation = prepared || await prepareAutopilotInstall({ policy, setup });
    preflight = preparation.preflight;
    priorProtection = normalizeProtection(preparation.priorProtection);
    nextProtection = buildProtectionDocument(priorProtection, policy);
    const confirmedContext = {
      ...preflight,
      priorProtection,
      nextProtection,
    };
    expectedDigest = planDigest(policy, confirmedContext);
    if (preparation.planDigest !== expectedDigest) {
      throw new Error("Prepared installation context does not match the current policy.");
    }
    if (Number.isNaN(Date.parse(preparation.preparedAt))) {
      throw new Error("Prepared installation timestamp is invalid.");
    }
    if (confirmation !== expectedDigest) {
      throw new Error("Plan confirmation digest does not match the current policy, CLI origin, or protection document. Run `autopilot plan` again.");
    }
    createdKey = await setup.createAgentKey(policy.mailbox.agentEmail);
    remoteMutationAttempted = true;
    await setup.replaceProtection(policy.mailbox.agentEmail, nextProtection);
    const verified = await setup.getProtection(policy.mailbox.agentEmail);
    if (!protectionsEqual(verified, nextProtection) || !protectionMatchesPolicy(verified, policy)) {
      throw new Error("e2a protection verification did not match the confirmed Autopilot policy.");
    }

    privateDirectory(paths.root);
    localCreated = true;
    privateDirectory(paths.stateRoot);
    privateDirectory(path.join(paths.stateRoot, "jobs"));
    privateDirectory(path.join(paths.stateRoot, "locks"));
    privateDirectory(paths.logsRoot);
    atomicWrite(
      path.join(paths.stateRoot, "reconcile.json"),
      `${JSON.stringify({ version: 1, since: preparation.preparedAt })}\n`,
    );
    copyRuntimeBundle(runtimeSourceRoot, paths.runtimeRoot);
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
          installedAt: preparation.preparedAt,
        },
        null,
        2,
      )}\n`,
    );

    if (paths.servicePath) {
      const serviceValues = {
        nodePath,
        runnerPath: paths.runnerPath,
        policyPath: paths.policyPath,
        secretsPath: paths.secretsPath,
        stateRoot: paths.stateRoot,
        stdoutPath: paths.stdoutPath,
        stderrPath: paths.stderrPath,
        pathValue: environment.PATH,
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
    if (remoteMutationAttempted && priorProtection && nextProtection) {
      try {
        const currentProtection = await setup.getProtection(policy.mailbox.agentEmail);
        if (protectionsEqual(currentProtection, nextProtection)) {
          await setup.replaceProtection(policy.mailbox.agentEmail, priorProtection);
        } else if (!protectionsEqual(currentProtection, priorProtection)) {
          rollbackErrors.push(
            "protection changed concurrently; refusing to overwrite the administrator's newer document",
          );
        }
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
  } finally {
    setup.clearAccountCredential?.();
  }
}
