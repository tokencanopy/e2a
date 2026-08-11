import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, symlink, unlink, writeFile } from "node:fs/promises";
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

async function trustedRuntimeFixture(root) {
  const sourceRoot = path.join(root, "source");
  await mkdir(sourceRoot, { recursive: true });
  await writeFile(path.join(sourceRoot, "package.json"), '{"private":true}\n');
  await writeFile(path.join(sourceRoot, "email-evals-runtime.bundle.mjs"), "export {};\n");
  await writeFile(path.join(sourceRoot, "THIRD_PARTY_NOTICES.md"), "# Synthetic notices\n");
  return sourceRoot;
}

test("installRuntime verifies the checked-in trusted bundle without installing code", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-"));
  const sourceRoot = await trustedRuntimeFixture(root);
  const suiteRoot = path.join(root, "suite");
  await mkdir(suiteRoot);
  const result = await installRuntime({ suiteRoot, sourceRoot });
  assert.equal(result.root, sourceRoot);
  assert.match(await readFile(result.packageFile, "utf8"), /private/);
  assert.match(await readFile(result.cli, "utf8"), /export/);
});

test("installRuntime ignores a suite-local .eval-runtime", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-"));
  await mkdir(path.join(root, "suite"));
  await mkdir(path.join(root, "outside"));
  await symlink(path.join(root, "outside"), path.join(root, "suite", ".eval-runtime"));
  const sourceRoot = await trustedRuntimeFixture(root);
  const result = await installRuntime({
    suiteRoot: path.join(root, "suite"), sourceRoot,
  });
  assert.equal(result.root, sourceRoot);
});

test("installRuntime refuses a symlinked trusted bundle without modifying its target", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-"));
  const sourceRoot = path.join(root, "source");
  const suiteRoot = path.join(root, "suite");
  const outsideFile = path.join(root, "outside-bundle.mjs");
  await mkdir(sourceRoot, { recursive: true });
  await mkdir(suiteRoot);
  await writeFile(outsideFile, "outside bundle contents\n");
  await symlink(outsideFile, path.join(sourceRoot, "email-evals-runtime.bundle.mjs"));

  await assert.rejects(
    installRuntime({ suiteRoot, sourceRoot }),
    /Refusing to follow symlink/,
  );
  assert.equal(await readFile(outsideFile, "utf8"), "outside bundle contents\n");
});

test("installRuntime requires regular checked-in third-party notices", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-"));
  const sourceRoot = await trustedRuntimeFixture(root);
  const suiteRoot = path.join(root, "suite");
  const notices = path.join(sourceRoot, "THIRD_PARTY_NOTICES.md");
  await mkdir(suiteRoot);
  await unlink(notices);
  await assert.rejects(
    installRuntime({ suiteRoot, sourceRoot }),
    /Trusted runtime file is missing: THIRD_PARTY_NOTICES\.md/,
  );

  const outside = path.join(root, "outside-notices.md");
  await writeFile(outside, "# Outside notices\n");
  await symlink(outside, notices);
  await assert.rejects(
    installRuntime({ suiteRoot, sourceRoot }),
    /Refusing to follow symlink/,
  );
  assert.equal(await readFile(outside, "utf8"), "# Outside notices\n");
});
