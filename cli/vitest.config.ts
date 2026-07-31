import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // test/harness holds the offline unit tests for the e2e coverage harness
    // itself (e.g. the --help parser); the live binary-spawn suites under
    // test/*.test.ts stay on vitest.e2e.config.ts.
    include: ["src/**/*.test.ts", "test/harness/*.test.ts"],
    coverage: {
      provider: "v8",
      include: ["src/**/*.ts"],
      exclude: ["src/**/*.test.ts"],
      // Floors sit a few points under current coverage (lines 77.5,
      // statements 77.3, functions 77.9, branches 72.2). Ratchet up, never down.
      thresholds: {
        lines: 73,
        statements: 73,
        functions: 73,
        branches: 68,
      },
    },
  },
});
