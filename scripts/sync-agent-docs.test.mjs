import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, test } from "node:test";

import {
  AGENT_DOC_MIRRORS,
  LLMS_FULL_SOURCES,
  LLMS_FULL_TARGET,
  parseArgs,
  syncAgentDocs,
} from "./sync-agent-docs.mjs";

const roots = [];

afterEach(async () => {
  await Promise.all(roots.splice(0).map((root) => rm(root, { recursive: true })));
});

async function fixture() {
  const repoRoot = await mkdtemp(join(tmpdir(), "e2a-agent-docs-"));
  roots.push(repoRoot);
  for (const [index, [source]] of AGENT_DOC_MIRRORS.entries()) {
    const sourcePath = join(repoRoot, source);
    await mkdir(join(sourcePath, ".."), { recursive: true });
    await writeFile(sourcePath, `canonical-${index}\n`);
  }
  return repoRoot;
}

test("maps every public agent document to its canonical plugin source", () => {
  assert.deepEqual(AGENT_DOC_MIRRORS, [
    ["plugins/e2a/docs/setup.md", "web/public/setup.md"],
    ["plugins/e2a/docs/auth.md", "web/public/auth.md"],
    ["plugins/e2a/docs/sdk.md", "web/public/sdk.md"],
    ["plugins/e2a/docs/templates.md", "web/public/templates.md"],
    ["plugins/e2a/docs/llms.txt", "web/public/llms.txt"],
  ]);
});

test("sync creates byte-identical hosted mirrors and check accepts them", async () => {
  const repoRoot = await fixture();

  await syncAgentDocs({ repoRoot, check: false, log: () => {} });

  for (const [source, target] of AGENT_DOC_MIRRORS) {
    assert.deepEqual(
      await readFile(join(repoRoot, target)),
      await readFile(join(repoRoot, source)),
    );
  }
  await assert.doesNotReject(
    syncAgentDocs({ repoRoot, check: true, log: () => {} }),
  );
});

test("check reports every missing or stale hosted mirror without writing", async () => {
  const repoRoot = await fixture();
  const [, staleTarget] = AGENT_DOC_MIRRORS.find(
    ([, target]) => target === "web/public/templates.md",
  );
  await mkdir(join(repoRoot, staleTarget, ".."), { recursive: true });
  await writeFile(join(repoRoot, staleTarget), "stale\n");

  await assert.rejects(
    syncAgentDocs({ repoRoot, check: true, log: () => {} }),
    (error) => {
      assert.match(error.message, /missing hosted agent doc: web\/public\/setup\.md/);
      assert.match(error.message, /stale hosted agent doc: web\/public\/templates\.md/);
      return true;
    },
  );
  const missingState = await readFile(join(repoRoot, AGENT_DOC_MIRRORS[0][1])).then(
    () => "present",
    () => "missing",
  );
  assert.equal(missingState, "missing");
  assert.equal(await readFile(join(repoRoot, staleTarget), "utf8"), "stale\n");
});

test("sync fails clearly when a canonical source is missing", async () => {
  const repoRoot = await fixture();
  await rm(join(repoRoot, AGENT_DOC_MIRRORS[0][0]));

  await assert.rejects(
    syncAgentDocs({ repoRoot, check: false, log: () => {} }),
    /missing canonical agent doc: plugins\/e2a\/docs\/setup\.md/,
  );
});

test("sync inlines every corpus source into llms-full.txt", async () => {
  const repoRoot = await fixture();

  await syncAgentDocs({ repoRoot, check: false, log: () => {} });

  const full = await readFile(join(repoRoot, LLMS_FULL_TARGET), "utf8");
  for (const [source, url] of LLMS_FULL_SOURCES) {
    const body = await readFile(join(repoRoot, source), "utf8");
    assert.ok(full.includes(`<!-- source: ${url} -->`), `missing marker for ${url}`);
    assert.ok(full.includes(body.trim()), `missing body of ${source}`);
  }
  assert.match(full, /open-source email API for applications and AI agents/i);
  await assert.doesNotReject(
    syncAgentDocs({ repoRoot, check: true, log: () => {} }),
  );
});

test("agent answer surfaces describe domain evidence without identity overclaims", async () => {
  for (const path of [
    "../plugins/e2a/docs/auth.md",
    "../plugins/e2a/docs/llms.txt",
    "../plugins/e2a/docs/setup.md",
    "../web/public/auth.md",
    "../web/public/llms.txt",
    "../web/public/llms-full.txt",
  ]) {
    const body = await readFile(new URL(path, import.meta.url), "utf8");
    assert.doesNotMatch(
      body,
      /authenticated email address|verified email inbox|sender verification|end-to-end-verified email address|identity claim e2a stands behind|a verified address|provides the email identity|inbound authentication evidence/i,
      `${path} contains an identity overclaim`,
    );
  }
  const llms = await readFile(
    new URL("../plugins/e2a/docs/llms.txt", import.meta.url),
    "utf8",
  );
  assert.match(llms, /structured inbound domain evidence/i);
  assert.match(llms, /not a person, mailbox, or message content/i);
  assert.match(llms, /transactional application email and agent-owned inboxes/i);
  assert.match(llms, /does not require an AI agent or agent framework/i);
  assert.doesNotMatch(llms, /paid Pro and Scale tiers/i);
});

test("check reports a stale llms-full.txt without rewriting it", async () => {
  const repoRoot = await fixture();
  await syncAgentDocs({ repoRoot, check: false, log: () => {} });
  await writeFile(join(repoRoot, LLMS_FULL_TARGET), "stale\n");

  await assert.rejects(
    syncAgentDocs({ repoRoot, check: true, log: () => {} }),
    /stale generated corpus: web\/public\/llms-full\.txt/,
  );
  assert.equal(await readFile(join(repoRoot, LLMS_FULL_TARGET), "utf8"), "stale\n");
});

test("parseArgs accepts check mode and rejects unknown options", () => {
  assert.deepEqual(parseArgs([]), { check: false });
  assert.deepEqual(parseArgs(["--check"]), { check: true });
  assert.throws(() => parseArgs(["--wat"]), /unknown option: --wat/);
  assert.throws(
    () => parseArgs(["--check", "extra"]),
    /usage: node scripts\/sync-agent-docs\.mjs \[--check\]/,
  );
});
