import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";

const SOURCE = "https://umami.tokencanopy.com/script.js";
const TARGET = new URL("../public/vendor/umami/umami-v3.2.0.1ad1145d.js", import.meta.url);
const EXPECTED_BYTES = 4655;
const EXPECTED_SHA256 = "1ad1145d19d4558c20f5469ca4a5fc50a1a46f860858c9c91bfcd56fd29a522a";
const MANIFEST = new URL("../public/vendor/umami/manifest.json", import.meta.url);
const LICENSE = new URL("../public/vendor/umami/LICENSE.txt", import.meta.url);

const EXPECTED_MANIFEST = {
  name: "Umami tracker",
  version: "3.2.0",
  upstreamCommit: "2f6e2b5",
  source: SOURCE,
  upstream: "https://github.com/umami-software/umami/releases/tag/v3.2.0",
  sha256: EXPECTED_SHA256,
  bytes: EXPECTED_BYTES,
  license: "MIT",
};

function sha256(contents) {
  return createHash("sha256").update(contents).digest("hex");
}

function assertManifest(contents) {
  const manifest = JSON.parse(contents);
  if (JSON.stringify(manifest) !== JSON.stringify(EXPECTED_MANIFEST)) {
    throw new Error("Umami manifest does not match the reviewed artifact metadata");
  }
}

function assertLicense(contents) {
  if (
    !contents.startsWith("MIT License\n") ||
    !contents.includes("Copyright (c) 2022 Umami Software, Inc.")
  ) {
    throw new Error("Umami MIT license is missing or invalid");
  }
}

function assertTracker(contents) {
  if (contents.length !== EXPECTED_BYTES) {
    throw new Error(
      `Umami tracker has ${contents.length} bytes; expected ${EXPECTED_BYTES}`,
    );
  }
  if (sha256(contents) !== EXPECTED_SHA256) {
    throw new Error("Umami tracker SHA-256 does not match the reviewed artifact");
  }
}

async function verifySupportingFiles() {
  assertManifest(await readFile(MANIFEST, "utf8"));
  assertLicense(await readFile(LICENSE, "utf8"));
}

async function check() {
  await verifySupportingFiles();
  assertTracker(await readFile(TARGET));
}

async function update() {
  await verifySupportingFiles();

  const response = await fetch(SOURCE, { redirect: "error" });
  if (response.status !== 200) {
    throw new Error(`Umami tracker download returned HTTP ${response.status}`);
  }

  const tracker = Buffer.from(await response.arrayBuffer());
  assertTracker(tracker);

  await mkdir(new URL(".", TARGET), { recursive: true });
  await writeFile(TARGET, tracker);
}

const command = process.argv[2];
if (command !== "--check" && command !== "--update") {
  console.error("Usage: node scripts/vendor-umami-tracker.mjs --check|--update");
  process.exitCode = 1;
} else {
  try {
    await (command === "--check" ? check() : update());
  } catch (error) {
    console.error(error instanceof Error ? error.message : error);
    process.exitCode = 1;
  }
}
