import { execFile } from "node:child_process";

const MAX_SETUP_RESPONSE_BYTES = 1024 * 1024;

function copyDirection(value = {}) {
  return {
    gate: {
      policy: value.gate?.policy ?? "open",
      allowlist: [...(value.gate?.allowlist ?? [])],
      action: value.gate?.action ?? "flag",
    },
    scan: { sensitivity: value.scan?.sensitivity ?? "off" },
  };
}

export function normalizeProtection(value = {}) {
  const holds = value.holds ?? {};
  return {
    inbound: copyDirection(value.inbound),
    outbound: copyDirection(value.outbound),
    holds: {
      ttl_seconds: holds.ttl_seconds ?? holds.ttlSeconds ?? 604800,
      on_expiry: holds.on_expiry ?? holds.onExpiry ?? "reject",
      suppress_notifications:
        holds.suppress_notifications ?? holds.suppressNotifications ?? false,
    },
  };
}

export function buildProtectionDocument(currentValue, policy) {
  const current = normalizeProtection(currentValue);
  const inboundEntries =
    policy.inbound.mode === "domains"
      ? [...policy.inbound.domains]
      : [...policy.inbound.addresses];
  const outbound = copyDirection(current.outbound);

  if (policy.outbound.requireReview) {
    outbound.gate = { policy: "allowlist", allowlist: [], action: "review" };
  } else {
    if (outbound.gate.action === "review") outbound.gate.action = "flag";
  }

  return {
    inbound: {
      gate: {
        policy: policy.inbound.mode === "domains" ? "domain" : "allowlist",
        allowlist: inboundEntries,
        action: "review",
      },
      scan: {
        sensitivity: policy.screening.promptInjection ? "medium" : "off",
      },
    },
    outbound,
    holds: {
      ttl_seconds: current.holds.ttl_seconds,
      on_expiry: "reject",
      suppress_notifications: false,
    },
  };
}

function sameList(left, right) {
  return (
    Array.isArray(left) &&
    Array.isArray(right) &&
    left.length === right.length &&
    left.every((value, index) => value === right[index])
  );
}

export function protectionMatchesPolicy(value, policy) {
  const actual = normalizeProtection(value);
  const inboundPolicy = policy.inbound.mode === "domains" ? "domain" : "allowlist";
  const inboundEntries =
    policy.inbound.mode === "domains" ? policy.inbound.domains : policy.inbound.addresses;
  if (
    actual.inbound.gate.policy !== inboundPolicy ||
    actual.inbound.gate.action !== "review" ||
    !sameList(actual.inbound.gate.allowlist, inboundEntries) ||
    actual.inbound.scan.sensitivity !==
      (policy.screening.promptInjection ? "medium" : "off") ||
    actual.holds.on_expiry !== "reject" ||
    actual.holds.suppress_notifications !== false
  ) {
    return false;
  }
  if (policy.outbound.requireReview) {
    return (
      actual.outbound.gate.policy === "allowlist" &&
      actual.outbound.gate.action === "review" &&
      sameList(actual.outbound.gate.allowlist, [])
    );
  }
  return (
    actual.outbound.gate.action !== "review"
  );
}

export function protectionsEqual(left, right) {
  return JSON.stringify(normalizeProtection(left)) === JSON.stringify(normalizeProtection(right));
}

function parseJson(output, label) {
  try {
    return JSON.parse(String(output));
  } catch {
    throw new Error(`${label} returned invalid JSON.`);
  }
}

function origin(value, label) {
  let parsed;
  try {
    parsed = new URL(String(value).trim());
  } catch {
    throw new Error(`${label} is not a valid URL.`);
  }
  const loopback = ["localhost", "127.0.0.1", "[::1]"].includes(parsed.hostname);
  if (
    (parsed.protocol !== "https:" && !(parsed.protocol === "http:" && loopback)) ||
    parsed.username ||
    parsed.password
  ) {
    throw new Error(`${label} must use HTTPS (HTTP is allowed only for loopback development).`);
  }
  return parsed.origin;
}

async function boundedJson(response, label) {
  const declared = Number(response.headers?.get?.("content-length"));
  if (Number.isFinite(declared) && declared > MAX_SETUP_RESPONSE_BYTES) {
    throw new Error(`${label} response is too large.`);
  }
  if (response.body?.getReader) {
    const reader = response.body.getReader();
    const chunks = [];
    let size = 0;
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > MAX_SETUP_RESPONSE_BYTES) {
        await reader.cancel();
        throw new Error(`${label} response is too large.`);
      }
      chunks.push(Buffer.from(value));
    }
    try {
      return JSON.parse(Buffer.concat(chunks).toString("utf8"));
    } catch {
      throw new Error(`${label} returned invalid JSON.`);
    }
  }
  try {
    return await response.json();
  } catch {
    throw new Error(`${label} returned invalid JSON.`);
  }
}

export class E2aSetupClient {
  constructor({
    command,
    baseArgs = [],
    environment = process.env,
    execFileImpl = execFile,
    fetchImpl = globalThis.fetch,
    timeoutMs = 30_000,
  }) {
    this.command = command;
    this.baseArgs = [...baseArgs];
    this.environment = environment;
    this.execFileImpl = execFileImpl;
    this.fetchImpl = fetchImpl;
    this.timeoutMs = timeoutMs;
    this.accountApiKey = null;
    this.deploymentUrl = null;
    this.apiBaseUrl = null;
  }

  async run(args) {
    return new Promise((resolve, reject) => {
      this.execFileImpl(
        this.command,
        [...this.baseArgs, ...args],
        { env: this.environment, maxBuffer: 1024 * 1024, timeout: this.timeoutMs },
        (error, stdout) => {
          if (error) {
            reject(new Error(`e2a CLI command failed: ${args.slice(0, 2).join(" ")}.`));
            return;
          }
          resolve(String(stdout));
        },
      );
    });
  }

  async preflight(agentEmail) {
    const identity = parseJson(await this.run(["whoami", "--json"]), "e2a whoami");
    if (identity.scope !== "account") {
      throw new Error("Autopilot installation requires the current e2a CLI session to use an account-scoped credential.");
    }
    await this.run(["agents", "get", agentEmail, "--json"]);
    const deploymentUrl = origin(await this.run(["config", "get", "api_url"]), "e2a deployment URL");
    const accountApiKey = String(await this.run(["config", "get", "api_key"])).trim();
    if (!accountApiKey) throw new Error("The current e2a CLI session has no API credential.");
    this.deploymentUrl = deploymentUrl;
    this.apiBaseUrl = deploymentUrl;
    this.accountApiKey = accountApiKey;
    return {
      deploymentUrl: this.deploymentUrl,
      apiBaseUrl: this.apiBaseUrl,
      cliCommand: this.command,
      cliBaseArgs: [...this.baseArgs],
    };
  }

  async request(agentEmail, { method = "GET", body } = {}) {
    if (!this.accountApiKey || !this.apiBaseUrl) {
      throw new Error("Run setup preflight before accessing protection configuration.");
    }
    let response;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    try {
      try {
        response = await this.fetchImpl(
          `${this.apiBaseUrl}/v1/agents/${encodeURIComponent(agentEmail)}/protection`,
          {
            method,
            headers: {
              authorization: `Bearer ${this.accountApiKey}`,
              accept: "application/json",
              ...(body ? { "content-type": "application/json" } : {}),
            },
            ...(body ? { body: JSON.stringify(body) } : {}),
            signal: controller.signal,
          },
        );
      } catch {
        throw new Error("e2a protection request failed before a response.");
      }
      if (!response.ok) {
        throw new Error(`e2a protection request failed with status ${response.status}.`);
      }
      const value = await boundedJson(response, "e2a protection request");
      return normalizeProtection(value);
    } finally {
      clearTimeout(timer);
    }
  }

  getProtection(agentEmail) {
    return this.request(agentEmail);
  }

  replaceProtection(agentEmail, protection) {
    return this.request(agentEmail, { method: "PUT", body: normalizeProtection(protection) });
  }

  async createAgentKey(agentEmail) {
    const created = parseJson(
      await this.run([
        "keys",
        "create",
        "--agent",
        agentEmail,
        "--name",
        "e2a Autopilot local supervisor",
        "--json",
      ]),
      "e2a keys create",
    );
    if (created.scope !== "agent" || created.agentEmail && created.agentEmail !== agentEmail) {
      throw new Error("e2a created a credential with an unexpected scope or mailbox binding.");
    }
    if (!created.id || !created.key) {
      throw new Error("e2a keys create did not return the one-time credential and key ID.");
    }
    return { id: created.id, key: created.key };
  }

  async revokeKey(keyId) {
    await this.run(["keys", "delete", keyId]);
  }

  clearAccountCredential() {
    this.accountApiKey = null;
  }
}
