import { addressParser } from "postal-mime";

const MAX_DURATION_MS = 24 * 60 * 60 * 1_000;
const durationPattern = /^(0|[1-9][0-9]*)(ms|s|m|h)$/;
// RFC address-list syntax separators cannot occur in an unquoted addr-spec.
// Apostrophes, braces, plus, hyphen, and the rest of RFC atext deliberately
// are not separators. Scanning maximal spans once prevents both quadratic
// regex retry behavior and prefix replacement inside a longer mailbox.
const MAILBOX_TEXT_BOUNDARIES = new Set(["<", ">", ",", ";", ":"]);

function mailboxTextBoundary(character, state) {
  if (state.quoted || state.commentDepth > 0 || state.domainLiteral) return false;
  if (character === ")") return true;
  return MAILBOX_TEXT_BOUNDARIES.has(character) || /\s/u.test(character)
    || character.charCodeAt(0) <= 0x1f || character.charCodeAt(0) === 0x7f;
}

function scanMailboxText(value, visit) {
  let cursor = 0;
  while (cursor < value.length) {
    const boundaryState = { quoted: false, commentDepth: 0, domainLiteral: false };
    if (mailboxTextBoundary(value[cursor], boundaryState)) {
      visit({ boundary: value[cursor] });
      cursor += 1;
      continue;
    }
    const start = cursor;
    const state = { quoted: false, commentDepth: 0, domainLiteral: false, escaped: false };
    let containsAt = false;
    while (cursor < value.length && !mailboxTextBoundary(value[cursor], state)) {
      const character = value[cursor];
      if (character === "@") containsAt = true;
      if (state.escaped) {
        state.escaped = false;
      } else if (character === "\\" && (state.quoted || state.commentDepth > 0 || state.domainLiteral)) {
        state.escaped = true;
      } else if (state.commentDepth > 0) {
        if (character === "(") state.commentDepth += 1;
        if (character === ")") state.commentDepth -= 1;
      } else if (state.quoted) {
        if (character === "\"") state.quoted = false;
      } else if (state.domainLiteral) {
        if (character === "]") state.domainLiteral = false;
      } else if (character === "(") {
        state.commentDepth = 1;
      } else if (character === "\"") {
        state.quoted = true;
      } else if (character === "[") {
        state.domainLiteral = true;
      }
      cursor += 1;
    }
    const candidate = value.slice(start, cursor);
    if (!containsAt) {
      visit({ candidate, mailbox: null, unsafe: false });
      continue;
    }
    try {
      visit({ candidate, mailbox: normalizeMailbox(candidate), unsafe: false });
    } catch (error) {
      if (error instanceof NormalizationError) {
        // An invalid maximal span may contain multiple adjacent valid
        // mailboxes. Fail closed rather than leak either half while trying to
        // guess a split that could corrupt a longer valid addr-spec.
        visit({ candidate, mailbox: null, unsafe: true });
        continue;
      }
      throw error;
    }
  }
}

export class NormalizationError extends Error {
  constructor(code, message = "Invalid normalized value") {
    super(message);
    this.name = "NormalizationError";
    this.code = code;
  }
}

function sourceAddressSpec(value) {
  const source = value.trim();
  let quoted = false;
  let escaped = false;
  let commentDepth = 0;
  let angleStart = -1;
  let angleEnd = -1;
  for (let index = 0; index < source.length; index += 1) {
    const character = source[index];
    if (escaped) {
      escaped = false;
      continue;
    }
    if (quoted) {
      if (character === "\\") escaped = true;
      else if (character === '"') quoted = false;
      continue;
    }
    if (commentDepth > 0) {
      if (character === "\\") escaped = true;
      else if (character === "(") commentDepth += 1;
      else if (character === ")") commentDepth -= 1;
      continue;
    }
    if (character === '"') quoted = true;
    else if (character === "(") commentDepth = 1;
    else if (character === "<") {
      if (angleStart !== -1 || angleEnd !== -1) return null;
      angleStart = index;
    } else if (character === ">") {
      if (angleStart === -1 || angleEnd !== -1) return null;
      angleEnd = index;
    }
  }
  if (quoted || escaped || commentDepth > 0) return null;
  if (angleStart === -1) return angleEnd === -1 ? source : null;
  if (angleEnd < angleStart || source.slice(angleEnd + 1).trim().length > 0) return null;
  return source.slice(angleStart + 1, angleEnd).trim();
}

function quotedAddressSpec(value) {
  if (typeof value !== "string" || value[0] !== '"') return null;
  let decoded = "";
  let escaped = false;
  let closing = -1;
  for (let index = 1; index < value.length; index += 1) {
    const character = value[index];
    if (escaped) {
      if (character.charCodeAt(0) <= 0x1f || character.charCodeAt(0) === 0x7f) return null;
      decoded += character;
      escaped = false;
    } else if (character === "\\") {
      escaped = true;
    } else if (character === '"') {
      closing = index;
      break;
    } else {
      if (character.charCodeAt(0) <= 0x1f || character.charCodeAt(0) === 0x7f) return null;
      decoded += character;
    }
  }
  if (closing === -1 || escaped || value[closing + 1] !== "@") return null;
  const domain = value.slice(closing + 2);
  if (!domain || /[\s@<>,;:]/u.test(domain)) return null;
  const quotedLocal = value.slice(0, closing + 1).toLowerCase();
  return {
    address: `${quotedLocal}@${domain.toLowerCase()}`,
    semantic: `${decoded.toLowerCase()}@${domain.toLowerCase()}`,
  };
}

function parsedAddressSemantic(value) {
  const quoted = quotedAddressSpec(value);
  if (quoted) return quoted.semantic;
  const address = value.trim().toLowerCase();
  const at = address.lastIndexOf("@");
  if (at <= 0 || at === address.length - 1 || /[\r\n]/.test(address.slice(at + 1))) return null;
  return `${address.slice(0, at)}@${address.slice(at + 1)}`;
}

function normalizedMailbox(entry, source) {
  if (!entry || typeof entry.address !== "string" || !entry.address || /[\r\n]/.test(entry.address)) {
    throw new NormalizationError("invalid_mailbox", "Invalid mailbox");
  }
  const addressSpec = sourceAddressSpec(source);
  if (addressSpec === null) throw new NormalizationError("invalid_mailbox", "Invalid mailbox");
  const quoted = quotedAddressSpec(addressSpec);
  if (addressSpec.startsWith('"')) {
    if (!quoted || parsedAddressSemantic(entry.address) !== quoted.semantic) {
      throw new NormalizationError("invalid_mailbox", "Invalid mailbox");
    }
    return { address: quoted.address, displayName: entry.name || undefined };
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
  return normalizedMailbox(parsed[0], value);
}

/** Replace parser-valid mailbox tokens in free text without matching address prefixes. */
export function replaceMailboxText(value, replacer) {
  if (typeof value !== "string" || typeof replacer !== "function") return value;
  const chunks = [];
  scanMailboxText(value, ({ boundary, candidate, mailbox, unsafe }) => {
    if (boundary !== undefined) chunks.push(boundary);
    else if (unsafe) chunks.push("[REDACTED:address]");
    else if (mailbox) chunks.push(replacer(mailbox, candidate));
    else chunks.push(candidate);
  });
  return chunks.join("");
}

export function mailboxAddressesInText(value) {
  const addresses = [];
  if (typeof value !== "string") return addresses;
  scanMailboxText(value, ({ mailbox }) => {
    if (mailbox) addresses.push(mailbox.address);
  });
  return addresses;
}

export function containsMailboxText(value) {
  if (typeof value !== "string") return false;
  let found = false;
  scanMailboxText(value, ({ mailbox, unsafe }) => {
    if (mailbox || unsafe) found = true;
  });
  return found;
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
