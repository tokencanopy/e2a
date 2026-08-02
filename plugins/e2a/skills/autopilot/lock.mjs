import {
  chmodSync,
  closeSync,
  existsSync,
  mkdirSync,
  openSync,
  readFileSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";

function processIsAlive(pid, kill = process.kill) {
  try {
    kill(pid, 0);
    return true;
  } catch (error) {
    if (error?.code === "ESRCH") return false;
    if (error?.code === "EPERM") return true;
    throw error;
  }
}

function readOwner(file) {
  try {
    return JSON.parse(readFileSync(file, "utf8"));
  } catch {
    return null;
  }
}

export function supervisorLockPath(stateRoot) {
  return path.join(stateRoot, "locks", "supervisor.lock");
}

export function inspectSupervisorLock(
  stateRoot,
  { kill = process.kill } = {},
) {
  const file = supervisorLockPath(stateRoot);
  if (!existsSync(file)) return { active: false, file };
  const owner = readOwner(file);
  if (!Number.isSafeInteger(owner?.pid) || owner.pid <= 0) {
    return { active: true, indeterminate: true, file };
  }
  return {
    active: processIsAlive(owner.pid, kill),
    pid: owner.pid,
    token: owner.token,
    file,
  };
}

export function acquireSupervisorLock(
  stateRoot,
  { pid = process.pid, kill = process.kill } = {},
) {
  const directory = path.join(stateRoot, "locks");
  mkdirSync(directory, { recursive: true, mode: 0o700 });
  chmodSync(directory, 0o700);
  const file = supervisorLockPath(stateRoot);
  const token = randomUUID();

  for (let attempt = 0; attempt < 2; attempt += 1) {
    let descriptor;
    try {
      descriptor = openSync(file, "wx", 0o600);
      writeFileSync(
        descriptor,
        `${JSON.stringify({ version: 1, pid, token, acquiredAt: new Date().toISOString() })}\n`,
        "utf8",
      );
      chmodSync(file, 0o600);
      let released = false;
      return {
        file,
        release() {
          if (released) return;
          released = true;
          closeSync(descriptor);
          descriptor = undefined;
          const owner = readOwner(file);
          if (owner?.token === token) unlinkSync(file);
        },
      };
    } catch (error) {
      if (descriptor !== undefined) closeSync(descriptor);
      if (error?.code !== "EEXIST") throw error;
      const current = inspectSupervisorLock(stateRoot, { kill });
      if (current.active) {
        throw new Error(
          `Autopilot supervisor is already running${current.pid ? ` as PID ${current.pid}` : ""}.`,
        );
      }
      try {
        unlinkSync(file);
      } catch (unlinkError) {
        // A concurrent acquirer may have reclaimed the stale lock between our
        // inspect and unlink; tolerate the miss and re-loop so the loser of
        // that race gets a clean "already running" from the next attempt.
        if (unlinkError?.code !== "ENOENT") throw unlinkError;
      }
    }
  }
  throw new Error("Could not acquire the Autopilot supervisor lock.");
}
