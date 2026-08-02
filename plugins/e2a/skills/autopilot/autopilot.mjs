#!/usr/bin/env node

import { randomUUID } from "node:crypto";
import {
  chmodSync,
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

import {
  answerQuestion,
  buildPolicyFromInterview,
  createInterview,
  nextQuestion,
} from "./interview.mjs";
import { planDigest, renderPlan } from "./policy.mjs";

function parseArgs(argv) {
  const [command, ...rest] = argv;
  const options = {};
  for (let index = 0; index < rest.length; index += 1) {
    const flag = rest[index];
    if (!flag.startsWith("--")) throw new Error(`Unexpected argument: ${flag}`);
    const value = rest[index + 1];
    if (!value || value.startsWith("--")) {
      throw new Error(`Missing value for ${flag}.`);
    }
    options[flag.slice(2)] = value;
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
  const digest = planDigest(policy);

  console.log(`\n${plan}`);
  console.log(`\nPlan confirmation digest: ${digest}`);
  console.log(`Draft policy: ${policyFile}`);
  console.log("No changes have been applied to e2a, the server, or a service.");
}

function commandPlan(options) {
  const policyFile = path.resolve(options.policy || path.join(defaultRoot(), "policy.json"));
  const policy = readJson(policyFile);
  console.log(renderPlan(policy));
  console.log(`\nPlan confirmation digest: ${planDigest(policy)}`);
  console.log("No changes have been applied.");
}

function usage() {
  return [
    "usage:",
    "  autopilot.mjs interview [--state PATH] [--policy PATH] [--platform NAME]",
    "  autopilot.mjs plan [--policy PATH]",
    "",
    "Both commands are local and non-mutating with respect to e2a configuration.",
  ].join("\n");
}

async function main() {
  const { command, options } = parseArgs(process.argv.slice(2));
  switch (command) {
    case "interview":
      await commandInterview(options);
      break;
    case "plan":
      commandPlan(options);
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
