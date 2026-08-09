import { copyFile, lstat, mkdir, chmod } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { spawn } from "node:child_process";

const setupDirectory = path.dirname(fileURLToPath(import.meta.url));
const bundledRuntime = path.join(setupDirectory, "runtime");
const runtimeFiles = ["package.json", "package-lock.json", "cli.mjs"];

export function runtimePaths(suiteRoot) {
  const root = path.join(path.resolve(suiteRoot), ".eval-runtime");
  return {
    root,
    source: bundledRuntime,
    packageFile: path.join(root, "package.json"),
    cli: path.join(root, "cli.mjs"),
  };
}

async function pathExists(file) {
  try {
    return await lstat(file);
  } catch (error) {
    if (error?.code === "ENOENT") return null;
    throw error;
  }
}

async function createPrivateDirectory(directory) {
  await mkdir(directory, { recursive: true, mode: 0o700 });
  await chmod(directory, 0o700);
}

async function copyIfPresent(source, destination) {
  const entry = await pathExists(source);
  if (!entry) return;
  if (!entry.isFile() || entry.isSymbolicLink()) {
    throw new Error(`Runtime source entry must be a regular file: ${source}`);
  }
  await copyFile(source, destination);
}

async function copyLibrary(sourceDirectory, destinationDirectory) {
  const source = path.join(sourceDirectory, "lib");
  const entry = await pathExists(source);
  if (!entry) return;
  if (!entry.isDirectory() || entry.isSymbolicLink()) {
    throw new Error(`Runtime source library must be a directory: ${source}`);
  }

  const { readdir } = await import("node:fs/promises");
  await createPrivateDirectory(destinationDirectory);
  for (const child of await readdir(source, { withFileTypes: true })) {
    const childSource = path.join(source, child.name);
    const childDestination = path.join(destinationDirectory, child.name);
    if (child.isDirectory()) {
      await copyLibraryDirectory(childSource, childDestination);
    } else if (child.isFile() && !child.isSymbolicLink()) {
      await copyFile(childSource, childDestination);
    } else {
      throw new Error(`Runtime source contains an unsupported entry: ${childSource}`);
    }
  }
}

async function copyLibraryDirectory(sourceDirectory, destinationDirectory) {
  const { readdir } = await import("node:fs/promises");
  await createPrivateDirectory(destinationDirectory);
  for (const child of await readdir(sourceDirectory, { withFileTypes: true })) {
    const childSource = path.join(sourceDirectory, child.name);
    const childDestination = path.join(destinationDirectory, child.name);
    if (child.isDirectory()) {
      await copyLibraryDirectory(childSource, childDestination);
    } else if (child.isFile() && !child.isSymbolicLink()) {
      await copyFile(childSource, childDestination);
    } else {
      throw new Error(`Runtime source contains an unsupported entry: ${childSource}`);
    }
  }
}

function runNpmCi(args, options) {
  return new Promise((resolve, reject) => {
    const child = spawn("npm", args, { ...options, stdio: "inherit" });
    child.once("error", reject);
    child.once("exit", (code, signal) => {
      if (code === 0) {
        resolve();
      } else {
        reject(new Error(`npm ci failed${signal ? ` (${signal})` : ` with exit code ${code}`}`));
      }
    });
  });
}

export async function installRuntime({ suiteRoot, sourceRoot = bundledRuntime, runNpm = runNpmCi }) {
  const paths = runtimePaths(suiteRoot);
  const source = path.resolve(sourceRoot);
  const existing = await pathExists(paths.root);
  if (existing?.isSymbolicLink()) {
    throw new Error(`Refusing to follow symlink at ${paths.root}`);
  }
  if (existing && !existing.isDirectory()) {
    throw new Error(`Runtime path is not a directory: ${paths.root}`);
  }

  await createPrivateDirectory(paths.root);
  for (const file of runtimeFiles) {
    await copyIfPresent(path.join(source, file), path.join(paths.root, file));
  }
  await copyLibrary(source, path.join(paths.root, "lib"));
  await runNpm(["ci", "--omit=dev", "--ignore-scripts"], { cwd: paths.root });
  return paths;
}

function parseRoot(args) {
  if (args.length !== 2 || args[0] !== "--root" || !args[1]) {
    throw new Error("Usage: setup.mjs --root <suite-root>");
  }
  return args[1];
}

async function main() {
  const suiteRoot = parseRoot(process.argv.slice(2));
  const paths = await installRuntime({ suiteRoot });
  process.stdout.write(`Installed email eval runtime at ${paths.root}\n`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  });
}
