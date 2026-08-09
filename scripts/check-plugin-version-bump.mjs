#!/usr/bin/env node

import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const REPO_ROOT = fileURLToPath(new URL("../", import.meta.url));
const IDENTIFIER = String.raw`(?:0|[1-9]\d*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)`;
const SEMVER_RE = new RegExp(
  String.raw`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)`
    + String.raw`(?:-(${IDENTIFIER}(?:\.${IDENTIFIER})*))?`
    + String.raw`(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`,
);

function parseSemVer(version, label = "version") {
  if (typeof version !== "string") {
    throw new Error(`${label} ${JSON.stringify(version)} is invalid SemVer`);
  }
  const match = version.match(SEMVER_RE);
  if (!match) throw new Error(`${label} "${version}" is invalid SemVer`);
  return {
    major: BigInt(match[1]),
    minor: BigInt(match[2]),
    patch: BigInt(match[3]),
    prerelease: match[4]?.split(".").map((identifier) => ({
      numeric: /^\d+$/.test(identifier),
      value: /^\d+$/.test(identifier) ? BigInt(identifier) : identifier,
    })) ?? null,
  };
}

function compareParsedSemVer(left, right) {
  for (const field of ["major", "minor", "patch"]) {
    if (left[field] > right[field]) return 1;
    if (left[field] < right[field]) return -1;
  }
  if (left.prerelease === null && right.prerelease === null) return 0;
  if (left.prerelease === null) return 1;
  if (right.prerelease === null) return -1;
  const length = Math.max(left.prerelease.length, right.prerelease.length);
  for (let index = 0; index < length; index += 1) {
    const leftIdentifier = left.prerelease[index];
    const rightIdentifier = right.prerelease[index];
    if (leftIdentifier === undefined) return -1;
    if (rightIdentifier === undefined) return 1;
    if (leftIdentifier.numeric && !rightIdentifier.numeric) return -1;
    if (!leftIdentifier.numeric && rightIdentifier.numeric) return 1;
    if (leftIdentifier.value > rightIdentifier.value) return 1;
    if (leftIdentifier.value < rightIdentifier.value) return -1;
  }
  return 0;
}

export function compareSemVer(left, right) {
  return compareParsedSemVer(parseSemVer(left), parseSemVer(right));
}

export function changedPlugins(paths) {
  return [...new Set(paths.flatMap((path) =>
    path.startsWith("plugins/e2a-labs/") ? ["e2a-labs"]
      : path.startsWith("plugins/e2a/") ? ["e2a"] : [],
  ))];
}

export function unchangedVersions({ changed, baseVersions, currentVersions }) {
  return changed.filter((name) => {
    if (baseVersions[name] === undefined) return false;
    const current = parseSemVer(currentVersions[name], `${name}: current version`);
    const base = parseSemVer(baseVersions[name], `${name}: base version`);
    return compareParsedSemVer(current, base) <= 0;
  });
}

function manifestPath(name) {
  return `plugins/${name}/.claude-plugin/plugin.json`;
}

function git(args) {
  const result = spawnSync("git", args, {
    cwd: REPO_ROOT,
    encoding: "utf8",
    maxBuffer: 10 * 1024 * 1024,
  });
  if (result.error) throw result.error;
  if (result.status !== 0) {
    const detail = result.stderr.trim() || `git ${args[0]} exited ${result.status}`;
    throw new Error(detail);
  }
  return result;
}

function readVersion(label, raw) {
  let manifest;
  try {
    manifest = JSON.parse(raw);
  } catch (error) {
    const detail = error instanceof Error ? error.message : String(error);
    throw new Error(`${label}: invalid JSON (${detail})`);
  }
  if (typeof manifest.version !== "string" || manifest.version.length === 0) {
    throw new Error(`${label}: missing string "version"`);
  }
  return manifest.version;
}

function readBaseVersion(base, name) {
  const path = manifestPath(name);
  const object = `${base}:${path}`;
  const matchingPath = git(["ls-tree", "--name-only", base, "--", path]).stdout.trim();
  if (matchingPath === "") return undefined;
  if (matchingPath !== path) {
    throw new Error(`${base}: unexpected manifest path returned for ${path}: ${matchingPath}`);
  }
  return readVersion(`${object}`, git(["show", object]).stdout);
}

function readCurrentVersion(name) {
  const path = manifestPath(name);
  return readVersion(path, readFileSync(resolve(REPO_ROOT, path), "utf8"));
}

function main(args) {
  if (args.length !== 1) {
    console.error("usage: node scripts/check-plugin-version-bump.mjs <base-revision>");
    process.exitCode = 2;
    return;
  }

  const [base] = args;
  if (base.startsWith("-")) {
    console.error(`base revision must not start with "-": ${base}`);
    process.exitCode = 2;
    return;
  }
  try {
    const verifiedBase = git([
      "rev-parse", "--verify", "--end-of-options", `${base}^{commit}`,
    ]).stdout.trim();
    if (!/^[0-9a-f]{40,64}$/.test(verifiedBase)) {
      throw new Error(`could not resolve base revision to a commit: ${base}`);
    }
    // Disabling rename detection guarantees a move between the independently
    // versioned package roots reports both its deleted and added paths.
    const changed = changedPlugins(
      git(["diff", "--name-only", "--no-renames", "-z", verifiedBase, "--", "plugins/e2a", "plugins/e2a-labs"])
        .stdout.split("\0")
        .filter(Boolean),
    );
    const baseVersions = {};
    const currentVersions = {};
    for (const name of changed) {
      baseVersions[name] = readBaseVersion(verifiedBase, name);
      currentVersions[name] = readCurrentVersion(name);
    }

    const unchanged = unchangedVersions({ changed, baseVersions, currentVersions });
    if (unchanged.length > 0) {
      console.error("Changed plugins must bump their versions:");
      for (const name of unchanged) {
        if (baseVersions[name] === currentVersions[name]) {
          console.error(`  - ${name} remains at ${currentVersions[name]}`);
        } else {
          console.error(`  - ${name}: ${baseVersions[name]} -> ${currentVersions[name]} does not increase SemVer precedence`);
        }
      }
      process.exitCode = 1;
      return;
    }

    if (changed.length === 0) {
      console.log("No plugin package changes detected.");
      return;
    }
    console.log(`Plugin version bumps are current: ${changed.join(", ")}`);
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1])) {
  main(process.argv.slice(2));
}
