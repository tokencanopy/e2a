import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const root = path.resolve(import.meta.dirname, "..");
const cli = path.join(root, "autopilot.mjs");

test("interview writes a private policy and prints a non-mutating plan", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "autopilot-interview-"));
  const state = path.join(dir, "state", "interview.json");
  const policy = path.join(dir, "state", "policy.json");
  const input = [
    "Resolve routine customer questions.",
    "", // customer-support
    "Setup, usage, and troubleshooting.",
    "Refunds and legal requests.",
    "Approved handbook.",
    "Billing, security, or uncertainty.",
    "Reply within one business day.",
    "", // submit replies for review
    "", // default tone
    "Example Support",
    "support@example.com",
    "owner@example.com",
    "addresses",
    "vip@customer.test, buyer@customer.test",
    "", // review on
    "", // owner CC on
    "", // screening on
    "codex",
    "/usr/local/bin/codex",
    "/srv/autopilot/support",
    "custom",
    "I understand",
    "foreground",
  ].join("\n") + "\n";

  const result = spawnSync(
    process.execPath,
    [cli, "interview", "--state", state, "--policy", policy],
    { input, encoding: "utf8" },
  );

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /e2a Autopilot installation plan/);
  assert.match(result.stdout, /No changes have been applied/);
  assert.match(result.stdout, /Run `autopilot plan` .* installable confirmation digest/);
  const saved = JSON.parse(readFileSync(policy, "utf8"));
  assert.equal(saved.outbound.requireReview, true);
  assert.equal(saved.outbound.ccOwner, true);
  assert.equal(saved.inbound.fallback, "review");
  assert.equal(statSync(policy).mode & 0o777, 0o600);
  assert.equal(statSync(path.dirname(policy)).mode & 0o777, 0o700);
});

test("plan rejects an invalid policy without mutating it", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "autopilot-plan-"));
  const policy = path.join(dir, "policy.json");
  const original = '{"version":1,"mailbox":{}}\n';
  const write = spawnSync(
    process.execPath,
    ["-e", "require('node:fs').writeFileSync(process.argv[1], process.argv[2])", policy, original],
  );
  assert.equal(write.status, 0);

  const result = spawnSync(process.execPath, [cli, "plan", "--policy", policy], {
    encoding: "utf8",
  });

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Cannot render an invalid Autopilot policy/);
  assert.equal(readFileSync(policy, "utf8"), original);
});
