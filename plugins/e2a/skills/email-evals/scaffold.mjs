import { link, lstat, mkdir, open, readFile, realpath, unlink } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { randomUUID } from "node:crypto";
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

async function createSafeDirectory(directory) {
  const existing = await pathEntry(directory);
  if (!existing) {
    try {
      await mkdir(directory);
    } catch (error) {
      if (error?.code !== "EEXIST") throw error;
    }
  }
  await assertSafeDirectory(directory);
}

async function canonicalizeTemporaryDirectoryRoot(root) {
  const requestedRoot = path.resolve(root);
  const configuredTempDirectory = path.resolve(tmpdir());
  if (!isContainedPath(configuredTempDirectory, requestedRoot)) return requestedRoot;
  return path.join(
    await realpath(configuredTempDirectory),
    path.relative(configuredTempDirectory, requestedRoot),
  );
}

async function prepareRoot(root) {
  const requestedRoot = await canonicalizeTemporaryDirectoryRoot(root);
  const parsed = path.parse(requestedRoot);
  let current = parsed.root;
  for (const segment of path.relative(parsed.root, requestedRoot).split(path.sep).filter(Boolean)) {
    current = path.join(current, segment);
    await createSafeDirectory(current);
  }
  return realpath(requestedRoot);
}

async function inspectSafeParent(root, parent) {
  assertContainedPath(root, parent);
  await assertSafeDirectory(parent);
  const resolvedParent = await realpath(parent);
  assertContainedPath(root, resolvedParent);
  const entry = await pathEntry(parent);
  return { dev: entry.dev, ino: entry.ino };
}

async function prepareParent(root, parent) {
  assertContainedPath(root, parent);
  let current = root;
  for (const segment of path.relative(root, parent).split(path.sep).filter(Boolean)) {
    current = path.join(current, segment);
    await createSafeDirectory(current);
  }
  return inspectSafeParent(root, parent);
}

async function assertUnchangedParent(root, parent, identity) {
  const current = await inspectSafeParent(root, parent);
  if (current.dev !== identity.dev || current.ino !== identity.ino) {
    throw new Error(`Scaffold parent changed during write: ${parent}`);
  }
}

async function removePublishedTemporary(file) {
  try {
    await unlink(file);
  } catch (error) {
    if (error?.code !== "ENOENT") {
      // The destination is already atomically published; a stale private temp
      // file is safer than failing or touching the destination.
    }
  }
}

async function writeExclusive(destination, content, { root, parent, parentIdentity, openFile = open, linkFile = link }) {
  const temporary = path.join(parent, `.${path.basename(destination)}.email-evals-${randomUUID()}.tmp`);
  let handle;
  try {
    handle = await openFile(temporary, "wx", 0o600);
    await handle.writeFile(content, "utf8");
    await handle.close();
    handle = undefined;
    await assertUnchangedParent(root, parent, parentIdentity);
    try {
      await linkFile(temporary, destination);
    } catch (error) {
      if (error?.code !== "EEXIST") throw error;
      await removePublishedTemporary(temporary);
      return false;
    }
    await removePublishedTemporary(temporary);
    return true;
  } catch (error) {
    if (handle) {
      try {
        await handle.close();
      } catch {
        // The original write failure remains the caller-visible error.
      }
      handle = undefined;
    }
    // Do not delete a failed temporary path: without openat-style APIs, a
    // later path replacement could be user-owned. No destination was linked.
    throw error;
  }
}

export async function scaffoldSuite(options, { openFile = open, linkFile = link } = {}) {
  validateOptions(options);
  const root = await prepareRoot(options.root);
  const created = [];
  const preserved = [];

  for (const relativeFile of templateFiles) {
    const template = await readFile(path.join(templatesDirectory, relativeFile), "utf8");
    const destination = path.join(root, relativeFile);
    const parent = path.dirname(destination);
    const parentIdentity = await prepareParent(root, parent);
    if (await writeExclusive(destination, expandTemplate(template, options), {
      root,
      parent,
      parentIdentity,
      openFile,
      linkFile,
    })) {
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
