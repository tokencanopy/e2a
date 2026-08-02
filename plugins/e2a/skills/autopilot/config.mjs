import { readFileSync, statSync } from "node:fs";
import path from "node:path";

import { normalizePolicy, validatePolicy } from "./policy.mjs";

const SECRET_FIELDS = new Set([
  "version",
  "apiKey",
  "forwardToken",
  "cliCommand",
  "cliBaseArgs",
  "deploymentUrl",
  "apiBaseUrl",
]);

function readJson(file, label) {
  try {
    return JSON.parse(readFileSync(file, "utf8"));
  } catch (error) {
    throw new Error(`Cannot read ${label} at ${file}: ${error.message}`);
  }
}

function requirePrivateFile(file, label) {
  const mode = statSync(file).mode & 0o777;
  if (mode !== 0o600) {
    throw new Error(`${label} must have mode 0600; found ${mode.toString(8).padStart(4, "0")}.`);
  }
}

function requiredText(value, label) {
  if (typeof value !== "string" || !value.trim()) throw new Error(`${label} is required.`);
  return value.trim();
}

function httpOrigin(value, label) {
  const parsed = new URL(requiredText(value, label));
  if (!["http:", "https:"].includes(parsed.protocol) || parsed.username || parsed.password) {
    throw new Error(`${label} must be an HTTP(S) origin without embedded credentials.`);
  }
  return parsed.origin;
}

export function loadInstalledConfig({ policyPath, secretsPath, stateRoot }) {
  for (const [value, label] of [
    [policyPath, "Policy path"],
    [secretsPath, "Credential path"],
    [stateRoot, "State root"],
  ]) {
    if (!path.isAbsolute(value || "")) throw new Error(`${label} must be an absolute path.`);
  }

  requirePrivateFile(policyPath, "Policy file");
  requirePrivateFile(secretsPath, "Credential file");
  const policy = normalizePolicy(readJson(policyPath, "Autopilot policy"));
  const policyErrors = validatePolicy(policy);
  if (policyErrors.length > 0) {
    throw new Error(`Invalid Autopilot policy:\n- ${policyErrors.join("\n- ")}`);
  }

  const rawSecrets = readJson(secretsPath, "Autopilot credential file");
  if (!rawSecrets || typeof rawSecrets !== "object" || Array.isArray(rawSecrets)) {
    throw new Error("Autopilot credential file must contain a JSON object.");
  }
  for (const field of Object.keys(rawSecrets)) {
    if (!SECRET_FIELDS.has(field)) throw new Error(`Unknown credential field: ${field}.`);
  }
  if (rawSecrets.version !== 1) {
    throw new Error(`Unsupported credential file version: ${rawSecrets.version}.`);
  }
  const cliCommand = requiredText(rawSecrets.cliCommand, "CLI command");
  if (!path.isAbsolute(cliCommand)) throw new Error("CLI command must be an absolute path.");
  if (
    !Array.isArray(rawSecrets.cliBaseArgs) ||
    rawSecrets.cliBaseArgs.some((value) => typeof value !== "string")
  ) {
    throw new Error("CLI base arguments must be an array of strings.");
  }

  const secrets = {
    version: 1,
    apiKey: requiredText(rawSecrets.apiKey, "Agent-scoped e2a credential"),
    forwardToken: requiredText(rawSecrets.forwardToken, "Forward capability"),
    cliCommand,
    cliBaseArgs: [...rawSecrets.cliBaseArgs],
    deploymentUrl: httpOrigin(rawSecrets.deploymentUrl, "e2a deployment URL"),
    apiBaseUrl: httpOrigin(rawSecrets.apiBaseUrl, "e2a API URL"),
  };

  return {
    policy,
    secrets,
    cli: { command: secrets.cliCommand, baseArgs: secrets.cliBaseArgs },
    stateRoot,
    policyPath,
    secretsPath,
  };
}
