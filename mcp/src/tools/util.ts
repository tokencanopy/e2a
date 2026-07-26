import { z, type ZodRawShape } from "zod";
import { E2AError } from "@e2a/sdk/v1";

export type ToolResult = {
  content: Array<{ type: "text"; text: string }>;
  structuredContent?: { [key: string]: unknown };
  isError?: boolean;
};

type SnakeCase<S extends string> =
  S extends `${infer Head}${infer Tail}`
    ? Head extends Lowercase<Head>
      ? `${Head}${SnakeCase<Tail>}`
      : `_${Lowercase<Head>}${SnakeCase<Tail>}`
    : S;

type TrimLeadingUnderscore<S extends string> = S extends `_${infer Rest}` ? Rest : S;

/** The public MCP success shape corresponding to an ergonomic SDK value. */
export type McpOutput<T> =
  T extends Date ? T
    : T extends readonly (infer Item)[] ? McpOutput<Item>[]
      : T extends object ? {
          [Key in keyof T as Key extends string
            ? TrimLeadingUnderscore<SnakeCase<Key>>
            : Key]: McpOutput<T[Key]>
        }
        : T;

function snakeCaseKey(key: string): string {
  return key
    .replace(/([A-Z]+)([A-Z][a-z])/g, "$1_$2")
    .replace(/([a-z0-9])([A-Z])/g, "$1_$2")
    .toLowerCase();
}

type GeneratedAttribute = {
  name: string;
  baseName: string;
  type: string;
};

function generatedAttributes(value: object): Map<string, GeneratedAttribute> | undefined {
  const ctor = value.constructor as {
    getAttributeTypeMap?: () => GeneratedAttribute[];
  };
  if (typeof ctor.getAttributeTypeMap !== "function") return undefined;
  return new Map(ctor.getAttributeTypeMap().map((attribute) => [attribute.name, attribute]));
}

function isFreeFormMapType(type: string): boolean {
  return /^\{\s*\[key:\s*string\]\s*:\s*(?:any|unknown);?\s*\}$/.test(type);
}

/**
 * Convert SDK response models back to the REST-style names used by MCP.
 *
 * SDK model property names are ergonomic camelCase. MCP is a separate public
 * boundary and deliberately follows the REST contract's snake_case vocabulary.
 * Recurse through arrays/nested view objects without touching Dates or
 * primitives. Already-snake_case keys (including reserved-word-safe `from_`)
 * are preserved verbatim.
 *
 * IMPORTANT: the generated SDK's ObjectSerializer.deserialize returns CLASS
 * INSTANCES (`new typeMap[type]()`), not plain object literals. Their generated
 * attribute map is the authoritative boundary metadata: `baseName` is the REST
 * key and a `{ [key: string]: any }` attribute is an OPEN JSON map whose keys
 * belong to the user/event payload and must remain byte-for-byte unchanged.
 * Without that distinction, a template variable like `firstName` became
 * `first_name` inside suggested_data and silently rendered blank when reused.
 *
 * Non-generated objects still use the historical recursive snake_case fallback
 * so hand-built MCP envelopes and test doubles retain the frozen v1 shape.
 */
export function toMcpOutput<T>(value: T): McpOutput<T> {
  return convertMcpOutput(value, false) as McpOutput<T>;
}

function convertMcpOutput(value: unknown, preserveMapKeys: boolean): unknown {
  if (Array.isArray(value)) {
    return value.map((item) => convertMcpOutput(item, preserveMapKeys));
  }
  if (value !== null && typeof value === "object" && !(value instanceof Date)) {
    const attributes = preserveMapKeys ? undefined : generatedAttributes(value);
    const out: Record<string, unknown> = {};
    for (const [key, item] of Object.entries(value)) {
      const attribute = attributes?.get(key);
      const outputKey = preserveMapKeys ? key : attribute?.baseName ?? snakeCaseKey(key);
      const preserveChildKeys = preserveMapKeys || (attribute !== undefined && isFreeFormMapType(attribute.type));
      out[outputKey] = convertMcpOutput(item, preserveChildKeys);
    }
    return out;
  }
  return value;
}

// strictInputSchema wraps a zod raw shape in a strict ZodObject so the
// MCP SDK rejects unknown argument keys instead of silently stripping
// them. Without this, a typo like `limit` against a tool that takes
// `page_size` succeeds with the bogus arg ignored — which the e2e
// contract sweep caught as a foot-gun. Apply to every registerTool call.
export function strictInputSchema<S extends ZodRawShape>(shape: S) {
  return z.object(shape).strict();
}

// paginationInput is the ONE pagination shape for every cursor-paginated list
// tool (§6a #3): `cursor` + `limit` in, and the tool returns `{ <items>,
// next_cursor }`. Spread it into the tool's input schema. This replaces the old
// mix of `token` / `page_size` / bare `limit`.
//
// FROZEN GA CONTRACT — do not change the list-envelope shape. The MCP list tools
// deliberately return a DOMAIN-NAMED array (`agents`, `messages`, `events`, …)
// and OMIT `next_cursor` when the list is exhausted (its absence = last page).
// This is intentionally DIFFERENT from the REST envelope (`{ items, next_cursor:
// null }`) and mirrors the MCP protocol's own pagination idiom (named array +
// cursor omitted at end — the spec says treat a *missing* nextCursor as the end;
// there is no `null` convention). It is the better fit for an LLM caller: named
// keys are self-describing in a transcript, and omitting the cursor is a cleaner
// "stop" signal than an explicit `null` (which models echo back and loop on).
// Because agents/prompts learn this shape, the result key names, the
// omit-when-done contract, and the `cursor`/`limit` input names are all part of
// the stable v1 contract post-GA — do NOT rename keys, switch to a generic
// `items`, or start emitting `next_cursor: null`. (Adding structuredContent /
// outputSchema later is additive and non-breaking, so that stays a future option.)
// Ref: MCP pagination spec — https://modelcontextprotocol.io/specification/2025-06-18/server/utilities/pagination
export const paginationInput = {
  cursor: z
    .string()
    .optional()
    .describe(
      "Pagination cursor. Pass the `next_cursor` from a previous response to fetch the next page; omit for the first page. The response includes `next_cursor` ONLY when more pages remain — when it is absent, you have reached the last page; STOP (do not keep calling).",
    ),
  limit: z
    .number()
    .int()
    .positive()
    .max(100)
    .optional()
    .describe("Max items in this page (1–100). Defaults to a server-chosen page size (100)."),
} as const;

// emailSelector is the ONE agent-selector field for every tool that acts on a
// single agent's mailbox. The credential's scope decides whether it may be
// omitted: an agent-scoped credential IS one agent, so the field defaults to
// that bound agent; an account-scoped credential owns many agents and has no
// default, so the field is REQUIRED at runtime even though the schema marks it
// optional (the schema cannot know the credential's scope). Share this field —
// and its wording — everywhere the selector appears so agents learn one rule,
// not ten. Description-only contract: do NOT change the optionality; making it
// schema-required would break agent-scoped callers that correctly omit it.
export const emailSelector = z
  .string()
  .optional()
  .describe(
    "Owning agent's inbox (full email address). REQUIRED when the credential is account-scoped — an account-scoped credential owns many agents and has no bound agent to default to, so omitting `email` fails the call. Defaults to the bound agent for agent-scoped credentials (omit it there).",
  );

// CodedError: a wrapper-thrown error that carries a stable machine `code`
// from the SERVER'S canonical vocabulary (e.g. "not_found") instead of the
// blanket `invalid_request` that plain wrapper Errors map to. Use it when the
// wrapper synthesizes a failure that is semantically NOT a caller-input
// problem — e.g. ownerOfPending's "pending message not found / already
// resolved", where an agent branching on `invalid_request` would wrongly
// re-validate its arguments. No status/request_id ride along: the failure was
// synthesized by the wrapper, not read off a specific HTTP response. The text
// form stays the code-less `e2a error: msg` prose (frozen wrapper shape).
export class CodedError extends Error {
  readonly code: string;
  constructor(code: string, message: string) {
    super(message);
    this.name = "CodedError";
    this.code = code;
  }
}

export async function runTool<T>(fn: () => Promise<T>): Promise<ToolResult> {
  try {
    const result = await fn();
    const output = toMcpOutput(result);
    const text =
      output === undefined
        ? "OK"
        : typeof output === "string"
          ? output
          : JSON.stringify(output, null, 2);
    return { content: [{ type: "text", text }] };
  } catch (err) {
    // Every tool error carries BOTH representations (GA review Tier-2 #12/#31):
    //
    //  * `content` (text) — the human/legacy form. Its wording is a de-facto
    //    FROZEN contract (`e2a error [code](retryable)?: msg` for API errors,
    //    `e2a error: msg` for wrapper errors) because deployed agents regex it.
    //    Do NOT change it.
    //  * `structuredContent` — the sanctioned machine-branchable form (see
    //    structuredError below): { code, retryable, status?, request_id?,
    //    retry_after_seconds?, details? }. New agents should branch on this
    //    instead of parsing the text.
    //
    // The MCP TS SDK skips outputSchema validation for isError results
    // server-side and allows structuredContent without a declared schema.
    // (A client MAY still validate against a declared outputSchema — but no
    // e2a tool declares one, so this is additive and non-breaking.)
    const structured = structuredError(err);
    // Surface the API envelope's machine-branchable `code` (§6a #4) so an agent
    // can branch on a stable code (e.g. domain_not_verified, message_not_pending,
    // recipient_suppressed) instead of parsing prose. The retryable hint tells
    // it whether a retry could ever help. Errors thrown by the wrapper itself
    // (plain Error — e.g. "email is required") have no code and fall through to
    // the prose form (text stays code-less; structuredContent still carries a
    // stable code).
    if (err instanceof E2AError && err.code) {
      const retry = err.retryable ? " (retryable)" : "";
      // Only the `code` (a trusted snake_case token) goes inside the brackets.
      // The message is free-form and can echo caller/recipient input, so bound +
      // sanitize it: strip control chars/newlines (keep the `[code]` convention
      // parseable) and cap length (avoid context blowup). The text carries code +
      // message only; request_id/details ride in structuredContent (details
      // size-capped there).
      return {
        content: [{ type: "text", text: `e2a error [${err.code}]${retry}: ${sanitizeMessage(err.message)}` }],
        structuredContent: structured,
        isError: true,
      };
    }
    const message = err instanceof Error ? err.message : String(err);
    return {
      content: [{ type: "text", text: `e2a error: ${sanitizeMessage(message)}` }],
      structuredContent: structured,
      isError: true,
    };
  }
}

// structuredError builds the machine-branchable error payload emitted as
// `structuredContent` on every isError tool result.
//
// Shape (stable, additive-only post-GA):
//   code                 stable snake_case token — ALWAYS present. API errors
//                        carry the envelope code (domain_not_verified,
//                        rate_limited, …); wrapper-thrown validation errors
//                        (missing agent arg, confirm guard) reuse the server's
//                        canonical validation code `invalid_request`; a
//                        CodedError carries its own server-vocabulary code
//                        (e.g. `not_found`).
//   retryable            boolean — ALWAYS present; true when a retry could
//                        plausibly succeed (429 / 5xx / connection).
//   status               HTTP status of the API response (0 = connection-level
//                        failure). ABSENT for wrapper errors: no request was
//                        ever made.
//   request_id           server X-Request-Id — quote it in support requests.
//   retry_after_seconds  back-off hint (Retry-After header or the send-path
//                        429's details.retry_after_seconds).
//   details              envelope `error.details` (field-level validation
//                        info). Omitted when it doesn't JSON-serialize or its
//                        JSON exceeds MAX_DETAILS_JSON_LEN (context-blowup /
//                        echoed-input guard, mirroring sanitizeMessage).
const MAX_DETAILS_JSON_LEN = 2000;
function structuredError(err: unknown): { [key: string]: unknown } {
  if (err instanceof E2AError) {
    const out: { [key: string]: unknown } = {
      // toE2AError always synthesizes a code; "error" only guards a
      // hand-constructed codeless E2AError (mirrors the SDK's own fallback).
      code: err.code || "error",
      retryable: err.retryable,
      status: err.status,
    };
    if (err.requestId) out.request_id = err.requestId;
    if (err.retryAfterSeconds !== undefined) out.retry_after_seconds = err.retryAfterSeconds;
    if (err.details !== undefined) {
      try {
        const json = JSON.stringify(err.details);
        if (json !== undefined && json.length <= MAX_DETAILS_JSON_LEN) out.details = err.details;
      } catch {
        // Unserializable (cycles, BigInt) — omit rather than fail the result.
      }
    }
    return out;
  }
  // Wrapper-thrown error with an explicit server-vocabulary code (e.g.
  // ownerOfPending's "not_found" for a pending draft that was already
  // approved/rejected/expired). Not retryable, and no status/request_id —
  // the wrapper synthesized it rather than reading it off a response.
  if (err instanceof CodedError) {
    return { code: err.code, retryable: false };
  }
  // Wrapper-thrown validation error (plain Error from the MCP layer itself —
  // e.g. "delete_agent requires confirm:true", missing agent selection). It is
  // always a caller-input problem, so reuse the server's canonical validation
  // code instead of surfacing no code at all. No status/request_id: no HTTP
  // exchange happened.
  return { code: "invalid_request", retryable: false };
}

// sanitizeMessage makes an error message safe to splice into the single-line
// `[code]: <message>` tool-error text: collapse control chars / newlines to
// spaces (so they can't forge a second `[code]` bracket or break a parser) and
// cap the length (an attacker-influenced or raw message must not blow up the
// agent's context).
const MAX_ERROR_MESSAGE_LEN = 500;
function sanitizeMessage(message: string): string {
  // eslint-disable-next-line no-control-regex
  const oneLine = message.replace(/[\x00-\x1f\x7f]+/g, " ").trim();
  return oneLine.length > MAX_ERROR_MESSAGE_LEN
    ? oneLine.slice(0, MAX_ERROR_MESSAGE_LEN) + "…"
    : oneLine;
}
