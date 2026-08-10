import { lstat } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const setupDirectory = path.dirname(fileURLToPath(import.meta.url));
const bundledRuntime = path.join(setupDirectory, "runtime");
const runtimeFiles = ["email-evals-runtime.bundle.mjs", "THIRD_PARTY_NOTICES.md"];

export function runtimePaths(_suiteRoot, sourceRoot = bundledRuntime) {
  const root = path.resolve(sourceRoot);
  return {
    root,
    source: root,
    packageFile: path.join(root, "package.json"),
    cli: path.join(root, "email-evals-runtime.bundle.mjs"),
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

async function safeDestinationEntry(destination, expectedType) {
  const entry = await pathExists(destination);
  if (!entry) return null;
  if (entry.isSymbolicLink()) {
    throw new Error(`Refusing to follow symlink at ${destination}`);
  }
  if (expectedType === "directory" && !entry.isDirectory()) {
    throw new Error(`Runtime path is not a directory: ${destination}`);
  }
  if (expectedType === "file" && !entry.isFile()) {
    throw new Error(`Runtime path is not a regular file: ${destination}`);
  }
  return entry;
}

export async function installRuntime({ suiteRoot, sourceRoot = bundledRuntime }) {
  const paths = runtimePaths(suiteRoot, sourceRoot);
  await safeDestinationEntry(path.resolve(suiteRoot), "directory");
  await safeDestinationEntry(paths.root, "directory");
  for (const file of runtimeFiles) {
    const entry = await safeDestinationEntry(path.join(paths.root, file), "file");
    if (!entry) throw new Error(`Trusted runtime file is missing: ${file}`);
  }
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
  process.stdout.write(`Prepared trusted email eval runtime at ${paths.root}\n`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  });
}
