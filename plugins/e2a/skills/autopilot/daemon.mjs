import { execFile, spawn } from "node:child_process";
import { chmodSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";

import { createForwardReceiver, isAuthorizedMessage } from "./supervisor.mjs";

const SAFE_CHILD_ENV = [
  "PATH",
  "HOME",
  "LANG",
  "LC_ALL",
  "LC_CTYPE",
  "TMPDIR",
  "SSL_CERT_FILE",
  "SSL_CERT_DIR",
];

export function buildListenerArgs({ baseArgs = [], agentEmail, port, forwardToken }) {
  return [
    ...baseArgs,
    "listen",
    "--agent",
    agentEmail,
    "--forward",
    `http://127.0.0.1:${port}/hook`,
    "--forward-token",
    forwardToken,
  ];
}

export function buildReconcileArgs({ baseArgs = [], agentEmail, since }) {
  if (!since || Number.isNaN(Date.parse(since))) {
    throw new Error("A valid durable reconciliation cursor is required.");
  }
  return [
    ...baseArgs,
    "messages",
    "list",
    "--agent",
    agentEmail,
    "--direction",
    "inbound",
    "--read-status",
    "all",
    "--since",
    since,
    "--json",
  ];
}

export function readReconcileCursor(file) {
  let value;
  try {
    value = JSON.parse(readFileSync(file, "utf8"));
  } catch (error) {
    throw new Error(`Cannot read the durable reconciliation cursor: ${error.message}`);
  }
  if (value?.version !== 1 || typeof value.since !== "string" || Number.isNaN(Date.parse(value.since))) {
    throw new Error("The durable reconciliation cursor is invalid.");
  }
  return value;
}

export function writeReconcileCursor(file, since) {
  const temporary = path.join(
    path.dirname(file),
    `.${path.basename(file)}.${randomUUID()}.tmp`,
  );
  writeFileSync(temporary, `${JSON.stringify({ version: 1, since })}\n`, {
    encoding: "utf8",
    flag: "wx",
    mode: 0o600,
  });
  renameSync(temporary, file);
  chmodSync(file, 0o600);
}

export function formatListenerStart(command, args) {
  const safe = [...args];
  const tokenIndex = safe.indexOf("--forward-token");
  if (tokenIndex >= 0 && tokenIndex + 1 < safe.length) safe[tokenIndex + 1] = "[redacted]";
  return `starting listener: ${command} ${safe.join(" ")}`;
}

export function buildCliEnvironment(environment, { apiKey, agentEmail, deploymentUrl }) {
  const result = {};
  for (const name of SAFE_CHILD_ENV) {
    if (typeof environment?.[name] === "string" && environment[name]) {
      result[name] = environment[name];
    }
  }
  result.E2A_API_KEY = apiKey;
  result.E2A_AGENT_EMAIL = agentEmail;
  result.E2A_URL = deploymentUrl;
  return result;
}

export function parseReconcileOutput(output) {
  const messages = [];
  const lines = String(output)
    .split("\n")
    .map((line) => line.trim())
    .filter(Boolean);
  for (const [index, line] of lines.entries()) {
    let value;
    try {
      value = JSON.parse(line);
    } catch {
      throw new Error(`Reconcile returned invalid NDJSON on line ${index + 1}.`);
    }
    if (!value || typeof value !== "object" || typeof value.id !== "string") {
      throw new Error(`Reconcile returned an invalid message summary on line ${index + 1}.`);
    }
    messages.push(value);
  }
  return messages;
}

export function reconcileSummaries({ policy, spool, messages }) {
  const result = { checked: messages.length, enqueued: 0, refused: 0, deduplicated: 0 };
  for (const message of messages) {
    if (!isAuthorizedMessage(policy, message)) {
      result.refused += 1;
      continue;
    }
    const queued = spool.enqueue({
      messageId: message.id,
      from: message.headerFrom ?? message.header_from,
      verifiedDomain: message.verifiedDomain ?? message.verified_domain,
      source: "reconcile",
    });
    if (queued.created) result.enqueued += 1;
    else result.deduplicated += 1;
  }
  return result;
}

export class AutopilotDaemon {
  constructor({
    policy,
    secrets,
    spool,
    supervisor,
    cli,
    environment = process.env,
    spawnImpl = spawn,
    execFileImpl = execFile,
    log = () => {},
    receiverPort = 0,
    bounceIntervalMs = 15 * 60 * 1_000,
    reconcileIntervalMs = 10 * 60 * 1_000,
    reconcileStatePath,
    now = Date.now,
  }) {
    this.policy = policy;
    this.secrets = secrets;
    this.spool = spool;
    this.supervisor = supervisor;
    this.cli = cli;
    this.environment = environment;
    this.spawnImpl = spawnImpl;
    this.execFileImpl = execFileImpl;
    this.log = log;
    this.receiverPort = receiverPort;
    this.bounceIntervalMs = bounceIntervalMs;
    this.reconcileIntervalMs = reconcileIntervalMs;
    this.reconcileStatePath = reconcileStatePath;
    this.now = now;
    this.receiver = null;
    this.listener = null;
    this.bounceTimer = null;
    this.reconcileTimer = null;
    this.restartTimer = null;
    this.retryTimer = null;
    this.retryAt = null;
    this.restartBackoffMs = 1_000;
    this.listenerStableTimer = null;
    this.bounceRequested = false;
    this.stopping = false;
    this.draining = false;
  }

  cliEnvironment() {
    return buildCliEnvironment(this.environment, {
      apiKey: this.secrets.apiKey,
      agentEmail: this.policy.mailbox.agentEmail,
      deploymentUrl: this.secrets.deploymentUrl,
    });
  }

  async start() {
    this.spool.recoverRunning();
    this.spool.promoteReadyRetries();
    this.schedulePersistedRetry();
    this.receiver = await createForwardReceiver({
      port: this.receiverPort,
      token: this.secrets.forwardToken,
      policy: this.policy,
      spool: this.spool,
      onJob: () => this.requestDrain(),
    });
    this.log(`receiver listening on 127.0.0.1:${this.receiver.port}/hook`);
    this.spawnListener();
    this.bounceTimer = setInterval(() => this.bounceListener(), this.bounceIntervalMs);
    this.reconcileTimer = setInterval(() => void this.reconcile(), this.reconcileIntervalMs);
    void this.reconcile();
    this.requestDrain();
  }

  spawnListener() {
    if (this.stopping) return;
    const args = buildListenerArgs({
      baseArgs: this.cli.baseArgs,
      agentEmail: this.policy.mailbox.agentEmail,
      port: this.receiver.port,
      forwardToken: this.secrets.forwardToken,
    });
    this.log(formatListenerStart(this.cli.command, args));
    const child = this.spawnImpl(this.cli.command, args, {
      env: this.cliEnvironment(),
      stdio: ["ignore", "ignore", "ignore"],
      windowsHide: true,
    });
    this.listener = child;
    child.on("spawn", () => {
      this.listenerStableTimer = setTimeout(() => {
        this.listenerStableTimer = null;
        this.restartBackoffMs = 1_000;
      }, 60_000);
    });
    child.on("error", () => {});
    child.on("exit", (code, signal) => {
      if (this.listenerStableTimer) {
        clearTimeout(this.listenerStableTimer);
        this.listenerStableTimer = null;
      }
      if (this.listener === child) this.listener = null;
      if (this.stopping) return;
      if (this.bounceRequested) {
        this.bounceRequested = false;
        this.log("listener restarted for scheduled connection refresh");
        this.spawnListener();
        return;
      }
      if (code === 5) {
        this.log("listener was replaced by another e2a listener; live forwarding is disabled and reconciliation remains active");
        return;
      }
      const delay = this.restartBackoffMs;
      this.restartBackoffMs = Math.min(this.restartBackoffMs * 2, 30_000);
      this.log(`listener exited (code=${code} signal=${signal}); restart in ${delay}ms`);
      this.restartTimer = setTimeout(() => this.spawnListener(), delay);
    });
  }

  bounceListener() {
    if (!this.listener || this.stopping) return;
    this.bounceRequested = true;
    this.listener.kill("SIGTERM");
  }

  async reconcile() {
    if (this.stopping) return null;
    if (!this.reconcileStatePath) {
      this.log("reconcile failed: missing_cursor_path");
      return null;
    }
    try {
      const cursor = readReconcileCursor(this.reconcileStatePath);
      const reconcileStartedAt = new Date(this.now()).toISOString();
      const args = buildReconcileArgs({
        baseArgs: this.cli.baseArgs,
        agentEmail: this.policy.mailbox.agentEmail,
        since: cursor.since,
      });
      const stdout = await new Promise((resolve, reject) => {
        this.execFileImpl(
          this.cli.command,
          args,
          { env: this.cliEnvironment(), maxBuffer: 10 * 1024 * 1024 },
          (error, output) => (error ? reject(error) : resolve(output)),
        );
      });
      const result = reconcileSummaries({
        policy: this.policy,
        spool: this.spool,
        messages: parseReconcileOutput(stdout),
      });
      writeReconcileCursor(this.reconcileStatePath, reconcileStartedAt);
      this.log(
        `reconcile checked=${result.checked} enqueued=${result.enqueued} refused=${result.refused} deduplicated=${result.deduplicated}`,
      );
      if (result.enqueued > 0) this.requestDrain();
      return result;
    } catch (error) {
      this.log(`reconcile failed: ${error?.code || "error"}`);
      return null;
    }
  }

  requestDrain() {
    if (this.draining || this.stopping) return;
    this.draining = true;
    void (async () => {
      try {
        while (!this.stopping) {
          const result = await this.supervisor.runNextJob();
          if (result.state === "idle") break;
          if (result.state === "retry" && Number.isFinite(result.job?.availableAt)) {
            this.scheduleRetry(result.job.availableAt);
          }
          this.log(`job ${result.job?.messageId || "unknown"} state=${result.state}`);
        }
      } finally {
        this.draining = false;
      }
    })();
  }

  scheduleRetry(availableAt) {
    if (this.stopping) return;
    if (this.retryTimer && this.retryAt <= availableAt) return;
    if (this.retryTimer) clearTimeout(this.retryTimer);
    this.retryAt = availableAt;
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null;
      this.retryAt = null;
      this.requestDrain();
    }, Math.max(0, availableAt - Date.now()));
  }

  schedulePersistedRetry() {
    const availableAt = this.spool.nextRetryAt?.();
    if (Number.isFinite(availableAt)) this.scheduleRetry(availableAt);
  }

  async stop() {
    this.stopping = true;
    for (const timer of [
      this.bounceTimer,
      this.reconcileTimer,
      this.restartTimer,
      this.retryTimer,
      this.listenerStableTimer,
    ]) {
      if (timer) clearTimeout(timer);
    }
    if (this.listener) {
      this.listener.kill("SIGTERM");
      this.listener = null;
    }
    if (this.receiver) {
      await this.receiver.close();
      this.receiver = null;
    }
  }
}
