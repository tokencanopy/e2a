import { open, mkdir, readFile } from "node:fs/promises";
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

async function writeExclusive(file, content) {
  let handle;
  try {
    handle = await open(file, "wx", 0o600);
    await handle.writeFile(content, "utf8");
    return true;
  } catch (error) {
    if (error?.code === "EEXIST") return false;
    throw error;
  } finally {
    await handle?.close();
  }
}

export async function scaffoldSuite(options) {
  validateOptions(options);
  const root = path.resolve(options.root);
  await mkdir(root, { recursive: true });
  const created = [];
  const preserved = [];

  for (const relativeFile of templateFiles) {
    const template = await readFile(path.join(templatesDirectory, relativeFile), "utf8");
    const destination = path.join(root, relativeFile);
    await mkdir(path.dirname(destination), { recursive: true });
    if (await writeExclusive(destination, expandTemplate(template, options))) {
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
