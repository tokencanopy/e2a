import assert from "node:assert/strict";
import { test } from "node:test";
import { changedPlugins, unchangedVersions } from "./check-plugin-version-bump.mjs";

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
