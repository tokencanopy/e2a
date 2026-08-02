import { createHash, randomUUID } from "node:crypto";
import {
  chmodSync,
  closeSync,
  existsSync,
  fsyncSync,
  linkSync,
  mkdirSync,
  openSync,
  readFileSync,
  readdirSync,
  renameSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";

const STATES = ["pending", "running", "retry", "done", "dead"];

function safeText(value, limit) {
  return typeof value === "string" ? value.slice(0, limit) : "";
}

function keyFor(messageId) {
  return `${createHash("sha256").update(messageId, "utf8").digest("hex")}.json`;
}

function serialize(value) {
  return `${JSON.stringify(value, null, 2)}\n`;
}

export class JobSpool {
  constructor(root, { now = Date.now } = {}) {
    if (!root || !path.isAbsolute(root)) {
      throw new Error("Job spool root must be an absolute path.");
    }
    this.root = root;
    this.now = now;
    this.init();
  }

  init() {
    mkdirSync(this.root, { recursive: true, mode: 0o700 });
    chmodSync(this.root, 0o700);
    for (const state of STATES) {
      const directory = this.stateDir(state);
      mkdirSync(directory, { recursive: true, mode: 0o700 });
      chmodSync(directory, 0o700);
    }
  }

  stateDir(state) {
    if (!STATES.includes(state)) throw new Error(`Unknown job state: ${state}.`);
    return path.join(this.root, state);
  }

  fileFor(state, messageId) {
    return path.join(this.stateDir(state), keyFor(messageId));
  }

  locate(messageId) {
    const key = keyFor(messageId);
    for (const state of STATES) {
      const file = path.join(this.stateDir(state), key);
      if (existsSync(file)) return { state, file };
    }
    return null;
  }

  read(file) {
    const job = JSON.parse(readFileSync(file, "utf8"));
    return { ...job, file };
  }

  writeAtomic(file, job) {
    const temporary = path.join(
      path.dirname(file),
      `.${path.basename(file)}.${randomUUID()}.tmp`,
    );
    let descriptor;
    try {
      descriptor = openSync(temporary, "wx", 0o600);
      writeFileSync(descriptor, serialize(job), "utf8");
      fsyncSync(descriptor);
      closeSync(descriptor);
      descriptor = undefined;
      renameSync(temporary, file);
      chmodSync(file, 0o600);
    } finally {
      if (descriptor !== undefined) closeSync(descriptor);
      if (existsSync(temporary)) unlinkSync(temporary);
    }
  }

  createExclusive(file, job) {
    const temporary = path.join(
      path.dirname(file),
      `.${path.basename(file)}.${randomUUID()}.tmp`,
    );
    let descriptor;
    try {
      descriptor = openSync(temporary, "wx", 0o600);
      writeFileSync(descriptor, serialize(job), "utf8");
      fsyncSync(descriptor);
      closeSync(descriptor);
      descriptor = undefined;
      linkSync(temporary, file);
      chmodSync(file, 0o600);
    } finally {
      if (descriptor !== undefined) closeSync(descriptor);
      if (existsSync(temporary)) unlinkSync(temporary);
    }
  }

  enqueue(input) {
    const messageId = safeText(input?.messageId, 512).trim();
    if (!messageId) throw new Error("A message ID is required to enqueue a job.");
    const existing = this.locate(messageId);
    if (existing) {
      return { created: false, state: existing.state, job: this.read(existing.file) };
    }

    const createdAt = this.now();
    const job = {
      version: 1,
      messageId,
      from: safeText(input?.from, 1024),
      verifiedDomain: safeText(input?.verifiedDomain, 253).toLowerCase(),
      source: safeText(input?.source, 64) || "unknown",
      state: "pending",
      attempts: 0,
      createdAt,
      updatedAt: createdAt,
    };
    const file = this.fileFor("pending", messageId);
    try {
      this.createExclusive(file, job);
    } catch (error) {
      if (error?.code !== "EEXIST") throw error;
      const raced = this.locate(messageId);
      if (!raced) throw error;
      return { created: false, state: raced.state, job: this.read(raced.file) };
    }
    return { created: true, state: "pending", job: this.read(file) };
  }

  list(state) {
    const directory = this.stateDir(state);
    return readdirSync(directory)
      .filter((name) => name.endsWith(".json"))
      .map((name) => this.read(path.join(directory, name)))
      .sort(
        (left, right) =>
          left.createdAt - right.createdAt ||
          left.messageId.localeCompare(right.messageId),
      );
  }

  transition(messageId, fromState, toState, patch = {}) {
    const source = this.fileFor(fromState, messageId);
    if (!existsSync(source)) {
      throw new Error(`Job ${messageId} is not in ${fromState}.`);
    }
    const previous = this.read(source);
    if (previous.messageId !== messageId) {
      throw new Error(`Job ID mismatch in ${source}.`);
    }
    const destination = this.fileFor(toState, messageId);
    if (existsSync(destination)) {
      throw new Error(`Job ${messageId} already exists in ${toState}.`);
    }
    renameSync(source, destination);
    const job = {
      ...previous,
      ...patch,
      state: toState,
      updatedAt: this.now(),
    };
    delete job.file;
    this.writeAtomic(destination, job);
    return this.read(destination);
  }

  claimNext() {
    const next = this.list("pending")[0];
    if (!next) return null;
    return this.transition(next.messageId, "pending", "running", {
      attempts: next.attempts + 1,
      startedAt: this.now(),
    });
  }

  complete(messageId, outcome = {}) {
    return this.transition(messageId, "running", "done", {
      outcome,
      completedAt: this.now(),
    });
  }

  checkpointEffects(messageId, patch = {}) {
    const located = this.locate(messageId);
    if (!located || located.state !== "running") {
      throw new Error(`Job ${messageId} is not running.`);
    }
    const previous = this.read(located.file);
    const effects = {
      ...(previous.effects || {}),
      ...patch,
    };
    const job = {
      ...previous,
      effects,
      updatedAt: this.now(),
    };
    delete job.file;
    this.writeAtomic(located.file, job);
    return this.read(located.file);
  }

  fail(messageId, error, { maxAttempts = 3, baseDelayMs = 1_000 } = {}) {
    const located = this.locate(messageId);
    if (!located || located.state !== "running") {
      throw new Error(`Job ${messageId} is not running.`);
    }
    const job = this.read(located.file);
    const lastError = safeText(error, 500) || "unknown runtime failure";
    if (job.attempts >= maxAttempts) {
      return {
        state: "dead",
        job: this.transition(messageId, "running", "dead", {
          lastError,
          failedAt: this.now(),
        }),
      };
    }
    const delay = baseDelayMs * 2 ** Math.max(0, job.attempts - 1);
    return {
      state: "retry",
      job: this.transition(messageId, "running", "retry", {
        lastError,
        availableAt: this.now() + delay,
      }),
    };
  }

  recoverRunning() {
    const interrupted = this.list("running");
    for (const job of interrupted) {
      this.transition(job.messageId, "running", "retry", {
        lastError: "supervisor restarted while job was running",
        availableAt: this.now(),
      });
    }
    return interrupted.length;
  }

  promoteReadyRetries() {
    const current = this.now();
    const ready = this.list("retry").filter(
      (job) => !Number.isFinite(job.availableAt) || job.availableAt <= current,
    );
    for (const job of ready) {
      this.transition(job.messageId, "retry", "pending", {
        availableAt: null,
      });
    }
    return ready.length;
  }

  nextRetryAt() {
    const values = this.list("retry")
      .map((job) => job.availableAt)
      .filter(Number.isFinite);
    return values.length > 0 ? Math.min(...values) : null;
  }

  counts() {
    return Object.fromEntries(STATES.map((state) => [state, this.list(state).length]));
  }
}
