import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
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
