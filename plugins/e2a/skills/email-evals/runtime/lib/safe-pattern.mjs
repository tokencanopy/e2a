import { RE2JS } from "re2js";

export class SafePatternSyntaxError extends Error {}

export function compileSafePattern(source) {
  let compiled;
  try {
    compiled = RE2JS.compile(source);
  } catch {
    throw new SafePatternSyntaxError(
      "Regular expression must use RE2-compatible syntax",
    );
  }
  return Object.freeze({
    source,
    test(value) { return compiled.test(String(value)); },
    replaceAll(value, replacement) {
      return compiled
        .matcher(String(value))
        .replaceAll(RE2JS.quoteReplacement(String(replacement)));
    },
  });
}
