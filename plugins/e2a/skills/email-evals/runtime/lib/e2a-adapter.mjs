import { createHash } from "node:crypto";
import { E2AClient } from "@e2a/sdk/v1";
import { EvalError } from "./errors.mjs";
import { NormalizationError, normalizeAddressSet, normalizeMailbox } from "./normalize.mjs";

const CAPABILITIES = Object.freeze([
  "message_action",
  "visible_recipients",
  "blind_recipients",
  "envelope_recipients",
  "thread_headers",
  "raw_mime",
  "attachment_hashes",
  "delivery_lifecycle",
]);

const TIMEOUTS = Object.freeze({ maxRetries: 2, maxElapsedMs: 15_000, timeoutMs: 10_000 });

class SerializableSet extends Set {
  toJSON() {
    return [...this];
  }
}

function configurationError(code, message) {
  return new EvalError("configuration_error", code, message);
}

function stableJson(value) {
  if (Array.isArray(value)) return `[${value.map(stableJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${stableJson(value[key])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function normalizeAddress(value, code = "invalid_mailbox") {
  try {
    return normalizeMailbox(value).address;
  } catch (error) {
    if (error instanceof NormalizationError) throw configurationError(code, "Invalid evaluation mailbox");
    throw error;
  }
}

function normalizeAddressList(value, code) {
  try {
    return normalizeAddressSet(value);
  } catch (error) {
    if (error instanceof NormalizationError) throw configurationError(code, "Invalid evaluation address set");
    throw error;
  }
}

function isNotFound(error) {
  return error?.status === 404 || error?.code === "not_found" || error?.code === "agent_not_found";
}

async function readOwnedAgent(client, email) {
  try {
    const agent = await client.agents.get(email);
    if (!agent || normalizeAddress(agent.email, "agent_identity_mismatch") !== email) {
      throw configurationError("agent_identity_mismatch", "Evaluation agent identity did not match");
    }
    return agent;
  } catch (error) {
    if (error instanceof EvalError) throw error;
    if (isNotFound(error)) throw configurationError("agent_not_found", "A dedicated evaluation agent is unavailable");
    throw new EvalError("transport_error", "agent_lookup_failed", "Unable to read dedicated evaluation agent");
  }
}

async function readProtection(client, email) {
  try {
    return await client.agents.getProtection(email);
  } catch {
    // The endpoint is account-scope only. Do not disclose the address or the
    // SDK/server error, either of which could distinguish account state.
    throw configurationError("account_scope_required", "Account-scoped access is required to read evaluation protection");
  }
}

function normalizeGate(document, gateCode) {
  const gate = document?.outbound?.gate;
  if (!gate || typeof gate !== "object") throw configurationError(gateCode, "Outbound recipient gate is not configured exactly");
  if (gate.policy !== "allowlist") throw configurationError(gateCode, "Outbound recipient gate is not configured exactly");
  if (gate.action !== "block") throw configurationError(gateCode.replace("not_exact", "not_blocking"), "Outbound recipient gate must block non-matches");
  return {
    policy: gate.policy,
    action: gate.action,
    allowlist: normalizeAddressList(gate.allowlist, gateCode),
  };
}

function sameSet(left, right) {
  return left.length === right.length && left.every((entry, index) => entry === right[index]);
}

function aliasesFor(actor, target, probes) {
  const aliases = new Map([[actor, "actor"], [target, "target"]]);
  probes.forEach((probe, index) => aliases.set(probe, `probe:${index + 1}`));
  return aliases;
}

function aliasOf(aliases, address) {
  const alias = aliases.get(address);
  if (!alias) throw configurationError("recipient_outside_allowlist", "Resolved recipient is outside the transport allowlist");
  return alias;
}

function exactlyAddresses(value, code = "invalid_suite") {
  if (!value || typeof value !== "object" || !Array.isArray(value.exactly)) {
    throw configurationError(code, "Expected exact recipient set");
  }
  return normalizeAddressList(value.exactly, code);
}

function collectCaseAddresses(testCase) {
  if (!testCase || typeof testCase !== "object" || typeof testCase.id !== "string" || !testCase.expect || typeof testCase.expect !== "object") {
    throw configurationError("invalid_suite", "Resolved evaluation case is invalid");
  }
  const action = testCase.expect.action;
  if (!action || typeof action !== "object" || typeof action.kind !== "string" || !Number.isSafeInteger(action.count) || action.count < 0) {
    throw configurationError("invalid_suite", "Resolved case action is invalid");
  }

  const addresses = [];
  const sender = testCase.expect.sender;
  if (sender !== undefined) {
    if (!sender || typeof sender !== "object") throw configurationError("invalid_suite", "Resolved case sender is invalid");
    for (const field of ["exactly", "sentAs"]) {
      if (sender[field] !== undefined) addresses.push(normalizeAddress(sender[field], "invalid_suite"));
    }
    if (sender.replyTo !== undefined) addresses.push(...exactlyAddresses(sender.replyTo));
  }
  const recipients = testCase.expect.recipients;
  if (recipients !== undefined) {
    if (!recipients || typeof recipients !== "object") throw configurationError("invalid_suite", "Resolved case recipients are invalid");
    for (const field of ["to", "cc", "bcc", "envelope"]) {
      if (recipients[field] !== undefined) addresses.push(...exactlyAddresses(recipients[field]));
    }
  }
  if (action.count > 0 && recipients?.envelope === undefined) {
    throw configurationError("missing_envelope_allowlist", "Outbound cases require an exact envelope expectation");
  }
  return { id: testCase.id, action: { kind: action.kind, count: action.count }, addresses: [...new Set(addresses)].sort() };
}

function makePlan({ baseUrl, aliases, allowedAliases, cases }) {
  return {
    baseUrl,
    recipientAliases: allowedAliases,
    capabilities: [...CAPABILITIES],
    timeouts: { ...TIMEOUTS },
    networkSends: false,
    cases: cases.map((testCase) => ({
      id: testCase.id,
      expectedAction: testCase.action,
      recipientAliases: testCase.addresses.map((address) => aliasOf(aliases, address)),
    })),
  };
}

/**
 * Creates the e2a transport adapter. Its first operation is deliberately a
 * read-only containment preflight; no protection mutation or mail send lives
 * on this path.
 */
export function createE2AAdapter({ apiKey, baseUrl, client, now = () => Date.now(), sleep = () => Promise.resolve() }) {
  // Keep these seams available for the later polling implementation without
  // exposing credentials in the serializable adapter result.
  void now;
  void sleep;
  const sdk = client ?? new E2AClient({ apiKey, baseUrl, ...TIMEOUTS });
  const capabilities = new SerializableSet(CAPABILITIES);

  return Object.freeze({
    capabilities,
    async preflight(resolvedSuite) {
      const actor = normalizeAddress(resolvedSuite?.actor?.email);
      const target = normalizeAddress(resolvedSuite?.target?.email);
      if (actor === target) throw configurationError("same_actor_target", "Actor and target must differ");

      const allowed = normalizeAddressList(resolvedSuite?.transport?.allowedEnvelopeRecipients, "invalid_allowlist");
      const allowedSet = new Set(allowed);
      if (!allowedSet.has(actor) || !allowedSet.has(target)) {
        throw configurationError("invalid_allowlist", "Allowlist must include actor and target");
      }
      const probes = allowed.filter((address) => address !== actor && address !== target);
      const aliases = aliasesFor(actor, target, probes);
      const cases = Array.isArray(resolvedSuite?.cases) ? resolvedSuite.cases.map(collectCaseAddresses) : (() => {
        throw configurationError("invalid_suite", "Resolved evaluation cases are invalid");
      })();
      for (const testCase of cases) {
        for (const address of testCase.addresses) aliasOf(aliases, address);
      }

      await readOwnedAgent(sdk, actor);
      await readOwnedAgent(sdk, target);
      const actorProtection = normalizeGate(await readProtection(sdk, actor), "actor_gate_not_exact");
      const targetProtection = normalizeGate(await readProtection(sdk, target), "target_gate_not_exact");
      if (!sameSet(actorProtection.allowlist, [target])) {
        throw configurationError("actor_gate_not_exact", "Actor outbound gate must contain only the target");
      }
      const expectedTargetGate = [actor, ...probes].sort();
      if (!sameSet(targetProtection.allowlist, expectedTargetGate)) {
        throw configurationError("target_gate_not_exact", "Target outbound gate must match the evaluation allowlist");
      }
      for (const probe of probes) await readOwnedAgent(sdk, probe);

      const aliasedActorGate = actorProtection.allowlist.map((address) => aliasOf(aliases, address));
      const aliasedTargetGate = targetProtection.allowlist.map((address) => aliasOf(aliases, address));
      const protectionDigest = createHash("sha256").update(stableJson({
        actor: { policy: actorProtection.policy, action: actorProtection.action, allowlist: aliasedActorGate },
        target: { policy: targetProtection.policy, action: targetProtection.action, allowlist: aliasedTargetGate },
      })).digest("hex");
      const allowedAliases = allowed.map((address) => aliasOf(aliases, address));

      return {
        capabilities,
        actor: { email: "actor" },
        target: { email: "target" },
        probes: probes.map((address) => aliasOf(aliases, address)),
        protectionDigest,
        plan: makePlan({ baseUrl, aliases, allowedAliases, cases }),
      };
    },
  });
}

export const E2A_ADAPTER_CAPABILITIES = Object.freeze([...CAPABILITIES]);
