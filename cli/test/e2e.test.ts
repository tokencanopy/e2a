/**
 * Live binary-spawn parity harness for the CLI against a RUNNING server (staging).
 *
 * Spawns the ACTUAL built binary (dist/bin/e2a.js) and asserts on its --json
 * output and its frozen exit codes (src/exit.ts) — so a green run attests the
 * shipped CLI works end-to-end against a live deployment. The CLI is a deliberate
 * SUBSET of the API (no `domains`, no `agents delete`), so this exercises the real
 * parity surface only.
 *
 * Gated on staging creds; skips cleanly when absent (kept OUT of the default
 * `vitest run`, whose include is src/**). Run:
 *   npm run build && \
 *   E2A_URL=… E2A_API_KEY=… E2A_SHARED_DOMAIN=… npm run test:e2e --workspace @e2a/cli
 *
 * Env (note: the CLI reads E2A_URL, NOT E2A_API_URL):
 *   E2A_URL             staging base URL (or a local tunnel)
 *   E2A_API_KEY         an account-scoped key for the target account
 *   E2A_SHARED_DOMAIN   shared domain for throwaway agents (e.g. agents-staging.e2a.dev)
 */
import { describe, it, expect, afterAll } from "vitest";
import { spawnSync, spawn } from "node:child_process";
import { writeFileSync, rmSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { parseHelpCommands, recordAdvertised, recordCovered, flushCliCoverage } from "./harness/cli-coverage.js";

const CLI = fileURLToPath(new URL("../dist/bin/e2a.js", import.meta.url));

const URL_ = process.env.E2A_URL || "";
const KEY = process.env.E2A_API_KEY || "";
const DOMAIN = process.env.E2A_SHARED_DOMAIN || (process.env.E2A_AGENT_EMAIL || "").split("@")[1] || "";
const live = Boolean(URL_ && KEY && DOMAIN);

interface Run {
  code: number;
  stdout: string;
  stderr: string;
}

// run spawns the built CLI with the staging env. HOME is isolated so a real
// ~/.e2a/config can't influence the run (env still wins over file regardless).
function run(args: string[], extra: Record<string, string> = {}): Run {
  const env: Record<string, string | undefined> = {
    ...process.env,
    E2A_URL: URL_,
    E2A_API_KEY: KEY,
    HOME: "/tmp/e2a-cli-e2e-home",
    ...extra,
  };
  // CRITICAL: the CLI entrypoint skips main() when VITEST_WORKER_ID is set (its
  // in-process import guard). We spawn the REAL binary, so those vitest markers
  // must not leak into the child or every command no-ops with exit 0 / no output.
  delete env.VITEST_WORKER_ID;
  delete env.VITEST;
  delete env.VITEST_POOL_ID;
  const r = spawnSync("node", [CLI, ...args], { encoding: "utf8", env });
  return { code: r.status ?? -1, stdout: r.stdout ?? "", stderr: r.stderr ?? "" };
}

// Async counterpart to run(), for commands that must be started and left
// running WHILE another `run()` call happens concurrently (only `listen`
// needs this — every other command is a single synchronous request/response
// round-trip). Same VITEST_* stripping as run(); see its comment above for
// why that stripping is load-bearing.
function runAsync(args: string[], extra: Record<string, string> = {}): Promise<Run> {
  const env: Record<string, string | undefined> = {
    ...process.env,
    E2A_URL: URL_,
    E2A_API_KEY: KEY,
    HOME: "/tmp/e2a-cli-e2e-home",
    ...extra,
  };
  delete env.VITEST_WORKER_ID;
  delete env.VITEST;
  delete env.VITEST_POOL_ID;
  return new Promise((resolve) => {
    const child = spawn("node", [CLI, ...args], { env });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (d) => (stdout += d));
    child.stderr.on("data", (d) => (stderr += d));
    child.on("close", (code) => resolve({ code: code ?? -1, stdout, stderr }));
  });
}

const sleep = (ms: number) => new Promise((res) => setTimeout(res, ms));

// The CLI can't delete agents; clean up created inboxes over the API.
async function apiDeleteAgent(email: string): Promise<string | undefined> {
  try {
    const res = await fetch(`${URL_}/v1/agents/${encodeURIComponent(email)}?confirm=DELETE`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${KEY}` },
    });
    if (res.ok) return undefined;
    return `${email}: HTTP ${res.status} ${res.statusText}: ${(await res.text()).slice(0, 200)}`;
  } catch (err) {
    return `${email}: ${err instanceof Error ? err.message : String(err)}`;
  }
}

const createdAgents: string[] = [];
afterAll(async () => {
  const cleanupFailures: string[] = [];
  try {
    for (const a of createdAgents) {
      const failure = await apiDeleteAgent(a);
      if (failure) cleanupFailures.push(failure);
    }
  } finally {
    // Explicit flush: this suite runs inside a vitest worker, whose 'exit'
    // lifecycle (the recorder's best-effort fallback) is not guaranteed to
    // line up with the outer vitest process — see cli-coverage.ts. Calling
    // this here, unconditionally, is what actually gets the shard written for
    // `npm run coverage:gate:cli` to read.
    flushCliCoverage();
  }
  expect(cleanupFailures, cleanupFailures.join("\n")).toEqual([]);
});

describe.skipIf(!live)("cli live parity", () => {
  // Coverage-gate denominator. Parses the command catalog from the REAL
  // built binary's `--help` stdout — not from grepping cli/src/bin/e2a.ts's
  // switch statement or USAGE string as source text — so the gate measures
  // what a user invoking `e2a --help` actually sees they can run. See
  // test/harness/cli-coverage.ts and test/cli-coverage-gate.ts for the full
  // rationale (mirrors the MCP tools/list coverage gate's reasoning).
  it("--help advertises the full command catalog (denominator for the coverage gate)", () => {
    const r = run(["--help"]);
    expect(r.code, r.stderr).toBe(0);
    const commands = parseHelpCommands(r.stdout);
    // Pinned so a CLI surface change (command added/removed/renamed) fails
    // loudly and specifically HERE, not just as a silent shift in the gate's
    // total — a human reviewing this diff should see exactly what changed.
    expect(commands).toEqual([
      "agents",
      "config",
      "contacts",
      "doctor",
      "keys",
      "listen",
      "login",
      "messages",
      "protection",
      "reply",
      "send",
      "suppressions",
      "whoami",
    ]);
    recordAdvertised(commands);
  });

  it("whoami --json → identity (exit 0)", () => {
    const r = run(["whoami", "--json"]);
    expect(r.code, r.stderr).toBe(0);
    const j = JSON.parse(r.stdout);
    expect(j.user?.email).toBeTruthy();
    expect(j.scope).toBe("account");
    recordCovered("whoami");
  });

  it("agents create → get → list, then send → messages list (self loopback)", async () => {
    const bot = `cli-live-${Date.now().toString(36)}@${DOMAIN}`;

    const created = run(["agents", "create", bot, "--name", "cli live e2e", "--json"]);
    expect(created.code, created.stderr).toBe(0);
    createdAgents.push(bot);
    expect(JSON.parse(created.stdout).email).toBe(bot);

    const got = run(["agents", "get", bot, "--json"]);
    expect(got.code, got.stderr).toBe(0);
    expect(JSON.parse(got.stdout).email).toBe(bot);

    const list = run(["agents", "list", "--json"]);
    expect(list.code, list.stderr).toBe(0);
    expect(list.stdout).toContain(bot);
    recordCovered("agents");

    // Send self→self on the fresh (unprotected) inbox: delivers + loops back.
    const subject = `cli-live ${Date.now()}`;
    const sent = run(["send", "--agent", bot, "--to", bot, "--subject", subject, "--body", "hi from cli e2e", "--json"]);
    expect(sent.code, sent.stderr).toBe(0); // 3 would mean HELD; a fresh inbox is unprotected
    const sentId = JSON.parse(sent.stdout).messageId;
    expect(sentId).toBeTruthy();
    recordCovered("send");

    // Poll messages list until the loopback lands (NDJSON, one row per line).
    let rows: string[] = [];
    for (let i = 0; i < 12 && rows.length === 0; i++) {
      const ml = run(["messages", "list", "--agent", bot, "--limit", "20", "--json"]);
      expect(ml.code, ml.stderr).toBe(0);
      rows = ml.stdout.split("\n").filter((l) => l.trim().length > 0);
      if (rows.length === 0) await sleep(1500);
    }
    expect(rows.length, "the loopback message must appear in `messages list`").toBeGreaterThan(0);
    // Correlate to OUR send: fetch the row's message and check the subject matches
    // (a fresh inbox only holds the loopback, but this proves it's genuinely ours).
    const firstId = JSON.parse(rows[0]).id ?? JSON.parse(rows[0]).messageId;
    expect(firstId).toBeTruthy();
    const gotMsg = run(["messages", "get", firstId, "--agent", bot, "--json"]);
    expect(gotMsg.code, gotMsg.stderr).toBe(0);
    expect(JSON.parse(gotMsg.stdout).subject).toBe(subject);

    // `messages lifecycle` real depth check (bonus — the coverage gate only
    // requires the top-level `messages` command, already satisfied by
    // list/get above, but this exercises the third subcommand for real).
    const lifecycle = run(["messages", "lifecycle", sentId, "--agent", bot, "--json"]);
    expect(lifecycle.code, lifecycle.stderr).toBe(0);
    const lifecycleJson = JSON.parse(lifecycle.stdout);
    expect(Array.isArray(lifecycleJson.items)).toBe(true);
    expect(lifecycleJson.items.length).toBeGreaterThan(0);
    recordCovered("messages");
  }, 40_000);

  it("reply threads a reply onto an inbound message (exit 0)", async () => {
    const bot = `cli-live-reply-${Date.now().toString(36)}@${DOMAIN}`;
    const created = run(["agents", "create", bot, "--name", "cli live reply e2e", "--json"]);
    expect(created.code, created.stderr).toBe(0);
    createdAgents.push(bot);

    const subject = `cli-live-reply ${Date.now()}`;
    const sent = run(["send", "--agent", bot, "--to", bot, "--subject", subject, "--body", "hi from cli e2e reply setup", "--json"]);
    expect(sent.code, sent.stderr).toBe(0);

    // Poll for the inbound loopback copy to reply to (need a real received
    // message id — replying to our own outbound send id is not the same
    // in-thread operation `e2a reply` is documented to perform).
    let inboundId = "";
    for (let i = 0; i < 12 && !inboundId; i++) {
      const ml = run(["messages", "list", "--agent", bot, "--limit", "20", "--json"]);
      expect(ml.code, ml.stderr).toBe(0);
      const rows = ml.stdout.split("\n").filter((l) => l.trim().length > 0);
      if (rows.length > 0) inboundId = JSON.parse(rows[0]).id ?? JSON.parse(rows[0]).messageId;
      else await sleep(1500);
    }
    expect(inboundId, "the loopback message must appear before replying to it").toBeTruthy();

    const replied = run(["reply", inboundId, "--agent", bot, "--body", "hi from cli e2e reply", "--json"]);
    expect(replied.code, replied.stderr).toBe(0);
    const replyResult = JSON.parse(replied.stdout);
    expect(replyResult.messageId).toBeTruthy();
    expect(replyResult.status).not.toBe("pending_review");
    recordCovered("reply");
  }, 40_000);

  it("keys create → list → delete (exit 0 each)", () => {
    const created = run(["keys", "create", "--name", "cli-live-key", "--json"]);
    expect(created.code, created.stderr).toBe(0);
    const key = JSON.parse(created.stdout);
    const keyId = key.id ?? key.keyId;
    expect(keyId).toBeTruthy();
    try {
      const list = run(["keys", "list", "--json"]);
      expect(list.code, list.stderr).toBe(0);
      expect(list.stdout).toContain(keyId);

      const del = run(["keys", "delete", keyId]);
      expect(del.code, del.stderr).toBe(0);
      recordCovered("keys");
    } finally {
      // Guarantee the key never lingers on staging even if an assertion threw.
      run(["keys", "delete", keyId]);
    }
  });

  it("protection get → set → get reflects the change (throwaway agent)", () => {
    const bot = `cli-live-protection-${Date.now().toString(36)}@${DOMAIN}`;
    const created = run(["agents", "create", bot, "--name", "cli live protection e2e", "--json"]);
    expect(created.code, created.stderr).toBe(0);
    createdAgents.push(bot);

    const before = run(["protection", "get", bot, "--json"]);
    expect(before.code, before.stderr).toBe(0);
    const beforeCfg = JSON.parse(before.stdout);
    // A fresh agent starts with outbound review off (gate action "flag").
    expect(beforeCfg.outbound.gate.action).toBe("flag");

    const setOn = run(["protection", "set", bot, "--outbound-review", "on", "--json"]);
    expect(setOn.code, setOn.stderr).toBe(0);
    const setOnCfg = JSON.parse(setOn.stdout);
    expect(setOnCfg.outbound.gate.action).toBe("review");

    const after = run(["protection", "get", bot, "--json"]);
    expect(after.code, after.stderr).toBe(0);
    expect(JSON.parse(after.stdout).outbound.gate.action).toBe("review");

    // Restore, so a later probe against this (soon-deleted) agent doesn't
    // trip on a held send.
    const setOff = run(["protection", "set", bot, "--outbound-review", "off", "--json"]);
    expect(setOff.code, setOff.stderr).toBe(0);
    recordCovered("protection");
  });

  it("suppressions: agent-scoped add → list → remove, plus account-wide list", () => {
    const slug = Date.now().toString(36);
    const bot = `cli-live-supp-${slug}@${DOMAIN}`;
    const blocked = `cli-supp-blocked-${slug}@example.invalid`;

    const created = run(["agents", "create", bot, "--name", "cli supp e2e", "--json"]);
    expect(created.code, created.stderr).toBe(0);
    createdAgents.push(bot);

    // Account-wide add is impossible by design — account entries come only from
    // bounces/complaints — so coverage exercises the agent-scoped manual block.
    const add = run(["suppressions", "add", blocked, "--agent", bot, "--reason", "cli e2e", "--json"]);
    expect(add.code, add.stderr).toBe(0);
    expect(JSON.parse(add.stdout).address).toBe(blocked);

    const listAgent = run(["suppressions", "list", "--agent", bot, "--json"]);
    expect(listAgent.code, listAgent.stderr).toBe(0);
    expect(listAgent.stdout).toContain(blocked);

    const removed = run(["suppressions", "remove", blocked, "--agent", bot, "--json"]);
    expect(removed.code, removed.stderr).toBe(0);
    expect(run(["suppressions", "list", "--agent", bot, "--json"]).stdout).not.toContain(blocked);

    // Account-wide list is read-only and typically empty on staging (no bounce
    // simulators there); assert it merely resolves for a bare account key.
    const listAccount = run(["suppressions", "list", "--json"]);
    expect(listAccount.code, listAccount.stderr).toBe(0);

    recordCovered("suppressions");
  });

  it("contacts: create/get/list/update/delete + import/imports delete + outreach tree", () => {
    const slug = Date.now().toString(36);
    const addr = `cli-live-contacts-${slug}@example.com`;
    const importAddr1 = `cli-live-contacts-import-a-${slug}@example.com`;
    const importAddr2 = `cli-live-contacts-import-b-${slug}@example.com`;
    const csvPath = `/tmp/e2a-cli-e2e-contacts-${slug}.csv`;
    writeFileSync(
      csvPath,
      `email,name,company\n${importAddr1},Import One,Acme\n${importAddr2},Import Two,Acme\n`,
    );

    try {
      // Identity CRUD.
      const created = run([
        "contacts", "create", addr,
        "--name", "CLI Live", "--metadata", '{"origin":"cli-e2e"}', "--json",
      ]);
      expect(created.code, created.stderr).toBe(0);
      expect(JSON.parse(created.stdout).address).toBe(addr);

      const got = run(["contacts", "get", addr]);
      expect(got.code, got.stderr).toBe(0);
      const etag = got.stdout.match(/^etag:\s+(\S+)$/m)?.[1];
      expect(etag, `contacts get must print an etag line:\n${got.stdout}`).toBeTruthy();

      const list = run(["contacts", "list", "--json"]);
      expect(list.code, list.stderr).toBe(0);
      expect(list.stdout).toContain(addr);

      const updated = run([
        "contacts", "update", addr, "--name", "CLI Live Updated", "--if-match", etag!, "--json",
      ]);
      expect(updated.code, updated.stderr).toBe(0);
      expect(JSON.parse(updated.stdout).displayName).toBe("CLI Live Updated");

      // CSV import: --dry-run previews without writing, the real run writes,
      // imports delete reverses the batch.
      const dryRun = run(["contacts", "import", csvPath, "--dry-run", "--json"]);
      expect(dryRun.code, dryRun.stderr).toBe(0);
      expect(JSON.parse(dryRun.stdout).rows).toBe(2);
      // Preview must not have created anything.
      expect(run(["contacts", "get", importAddr1]).code).not.toBe(0);

      const imported = run(["contacts", "import", csvPath, "--json"]);
      expect(imported.code, imported.stderr).toBe(0);
      const importResult = JSON.parse(imported.stdout);
      expect(importResult.batchId).toBeTruthy();
      expect(importResult.created).toBe(2);

      const reversed = run(["contacts", "imports", "delete", importResult.batchId, "--json"]);
      expect(reversed.code, reversed.stderr).toBe(0);
      expect(JSON.parse(reversed.stdout).deleted).toBe(true);
      expect(run(["contacts", "get", importAddr1]).code).not.toBe(0);

      // Outreach tree against a fresh (throwaway) agent.
      const bot = `cli-live-outreach-${slug}@${DOMAIN}`;
      const createdAgent = run(["agents", "create", bot, "--name", "cli live contacts e2e", "--json"]);
      expect(createdAgent.code, createdAgent.stderr).toBe(0);
      createdAgents.push(bot);

      const nextAction = new Date(Date.now() + 86_400_000).toISOString();
      const enrolled = run([
        "contacts", "outreach", "set", addr,
        "--agent", bot, "--stage", "prospect", "--next-action", nextAction, "--json",
      ]);
      expect(enrolled.code, enrolled.stderr).toBe(0);
      expect(JSON.parse(enrolled.stdout).stage).toBe("prospect");

      const outreach = run(["contacts", "outreach", "get", addr, "--agent", bot]);
      expect(outreach.code, outreach.stderr).toBe(0);
      // TSV: address \t stage \t nextActionAt \t etag
      const fields = outreach.stdout.trim().split("\t");
      expect(fields[0]).toBe(addr);
      expect(fields[1]).toBe("prospect");
      expect(fields[3], `outreach get must end with an etag field:\n${outreach.stdout}`).toBeTruthy();

      const outreachList = run([
        "contacts", "outreach", "list", "--agent", bot, "--stage", "prospect", "--json",
      ]);
      expect(outreachList.code, outreachList.stderr).toBe(0);
      expect(outreachList.stdout).toContain(addr);

      const unenrolled = run(["contacts", "outreach", "delete", addr, "--agent", bot, "--json"]);
      expect(unenrolled.code, unenrolled.stderr).toBe(0);
      expect(run(["contacts", "outreach", "get", addr, "--agent", bot]).code).not.toBe(0);

      // Deleting the identity removes the contact (suppression survives server-side).
      const deleted = run(["contacts", "delete", addr, "--json"]);
      expect(deleted.code, deleted.stderr).toBe(0);
      expect(JSON.parse(deleted.stdout).address).toBe(addr);
      expect(run(["contacts", "get", addr]).code).not.toBe(0);

      recordCovered("contacts");
    } finally {
      // Tolerate non-zero: the happy path already deleted these.
      for (const a of [addr, importAddr1, importAddr2]) run(["contacts", "delete", a]);
      rmSync(csvPath, { force: true });
    }
  }, 60_000);

  it("doctor: healthy (0), warnings-only (8), and config failure (9) exit codes", () => {
    // The account-scoped conformance credential is deliberately shared with
    // suites that create custom-domain fixtures. Doctor correctly diagnoses
    // their missing DNS, so that mutable account can never be a deterministic
    // "healthy" fixture. Mint a short-lived agent-scoped key instead: domain
    // and webhook checks then skip by contract, while the real API, auth,
    // agent-access, SMTP, report, and exit-code paths still run live.
    const doctorAgent = `cli-live-doctor-${Date.now().toString(36)}@${DOMAIN}`;
    const createdAgent = run(["agents", "create", doctorAgent, "--name", "cli live doctor e2e", "--json"]);
    expect(createdAgent.code, createdAgent.stderr).toBe(0);
    createdAgents.push(doctorAgent);

    let keyId = "";
    let primaryError: unknown;
    try {
      const createdKey = run([
        "keys",
        "create",
        "--agent",
        doctorAgent,
        "--name",
        "cli-live-doctor-isolated",
        "--json",
      ]);
      expect(createdKey.code, createdKey.stderr).toBe(0);
      const key = JSON.parse(createdKey.stdout);
      keyId = key.id ?? key.keyId;
      expect(keyId).toBeTruthy();
      expect(
        typeof key.key === "string" && key.key.startsWith("e2a_agt_"),
        "agent-scoped key response must contain an e2a_agt_ secret",
      ).toBe(true);
      const isolated = {
        E2A_API_KEY: key.key,
        E2A_AGENT_EMAIL: doctorAgent,
      };

      const healthy = run(["doctor", "--json"], isolated);
      const healthyReport = JSON.parse(healthy.stdout);
      expect(healthy.code, healthy.stderr).toBe(healthyReport.exit_code);
      expect(healthy.code, JSON.stringify(healthyReport, null, 2)).toBe(0);
      expect(healthyReport.schema).toBe("e2a.doctor/v1");
      expect(healthyReport.status).toBe("healthy");
      const authCheck = healthyReport.checks.find((c: { id: string }) => c.id === "api.auth");
      expect(authCheck?.evidence?.scope).toBe("agent");
      expect(authCheck?.evidence?.bound_agent).toBe(doctorAgent);
      const agentCheck = healthyReport.checks.find((c: { id: string }) => c.id === "agent.access");
      expect(agentCheck?.status).toBe("pass");
      expect(agentCheck?.evidence?.email).toBe(doctorAgent);
      for (const id of ["domain.registered", "webhook.config"]) {
        const scopedSkip = healthyReport.checks.find((c: { id: string }) => c.id === id);
        expect(scopedSkip?.status, `${id} must skip for an agent-scoped key`).toBe("skip");
        expect(scopedSkip?.reason_code).toBe("requires_account_scope");
      }

      // Warnings-only (8): force the ONE warn-producing, side-effect-free
      // client-side condition doctor has — a partially-configured
      // E2A_OUTBOUND_SMTP_* environment (host without from_domain) — with
      // nothing else in a fail/auth/config state, so the exit-code priority
      // (auth > config > transient > warn) lands on WARN.
      const warn = run(["doctor", "--json"], {
        ...isolated,
        E2A_OUTBOUND_SMTP_HOST: "smtp.example.com",
      });
      const warnReport = JSON.parse(warn.stdout);
      expect(warn.code, warn.stderr).toBe(8);
      expect(warnReport.exit_code).toBe(8);
      expect(warnReport.status).toBe("warnings");
      const smtpCheck = warnReport.checks.find((c: { id: string }) => c.id === "smtp.config");
      expect(smtpCheck?.status).toBe("warn");
      expect(smtpCheck?.reason_code).toBe("smtp_partial");

      // Definite configuration failure (9): a nonexistent agent is a config
      // problem, not a network one — read-only (GET only), no fixture needed.
      const bogusAgent = `cli-live-doctor-missing-${Date.now().toString(36)}@${DOMAIN}`;
      const fail = run(["doctor", "--agent", bogusAgent, "--json"], isolated);
      const failReport = JSON.parse(fail.stdout);
      expect(fail.code, fail.stderr).toBe(9);
      expect(failReport.exit_code).toBe(9);
      expect(failReport.status).toBe("failed");
      const failedCheck = failReport.checks.find((c: { id: string }) => c.id === "agent.access");
      expect(failedCheck?.status).toBe("fail");
      expect(failedCheck?.reason_code).toBe("agent_not_found");

      recordCovered("doctor");
    } catch (err) {
      primaryError = err;
      throw err;
    } finally {
      if (keyId) {
        const revoked = run(["keys", "delete", keyId]);
        if (revoked.code !== 0) {
          if (primaryError) {
            console.warn(`failed to revoke temporary doctor key ${keyId}: ${revoked.stderr}`);
          } else {
            expect(revoked.code, revoked.stderr).toBe(0);
          }
        }
      }
    }
  });

  it("config: list/get/set round-trip against an isolated HOME", () => {
    // A HOME distinct from every other test's shared /tmp/e2a-cli-e2e-home,
    // so a written config.json can't leak into (or be clobbered by) any
    // other test in this file.
    const configHome = `/tmp/e2a-cli-e2e-config-home-${Date.now()}`;
    // E2A_AGENT_EMAIL overrides the file on read (cli/src/config.ts) — unset
    // it (empty string beats the shell-inherited value) so `config get`
    // proves the FILE round-trip, not the environment shadowing it.
    const noAgentEnvOverride = { HOME: configHome, E2A_AGENT_EMAIL: "" };

    const list = run(["config", "list"], noAgentEnvOverride);
    expect(list.code, list.stderr).toBe(0);
    for (const key of ["api_key=", "api_url=", "agent_email=", "shared_domain=", "key_scope="]) {
      expect(list.stdout).toContain(key);
    }

    const value = `cli-live-config-${Date.now().toString(36)}@example.com`;
    const set = run(["config", "set", "agent_email", value], noAgentEnvOverride);
    expect(set.code, set.stderr).toBe(0);
    expect(set.stdout.trim()).toBe(`agent_email=${value}`);

    const get = run(["config", "get", "agent_email"], noAgentEnvOverride);
    expect(get.code, get.stderr).toBe(0);
    expect(get.stdout.trim()).toBe(value);

    recordCovered("config");
  });

  it("listen --once streams a real inbound loopback message, then exits 0", async () => {
    const bot = `cli-live-listen-${Date.now().toString(36)}@${DOMAIN}`;
    const created = run(["agents", "create", bot, "--name", "cli live listen e2e", "--json"]);
    expect(created.code, created.stderr).toBe(0);
    createdAgents.push(bot);

    const until = new Date(Date.now() + 20_000).toISOString();
    const listening = runAsync(["listen", "--once", "--agent", bot, "--until", until, "--json"]);

    // Give the WebSocket a moment to connect before the message is sent —
    // listen must be observing BEFORE the send, or it would only prove
    // --once's own poll-on-connect fallback rather than the live stream.
    await sleep(2000);
    const subject = `cli-live-listen ${Date.now()}`;
    const sent = run(["send", "--agent", bot, "--to", bot, "--subject", subject, "--body", "hi from cli e2e listen", "--json"]);
    expect(sent.code, sent.stderr).toBe(0);

    const result = await listening;
    expect(result.code, result.stderr).toBe(0); // 6 would mean TIMEOUT — no message observed
    const notification = JSON.parse(result.stdout);
    expect(notification.subject).toBe(subject);
    // Documented side effect: --once with --json fetches the message over
    // the API GET, which marks it read.
    expect(notification.readStatus).toBe("read");

    recordCovered("listen");
  }, 30_000);

  it("login fails fast against an unreachable API, before opening a browser (exit 1)", () => {
    // `login`'s interactive browser-OAuth success path cannot be driven
    // headlessly (see cli-coverage-gate.ts's ALLOWLIST entry for the full
    // justification) — this asserts the real preflight failure mode instead:
    // an unreachable E2A_URL must abort BEFORE the local callback server
    // opens or any browser launches, not hang for the 2-minute login timeout.
    const r = run(["login"], { E2A_URL: "https://nonexistent-host-e2a-cli-e2e-test.invalid" });
    expect(r.code, r.stdout).toBe(1);
    expect(r.stderr).toContain("could not reach");
    // Deliberately NOT recordCovered("login") — this proves the failure
    // mode, not a successful login. `login` stays allowlisted.
  });

  it("honors the frozen exit-code contract (usage=2, auth=4)", () => {
    // Unknown command → usage error (2).
    expect(run(["domains", "list"]).code).toBe(2); // CLI has no `domains` command
    // Bad key → auth error (4).
    expect(run(["whoami", "--json"], { E2A_API_KEY: "e2a_bogus_key_definitely_invalid" }).code).toBe(4);
  });
});
