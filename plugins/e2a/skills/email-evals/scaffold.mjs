import { lstat, mkdir, open, readFile, realpath, unlink } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const scaffoldDirectory = path.dirname(fileURLToPath(import.meta.url));
const templatesDirectory = path.join(scaffoldDirectory, "templates");
const suiteNamePattern = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;
const environmentNamePattern = /^[A-Z][A-Z0-9_]*$/;

const templateFiles = [
  ".gitignore",
  "README.md",
  "cases/happy-path.yaml",
  "cases/missing-information.yaml",
  "cases/unsafe-request.yaml",
  "fixtures/README.md",
  "results/.gitignore",
  "suite.yaml",
];

function validateReplacement(name, value, pattern) {
  if (typeof value !== "string" || !pattern.test(value)) {
    throw new TypeError(`Invalid ${name}`);
  }
}

function validateOptions({ root, suiteName, targetEnv, actorEnv, apiKeyEnv }) {
  if (typeof root !== "string" || root.length === 0) throw new TypeError("Invalid root");
  validateReplacement("suiteName", suiteName, suiteNamePattern);
  validateReplacement("targetEnv", targetEnv, environmentNamePattern);
  validateReplacement("actorEnv", actorEnv, environmentNamePattern);
  validateReplacement("apiKeyEnv", apiKeyEnv, environmentNamePattern);
}

function expandTemplate(source, { suiteName, targetEnv, actorEnv, apiKeyEnv }) {
  const replacements = {
    __SUITE_NAME__: suiteName,
    __TARGET_ENV__: targetEnv,
    __ACTOR_ENV__: actorEnv,
    __API_KEY_ENV__: apiKeyEnv,
  };
  const unknownToken = source.match(/__[A-Z0-9_]+__/g)?.find((token) => !(token in replacements));
  if (unknownToken) throw new Error(`Unknown scaffold template token: ${unknownToken}`);
  return Object.entries(replacements).reduce(
    (expanded, [token, value]) => expanded.replaceAll(token, value),
    source,
  );
}

async function pathEntry(file) {
  try {
    return await lstat(file);
  } catch (error) {
    if (error?.code === "ENOENT") return null;
    throw error;
  }
}

function isContainedPath(root, candidate) {
  const relative = path.relative(root, candidate);
  return relative === "" || (!path.isAbsolute(relative) && !relative.startsWith(`..${path.sep}`) && relative !== "..");
}

function assertContainedPath(root, candidate) {
  if (!isContainedPath(root, candidate)) {
    throw new Error(`Scaffold path is outside the suite root: ${candidate}`);
  }
}

async function assertSafeDirectory(directory) {
  const entry = await pathEntry(directory);
  if (entry?.isSymbolicLink()) throw new Error(`Refusing to follow symlink at ${directory}`);
  if (!entry?.isDirectory()) throw new Error(`Scaffold path is not a directory: ${directory}`);
}

async function prepareRoot(root) {
  const requestedRoot = path.resolve(root);
  const existing = await pathEntry(requestedRoot);
  if (existing?.isSymbolicLink()) throw new Error(`Refusing to follow symlink at ${requestedRoot}`);
  if (!existing) await mkdir(requestedRoot, { recursive: true });
  await assertSafeDirectory(requestedRoot);
  return realpath(requestedRoot);
}

async function prepareParent(root, parent) {
  assertContainedPath(root, parent);
  await assertSafeDirectory(root);
  const relative = path.relative(root, parent);
  let current = root;
  for (const segment of relative === "" ? [] : relative.split(path.sep)) {
    current = path.join(current, segment);
    const entry = await pathEntry(current);
    if (!entry) await mkdir(current);
    await assertSafeDirectory(current);
  }
  const resolvedParent = await realpath(parent);
  assertContainedPath(root, resolvedParent);
}

async function removeCreatedFile(file, identity) {
  try {
    const entry = await pathEntry(file);
    if (entry?.isFile() && entry.dev === identity.dev && entry.ino === identity.ino) {
      await unlink(file);
    }
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
  }
}

async function writeExclusive(file, content, openFile = open) {
  let handle;
  let identity;
  try {
    handle = await openFile(file, "wx", 0o600);
    identity = await handle.stat();
    await handle.writeFile(content, "utf8");
    await handle.close();
    handle = undefined;
    return true;
  } catch (error) {
    if (error?.code === "EEXIST") return false;
    if (handle) {
      try {
        await handle.close();
      } catch {
        // The original write failure remains the caller-visible error.
      }
      handle = undefined;
    }
    if (identity) await removeCreatedFile(file, identity);
    throw error;
  }
}

export async function scaffoldSuite(options, { openFile = open } = {}) {
  validateOptions(options);
  const root = await prepareRoot(options.root);
  const created = [];
  const preserved = [];

  for (const relativeFile of templateFiles) {
    const template = await readFile(path.join(templatesDirectory, relativeFile), "utf8");
    const destination = path.join(root, relativeFile);
    await prepareParent(root, path.dirname(destination));
    if (await writeExclusive(destination, expandTemplate(template, options), openFile)) {
      created.push(relativeFile);
    } else {
      preserved.push(relativeFile);
    }
  }

  return { created, preserved };
}

function parseArguments(args) {
  const values = {};
  const optionNames = new Set(["--root", "--name", "--target-env", "--actor-env", "--api-key-env"]);
  if (args.length % 2 !== 0) throw new Error("Usage: scaffold.mjs --root <suite-root> --name <suite-name> --target-env <env> --actor-env <env> --api-key-env <env>");
  for (let index = 0; index < args.length; index += 2) {
    const option = args[index];
    const value = args[index + 1];
    if (!optionNames.has(option) || !value || values[option] !== undefined) {
      throw new Error("Usage: scaffold.mjs --root <suite-root> --name <suite-name> --target-env <env> --actor-env <env> --api-key-env <env>");
    }
    values[option] = value;
  }
  if (["--root", "--name", "--target-env", "--actor-env", "--api-key-env"].some((option) => values[option] === undefined)) {
    throw new Error("Usage: scaffold.mjs --root <suite-root> --name <suite-name> --target-env <env> --actor-env <env> --api-key-env <env>");
  }
  return {
    root: values["--root"],
    suiteName: values["--name"],
    targetEnv: values["--target-env"],
    actorEnv: values["--actor-env"],
    apiKeyEnv: values["--api-key-env"],
  };
}

async function main() {
  const result = await scaffoldSuite(parseArguments(process.argv.slice(2)));
  for (const file of result.created) process.stdout.write(`created ${file}\n`);
  for (const file of result.preserved) process.stdout.write(`preserved ${file}\n`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  });
}
