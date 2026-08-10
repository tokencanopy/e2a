import assert from "node:assert/strict";
import { link, mkdtemp, mkdir, open, readFile, readdir, realpath, rename, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { scaffoldSuite } from "../../scaffold.mjs";

const validOptions = {
  suiteName: "fictional-support-smoke",
  targetEnv: "E2A_EVAL_TARGET",
  actorEnv: "E2A_EVAL_ACTOR",
};

const expectedFiles = [
  "README.md",
  "cases/happy-path.yaml",
  "cases/missing-information.yaml",
  "cases/unsafe-request.yaml",
  "fixtures/README.md",
  "results/.gitignore",
  "suite.yaml",
];

async function createRoot() {
  return realpath(await mkdtemp(path.join(tmpdir(), "email-evals-scaffold-")));
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

  await assert.rejects(readFile(path.join(root, ".gitignore"), "utf8"), (error) => error.code === "ENOENT");
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

test("scaffold rejects symlinked roots and generated parents without writing outside the suite", async () => {
  const root = await createRoot();
  const outsideCases = path.join(root, "outside-cases");
  await mkdir(outsideCases);
  await symlink(outsideCases, path.join(root, "cases"));

  await assert.rejects(scaffoldSuite({ root, ...validOptions }), /symlink/i);
  assert.deepEqual(await readdir(outsideCases), []);

  const outsideRoot = path.join(root, "outside-root");
  const symlinkedRoot = path.join(root, "symlinked-root");
  await mkdir(outsideRoot);
  await symlink(outsideRoot, symlinkedRoot);
  await assert.rejects(scaffoldSuite({ root: symlinkedRoot, ...validOptions }), /symlink/i);
  assert.deepEqual(await readdir(outsideRoot), []);

  const requestedParent = path.join(root, "requested-parent");
  const outsideAncestor = path.join(root, "outside-ancestor");
  await mkdir(requestedParent);
  await mkdir(outsideAncestor);
  await symlink(outsideAncestor, path.join(requestedParent, "link"));
  await assert.rejects(
    scaffoldSuite({ root: path.join(requestedParent, "link", "evals", "email"), ...validOptions }),
    /symlink/i,
  );
  assert.deepEqual(await readdir(outsideAncestor), []);
});

test("scaffold removes a file it exclusively created when writing it fails", async () => {
  const root = await createRoot();
  const openFile = async (...args) => {
    const handle = await open(...args);
    return {
      close: () => handle.close(),
      stat: () => handle.stat(),
      writeFile: async () => {
        await writeFile(path.join(root, "README.md"), "later user replacement\n", { flag: "wx" });
        throw new Error("simulated write failure");
      },
    };
  };

  await assert.rejects(
    scaffoldSuite({ root, ...validOptions }, { openFile }),
    /simulated write failure/,
  );
  assert.equal(await readFile(path.join(root, "README.md"), "utf8"), "later user replacement\n");
});

test("scaffold does not publish through a parent swapped before the no-clobber link", async () => {
  const root = await createRoot();
  const movedRoot = `${root}-moved`;
  const outside = `${root}-outside`;
  await mkdir(outside);

  const linkFile = async (temporary, destination) => {
    await rename(root, movedRoot);
    await symlink(outside, root);
    return link(temporary, destination);
  };

  await assert.rejects(scaffoldSuite({ root, ...validOptions }, { linkFile }), /ENOENT/);
  assert.deepEqual(await readdir(outside), []);
});

test("scaffold rejects a root swapped after README before creating descendant parents", async () => {
  const root = await createRoot();
  const movedRoot = `${root}-moved-after-readme`;
  const outside = `${root}-outside-after-readme`;
  await mkdir(outside);

  const linkFile = async (temporary, destination) => {
    const result = await link(temporary, destination);
    if (path.basename(destination) === "README.md") {
      await rename(root, movedRoot);
      await symlink(outside, root);
    }
    return result;
  };

  await assert.rejects(scaffoldSuite({ root, ...validOptions }, { linkFile }), /symlink|outside/i);
  assert.deepEqual(await readdir(outside), []);
});
