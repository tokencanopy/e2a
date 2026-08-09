import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { chmod, mkdtemp, mkdir, readFile, rename, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { main as launch, runRuntimeNode } from "../../launcher.mjs";

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

function fakeRuntimeChild() {
  const child = new EventEmitter();
  const stdout = new EventEmitter();
  const stderr = new EventEmitter();
  let destroyed = 0;
  let unrefed = 0;
  stderr.destroy = () => { destroyed += 1; };
  stdout.destroy = () => {};
  child.stdout = stdout;
  child.stderr = stderr;
  child.unref = () => { unrefed += 1; };
  return { child, stdout, stderr, destroyed: () => destroyed, unrefed: () => unrefed };
}

function manualTimers() {
  const timers = new Set();
  return {
    pending: () => [...timers],
    set: (callback) => {
      timers.add(callback);
      return callback;
    },
    clear: (callback) => timers.delete(callback),
  };
}

function completedSummary(errorClass, id = "synthetic-case") {
  const assertion = errorClass === "assertion_failure";
  return {
    command: "COMMAND",
    summary: {
      runId: "run_20260809T120000_0123abcd",
      status: "fail",
      complete: true,
      counts: { total: 1, passed: 0, failed: assertion ? 1 : 0, errors: assertion ? 0 : 1 },
      capabilities: [],
      cases: [{ id, status: assertion ? "fail" : "error", errorClass }],
    },
  };
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

test("launcher contains runtime stderr while preserving its fixed diagnostic protocol", async () => {
  const fixture = await runtimeFixture();
  const launcher = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../email-evals.sh");
  const cli = path.join(fixture.runtime, "cli.mjs");

  await writeFile(cli, 'throw new Error("e2a_acct_synthetic /private/tmp/secret-suite.yaml");\n');
  const importFailure = await shell("bash", [launcher, "validate", "--suite", fixture.suite]);
  assert.equal(importFailure.code, 4);
  assert.equal(importFailure.stderr, "email-evals: runtime failure\n");
  assert.doesNotMatch(importFailure.stderr, /e2a_acct_|\/private\/tmp/);

  await writeFile(cli, 'process.stderr.write("email-evals: transport_error\\n"); process.exitCode = 3;\n');
  const knownDiagnostic = await shell("bash", [launcher, "validate", "--suite", fixture.suite]);
  assert.equal(knownDiagnostic.code, 3);
  assert.equal(knownDiagnostic.stderr, "email-evals: transport_error\n");

  await writeFile(cli, 'process.stderr.write("e2a_acct_synthetic".repeat(1024 * 1024)); process.exitCode = 4;\n');
  const oversized = await shell("bash", [launcher, "validate", "--suite", fixture.suite]);
  assert.equal(oversized.code, 4);
  assert.equal(oversized.stderr, "email-evals: runtime failure\n");
  assert.doesNotMatch(oversized.stderr, /e2a_acct_/);
});

test("launcher preserves every completed JSON result exit class for run and regrade", async () => {
  const fixture = await runtimeFixture();
  const launcher = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../email-evals.sh");
  const cli = path.join(fixture.runtime, "cli.mjs");
  for (const command of ["run", "regrade"]) {
    for (const [errorClass, exitCode] of [
      ["assertion_failure", 1],
      ["configuration_error", 2],
      ["capability_error", 2],
      ["transport_error", 3],
      ["target_timeout", 3],
      ["grader_error", 4],
    ]) {
      const output = completedSummary(errorClass);
      output.command = command;
      await writeFile(cli, `process.stdout.write(${JSON.stringify(`${JSON.stringify(output)}\n`)}); process.exitCode = ${exitCode};\n`);
      const args = command === "run"
        ? [command, "--suite", fixture.suite, "--json"]
        : [command, "--suite", fixture.suite, "--run", path.join(fixture.root, "run"), "--json"];
      const result = await shell("bash", [launcher, ...args]);
      assert.equal(result.code, exitCode, `${command}:${errorClass}`);
      assert.equal(result.stdout, `${JSON.stringify(output)}\n`, `${command}:${errorClass}`);
      assert.equal(result.stderr, "", `${command}:${errorClass}`);
    }
  }
});

test("launcher accepts runtime-redacted case IDs for run and regrade", async () => {
  const fixture = await runtimeFixture();
  const launcher = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../email-evals.sh");
  const cli = path.join(fixture.runtime, "cli.mjs");
  for (const command of ["run", "regrade"]) {
    const output = completedSummary("transport_error", "[ENV:E2A_CASE_ID]");
    output.command = command;
    await writeFile(cli, `process.stdout.write(${JSON.stringify(`${JSON.stringify(output)}\n`)}); process.exitCode = 3;\n`);
    const args = command === "run"
      ? [command, "--suite", fixture.suite, "--json"]
      : [command, "--suite", fixture.suite, "--run", path.join(fixture.root, "run"), "--json"];
    const result = await shell("bash", [launcher, ...args]);
    assert.equal(result.code, 3, command);
    assert.equal(result.stdout, `${JSON.stringify(output)}\n`, command);
    assert.equal(result.stderr, "", command);
  }
});

test("launcher preserves bounded human summaries only with their matching fixed diagnostic", async () => {
  const fixture = await runtimeFixture();
  const launcher = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../email-evals.sh");
  const cli = path.join(fixture.runtime, "cli.mjs");
  for (const command of ["run", "regrade"]) {
    for (const [errorClass, exitCode] of [
      ["assertion_failure", 1],
      ["configuration_error", 2],
      ["capability_error", 2],
      ["transport_error", 3],
      ["target_timeout", 3],
      ["grader_error", 4],
    ]) {
      const output = "Status: fail; 0/1 passed\nComplete: yes\nReport: run_20260809T120000_0123abcd/report.md\n";
      await writeFile(cli, `process.stdout.write(${JSON.stringify(output)}); process.stderr.write(${JSON.stringify(`email-evals: ${errorClass}\n`)}); process.exitCode = ${exitCode};\n`);
      const args = command === "run"
        ? [command, "--suite", fixture.suite]
        : [command, "--suite", fixture.suite, "--run", path.join(fixture.root, "run")];
      const result = await shell("bash", [launcher, ...args]);
      assert.equal(result.code, exitCode, `${command}:${errorClass}`);
      assert.equal(result.stdout, output, `${command}:${errorClass}`);
      assert.equal(result.stderr, `email-evals: ${errorClass}\n`, `${command}:${errorClass}`);
    }
  }

  await writeFile(cli, 'process.stdout.write("Status: fail; 0/1 passed\\nComplete: yes\\nReport: run_20260809T120000_0123abcd/report.md\\ne2a_acct_synthetic\\n"); process.stderr.write("email-evals: transport_error\\n"); process.exitCode = 3;\n');
  const forged = await shell("bash", [launcher, "run", "--suite", fixture.suite]);
  assert.equal(forged.code, 4);
  assert.equal(forged.stdout, "");
  assert.equal(forged.stderr, "email-evals: runtime failure\n");
});

test("launcher rejects forged completed output without forwarding bounded child data", async () => {
  const fixture = await runtimeFixture();
  const launcher = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../email-evals.sh");
  const cli = path.join(fixture.runtime, "cli.mjs");
  const valid = completedSummary("transport_error");
  valid.command = "run";
  const credentialId = completedSummary("transport_error", "e2a_acct_synthetic");
  credentialId.command = "run";
  const underscoreId = completedSummary("transport_error", "synthetic_case");
  underscoreId.command = "run";
  for (const source of [
    `process.stdout.write(${JSON.stringify(`${JSON.stringify(valid)}\nextra\n`)}); process.exitCode = 3;\n`,
    `const output = ${JSON.stringify(valid)}; output.summary.cases[0].errorClass = "assertion_failure"; process.stdout.write(JSON.stringify(output) + "\\n"); process.exitCode = 3;\n`,
    `const output = ${JSON.stringify(valid)}; output.secret = "e2a_acct_synthetic /private/tmp/secret"; process.stdout.write(JSON.stringify(output) + "\\n"); process.exitCode = 3;\n`,
    `process.stdout.write(${JSON.stringify(`${JSON.stringify(credentialId)}\n`)}); process.exitCode = 3;\n`,
    `process.stdout.write(${JSON.stringify(`${JSON.stringify(underscoreId)}\n`)}); process.exitCode = 3;\n`,
    `process.stdout.write(${JSON.stringify(`${JSON.stringify(valid)}\n`)}); process.stderr.write("email-evals: transport_error\\n"); process.exitCode = 3;\n`,
    `process.stdout.write(${JSON.stringify(`${JSON.stringify(valid)}\n`)}); process.stderr.write("email-evals: target_timeout\\n"); process.exitCode = 3;\n`,
    `process.stdout.write(${JSON.stringify(`${JSON.stringify(valid)}\n`)}); process.stderr.write("e2a_acct_synthetic /private/tmp/secret\\n"); process.exitCode = 3;\n`,
    `process.stdout.write("x".repeat(17 * 1024 * 1024)); process.exitCode = 3;\n`,
  ]) {
    await writeFile(cli, source);
    const result = await shell("bash", [launcher, "run", "--suite", fixture.suite, "--json"]);
    assert.equal(result.code, 4);
    assert.equal(result.stdout, "");
    assert.equal(result.stderr, "email-evals: runtime failure\n");
    assert.doesNotMatch(result.stdout + result.stderr, /e2a_acct_|\/private\/tmp\/secret/);
  }
});

test("launcher rejects signaled completed output and incomplete human output", async () => {
  const fixture = await runtimeFixture();
  const valid = completedSummary("grader_error");
  valid.command = "run";
  for (const [json, childOutput, childError] of [
    [true, `${JSON.stringify(valid)}\n`, ""],
    [false, "Status: fail; 0/1 passed\nComplete: yes\nReport: run_20260809T120000_0123abcd/report.md\n", "email-evals: grader_error\n"],
  ]) {
    const child = fakeRuntimeChild();
    const output = [];
    const errors = [];
    const launched = launch(["run", "--suite", fixture.suite, ...(json ? ["--json"] : [])], {
      cwd: fixture.root,
      stdout: { write: (line) => output.push(line) },
      stderr: { write: (line) => errors.push(line) },
      spawnChild: () => {
        queueMicrotask(() => {
          child.stdout.emit("data", Buffer.from(childOutput));
          child.stderr.emit("data", Buffer.from(childError));
          child.stdout.emit("end");
          child.stderr.emit("end");
          child.child.emit("exit", null, "SIGTERM");
        });
        return child.child;
      },
    });
    assert.equal(await launched, 4);
    assert.equal(output.join(""), "");
    assert.equal(errors.join(""), "email-evals: runtime failure\n");
  }

  const cli = path.join(fixture.runtime, "cli.mjs");
  await writeFile(cli, 'process.stdout.write("Status: fail; 0/1 passed\\nComplete: no\\nReport: run_20260809T120000_0123abcd/report.md\\n"); process.stderr.write("email-evals: transport_error\\n"); process.exitCode = 3;\n');
  const incomplete = await shell("bash", [path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../email-evals.sh"), "run", "--suite", fixture.suite]);
  assert.equal(incomplete.code, 4);
  assert.equal(incomplete.stdout, "");
  assert.equal(incomplete.stderr, "email-evals: runtime failure\n");
});

test("runtime launcher waits for stderr finalization and fails closed on event races", async () => {
  const forwarded = fakeRuntimeChild();
  const forwardedTimers = manualTimers();
  const forwardedRun = runRuntimeNode([], {}, {
    spawnChild: () => forwarded.child,
    setTimer: forwardedTimers.set,
    clearTimer: forwardedTimers.clear,
    graceMs: 1,
  });
  forwarded.child.emit("exit", 3, null);
  assert.equal(forwardedTimers.pending().length, 1);
  forwarded.stdout.emit("close");
  forwarded.stderr.emit("data", Buffer.from("email-evals: transport_error\n"));
  forwarded.stderr.emit("close");
  assert.deepEqual(await forwardedRun, { code: 3, stdout: "", stderr: "email-evals: transport_error\n", truncated: false, finalized: true, termination: "exit" });
  assert.equal(forwardedTimers.pending().length, 0);

  const held = fakeRuntimeChild();
  const heldTimers = manualTimers();
  const heldRun = runRuntimeNode([], {}, {
    spawnChild: () => held.child,
    setTimer: heldTimers.set,
    clearTimer: heldTimers.clear,
    graceMs: 1,
  });
  held.child.emit("exit", 3, null);
  heldTimers.pending()[0]();
  assert.deepEqual(await heldRun, { code: 4, stdout: "", stderr: "", truncated: true, finalized: false, termination: "timeout" });
  assert.equal(held.destroyed(), 1);
  assert.equal(held.unrefed(), 1);

  const spawnFailure = fakeRuntimeChild();
  const failedRun = runRuntimeNode([], {}, { spawnChild: () => spawnFailure.child });
  spawnFailure.child.emit("error", new Error("e2a_acct_synthetic /private/tmp/error"));
  assert.deepEqual(await failedRun, { code: 4, stdout: "", stderr: "", truncated: true, finalized: false, termination: "spawn_error" });

  const signaled = fakeRuntimeChild();
  const signaledRun = runRuntimeNode([], {}, { spawnChild: () => signaled.child });
  signaled.child.emit("exit", null, "SIGTERM");
  signaled.stdout.emit("end");
  signaled.stderr.emit("data", Buffer.from("email-evals: transport_error\n"));
  signaled.stderr.emit("end");
  const signaledResult = await signaledRun;
  signaled.child.emit("error", new Error("late error"));
  signaled.stderr.emit("close");
  assert.deepEqual(signaledResult, { code: 4, stdout: "", stderr: "email-evals: transport_error\n", truncated: false, finalized: true, termination: "signal" });

  const fixture = await runtimeFixture();
  const launcherFailure = fakeRuntimeChild();
  let spawned;
  const errors = [];
  const failureRun = launch(["validate", "--suite", fixture.suite], {
    cwd: fixture.root,
    stderr: { write: (line) => errors.push(line) },
    spawnChild: () => {
      spawned?.();
      return launcherFailure.child;
    },
  });
  await new Promise((resolve) => { spawned = resolve; });
  launcherFailure.child.emit("error", new Error("e2a_acct_synthetic /private/tmp/error"));
  assert.equal(await failureRun, 4);
  launcherFailure.child.emit("exit", 3, null);
  launcherFailure.stderr.emit("close");
  assert.deepEqual(errors, ["email-evals: runtime failure\n"]);
});

test("runtime launcher catches stdout and stderr stream errors without leaking them", async () => {
  for (const streamName of ["stdout", "stderr"]) {
    const child = fakeRuntimeChild();
    const result = runRuntimeNode([], {}, { spawnChild: () => child.child });
    child[streamName].emit("error", new Error(`e2a_acct_synthetic /private/tmp/${streamName}`));
    assert.deepEqual(await result, {
      code: 4, stdout: "", stderr: "", truncated: true, finalized: false, termination: "stream_error",
    });
  }

  const fixture = await runtimeFixture();
  for (const streamName of ["stdout", "stderr"]) {
    const child = fakeRuntimeChild();
    const output = [];
    const errors = [];
    const launched = launch(["validate", "--suite", fixture.suite], {
      cwd: fixture.root,
      stdout: { write: (line) => output.push(line) },
      stderr: { write: (line) => errors.push(line) },
      spawnChild: () => {
        queueMicrotask(() => child[streamName].emit(
          "error", new Error(`e2a_acct_synthetic /private/tmp/${streamName}`),
        ));
        return child.child;
      },
    });
    assert.equal(await launched, 4, streamName);
    assert.equal(output.join(""), "", streamName);
    assert.equal(errors.join(""), "email-evals: runtime failure\n", streamName);
  }
});
