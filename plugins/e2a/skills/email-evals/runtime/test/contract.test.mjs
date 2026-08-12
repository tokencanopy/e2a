import assert from "node:assert/strict";
import { mkdir, mkdtemp, readFile, rename, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { loadSuite } from "../lib/contract.mjs";
import {
  normalizeAddressSet,
  normalizeMailbox,
  parseDuration,
  replaceMailboxText,
} from "../lib/normalize.mjs";
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
  assert.match(suite.executionDigest, /^[a-f0-9]{64}$/);
  assert.equal(suite.cases.length, 3);
  assert.equal(suite.cases[0].expect.timing.replyWithinMs, 60_000);
  assert.deepEqual(suite.cases[0].expect.recipients.envelope.exactly, ["actor@eval.test"]);
  assert.equal(suite.cases[0].expect.body.forbiddenPatternPatterns[0].test("sk-synthetic"), true);
  assert.doesNotMatch(JSON.stringify(suite.cases[0].expect.body), /forbiddenPatternPatterns/);
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
    (error) => error.errorClass === "configuration_error" && error.code === "environment_reference_not_allowed",
  );
});

test("unsupported regex syntax fails as invalid_regex at its YAML pointer before execution", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-unsupported-regex-"));
  await writeMinimalSuite(root, { caseSource: [
    "id: synthetic-case", "send: { subject: Synthetic, text: Synthetic }", "expect:",
    "  action: { kind: none, count: 0 }", "  body:", "    forbidden_patterns: ['(a+)\\1']", "",
  ].join("\n") });
  await assert.rejects(
    loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }),
    (error) => error.errorClass === "configuration_error"
      && error.code === "invalid_regex"
      && error.details?.path === "/expect/body/forbidden_patterns/0",
  );
});

test("unsupported subject regex syntax fails at the scalar YAML pointer before execution", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-unsupported-subject-regex-"));
  await writeMinimalSuite(root, { caseSource: [
    "id: synthetic-case", "send: { subject: Synthetic, text: Synthetic }", "expect:",
    "  action: { kind: none, count: 0 }", "  subject:", "    regex: '(a+)\\1'", "",
  ].join("\n") });
  await assert.rejects(
    loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }),
    (error) => error.errorClass === "configuration_error"
      && error.code === "invalid_regex"
      && error.details?.path === "/expect/subject/regex",
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

test("suite timeout budget leaves bounded overhead beneath the public launcher wall", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-suite-budget-"));
  const caseSource = (id) => [
    `id: ${id}`, "send: { subject: Synthetic, text: Synthetic }",
    "expect: { action: { kind: none, count: 0 } }", "",
  ].join("\n");
  await writeFile(path.join(root, "first.yaml"), caseSource("first"));
  await writeFile(path.join(root, "second.yaml"), caseSource("second"));
  const suiteSource = (timeout) => [
    "version: 1", "name: synthetic", "target: { email: \"${E2A_EVAL_TARGET}\" }",
    "actor: { email: \"${E2A_EVAL_ACTOR}\" }", "transport:", "  adapter: e2a",
    "  api_key: \"${E2A_EVAL_API_KEY}\"",
    "  allowed_envelope_recipients: [\"${E2A_EVAL_ACTOR}\", \"${E2A_EVAL_TARGET}\"]",
    `defaults: { timeout: ${timeout}, settle: 1s, poll_interval: 1s }`,
    "cases: [first.yaml, second.yaml]", "",
  ].join("\n");
  await writeFile(path.join(root, "suite.yaml"), suiteSource("750s"));
  await assert.doesNotReject(loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }));
  await writeFile(path.join(root, "suite.yaml"), suiteSource("13m"));
  await assert.rejects(
    loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }),
    (error) => error.errorClass === "configuration_error"
      && error.code === "suite_timeout_budget_exceeded"
      && error.details?.path === "/defaults/timeout",
  );
});

test("action cardinality cannot exceed the bounded observation set", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-action-count-"));
  const source = (count) => [
    "id: synthetic-case", "send: { subject: Synthetic, text: Synthetic }", "expect:",
    `  action: { kind: reply, count: ${count} }`,
    "  recipients: { envelope: { exactly: [\"${E2A_EVAL_ACTOR}\"] } }", "",
  ].join("\n");
  await writeMinimalSuite(root, { caseSource: source(100) });
  await assert.doesNotReject(loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }));
  await writeMinimalSuite(root, { caseSource: source(101) });
  await assert.rejects(
    loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }),
    (error) => error.errorClass === "configuration_error" && error.code === "invalid_action_count"
      && error.details?.path === "/expect/action/count",
  );
});

test("full and execution digests separate assertion-only edits from execution edits", async () => {
  const roots = await Promise.all([0, 1, 2, 3].map(() => mkdtemp(path.join(tmpdir(), "email-evals-digest-split-"))));
  const caseFor = ({ actorReceived, text, fact }) => [
    "id: synthetic-case", "send:", "  subject: Synthetic", `  text: ${text}`, "expect:",
    "  action: { kind: none, count: 0 }", `  body: { required_facts: [${fact}] }`,
    `  lifecycle: { actor_received: ${actorReceived} }`, "",
  ].join("\n");
  await writeMinimalSuite(roots[0], { caseSource: caseFor({ actorReceived: true, text: "Synthetic", fact: "First" }) });
  await writeMinimalSuite(roots[1], { caseSource: caseFor({ actorReceived: true, text: "Synthetic", fact: "Second" }) });
  await writeMinimalSuite(roots[2], { caseSource: caseFor({ actorReceived: true, text: "Changed", fact: "First" }) });
  await writeMinimalSuite(roots[3], { caseSource: caseFor({ actorReceived: false, text: "Synthetic", fact: "First" }) });
  const [first, assertionEdit, executionEdit, receiptEdit] = await Promise.all(roots.map((root) => (
    loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment })
  )));
  assert.notEqual(first.digest, assertionEdit.digest);
  assert.equal(first.executionDigest, assertionEdit.executionDigest);
  assert.notEqual(first.executionDigest, executionEdit.executionDigest);
  assert.notEqual(first.executionDigest, receiptEdit.executionDigest);
});

test("digests bind resolved actor target and containment identities without binding the API key", async () => {
  const suiteFile = fixture("contracts/valid/suite.yaml");
  const first = await loadSuite(suiteFile, { environment: validEnvironment });
  const rotated = await loadSuite(suiteFile, { environment: {
    ...validEnvironment,
    E2A_EVAL_ACTOR: "rotated-actor@eval.test",
    E2A_EVAL_TARGET: "rotated-target@eval.test",
  } });
  const keyOnly = await loadSuite(suiteFile, { environment: {
    ...validEnvironment,
    E2A_EVAL_API_KEY: "synthetic-key-rotated",
  } });
  assert.notEqual(first.digest, rotated.digest);
  assert.notEqual(first.executionDigest, rotated.executionDigest);
  assert.equal(first.digest, keyOnly.digest);
  assert.equal(first.executionDigest, keyOnly.executionDigest);
});

test("sent_as accepts bounded future-safe tokens and rejects mailbox-shaped or credential-overlapping values", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-sent-as-"));
  const source = (sentAs) => [
    "id: synthetic-case", "send: { subject: Synthetic, text: Synthetic }", "expect:",
    "  action: { kind: none, count: 0 }", "  sender:", `    sent_as: ${sentAs}`, "",
  ].join("\n");
  await writeMinimalSuite(root, { caseSource: source("future_route") });
  assert.equal((await loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment })).cases[0].expect.sender.sentAs, "future_route");
  await writeMinimalSuite(root, { caseSource: source("target@eval.test") });
  await assert.rejects(
    loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }),
    (error) => error.errorClass === "configuration_error" && error.code === "invalid_sent_as",
  );
  await writeMinimalSuite(root, { caseSource: source("e2a_custom") });
  await assert.rejects(
    loadSuite(path.join(root, "suite.yaml"), { environment: { ...validEnvironment, E2A_EVAL_API_KEY: "e2a_custom" } }),
    (error) => error.errorClass === "configuration_error" && error.code === "sent_as_conflicts_credential",
  );
  await assert.rejects(
    loadSuite(path.join(root, "suite.yaml"), { environment: { ...validEnvironment, E2A_EVAL_API_KEY: "custom" } }),
    (error) => error.errorClass === "configuration_error" && error.code === "sent_as_conflicts_credential",
  );
});

test("suite loading bounds files, cases, arrays, and semantic strings before execution", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-bounds-"));
  await writeMinimalSuite(root, { caseSource: [
    "id: synthetic-case", `send: { subject: ${"s".repeat(999)}, text: Synthetic }`,
    "expect: { action: { kind: none, count: 0 } }", "",
  ].join("\n") });
  await assert.rejects(loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }), (error) => error.code === "string_too_large");

  await writeMinimalSuite(root, { caseSource: [
    "id: synthetic-case", "send: { subject: Synthetic, text: Synthetic }", "expect:",
    "  action: { kind: none, count: 0 }", `  attachments: { exactly: ${JSON.stringify(Array.from({ length: 33 }, (_, index) => `file-${index}`))} }`, "",
  ].join("\n") });
  await assert.rejects(loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }), (error) => error.code === "array_too_large");

  await writeMinimalSuite(root);
  const suiteSource = await readFile(path.join(root, "suite.yaml"), "utf8");
  await writeFile(path.join(root, "suite.yaml"), suiteSource.replace("cases: [case.yaml]", `cases: ${JSON.stringify(Array(101).fill("case.yaml"))}`));
  await assert.rejects(loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }), (error) => error.code === "array_too_large");

  await writeMinimalSuite(root);
  await writeFile(path.join(root, "case.yaml"), `# ${"x".repeat(256 * 1024)}\n`);
  await assert.rejects(loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }), (error) => error.code === "yaml_too_large");
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

test("only documented credential and mailbox fields accept complete environment references", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-env-"));
  await writeMinimalSuite(root, { name: "${E2A_SUITE_NAME}", caseSource: [
    "id: ${E2A_CASE_ID}", "send:", "  subject: Synthetic", "  text: Synthetic", "expect:", "  action:", "    kind: ${E2A_ACTION}", "    count: 0",
    "  subject:", "    policy: ${E2A_POLICY}", "  attachments:", "    exactly:", "      - filename: ${E2A_FILENAME}", "        content_type: ${E2A_CONTENT_TYPE}", "        disposition: ${E2A_DISPOSITION}", "        sha256: ${E2A_HASH}", "",
  ].join("\n") });
  await assert.rejects(
    loadSuite(path.join(root, "suite.yaml"), { environment: {
      ...validEnvironment, E2A_SUITE_NAME: "synthetic", E2A_CASE_ID: "synthetic-case", E2A_ACTION: "none", E2A_POLICY: "preserve",
      E2A_FILENAME: "synthetic.txt", E2A_CONTENT_TYPE: "text/plain", E2A_DISPOSITION: "attachment", E2A_HASH: "abc",
    } }),
    (error) => error.errorClass === "configuration_error" && error.code === "environment_reference_not_allowed",
  );
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

  await writeMinimalSuite(root);
  const caseFile = path.join(root, "case.yaml");
  const original = await readFile(caseFile, "utf8");
  const before = await stat(caseFile);
  await assert.rejects(loadSuite(path.join(root, "suite.yaml"), {
    environment: validEnvironment,
    beforeRead: async ({ label }) => {
      if (label === "case") await writeFile(caseFile, original.replace("subject: Synthetic", "subject: Fictional"));
    },
  }), (error) => error.errorClass === "configuration_error" && error.code === "file_changed_during_load");
  const after = await stat(caseFile);
  assert.equal(after.ino, before.ino, "regression must mutate the same inode");
  assert.equal(after.size, before.size, "regression must preserve descriptor size");

  await writeMinimalSuite(root);
  await assert.rejects(loadSuite(path.join(root, "suite.yaml"), {
    environment: validEnvironment,
    beforeRead: async ({ label }) => {
      if (label === "case") await writeFile(caseFile, `${await readFile(caseFile, "utf8")}# bounded growth\n`);
    },
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

test("case symlinks are rejected before they can escape the suite root", async () => {
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
    (error) => error.errorClass === "configuration_error" && error.code === "case_file_unreadable",
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

test("normalizers preserve parser-valid quoted local parts as round-trippable addresses", () => {
  const mailboxes = [
    ['"a b"@EXAMPLE.COM', { address: '"a b"@example.com', displayName: undefined }],
    ['" a"@EXAMPLE.COM', { address: '" a"@example.com', displayName: undefined }],
    ['"a "@EXAMPLE.COM', { address: '"a "@example.com', displayName: undefined }],
    ['"a@b"@EXAMPLE.COM', { address: '"a@b"@example.com', displayName: undefined }],
    ['"a"@[127.0.0.1]', { address: '"a"@[127.0.0.1]', displayName: undefined }],
    ['Quoted Person <"a b"@EXAMPLE.COM> (comment)', { address: '"a b"@example.com', displayName: "Quoted Person" }],
    ['Edge Person <" a "@EXAMPLE.COM> (comment)', { address: '" a "@example.com', displayName: "Edge Person" }],
    ['(leading) "a b"@EXAMPLE.COM (trailing)', { address: '"a b"@example.com', displayName: undefined }],
    ['Escaped Quote <"a\\\"b"@EXAMPLE.COM>', { address: '"a\\\"b"@example.com', displayName: "Escaped Quote" }],
    ['Escaped Slash <"a\\\\b"@EXAMPLE.COM>', { address: '"a\\\\b"@example.com', displayName: "Escaped Slash" }],
    ['Escaped Space <"a\\ b"@EXAMPLE.COM>', { address: '"a b"@example.com', displayName: "Escaped Space" }],
  ];
  for (const [source, expected] of mailboxes) {
    const normalized = normalizeMailbox(source);
    assert.deepEqual(normalized, expected);
    assert.equal(normalizeMailbox(normalized.address).address, normalized.address);
  }
  assert.throws(() => normalizeAddressSet(['"A B"@example.com', '"a b"@EXAMPLE.COM']), /duplicate/i);
  assert.throws(() => normalizeAddressSet(['"a b"@example.com', '"a\\ b"@EXAMPLE.COM']), /duplicate/i);
  for (const malformed of [
    '"unterminated@example.com', '"dangling\\"@example.com', '"control\u0000"@example.com',
  ]) assert.throws(() => normalizeMailbox(malformed), /mailbox/i);
});

test("quoted mailbox normalization preserves a whitespace-only local part", () => {
  assert.deepEqual(normalizeMailbox('" "@EXAMPLE.COM'), {
    address: '" "@example.com',
    displayName: undefined,
  });
  assert.equal(normalizeMailbox('" "@example.com').address, '" "@example.com');
});

test("quoted mailbox normalization accepts nested surrounding CFWS", () => {
  assert.deepEqual(normalizeMailbox('(outer(inner)) "a b"@EXAMPLE.COM (tail(nested))'), {
    address: '"a b"@example.com',
    displayName: undefined,
  });
  assert.deepEqual(normalizeMailbox('Quoted Person <"a b"@EXAMPLE.COM> (tail(nested))'), {
    address: '"a b"@example.com',
    displayName: "Quoted Person",
  });
});

test("mailbox normalization rejects raw and escaped controls", () => {
  const controls = ["\t", "\r", "\n", "\0", "\x1f", "\x7f"];
  const escapedTabLiteral = `"a"@[foo\\${controls[0]}bar]`;
  assert.throws(() => normalizeMailbox(escapedTabLiteral), /mailbox/i);
  assert.equal(replaceMailboxText(escapedTabLiteral, () => "unexpected"), "[REDACTED:address]");

  for (const control of controls) {
    for (const mailbox of [
      `"a${control}b"@example.com`,
      `"a\\${control}b"@example.com`,
      `"a"@[foo${control}bar]`,
      `"a"@[foo\\${control}bar]`,
    ]) assert.throws(() => normalizeMailbox(mailbox), /mailbox/i);
  }
});

test("mailbox normalization rejects malformed comments, quotes, and brackets conservatively", () => {
  const malformed = [
    '"a"@[foo\\]',
    '"a"@[unterminated',
    '"a"@example.com)',
    '"a"@example].com',
    '(unterminated "a"@example.com',
    '(dangling\\ "a"@example.com',
    '"unterminated@example.com',
    '"dangling\\"@example.com',
  ];
  for (const mailbox of malformed) {
    assert.throws(() => normalizeMailbox(mailbox), /mailbox/i);
    assert.doesNotMatch(replaceMailboxText(mailbox, () => "aliased"), /@example|@\[foo/u);
  }
});

test("closed contract rejects oversized suite and case identifiers with safe pointers", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-identifiers-"));
  await writeMinimalSuite(root, { name: "s".repeat(129) });
  await assert.rejects(
    loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }),
    (error) => error.errorClass === "configuration_error" && error.code === "identifier_too_long"
      && error.details?.path === "/name" && Object.keys(error.details).length === 1,
  );

  await writeMinimalSuite(root, { caseSource: [
    `id: ${"c".repeat(129)}`, "send: { subject: Synthetic, text: Synthetic }", "expect: { action: { kind: none, count: 0 } }", "",
  ].join("\n") });
  await assert.rejects(
    loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }),
    (error) => error.errorClass === "configuration_error" && error.code === "identifier_too_long"
      && error.details?.path === "/cases/0/id" && Object.keys(error.details).length === 1,
  );

  for (const [literal, code] of [
    [`${"é".repeat(64)}a`, "identifier_too_long"],
    ["é".repeat(64), "invalid_case_id"],
  ]) {
    await writeMinimalSuite(root, {
      caseSource: [
        `id: ${literal}`,
        "send: { subject: Synthetic, text: Synthetic }",
        "expect: { action: { kind: none, count: 0 } }", "",
      ].join("\n"),
    });
    await assert.rejects(
      loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment }),
      (error) => error.errorClass === "configuration_error" && error.code === code
        && error.details?.path === "/cases/0/id" && Object.keys(error.details).length === 1,
    );
  }
});

test("scaffolded synthetic starter templates satisfy the same closed loader", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-scaffold-contract-"));
  await scaffoldSuite({
    root,
    suiteName: "fictional-support-smoke",
    targetEnv: "E2A_EVAL_TARGET",
    actorEnv: "E2A_EVAL_ACTOR",
  });
  const suite = await loadSuite(path.join(root, "suite.yaml"), { environment: validEnvironment });
  assert.equal(suite.cases.length, 3);
});
