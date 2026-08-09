import { constants as fsConstants } from "node:fs";
import { lstat, open, realpath } from "node:fs/promises";
import path from "node:path";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { CliUsageError, parseRuntimeArguments, usage } from "./runtime/lib/cli-arguments.mjs";

const launcherDirectory = path.dirname(fileURLToPath(import.meta.url));
const RUNTIME_STDERR_LIMIT = 8192;
const SAFE_RUNTIME_DIAGNOSTICS = new Map([
  ["email-evals: assertion_failure\n", 1],
  ["email-evals: configuration_error\n", 2],
  ["email-evals: capability_error\n", 2],
  ["email-evals: transport_error\n", 3],
  ["email-evals: target_timeout\n", 3],
  ["email-evals: grader_error\n", 4],
  ["email-evals: unexpected runner failure\n", 4],
]);

function unavailable(stderr) {
  stderr.write("email-evals: runtime unavailable\n");
  return 2;
}

function runNode(args, options) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, args, options);
    child.once("error", reject);
    child.once("exit", (code, signal) => resolve(code ?? (signal ? 4 : 4)));
  });
}

function runRuntimeNode(args, options) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, args, { ...options, stdio: ["inherit", "inherit", "pipe"] });
    const chunks = [];
    let size = 0;
    let truncated = false;
    child.stderr.on("data", (chunk) => {
      if (size >= RUNTIME_STDERR_LIMIT) {
        truncated = true;
        return;
      }
      const remaining = RUNTIME_STDERR_LIMIT - size;
      const retained = chunk.length > remaining ? chunk.subarray(0, remaining) : chunk;
      chunks.push(retained);
      size += retained.length;
      if (chunk.length > remaining) truncated = true;
    });
    child.once("error", reject);
    child.once("exit", (code, signal) => resolve({
      code: code ?? (signal ? 4 : 4),
      stderr: Buffer.concat(chunks).toString("utf8"),
      truncated,
    }));
  });
}

function safeRuntimeExit(result, stderr) {
  const expectedCode = !result.truncated ? SAFE_RUNTIME_DIAGNOSTICS.get(result.stderr) : undefined;
  if (expectedCode !== undefined && result.code === expectedCode) {
    stderr.write(result.stderr);
    return result.code;
  }
  if (!result.truncated && result.stderr === "" && result.code === 0) return 0;
  stderr.write("email-evals: runtime failure\n");
  return 4;
}

async function regularNoLink(file) {
  const state = await lstat(file);
  if (state.isSymbolicLink() || !state.isFile()) throw new Error("unsafe runtime path");
  return state;
}

async function directoryNoLink(directory) {
  const state = await lstat(directory);
  if (state.isSymbolicLink() || !state.isDirectory()) throw new Error("unsafe runtime path");
  return realpath(directory);
}

function sameIdentity(left, right) {
  return left.dev === right.dev && left.ino === right.ino && left.isFile() && right.isFile();
}

async function launchLocal(command, args, dependencies) {
  const script = command === "scaffold" ? "scaffold.mjs" : "setup.mjs";
  try {
    return await dependencies.spawnNode([path.join(launcherDirectory, script), ...args], {
      cwd: dependencies.cwd,
      env: dependencies.environment,
      stdio: "inherit",
    });
  } catch {
    dependencies.stderr.write("email-evals: launcher failure\n");
    return 4;
  }
}

async function launchRuntime(argv, parsed, dependencies) {
  let handle;
  try {
    const requestedSuite = path.resolve(dependencies.cwd, parsed.suite);
    const requestedParent = path.dirname(requestedSuite);
    const suiteName = path.basename(requestedSuite);
    const suiteParent = await directoryNoLink(requestedParent);
    const suite = path.join(suiteParent, suiteName);
    const suiteState = await regularNoLink(suite);
    const resolvedSuite = await realpath(suite);
    if (path.dirname(resolvedSuite) !== suiteParent || path.basename(resolvedSuite) !== suiteName) throw new Error("suite escaped root");

    const runtime = path.join(suiteParent, ".eval-runtime");
    const resolvedRuntime = await directoryNoLink(runtime);
    if (path.dirname(resolvedRuntime) !== suiteParent || path.basename(resolvedRuntime) !== ".eval-runtime") throw new Error("runtime escaped root");
    const cli = path.join(resolvedRuntime, "cli.mjs");
    const beforeOpen = await regularNoLink(cli);
    const resolvedCli = await realpath(cli);
    if (path.dirname(resolvedCli) !== resolvedRuntime || path.basename(resolvedCli) !== "cli.mjs") throw new Error("cli escaped runtime");
    handle = await open(resolvedCli, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW);
    if (!sameIdentity(beforeOpen, await handle.stat())) throw new Error("cli changed while opening");
    await dependencies.beforeExec?.();
    const immediatelyBeforeExec = await regularNoLink(resolvedCli);
    if (!sameIdentity(beforeOpen, immediatelyBeforeExec) || await realpath(resolvedCli) !== resolvedCli) throw new Error("cli changed before execution");
    const result = await dependencies.spawnRuntime([resolvedCli, ...argv], {
      cwd: dependencies.cwd,
      env: dependencies.environment,
    });
    return safeRuntimeExit(result, dependencies.stderr);
  } catch {
    return unavailable(dependencies.stderr);
  } finally {
    await handle?.close().catch(() => {});
  }
}

export async function main(argv, supplied = {}) {
  const dependencies = {
    cwd: process.cwd(),
    environment: process.env,
    stdout: process.stdout,
    stderr: process.stderr,
    spawnNode: runNode,
    spawnRuntime: runRuntimeNode,
    ...supplied,
  };
  if (argv[0] === "scaffold" || argv[0] === "setup") return launchLocal(argv[0], argv.slice(1), dependencies);
  let parsed;
  try {
    parsed = parseRuntimeArguments(argv);
  } catch (error) {
    if (error instanceof CliUsageError) {
      dependencies.stderr.write(`${error.message}\n`);
      return 2;
    }
    dependencies.stderr.write("email-evals: launcher failure\n");
    return 4;
  }
  if (parsed.help) {
    dependencies.stdout.write(`${usage()}\n`);
    return 0;
  }
  return launchRuntime(argv, parsed, dependencies);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2)).then((code) => { process.exitCode = code; });
}
