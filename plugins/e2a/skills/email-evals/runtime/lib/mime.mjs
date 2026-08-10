import { createHash } from "node:crypto";
import PostalMime from "postal-mime";
import { EvalError } from "./errors.mjs";
import { NormalizationError, normalizeMailboxHeader } from "./normalize.mjs";

const DEFAULT_MAX_BYTES = 25 * 1024 * 1024;
const MAX_MESSAGE_ID_BYTES = 998;
const MAX_HEADERS_BYTES = 262_144;
const CANONICAL_BASE64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;
const RAW_IDENTITY_HEADERS = new Set([
  "message-id", "in-reply-to", "references", "from", "reply-to", "to", "cc",
]);

function graderError(code, message) {
  return new EvalError("grader_error", code, message);
}

function decodedLength(value) {
  const padding = value.endsWith("==") ? 2 : value.endsWith("=") ? 1 : 0;
  return (value.length / 4) * 3 - padding;
}

function canonicalBase64(value, maxBytes) {
  if (typeof value !== "string") {
    throw graderError("invalid_mime_base64", "Raw MIME must be canonical base64");
  }
  if (!Number.isSafeInteger(maxBytes) || maxBytes < 0) throw graderError("invalid_mime_limit", "MIME byte limit is invalid");
  if (decodedLength(value) > maxBytes) throw graderError("mime_too_large", "Raw MIME exceeds the configured byte limit");
  if (!CANONICAL_BASE64.test(value)) throw graderError("invalid_mime_base64", "Raw MIME must be canonical base64");
  const bytes = Buffer.from(value, "base64");
  if (bytes.length !== decodedLength(value) || bytes.toString("base64") !== value) {
    throw graderError("invalid_mime_base64", "Raw MIME must be canonical base64");
  }
  return bytes;
}

export function normalizeMessageIdToken(value) {
  if (typeof value !== "string" || Buffer.byteLength(value, "utf8") > MAX_MESSAGE_ID_BYTES
    || /[\r\n\u0000-\u0008\u000B-\u001F\u007F]/.test(value)) return null;
  const match = value.match(/^[ \t]*<([^<>\s\u0000-\u001F\u007F]+)>[ \t]*$/);
  if (!match) return null;
  const token = match[1];
  const separator = token.indexOf("@");
  if (separator <= 0 || separator !== token.lastIndexOf("@") || separator === token.length - 1) return null;
  const dotAtom = (part) => part.split(".").every((segment) => segment.length > 0
    && /^[A-Za-z0-9!#$%&'*+\-/=?^_`{|}~]+$/.test(segment));
  const left = token.slice(0, separator);
  const right = token.slice(separator + 1);
  const literal = /^\[(?:[\x21-\x5A\x5E-\x7E]|\\[\x21-\x7E])+\]$/.test(right);
  return dotAtom(left) && (dotAtom(right) || literal) ? token : null;
}

function headersFor(headers, key) {
  return Array.isArray(headers) ? headers.filter((item) => item?.key?.toLowerCase() === key).map((item) => item.value) : [];
}

function singletonHeader(headers, key, { required = false } = {}) {
  const values = headersFor(headers, key);
  if (values.length > 1) throw graderError("duplicate_mime_header", "Raw MIME repeated a singleton header");
  if (required && values.length !== 1) throw graderError("missing_mime_header", "Raw MIME omitted a required singleton header");
  return values[0] ?? null;
}

function rawIdentityHeaders(bytes) {
  let end = bytes.indexOf(Buffer.from("\r\n\r\n"));
  if (end < 0) {
    end = bytes.indexOf(Buffer.from("\n\n"));
  }
  if (end < 0 || end > MAX_HEADERS_BYTES) {
    throw graderError("mime_parse_failed", "Raw MIME could not be parsed");
  }
  const source = bytes.subarray(0, end).toString("latin1");
  if (/\r(?!\n)/.test(source)) throw graderError("mime_parse_failed", "Raw MIME could not be parsed");
  const lines = source.split(/\r?\n/);
  const headers = [];
  let current = null;
  for (const line of lines) {
    if (/^[ \t]/.test(line)) {
      if (!current) throw graderError("mime_parse_failed", "Raw MIME could not be parsed");
      current.value += line;
      continue;
    }
    const separator = line.indexOf(":");
    if (separator <= 0 || !/^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/.test(line.slice(0, separator))) {
      throw graderError("mime_parse_failed", "Raw MIME could not be parsed");
    }
    const key = line.slice(0, separator).toLowerCase();
    current = { key, value: line.slice(separator + 1) };
    if (RAW_IDENTITY_HEADERS.has(key)) headers.push(current);
  }
  return headers;
}

function exactMessageId(value, name) {
  const token = normalizeMessageIdToken(value);
  if (token === null) throw graderError("malformed_mime_header", `Raw MIME ${name} header is malformed`);
  return token;
}

function exactReferences(value) {
  if (value === null) return [];
  if (typeof value !== "string" || Buffer.byteLength(value, "utf8") > MAX_MESSAGE_ID_BYTES
    || /[\r\n\u0000-\u001F\u007F]/.test(value)) {
    throw graderError("malformed_mime_header", "Raw MIME References header is malformed");
  }
  const source = value.replace(/^[ \t]+|[ \t]+$/g, "");
  const values = [];
  let cursor = 0;
  while (cursor < source.length) {
    const match = /^<[^<>]*>/.exec(source.slice(cursor));
    const token = match ? normalizeMessageIdToken(match[0]) : null;
    if (!match || token === null) throw graderError("malformed_mime_header", "Raw MIME References header is malformed");
    values.push(token);
    cursor += match[0].length;
    if (cursor === source.length) break;
    const whitespace = /^[ \t]+/.exec(source.slice(cursor));
    if (!whitespace) throw graderError("malformed_mime_header", "Raw MIME References header is malformed");
    cursor += whitespace[0].length;
  }
  if (values.length === 0) throw graderError("malformed_mime_header", "Raw MIME References header is malformed");
  return values;
}

function strictMailbox(value, name) {
  try {
    return normalizeMailboxHeader(value);
  } catch (error) {
    if (error instanceof NormalizationError) throw graderError("malformed_mime_header", `Raw MIME ${name} header is malformed`);
    throw error;
  }
}

function splitAddressList(value, name) {
  if (typeof value !== "string" || value.length === 0 || /[\r\n\u0000-\u0008\u000B-\u001F\u007F]/.test(value)) {
    throw graderError("malformed_mime_header", `Raw MIME ${name} header is malformed`);
  }
  const result = [];
  let start = 0;
  let quoted = false;
  let escaped = false;
  let commentDepth = 0;
  let angle = false;
  for (let cursor = 0; cursor < value.length; cursor += 1) {
    const character = value[cursor];
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
      if (angle) throw graderError("malformed_mime_header", `Raw MIME ${name} header is malformed`);
      angle = true;
    } else if (character === ">") {
      if (!angle) throw graderError("malformed_mime_header", `Raw MIME ${name} header is malformed`);
      angle = false;
    } else if (character === "," && !angle) {
      const entry = value.slice(start, cursor).replace(/^[ \t]+|[ \t]+$/g, "");
      if (!entry) throw graderError("malformed_mime_header", `Raw MIME ${name} header is malformed`);
      result.push(entry);
      start = cursor + 1;
    }
  }
  if (quoted || escaped || commentDepth !== 0 || angle) {
    throw graderError("malformed_mime_header", `Raw MIME ${name} header is malformed`);
  }
  const final = value.slice(start).replace(/^[ \t]+|[ \t]+$/g, "");
  if (!final) throw graderError("malformed_mime_header", `Raw MIME ${name} header is malformed`);
  result.push(final);
  return result;
}

function strictAddressList(headers, key) {
  const value = singletonHeader(headers, key);
  if (value === null) return [];
  return splitAddressList(value, key).map((entry) => strictMailbox(entry, key).address);
}

function attachmentMetadata(attachment) {
  let content;
  if (attachment?.content instanceof ArrayBuffer) content = new Uint8Array(attachment.content);
  else if (ArrayBuffer.isView(attachment?.content)) content = new Uint8Array(attachment.content.buffer, attachment.content.byteOffset, attachment.content.byteLength);
  else if (typeof attachment?.content === "string") content = Buffer.from(attachment.content, "utf8");
  else content = new Uint8Array();
  return {
    filename: attachment?.filename ?? null,
    contentType: attachment?.mimeType ?? null,
    disposition: attachment?.disposition ?? null,
    sizeBytes: content.byteLength,
    sha256: createHash("sha256").update(content).digest("hex"),
  };
}

/** Parse bounded canonical raw MIME into serializable evidence without retaining MIME bytes. */
export async function parseMimeEvidence(rawBase64, { maxBytes = DEFAULT_MAX_BYTES, requireMessageId = true } = {}) {
  let bytes;
  try {
    if (typeof requireMessageId !== "boolean") throw graderError("invalid_mime_limit", "MIME options are invalid");
    bytes = canonicalBase64(rawBase64, maxBytes);
    const identityHeaders = rawIdentityHeaders(bytes);
    let parsed;
    try {
      parsed = await PostalMime.parse(bytes, {
        attachmentEncoding: "arraybuffer",
        maxNestingDepth: 32,
        maxHeadersSize: MAX_HEADERS_BYTES,
      });
    } catch {
      throw graderError("mime_parse_failed", "Raw MIME could not be parsed");
    }
    const rawMessageId = singletonHeader(identityHeaders, "message-id", { required: requireMessageId });
    const messageId = rawMessageId === null ? null : exactMessageId(rawMessageId, "Message-ID");
    const rawInReplyTo = singletonHeader(identityHeaders, "in-reply-to");
    const inReplyTo = rawInReplyTo === null ? null : exactMessageId(rawInReplyTo, "In-Reply-To");
    const rawReferences = singletonHeader(identityHeaders, "references");
    const references = exactReferences(rawReferences);
    const rawFrom = singletonHeader(identityHeaders, "from", { required: true });
    const from = strictMailbox(rawFrom, "From");
    const rawReplyTo = singletonHeader(identityHeaders, "reply-to");
    const replyTo = rawReplyTo === null ? [] : strictAddressList([{ key: "reply-to", value: rawReplyTo }], "reply-to");
    const rawSubject = singletonHeader(parsed.headers, "subject");
    if (rawSubject !== null && typeof parsed.subject === "string" && rawSubject !== parsed.subject) {
      throw graderError("conflicting_mime_header", "Raw MIME Subject representations conflict");
    }
    return {
      messageId,
      inReplyTo,
      references,
      subject: rawSubject,
      from: rawFrom,
      fromAddress: from.address,
      replyTo,
      to: strictAddressList(identityHeaders, "to"),
      cc: strictAddressList(identityHeaders, "cc"),
      text: typeof parsed.text === "string" ? parsed.text : null,
      htmlPresent: typeof parsed.html === "string" && parsed.html.length > 0,
      sizeBytes: bytes.byteLength,
      attachments: Array.isArray(parsed.attachments) ? parsed.attachments.map(attachmentMetadata) : [],
    };
  } finally {
    // Never return or retain raw MIME / attachment bytes in evaluator evidence.
    bytes = undefined;
  }
}
