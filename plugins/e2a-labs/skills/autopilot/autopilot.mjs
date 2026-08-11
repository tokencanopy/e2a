#!/usr/bin/env node

import { randomUUID } from "node:crypto";
import {
  accessSync,
  chmodSync,
  constants,
  existsSync,
  mkdirSync,
  readFileSync,
  renameSync,
  writeFileSync,
} from "node:fs";
import { homedir } from "node:os";
import path from "node:path";
import { createInterface } from "node:readline/promises";
import { stdin, stdout } from "node:process";
import { delimiter } from "node:path";
import { spawnSync } from "node:child_process";

import {
  answerQuestion,
  buildPolicyFromInterview,
  createInterview,
  nextQuestion,
} from "./interview.mjs";
import { installAutopilot, prepareAutopilotInstall } from "./installer.mjs";
import {
  controlService,
  loadInstallation,
  localStatus,
  uninstallAutopilot,
} from "./operator.mjs";
import { planDigest, renderPlan, validatePolicy } from "./policy.mjs";
import { runInstalledDaemon } from "./runner.mjs";
import { E2aSetupClient, protectionMatchesPolicy } from "./setup.mjs";

const BOOLEAN_FLAGS = new Set(["verify", "follow", "json"]);

function parseArgs(argv) {
  const [command, ...rest] = argv;
  const options = {};
  for (let index = 0; index < rest.length; index += 1) {
    const flag = rest[index];
    if (!flag.startsWith("--")) throw new Error(`Unexpected argument: ${flag}`);
    const name = flag.slice(2);
    if (BOOLEAN_FLAGS.has(name)) {
      options[name] = true;
      continue;
    }
    const value = rest[index + 1];
    if (!value || value.startsWith("--")) {
      throw new Error(`Missing value for ${flag}.`);
    }
    options[name] = value;
    index += 1;
  }
  return { command, options };
}

function defaultRoot() {
  return path.join(homedir(), ".local", "share", "e2a-autopilot", "draft");
}

function ensurePrivateDirectory(directory) {
  mkdirSync(directory, { recursive: true, mode: 0o700 });
  chmodSync(directory, 0o700);
}

function writePrivateJson(file, value) {
  const directory = path.dirname(file);
  ensurePrivateDirectory(directory);
  const temporary = path.join(directory, `.${path.basename(file)}.${randomUUID()}.tmp`);
  writeFileSync(temporary, `${JSON.stringify(value, null, 2)}\n`, {
    encoding: "utf8",
    flag: "wx",
    mode: 0o600,
  });
  renameSync(temporary, file);
  chmodSync(file, 0o600);
}

function readJson(file) {
  try {
    return JSON.parse(readFileSync(file, "utf8"));
  } catch (error) {
    throw new Error(`Cannot read ${file}: ${error.message}`);
  }
}

async function commandInterview(options) {
  const stateFile = path.resolve(options.state || path.join(defaultRoot(), "interview.json"));
  const policyFile = path.resolve(options.policy || path.join(defaultRoot(), "policy.json"));
  let state = existsSync(stateFile)
    ? readJson(stateFile)
    : createInterview({ platform: options.platform || process.platform });

  console.log("e2a Autopilot policy interview");
  console.log("I will ask one topic at a time and save progress after each answer.");
  console.log("This command only writes a local draft; it does not change e2a or install a service.\n");

  const terminal = createInterface({ input: stdin, output: stdout });
  const lines = terminal[Symbol.asyncIterator]();
  try {
    while (true) {
      const question = nextQuestion(state);
      if (!question) break;
      let accepted = false;
      while (!accepted) {
        stdout.write(`${question.prompt}\n> `);
        const line = await lines.next();
        if (line.done) {
          throw new Error(`Interview stopped before ${question.id} was answered.`);
        }
        const raw = line.value;
        try {
          state = answerQuestion(state, raw);
          writePrivateJson(stateFile, state);
          accepted = true;
        } catch (error) {
          console.error(`Please try again: ${error.message}`);
        }
      }
    }
  } finally {
    terminal.close();
  }

  const policy = buildPolicyFromInterview(state);
  writePrivateJson(policyFile, policy);
  const plan = renderPlan(policy);

  console.log(`\n${plan}`);
  console.log(`Draft policy: ${policyFile}`);
  console.log("No changes have been applied. Run `autopilot plan` to resolve the setup origin and receive an installable confirmation digest.");
}

async function commandPlan(options) {
  const policyFile = path.resolve(options.policy || path.join(defaultRoot(), "policy.json"));
  const policy = readJson(policyFile);
  const errors = validatePolicy(policy);
  if (errors.length > 0) {
    throw new Error(`Cannot render an invalid Autopilot policy:\n- ${errors.join("\n- ")}`);
  }
  const setup = setupClient(options);
  try {
    const prepared = await prepareAutopilotInstall({ policy, setup });
    console.log(renderPlan(policy, prepared.context));
    console.log(`\nPlan confirmation digest: ${planDigest(policy, prepared.context)}`);
    console.log("No changes have been applied.");
  } finally {
    setup.clearAccountCredential();
  }
}

function resolveExecutable(value) {
  const command = value || process.env.E2A_CLI || "e2a";
  const candidates = path.isAbsolute(command)
    ? [command]
    : String(process.env.PATH || "")
        .split(delimiter)
        .filter(Boolean)
        .map((directory) => path.join(directory, command));
  for (const candidate of candidates) {
    try {
      accessSync(candidate, constants.X_OK);
      return path.resolve(candidate);
    } catch {
      // Continue searching PATH.
    }
  }
  throw new Error(`Cannot find executable e2a CLI: ${command}. Pass --e2a /absolute/path/to/e2a.`);
}

function setupClient(options) {
  if (options["api-url"]) {
    throw new Error("--api-url is not supported; Autopilot uses the exact origin from the authenticated e2a CLI profile.");
  }
  return new E2aSetupClient({
    command: resolveExecutable(options.e2a),
  });
}

function agentOption(options) {
  if (!options.agent) throw new Error("Provide the installed inbox with --agent ADDRESS.");
  return options.agent;
}

async function commandInstall(options) {
  const policyFile = path.resolve(options.policy || path.join(defaultRoot(), "policy.json"));
  const policy = readJson(policyFile);
  const setup = setupClient(options);
  let prepared;
  try {
    prepared = await prepareAutopilotInstall({ policy, setup });
    console.log(renderPlan(policy, prepared.context));
    console.log(`\nPlan confirmation digest: ${prepared.planDigest}`);
    if (!options.confirm) {
      throw new Error("Install is mutating. Re-run with --confirm <plan-digest> after reviewing the resolved plan.");
    }
    console.log("\nApplying the confirmed plan...");
  } catch (error) {
    setup.clearAccountCredential();
    throw error;
  }
  const result = await installAutopilot({
    policy,
    confirmation: options.confirm,
    setup,
    prepared,
  });
  console.log(`Installed private state: ${result.paths.root}`);
  if (result.paths.servicePath) console.log(`Generated service: ${result.paths.servicePath}`);
  console.log("The service was verified but not started. Run `autopilot start --agent <address>` when ready.");
}

function commandStart(options) {
  const installed = loadInstallation({ agentEmail: agentOption(options) });
  const result = controlService(installed, "start");
  console.log(`Autopilot service: ${result.state} (${result.manager}).`);
}

function commandStop(options) {
  const installed = loadInstallation({ agentEmail: agentOption(options) });
  const result = controlService(installed, "stop");
  console.log(`Autopilot service: ${result.state} (${result.manager}).`);
}

async function commandRun(options) {
  const installed = loadInstallation({ agentEmail: agentOption(options) });
  const environment = {
    ...process.env,
    E2A_AUTOPILOT_POLICY_PATH: installed.paths.policyPath,
    E2A_AUTOPILOT_SECRETS_PATH: installed.paths.secretsPath,
    E2A_AUTOPILOT_STATE_ROOT: installed.paths.stateRoot,
  };
  await runInstalledDaemon({ environment });
}

async function commandStatus(options) {
  const installed = loadInstallation({ agentEmail: agentOption(options) });
  const status = {
    ...localStatus(installed),
    service: controlService(installed, "status"),
  };
  if (options.verify) {
    const setup = setupClient(options);
    try {
      await setup.preflight(installed.policy.mailbox.agentEmail);
      const protection = await setup.getProtection(installed.policy.mailbox.agentEmail);
      status.protection = protectionMatchesPolicy(protection, installed.policy)
        ? "matches-policy"
        : "DRIFTED";
    } finally {
      setup.clearAccountCredential();
    }
  } else {
    status.protection = "not-verified (use --verify with an account-scoped CLI session)";
  }
  if (options.json) {
    console.log(JSON.stringify(status, null, 2));
    return;
  }
  console.log(`Autopilot ${status.agentEmail}`);
  console.log(`Service: ${status.service.state} (${status.manager})`);
  console.log(`Protection: ${status.protection}`);
  console.log(
    `Jobs: pending=${status.jobs.pending} running=${status.jobs.running} retry=${status.jobs.retry} done=${status.jobs.done} dead=${status.jobs.dead}`,
  );
  console.log(`State: ${status.root}`);
}

function commandLogs(options) {
  const installed = loadInstallation({ agentEmail: agentOption(options) });
  console.log(`stdout: ${installed.paths.stdoutPath}`);
  console.log(`stderr: ${installed.paths.stderrPath}`);
  if (options.follow) {
    const result = spawnSync("tail", ["-f", installed.paths.stdoutPath, installed.paths.stderrPath], {
      stdio: "inherit",
    });
    if (result.error) throw new Error("Cannot run tail to follow Autopilot logs.");
  }
}

async function commandUninstall(options) {
  const agentEmail = agentOption(options);
  console.log("e2a Autopilot uninstall plan");
  console.log(`- Stop the local service for ${agentEmail}.`);
  console.log("- Revoke its dedicated agent-scoped credential.");
  console.log("- Remove the service definition and archive local state beside the installation.");
  console.log("- Leave e2a protection unchanged to avoid overwriting later administrator changes.");
  const result = await uninstallAutopilot({
    agentEmail,
    confirmation: options.confirm,
    setup: setupClient(options),
  });
  console.log(`Uninstalled. Recoverable local archive: ${result.archivePath}`);
  console.log(result.note);
}

function usage() {
  return [
    "usage:",
    "  autopilot.mjs interview [--state PATH] [--policy PATH] [--platform NAME]",
    "  autopilot.mjs plan [--policy PATH] [--e2a PATH]",
    "  autopilot.mjs install [--policy PATH] --confirm DIGEST [--e2a PATH]",
    "  autopilot.mjs start --agent ADDRESS",
    "  autopilot.mjs stop --agent ADDRESS",
    "  autopilot.mjs run --agent ADDRESS",
    "  autopilot.mjs status --agent ADDRESS [--verify] [--json] [--e2a PATH]",
    "  autopilot.mjs logs --agent ADDRESS [--follow]",
    "  autopilot.mjs uninstall --agent ADDRESS --confirm DELETE [--e2a PATH]",
    "",
    "interview, plan, status, and logs are read-only. install and uninstall require exact confirmation.",
  ].join("\n");
}

async function main() {
  const { command, options } = parseArgs(process.argv.slice(2));
  switch (command) {
    case "interview":
      await commandInterview(options);
      break;
    case "plan":
      await commandPlan(options);
      break;
    case "install":
      await commandInstall(options);
      break;
    case "start":
      commandStart(options);
      break;
    case "stop":
      commandStop(options);
      break;
    case "run":
      await commandRun(options);
      break;
    case "status":
      await commandStatus(options);
      break;
    case "logs":
      commandLogs(options);
      break;
    case "uninstall":
      await commandUninstall(options);
      break;
    case "help":
    case "--help":
    case "-h":
    case undefined:
      console.log(usage());
      break;
    default:
      throw new Error(`Unknown command: ${command}\n\n${usage()}`);
  }
}

main().catch((error) => {
  console.error(`autopilot: ${error.message}`);
  process.exitCode = 1;
});
