import assert from "node:assert/strict";
import { mkdir, mkdtemp, readFile, rename, symlink, writeFile } from "node:fs/promises";
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
  assert.equal(suite.cases[0].expect.body.forbiddenPatternRegexes[0] instanceof RegExp, true);
  assert.doesNotMatch(JSON.stringify(suite.cases[0].expect.body), /forbiddenPatternRegexes/);
});

test("environment-backed regular expressions are rejected for replay-safe grading", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-env-regex-"));
  await mkdir(path.join(root, "cases"));
  await writeFile(path.join(root, "suite.yaml"), `
version: 1
name: synthetic
target: { email: "\${E2A_EVAL_TARGET}" }
actor: { email: "\${E2A_EVAL_ACTOR}" }
transport:
  adapter: e2a
  api_key: "\${E2A_EVAL_API_KEY}"
  base_url: https://api.example.test
  allowed_envelope_recipients: ["\${E2A_EVAL_ACTOR}", "\${E2A_EVAL_TARGET}"]
cases: [cases/env-regex.yaml]
`);
  await writeFile(path.join(root, "cases/env-regex.yaml"), `
id: env-regex
send: { subject: Synthetic, text: Synthetic }
expect:
  action: { kind: none, count: 0 }
  body: { forbidden_patterns: ["\${E2A_EVAL_PATTERN}"] }
`);
  await assert.rejects(
    loadSuite(path.join(root, "suite.yaml"), { environment: { ...validEnvironment, E2A_EVAL_PATTERN: "secret-[0-9]+" } }),
    (error) => error.errorClass === "configuration_error" && error.code === "regex_environment_not_supported",
  );
});

test("digest uses the unresolved alias-safe contract, not an API-key value", async () => {
  const first = await loadSuite(fixture("contracts/valid/suite.yaml"), { environment: validEnvironment });
  const second = await loadSuite(fixture("contracts/valid/suite.yaml"), {
    environment: { ...validEnvironment, E2A_EVAL_API_KEY: "synthetic-key-rotated" },
  });
  assert.equal(first.digest, second.digest);
  assert.doesNotMatch(JSON.stringify(first).replace(first.transport.apiKey, "[key]"), /synthetic-key-rotated/);
});

async function writeMinimalSuite(root, { name = "synthetic", caseSource, recipients = "[\"${E2A_EVAL_TARGET}\", \"${E2A_EVAL_ACTOR}\"]" } = {}) {
  await writeFile(path.join(root, "suite.yaml"), [
    "version: 1", `name: ${name}`, "target:", "  email: ${E2A_EVAL_TARGET}", "actor:", "  email: ${E2A_EVAL_ACTOR}",
    "transport:", "  adapter: e2a", "  api_key: ${E2A_EVAL_API_KEY}", `  allowed_envelope_recipients: ${recipients}`,
    "cases: [case.yaml]", "",
  ].join("\n"));
  await writeFile(path.join(root, "case.yaml"), caseSource ?? [
    "id: synthetic-case", "send: { subject: Synthetic, text: Synthetic }", "expect:", "  action: { kind: none, count: 0 }", "",
  ].join("\n"));
}

test("complete references resolve in IDs, enums, policies, and nested attachments", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-env-"));
  await writeMinimalSuite(root, { name: "${E2A_SUITE_NAME}", caseSource: [
    "id: ${E2A_CASE_ID}", "send:", "  subject: Synthetic", "  text: Synthetic", "expect:", "  action:", "    kind: ${E2A_ACTION}", "    count: 0",
    "  subject:", "    policy: ${E2A_POLICY}", "  attachments:", "    exactly:", "      - filename: ${E2A_FILENAME}", "        content_type: ${E2A_CONTENT_TYPE}", "        disposition: ${E2A_DISPOSITION}", "        sha256: ${E2A_HASH}", "",
  ].join("\n") });
  const suite = await loadSuite(path.join(root, "suite.yaml"), { environment: {
    ...validEnvironment, E2A_SUITE_NAME: "synthetic", E2A_CASE_ID: "synthetic-case", E2A_ACTION: "none", E2A_POLICY: "preserve",
    E2A_FILENAME: "synthetic.txt", E2A_CONTENT_TYPE: "text/plain", E2A_DISPOSITION: "attachment", E2A_HASH: "abc",
  } });
  assert.equal(suite.cases[0].id, "synthetic-case");
  assert.equal(suite.cases[0].expect.action.kind, "none");
  assert.equal(suite.cases[0].expect.attachments.exactly[0].filename, "synthetic.txt");
});

test("empty attachment expectation mappings are rejected instead of becoming count-only", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-attachment-"));
  await writeMinimalSuite(root, { caseSource: [
    "id: synthetic-case", "send: { subject: Synthetic, text: Synthetic }", "expect:", "  action: { kind: none, count: 0 }", "  attachments:", "    exactly: [{}]", "",
  ].join("\n") });
  await assert.rejects(loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }), (error) => error.errorClass === "configuration_error" && error.code === "invalid_attachment_expectation");
});

for (const [field, source] of [
  ["id", "id: bad-${E2A_CASE_ID}"],
  ["enum", "expect:\n  action:\n    kind: reply-${E2A_ACTION}\n    count: 1"],
  ["policy", "expect:\n  action:\n    kind: none\n    count: 0\n  subject:\n    policy: preserve-${E2A_POLICY}"],
  ["attachment", "expect:\n  action:\n    kind: none\n    count: 0\n  attachments:\n    exactly:\n      - filename: bad-${E2A_FILENAME}"],
]) {
  test(`partial references fail in nested ${field} scalars`, async () => {
    const root = await mkdtemp(path.join(tmpdir(), "email-evals-partial-"));
    const caseSource = field === "id"
      ? `${source}\nsend: { subject: Synthetic, text: Synthetic }\nexpect: { action: { kind: none, count: 0 } }\n`
      : `id: synthetic-case\nsend: { subject: Synthetic, text: Synthetic }\n${source}\n`;
    await writeMinimalSuite(root, { caseSource });
    await assert.rejects(loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }), (error) => error.code === "partial_environment_reference");
  });
}

test("digest aliases literal display-name mailboxes and canonicalizes reordered recipient sets", async () => {
  const firstRoot = await mkdtemp(path.join(tmpdir(), "email-evals-digest-"));
  const secondRoot = await mkdtemp(path.join(tmpdir(), "email-evals-digest-"));
  const caseSource = "id: synthetic-case\nsend: { subject: Synthetic, text: Synthetic }\nexpect: { action: { kind: none, count: 0 } }\n";
  await writeMinimalSuite(firstRoot, { recipients: "[\"Actor <ACTOR@EVAL.TEST>\", \"Target <TARGET@EVAL.TEST>\"]", caseSource });
  await writeMinimalSuite(secondRoot, { recipients: "[\"Target <target@eval.test>\", \"Actor <actor@eval.test>\"]", caseSource });
  const first = await loadSuite(path.join(firstRoot, "suite.yaml"), { environment: validEnvironment });
  const second = await loadSuite(path.join(secondRoot, "suite.yaml"), { environment: validEnvironment });
  assert.equal(first.digest, second.digest);
});

test("digest never aliases ordinary text that happens to equal a typed mailbox", async () => {
  const firstRoot = await mkdtemp(path.join(tmpdir(), "email-evals-digest-text-"));
  const secondRoot = await mkdtemp(path.join(tmpdir(), "email-evals-digest-text-"));
  const caseFor = (literal) => [
    "id: synthetic-case", "send:", `  subject: ${literal}`, "  text: Synthetic", "expect:", "  action: { kind: none, count: 0 }",
    "  body:", `    required_facts: [${literal}]`, "  attachments:", "    exactly:", `      - filename: ${literal}`, "",
  ].join("\n");
  await writeMinimalSuite(firstRoot, { caseSource: caseFor("actor@eval.test") });
  await writeMinimalSuite(secondRoot, { caseSource: caseFor("actor") });
  const first = await loadSuite(path.join(firstRoot, "suite.yaml"), { environment: validEnvironment });
  const second = await loadSuite(path.join(secondRoot, "suite.yaml"), { environment: validEnvironment });
  assert.notEqual(first.digest, second.digest);
});

test("alias expansion and deterministic file swaps are stable configuration errors", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-io-"));
  await writeMinimalSuite(root);
  await writeFile(path.join(root, "suite.yaml"), `version: 1\nname: &value synthetic\ntarget: { email: *value }\n${"cases: [*value, *value, *value, *value, *value, *value, *value, *value, *value, *value]\n".repeat(20)}`);
  await assert.rejects(loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }), /Suite YAML is invalid/);

  await writeMinimalSuite(root);
  await writeFile(path.join(root, "suite-replacement.yaml"), await readFile(path.join(root, "suite.yaml"), "utf8"));
  await assert.rejects(loadSuite(path.join(root, "suite.yaml"), {
    environment: validEnvironment,
    beforeRead: async ({ label }) => { if (label === "suite") await rename(path.join(root, "suite-replacement.yaml"), path.join(root, "suite.yaml")); },
  }), (error) => error.errorClass === "configuration_error" && error.code === "file_changed_during_load");

  await writeMinimalSuite(root);
  await writeFile(path.join(root, "replacement.yaml"), await readFile(path.join(root, "case.yaml"), "utf8"));
  await assert.rejects(loadSuite(path.join(root, "suite.yaml"), {
    environment: validEnvironment,
    beforeRead: async ({ label }) => { if (label === "case") await rename(path.join(root, "replacement.yaml"), path.join(root, "case.yaml")); },
  }), (error) => error.errorClass === "configuration_error" && error.code === "file_changed_during_load");
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
