#!/usr/bin/env node

import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const REPO_ROOT = fileURLToPath(new URL("../", import.meta.url));

export function changedPlugins(paths) {
  return [...new Set(paths.flatMap((path) =>
    path.startsWith("plugins/e2a-labs/") ? ["e2a-labs"]
      : path.startsWith("plugins/e2a/") ? ["e2a"] : [],
  ))];
}

export function unchangedVersions({ changed, baseVersions, currentVersions }) {
  return changed.filter((name) =>
    baseVersions[name] !== undefined && baseVersions[name] === currentVersions[name],
  );
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
        console.error(`  - ${name} remains at ${currentVersions[name]}`);
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
