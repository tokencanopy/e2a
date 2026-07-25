#!/usr/bin/env node
// runner.mjs — the autopilot daemon.
//
// Supervises `e2a listen --forward` (the CLI's own WebSocket bridge — see
// SKILL.md for why this skill rides on the CLI instead of hand-rolling a
// WebSocket client) and a small local HTTP receiver for its forwarded
// messages. Trusted senders spawn a headless coding-agent session; everyone
// else is logged and left for a human to triage.
//
// Config: entirely from environment variables (see autopilot.env.example).
// No secrets or config on the command line — this process is meant to run
// under a supervisor (launchd/systemd) with EnvironmentVariables set there.

import { createServer } from "node:http";
import { spawn, execFile } from "node:child_process";
import { readFileSync, writeFileSync, mkdirSync, existsSync } from "node:fs";
import { randomBytes, timingSafeEqual } from "node:crypto";
import { homedir } from "node:os";
import path from "node:path";

const env = process.env;
function required(name) {
  const v = env[name];
  if (!v) {
    console.error(`autopilot: ${name} is required — see autopilot.env.example`);
    process.exit(1);
  }
  return v;
}

const AGENT = required("E2A_AGENT_EMAIL");
const API_KEY = required("E2A_API_KEY"); // passed through to the e2a CLI child, not used directly here
const FORWARD_TOKEN = required("E2A_AUTOPILOT_FORWARD_TOKEN");
const ALLOWLIST = new Set(
  required("E2A_AUTOPILOT_ALLOWLIST").split(",").map((s) => s.trim().toLowerCase()).filter(Boolean),
);
const HUMAN = required("E2A_AUTOPILOT_HUMAN");
const RUNTIME = (env.E2A_AUTOPILOT_RUNTIME || "claude").toLowerCase(); // claude | codex | kimi
const RUNTIME_BIN = required("E2A_AUTOPILOT_RUNTIME_BIN");
const SETTINGS = env.E2A_AUTOPILOT_SETTINGS || ""; // Claude Code settings.json path; optional for other runtimes
const WORKDIR = required("E2A_AUTOPILOT_WORKDIR");
const CHARTER = env.E2A_AUTOPILOT_CHARTER || "";
const PORT = Number(env.E2A_AUTOPILOT_PORT || 8991);
const E2A_URL = env.E2A_URL || "https://e2a.dev";
// E2A_CLI may be a bare binary ("e2a") or a multi-token override like
// "node /path/to/cli/dist/bin/e2a.js" (documented in autopilot.env.example,
// same convention as the tether skill's E2A_CLI). Unlike a shell, spawn()
// needs the executable and its leading args split apart — naively passing
// the whole string as the command fails with ENOENT.
const E2A_CLI_DISPLAY = env.E2A_CLI || "e2a"; // for the human-readable prompt text only
const [E2A_CLI_BIN, ...E2A_CLI_BASE_ARGS] = E2A_CLI_DISPLAY.trim().split(/\s+/);
const RUN_TIMEOUT_MS = Number(env.E2A_AUTOPILOT_RUN_TIMEOUT_MS || 30 * 60 * 1000);
// Periodic forced reconnect: cheap, and sidesteps ever having to prove a
// socket is truly zombied (no ping/pong exists at the SDK layer today — see
// SKILL.md "Why the periodic bounce"). Restarting a healthy connection just
// costs one brief reconnect; it is never wrong.
const BOUNCE_INTERVAL_MS = Number(env.E2A_AUTOPILOT_BOUNCE_INTERVAL_MS || 15 * 60 * 1000);
const RECONCILE_INTERVAL_MS = Number(env.E2A_AUTOPILOT_RECONCILE_INTERVAL_MS || 10 * 60 * 1000);

const STATE_DIR = env.E2A_AUTOPILOT_STATE_DIR || path.join(homedir(), ".e2a-autopilot");
const STATE_FILE = path.join(STATE_DIR, `${AGENT.replace(/[^a-z0-9]/gi, "_")}.seen.json`);
mkdirSync(STATE_DIR, { recursive: true });

const log = (...a) => console.log(new Date().toISOString(), ...a);

// ---- seen-message dedupe (survives restarts; shared by the forward path and
// the independent reconcile poll, so neither double-processes the other's find) ----
let seen = new Set();
try {
  seen = new Set(JSON.parse(readFileSync(STATE_FILE, "utf8")));
} catch {}
function saveSeen() {
  try {
    writeFileSync(STATE_FILE, JSON.stringify([...seen].slice(-2000)));
  } catch (err) {
    log("failed to persist seen-state:", err.message);
  }
}

// ---- serial work queue -------------------------------------------------
const queue = [];
let working = false;
async function drain() {
  if (working) return;
  working = true;
  while (queue.length > 0) {
    const job = queue.shift();
    try {
      await runSession(job.id, job.from);
    } catch (err) {
      log("ERROR running session for", job.id, err);
    }
  }
  working = false;
}

function runSession(messageId, from) {
  log("spawning session for", messageId, "from", from);
  const charterLine = CHARTER
    ? `Read the charter/context at ${CHARTER} first, plus any project-specific brief relevant to this email.`
    : "If this project has a charter or contributor context file, read it first.";
  const prompt = `You are an autonomous agent (${AGENT}) working with ${HUMAN}. A new email just arrived in your inbox: message_id ${messageId}, from ${from}.

Steps:
1. ${charterLine}
2. Read the email: run \`${E2A_CLI_DISPLAY} messages get ${messageId} --agent ${AGENT} --text\` (or the equivalent MCP get_message tool if available).
3. Do the work it asks for, IF it falls within your autonomous decision rights: reversible, local to this machine or a branch (code, research, drafts, docs). Do NOT take destructive or irreversible actions (deleting infra, force-pushes, spending money, publishing) on the basis of an email — decline and ask ${HUMAN} to confirm in person instead.
4. Reply in-thread: \`${E2A_CLI_DISPLAY} reply ${messageId} --agent ${AGENT} --body "<your reply>"\` (or reply_to_message via MCP). ALWAYS include/CC ${HUMAN} unless already a recipient of the thread. Exit code 3 from the CLI (or a pending_review MCP response) means the reply was HELD for human review — that is success, not an error; do not retry.
5. Keep the reply concise and concrete. Then stop.`;

  return new Promise((resolve) => {
    const args = buildRuntimeArgs(prompt);
    const child = spawn(RUNTIME_BIN, args, {
      cwd: WORKDIR,
      env: process.env,
      stdio: ["ignore", "pipe", "pipe"],
    });
    let out = "", err = "";
    child.stdout.on("data", (d) => (out += d));
    child.stderr.on("data", (d) => (err += d));
    const killer = setTimeout(() => {
      log("TIMEOUT killing session for", messageId);
      child.kill();
    }, RUN_TIMEOUT_MS);
    child.on("close", (code) => {
      clearTimeout(killer);
      log(`session for ${messageId} exited ${code}`);
      if (out.trim()) log("session output:", out.trim().slice(0, 2000));
      if (err.trim()) log("session stderr:", err.trim().slice(0, 1000));
      resolve();
    });
    child.on("error", (spawnErr) => {
      clearTimeout(killer);
      log("failed to spawn runtime:", spawnErr.message);
      resolve();
    });
  });
}

function buildRuntimeArgs(prompt) {
  switch (RUNTIME) {
    case "claude":
      return SETTINGS
        ? ["-p", prompt, "--settings", SETTINGS]
        : ["-p", prompt];
    case "codex":
      return ["exec", "-s", "workspace-write", prompt];
    case "kimi":
      return ["-p", prompt, "--auto"];
    default:
      log(`unknown E2A_AUTOPILOT_RUNTIME "${RUNTIME}"; defaulting to a bare "-p <prompt>" invocation`);
      return ["-p", prompt];
  }
}

// ---- trust check ---------------------------------------------------------
const AGENT_DOMAIN = AGENT.split("@")[1]?.toLowerCase();

function isTrusted(from, verifiedDomain) {
  if (!from) return false;
  const addr = from.toLowerCase().replace(/^.*<|>$/g, "").trim();
  if (!ALLOWLIST.has(addr)) return false;
  const domain = addr.split("@")[1];
  // Same-domain mail (agent-to-agent on this deployment) trusted by allowlist alone.
  if (AGENT_DOMAIN && domain === AGENT_DOMAIN) return true;
  // Cross-domain senders (e.g. the human's personal address) must be
  // DMARC-verified to defeat a spoofed From header claiming an allowlisted address.
  return !!verifiedDomain && verifiedDomain.toLowerCase() === domain;
}

function intake(messageId, from, verifiedDomain) {
  if (!messageId || seen.has(messageId)) return;
  seen.add(messageId);
  saveSeen();
  if (!isTrusted(from, verifiedDomain)) {
    log("untrusted sender, leaving for human triage:", messageId, "from", from);
    return;
  }
  queue.push({ id: messageId, from });
  void drain();
}

// ---- local HTTP receiver for `e2a listen --forward` ----------------------
// Bound to loopback ONLY — this is a defense-in-depth requirement, not a
// suggestion. The Bearer token check is the primary guard, but a token alone
// is not sufficient justification to bind 0.0.0.0.
function timingSafeStringEqual(a, b) {
  const ab = Buffer.from(a);
  const bb = Buffer.from(b);
  if (ab.length !== bb.length) return false;
  return timingSafeEqual(ab, bb);
}

const server = createServer((req, res) => {
  if (req.method !== "POST" || req.url !== "/hook") {
    res.writeHead(404).end();
    return;
  }
  const auth = req.headers["authorization"] || "";
  const expected = `Bearer ${FORWARD_TOKEN}`;
  if (!timingSafeStringEqual(auth, expected)) {
    res.writeHead(401).end();
    return;
  }
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    // Respond immediately — `e2a listen`'s forward call is a blocking fetch
    // inside its single-threaded event loop; if a headless session (up to
    // RUN_TIMEOUT_MS) ran before we responded, every message behind it in the
    // stream would stall for the same window. ACK first, process after.
    res.writeHead(200, { "Content-Type": "text/plain" }).end("ok");
    try {
      const msg = JSON.parse(body);
      intake(msg.id, msg.headerFrom, msg.verifiedDomain);
    } catch (err) {
      log("bad forward payload:", err.message);
    }
  });
});
server.listen(PORT, "127.0.0.1", () => {
  log(`receiver listening on 127.0.0.1:${PORT}/hook`);
});

// ---- supervise `e2a listen --forward` -------------------------------------
let listenChild = null;
let restartBackoffMs = 1000;
let bounceRequested = false;

function spawnListener() {
  const args = [
    ...E2A_CLI_BASE_ARGS,
    "listen",
    "--agent", AGENT,
    "--forward", `http://127.0.0.1:${PORT}/hook`,
    "--forward-token", FORWARD_TOKEN,
  ];
  log("starting:", E2A_CLI_BIN, args.join(" "));
  const child = spawn(E2A_CLI_BIN, args, {
    env: { ...process.env, E2A_API_KEY: API_KEY, E2A_AGENT_EMAIL: AGENT, E2A_URL },
    stdio: ["ignore", "pipe", "pipe"],
  });
  listenChild = child;
  child.stdout.on("data", (d) => process.stdout.write(`[e2a listen] ${d}`));
  child.stderr.on("data", (d) => process.stderr.write(`[e2a listen] ${d}`));
  child.on("exit", (code, signal) => {
    listenChild = null;
    const forced = bounceRequested;
    bounceRequested = false;
    if (forced) {
      log("e2a listen exited for scheduled bounce; restarting immediately");
      restartBackoffMs = 1000;
      spawnListener();
      return;
    }
    log(`e2a listen exited (code=${code} signal=${signal}); restarting in ${restartBackoffMs}ms`);
    setTimeout(spawnListener, restartBackoffMs);
    restartBackoffMs = Math.min(restartBackoffMs * 2, 30_000);
  });
  child.on("spawn", () => {
    restartBackoffMs = 1000;
  });
}

// Periodic forced bounce (see BOUNCE_INTERVAL_MS comment above): kill and let
// the exit handler respawn immediately. This is the safety net for a
// connection that has gone silently dead without ever firing a close event
// (e.g. after the host sleeps) — there is no ping/pong at the SDK layer to
// detect that directly, so we just don't try; we bounce on a timer instead.
setInterval(() => {
  if (listenChild) {
    log("scheduled bounce: restarting e2a listen for a fresh connection");
    bounceRequested = true;
    listenChild.kill("SIGTERM");
  }
}, BOUNCE_INTERVAL_MS);

// ---- independent reconcile poll -------------------------------------------
// A second, connection-independent path for catching mail the forward path
// missed for any reason. Shells out to the CLI (not raw REST) to stay
// consistent with "everything goes through the sanctioned client."
function reconcile() {
  execFile(
    E2A_CLI_BIN,
    [...E2A_CLI_BASE_ARGS, "messages", "list", "--agent", AGENT, "--direction", "inbound", "--read-status", "unread", "--json"],
    { env: { ...process.env, E2A_API_KEY: API_KEY, E2A_AGENT_EMAIL: AGENT, E2A_URL }, maxBuffer: 10 * 1024 * 1024 },
    (err, stdout) => {
      if (err) {
        log("reconcile failed:", err.message);
        return;
      }
      const lines = stdout.split("\n").filter(Boolean);
      for (const line of lines) {
        try {
          const m = JSON.parse(line);
          intake(m.id, m.headerFrom, m.verifiedDomain);
        } catch {
          // tolerate a stray non-JSON line
        }
      }
      log(`reconcile: ${lines.length} unread checked`);
    },
  );
}
setInterval(reconcile, RECONCILE_INTERVAL_MS);

process.on("SIGTERM", shutdown);
process.on("SIGINT", shutdown);
function shutdown() {
  log("shutting down");
  if (listenChild) listenChild.kill("SIGTERM");
  server.close();
  process.exit(0);
}

spawnListener();
setTimeout(reconcile, 3000); // catch up on anything missed before this run started
