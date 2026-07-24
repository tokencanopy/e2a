import type { Config } from "jest";

const config: Config = {
  testEnvironment: "jsdom",
  setupFiles: ["<rootDir>/jest.env.ts"],
  setupFilesAfterEnv: ["<rootDir>/jest.setup.ts"],
  testPathIgnorePatterns: ["<rootDir>/node_modules/", "<rootDir>/.next/"],
  moduleNameMapper: {
    "^@/(.*)$": "<rootDir>/src/$1",
    // @e2a/ui ships ESM-only; resolve it to its TS source so ts-jest transforms
    // it like the rest of the suite (jest is CJS and can't load the ESM dist).
    "^@e2a/ui$": "<rootDir>/../design-system/src/index.ts",
    // ...but the source then resolves react from design-system/node_modules — a
    // SECOND copy → null hook dispatcher. Pin react to web's single copy.
    "^react$": "<rootDir>/node_modules/react",
    "^react/(.*)$": "<rootDir>/node_modules/react/$1",
  },
  transform: {
    "^.+\\.tsx?$": [
      "ts-jest",
      {
        tsconfig: "tsconfig.json",
        jsx: "react-jsx",
      },
    ],
  },
  // Coverage gate (applies only when jest runs with --coverage, i.e.
  // `npm run test:coverage` in CI). Floors sit ~2 points under current
  // coverage (statements 87.5, branches 80.2, functions 85.3, lines 89.7).
  // Ratchet up, never down.
  coverageThreshold: {
    global: {
      statements: 85,
      branches: 78,
      functions: 83,
      lines: 87,
    },
  },
};

export default config;
