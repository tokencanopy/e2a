import { createHash } from "node:crypto";
import { homedir } from "node:os";
import path from "node:path";

import { serviceIdentity } from "./service.mjs";

export const POLICY_VERSION = 1;

const SUPPORTED_PROFILES = new Set(["customer-support", "custom"]);
const SUPPORTED_RUNTIMES = new Set([
  "claude",
  "codex",
  "openclaw",
  "hermes",
  "custom",
]);
const SUPPORTED_SANDBOXES = new Set(["custom"]);
const SUPPORTED_SERVICES = new Set(["launchd", "systemd", "foreground"]);
const SUPPORTED_INBOUND_MODES = new Set(["addresses", "domains"]);
const SUPPORTED_REPLY_MODES = new Set(["submit-for-review", "draft-only"]);
const OPT_OUTS = new Set([
  "outbound_review_opt_out",
  "owner_cc_opt_out",
  "screening_opt_out",
  "custom_sandbox_acknowledged",
]);

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
const DOMAIN_RE = /^(?=.{1,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

// The OpenClaw adapter ships disabled until its invocation flags are verified
// against a local installation (see runtime.mjs OPENCLAW_FLAGS_VERIFIED).
const UNAVAILABLE_RUNTIMES = new Map([
  ["openclaw", "The OpenClaw adapter is unavailable in this release: its invocation flags are unverified. Choose claude, codex, hermes, or a custom runtime."],
]);

const DEFAULT_LIMITS = {
  maxAttempts: 3,
  retryBaseDelayMs: 1_000,
  runtimeTimeoutMs: 5 * 60 * 1_000,
  bounceIntervalMs: 15 * 60 * 1_000,
  reconcileIntervalMs: 10 * 60 * 1_000,
};

function text(value) {
  return typeof value === "string" ? value.trim() : "";
}

function lowercase(value) {
  return text(value).toLowerCase();
}

function uniqueSorted(values, transform = text) {
  if (!Array.isArray(values)) return [];
  return [...new Set(values.map(transform).filter(Boolean))].sort();
}

function normalizeLimit(value, fallback) {
  if (value === undefined || value === null) return fallback;
  const number = Number(value);
  return Number.isFinite(number) ? Math.floor(number) : Number.NaN;
}

function normalizeLimits(input = {}) {
  return {
    maxAttempts: normalizeLimit(input.maxAttempts, DEFAULT_LIMITS.maxAttempts),
    retryBaseDelayMs: normalizeLimit(input.retryBaseDelayMs, DEFAULT_LIMITS.retryBaseDelayMs),
    runtimeTimeoutMs: normalizeLimit(input.runtimeTimeoutMs, DEFAULT_LIMITS.runtimeTimeoutMs),
    bounceIntervalMs: normalizeLimit(input.bounceIntervalMs, DEFAULT_LIMITS.bounceIntervalMs),
    reconcileIntervalMs: normalizeLimit(input.reconcileIntervalMs, DEFAULT_LIMITS.reconcileIntervalMs),
  };
}

export function createPolicy() {
  return {
    version: POLICY_VERSION,
    task: {
      profile: "customer-support",
      objective: "",
      instructions: "",
      replyMode: "submit-for-review",
    },
    mailbox: {
      agentEmail: "",
      ownerEmail: "",
    },
    inbound: {
      mode: "addresses",
      addresses: [],
      domains: [],
      fallback: "review",
    },
    outbound: {
      requireReview: true,
      ccOwner: true,
    },
    screening: {
      promptInjection: true,
    },
    runtime: {
      adapter: "claude",
      command: "",
      workdir: "",
      sandbox: "custom",
    },
    limits: { ...DEFAULT_LIMITS },
    service: {
      manager: process.platform === "darwin" ? "launchd" : "systemd",
    },
    acknowledgements: [],
  };
}

export function normalizePolicy(input = {}) {
  const defaults = createPolicy();
  const inboundAddresses = uniqueSorted(input.inbound?.addresses, lowercase);
  const inboundDomains = uniqueSorted(input.inbound?.domains, lowercase);
  const policy = {
    version: Number(input.version ?? defaults.version),
    task: {
      profile: lowercase(input.task?.profile || defaults.task.profile),
      objective: text(input.task?.objective),
      instructions: text(input.task?.instructions),
      replyMode: lowercase(input.task?.replyMode || defaults.task.replyMode),
    },
    mailbox: {
      agentEmail: lowercase(input.mailbox?.agentEmail),
      ownerEmail: lowercase(input.mailbox?.ownerEmail),
    },
    inbound: {
      mode: lowercase(
        input.inbound?.mode ||
          (inboundDomains.length > 0 && inboundAddresses.length === 0
            ? "domains"
            : defaults.inbound.mode),
      ),
      addresses: inboundAddresses,
      domains: inboundDomains,
      fallback: lowercase(input.inbound?.fallback || defaults.inbound.fallback),
    },
    outbound: {
      requireReview:
        input.outbound?.requireReview ?? defaults.outbound.requireReview,
      ccOwner: input.outbound?.ccOwner ?? defaults.outbound.ccOwner,
    },
    screening: {
      promptInjection:
        input.screening?.promptInjection ?? defaults.screening.promptInjection,
    },
    runtime: {
      adapter: lowercase(input.runtime?.adapter || defaults.runtime.adapter),
      command: text(input.runtime?.command),
      workdir: text(input.runtime?.workdir),
      sandbox: lowercase(input.runtime?.sandbox || defaults.runtime.sandbox),
    },
    limits: normalizeLimits(input.limits),
    service: {
      manager: lowercase(input.service?.manager || defaults.service.manager),
    },
    acknowledgements: uniqueSorted(input.acknowledgements, lowercase),
  };
  return policy;
}

export function validatePolicy(input) {
  const policy = normalizePolicy(input);
  const errors = [];

  if (policy.version !== POLICY_VERSION) {
    errors.push(`Unsupported policy version ${policy.version}; expected ${POLICY_VERSION}.`);
  }
  if (!SUPPORTED_PROFILES.has(policy.task.profile)) {
    errors.push(`Unsupported task profile: ${policy.task.profile || "(missing)"}.`);
  }
  if (!policy.task.objective) {
    errors.push("Describe the task outcome the agent should produce.");
  }
  if (!SUPPORTED_REPLY_MODES.has(policy.task.replyMode)) {
    errors.push(`Unsupported reply mode: ${policy.task.replyMode || "(missing)"}.`);
  }
  if (!EMAIL_RE.test(policy.mailbox.agentEmail)) {
    errors.push("Provide a valid e2a agent mailbox address.");
  }
  if (!EMAIL_RE.test(policy.mailbox.ownerEmail)) {
    errors.push("Provide a valid owner email address.");
  }
  for (const address of policy.inbound.addresses) {
    if (!EMAIL_RE.test(address)) {
      errors.push(`Invalid authorized sender address: ${address}.`);
    }
  }
  for (const domain of policy.inbound.domains) {
    if (!DOMAIN_RE.test(domain)) {
      errors.push(`Invalid authorized sender domain: ${domain}.`);
    }
  }
  if (!SUPPORTED_INBOUND_MODES.has(policy.inbound.mode)) {
    errors.push(`Unsupported inbound authorization mode: ${policy.inbound.mode || "(missing)"}.`);
  }
  if (policy.inbound.addresses.length + policy.inbound.domains.length === 0) {
    errors.push(
      "Add at least one authorized sender address or domain; public-any-sender mode is not supported.",
    );
  } else if (policy.inbound.mode === "addresses" && policy.inbound.domains.length > 0) {
    errors.push(
      "Address authorization mode cannot include domain entries; choose one mode per inbox.",
    );
  } else if (policy.inbound.mode === "domains" && policy.inbound.addresses.length > 0) {
    errors.push(
      "Domain authorization mode cannot include exact-address entries; choose one mode per inbox.",
    );
  }
  if (policy.inbound.fallback !== "review") {
    errors.push("Inbound fallback must be e2a human review in this release.");
  }
  if (typeof policy.outbound.requireReview !== "boolean") {
    errors.push("Outbound review must be enabled or explicitly disabled.");
  }
  if (typeof policy.outbound.ccOwner !== "boolean") {
    errors.push("Owner CC must be enabled or explicitly disabled.");
  }
  if (typeof policy.screening.promptInjection !== "boolean") {
    errors.push("Prompt-injection screening must be enabled or explicitly disabled.");
  }
  const acknowledgements = new Set(policy.acknowledgements);
  if (!policy.outbound.requireReview && !acknowledgements.has("outbound_review_opt_out")) {
    errors.push(
      "Outbound review is disabled; explicitly acknowledge outbound_review_opt_out.",
    );
  }
  if (!policy.outbound.ccOwner && !acknowledgements.has("owner_cc_opt_out")) {
    errors.push("Owner CC is disabled; explicitly acknowledge owner_cc_opt_out.");
  }
  if (!policy.screening.promptInjection && !acknowledgements.has("screening_opt_out")) {
    errors.push(
      "Prompt-injection screening is disabled; explicitly acknowledge screening_opt_out.",
    );
  }
  if (!SUPPORTED_RUNTIMES.has(policy.runtime.adapter)) {
    errors.push(`Unsupported runtime adapter: ${policy.runtime.adapter || "(missing)"}.`);
  } else if (UNAVAILABLE_RUNTIMES.has(policy.runtime.adapter)) {
    errors.push(UNAVAILABLE_RUNTIMES.get(policy.runtime.adapter));
  }
  const limitBounds = {
    maxAttempts: [1, 10],
    retryBaseDelayMs: [1, 86_400_000],
    runtimeTimeoutMs: [10_000, 3_600_000],
    bounceIntervalMs: [60_000, 86_400_000],
    reconcileIntervalMs: [60_000, 86_400_000],
  };
  for (const [field, [minimum, maximum]] of Object.entries(limitBounds)) {
    const value = policy.limits[field];
    if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
      errors.push(`limits.${field} must be an integer between ${minimum} and ${maximum}.`);
    }
  }
  if (!policy.runtime.command) {
    errors.push("Provide the absolute runtime command path.");
  } else if (!policy.runtime.command.startsWith("/")) {
    errors.push("Runtime command must be an absolute path.");
  }
  if (!policy.runtime.workdir) {
    errors.push("Provide the absolute task workspace path.");
  } else if (!policy.runtime.workdir.startsWith("/")) {
    errors.push("Task workspace must be an absolute path.");
  }
  if (!SUPPORTED_SANDBOXES.has(policy.runtime.sandbox)) {
    errors.push(`Unsupported sandbox declaration: ${policy.runtime.sandbox || "(missing)"}.`);
  }
  if (
    policy.runtime.sandbox === "custom" &&
    !acknowledgements.has("custom_sandbox_acknowledged")
  ) {
    errors.push(
      "Custom isolation is not verified; explicitly acknowledge custom_sandbox_acknowledged.",
    );
  }
  if (!SUPPORTED_SERVICES.has(policy.service.manager)) {
    errors.push(`Unsupported service manager: ${policy.service.manager || "(missing)"}.`);
  }
  for (const acknowledgement of acknowledgements) {
    if (!OPT_OUTS.has(acknowledgement)) {
      errors.push(`Unknown acknowledgement: ${acknowledgement}.`);
    }
  }

  return errors;
}

function enabled(value) {
  return value ? "enabled (recommended default)" : "DISABLED by warned opt-out";
}

function installationContext(input) {
  if (!input || typeof input !== "object") {
    throw new Error("A resolved installation context is required for confirmation.");
  }
  const cliCommand = text(input.cliCommand);
  const cliBaseArgs = Array.isArray(input.cliBaseArgs)
    ? input.cliBaseArgs.map((value) => String(value))
    : null;
  const deploymentUrl = text(input.deploymentUrl);
  const apiBaseUrl = text(input.apiBaseUrl);
  if (
    !cliCommand.startsWith("/") ||
    !cliBaseArgs ||
    !deploymentUrl ||
    !apiBaseUrl ||
    !input.priorProtection ||
    !input.nextProtection
  ) {
    throw new Error("The resolved installation context is incomplete.");
  }
  return {
    cliCommand,
    cliBaseArgs,
    deploymentUrl,
    apiBaseUrl,
    priorProtection: input.priorProtection,
    nextProtection: input.nextProtection,
  };
}

export function renderPlan(input, resolvedInstallation = null) {
  const policy = normalizePolicy(input);
  const errors = validatePolicy(policy);
  if (errors.length > 0) {
    throw new Error(`Cannot render an invalid Autopilot policy:\n- ${errors.join("\n- ")}`);
  }

  const addresses = policy.inbound.addresses.length
    ? policy.inbound.addresses.join(", ")
    : "none";
  const domains = policy.inbound.domains.length
    ? policy.inbound.domains.join(", ")
    : "none";
  const identity = serviceIdentity(policy.mailbox.agentEmail);
  const localRoot = path.join(
    homedir(),
    ".local",
    "share",
    "e2a-autopilot",
    identity.slug,
  );
  const serviceDefinition =
    policy.service.manager === "launchd"
      ? path.join(homedir(), "Library", "LaunchAgents", `${identity.launchdLabel}.plist`)
      : policy.service.manager === "systemd"
        ? path.join(homedir(), ".config", "systemd", "user", identity.systemdUnit)
        : "none (foreground/manual mode)";
  const inboundGate = policy.inbound.mode === "domains" ? "domain" : "allowlist";
  const resolved = resolvedInstallation
    ? installationContext(resolvedInstallation)
    : null;

  return [
    "e2a Autopilot installation plan",
    "",
    `Task profile: ${policy.task.profile}`,
    `Task outcome: ${policy.task.objective}`,
    `Reply capability: ${policy.task.replyMode}`,
    "Task instructions:",
    policy.task.instructions || "(none)",
    `Agent mailbox: ${policy.mailbox.agentEmail}`,
    `Owner: ${policy.mailbox.ownerEmail}`,
    `Inbound authorization mode: ${policy.inbound.mode}`,
    `Authorized sender addresses: ${addresses}`,
    `Authorized sender domains: ${domains}`,
    "Non-matching inbound senders: e2a human review",
    `Outbound human review: ${enabled(policy.outbound.requireReview)}`,
    policy.outbound.ccOwner
      ? `Owner CC: ${policy.mailbox.ownerEmail} on every reply`
      : "Owner CC: DISABLED by warned opt-out",
    `Prompt-injection screening: ${enabled(policy.screening.promptInjection)}`,
    `Runtime: ${policy.runtime.adapter} (${policy.runtime.command})`,
    `Isolation: ${policy.runtime.sandbox}`,
    `Workspace: ${policy.runtime.workdir}`,
    `Service: ${policy.service.manager}`,
    `Local root: ${localRoot}`,
    `Policy file: ${path.join(localRoot, "policy.json")}`,
    `Task file: ${path.join(localRoot, "task.md")}`,
    `Credential file (mode 0600): ${path.join(localRoot, "secrets.json")}`,
    `Pinned runtime harness: ${path.join(localRoot, "runtime")}`,
    `Durable state: ${path.join(localRoot, "state")}`,
    `Logs: ${path.join(localRoot, "logs")}`,
    `Service definition: ${serviceDefinition}`,
    ...(resolved
      ? [
          `Setup CLI: ${resolved.cliCommand} ${resolved.cliBaseArgs.join(" ")}`.trim(),
          `CLI deployment origin: ${resolved.deploymentUrl}`,
          `Protection API origin: ${resolved.apiBaseUrl}`,
          `Protection before: ${JSON.stringify(resolved.priorProtection)}`,
          `Protection after: ${JSON.stringify(resolved.nextProtection)}`,
        ]
      : [
          "Setup CLI and protection changes: unresolved (run `autopilot plan` for an installable confirmation digest)",
        ]),
    "",
    "Planned changes:",
    "- Create the owner-only files at the exact paths above.",
    "- Create one dedicated agent-scoped e2a credential for the local supervisor.",
    `- Set inbound.gate to policy=${inboundGate}, action=review, entries=${
      policy.inbound.mode === "domains" ? domains : addresses
    }.`,
    `- Set inbound.scan.sensitivity=${
      policy.screening.promptInjection ? "medium" : "off (warned opt-out)"
    }.`,
    policy.outbound.requireReview
      ? "- Set outbound.gate to policy=allowlist, action=review, entries=none (review every outbound message)."
      : "- Set outbound.gate.action=flag and outbound.scan.sensitivity=off as explicitly acknowledged.",
    "- Set holds.on_expiry=reject and holds.suppress_notifications=false.",
    `- Generate the ${policy.service.manager} service definition after remote verification; do not start it.`,
    "- If installation fails, restore the prior protection document, revoke the new key, and remove only this attempt's files.",
    "",
    "Implementation boundary:",
    "- No server, API, database, core CLI, SDK, or MCP code changes.",
    "- The job gateway never gives the task runtime an e2a credential or a mailbox list, search, or delete operation.",
    "- Installation requires an operator-supplied external isolation boundary; Autopilot records the acknowledgement but cannot verify that boundary.",
    "- Without a correctly configured container, VM, or separate-user boundary, a same-user runtime may inspect local process state or owner-readable files, including Autopilot credentials.",
    "- Owner CC is enforced by the local gateway, not the e2a server.",
    "- Later account-level protection changes cannot be detected continuously.",
  ].join("\n");
}

export function planDigest(input, resolvedInstallation) {
  const policy = normalizePolicy(input);
  const errors = validatePolicy(policy);
  if (errors.length > 0) {
    throw new Error(`Cannot confirm an invalid Autopilot policy:\n- ${errors.join("\n- ")}`);
  }
  const resolved = installationContext(resolvedInstallation);
  return createHash("sha256")
    .update(JSON.stringify({ policy, installation: resolved }), "utf8")
    .digest("hex");
}
