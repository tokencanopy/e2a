import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { EvalError } from "../lib/errors.mjs";
import { installRuntime, runtimePaths } from "../../setup.mjs";

test("EvalError serializes only the stable public fields", () => {
  const error = new EvalError("configuration_error", "missing_environment", "Missing E2A_EVAL_API_KEY", {
    environmentName: "E2A_EVAL_API_KEY",
  });
  assert.deepEqual(error.toJSON(), {
    class: "configuration_error",
    code: "missing_environment",
    message: "Missing E2A_EVAL_API_KEY",
    details: { environmentName: "E2A_EVAL_API_KEY" },
  });
});

test("installRuntime copies the pinned source then invokes npm ci in place", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-"));
  const sourceRoot = path.join(root, "source");
  const suiteRoot = path.join(root, "suite");
  await mkdir(sourceRoot);
  await mkdir(suiteRoot);
  await writeFile(path.join(sourceRoot, "package.json"), '{"private":true}\n');
  await writeFile(path.join(sourceRoot, "package-lock.json"), '{"lockfileVersion":3}\n');
  const calls = [];
  const result = await installRuntime({
    suiteRoot,
    sourceRoot,
    runNpm: async (args, options) => calls.push({ args, options }),
  });
  assert.deepEqual(calls[0].args, ["ci", "--omit=dev", "--ignore-scripts"]);
  assert.equal(calls[0].options.cwd, runtimePaths(suiteRoot).root);
  assert.match(await readFile(result.packageFile, "utf8"), /private/);
});

test("installRuntime refuses a symlinked .eval-runtime", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-"));
  await mkdir(path.join(root, "suite"));
  await mkdir(path.join(root, "outside"));
  await symlink(path.join(root, "outside"), path.join(root, "suite", ".eval-runtime"));
  await assert.rejects(
    installRuntime({ suiteRoot: path.join(root, "suite"), sourceRoot: path.join(root, "source") }),
    /Refusing to follow symlink/,
  );
});

test("installRuntime refuses a symlinked runtime package file without modifying its target", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-"));
  const sourceRoot = path.join(root, "source");
  const suiteRoot = path.join(root, "suite");
  const outsideFile = path.join(root, "outside-package.json");
  await mkdir(sourceRoot);
  await mkdir(path.join(suiteRoot, ".eval-runtime"), { recursive: true });
  await writeFile(path.join(sourceRoot, "package.json"), '{"private":true}\n');
  await writeFile(path.join(sourceRoot, "package-lock.json"), '{"lockfileVersion":3}\n');
  await writeFile(outsideFile, "outside package contents\n");
  await symlink(outsideFile, path.join(suiteRoot, ".eval-runtime", "package.json"));

  await assert.rejects(
    installRuntime({ suiteRoot, sourceRoot, runNpm: async () => {} }),
    /Refusing to follow symlink/,
  );
  assert.equal(await readFile(outsideFile, "utf8"), "outside package contents\n");
});

test("installRuntime refuses a symlinked runtime library without modifying its target", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-"));
  const sourceRoot = path.join(root, "source");
  const suiteRoot = path.join(root, "suite");
  const outsideLibrary = path.join(root, "outside-library");
  await mkdir(path.join(sourceRoot, "lib"), { recursive: true });
  await mkdir(path.join(suiteRoot, ".eval-runtime"), { recursive: true });
  await mkdir(outsideLibrary);
  await writeFile(path.join(sourceRoot, "package.json"), '{"private":true}\n');
  await writeFile(path.join(sourceRoot, "package-lock.json"), '{"lockfileVersion":3}\n');
  await writeFile(path.join(sourceRoot, "lib", "errors.mjs"), "export {};\n");
  await writeFile(path.join(outsideLibrary, "sentinel.txt"), "outside library contents\n");
  await symlink(outsideLibrary, path.join(suiteRoot, ".eval-runtime", "lib"));

  await assert.rejects(
    installRuntime({ suiteRoot, sourceRoot, runNpm: async () => {} }),
    /Refusing to follow symlink/,
  );
  assert.equal(await readFile(path.join(outsideLibrary, "sentinel.txt"), "utf8"), "outside library contents\n");
});
