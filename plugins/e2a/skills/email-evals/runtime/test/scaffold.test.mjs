import assert from "node:assert/strict";
import { mkdtemp, readFile, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { scaffoldSuite } from "../../scaffold.mjs";

const validOptions = {
  suiteName: "fictional-support-smoke",
  targetEnv: "E2A_EVAL_TARGET",
  actorEnv: "E2A_EVAL_ACTOR",
  apiKeyEnv: "E2A_EVAL_API_KEY",
};

const expectedFiles = [
  ".gitignore",
  "README.md",
  "cases/happy-path.yaml",
  "cases/missing-information.yaml",
  "cases/unsafe-request.yaml",
  "fixtures/README.md",
  "results/.gitignore",
  "suite.yaml",
];

async function createRoot() {
  return mkdtemp(path.join(tmpdir(), "email-evals-scaffold-"));
}

test("scaffold creates three synthetic cases and preserves existing files", async () => {
  const root = await createRoot();
  const first = await scaffoldSuite({ root, ...validOptions });
  assert.deepEqual(first.created.sort(), expectedFiles);
  assert.deepEqual(first.preserved, []);

  const suite = await readFile(path.join(root, "suite.yaml"), "utf8");
  assert.match(suite, /api_key: \$\{E2A_EVAL_API_KEY\}/);
  assert.doesNotMatch(suite, /agents\.e2a\.dev|@tokencanopy\.com/);

  await writeFile(path.join(root, "cases/happy-path.yaml"), "owner edit\n");
  const second = await scaffoldSuite({
    root,
    suiteName: "ignored",
    targetEnv: "X",
    actorEnv: "Y",
    apiKeyEnv: "Z",
  });
  assert.deepEqual(second.created, []);
  assert.deepEqual(second.preserved.sort(), expectedFiles);
  assert.equal(await readFile(path.join(root, "cases/happy-path.yaml"), "utf8"), "owner edit\n");
});

test("scaffold rejects invalid suite and environment token formats before writing", async () => {
  for (const [field, value] of [
    ["suiteName", "Fictional Support"],
    ["targetEnv", "e2a_eval_target"],
    ["actorEnv", "E2A-EVAL-ACTOR"],
    ["apiKeyEnv", "1E2A_EVAL_KEY"],
  ]) {
    const root = await createRoot();
    await assert.rejects(
      scaffoldSuite({ root, ...validOptions, [field]: value }),
      new RegExp(field),
    );
  }
});

test("scaffold emits the fixed public-safe starter contract", async () => {
  const root = await createRoot();
  await scaffoldSuite({ root, ...validOptions });

  assert.equal(await readFile(path.join(root, ".gitignore"), "utf8"), ".eval-runtime/\n");
  assert.equal(await readFile(path.join(root, "results/.gitignore"), "utf8"), "*\n!.gitignore\n");

  const cases = await Promise.all([
    "happy-path.yaml",
    "missing-information.yaml",
    "unsafe-request.yaml",
  ].map((file) => readFile(path.join(root, "cases", file), "utf8")));
  const source = cases.join("\n");
  assert.match(source, /ord_example_123/);
  assert.doesNotMatch(source, /[\w.+-]+@[\w.-]+/);
  assert.match(cases[0], /kind: reply\n    count: 1/);
  assert.match(cases[1], /kind: reply\n    count: 1/);
  assert.match(cases[2], /kind: none/);
  for (const fixture of cases) {
    assert.match(fixture, /cc:\n\s+exactly: \[\]/);
    assert.match(fixture, /bcc:\n\s+exactly: \[\]/);
  }
});

test("concurrent scaffolds exclusively create or preserve every starter file", async () => {
  const root = await createRoot();
  const [first, second] = await Promise.all([
    scaffoldSuite({ root, ...validOptions }),
    scaffoldSuite({ root, ...validOptions }),
  ]);
  assert.equal(first.created.length + second.created.length, expectedFiles.length);
  assert.equal(first.preserved.length + second.preserved.length, expectedFiles.length);
});
