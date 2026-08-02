import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { JobSpool } from "../spool.mjs";

function newSpool(now = () => 1_700_000_000_000) {
  const root = mkdtempSync(path.join(tmpdir(), "autopilot-spool-"));
  return { root, spool: new JobSpool(root, { now }) };
}

test("enqueue durably records metadata without the forwarded message body", () => {
  const { root, spool } = newSpool();
  const result = spool.enqueue({
    messageId: "msg_example_001",
    from: "customer@example.test",
    verifiedDomain: "example.test",
    source: "listener",
    body: "malicious content that must never enter the spool",
  });

  assert.equal(result.created, true);
  assert.equal(result.state, "pending");
  const pending = spool.list("pending");
  assert.equal(pending.length, 1);
  assert.equal(pending[0].messageId, "msg_example_001");
  assert.equal(pending[0].from, "customer@example.test");
  assert.equal(pending[0].verifiedDomain, "example.test");
  assert.equal(pending[0].body, undefined);
  const serialized = readFileSync(pending[0].file, "utf8");
  assert.doesNotMatch(serialized, /malicious content/);
  assert.equal(statSync(pending[0].file).mode & 0o777, 0o600);
  assert.equal(statSync(path.join(root, "pending")).mode & 0o777, 0o700);
});

test("enqueue deduplicates one message across every lifecycle state", () => {
  const { spool } = newSpool();
  assert.equal(spool.enqueue({ messageId: "msg_same", source: "listener" }).created, true);
  const claimed = spool.claimNext();
  assert.equal(claimed.messageId, "msg_same");

  const whileRunning = spool.enqueue({ messageId: "msg_same", source: "reconcile" });
  assert.deepEqual(
    { created: whileRunning.created, state: whileRunning.state },
    { created: false, state: "running" },
  );

  spool.complete("msg_same", { outcome: "pending_review" });
  const afterDone = spool.enqueue({ messageId: "msg_same", source: "listener" });
  assert.deepEqual(
    { created: afterDone.created, state: afterDone.state },
    { created: false, state: "done" },
  );
});

test("claimNext is oldest-first and increments the persisted attempt count", () => {
  let current = 1_700_000_000_000;
  const { spool } = newSpool(() => current);
  spool.enqueue({ messageId: "msg_first", source: "listener" });
  current += 100;
  spool.enqueue({ messageId: "msg_second", source: "listener" });

  const claimed = spool.claimNext();
  assert.equal(claimed.messageId, "msg_first");
  assert.equal(claimed.attempts, 1);
  assert.equal(spool.list("pending")[0].messageId, "msg_second");
  assert.equal(spool.list("running")[0].attempts, 1);
});

test("recoverRunning returns interrupted work to retry without losing it", () => {
  let current = 1_700_000_000_000;
  const { spool } = newSpool(() => current);
  spool.enqueue({ messageId: "msg_interrupted", source: "listener" });
  spool.claimNext();
  current += 500;

  assert.equal(spool.recoverRunning(), 1);
  const retry = spool.list("retry")[0];
  assert.equal(retry.messageId, "msg_interrupted");
  assert.equal(retry.lastError, "supervisor restarted while job was running");
  assert.equal(retry.availableAt, current);

  assert.equal(spool.promoteReadyRetries(), 1);
  assert.equal(spool.list("pending")[0].messageId, "msg_interrupted");
});

test("fail applies bounded exponential retry and then dead-letters", () => {
  let current = 1_700_000_000_000;
  const { spool } = newSpool(() => current);
  spool.enqueue({ messageId: "msg_flaky", source: "listener" });

  spool.claimNext();
  let failed = spool.fail("msg_flaky", "runtime exited 1", {
    maxAttempts: 2,
    baseDelayMs: 1_000,
  });
  assert.equal(failed.state, "retry");
  assert.equal(failed.job.availableAt, current + 1_000);
  assert.equal(spool.promoteReadyRetries(), 0);

  current += 1_000;
  assert.equal(spool.promoteReadyRetries(), 1);
  spool.claimNext();
  failed = spool.fail("msg_flaky", "runtime exited 1", {
    maxAttempts: 2,
    baseDelayMs: 1_000,
  });
  assert.equal(failed.state, "dead");
  assert.equal(spool.list("dead")[0].attempts, 2);
});

test("message IDs cannot escape the spool root", () => {
  const { root, spool } = newSpool();
  spool.enqueue({ messageId: "../../outside", source: "listener" });
  const job = spool.list("pending")[0];

  assert.equal(path.dirname(job.file), path.join(root, "pending"));
  assert.doesNotMatch(path.basename(job.file), /\.\./);
});
