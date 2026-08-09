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
