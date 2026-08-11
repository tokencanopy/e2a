import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { normalizeMessageIdToken, parseMimeEvidence } from "../lib/mime.mjs";

const directory = path.dirname(fileURLToPath(import.meta.url));
const fixture = async (name) => (await readFile(path.join(directory, "..", "testdata", name))).toString("base64");

test("parseMimeEvidence unfolds thread headers and hashes decoded attachment bytes", async () => {
  const parsed = await parseMimeEvidence(await fixture("mime/attachment.eml"), { maxBytes: 2_000_000 });
  assert.equal(parsed.messageId, "reply@agents.localhost");
  assert.equal(parsed.inReplyTo, "original@agents.localhost");
  assert.deepEqual(parsed.references, ["root@agents.localhost", "original@agents.localhost"]);
  assert.deepEqual(parsed.attachments, [{
    filename: "refund-policy.txt",
    contentType: "text/plain",
    disposition: "attachment",
    sizeBytes: 18,
    sha256: "d2b3ec0450082cc4693ad0a0c490c6c8581a03ed1c2552e9d5c4a09611a72300",
  }]);
  assert.doesNotMatch(JSON.stringify(parsed), /refunds in 30 days/);
});

test("parseMimeEvidence limits canonical base64 before decoding and maps parse inputs safely", async () => {
  await assert.rejects(parseMimeEvidence("%%%="), (error) => error.errorClass === "grader_error" && error.code === "invalid_mime_base64");
  const minimalBytes = Buffer.from("Message-ID: <minimal@agents.localhost>\r\nFrom: sender@eval.test\r\n\r\nbody", "utf8");
  const minimal = minimalBytes.toString("base64");
  assert.equal((await parseMimeEvidence(minimal, { maxBytes: minimalBytes.length })).sizeBytes, minimalBytes.length);
  await assert.rejects(parseMimeEvidence(minimal, { maxBytes: minimalBytes.length - 1 }), (error) => error.errorClass === "grader_error" && error.code === "mime_too_large");
  await assert.rejects(parseMimeEvidence(await fixture("mime/attachment.eml").then((value) => value.replace(/.$/, "!"))), (error) => error.errorClass === "grader_error" && error.code === "invalid_mime_base64");
  const oversizedHeaders = Buffer.from(`X-Synthetic: ${"x".repeat(262_200)}\r\n\r\nbody`, "utf8").toString("base64");
  await assert.rejects(parseMimeEvidence(oversizedHeaders), (error) => error.errorClass === "grader_error" && error.code === "mime_parse_failed");
});

test("sender MIME may defer Message-ID to an authoritative provider source", async () => {
  const raw = Buffer.from("From: sender@eval.test\r\nSubject: Synthetic\r\n\r\nbody", "utf8").toString("base64");
  await assert.rejects(
    parseMimeEvidence(raw),
    (error) => error.errorClass === "grader_error" && error.code === "missing_mime_header",
  );
  assert.equal((await parseMimeEvidence(raw, { requireMessageId: false })).messageId, null);
});

test("parseMimeEvidence rejects injected or duplicated singleton headers", async () => {
  await assert.rejects(
    parseMimeEvidence(await fixture("mime/header-injection.eml")),
    (error) => error.errorClass === "grader_error" && ["malformed_mime_header", "duplicate_mime_header"].includes(error.code),
  );
  for (const header of ["Message-ID", "In-Reply-To", "References", "From", "Reply-To", "Subject", "To", "Cc"]) {
    const raw = [
      "Message-ID: <message@agents.localhost>",
      "From: sender@eval.test",
      `${header}: ${header === "From" || header === "Reply-To" ? "other@eval.test" : header === "Subject" ? "one" : "<other@agents.localhost>"}`,
      `${header}: ${header === "From" || header === "Reply-To" ? "third@eval.test" : header === "Subject" ? "two" : "<third@agents.localhost>"}`,
      "", "body",
    ].join("\r\n");
    await assert.rejects(
      parseMimeEvidence(Buffer.from(raw).toString("base64")),
      (error) => error.errorClass === "grader_error" && error.code === "duplicate_mime_header",
      header,
    );
  }
});

test("To and Cc each permit one field containing multiple mailboxes", async () => {
  const raw = [
    "Message-ID: <message@agents.localhost>",
    "From: sender@eval.test",
    "To: First <first@eval.test>, second@eval.test",
    "Cc: Third <third@eval.test>, fourth@eval.test",
    "", "body",
  ].join("\r\n");
  const parsed = await parseMimeEvidence(Buffer.from(raw).toString("base64"));
  assert.deepEqual(parsed.to, ["first@eval.test", "second@eval.test"]);
  assert.deepEqual(parsed.cc, ["third@eval.test", "fourth@eval.test"]);
});

test("parseMimeEvidence rejects fused and punctuation-prefixed thread tokens without partial acceptance", async () => {
  const raw = [
    "Message-ID: junk<message@agents.localhost>",
    "In-Reply-To: prefix<original@agents.localhost>",
    "References: junk<original@agents.localhost> ,<punctuation@agents.localhost> <valid@agents.localhost>",
    "Content-Type: text/plain; charset=utf-8", "", "Synthetic body.", "",
  ].join("\r\n");
  await assert.rejects(
    parseMimeEvidence(Buffer.from(raw, "utf8").toString("base64")),
    (error) => error.errorClass === "grader_error" && error.code === "malformed_mime_header",
  );
});

test("parseMimeEvidence requires complete RFC-shaped Message-ID tokens", async () => {
  for (const token of ["<foo@>", "<@bar>", "<foo,bar>", "<foo..bar@example.test>"]) {
    for (const header of ["Message-ID", "In-Reply-To", "References"]) {
      const raw = [
        `Message-ID: ${header === "Message-ID" ? token : "<message@agents.localhost>"}`,
        "From: sender@eval.test",
        ...(header === "Message-ID" ? [] : [`${header}: ${token}`]),
        "", "body",
      ].join("\r\n");
      await assert.rejects(
        parseMimeEvidence(Buffer.from(raw).toString("base64")),
        (error) => error.errorClass === "grader_error" && error.code === "malformed_mime_header",
        `${header}: ${token}`,
      );
    }
  }
});

test("Message-ID normalization uses an exact conservative ASCII grammar", () => {
  assert.equal(normalizeMessageIdToken(" \t<valid@example.test>\t "), "valid@example.test");
  assert.equal(normalizeMessageIdToken("\u00a0<valid@example.test>\u00a0"), null);
  // Obsolete quoted id-left syntax is deliberately outside the accepted
  // subset; unsupported grammar fails closed rather than being partly parsed.
  assert.equal(normalizeMessageIdToken('<"foo bar"@example.test>'), null);
});

test("parseMimeEvidence requires complete mailbox and address-list consumption", async () => {
  for (const [header, value] of [
    ["From", "junk sender@eval.test"],
    ["From", "sender@eval.test trailing"],
    ["From", "Bad[Name] <sender@eval.test>"],
    ["From", "Bad\\Name <sender@eval.test>"],
    ["Reply-To", "sender@eval.test,, other@eval.test"],
    ["To", "junk actor@eval.test"],
    ["Cc", "actor@eval.test trailing"],
  ]) {
    const raw = [
      "Message-ID: <message@agents.localhost>",
      header === "From" ? `${header}: ${value}` : "From: sender@eval.test",
      ...(header === "From" ? [] : [`${header}: ${value}`]),
      "", "body",
    ].join("\r\n");
    await assert.rejects(
      parseMimeEvidence(Buffer.from(raw).toString("base64")),
      (error) => error.errorClass === "grader_error" && error.code === "malformed_mime_header",
      `${header}: ${value}`,
    );
  }
});

test("MIME identity headers reject non-RFC Unicode outer whitespace", async () => {
  for (const [header, value] of [
    ["Message-ID", "\u00a0<message@agents.localhost>\u00a0"],
    ["In-Reply-To", "\u00a0<original@agents.localhost>\u00a0"],
    ["References", "\u00a0<original@agents.localhost>\u00a0"],
    ["From", "\u00a0sender@eval.test\u00a0"],
    ["Reply-To", "\u00a0sender@eval.test\u00a0"],
    ["To", "\u00a0actor@eval.test\u00a0"],
  ]) {
    const raw = [
      header === "Message-ID" ? `${header}: ${value}` : "Message-ID: <message@agents.localhost>",
      header === "From" ? `${header}: ${value}` : "From: sender@eval.test",
      ...(["Message-ID", "From"].includes(header) ? [] : [`${header}: ${value}`]),
      "", "body",
    ].join("\r\n");
    await assert.rejects(
      parseMimeEvidence(Buffer.from(raw, "utf8").toString("base64")),
      (error) => error.errorClass === "grader_error" && error.code === "malformed_mime_header",
      header,
    );
  }
});
