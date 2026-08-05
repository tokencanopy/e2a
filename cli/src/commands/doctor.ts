import { Resolver } from "node:dns/promises";
import { createRequire } from "node:module";
import { E2AError } from "@e2a/sdk/v1";
import { loadConfig } from "../config.js";
import { createClient } from "../sdk.js";
import { EXIT, fail } from "../exit.js";

// `doctor` diagnoses the production email path WITHOUT creating, deleting, or
// mutating anything: it only issues GETs (plus client-side DNS lookups) and
// never calls the three endpoints with real-world side effects —
// POST /v1/domains/{d}/verify (flips verified state, enqueues SES
// provisioning), POST /v1/webhooks/{id}/test (delivers a real event to the
// customer's endpoint), POST /v1/agents/{email}/test (sends real mail).
// Webhook reachability therefore has no safe probe and is reported as an
// explicit skip, with recent delivery history as the observed signal.

export const DOCTOR_SCHEMA = "e2a.doctor/v1";
/** Hard per-operation deadline for every network call doctor makes. */
export const DOCTOR_TIMEOUT_MS = 5000;
export const EXIT_HEALTHY = EXIT.OK;

const HOSTED_URL = "https://e2a.dev";
const HOSTED_MCP_URL = "https://api.e2a.dev/mcp";
/** Enumeration caps — noted in the report evidence when hit, never silent. */
const MAX_DOMAINS = 50;
const MAX_WEBHOOKS = 50;

export type CheckStatus = "pass" | "warn" | "fail" | "skip";

export interface DoctorCheck {
  /** Stable machine ID (e.g. "domain.mx"). New IDs may be added; never renamed. */
  id: string;
  /** Stable human label for the check. */
  title: string;
  status: CheckStatus;
  /** Stable machine reason (e.g. "record_missing"). */
  reason_code: string;
  /** What was checked (URL, domain, record name, webhook id) when not global. */
  target?: string;
  /** One-line human explanation of the outcome. */
  detail: string;
  /** Check-specific structured facts. Never contains credentials. */
  evidence?: Record<string, unknown>;
  /** Concise fix, present on warn/fail (and some skips). */
  remediation?: string;
}

export interface DoctorReport {
  schema: typeof DOCTOR_SCHEMA;
  generated_at: string;
  cli_version: string;
  deployment_url: string;
  status: "healthy" | "warnings" | "failed";
  exit_code: number;
  summary: { pass: number; warn: number; fail: number; skip: number };
  checks: DoctorCheck[];
}

export interface DoctorOptions {
  agent?: string;
  domain?: string;
  mcpUrl?: string;
  json?: boolean;
}

/**
 * All I/O behind one seam so tests drive every status/exit path
 * deterministically. Every operation is bounded by DOCTOR_TIMEOUT_MS.
 */
export interface DoctorIO {
  now(): Date;
  cliVersion(): string;
  env(name: string): string | undefined;
  /** GET the URL; resolves for any HTTP response, throws on network failure. */
  httpGet(url: string): Promise<{ status: number; body: string }>;
  /** TXT records with chunks joined; [] when the name/records don't exist. */
  resolveTxt(name: string): Promise<string[]>;
  /** MX records; [] when the name/records don't exist. */
  resolveMx(name: string): Promise<Array<{ exchange: string; priority: number }>>;
}

const require = createRequire(import.meta.url);

/** Absolute http(s) URL or undefined. Doctor must never crash on bad input. */
export function parseHttpUrl(raw: string): URL | undefined {
  try {
    const url = new URL(raw);
    return url.protocol === "http:" || url.protocol === "https:" ? url : undefined;
  } catch {
    return undefined;
  }
}

/** URL safe to embed in a report: credentials stripped, unparseable left as-is. */
function redactUrl(raw: string): string {
  const url = parseHttpUrl(raw);
  if (!url || (!url.username && !url.password)) return raw;
  url.username = "";
  url.password = "";
  return url.toString();
}

export function defaultIO(): DoctorIO {
  // ENOTFOUND/ENODATA mean "no such record" — a definite answer, not an
  // error. Anything else (timeout, SERVFAIL, refused) is inconclusive and
  // propagates so the check reports dns_lookup_failed instead of a false
  // "record missing".
  const NO_RECORD = new Set(["ENOTFOUND", "ENODATA"]);
  const resolver = new Resolver({ timeout: DOCTOR_TIMEOUT_MS, tries: 1 });
  const noRecord = (err: unknown): boolean =>
    NO_RECORD.has((err as NodeJS.ErrnoException)?.code ?? "");
  // The Resolver `timeout` option is per-server, per-try: with several
  // nameservers configured one lookup could stack multiples of it. This race
  // enforces DOCTOR_TIMEOUT_MS as a hard per-operation bound regardless.
  const deadline = <T>(p: Promise<T>): Promise<T> => {
    let timer: NodeJS.Timeout;
    return Promise.race([
      p,
      new Promise<never>((_, reject) => {
        timer = setTimeout(
          () => reject(new Error(`DNS lookup timed out after ${DOCTOR_TIMEOUT_MS}ms`)),
          DOCTOR_TIMEOUT_MS,
        );
        timer.unref();
      }),
    ]).finally(() => clearTimeout(timer));
  };
  return {
    now: () => new Date(),
    cliVersion: () => (require("../../package.json") as { version: string }).version,
    env: (name) => process.env[name],
    httpGet: async (url) => {
      const res = await fetch(url, {
        signal: AbortSignal.timeout(DOCTOR_TIMEOUT_MS),
        headers: { accept: "application/json" },
      });
      return { status: res.status, body: await res.text() };
    },
    resolveTxt: async (name) => {
      try {
        return (await deadline(resolver.resolveTxt(name))).map((chunks) => chunks.join(""));
      } catch (err) {
        if (noRecord(err)) return [];
        throw err;
      }
    },
    resolveMx: async (name) => {
      try {
        return await deadline(resolver.resolveMx(name));
      } catch (err) {
        if (noRecord(err)) return [];
        throw err;
      }
    },
  };
}

// ---------------------------------------------------------------------------
// Check accumulation and exit-code computation
// ---------------------------------------------------------------------------

/** Which exit code a failing (or degraded) check argues for. */
type ExitClass = "auth" | "config" | "transient";

interface Recorded {
  check: DoctorCheck;
  exitClass?: ExitClass;
}

class Recorder {
  readonly recorded: Recorded[] = [];

  add(check: DoctorCheck, exitClass?: ExitClass): void {
    this.recorded.push({ check, exitClass });
  }

  pass(id: string, title: string, detail: string, extra: Partial<DoctorCheck> = {}): void {
    this.add({ id, title, status: "pass", reason_code: "ok", detail, ...extra });
  }

  skip(id: string, title: string, reason: string, detail: string, extra: Partial<DoctorCheck> = {}): void {
    this.add({ id, title, status: "skip", reason_code: reason, detail, ...extra });
  }

  warn(id: string, title: string, reason: string, detail: string, extra: Partial<DoctorCheck> = {}): void {
    this.add({ id, title, status: "warn", reason_code: reason, detail, ...extra });
  }

  fail(
    id: string,
    title: string,
    reason: string,
    exitClass: ExitClass,
    detail: string,
    extra: Partial<DoctorCheck> = {},
  ): void {
    this.add({ id, title, status: "fail", reason_code: reason, detail, ...extra }, exitClass);
  }
}

function buildReport(io: DoctorIO, deploymentUrl: string, rec: Recorder): DoctorReport {
  const checks = rec.recorded.map((r) => r.check);
  const summary = { pass: 0, warn: 0, fail: 0, skip: 0 };
  for (const c of checks) summary[c.status]++;

  // Severity order: a bad credential (4) must win over the config errors it
  // may cause downstream; a definite configuration failure (9) must win over
  // an incidental transient (1) so retry loops don't spin on a problem only
  // a human can fix; transient (1) means "retry doctor"; warnings (8) mean
  // "nothing broken, read the report".
  const classes = new Set(rec.recorded.map((r) => r.exitClass).filter(Boolean));
  let exitCode: number = EXIT.OK;
  if (classes.has("auth")) exitCode = EXIT.AUTH;
  else if (classes.has("config")) exitCode = EXIT.CONFIG;
  else if (classes.has("transient")) exitCode = EXIT.ERROR;
  else if (summary.warn > 0) exitCode = EXIT.WARN;

  return {
    schema: DOCTOR_SCHEMA,
    generated_at: io.now().toISOString(),
    cli_version: io.cliVersion(),
    deployment_url: deploymentUrl,
    status: summary.fail > 0 ? "failed" : summary.warn > 0 ? "warnings" : "healthy",
    exit_code: exitCode,
    summary,
    checks,
  };
}

// ---------------------------------------------------------------------------
// Error classification
// ---------------------------------------------------------------------------

type ErrorKind = "auth" | "not_found" | "transient" | "request";

function classifyError(err: unknown): { kind: ErrorKind; message: string } {
  if (err instanceof E2AError) {
    if (err.status === 401 || err.status === 403) return { kind: "auth", message: err.message };
    if (err.status === 404 || err.status === 410) return { kind: "not_found", message: err.message };
    if (err.status === 0 || err.status >= 500 || err.retryable) {
      return { kind: "transient", message: err.message };
    }
    return { kind: "request", message: err.message };
  }
  return { kind: "transient", message: err instanceof Error ? err.message : String(err) };
}

// ---------------------------------------------------------------------------
// Individual checks
// ---------------------------------------------------------------------------

interface Ctx {
  io: DoctorIO;
  rec: Recorder;
  config: ReturnType<typeof loadConfig>;
  opts: DoctorOptions;
  client?: ReturnType<typeof createClient>;
  apiReachable: boolean;
  /** Parsed /v1/info payload when reachable. */
  info?: { version?: string; shared_domain?: string; public_url?: string };
  account?: {
    user: { id: string; email: string };
    scope: string;
    planCode: string;
    agentEmail?: string;
  };
}

function checkCliConfig(ctx: Ctx): void {
  const { rec, io, config } = ctx;
  const keySource = io.env("E2A_API_KEY") ? "E2A_API_KEY" : config.api_key ? "~/.e2a/config.json" : "none";
  if (!config.api_key) {
    rec.fail(
      "cli.config",
      "CLI configuration",
      "no_api_key",
      "auth",
      `no API key configured; deployment ${config.api_url}`,
      {
        evidence: { deployment_url: config.api_url, key_source: keySource },
        remediation: "run `e2a login` (browser), or set E2A_API_KEY",
      },
    );
    return;
  }
  const evidence: Record<string, unknown> = {
    deployment_url: config.api_url,
    key_source: keySource,
  };
  if (config.key_scope) evidence.key_scope = config.key_scope;
  rec.pass("cli.config", "CLI configuration", `api key from ${keySource}; deployment ${config.api_url}`, {
    evidence,
  });
}

async function checkApiReachability(ctx: Ctx): Promise<void> {
  const { rec, io, config } = ctx;
  if (!parseHttpUrl(config.api_url)) {
    // A schemeless/garbled E2A_URL is a configuration problem, not a network
    // one — without this, fetch's URL-parse rejection would masquerade as a
    // retryable connection failure.
    rec.fail(
      "api.reachability",
      "API reachability",
      "invalid_deployment_url",
      "config",
      `${config.api_url} is not an absolute http(s) URL`,
      {
        target: config.api_url,
        remediation: "set E2A_URL to the deployment root, e.g. https://e2a.dev or https://mail.internal.example",
      },
    );
    return;
  }
  const url = `${config.api_url}/v1/info`;
  let res: { status: number; body: string };
  try {
    res = await io.httpGet(url);
  } catch (err) {
    rec.fail(
      "api.reachability",
      "API reachability",
      "connection_failed",
      "transient",
      `cannot reach ${url}`,
      {
        target: config.api_url,
        evidence: { error: err instanceof Error ? err.message : String(err) },
        remediation: "check the network and E2A_URL; for self-hosted, confirm the server is running",
      },
    );
    return;
  }
  if (res.status !== 200) {
    // 5xx/408/429: the deployment exists but is unhealthy or throttling —
    // retryable. Anything else (404 from some other website, 3xx chain
    // ending badly) means E2A_URL doesn't point at an e2a deployment root —
    // a configuration problem.
    const transient = res.status >= 500 || res.status === 408 || res.status === 429;
    rec.fail(
      "api.reachability",
      "API reachability",
      "http_error",
      transient ? "transient" : "config",
      `GET ${url} returned HTTP ${res.status}`,
      {
        target: config.api_url,
        evidence: { http_status: res.status },
        remediation: transient
          ? "the deployment is up but unhealthy — retry, then check server logs"
          : "E2A_URL must be the e2a deployment root (the host serving /v1)",
      },
    );
    return;
  }
  let info: Ctx["info"];
  try {
    info = JSON.parse(res.body) as Ctx["info"];
  } catch {
    info = undefined;
  }
  if (!info || typeof info.version !== "string") {
    rec.fail(
      "api.reachability",
      "API reachability",
      "unexpected_response",
      "config",
      `GET ${url} did not return a /v1/info payload`,
      {
        target: config.api_url,
        remediation: "E2A_URL must be the e2a deployment root (the host serving /v1)",
      },
    );
    return;
  }
  ctx.apiReachable = true;
  ctx.info = info;
  const evidence: Record<string, unknown> = { version: info.version };
  if (info.shared_domain) evidence.shared_domain = info.shared_domain;
  if (info.public_url) evidence.public_url = info.public_url;
  rec.pass("api.reachability", "API reachability", `server version ${info.version}`, {
    target: config.api_url,
    evidence,
  });
}

async function checkApiAuth(ctx: Ctx): Promise<void> {
  const { rec } = ctx;
  if (!ctx.config.api_key) {
    rec.skip("api.auth", "API credentials", "no_api_key", "skipped: no API key configured");
    return;
  }
  if (!ctx.apiReachable) {
    rec.skip("api.auth", "API credentials", "blocked_by_failure", "skipped: API unreachable", {
      evidence: { blocked_on: "api.reachability" },
    });
    return;
  }
  try {
    const account = await ctx.client!.account.get();
    ctx.account = account;
    const evidence: Record<string, unknown> = {
      user_email: account.user.email,
      scope: account.scope,
      plan_code: account.planCode,
    };
    if (account.agentEmail) evidence.bound_agent = account.agentEmail;
    rec.pass(
      "api.auth",
      "API credentials",
      `key valid — scope ${account.scope}, plan ${account.planCode}`,
      { evidence },
    );
  } catch (err) {
    const { kind, message } = classifyError(err);
    if (kind === "auth") {
      rec.fail("api.auth", "API credentials", "unauthorized", "auth", `key rejected: ${message}`, {
        remediation: "run `e2a login` again, or set a valid E2A_API_KEY (e2a keys list)",
      });
    } else if (kind === "transient") {
      rec.fail("api.auth", "API credentials", "connection_failed", "transient", message);
    } else {
      rec.fail("api.auth", "API credentials", "http_error", "config", message);
    }
  }
}

async function checkAgentAccess(ctx: Ctx): Promise<void> {
  const { rec, opts, config } = ctx;
  const email = opts.agent || config.agent_email || ctx.account?.agentEmail || "";
  if (!ctx.config.api_key || !ctx.apiReachable || !ctx.account) {
    rec.skip("agent.access", "Agent access", "blocked_by_failure", "skipped: API credentials not verified", {
      evidence: { blocked_on: ctx.config.api_key ? (ctx.apiReachable ? "api.auth" : "api.reachability") : "cli.config" },
    });
    return;
  }
  if (!email) {
    rec.skip(
      "agent.access",
      "Agent access",
      "no_agent_selected",
      "skipped: no agent selected",
      { remediation: "pass --agent, set E2A_AGENT_EMAIL, or run `e2a config set agent_email <email>`" },
    );
    return;
  }
  try {
    const agent = await ctx.client!.agents.get(email);
    rec.pass(
      "agent.access",
      "Agent access",
      `agent exists${agent.domainVerified ? "" : " (domain unverified)"}`,
      {
        target: email,
        evidence: { email: agent.email, domain: agent.domain, domain_verified: agent.domainVerified },
      },
    );
  } catch (err) {
    const { kind, message } = classifyError(err);
    if (kind === "not_found") {
      rec.fail("agent.access", "Agent access", "agent_not_found", "config", `no agent ${email}`, {
        target: email,
        remediation: "check the address (`e2a agents list`) or create it (`e2a agents create`)",
      });
    } else if (kind === "auth") {
      rec.fail("agent.access", "Agent access", "forbidden", "auth", `key cannot access ${email}: ${message}`, {
        target: email,
        remediation: "this key is bound to a different inbox — use that inbox or an account-scoped key",
      });
    } else if (kind === "transient") {
      rec.fail("agent.access", "Agent access", "connection_failed", "transient", message, { target: email });
    } else {
      rec.fail("agent.access", "Agent access", "http_error", "config", message, { target: email });
    }
  }
}

// --- domains ---------------------------------------------------------------

interface DomainRecord {
  type: string;
  name: string;
  value: string;
  priority: number | null;
  purpose: string;
  status: string;
}

interface DomainLike {
  domain: string;
  verified: boolean;
  agentCount: number;
  sendingStatus: string;
  sendingError?: string;
  dnsRecords: DomainRecord[];
}

async function collect<T>(pager: AsyncIterable<T>, max: number): Promise<{ items: T[]; truncated: boolean }> {
  const items: T[] = [];
  for await (const item of pager) {
    if (items.length >= max) return { items, truncated: true };
    items.push(item);
  }
  return { items, truncated: false };
}

const normHost = (h: string): string => h.toLowerCase().replace(/\.$/, "");

/**
 * The p= payload of a DKIM TXT record, whitespace/quote-stripped — mirrors
 * the server's dkim.ExtractPublicKeyFromTXT: only the key material is
 * compared, so extra tags (s=, t=, …) and tag reordering that operators
 * paste are tolerated exactly like the server's own verifier tolerates them.
 */
export function extractDkimKey(txt: string): string {
  const i = txt.indexOf("p=");
  if (i < 0) return "";
  let tail = txt.slice(i + 2);
  const j = tail.indexOf(";");
  if (j >= 0) tail = tail.slice(0, j);
  return tail.replace(/[\s"]/g, "");
}

async function checkTxtRecord(
  ctx: Ctx,
  id: string,
  title: string,
  domain: string,
  record: DomainRecord,
): Promise<void> {
  const { rec, io } = ctx;
  const dkim = record.purpose === "dkim";
  let published: string[];
  try {
    published = await io.resolveTxt(record.name);
  } catch (err) {
    rec.fail(id, title, "dns_lookup_failed", "transient", `TXT lookup for ${record.name} failed`, {
      target: record.name,
      evidence: { error: err instanceof Error ? err.message : String(err) },
      remediation: `DNS lookup was inconclusive — retry, or check the record with \`dig TXT '${record.name}'\``,
    });
    return;
  }
  // Match with the server's OWN probe semantics (internal/agent/api.go
  // checkDomainRecords + classifyDKIM), so doctor never fails a record the
  // server accepts: ownership is a substring match (any TXT containing the
  // token), SPF is case-insensitive starts-with-v=spf1 plus contains the
  // prescribed include, DKIM compares only the extracted p= payload.
  const spf = record.value.trim().toLowerCase().startsWith("v=spf1");
  const spfInclude = spf
    ? record.value.toLowerCase().split(/\s+/).find((t) => t.startsWith("include:"))
    : undefined;
  const matches = dkim
    ? published.some((v) => extractDkimKey(v) !== "" && extractDkimKey(v) === extractDkimKey(record.value))
    : spf && spfInclude
      ? published.some((v) => v.trim().toLowerCase().startsWith("v=spf1") && v.toLowerCase().includes(spfInclude))
      : record.purpose === "ownership"
        ? published.some((v) => v.includes(record.value))
        : published.some((v) => v.trim() === record.value.trim());
  if (matches) {
    rec.pass(id, title, `${domain}: TXT record found at ${record.name}`, {
      target: record.name,
      evidence: { expected: record.value, server_status: record.status },
    });
    return;
  }
  // A same-purpose record with a different value is a mismatch (usually a
  // truncated or stale copy — the distinction tells the user to re-publish
  // the full value rather than "add the record"); otherwise the record is
  // simply missing among unrelated TXT entries.
  const conflicting = dkim
    ? published.filter((v) => extractDkimKey(v) !== "")
    : spf
      ? published.filter((v) => v.trim().toLowerCase().startsWith("v=spf1"))
      : [];
  if (conflicting.length > 0) {
    rec.fail(id, title, "record_mismatch", "config", `${domain}: TXT at ${record.name} does not match`, {
      target: record.name,
      evidence: { expected: record.value, found: conflicting, server_status: record.status },
      remediation: `replace the TXT record at ${record.name} with "${record.value}"`,
    });
    return;
  }
  rec.fail(id, title, "record_missing", "config", `${domain}: TXT record missing at ${record.name}`, {
    target: record.name,
    evidence: { expected: record.value, server_status: record.status },
    remediation: `add TXT record ${record.name} with value "${record.value}"`,
  });
}

async function checkMxRecord(
  ctx: Ctx,
  id: string,
  title: string,
  domain: string,
  record: DomainRecord,
  extraEvidence: Record<string, unknown> = {},
): Promise<void> {
  const { rec, io } = ctx;
  let published: Array<{ exchange: string; priority: number }>;
  try {
    published = await io.resolveMx(record.name);
  } catch (err) {
    rec.fail(id, title, "dns_lookup_failed", "transient", `MX lookup for ${record.name} failed`, {
      target: record.name,
      evidence: { error: err instanceof Error ? err.message : String(err), ...extraEvidence },
      remediation: `DNS lookup was inconclusive — retry, or check the record with \`dig MX '${record.name}'\``,
    });
    return;
  }
  const remediation = `add MX record ${record.name} → ${record.value}` +
    (record.priority !== null ? ` (priority ${record.priority})` : "");
  if (published.some((mx) => normHost(mx.exchange) === normHost(record.value))) {
    rec.pass(id, title, `${domain}: MX record found (${record.value})`, {
      target: record.name,
      evidence: { expected: record.value, server_status: record.status, ...extraEvidence },
    });
  } else if (published.length > 0) {
    rec.fail(id, title, "record_mismatch", "config", `${domain}: MX at ${record.name} points elsewhere`, {
      target: record.name,
      evidence: {
        expected: record.value,
        found: published.map((mx) => normHost(mx.exchange)).sort(),
        server_status: record.status,
        ...extraEvidence,
      },
      remediation,
    });
  } else {
    rec.fail(id, title, "record_missing", "config", `${domain}: MX record missing — expected ${record.value}`, {
      target: record.name,
      evidence: { expected: record.value, server_status: record.status, ...extraEvidence },
      remediation,
    });
  }
}

async function checkOneDomain(ctx: Ctx, domain: DomainLike): Promise<void> {
  const { rec, io } = ctx;
  const d = domain.domain;
  rec.pass("domain.registered", "Domain registration", `${d}: registered${domain.verified ? ", verified" : ", not yet verified"}`, {
    target: d,
    evidence: { verified: domain.verified, sending_status: domain.sendingStatus, agent_count: domain.agentCount },
  });

  const byPurpose = new Map<string, DomainRecord>();
  for (const r of domain.dnsRecords) byPurpose.set(r.purpose, r);

  const ownership = byPurpose.get("ownership");
  if (ownership) await checkTxtRecord(ctx, "domain.ownership", "Ownership TXT record", d, ownership);

  const mx = byPurpose.get("inbound_mx");
  if (mx) {
    const wildcardPrescribed = byPurpose.has("inbound_mx_wildcard");
    await checkMxRecord(
      ctx,
      "domain.mx",
      "Inbound MX record",
      d,
      mx,
      wildcardPrescribed ? { wildcard_prescribed: true } : {},
    );
  }

  const dkim = byPurpose.get("dkim");
  if (dkim) {
    await checkTxtRecord(ctx, "domain.dkim", "DKIM TXT record", d, dkim);
  } else {
    rec.skip("domain.dkim", "DKIM TXT record", "not_prescribed", `${d}: no DKIM record prescribed yet`, {
      target: d,
      remediation: "DKIM is provisioned after domain verification — publish the ownership TXT and inbound MX first",
    });
  }

  const mailFromMx = byPurpose.get("mail_from_mx");
  if (mailFromMx) {
    await checkMxRecord(ctx, "domain.mailfrom_mx", "MAIL FROM MX record", d, mailFromMx);
  } else {
    rec.skip("domain.mailfrom_mx", "MAIL FROM MX record", "not_prescribed", `${d}: no MAIL FROM MX prescribed`, {
      target: d,
    });
  }

  const spf = byPurpose.get("mail_from_spf");
  if (spf) {
    await checkTxtRecord(ctx, "domain.spf", "SPF TXT record", d, spf);
  } else {
    rec.skip("domain.spf", "SPF TXT record", "not_prescribed", `${d}: no SPF record prescribed`, {
      target: d,
    });
  }

  // DMARC is advisory: e2a does not prescribe a record, but receivers
  // increasingly require one for inbox placement. Warn-only, never fail.
  const dmarcName = `_dmarc.${d}`;
  try {
    const dmarc = (await io.resolveTxt(dmarcName)).filter((v) => v.trim().startsWith("v=DMARC1"));
    if (dmarc.length > 0) {
      rec.pass("domain.dmarc", "DMARC record (advisory)", `${d}: DMARC record present`, {
        target: dmarcName,
        evidence: { record: dmarc[0] },
      });
    } else {
      rec.warn("domain.dmarc", "DMARC record (advisory)", "no_dmarc_record", `${d}: no DMARC record`, {
        target: dmarcName,
        remediation: `add TXT record ${dmarcName} with value "v=DMARC1; p=none;" and tighten the policy once reports look clean`,
      });
    }
  } catch (err) {
    // Advisory check, advisory outcome: an inconclusive lookup here must not
    // fail the run — DMARC can never fail doctor, per its warn-only contract.
    rec.warn("domain.dmarc", "DMARC record (advisory)", "dns_lookup_failed", `TXT lookup for ${dmarcName} was inconclusive`, {
      target: dmarcName,
      evidence: { error: err instanceof Error ? err.message : String(err) },
    });
  }

  switch (domain.sendingStatus) {
    case "verified":
      rec.pass("domain.sending", "Sending status", `${d}: sending identity verified`, { target: d });
      break;
    case "pending":
      rec.warn("domain.sending", "Sending status", "sending_pending", `${d}: sending identity still pending`, {
        target: d,
        remediation: "publish the prescribed DNS records (if managed by Cloudflare, add Cloudflare MCP so your coding agent can set them up) and allow time for propagation, then re-run doctor",
      });
      break;
    case "failed":
      rec.fail("domain.sending", "Sending status", "sending_failed", "config", `${d}: sending identity failed`, {
        target: d,
        evidence: { sending_error: domain.sendingError ?? "" },
        remediation: "fix the reported record, then re-verify the domain from the dashboard or MCP tools",
      });
      break;
    default:
      rec.skip("domain.sending", "Sending status", "sending_not_provisioned", `${d}: sending identity not provisioned`, {
        target: d,
      });
  }
}

async function checkDomains(ctx: Ctx): Promise<void> {
  const { rec, opts } = ctx;
  if (!ctx.account) {
    rec.skip("domain.registered", "Domain registration", "blocked_by_failure", "skipped: API credentials not verified", {
      evidence: { blocked_on: "api.auth" },
    });
    return;
  }
  if (ctx.account.scope !== "account") {
    rec.skip(
      "domain.registered",
      "Domain registration",
      "requires_account_scope",
      "skipped: domain checks need an account-scoped key",
      { remediation: "run doctor with an account-scoped key (`e2a login`) to check custom domains" },
    );
    return;
  }
  let domains: DomainLike[];
  let truncated = false;
  try {
    if (opts.domain) {
      domains = [(await ctx.client!.domains.get(opts.domain)) as unknown as DomainLike];
    } else {
      const collected = await collect(ctx.client!.domains.list({}) as AsyncIterable<DomainLike>, MAX_DOMAINS);
      domains = collected.items;
      truncated = collected.truncated;
    }
  } catch (err) {
    const { kind, message } = classifyError(err);
    if (kind === "not_found" && opts.domain) {
      rec.fail("domain.registered", "Domain registration", "not_registered", "config", `${opts.domain} is not registered on this account`, {
        target: opts.domain,
        remediation: "register it first (dashboard or MCP `register_domain`), or check the spelling",
      });
    } else if (kind === "transient") {
      rec.fail("domain.registered", "Domain registration", "connection_failed", "transient", message);
    } else {
      rec.fail("domain.registered", "Domain registration", "http_error", "config", message);
    }
    return;
  }
  if (domains.length === 0) {
    rec.skip("domain.registered", "Domain registration", "no_domains", "no custom domains registered — using the shared domain", {
      evidence: ctx.info?.shared_domain ? { shared_domain: ctx.info.shared_domain } : undefined,
    });
    return;
  }
  if (truncated) {
    rec.warn("domain.registered", "Domain registration", "too_many_domains", `only the first ${MAX_DOMAINS} domains were checked`, {
      evidence: { checked: MAX_DOMAINS },
      remediation: "re-run with --domain <domain> to check a specific domain",
    });
  }
  for (const domain of domains) {
    // One malformed domain object must not cost the rest of the report.
    try {
      await checkOneDomain(ctx, domain);
    } catch (err) {
      rec.fail("doctor.internal", "Doctor internal error", "internal_error", "transient", `${domain?.domain ?? "domain"}: unexpected error: ${err instanceof Error ? err.message : String(err)}`, {
        target: domain?.domain,
      });
    }
  }
}

// --- MCP ---------------------------------------------------------------------

async function checkMcp(ctx: Ctx): Promise<void> {
  const { rec, io, opts, config } = ctx;
  const mcpUrl = opts.mcpUrl ?? (config.api_url === HOSTED_URL ? HOSTED_MCP_URL : undefined);
  if (!mcpUrl) {
    const detail = "skipped: no MCP endpoint known for this deployment";
    const remediation = "pass --mcp-url https://<host>/mcp to probe a self-hosted MCP server";
    rec.skip("mcp.reachability", "MCP reachability", "no_mcp_url", detail, { remediation });
    rec.skip("mcp.auth_metadata", "MCP auth metadata", "no_mcp_url", detail, { remediation });
    return;
  }
  // The doctor() wrapper rejects a malformed --mcp-url as a usage error;
  // this guard keeps runDoctor crash-free for programmatic callers too.
  const parsed = parseHttpUrl(mcpUrl);
  if (!parsed) {
    const detail = `${mcpUrl} is not an absolute http(s) URL`;
    const remediation = "pass --mcp-url as an absolute URL, e.g. https://host.example/mcp";
    rec.fail("mcp.reachability", "MCP reachability", "invalid_mcp_url", "config", detail, {
      target: mcpUrl,
      remediation,
    });
    rec.fail("mcp.auth_metadata", "MCP auth metadata", "invalid_mcp_url", "config", detail, {
      target: mcpUrl,
      remediation,
    });
    return;
  }
  try {
    // 405 (GET on the POST-only JSON-RPC endpoint) and 401 (no token) both
    // prove the endpoint is routed and alive; 404/410 mean the path is NOT
    // routed — the exact misconfiguration this check exists to catch. GET
    // carries no JSON-RPC payload → no side effects.
    const res = await io.httpGet(mcpUrl);
    if (res.status === 404 || res.status === 410) {
      rec.fail("mcp.reachability", "MCP reachability", "not_routed", "config", `HTTP ${res.status} — no MCP endpoint at ${mcpUrl}`, {
        target: mcpUrl,
        evidence: { http_status: res.status },
        remediation: "check the MCP path (hosted: https://api.e2a.dev/mcp; self-hosted: usually /mcp on the MCP server host)",
      });
    } else if (res.status >= 500) {
      rec.fail("mcp.reachability", "MCP reachability", "http_error", "transient", `HTTP ${res.status} from ${mcpUrl}`, {
        target: mcpUrl,
        evidence: { http_status: res.status },
        remediation: "the MCP endpoint is routed but erroring — retry, then check the MCP server logs",
      });
    } else {
      rec.pass("mcp.reachability", "MCP reachability", `endpoint responded (HTTP ${res.status})`, {
        target: mcpUrl,
        evidence: { http_status: res.status },
      });
    }
  } catch (err) {
    rec.fail("mcp.reachability", "MCP reachability", "connection_failed", "transient", `cannot reach ${mcpUrl}`, {
      target: mcpUrl,
      evidence: { error: err instanceof Error ? err.message : String(err) },
      remediation: "check the MCP host and network; for self-hosted, confirm the MCP server is running",
    });
  }

  const metadataUrl = `${parsed.origin}/.well-known/oauth-protected-resource`;
  try {
    const res = await io.httpGet(metadataUrl);
    if (res.status !== 200) {
      rec.warn("mcp.auth_metadata", "MCP auth metadata", "metadata_missing", `HTTP ${res.status} from ${metadataUrl}`, {
        target: metadataUrl,
        evidence: { http_status: res.status },
        remediation: "OAuth clients discover the authorization server here (RFC 9728); interactive MCP login may fail without it",
      });
      return;
    }
    let meta: { authorization_servers?: unknown; scopes_supported?: unknown };
    try {
      meta = JSON.parse(res.body) as typeof meta;
    } catch {
      meta = {};
    }
    if (!Array.isArray(meta.authorization_servers) || meta.authorization_servers.length === 0) {
      rec.warn("mcp.auth_metadata", "MCP auth metadata", "metadata_invalid", `no authorization_servers advertised at ${metadataUrl}`, {
        target: metadataUrl,
      });
      return;
    }
    rec.pass("mcp.auth_metadata", "MCP auth metadata", `advertises ${(meta.authorization_servers as string[]).join(", ")}`, {
      target: metadataUrl,
      evidence: {
        authorization_servers: meta.authorization_servers,
        scopes_supported: meta.scopes_supported,
      },
    });
  } catch (err) {
    rec.fail("mcp.auth_metadata", "MCP auth metadata", "connection_failed", "transient", `cannot reach ${metadataUrl}`, {
      target: metadataUrl,
      evidence: { error: err instanceof Error ? err.message : String(err) },
    });
  }
}

// --- webhooks -----------------------------------------------------------------

interface WebhookLike {
  id: string;
  url: string;
  enabled: boolean;
  events: string[];
  autoDisabledAt?: Date;
  lastDeliveredAt?: Date;
}

interface DeliveryLike {
  status: string;
  attempts: number;
  lastError?: string;
  lastStatusCode?: number;
  lastAttemptAt?: Date;
}

async function checkWebhooks(ctx: Ctx): Promise<void> {
  const { rec } = ctx;
  if (!ctx.account) {
    rec.skip("webhook.config", "Webhook configuration", "blocked_by_failure", "skipped: API credentials not verified", {
      evidence: { blocked_on: "api.auth" },
    });
    return;
  }
  if (ctx.account.scope !== "account") {
    rec.skip(
      "webhook.config",
      "Webhook configuration",
      "requires_account_scope",
      "skipped: webhook checks need an account-scoped key",
      { remediation: "run doctor with an account-scoped key (`e2a login`) to check webhooks" },
    );
    return;
  }
  let webhooks: WebhookLike[];
  let truncated = false;
  try {
    const collected = await collect(ctx.client!.webhooks.list({}) as AsyncIterable<WebhookLike>, MAX_WEBHOOKS);
    webhooks = collected.items;
    truncated = collected.truncated;
  } catch (err) {
    const { kind, message } = classifyError(err);
    rec.fail(
      "webhook.config",
      "Webhook configuration",
      kind === "transient" ? "connection_failed" : "http_error",
      kind === "transient" ? "transient" : "config",
      message,
    );
    return;
  }
  if (webhooks.length === 0) {
    rec.skip("webhook.config", "Webhook configuration", "no_webhooks", "no webhooks configured — agents rely on WebSocket, polling, or MCP", {});
    return;
  }
  if (truncated) {
    rec.warn("webhook.config", "Webhook configuration", "too_many_webhooks", `only the first ${MAX_WEBHOOKS} webhooks were checked`, {
      evidence: { checked: MAX_WEBHOOKS },
    });
  }

  for (const wh of webhooks) {
    // One malformed webhook object must not cost the rest of the report.
    try {
      await checkOneWebhook(ctx, wh);
    } catch (err) {
      rec.fail("doctor.internal", "Doctor internal error", "internal_error", "transient", `webhook ${wh.id}: unexpected error: ${err instanceof Error ? err.message : String(err)}`, {
        target: wh.id,
      });
    }
  }

  rec.skip(
    "webhook.reachability",
    "Webhook reachability",
    "no_safe_probe",
    "skipped: no safe non-delivering probe exists (POST /v1/webhooks/{id}/test delivers a real event) — webhook.delivery reflects observed reachability",
    {},
  );
}

async function checkOneWebhook(ctx: Ctx, wh: WebhookLike): Promise<void> {
  const { rec } = ctx;
  // Webhook URLs can carry basic-auth userinfo; never echo credentials.
  const target = redactUrl(wh.url);
  if (wh.autoDisabledAt) {
    rec.fail("webhook.config", "Webhook configuration", "webhook_auto_disabled", "config", `${wh.id}: auto-disabled after repeated delivery failures`, {
      target,
      evidence: { id: wh.id, auto_disabled_at: new Date(wh.autoDisabledAt).toISOString() },
      remediation: "fix the receiving endpoint, then re-enable the webhook (dashboard or MCP `update_webhook`)",
    });
  } else if (!wh.enabled) {
    rec.warn("webhook.config", "Webhook configuration", "webhook_disabled", `${wh.id}: disabled`, {
      target,
      evidence: { id: wh.id },
      remediation: "events are not being delivered — re-enable it if that is unintended",
    });
  } else if (!wh.url.startsWith("https://")) {
    rec.warn("webhook.config", "Webhook configuration", "insecure_url", `${wh.id}: URL is not HTTPS`, {
      target,
      evidence: { id: wh.id },
      remediation: "use an HTTPS endpoint — production deployments reject plain HTTP webhook URLs",
    });
  } else {
    rec.pass("webhook.config", "Webhook configuration", `${wh.id}: enabled, ${wh.events.length} event type(s)`, {
      target,
      evidence: { id: wh.id, events: wh.events },
    });
  }

  try {
    // Deliveries come newest-first; only the latest outcome matters here,
    // so fetch exactly one — never a second page.
    const deliveries = (await collect(
      ctx.client!.webhooks.deliveries(wh.id, { limit: 1 }) as AsyncIterable<DeliveryLike>,
      1,
    )).items;
    const latest = deliveries[0];
    if (!latest || (latest.status === "pending" && latest.attempts === 0)) {
      rec.skip("webhook.delivery", "Webhook delivery history", "no_deliveries", `${wh.id}: no completed deliveries yet`, {
        target: wh.id,
      });
    } else if (latest.status === "delivered") {
      const evidence: Record<string, unknown> = { last_status: "delivered" };
      if (latest.lastStatusCode !== undefined) evidence.last_status_code = latest.lastStatusCode;
      rec.pass("webhook.delivery", "Webhook delivery history", `${wh.id}: latest delivery succeeded`, {
        target: wh.id,
        evidence,
      });
    } else {
      rec.warn("webhook.delivery", "Webhook delivery history", "deliveries_failing", `${wh.id}: latest delivery ${latest.status}`, {
        target: wh.id,
        evidence: {
          last_status: latest.status,
          last_error: latest.lastError ?? "",
          last_status_code: latest.lastStatusCode ?? 0,
          attempts: latest.attempts,
        },
        remediation: "check the receiving endpoint's logs and TLS setup; deliveries retry with backoff",
      });
    }
  } catch (err) {
    const { kind, message } = classifyError(err);
    rec.fail(
      "webhook.delivery",
      "Webhook delivery history",
      kind === "transient" ? "connection_failed" : "http_error",
      kind === "transient" ? "transient" : "config",
      `${wh.id}: ${message}`,
      { target: wh.id },
    );
  }
}

// --- outbound SMTP visibility ---------------------------------------------------

function checkSmtpConfig(ctx: Ctx): void {
  const { rec, io } = ctx;
  const host = io.env("E2A_OUTBOUND_SMTP_HOST");
  const port = io.env("E2A_OUTBOUND_SMTP_PORT");
  const fromDomain = io.env("E2A_OUTBOUND_SMTP_FROM_DOMAIN");
  const credentialsSet = Boolean(io.env("E2A_OUTBOUND_SMTP_USERNAME") || io.env("E2A_OUTBOUND_SMTP_PASSWORD"));
  if (!host && !port && !fromDomain) {
    rec.skip(
      "smtp.config",
      "Outbound SMTP configuration",
      "not_visible",
      "no E2A_OUTBOUND_SMTP_* environment visible from this machine (normal for hosted e2a; self-hosted operators should run doctor on the server host)",
      {},
    );
    return;
  }
  // Never echo E2A_OUTBOUND_SMTP_USERNAME / _PASSWORD — presence only.
  const evidence: Record<string, unknown> = {
    host: host ?? "",
    port: port ?? "",
    from_domain: fromDomain ?? "",
    credentials: credentialsSet ? "set" : "not_set",
  };
  if (host && fromDomain) {
    rec.pass("smtp.config", "Outbound SMTP configuration", `outbound SMTP via ${host}${port ? `:${port}` : ""}, from_domain ${fromDomain}`, {
      evidence,
    });
  } else {
    rec.warn("smtp.config", "Outbound SMTP configuration", "smtp_partial", "outbound SMTP configuration is incomplete", {
      evidence,
      remediation: "set E2A_OUTBOUND_SMTP_HOST, E2A_OUTBOUND_SMTP_PORT, and E2A_OUTBOUND_SMTP_FROM_DOMAIN together",
    });
  }
}

// ---------------------------------------------------------------------------
// Runner + output
// ---------------------------------------------------------------------------

export async function runDoctor(opts: DoctorOptions, io: DoctorIO): Promise<DoctorReport> {
  const config = { ...loadConfig() };
  // A trailing slash on E2A_URL must not break probe URLs or hosted detection.
  config.api_url = config.api_url.replace(/\/+$/, "") || config.api_url;
  const rec = new Recorder();
  const ctx: Ctx = { io, rec, config, opts, apiReachable: false };
  if (config.api_key) {
    // Pass the normalized config through so the SDK client hits the exact
    // same base URL the probes use (and the config file isn't parsed twice).
    ctx.client = createClient({ timeoutMs: DOCTOR_TIMEOUT_MS, maxRetries: 0 }, config);
  }

  checkCliConfig(ctx);
  await checkApiReachability(ctx);
  await checkApiAuth(ctx);
  await checkAgentAccess(ctx);
  await checkDomains(ctx);
  await checkMcp(ctx);
  await checkWebhooks(ctx);
  checkSmtpConfig(ctx);

  return buildReport(io, config.api_url, rec);
}

// Details and remediations interpolate server- and DNS-derived strings; strip
// control characters (including ANSI escapes) so a hostile record can't
// inject terminal escapes into the report. JSON output is already safe via
// JSON.stringify's escaping.
const stripControl = (s: string): string => s.replace(/[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/g, "");

function renderHuman(report: DoctorReport): string {
  const lines: string[] = [
    `doctor: read-only diagnostics for ${report.deployment_url} (sends no mail; changes no DNS or webhooks)`,
    "",
  ];
  for (const c of report.checks) {
    lines.push(`${c.status.padEnd(4)}  ${c.id.padEnd(22)}${c.detail}`);
    // Skips carry guidance too (e.g. how to enable the MCP probe) — print
    // every remediation, not just failing ones.
    if (c.remediation) {
      lines.push(`      fix: ${c.remediation}`);
    }
  }
  const s = report.summary;
  lines.push(
    "",
    `${s.fail} fail, ${s.warn} warn, ${s.pass} pass, ${s.skip} skip — ${report.status} (exit ${report.exit_code})`,
  );
  return lines.map(stripControl).join("\n") + "\n";
}

export async function doctor(opts: DoctorOptions, io: DoctorIO = defaultIO()): Promise<void> {
  if (opts.mcpUrl !== undefined && !parseHttpUrl(opts.mcpUrl)) {
    fail(EXIT.USAGE, "--mcp-url must be an absolute http(s) URL, e.g. https://host.example/mcp");
  }
  const report = await runDoctor(opts, io);
  if (opts.json) {
    process.stdout.write(JSON.stringify(report, null, 2) + "\n");
  } else {
    process.stdout.write(renderHuman(report));
  }
  // exitCode (not process.exit) so stdout flushes; doctor holds no sockets open.
  process.exitCode = report.exit_code;
}
