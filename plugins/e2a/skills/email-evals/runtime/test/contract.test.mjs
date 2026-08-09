import assert from "node:assert/strict";
import { mkdir, mkdtemp, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { loadSuite } from "../lib/contract.mjs";
import { normalizeAddressSet, normalizeMailbox, parseDuration } from "../lib/normalize.mjs";
import { scaffoldSuite } from "../../scaffold.mjs";

const testDirectory = path.dirname(fileURLToPath(import.meta.url));
const fixture = (name) => path.join(testDirectory, "..", "testdata", name);
const validEnvironment = {
  E2A_EVAL_TARGET: "target@eval.test",
  E2A_EVAL_ACTOR: "actor@eval.test",
  E2A_EVAL_API_KEY: "e2a_acct_synthetic",
};

test("loadSuite resolves complete scalar references and canonicalizes the closed contract", async () => {
  const suite = await loadSuite(fixture("contracts/valid/suite.yaml"), { environment: validEnvironment });
  assert.equal(suite.version, 1);
  assert.equal(suite.name, "fictional-support-smoke");
  assert.equal(suite.target.email, "target@eval.test");
  assert.equal(suite.actor.email, "actor@eval.test");
  assert.equal(suite.transport.apiKey, "e2a_acct_synthetic");
  assert.equal(suite.transport.baseUrl, "https://api.e2a.dev");
  assert.deepEqual(suite.transport.allowedEnvelopeRecipients, ["actor@eval.test", "target@eval.test"]);
  assert.deepEqual(suite.defaults, { timeoutMs: 60_000, settleMs: 5_000, pollIntervalMs: 500 });
  assert.match(suite.digest, /^[a-f0-9]{64}$/);
  assert.equal(suite.cases.length, 3);
  assert.equal(suite.cases[0].expect.timing.replyWithinMs, 60_000);
  assert.deepEqual(suite.cases[0].expect.recipients.envelope.exactly, ["actor@eval.test"]);
});

test("digest uses the unresolved alias-safe contract, not an API-key value", async () => {
  const first = await loadSuite(fixture("contracts/valid/suite.yaml"), { environment: validEnvironment });
  const second = await loadSuite(fixture("contracts/valid/suite.yaml"), {
    environment: { ...validEnvironment, E2A_EVAL_API_KEY: "e2a_acct_rotated_synthetic" },
  });
  assert.equal(first.digest, second.digest);
  assert.doesNotMatch(JSON.stringify(first).replace(first.transport.apiKey, "[key]"), /e2a_acct_rotated_synthetic/);
});

for (const [name, code] of [
  ["unknown-key", "unknown_key"],
  ["duplicate-key", "duplicate_key"],
  ["partial-environment", "partial_environment_reference"],
  ["missing-environment", "missing_environment"],
  ["bad-duration", "invalid_duration"],
  ["bad-regex", "invalid_regex"],
  ["case-traversal", "path_outside_suite"],
  ["missing-envelope-expectation", "missing_envelope_allowlist"],
  ["recipient-outside-allowlist", "recipient_outside_allowlist"],
]) {
  test(name, async () => {
    await assert.rejects(
      loadSuite(fixture(`contracts/invalid/${name}/suite.yaml`), { environment: validEnvironment }),
      (error) => error.errorClass === "configuration_error" && error.code === code,
    );
  });
}

test("case symlinks cannot escape the suite root after realpath resolution", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-contract-"));
  const outside = path.join(root, "outside.yaml");
  await writeFile(outside, "id: escaped\nsend: { subject: x, text: x }\nexpect: { action: { kind: none, count: 0 } }\n");
  const suiteRoot = path.join(root, "suite");
  await mkdir(suiteRoot);
  await symlink(outside, path.join(suiteRoot, "case.yaml"));
  await writeFile(path.join(suiteRoot, "suite.yaml"), [
    "version: 1", "name: synthetic", "target:", "  email: ${E2A_EVAL_TARGET}",
    "actor:", "  email: ${E2A_EVAL_ACTOR}", "transport:", "  adapter: e2a", "  api_key: ${E2A_EVAL_API_KEY}",
    "  allowed_envelope_recipients: [\"${E2A_EVAL_TARGET}\", \"${E2A_EVAL_ACTOR}\"]", "cases: [case.yaml]", "",
  ].join("\n"));
  await assert.rejects(
    loadSuite(path.join(suiteRoot, "suite.yaml"), { environment: validEnvironment }),
    (error) => error.errorClass === "configuration_error" && error.code === "path_outside_suite",
  );
});

test("normalizers preserve display names, reject duplicates before dedupe, and bound durations", () => {
  assert.deepEqual(normalizeMailbox("Example Person <ACTOR@EVAL.TEST>"), {
    address: "actor@eval.test", displayName: "Example Person",
  });
  assert.deepEqual(normalizeAddressSet(["Target <TARGET@EVAL.TEST>", "actor@eval.test"]), ["actor@eval.test", "target@eval.test"]);
  assert.throws(() => normalizeAddressSet(["actor@eval.test", "ACTOR@EVAL.TEST"]), /duplicate/i);
  assert.equal(parseDuration("60s"), 60_000);
  assert.equal(parseDuration("2m"), 120_000);
  assert.throws(() => parseDuration("0s"), /duration/i);
  assert.throws(() => parseDuration("999h"), /duration/i);
});

test("scaffolded synthetic starter templates satisfy the same closed loader", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-scaffold-contract-"));
  await scaffoldSuite({
    root,
    suiteName: "fictional-support-smoke",
    targetEnv: "E2A_EVAL_TARGET",
    actorEnv: "E2A_EVAL_ACTOR",
    apiKeyEnv: "E2A_EVAL_API_KEY",
  });
  const suite = await loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment });
  assert.equal(suite.cases.length, 3);
});
