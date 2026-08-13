import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";

export const SOURCE = "https://umami.tokencanopy.com/script.js";
const TARGET = new URL("../public/vendor/umami/umami-v3.2.0.1ad1145d.js", import.meta.url);
export const EXPECTED_BYTES = 4655;
export const EXPECTED_SHA256 = "1ad1145d19d4558c20f5469ca4a5fc50a1a46f860858c9c91bfcd56fd29a522a";
export const EXPECTED_LICENSE_BYTES = 1093;
export const EXPECTED_LICENSE_SHA256 = "d59f69c3a56253a150adfc42b41acd49e9024410ad087c58a23d506623258dfe";
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
  if (contents.length !== EXPECTED_LICENSE_BYTES) {
    throw new Error(
      `Umami MIT license has ${contents.length} bytes; expected ${EXPECTED_LICENSE_BYTES}`,
    );
  }
  if (sha256(contents) !== EXPECTED_LICENSE_SHA256) {
    throw new Error("Umami MIT license SHA-256 does not match upstream v3.2.0");
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
  assertLicense(await readFile(LICENSE));
}

async function check() {
  await verifySupportingFiles();
  assertTracker(await readFile(TARGET));
}

export async function update({ fetchImpl = fetch, target = TARGET } = {}) {
  await verifySupportingFiles();

  const response = await fetchImpl(SOURCE, { redirect: "error" });
  if (response.status !== 200) {
    throw new Error(`Umami tracker download returned HTTP ${response.status}`);
  }

  const tracker = Buffer.from(await response.arrayBuffer());
  assertTracker(tracker);

  await mkdir(new URL(".", target), { recursive: true });
  await writeFile(target, tracker);
}

async function main(command) {
  if (command !== "--check" && command !== "--update") {
    console.error("Usage: node scripts/vendor-umami-tracker.mjs --check|--update");
    process.exitCode = 1;
    return;
  }

  try {
    await (command === "--check" ? check() : update());
  } catch (error) {
    console.error(error instanceof Error ? error.message : error);
    process.exitCode = 1;
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  await main(process.argv[2]);
}
