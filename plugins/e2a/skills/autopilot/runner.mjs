#!/usr/bin/env node

import path from "node:path";
import { fileURLToPath } from "node:url";

import { loadInstalledConfig } from "./config.mjs";
import { AutopilotDaemon } from "./daemon.mjs";
import { E2aMailClient } from "./mail-client.mjs";
import { JobSpool } from "./spool.mjs";
import { AutopilotSupervisor } from "./supervisor.mjs";
import { acquireSupervisorLock } from "./lock.mjs";

const directory = path.dirname(fileURLToPath(import.meta.url));

export function createInstalledDaemon({ environment = process.env, log } = {}) {
  const installed = loadInstalledConfig({
    policyPath: requiredEnvironmentFrom(environment, "E2A_AUTOPILOT_POLICY_PATH"),
    secretsPath: requiredEnvironmentFrom(environment, "E2A_AUTOPILOT_SECRETS_PATH"),
    stateRoot: requiredEnvironmentFrom(environment, "E2A_AUTOPILOT_STATE_ROOT"),
  });
  const spool = new JobSpool(path.join(installed.stateRoot, "jobs"));
  const mail = new E2aMailClient({
    baseUrl: installed.secrets.apiBaseUrl,
    apiKey: installed.secrets.apiKey,
    agentEmail: installed.policy.mailbox.agentEmail,
  });
  const supervisor = new AutopilotSupervisor({
    policy: installed.policy,
    spool,
    mail,
    stateRoot: installed.stateRoot,
    helperPath: path.join(directory, "job-tool.mjs"),
  });
  return new AutopilotDaemon({
    policy: installed.policy,
    secrets: installed.secrets,
    spool,
    supervisor,
    cli: installed.cli,
    reconcileStatePath: path.join(installed.stateRoot, "reconcile.json"),
    environment,
    log,
  });
}

function requiredEnvironmentFrom(environment, name) {
  const value = environment[name];
  if (!value) throw new Error(`${name} is required.`);
  if (!path.isAbsolute(value)) throw new Error(`${name} must be an absolute path.`);
  return value;
}

export async function runInstalledDaemon({ environment = process.env } = {}) {
  const log = (message) => {
    process.stdout.write(`${new Date().toISOString()} ${message}\n`);
  };
  const stateRoot = requiredEnvironmentFrom(environment, "E2A_AUTOPILOT_STATE_ROOT");
  const lifetimeLock = acquireSupervisorLock(stateRoot);
  let daemon;
  try {
    daemon = createInstalledDaemon({ environment, log });
  } catch (error) {
    lifetimeLock.release();
    throw error;
  }
  const stopDaemon = daemon.stop.bind(daemon);
  daemon.stop = async () => {
    try {
      await stopDaemon();
    } finally {
      lifetimeLock.release();
    }
  };
  let stopping = false;
  const stop = async (signal) => {
    if (stopping) return;
    stopping = true;
    log(`received ${signal}; stopping`);
    await daemon.stop();
  };
  process.once("SIGTERM", () => void stop("SIGTERM"));
  process.once("SIGINT", () => void stop("SIGINT"));
  try {
    await daemon.start();
  } catch (error) {
    await daemon.stop();
    throw error;
  }
  log("autopilot supervisor started");
  return daemon;
}

const isDirect =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isDirect) {
  runInstalledDaemon().catch((error) => {
    process.stderr.write(`autopilot: ${error.message}\n`);
    process.exitCode = 1;
  });
}
