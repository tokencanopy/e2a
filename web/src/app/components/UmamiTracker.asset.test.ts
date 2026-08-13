import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const umamiDirectory = join(
  process.cwd(),
  "public",
  "vendor",
  "umami",
);

describe("vendored Umami tracker", () => {
  it("pins the reviewed tracker bytes with its upstream MIT license", () => {
    const manifest = JSON.parse(
      readFileSync(join(umamiDirectory, "manifest.json"), "utf8"),
    ) as {
      bytes: number;
      license: string;
      name: string;
      sha256: string;
      source: string;
      upstream: string;
      upstreamCommit: string;
      version: string;
    };
    const tracker = readFileSync(
      join(umamiDirectory, "umami-v3.2.0.1ad1145d.js"),
    );
    const license = readFileSync(join(umamiDirectory, "LICENSE.txt"));

    expect(manifest).toMatchObject({
      bytes: 4655,
      license: "MIT",
      name: "Umami tracker",
      sha256: "1ad1145d19d4558c20f5469ca4a5fc50a1a46f860858c9c91bfcd56fd29a522a",
      source: "https://umami.tokencanopy.com/script.js",
      upstream: "https://github.com/umami-software/umami/releases/tag/v3.2.0",
      upstreamCommit: "2f6e2b5",
      version: "3.2.0",
    });
    expect(tracker).toHaveLength(4655);
    expect(createHash("sha256").update(tracker).digest("hex")).toBe(
      "1ad1145d19d4558c20f5469ca4a5fc50a1a46f860858c9c91bfcd56fd29a522a",
    );
    expect(license).toHaveLength(1093);
    expect(createHash("sha256").update(license).digest("hex")).toBe(
      "d59f69c3a56253a150adfc42b41acd49e9024410ad087c58a23d506623258dfe",
    );
    expect(license.toString("utf8").startsWith("MIT License")).toBe(true);
    expect(license.toString("utf8")).toContain(
      "Copyright (c) 2022 Umami Software, Inc.",
    );
  });
});
