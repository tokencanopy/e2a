import { addressParser } from "postal-mime";

const MAX_DURATION_MS = 24 * 60 * 60 * 1_000;
const durationPattern = /^(0|[1-9][0-9]*)(ms|s|m|h)$/;

export class NormalizationError extends Error {
  constructor(code, message = "Invalid normalized value") {
    super(message);
    this.name = "NormalizationError";
    this.code = code;
  }
}

function normalizedMailbox(entry) {
  if (!entry || typeof entry.address !== "string" || !entry.address || /[\r\n]/.test(entry.address)) {
    throw new NormalizationError("invalid_mailbox", "Invalid mailbox");
  }
  const address = entry.address.trim().toLowerCase();
  const at = address.lastIndexOf("@");
  if (at <= 0 || at === address.length - 1 || address.indexOf("@") !== at || /\s/.test(address)) {
    throw new NormalizationError("invalid_mailbox", "Invalid mailbox");
  }
  return { address, displayName: entry.name || undefined };
}

export function normalizeMailbox(value) {
  if (typeof value !== "string" || value.length === 0 || /[\r\n]/.test(value)) {
    throw new NormalizationError("invalid_mailbox", "Invalid mailbox");
  }
  const parsed = addressParser(value, { flatten: true });
  if (parsed.length !== 1) throw new NormalizationError("invalid_mailbox", "Expected exactly one mailbox");
  return normalizedMailbox(parsed[0]);
}

export function normalizeAddressSet(values) {
  if (!Array.isArray(values)) throw new NormalizationError("invalid_address_set", "Expected an address list");
  const source = values.map((value) => normalizeMailbox(value).address);
  const seen = new Set();
  for (const address of source) {
    if (seen.has(address)) throw new NormalizationError("duplicate_address", "Duplicate address");
    seen.add(address);
  }
  return [...seen].sort();
}

export function parseDuration(value) {
  if (typeof value !== "string") throw new NormalizationError("invalid_duration", "Invalid duration");
  const match = value.match(durationPattern);
  if (!match) throw new NormalizationError("invalid_duration", "Invalid duration");
  const amount = Number(match[1]);
  const scale = { ms: 1, s: 1_000, m: 60_000, h: 3_600_000 }[match[2]];
  const milliseconds = amount * scale;
  if (!Number.isSafeInteger(milliseconds) || milliseconds <= 0 || milliseconds > MAX_DURATION_MS) {
    throw new NormalizationError("invalid_duration", "Invalid duration");
  }
  return milliseconds;
}

export function normalizeMessageId(value) {
  if (typeof value !== "string") throw new NormalizationError("invalid_message_id", "Invalid Message-ID");
  return value.trim().replace(/^<|>$/g, "");
}

export const durationBounds = Object.freeze({ maxMs: MAX_DURATION_MS });
