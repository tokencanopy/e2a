import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, rename, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { loadSuite } from "../lib/contract.mjs";
import {
  aliasCaseRecord,
  CASES_ARTIFACT_LIMITS,
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

test("artifact writer accepts the exact cumulative byte limit and rejects the next line", async () => {
  const writer = await createArtifactWriter({ outputRoot: await root(), runId: RUN_ID });
  const lineCount = CASES_ARTIFACT_LIMITS.totalBytes / CASES_ARTIFACT_LIMITS.lineBytes;
  for (let index = 0; index < lineCount; index += 1) {
    const record = { id: `exact-${index}`, padding: "" };
    const emptyBytes = Buffer.byteLength(`${JSON.stringify(record)}\n`, "utf8");
    record.padding = "X".repeat(CASES_ARTIFACT_LIMITS.lineBytes - emptyBytes);
    assert.equal(Buffer.byteLength(`${JSON.stringify(record)}\n`, "utf8"), CASES_ARTIFACT_LIMITS.lineBytes);
    await writer.appendCase(record, { status: "incomplete", cases: [] });
  }
  assert.equal((await stat(writer.files.cases)).size, 16_777_216);
  await assert.rejects(
    writer.appendCase({ id: "overflow" }, { status: "incomplete", cases: [] }),
    (error) => error.code === "cases_artifact_limit" && error.lineDurable === false,
  );
  await writer.close();
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

test("aliasing covers nested normalized address fields and mailbox-like display text", () => {
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
  assert.equal(record.expectation.sender.displayName, "actor");
  assert.equal(record.expectation.body.requiredFacts[0], "actor");
  assert.deepEqual(record.expectation.recipients.envelope.exactly, ["actor", "probe:1"]);
  assert.equal(record.evidence.candidates[0].from, "target");
  assert.equal(record.evidence.candidates[0].mime.text, "actor");
  assert.equal(Object.hasOwn(record.evidence.candidates[0], "rawMime"), false);
  assert.equal(Object.hasOwn(record.evidence.candidates[0], "attachmentBytes"), false);
  assert.equal(record.assertions[0].actual.addresses[0], "actor");
  assert.equal(record.assertions[0].actual.movements[0].address, "probe:1");
});

test("mailbox scanning follows parser semantics without rewriting address prefixes", () => {
  const record = aliasCaseRecord({
    id: "mailbox-boundaries",
    status: "error",
    expectation: {
      body: { requiredFacts: ["Contact a@b, u@example.c, a@[127.0.0.1], ü@b, and actor@eval.test-extra"] },
    },
    evidence: { version: 1, capabilities: [], candidates: [{ from: "Short <a@b>" }] },
    assertions: [],
    primaryError: {
      class: "transport_error",
      code: "poll_failed",
      origin: "adapter",
      message: "actor@eval.test-extra\r\nforged-header: value",
    },
    secondaryErrors: [],
  }, suite());
  const artifact = JSON.stringify(record);
  assert.doesNotMatch(artifact, /a@b|u@example\.c|a@\[127\.0\.0\.1\]|ü@b|actor@eval\.test-extra|actor-extra/);
  assert.match(record.expectation.body.requiredFacts[0], /observed:1|observed:2|observed:3/);
  assert.match(record.evidence.candidates[0].from.address, /^observed:\d+$/);
  assert.equal(record.evidence.candidates[0].from.displayName, "Short");
  assert.doesNotMatch(record.primaryError.message, /[\r\n]/);
});

test("mailbox scanning aliases complete RFC atext spans including apostrophes", () => {
  const configured = suite();
  configured.actor.email = "o'reilly@b";
  configured.transport.allowedEnvelopeRecipients = [
    configured.target.email, configured.actor.email, "alpha@eval.test", "zeta@eval.test",
  ];
  const atext = "a!#$%&'*+-/=?^_`{|}~@b";
  const text = `Known o'reilly@b; plus plus+tag@x-b; atext ${atext}; comments a(comment)@example.com and a@(comment)example.com; parenthesized (inside@b); Unicode ü@b; display Élodie <display@x-b>; distinct xo'reilly@b; adjacent left@b/right@c`;
  const malformed = ["\"unterminated@example.com", "prefix(unclosed@example.com", "[unclosed@example.com"];
  const record = aliasCaseRecord({
    id: "atext-mailboxes",
    status: "error",
    expectation: { body: { requiredFacts: [text, ...malformed] } },
    evidence: { version: 1, capabilities: [], candidates: [{ mime: { text } }] },
    assertions: [],
    primaryError: {
      class: "transport_error", code: "poll_failed", origin: "adapter", message: text,
    },
    secondaryErrors: [],
  }, configured);

  const artifact = JSON.stringify(record);
  for (const mailbox of [
    "o'reilly@b", "plus+tag@x-b", atext, "a(comment)@example.com", "a@(comment)example.com",
    "inside@b", "unterminated@example.com", "unclosed@example.com", "ü@b", "display@x-b",
    "xo'reilly@b", "left@b", "right@c",
  ]) {
    assert.doesNotMatch(artifact, new RegExp(mailbox.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
  }
  assert.match(record.expectation.body.requiredFacts[0], /Known actor/);
  assert.doesNotMatch(record.expectation.body.requiredFacts[0], /xactor/);
  assert.match(record.expectation.body.requiredFacts[0], /Élodie <observed:\d+>/);
  assert.match(record.expectation.body.requiredFacts[0], /comments observed:\d+ and observed:\d+/);
  assert.match(record.expectation.body.requiredFacts[0], /adjacent \[REDACTED:address\]/);
  assert.ok(record.expectation.body.requiredFacts.slice(1).every(
    (value) => value === "[REDACTED:address]" || /^observed:\d+$/.test(value),
  ));
  assert.equal(record.evidence.candidates[0].mime.text, record.expectation.body.requiredFacts[0]);
});

test("mailbox scanning aliases complete quoted local parts instead of generic redaction", () => {
  const configured = suite();
  configured.actor.email = '"a b"@example.com';
  configured.target.email = '"a@b"@example.com';
  configured.transport.allowedEnvelopeRecipients = [
    configured.actor.email, configured.target.email, '"a\\\"b"@example.com', '"a\\\\b"@example.com',
  ];
  const text = 'Known "a b"@example.com; at "a@b"@example.com; quote Escaped <"a\\\"b"@example.com>; slash <"a\\\\b"@example.com>';
  const record = aliasCaseRecord({
    id: "quoted-mailboxes",
    status: "error",
    expectation: { body: { requiredFacts: [text] } },
    evidence: { version: 1, capabilities: [], candidates: [{ mime: { text } }] },
    assertions: [],
    primaryError: { class: "transport_error", code: "poll_failed", origin: "adapter", message: text },
    secondaryErrors: [],
  }, configured);

  for (const value of ['"a b"@example.com', '"a@b"@example.com', '"a\\\"b"@example.com', '"a\\\\b"@example.com']) {
    assert.doesNotMatch(JSON.stringify(record), new RegExp(value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
  }
  assert.equal(record.expectation.body.requiredFacts[0], "Known actor; at target; quote Escaped <probe:1>; slash <probe:2>");
  assert.equal(record.evidence.candidates[0].mime.text, record.expectation.body.requiredFacts[0]);
  assert.equal(record.primaryError.message, record.expectation.body.requiredFacts[0]);
  assert.doesNotMatch(JSON.stringify(record), /\[REDACTED:address\]/);
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

test("persisted expectations and assertion expected values cannot retain credentials", () => {
  const configured = suite();
  const source = {
    id: "expectation-redaction",
    status: "fail",
    expectation: {
      action: { kind: "none", count: 0 },
      sender: { exactly: TARGET },
      body: { requiredFacts: ["literal synthetic-credential must not persist"] },
    },
    evidence: { candidates: [] },
    assertions: [{
      id: "body.required_facts",
      status: "fail",
      code: "missing_required_fact",
      expected: ["literal synthetic-credential must not persist", "actor", "none"],
      actual: "synthetic-credential",
      evidenceRefs: [],
    }],
    primaryError: null,
    secondaryErrors: [],
  };
  const record = aliasCaseRecord(source, configured);
  const serialized = JSON.stringify(record);
  assert.doesNotMatch(serialized, /synthetic-credential/);
  assert.equal(record.expectation.action.kind, "none");
  assert.equal(record.expectation.sender.exactly, "target");
  assert.match(record.expectation.body.requiredFacts[0], /\[REDACTED:credential\]/);
  assert.deepEqual(record.assertions[0].expected.slice(1), ["actor", "none"]);
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

test("real loaded suites omit regex caches, redact credentials, and alias unknown observed mailboxes", async () => {
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
  allowed_envelope_recipients: ["\${E2A_EVAL_ACTOR}", "\${E2A_EVAL_TARGET}"]
cases: [cases/env.yaml]
`);
  await writeFile(path.join(fixtureRoot, "cases/env.yaml"), `
id: env-case
send: { subject: Synthetic, text: Synthetic }
expect:
  action: { kind: none, count: 0 }
  body:
    required_facts: ["synthetic-required-fact"]
    forbidden_patterns: ["token-[0-9]+"]
`);
  const loaded = await loadSuite(path.join(fixtureRoot, "suite.yaml"), { environment: {
    E2A_EVAL_TARGET: TARGET,
    E2A_EVAL_ACTOR: ACTOR,
    E2A_EVAL_API_KEY: "synthetic-credential",
  } });
  const record = aliasCaseRecord({
    id: "env-case", status: "fail", expectation: loaded.cases[0].expect,
    evidence: { version: 1, capabilities: ["message_action"], candidates: [{ direction: "outbound", sentAs: "own_address", to: ["outside@example.com"], mime: { text: "synthetic-credential token-123 e2a_unknown_secret" } }] },
    assertions: [{ id: "recipients.to", status: "fail", code: "unexpected_recipient", expected: [], actual: { unexpected: ["outside@example.com"], snippet: "token-123" }, evidenceRefs: [] }],
    primaryError: null, secondaryErrors: [],
  }, loaded);
  const serialized = JSON.stringify(record);
  assert.notDeepEqual(record.expectation, { unavailable: "serialization_error" });
  assert.match(serialized, /\[ENV:E2A_EVAL_API_KEY\]|\[REDACTED:0\]/);
  assert.match(serialized, /observed:1/);
  assert.doesNotMatch(serialized, /token-123/);
  assert.equal(record.evidence.candidates[0].direction, "outbound");
  assert.equal(record.evidence.candidates[0].sentAs, "own_address");
  assert.doesNotMatch(serialized, /synthetic-credential|token-123|e2a_unknown_secret|outside@example\.com/);
});

test("credential values cannot corrupt aliases or semantic evidence tokens", async () => {
  const fixtureRoot = await root();
  await mkdir(path.join(fixtureRoot, "cases"));
  await writeFile(path.join(fixtureRoot, "suite.yaml"), `
version: 1
name: structural-token-suite
target: { email: "\${E2A_EVAL_TARGET}" }
actor: { email: "\${E2A_EVAL_ACTOR}" }
transport:
  adapter: e2a
  api_key: "\${E2A_EVAL_API_KEY}"
  allowed_envelope_recipients: ["\${E2A_EVAL_ACTOR}", "\${E2A_EVAL_TARGET}"]
cases: [cases/none.yaml]
`);
  await writeFile(path.join(fixtureRoot, "cases/none.yaml"), `
id: structural-token-case
send: { subject: Synthetic, text: Synthetic }
expect: { action: { kind: none, count: 0 } }
`);

  for (const credential of ["actor", "target", "sent", "outbound", "own_address"]) {
    const loaded = await loadSuite(path.join(fixtureRoot, "suite.yaml"), { environment: {
      E2A_EVAL_TARGET: TARGET,
      E2A_EVAL_ACTOR: ACTOR,
      E2A_EVAL_API_KEY: credential,
    } });
    const record = aliasCaseRecord({
      id: "structural-token-case",
      status: "fail",
      expectation: loaded.cases[0].expect,
      evidence: {
        version: 1,
        capabilities: ["message_action"],
        candidates: [{
          direction: "outbound",
          provenance: "target_outbound",
          sentAs: "own_address",
          from: TARGET,
          to: [ACTOR],
          lifecycle: { submission: "sent" },
          mime: { text: `secret=${credential}` },
        }],
      },
      assertions: [],
      primaryError: null,
      secondaryErrors: [],
    }, loaded);
    const candidate = record.evidence.candidates[0];
    assert.equal(candidate.from, "target");
    assert.deepEqual(candidate.to, ["actor"]);
    assert.equal(candidate.direction, "outbound");
    assert.equal(candidate.provenance, "target_outbound");
    assert.equal(candidate.sentAs, "own_address");
    assert.equal(candidate.lifecycle.submission, "sent");
    assert.match(candidate.mime.text, /^secret=\[ENV:E2A_EVAL_API_KEY(?::semantic:\d+)?\]$/);
  }
});

test("credential collisions inside structural refs are tokenized before artifact publication", async () => {
  const fixtureRoot = await root();
  await mkdir(path.join(fixtureRoot, "cases"));
  await writeFile(path.join(fixtureRoot, "suite.yaml"), `
version: 1
name: structural-ref-suite
target: { email: "\${E2A_EVAL_TARGET}" }
actor: { email: "\${E2A_EVAL_ACTOR}" }
transport:
  adapter: e2a
  api_key: "\${E2A_EVAL_API_KEY}"
  allowed_envelope_recipients: ["\${E2A_EVAL_ACTOR}", "\${E2A_EVAL_TARGET}"]
cases: [cases/none.yaml]
`);
  await writeFile(path.join(fixtureRoot, "cases/none.yaml"), `
id: structural-ref-case
send: { subject: Synthetic, text: Synthetic }
expect: { action: { kind: none, count: 0 } }
`);
  const credential = "e2a_acct_synthetic_collision";
  const loaded = await loadSuite(path.join(fixtureRoot, "suite.yaml"), { environment: {
    E2A_EVAL_TARGET: TARGET,
    E2A_EVAL_ACTOR: ACTOR,
    E2A_EVAL_API_KEY: credential,
  } });
  const record = aliasCaseRecord({
    id: "structural-ref-case", status: "pass", expectation: loaded.cases[0].expect,
    evidence: { version: 1, capabilities: [], candidates: [{
      ref: `evt_${credential}`, direction: "outbound", provenance: "target_outbound",
    }] },
    assertions: [], primaryError: null, secondaryErrors: [],
  }, loaded);
  assert.doesNotMatch(JSON.stringify(record), new RegExp(credential));
  assert.match(record.evidence.candidates[0].ref, /\[ENV:E2A_EVAL_API_KEY\]/);
});

test("the exact API key is redacted even when it has valid sent-as token syntax", () => {
  const configured = suite();
  configured.transport.apiKey = "e2a_custom";
  const record = aliasCaseRecord({
    id: "sent-as-credential-collision",
    status: "pass",
    expectation: { action: { kind: "none", count: 0 }, sender: { sentAs: "e2a_custom" } },
    evidence: { candidates: [{ sentAs: "e2a_custom" }] },
    assertions: [{
      id: "sender.sent_as", status: "pass", code: "matched",
      expected: "e2a_custom", actual: { actual: "e2a_custom" }, evidenceRefs: [],
    }],
    primaryError: null,
    secondaryErrors: [],
  }, configured);
  assert.doesNotMatch(JSON.stringify(record), /e2a_custom/);
  assert.equal(record.evidence.candidates[0].sentAs, "[REDACTED:credential]");
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
