import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import {
  acquireSupervisorLock,
  inspectSupervisorLock,
  supervisorLockPath,
} from "../lock.mjs";

test("supervisor lifetime lock rejects a concurrent daemon and releases cleanly", () => {
  const stateRoot = mkdtempSync(path.join(tmpdir(), "autopilot-lock-"));
  const alive = (pid) => {
    if (pid === 4242) return;
    const error = new Error("missing");
    error.code = "ESRCH";
    throw error;
  };
  const first = acquireSupervisorLock(stateRoot, { pid: 4242, kill: alive });

  assert.throws(
    () => acquireSupervisorLock(stateRoot, { pid: 4343, kill: alive }),
    /already running.*4242/i,
  );
  assert.equal(inspectSupervisorLock(stateRoot, { kill: alive }).active, true);

  first.release();
  assert.equal(inspectSupervisorLock(stateRoot, { kill: alive }).active, false);
});

test("supervisor lifetime lock recovers a stale owner", () => {
  const stateRoot = mkdtempSync(path.join(tmpdir(), "autopilot-lock-"));
  const locks = path.join(stateRoot, "locks");
  const stale = acquireSupervisorLock(stateRoot, {
    pid: 1111,
    kill() {
      const error = new Error("missing");
      error.code = "ESRCH";
      throw error;
    },
  });
  // Simulate a crash: leave the lock path behind without calling release.
  writeFileSync(supervisorLockPath(stateRoot), JSON.stringify({ pid: 1111, token: "stale" }));

  const replacement = acquireSupervisorLock(stateRoot, {
    pid: 2222,
    kill() {
      const error = new Error("missing");
      error.code = "ESRCH";
      throw error;
    },
  });
  assert.equal(path.dirname(replacement.file), locks);
  replacement.release();
  stale.release();
});

test("stale-lock reclaim tolerates a concurrent winner and reports a clean conflict", () => {
  const stateRoot = mkdtempSync(path.join(tmpdir(), "autopilot-lock-"));
  mkdirSync(path.join(stateRoot, "locks"), { recursive: true });
  writeFileSync(supervisorLockPath(stateRoot), JSON.stringify({ pid: 1111, token: "stale" }), { mode: 0o600 });

  // Simulate the race: the lock file disappears between inspect and unlink
  // (a concurrent acquirer already reclaimed it), so the reclaim must
  // tolerate ENOENT and re-loop instead of crashing.
  const file = supervisorLockPath(stateRoot);
  let vanished = false;
  const winner = acquireSupervisorLock(stateRoot, {
    pid: 2222,
    kill() {
      if (!vanished) {
        vanished = true;
        rmSync(file, { force: true });
      }
      const error = new Error("missing");
      error.code = "ESRCH";
      throw error;
    },
  });
  assert.equal(inspectSupervisorLock(stateRoot, { kill: () => {} }).pid, 2222);

  // The concurrent loser gets the clean "already running" error.
  assert.throws(
    () => acquireSupervisorLock(stateRoot, { pid: 3333, kill: () => {} }),
    /already running.*2222/i,
  );
  winner.release();
});
