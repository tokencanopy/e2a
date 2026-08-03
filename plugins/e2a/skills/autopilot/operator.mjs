import { execFileSync } from "node:child_process";
import {
  existsSync,
  readdirSync,
  readFileSync,
  renameSync,
  rmSync,
} from "node:fs";
import { homedir, userInfo } from "node:os";
import path from "node:path";

import { loadInstalledConfig } from "./config.mjs";
import { installationPaths } from "./installer.mjs";
import { normalizePolicy, validatePolicy } from "./policy.mjs";
import { serviceCommands } from "./service.mjs";

function readJson(file, label) {
  try {
    return JSON.parse(readFileSync(file, "utf8"));
  } catch (error) {
    throw new Error(`Cannot read ${label} at ${file}: ${error.message}`);
  }
}

export function loadInstallation({ agentEmail, home = homedir() }) {
  if (typeof agentEmail !== "string" || !agentEmail.trim()) {
    throw new Error("Provide the installed agent address with --agent.");
  }
  const seed = { mailbox: { agentEmail: agentEmail.trim().toLowerCase() }, service: {} };
  const seedPaths = installationPaths(seed, home);
  if (!existsSync(seedPaths.root)) {
    throw new Error(`Autopilot is not installed for ${agentEmail}.`);
  }
  const policy = normalizePolicy(readJson(seedPaths.policyPath, "Autopilot policy"));
  const errors = validatePolicy(policy);
  if (errors.length > 0) throw new Error(`Invalid installed Autopilot policy:\n- ${errors.join("\n- ")}`);
  if (policy.mailbox.agentEmail !== agentEmail.trim().toLowerCase()) {
    throw new Error("Installed policy mailbox does not match the requested agent.");
  }
  const paths = installationPaths(policy, home);
  const config = loadInstalledConfig({
    policyPath: paths.policyPath,
    secretsPath: paths.secretsPath,
    stateRoot: paths.stateRoot,
  });
  return { ...config, paths };
}

function countJobs(directory) {
  try {
    return readdirSync(directory, { withFileTypes: true }).filter(
      (entry) => entry.isFile() && entry.name.endsWith(".json"),
    ).length;
  } catch {
    return 0;
  }
}

export function localStatus(installed) {
  const jobs = {};
  for (const state of ["pending", "running", "retry", "done", "dead"]) {
    jobs[state] = countJobs(path.join(installed.paths.stateRoot, "jobs", state));
  }
  return {
    installed: true,
    agentEmail: installed.policy.mailbox.agentEmail,
    ownerEmail: installed.policy.mailbox.ownerEmail,
    manager: installed.policy.service.manager,
    root: installed.paths.root,
    servicePath: installed.paths.servicePath,
    jobs,
  };
}

export function controlService(
  installed,
  action,
  { execFileSyncImpl = execFileSync, uid = userInfo().uid, ignoreFailure = false } = {},
) {
  const manager = installed.policy.service.manager;
  if (manager === "foreground") {
    if (action === "status") return { manager, state: "manual" };
    throw new Error(`Foreground Autopilot has no ${action} service action; use \`autopilot run\`.`);
  }
  const commands = serviceCommands({
    manager,
    action,
    servicePath: installed.paths.servicePath,
    identity: installed.paths.identity,
    uid,
  });
  try {
    for (const [command, args] of commands) {
      execFileSyncImpl(command, args, { stdio: action === "status" ? "pipe" : "inherit" });
    }
    return { manager, state: action === "start" ? "running" : action === "stop" ? "stopped" : "running" };
  } catch (error) {
    if (ignoreFailure) return { manager, state: "not-running" };
    if (action === "status") return { manager, state: "not-running" };
    throw new Error(`${manager} ${action} failed.`);
  }
}

function archiveStamp(date) {
  return date.toISOString().replaceAll("-", "").replaceAll(":", "").replace(/\.\d{3}Z$/, "Z");
}

export async function uninstallAutopilot({
  agentEmail,
  confirmation,
  home = homedir(),
  setup,
  control = (installed, action) => controlService(installed, action, { ignoreFailure: true }),
  now = () => new Date(),
}) {
  if (confirmation !== "DELETE") {
    throw new Error("Uninstall requires --confirm DELETE.");
  }
  if (!setup) throw new Error("An e2a setup client is required for credential revocation.");
  const installed = loadInstallation({ agentEmail, home });
  const archivePath = `${installed.paths.root}.uninstalled-${archiveStamp(now())}`;
  if (existsSync(archivePath)) throw new Error(`Uninstall archive already exists at ${archivePath}.`);

  control(installed, "stop");
  try {
    await setup.preflight(installed.policy.mailbox.agentEmail);
    await setup.revokeKey(installed.secrets.keyId);
  } finally {
    setup.clearAccountCredential?.();
  }
  if (installed.paths.servicePath) rmSync(installed.paths.servicePath, { force: true });
  renameSync(installed.paths.root, archivePath);
  return {
    uninstalled: true,
    archivePath,
    protectionRestored: false,
    note: "e2a protection remains at the last configured posture to avoid overwriting later administrator changes.",
  };
}
