import assert from "node:assert/strict";
import test from "node:test";
import { compileSafePattern } from "../lib/safe-pattern.mjs";

test("safe patterns match and replace globally", () => {
  const pattern = compileSafePattern("secret-[0-9]+");
  assert.equal(pattern.test("a secret-12 b"), true);
  assert.equal(pattern.replaceAll("secret-1 secret-2", "[REDACTED]"), "[REDACTED] [REDACTED]");
});

test("safe patterns reject unsupported backtracking features", () => {
  assert.throws(() => compileSafePattern("(a+)\\1"), /RE2-compatible/);
  assert.throws(() => compileSafePattern("(?<=token-)value"), /RE2-compatible/);
});

test("nested quantifiers complete without catastrophic backtracking", () => {
  const started = performance.now();
  const pattern = compileSafePattern("(a+)+$");
  assert.equal(pattern.test(`${"a".repeat(200_000)}!`), false);
  assert.ok(performance.now() - started < 1_000);
});
