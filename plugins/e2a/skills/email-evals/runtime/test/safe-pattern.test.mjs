import assert from "node:assert/strict";
import test from "node:test";
import { compileSafePattern } from "../lib/safe-pattern.mjs";

test("safe patterns match and replace globally", () => {
  const pattern = compileSafePattern("secret-[0-9]+");
  assert.equal(pattern.test("a secret-12 b"), true);
  assert.equal(pattern.replaceAll("secret-1 secret-2", "[REDACTED]"), "[REDACTED] [REDACTED]");
});

test("safe patterns match and replace Unicode and astral text", () => {
  const pattern = compileSafePattern("(?:秘密|🔐)-[0-9]+");
  assert.equal(pattern.test("前缀 秘密-12 后缀"), true);
  assert.equal(pattern.test("safe 🔐-34 text"), true);
  assert.equal(
    pattern.replaceAll("秘密-12 and 🔐-34", "[REDACTED]"),
    "[REDACTED] and [REDACTED]",
  );
});

test("safe patterns support inline multiline anchors for message text", () => {
  const pattern = compileSafePattern("(?m)^X-Synthetic-Token: [^\\r\\n]+$");
  const headers = "Subject: Fixture\nX-Synthetic-Token: first\nBody\nX-Synthetic-Token: second";
  assert.equal(pattern.test(headers), true);
  assert.equal(
    pattern.replaceAll(headers, "X-Synthetic-Token: [REDACTED]"),
    "Subject: Fixture\nX-Synthetic-Token: [REDACTED]\nBody\nX-Synthetic-Token: [REDACTED]",
  );
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
