const COMMANDS = new Set(["validate", "run", "regrade"]);
const OPTION_NAMES = new Set(["--suite", "--output", "--run", "--json"]);
const COMMAND_OPTIONS = Object.freeze({
  validate: new Set(["--suite", "--json"]),
  run: new Set(["--suite", "--output", "--json"]),
  regrade: new Set(["--suite", "--run", "--json"]),
});
const VALUE_OPTIONS = new Set(["--suite", "--output", "--run"]);

export class CliUsageError extends Error {}

export function usage() {
  return [
    "Usage:",
    "  email-evals validate --suite <suite.yaml> [--json]",
    "  email-evals run --suite <suite.yaml> [--output <results-dir>] [--json]",
    "  email-evals regrade --suite <suite.yaml> --run <run-dir> [--json]",
  ].join("\n");
}

function safeOption(option) {
  return /^--[a-z][a-z0-9-]*$/.test(option) ? option : "[invalid option]";
}

/** Parse the complete stable runtime grammar without filesystem or transport work. */
export function parseRuntimeArguments(argv) {
  if ((argv.length === 1 && argv[0] === "--help") || (argv.length === 1 && argv[0] === "help")) return { help: true };
  const [command, ...tokens] = argv;
  if (!COMMANDS.has(command)) throw new CliUsageError(command === undefined ? "Missing command" : "Unknown command");
  const values = {};
  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    if (!OPTION_NAMES.has(token)) {
      if (typeof token === "string" && token.startsWith("--")) throw new CliUsageError(`Unknown option: ${safeOption(token)}`);
      throw new CliUsageError("Unexpected positional argument");
    }
    if (!COMMAND_OPTIONS[command].has(token)) throw new CliUsageError(`Option not allowed for ${command}: ${token}`);
    if (Object.hasOwn(values, token)) throw new CliUsageError(`Duplicate option: ${token}`);
    if (VALUE_OPTIONS.has(token)) {
      const value = tokens[index + 1];
      if (typeof value !== "string" || value.length === 0 || value.startsWith("--")) throw new CliUsageError(`Missing value for: ${token}`);
      values[token] = value;
      index += 1;
    } else {
      values[token] = true;
    }
  }
  for (const required of command === "regrade" ? ["--suite", "--run"] : ["--suite"]) {
    if (!Object.hasOwn(values, required)) throw new CliUsageError(`Missing required option: ${required}`);
  }
  return { command, suite: values["--suite"], output: values["--output"], run: values["--run"], json: values["--json"] === true };
}
