import assert from "node:assert/strict";
import { chmod, mkdtemp, mkdir, readFile, rename, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { main as launch } from "../../launcher.mjs";

function shell(command, args) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (chunk) => { stdout += chunk; });
    child.stderr.on("data", (chunk) => { stderr += chunk; });
    child.once("error", reject);
    child.once("exit", (code) => resolve({ code, stdout, stderr }));
  });
}

async function runtimeFixture() {
  const root = await mkdtemp(path.join(tmpdir(), "email-evals-launcher-"));
  const runtime = path.join(root, ".eval-runtime");
  const suite = path.join(root, "suite.yaml");
  const marker = path.join(root, "executed.txt");
  await mkdir(runtime);
  await writeFile(suite, "version: 1\n");
  await writeFile(path.join(runtime, "cli.mjs"), `import { writeFile } from "node:fs/promises"; await writeFile(process.env.E2A_EVALS_SENTINEL, "safe");`);
  await chmod(path.join(runtime, "cli.mjs"), 0o700);
  return { root, runtime, suite, marker };
}

test("launcher rejects complete invalid runtime grammar before any suite runtime executes", async () => {
  const fixture = await runtimeFixture();
  const launcher = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../email-evals.sh");
  for (const args of [
    ["validate", "--suite", fixture.suite, "--suite", fixture.suite],
    ["validate", "--suite", fixture.suite, "--retain-raw"],
    ["validate", "--suite", fixture.suite, "--output", "results"],
    ["validate", "--suite", fixture.suite, "extra"],
  ]) {
    const result = await shell("bash", [launcher, ...args]);
    assert.equal(result.code, 2, args.join(" "));
    await assert.rejects(readFile(fixture.marker, "utf8"), { code: "ENOENT" });
  }
});

test("launcher help is dependency-free and hostile suite paths never reach native diagnostics", async () => {
  const launcher = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../email-evals.sh");
  for (const args of [["--help"], ["help"]]) {
    const result = await shell("bash", [launcher, ...args]);
    assert.equal(result.code, 0);
    assert.match(result.stdout, /email-evals validate --suite/);
  }
  const hostile = "/private/tmp/e2a-launcher-hostile\naddress@eval.test";
  const result = await shell("bash", [launcher, "validate", "--suite", hostile]);
  assert.equal(result.code, 2);
  assert.equal(result.stderr, "email-evals: runtime unavailable\n");
});

test("launcher rejects suite/runtime/CLI symlinks and detects a post-check runtime replacement", async () => {
  const fixture = await runtimeFixture();
  const environment = { E2A_EVALS_SENTINEL: fixture.marker };
  const suiteLink = path.join(fixture.root, "suite-link.yaml");
  await symlink(fixture.suite, suiteLink);
  assert.equal(await launch(["validate", "--suite", suiteLink], { cwd: fixture.root, environment }), 2);

  const runtimeLink = path.join(fixture.root, "runtime-link");
  await rename(fixture.runtime, runtimeLink);
  await symlink(runtimeLink, fixture.runtime);
  assert.equal(await launch(["validate", "--suite", fixture.suite], { cwd: fixture.root, environment }), 2);
  await rename(fixture.runtime, path.join(fixture.root, "runtime-symlink"));
  await rename(runtimeLink, fixture.runtime);

  const cli = path.join(fixture.runtime, "cli.mjs");
  const cliSource = await readFile(cli, "utf8");
  await rename(cli, path.join(fixture.runtime, "cli-source.mjs"));
  await symlink(path.join(fixture.runtime, "cli-source.mjs"), cli);
  assert.equal(await launch(["validate", "--suite", fixture.suite], { cwd: fixture.root, environment }), 2);
  await rename(cli, path.join(fixture.runtime, "cli-symlink"));
  await writeFile(cli, cliSource);

  assert.equal(await launch(["validate", "--suite", fixture.suite], {
    cwd: fixture.root,
    environment,
    beforeExec: async () => {
      await rename(fixture.runtime, path.join(fixture.root, "runtime-pinned"));
      await mkdir(fixture.runtime);
      await writeFile(path.join(fixture.runtime, "cli.mjs"), `import { writeFile } from "node:fs/promises"; await writeFile(process.env.E2A_EVALS_SENTINEL, "malicious");`);
    },
  }), 2);
  await assert.rejects(readFile(fixture.marker, "utf8"), { code: "ENOENT" });
});
