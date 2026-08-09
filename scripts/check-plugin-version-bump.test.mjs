import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { copyFile, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import {
  changedPlugins,
  compareSemVer,
  unchangedVersions,
} from "./check-plugin-version-bump.mjs";

const gateSource = fileURLToPath(new URL("./check-plugin-version-bump.mjs", import.meta.url));

function run(repo, command, args) {
  return spawnSync(command, args, {
    cwd: repo,
    encoding: "utf8",
    maxBuffer: 10 * 1024 * 1024,
  });
}

function git(repo, args) {
  const result = run(repo, "git", ["-c", "core.fsmonitor=false", ...args]);
  if (result.error) throw result.error;
  if (result.status !== 0) {
    throw new Error(result.stderr.trim() || `git ${args[0]} exited ${result.status}`);
  }
  return result.stdout.trim();
}

async function put(repo, path, contents) {
  const target = join(repo, path);
  await mkdir(dirname(target), { recursive: true });
  await writeFile(target, contents);
}

async function putManifest(repo, plugin, version) {
  await put(
    repo,
    `plugins/${plugin}/.claude-plugin/plugin.json`,
    `${JSON.stringify({ name: plugin, version }, null, 2)}\n`,
  );
}

function commitAll(repo, message) {
  git(repo, ["add", "--all"]);
  git(repo, [
    "-c", "user.name=Release Gate Test",
    "-c", "user.email=release-gate@example.com",
    "-c", "commit.gpgSign=false",
    "-c", "core.hooksPath=/dev/null",
    "commit", "--quiet", "-m", message,
  ]);
  return git(repo, ["rev-parse", "HEAD"]);
}

function runGate(repo, base) {
  return run(repo, process.execPath, ["scripts/check-plugin-version-bump.mjs", base]);
}

async function withRepo(callback) {
  const repo = await mkdtemp(join(tmpdir(), "e2a-plugin-version-gate-"));
  try {
    await mkdir(join(repo, "scripts"), { recursive: true });
    await copyFile(gateSource, join(repo, "scripts", "check-plugin-version-bump.mjs"));
    git(repo, ["init", "--quiet"]);
    await callback(repo);
  } finally {
    await rm(repo, { recursive: true, force: true });
  }
}

test("maps changed files to independently versioned plugins", () => {
  assert.deepEqual(changedPlugins(["plugins/e2a/skills/e2a/SKILL.md"]), ["e2a"]);
  assert.deepEqual(changedPlugins(["plugins/e2a-labs/skills/tether/lib.sh"]), ["e2a-labs"]);
  assert.deepEqual(changedPlugins([
    "plugins/e2a/skills/e2a/SKILL.md",
    "plugins/e2a-labs/skills/tether/lib.sh",
  ]).sort(), ["e2a", "e2a-labs"]);
});

test("requires bumps only for plugins that existed in the base", () => {
  assert.deepEqual(unchangedVersions({
    changed: ["e2a", "e2a-labs"],
    baseVersions: { e2a: "0.6.0" },
    currentVersions: { e2a: "0.6.0", "e2a-labs": "0.1.0" },
  }), ["e2a"]);
});

test("a Labs-only change requires only the Labs version", () => {
  assert.deepEqual(unchangedVersions({
    changed: ["e2a-labs"],
    baseVersions: { e2a: "0.6.0", "e2a-labs": "0.1.0" },
    currentVersions: { e2a: "0.6.0", "e2a-labs": "0.1.0" },
  }), ["e2a-labs"]);
});

test("a move between package roots changes both plugins", () => {
  assert.deepEqual(changedPlugins([
    "plugins/e2a/skills/tether/SKILL.md",
    "plugins/e2a-labs/skills/tether/SKILL.md",
  ]), ["e2a", "e2a-labs"]);
});

test("reports every changed plugin whose version is unchanged", () => {
  assert.deepEqual(unchangedVersions({
    changed: ["e2a", "e2a-labs"],
    baseVersions: { e2a: "0.7.0", "e2a-labs": "0.1.0" },
    currentVersions: { e2a: "0.7.0", "e2a-labs": "0.1.0" },
  }), ["e2a", "e2a-labs"]);
});

test("compares strict SemVer 2.0.0 precedence", () => {
  const ordered = [
    "1.0.0-alpha",
    "1.0.0-alpha.1",
    "1.0.0-alpha.beta",
    "1.0.0-beta",
    "1.0.0-beta.2",
    "1.0.0-beta.11",
    "1.0.0-rc.1",
    "1.0.0",
  ];
  for (let index = 1; index < ordered.length; index += 1) {
    assert.equal(compareSemVer(ordered[index], ordered[index - 1]), 1);
  }
  assert.equal(compareSemVer("1.0.0+build.2", "1.0.0+build.1"), 0);
  assert.equal(compareSemVer("100000000000000000000.0.0", "99999999999999999999.0.0"), 1);
  for (const invalid of ["v1.0.0", "1.0", "01.0.0", "1.0.0-01", "1.0.0+", "1.0.0 ", "1.0.0\n"]) {
    assert.throws(() => compareSemVer(invalid, "1.0.0"), /invalid SemVer/);
  }
});

test("CLI treats a detected cross-root rename as changes to both plugins", async () => {
  await withRepo(async (repo) => {
    await putManifest(repo, "e2a", "0.7.0");
    await putManifest(repo, "e2a-labs", "0.1.0");
    await put(repo, "plugins/e2a/skills/moved/SKILL.md", "stable content\n");
    const base = commitAll(repo, "base");

    await mkdir(join(repo, "plugins/e2a-labs/skills/moved"), { recursive: true });
    git(repo, [
      "mv",
      "plugins/e2a/skills/moved/SKILL.md",
      "plugins/e2a-labs/skills/moved/SKILL.md",
    ]);
    assert.match(
      git(repo, ["diff", "--name-status", "--find-renames", base]),
      /^R100\tplugins\/e2a\/skills\/moved\/SKILL\.md\tplugins\/e2a-labs\/skills\/moved\/SKILL\.md$/,
    );

    const result = runGate(repo, base);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /  - e2a remains at 0\.7\.0/);
    assert.match(result.stderr, /  - e2a-labs remains at 0\.1\.0/);
  });
});

test("CLI exempts a new Labs package but still requires an existing core bump", async () => {
  await withRepo(async (repo) => {
    await putManifest(repo, "e2a", "0.6.0");
    await put(repo, "plugins/e2a/README.md", "core before\n");
    const base = commitAll(repo, "base");

    await put(repo, "plugins/e2a/README.md", "core after\n");
    await putManifest(repo, "e2a-labs", "0.1.0");
    await put(repo, "plugins/e2a-labs/README.md", "new package\n");

    const result = runGate(repo, base);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /  - e2a remains at 0\.6\.0/);
    assert.doesNotMatch(result.stderr, /e2a-labs remains/);
  });
});

test("CLI fails closed on a malformed current manifest", async () => {
  await withRepo(async (repo) => {
    await putManifest(repo, "e2a", "0.7.0");
    const base = commitAll(repo, "base");
    await put(repo, "plugins/e2a/.claude-plugin/plugin.json", "{not-json\n");

    const result = runGate(repo, base);
    assert.equal(result.status, 1);
    assert.match(
      result.stderr,
      /plugins\/e2a\/\.claude-plugin\/plugin\.json: invalid JSON/,
    );
  });
});

test("CLI fails closed when a changed plugin's current manifest is missing", async () => {
  await withRepo(async (repo) => {
    await putManifest(repo, "e2a", "0.7.0");
    const base = commitAll(repo, "base");
    await rm(join(repo, "plugins/e2a/.claude-plugin/plugin.json"));

    const result = runGate(repo, base);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /ENOENT/);
    assert.match(result.stderr, /plugins\/e2a\/\.claude-plugin\/plugin\.json/);
  });
});

test("CLI rejects an option-like base before passing it to Git", async () => {
  await withRepo(async (repo) => {
    await put(repo, "README.md", "base\n");
    commitAll(repo, "base");

    const result = runGate(repo, "--help");
    assert.equal(result.status, 2);
    assert.equal(result.stdout, "");
    assert.match(result.stderr, /base revision must not start with "-": --help/);
    assert.doesNotMatch(result.stderr, /usage: git/);
  });
});

test("CLI diagnostics list every unchanged changed plugin", async () => {
  await withRepo(async (repo) => {
    await putManifest(repo, "e2a", "0.7.0");
    await putManifest(repo, "e2a-labs", "0.1.0");
    await put(repo, "plugins/e2a/README.md", "core before\n");
    await put(repo, "plugins/e2a-labs/README.md", "labs before\n");
    const base = commitAll(repo, "base");

    await put(repo, "plugins/e2a/README.md", "core after\n");
    await put(repo, "plugins/e2a-labs/README.md", "labs after\n");

    const result = runGate(repo, base);
    assert.equal(result.status, 1);
    const reported = [...result.stderr.matchAll(/^  - (e2a(?:-labs)?) remains at/gm)]
      .map((match) => match[1])
      .sort();
    assert.deepEqual(reported, ["e2a", "e2a-labs"]);
  });
});

test("CLI rejects a version downgrade", async () => {
  await withRepo(async (repo) => {
    await putManifest(repo, "e2a", "0.7.0");
    await put(repo, "plugins/e2a/README.md", "core before\n");
    const base = commitAll(repo, "base");

    await putManifest(repo, "e2a", "0.6.9");
    await put(repo, "plugins/e2a/README.md", "core after\n");

    const result = runGate(repo, base);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /e2a.*0\.7\.0.*0\.6\.9.*SemVer precedence/is);
  });
});

test("CLI rejects a build-metadata-only version change", async () => {
  await withRepo(async (repo) => {
    await putManifest(repo, "e2a", "0.7.0+build.1");
    const base = commitAll(repo, "base");

    await putManifest(repo, "e2a", "0.7.0+build.2");

    const result = runGate(repo, base);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /e2a.*0\.7\.0\+build\.1.*0\.7\.0\+build\.2.*SemVer precedence/is);
  });
});

test("CLI rejects a lower prerelease", async () => {
  await withRepo(async (repo) => {
    await putManifest(repo, "e2a-labs", "1.0.0-beta.11");
    const base = commitAll(repo, "base");

    await putManifest(repo, "e2a-labs", "1.0.0-beta.2");

    const result = runGate(repo, base);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /e2a-labs.*1\.0\.0-beta\.11.*1\.0\.0-beta\.2.*SemVer precedence/is);
  });
});

test("CLI rejects invalid SemVer", async () => {
  await withRepo(async (repo) => {
    await putManifest(repo, "e2a", "0.7.0");
    const base = commitAll(repo, "base");

    await putManifest(repo, "e2a", "v0.8.0");

    const result = runGate(repo, base);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /e2a.*v0\.8\.0.*invalid SemVer/is);
  });
});

test("CLI accepts valid independent precedence increases", async () => {
  await withRepo(async (repo) => {
    await putManifest(repo, "e2a", "0.7.0");
    await putManifest(repo, "e2a-labs", "0.1.0-rc.1");
    const base = commitAll(repo, "base");

    await putManifest(repo, "e2a", "0.7.1");
    await putManifest(repo, "e2a-labs", "0.1.0");

    const result = runGate(repo, base);
    assert.equal(result.status, 0, result.stderr);
    const reported = result.stdout.trim().replace("Plugin version bumps are current: ", "")
      .split(", ")
      .sort();
    assert.deepEqual(reported, ["e2a", "e2a-labs"]);
  });
});

test("CLI diagnostics identify every non-increasing package", async () => {
  await withRepo(async (repo) => {
    await putManifest(repo, "e2a", "0.7.0");
    await putManifest(repo, "e2a-labs", "0.1.0+build.1");
    const base = commitAll(repo, "base");

    await putManifest(repo, "e2a", "0.6.9");
    await putManifest(repo, "e2a-labs", "0.1.0+build.2");

    const result = runGate(repo, base);
    assert.equal(result.status, 1);
    const reported = [...result.stderr.matchAll(/^  - (e2a(?:-labs)?):/gm)]
      .map((match) => match[1])
      .sort();
    assert.deepEqual(reported, ["e2a", "e2a-labs"]);
  });
});
