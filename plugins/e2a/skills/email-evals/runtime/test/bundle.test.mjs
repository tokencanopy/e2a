import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { build } from "esbuild";

const runtimeDirectory = fileURLToPath(new URL("..", import.meta.url));
const bundle = new URL("../email-evals-runtime.bundle.mjs", import.meta.url);
const notices = new URL("../THIRD_PARTY_NOTICES.md", import.meta.url);
const requireBanner = [
  "/*! Third-party license notices: THIRD_PARTY_NOTICES.md */",
  'import { createRequire } from "node:module"; const require = createRequire(import.meta.url);',
].join("\n");
const noticePackages = Object.freeze([
  { name: "postal-mime", version: "2.7.5", source: "https://postal-mime.postalsys.com", license: "LICENSE.txt" },
  { name: "re2js", version: "2.8.6", source: "https://github.com/le0pard/re2js", license: "LICENSE" },
  { name: "ws", version: "8.21.3", source: "https://github.com/websockets/ws", license: "LICENSE" },
  { name: "yaml", version: "2.9.0", source: "https://eemeli.org/yaml/", license: "LICENSE" },
]);

test("checked-in trusted runtime bundle is byte-fresh", async () => {
  const generated = await build({
    absWorkingDir: runtimeDirectory,
    entryPoints: ["cli.mjs"],
    bundle: true,
    banner: { js: requireBanner },
    format: "esm",
    legalComments: "none",
    minifyWhitespace: true,
    platform: "node",
    target: "node18",
    write: false,
  });
  assert.equal(generated.outputFiles.length, 1);
  const checkedInBundle = await readFile(bundle, "utf8");
  assert.equal(checkedInBundle, generated.outputFiles[0].text);
  assert.doesNotMatch(checkedInBundle, /@agents\.e2a\.dev\b/);
});

test("checked-in third-party notices exactly match every bundled dependency license", async () => {
  const lines = [
    "# Third-party notices",
    "",
    "The checked-in `email-evals-runtime.bundle.mjs` contains code from the",
    "following packages. The first-party `@e2a/sdk` code remains covered by this",
    "repository's root `LICENSE`.",
    "",
  ];
  for (const dependency of noticePackages) {
    const directory = path.join(runtimeDirectory, "node_modules", dependency.name);
    const manifest = JSON.parse(await readFile(path.join(directory, "package.json"), "utf8"));
    assert.equal(manifest.version, dependency.version);
    const license = (await readFile(path.join(directory, dependency.license), "utf8")).trimEnd();
    lines.push(
      `## ${dependency.name} ${dependency.version}`,
      "",
      `Source: ${dependency.source}`,
      "",
      "```text",
      ...license.split("\n"),
      "```",
      "",
    );
  }
  lines.pop();
  assert.equal(await readFile(notices, "utf8"), `${lines.join("\n")}\n`);
});

test("checked-in trusted runtime bundle executes with the launcher's minimal environment", () => {
  const hostileCwd = mkdtempSync(path.join(tmpdir(), "email-evals-bundle-cwd-"));
  for (const packageName of ["yaml", "bufferutil", "utf-8-validate"]) {
    const packageDirectory = path.join(hostileCwd, "node_modules", packageName);
    mkdirSync(packageDirectory, { recursive: true });
    writeFileSync(path.join(packageDirectory, "index.js"), 'throw new Error("suite cwd dependency loaded");\n');
  }
  const result = spawnSync(process.execPath, [fileURLToPath(bundle), "--help"], {
    cwd: hostileCwd,
    encoding: "utf8",
    env: {
      ...(typeof process.env.PATH === "string" ? { PATH: process.env.PATH } : {}),
      WS_NO_BUFFER_UTIL: "1",
      WS_NO_UTF_8_VALIDATE: "1",
    },
    timeout: 10_000,
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /^Usage:\n  email-evals validate --suite <suite\.yaml>/);
  assert.equal(result.stderr, "");
});
