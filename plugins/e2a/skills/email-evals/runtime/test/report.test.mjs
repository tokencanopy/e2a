import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, rename, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { loadSuite } from "../lib/contract.mjs";
import {
  aliasCaseRecord,
  createArtifactWriter,
  renderMarkdown,
  validateRunId,
} from "../lib/report.mjs";

const ACTOR = "actor@eval.test";
const TARGET = "target@eval.test";
const RUN_ID = "run_20260808T120000_0123abcd";

function suite() {
  return {
    version: 1,
    name: "synthetic-suite",
    digest: "a".repeat(64),
    actor: { email: ACTOR },
    target: { email: TARGET },
    transport: {
      apiKey: "synthetic-credential",
      allowedEnvelopeRecipients: [TARGET, "zeta@eval.test", ACTOR, "alpha@eval.test"],
    },
    cases: [],
  };
}

async function root() {
  return mkdtemp(path.join(tmpdir(), "email-evals-report-"));
}

test("validateRunId accepts only the exact run format", () => {
  assert.equal(validateRunId(RUN_ID), RUN_ID);
  for (const value of ["../escape", "run_20260808T120000_deadbeef/child", "run_2026-08-08_deadbeef", "RUN_20260808T120000_DEADBEEF"]) {
    assert.throws(() => validateRunId(value), /run ID/i);
  }
});

test("artifact writer creates a private run and refuses collisions without overwrite", async () => {
  const outputRoot = await root();
  const first = await createArtifactWriter({ outputRoot, runId: RUN_ID });
  await first.appendCase({ id: "one", status: "pass" }, { status: "pass", counts: { total: 1, passed: 1, failed: 0, errors: 0 } });
  await first.finalize({ status: "pass", counts: { total: 1, passed: 1, failed: 0, errors: 0 }, cases: [{ id: "one", status: "pass", assertions: [] }] });
  assert.equal((await stat(first.runDirectory)).mode & 0o777, 0o700);
  const before = await readFile(first.files.cases, "utf8");
  await assert.rejects(createArtifactWriter({ outputRoot, runId: RUN_ID }), /already exists/i);
  assert.equal(await readFile(first.files.cases, "utf8"), before);
});

test("artifact writer refuses symlink roots and cannot clobber a summary symlink", async () => {
  const parent = await root();
  const outside = await root();
  const linked = path.join(parent, "linked");
  await symlink(outside, linked);
  await assert.rejects(createArtifactWriter({ outputRoot: linked, runId: RUN_ID }), /symlink/i);

  const writer = await createArtifactWriter({ outputRoot: parent, runId: RUN_ID });
  const sentinel = path.join(outside, "sentinel.json");
  await writeFile(sentinel, "sentinel\n");
  await symlink(sentinel, writer.files.summary);
  await assert.rejects(
    writer.appendCase({ id: "one", status: "pass" }, { status: "pass", counts: { total: 1, passed: 1, failed: 0, errors: 0 } }),
    /symlink/i,
  );
  await writer.close();
  assert.equal(await readFile(sentinel, "utf8"), "sentinel\n");
});

test("finalize rejects a swapped run parent and preserves outside artifacts", async () => {
  const outputRoot = await root();
  const outside = await root();
  const writer = await createArtifactWriter({ outputRoot, runId: RUN_ID });
  await writer.appendCase({ id: "one", status: "pass" }, { status: "pass", counts: { total: 1, passed: 1, failed: 0, errors: 0 } });
  const moved = path.join(outputRoot, `${RUN_ID}-moved`);
  await rename(writer.runDirectory, moved);
  await writeFile(path.join(outside, "summary.json"), "summary-sentinel\n");
  await writeFile(path.join(outside, "report.md"), "report-sentinel\n");
  await symlink(outside, writer.runDirectory);
  await assert.rejects(
    writer.finalize({ status: "pass", counts: { total: 1, passed: 1, failed: 0, errors: 0 }, cases: [] }),
    /run directory changed/i,
  );
  await writer.close();
  assert.equal(await readFile(path.join(outside, "summary.json"), "utf8"), "summary-sentinel\n");
  assert.equal(await readFile(path.join(outside, "report.md"), "utf8"), "report-sentinel\n");
});

test("aliasing covers nested normalized address fields but preserves display text collisions", () => {
  const record = aliasCaseRecord({
    id: "alias",
    status: "fail",
    expectation: {
      sender: { exactly: TARGET, displayName: ACTOR, replyTo: { exactly: [] } },
      recipients: { to: { exactly: [ACTOR] }, cc: { exactly: ["alpha@eval.test"] }, bcc: { exactly: [] }, envelope: { exactly: [ACTOR, "alpha@eval.test"] } },
      body: { requiredFacts: [ACTOR] },
    },
    evidence: {
      target: { email: TARGET },
      stimulus: { participants: [ACTOR] },
      candidates: [{
        from: TARGET, to: [ACTOR], cc: ["alpha@eval.test"], bcc: [], envelopeRecipients: [ACTOR, "alpha@eval.test"],
        rawMime: "must-not-escape", attachmentBytes: [1, 2, 3], mime: { text: ACTOR },
      }],
    },
    assertions: [{
      id: "recipients.to", status: "fail", code: "recipient_set_mismatch", expected: { exactly: [ACTOR] },
      actual: { addresses: [ACTOR], movements: [{ address: "alpha@eval.test", expectedField: "cc", actualField: "to" }] }, evidenceRefs: [],
    }],
    primaryError: null,
    secondaryErrors: [],
  }, suite());
  assert.equal(record.expectation.sender.exactly, "target");
  assert.equal(record.expectation.sender.displayName, ACTOR);
  assert.equal(record.expectation.body.requiredFacts[0], ACTOR);
  assert.deepEqual(record.expectation.recipients.envelope.exactly, ["actor", "probe:1"]);
  assert.equal(record.evidence.candidates[0].from, "target");
  assert.equal(record.evidence.candidates[0].mime.text, ACTOR);
  assert.equal(Object.hasOwn(record.evidence.candidates[0], "rawMime"), false);
  assert.equal(Object.hasOwn(record.evidence.candidates[0], "attachmentBytes"), false);
  assert.equal(record.assertions[0].actual.addresses[0], "actor");
  assert.equal(record.assertions[0].actual.movements[0].address, "probe:1");
});

test("forbidden diagnostic matches and secrets in errors are redacted without RegExp state leaks", () => {
  const configured = suite();
  configured.cases = [{ expect: { body: { forbiddenPatterns: ["token-[0-9]+"] } } }];
  const source = {
    id: "redaction",
    status: "error",
    expectation: configured.cases[0].expect,
    evidence: { candidates: [] },
    assertions: [{ id: "body.forbidden_patterns", status: "fail", code: "forbidden_pattern_matched", expected: ["token-[0-9]+"], actual: { snippet: "token-123 and token-456" }, evidenceRefs: [] }],
    primaryError: { class: "transport_error", code: "synthetic", message: `token-789 ${ACTOR} synthetic-credential` },
    secondaryErrors: [],
  };
  const first = aliasCaseRecord(source, configured);
  const second = aliasCaseRecord(source, configured);
  assert.deepEqual(second, first);
  const serialized = JSON.stringify(first);
  assert.doesNotMatch(serialized, /token-[0-9]+|@eval\.test|synthetic-credential/);
  assert.match(serialized, /\[REDACTED:0\]/);
});

test("invalid JSON data degrades to a safe reporting diagnostic", () => {
  const cyclic = {};
  cyclic.self = cyclic;
  const aliased = aliasCaseRecord({
    id: "cyclic", status: "error", expectation: {}, evidence: cyclic, assertions: [],
    primaryError: { class: "grader_error", code: "grader_threw", message: "Synthetic" }, secondaryErrors: [],
  }, suite());
  assert.deepEqual(aliased.evidence, { unavailable: "serialization_error" });
  assert.ok(aliased.secondaryErrors.some((error) => error.stage === "reporting" && error.code === "serialization_failed"));
  assert.doesNotThrow(() => JSON.stringify(aliased));
});

test("real loaded suites omit regex caches, tokenize resolved env values, and alias unknown observed mailboxes", async () => {
  const fixtureRoot = await root();
  await mkdir(path.join(fixtureRoot, "cases"));
  await writeFile(path.join(fixtureRoot, "suite.yaml"), `
version: 1
name: synthetic-env-suite
target: { email: "\${E2A_EVAL_TARGET}" }
actor: { email: "\${E2A_EVAL_ACTOR}" }
transport:
  adapter: e2a
  api_key: "\${E2A_EVAL_API_KEY}"
  base_url: https://api.example.test
  allowed_envelope_recipients: ["\${E2A_EVAL_ACTOR}", "\${E2A_EVAL_TARGET}"]
cases: [cases/env.yaml]
`);
  await writeFile(path.join(fixtureRoot, "cases/env.yaml"), `
id: env-case
send: { subject: Synthetic, text: Synthetic }
expect:
  action: { kind: none, count: 0 }
  body:
    required_facts: ["\${E2A_EVAL_FACT}"]
    forbidden_patterns: ["token-[0-9]+"]
`);
  const loaded = await loadSuite(path.join(fixtureRoot, "suite.yaml"), { environment: {
    E2A_EVAL_TARGET: TARGET,
    E2A_EVAL_ACTOR: ACTOR,
    E2A_EVAL_API_KEY: "synthetic-credential",
    E2A_EVAL_FACT: "synthetic-private-fact",
  } });
  const record = aliasCaseRecord({
    id: "env-case", status: "fail", expectation: loaded.cases[0].expect,
    evidence: { version: 1, capabilities: ["message_action"], candidates: [{ to: ["outside@example.com"], mime: { text: "synthetic-private-fact token-123" } }] },
    assertions: [{ id: "recipients.to", status: "fail", code: "unexpected_recipient", expected: [], actual: { unexpected: ["outside@example.com"], snippet: "token-123" }, evidenceRefs: [] }],
    primaryError: null, secondaryErrors: [],
  }, loaded);
  const serialized = JSON.stringify(record);
  assert.notDeepEqual(record.expectation, { unavailable: "serialization_error" });
  assert.match(serialized, /\[ENV:E2A_EVAL_FACT\]|\[REDACTED:0\]/);
  assert.match(serialized, /observed:1/);
  assert.doesNotMatch(serialized, /synthetic-private-fact|outside@example\.com/);
});

test("Markdown output is stable and separates failed and errored assertions", () => {
  const summary = {
    runId: RUN_ID,
    status: "fail",
    suite: { name: "synthetic-suite", version: 1, digest: "a".repeat(64) },
    counts: { total: 2, passed: 0, failed: 1, errors: 1 },
    cases: [
      { id: "failed|case", status: "fail", assertions: [{ id: "recipients.to", status: "fail", code: "missing_recipient" }], primaryError: { code: "assertions_failed" }, secondaryErrors: [] },
      { id: "error-case", status: "error", assertions: [{ id: "body.required_facts", status: "error", code: "missing_plain_text_evidence" }], primaryError: { code: "grader_threw" }, secondaryErrors: [] },
    ],
  };
  const first = renderMarkdown(summary);
  assert.equal(renderMarkdown(summary), first);
  assert.match(first, /\| failed\\\|case \| fail \| assertions_failed \|/);
  assert.match(first, /## Failed assertions/);
  assert.match(first, /## Assertion errors/);
});

test("pass and mixed Markdown remain byte-stable against synthetic goldens", async () => {
  const passSummary = JSON.parse(await readFile(new URL("../testdata/reports/pass/summary.json", import.meta.url), "utf8"));
  const mixedSummary = JSON.parse(await readFile(new URL("../testdata/reports/mixed/summary.json", import.meta.url), "utf8"));
  assert.equal(renderMarkdown(passSummary), await readFile(new URL("../testdata/reports/pass/report.md", import.meta.url), "utf8"));
  assert.equal(renderMarkdown(mixedSummary), await readFile(new URL("../testdata/reports/mixed/report.md", import.meta.url), "utf8"));
});

test("writer output matches every normalized pass and mixed golden artifact", async () => {
  for (const name of ["pass", "mixed"]) {
    const outputRoot = await root();
    const writer = await createArtifactWriter({ outputRoot, runId: RUN_ID });
    const goldenCases = await readFile(new URL(`../testdata/reports/${name}/cases.jsonl`, import.meta.url), "utf8");
    const records = goldenCases.trim().split("\n").map(JSON.parse);
    const summary = JSON.parse(await readFile(new URL(`../testdata/reports/${name}/summary.json`, import.meta.url), "utf8"));
    for (let index = 0; index < records.length; index += 1) {
      await writer.appendCase(records[index], { ...summary, cases: records.slice(0, index + 1) });
    }
    await writer.finalize(summary);
    assert.equal(await readFile(writer.files.cases, "utf8"), goldenCases);
    assert.deepEqual(JSON.parse(await readFile(writer.files.summary, "utf8")), summary);
    assert.equal(await readFile(writer.files.report, "utf8"), await readFile(new URL(`../testdata/reports/${name}/report.md`, import.meta.url), "utf8"));
  }
});
