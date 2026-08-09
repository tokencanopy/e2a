import { createHash } from "node:crypto";
import PostalMime from "postal-mime";
import { EvalError } from "./errors.mjs";

const DEFAULT_MAX_BYTES = 25 * 1024 * 1024;
const CANONICAL_BASE64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;
const MESSAGE_ID_TOKEN = /(?:^|[ \t])<([^<>\s\u0000-\u001F\u007F]+)>(?=$|[ \t])/g;

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

function safeToken(value) {
  if (typeof value !== "string" || /[\r\n\u0000-\u001F\u007F]/.test(value)) return null;
  MESSAGE_ID_TOKEN.lastIndex = 0;
  const bracketed = MESSAGE_ID_TOKEN.exec(value);
  return bracketed?.[1] ?? null;
}

function referenceTokens(value) {
  if (typeof value !== "string" || /[\r\n\u0000-\u001F\u007F]/.test(value)) return [];
  MESSAGE_ID_TOKEN.lastIndex = 0;
  const values = [];
  let match;
  while ((match = MESSAGE_ID_TOKEN.exec(value)) !== null) values.push(match[1]);
  return values;
}

function firstHeader(headers, key) {
  const header = Array.isArray(headers) ? headers.find((item) => item?.key?.toLowerCase() === key) : undefined;
  return header?.value;
}

function headersFor(headers, key) {
  return Array.isArray(headers) ? headers.filter((item) => item?.key?.toLowerCase() === key).map((item) => item.value) : [];
}

function address(address) {
  return address && typeof address.address === "string" ? address.address : null;
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
export async function parseMimeEvidence(rawBase64, { maxBytes = DEFAULT_MAX_BYTES } = {}) {
  let bytes;
  try {
    bytes = canonicalBase64(rawBase64, maxBytes);
    let parsed;
    try {
      parsed = await PostalMime.parse(bytes, {
        attachmentEncoding: "arraybuffer",
        maxNestingDepth: 32,
        maxHeadersSize: 262_144,
      });
    } catch {
      throw graderError("mime_parse_failed", "Raw MIME could not be parsed");
    }
    const messageId = safeToken(firstHeader(parsed.headers, "message-id") ?? parsed.messageId);
    const inReplyTo = safeToken(firstHeader(parsed.headers, "in-reply-to") ?? parsed.inReplyTo);
    const referenceValues = headersFor(parsed.headers, "references");
    const references = referenceValues.length > 0
      ? referenceValues.flatMap(referenceTokens)
      : referenceTokens(parsed.references);
    return {
      messageId,
      inReplyTo,
      references,
      subject: typeof parsed.subject === "string" ? parsed.subject : null,
      from: address(parsed.from),
      replyTo: Array.isArray(parsed.replyTo) ? parsed.replyTo.map(address).filter((value) => value !== null) : [],
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
