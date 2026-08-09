import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { parseMimeEvidence } from "../lib/mime.mjs";

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
  const tenBytes = Buffer.from("0123456789", "utf8").toString("base64");
  assert.equal((await parseMimeEvidence(tenBytes, { maxBytes: 10 })).sizeBytes, 10);
  await assert.rejects(parseMimeEvidence(tenBytes, { maxBytes: 9 }), (error) => error.errorClass === "grader_error" && error.code === "mime_too_large");
  await assert.rejects(parseMimeEvidence(await fixture("mime/attachment.eml").then((value) => value.replace(/.$/, "!"))), (error) => error.errorClass === "grader_error" && error.code === "invalid_mime_base64");
  const oversizedHeaders = Buffer.from(`X-Synthetic: ${"x".repeat(262_200)}\r\n\r\nbody`, "utf8").toString("base64");
  await assert.rejects(parseMimeEvidence(oversizedHeaders), (error) => error.errorClass === "grader_error" && error.code === "mime_parse_failed");
});

test("parseMimeEvidence keeps injected headers separate and selects only safe first thread tokens", async () => {
  const parsed = await parseMimeEvidence(await fixture("mime/header-injection.eml"));
  assert.equal(parsed.subject, "Synthetic update");
  assert.equal(parsed.inReplyTo, "first@agents.localhost");
  assert.deepEqual(parsed.references, ["root@agents.localhost", "first@agents.localhost", "second@agents.localhost"]);
  assert.doesNotMatch(JSON.stringify(parsed), /injected@agents\.localhost/);
});
