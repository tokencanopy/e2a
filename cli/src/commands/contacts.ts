import { readFile } from "node:fs/promises";
import type {
  ContactImportRow,
  CreateContactRequest,
  UpdateContactRequest,
  UpsertEngagementRequest,
} from "@e2a/sdk/v1";
import { ImportContactsRequestOnConflictEnum } from "@e2a/sdk/v1";
import { createClient, requireAgentEmail } from "../sdk.js";
import { EXIT, fail } from "../exit.js";
import { parseRfc3339 } from "../time.js";
import { sanitizeTsvField } from "./messages.js";

export interface OutputOptions { json?: boolean }

function jsonObject(raw: string | undefined, flag: string): Record<string, unknown> | undefined {
  if (raw === undefined) return undefined;
  try {
    const value: unknown = JSON.parse(raw);
    if (value === null || Array.isArray(value) || typeof value !== "object") {
      fail(EXIT.USAGE, `${flag} must be a JSON object`);
    }
    return value as Record<string, unknown>;
  } catch {
    fail(EXIT.USAGE, `${flag} must be valid JSON`);
  }
}

// An explicitly-empty --if-match is a usage error, never an unconditional write.
// The flag exists to make a write conditional; interpolating an empty variable
// into it (`--if-match "$ETAG"` with ETAG unset) would otherwise quietly send a
// header the server has to reject — and before the server rejected it, quietly
// perform the unguarded write the caller was trying to prevent.
function requireIfMatch(raw: string | undefined): string | undefined {
  if (raw === undefined) return undefined;
  if (raw.trim() === "") {
    fail(EXIT.USAGE, "--if-match requires an ETag value; omit the flag for an unconditional write");
  }
  return raw;
}

function positiveLimit(raw: string | undefined): number {
  if (raw === undefined) return 100;
  const value = Number(raw);
  if (!Number.isInteger(value) || value < 1 || value > 10_000) {
    fail(EXIT.USAGE, "--limit must be an integer from 1 to 10000");
  }
  return value;
}

function boolFilter(raw: string | undefined, flag: string): boolean | undefined {
  if (raw === undefined) return undefined;
  if (raw === "true") return true;
  if (raw === "false") return false;
  fail(EXIT.USAGE, `${flag} must be true or false`);
}

function dateValue(raw: string | undefined, flag: string): Date | undefined {
  if (raw === undefined) return undefined;
  // Strict shared parser (time.ts): an explicit UTC offset is required, so a
  // date-only or offsetless value fails fast instead of being read in local
  // time and silently shifting the filter.
  return parseRfc3339(raw, flag);
}

export async function contactsList(opts: {
  source?: string;
  importBatch?: string;
  createdAfter?: string;
  createdBefore?: string;
  limit?: string;
  json?: boolean;
}): Promise<void> {
  const client = createClient();
  const rows = await client.contacts.list({
    source: opts.source,
    importBatchId: opts.importBatch,
    createdAfter: dateValue(opts.createdAfter, "--created-after"),
    createdBefore: dateValue(opts.createdBefore, "--created-before"),
  }).toArray({ limit: positiveLimit(opts.limit) });
  for (const row of rows) {
    if (opts.json) process.stdout.write(JSON.stringify(row) + "\n");
    else process.stdout.write(
      `${row.address}\t${sanitizeTsvField(row.displayName)}\t${row.source}\t${row.importBatchId ?? ""}\n`,
    );
  }
}

export async function contactsGet(address: string, opts: OutputOptions): Promise<void> {
  const { data: row, etag } = await createClient().contacts.getWithETag(address);
  if (opts.json) process.stdout.write(JSON.stringify({ ...row, etag }) + "\n");
  else {
    process.stdout.write(`address: ${row.address}\n`);
    if (row.displayName) process.stdout.write(`name:    ${row.displayName}\n`);
    process.stdout.write(`source:  ${row.source}\nmetadata: ${JSON.stringify(row.metadata)}\n`);
    if (etag) process.stdout.write(`etag:    ${etag}\n`);
  }
}

export async function contactsCreate(address: string, opts: {
  name?: string;
  metadata?: string;
  idempotencyKey?: string;
  json?: boolean;
}): Promise<void> {
  const body: CreateContactRequest = {
    address,
    displayName: opts.name,
    metadata: jsonObject(opts.metadata, "--metadata"),
  };
  const row = await createClient().contacts.create(body, { idempotencyKey: opts.idempotencyKey });
  process.stdout.write(opts.json ? JSON.stringify(row) + "\n" : row.address + "\n");
}

export async function contactsUpdate(address: string, opts: {
  name?: string;
  clearName?: boolean;
  metadata?: string;
  ifMatch?: string;
  json?: boolean;
}): Promise<void> {
  if (opts.name !== undefined && opts.clearName) {
    fail(EXIT.USAGE, "--name and --clear-name cannot be used together");
  }
  if (opts.name === undefined && !opts.clearName && opts.metadata === undefined) {
    fail(EXIT.USAGE, "contacts update requires --name, --clear-name, and/or --metadata");
  }
  const patch: UpdateContactRequest = {
    displayName: opts.clearName ? "" : opts.name,
    metadata: jsonObject(opts.metadata, "--metadata"),
  };
  const contacts = createClient().contacts;
  const ifMatch = requireIfMatch(opts.ifMatch);
  const row = ifMatch
    ? await contacts.update(address, patch, { ifMatch })
    : await contacts.update(address, patch);
  process.stdout.write(opts.json ? JSON.stringify(row) + "\n" : row.address + "\n");
}

export async function contactsDelete(address: string, opts: OutputOptions): Promise<void> {
  const result = await createClient().contacts.delete(address);
  process.stdout.write(opts.json ? JSON.stringify(result) + "\n" : `deleted ${result.address}\n`);
}

export async function contactsDeleteImport(batchId: string, opts: OutputOptions): Promise<void> {
  const result = await createClient().contacts.deleteImport(batchId);
  process.stdout.write(opts.json ? JSON.stringify(result) + "\n" :
    `deleted ${result.contactsDeleted} contacts and ${result.engagementsDeleted} enrolments; ` +
    `retained ${result.contactsRetained} contacts\n`);
}

// parseCSV implements the RFC 4180 quoting rules needed by exports from Sheets,
// Excel, and CRMs: quoted commas/newlines, doubled quotes, CRLF, and UTF-8 BOM.
export function parseCSV(input: string): string[][] {
  const text = input.replace(/^\uFEFF/, "");
  const rows: string[][] = [];
  let row: string[] = [];
  let field = "";
  let quoted = false;
  for (let i = 0; i < text.length; i++) {
    const char = text[i];
    if (quoted) {
      if (char === '"') {
        if (text[i + 1] === '"') {
          field += '"';
          i++;
        } else {
          quoted = false;
        }
      } else if (char !== "\r") {
        field += char;
      }
      continue;
    }
    if (char === '"' && field === "") quoted = true;
    else if (char === ",") {
      row.push(field);
      field = "";
    } else if (char === "\n") {
      row.push(field);
      rows.push(row);
      row = [];
      field = "";
    } else if (char !== "\r") {
      field += char;
    }
  }
  if (quoted) fail(EXIT.USAGE, "CSV has an unterminated quoted field");
  if (field !== "" || row.length > 0) {
    row.push(field);
    rows.push(row);
  }
  return rows.filter((values) => values.some((value) => value.trim() !== ""));
}

export async function contactsImport(path: string, opts: {
  emailColumn?: string;
  nameColumn?: string;
  agent?: string;
  stage?: string;
  onConflict?: string;
  idempotencyKey?: string;
  dryRun?: boolean;
  json?: boolean;
}): Promise<void> {
  if (opts.stage && !opts.agent) fail(EXIT.USAGE, "--stage requires --agent");
  if (opts.onConflict && !["merge", "skip"].includes(opts.onConflict)) {
    fail(EXIT.USAGE, "--on-conflict must be merge or skip");
  }
  const table = parseCSV(await readFile(path, "utf8"));
  if (table.length < 2) fail(EXIT.USAGE, "CSV must contain a header and at least one data row");
  const headers = table[0].map((value) => value.trim());
  const emailColumn = opts.emailColumn ?? "email";
  const emailIndex = headers.indexOf(emailColumn);
  if (emailIndex < 0) fail(EXIT.USAGE, `CSV has no ${JSON.stringify(emailColumn)} column`);
  const nameIndex = opts.nameColumn ? headers.indexOf(opts.nameColumn) : headers.indexOf("name");
  if (opts.nameColumn && nameIndex < 0) {
    fail(EXIT.USAGE, `CSV has no ${JSON.stringify(opts.nameColumn)} column`);
  }
  const contacts: ContactImportRow[] = table.slice(1).map((values) => {
    const result: ContactImportRow = { address: (values[emailIndex] ?? "").trim() };
    if (nameIndex >= 0) result.displayName = values[nameIndex] ?? "";
    const metadata: Record<string, string> = {};
    headers.forEach((header, index) => {
      if (index !== emailIndex && index !== nameIndex && header) metadata[header] = values[index] ?? "";
    });
    if (Object.keys(metadata).length > 0) result.metadata = metadata;
    return result;
  });
  if (contacts.length > 1000) fail(EXIT.USAGE, "CSV has more than 1000 data rows; split it into batches");
  if (opts.dryRun) {
    const preview = { rows: contacts.length, contacts, agent_email: opts.agent, stage: opts.stage };
    process.stdout.write(opts.json ? JSON.stringify(preview) + "\n" :
      `Ready to import ${contacts.length} contact(s)${opts.agent ? ` and enroll with ${opts.agent}` : ""}.\n`);
    return;
  }
  const body = {
    contacts,
    onConflict: opts.onConflict === "skip"
      ? ImportContactsRequestOnConflictEnum.Skip
      : opts.onConflict === "merge"
        ? ImportContactsRequestOnConflictEnum.Merge
        : undefined,
    agentEmail: opts.agent,
    stage: opts.stage,
  };
  const resource = createClient().contacts;
  const result = opts.idempotencyKey
    ? await resource.import(body, { idempotencyKey: opts.idempotencyKey })
    : await resource.import(body);
  if (opts.json) process.stdout.write(JSON.stringify(result) + "\n");
  else {
    process.stdout.write(
      `batch ${result.batchId}: ${result.created} created, ${result.updated} updated, ` +
      `${result.skipped} skipped, ${result.failed} failed\n`,
    );
    for (const item of result.results) {
      if (item.status === "failed" || item.status === "skipped" || item.suppressed) {
        process.stdout.write(
          `${item.index}\t${item.address ?? ""}\t${item.status}\t${item.code ?? ""}` +
          `${item.suppressed ? "\tsuppressed" : ""}\n`,
        );
      }
    }
  }
}

export async function outreachList(opts: {
  agent?: string;
  stage?: string;
  replied?: string;
  suppressed?: string;
  nextActionBefore?: string;
  lastOutboundBefore?: string;
  limit?: string;
  json?: boolean;
}): Promise<void> {
  const agent = requireAgentEmail(opts.agent);
  const rows = await createClient().contacts.outreach(agent, {
    stage: opts.stage,
    replied: boolFilter(opts.replied, "--replied"),
    suppressed: boolFilter(opts.suppressed, "--suppressed"),
    nextActionBefore: dateValue(opts.nextActionBefore, "--next-action-before"),
    lastOutboundBefore: dateValue(opts.lastOutboundBefore, "--last-outbound-before"),
  }).toArray({ limit: positiveLimit(opts.limit) });
  for (const row of rows) {
    if (opts.json) process.stdout.write(JSON.stringify(row) + "\n");
    else process.stdout.write(
      `${row.address}\t${sanitizeTsvField(row.stage)}\t${row.nextActionAt?.toISOString() ?? ""}\t` +
      `${row.replied ? "replied" : "unreplied"}\t${row.suppressed ? "suppressed" : "mailable"}\n`,
    );
  }
}

export async function outreachGet(address: string, opts: {
  agent?: string;
  json?: boolean;
}): Promise<void> {
  const { data: row, etag } = await createClient().contacts.getOutreachWithETag(
    requireAgentEmail(opts.agent), address,
  );
  process.stdout.write(opts.json ? JSON.stringify({ ...row, etag }) + "\n" :
    `${row.address}\t${row.stage}\t${row.nextActionAt?.toISOString() ?? ""}\t${etag ?? ""}\n`);
}

export async function outreachSet(address: string, opts: {
  agent?: string;
  stage?: string;
  clearStage?: boolean;
  nextAction?: string;
  metadata?: string;
  ifMatch?: string;
  json?: boolean;
}): Promise<void> {
  if (opts.stage !== undefined && opts.clearStage) {
    fail(EXIT.USAGE, "--stage and --clear-stage cannot be used together");
  }
  if (opts.stage === undefined && !opts.clearStage && opts.nextAction === undefined && opts.metadata === undefined) {
    fail(EXIT.USAGE, "contacts outreach set requires --stage, --clear-stage, --next-action, and/or --metadata");
  }
  const body: UpsertEngagementRequest = {
    stage: opts.clearStage ? "" : opts.stage,
    metadata: jsonObject(opts.metadata, "--metadata"),
  };
  if (opts.nextAction !== undefined) {
    body.nextActionAt = opts.nextAction === "clear"
      ? null
      : dateValue(opts.nextAction, "--next-action");
  }
  const contacts = createClient().contacts;
  const agent = requireAgentEmail(opts.agent);
  const ifMatch = requireIfMatch(opts.ifMatch);
  const row = ifMatch
    ? await contacts.setOutreach(agent, address, body, { ifMatch })
    : await contacts.setOutreach(agent, address, body);
  process.stdout.write(opts.json ? JSON.stringify(row) + "\n" : row.address + "\n");
}

export async function outreachDelete(address: string, opts: {
  agent?: string;
  json?: boolean;
}): Promise<void> {
  const result = await createClient().contacts.deleteOutreach(requireAgentEmail(opts.agent), address);
  process.stdout.write(opts.json ? JSON.stringify(result) + "\n" : `un-enrolled ${result.address}\n`);
}
