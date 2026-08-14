import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";

export const SOURCE =
  "https://cdn.redoc.ly/redoc/v2.5.0/bundles/redoc.standalone.js";
const TARGET = new URL(
  "../public/vendor/redoc/redoc-v2.5.0.standalone.js",
  import.meta.url,
);
export const EXPECTED_BYTES = 910994;
export const EXPECTED_SHA256 =
  "0ec05be285ac885a330289b02f470e1bdbd2b6b3223a9fa213f24bf805a851d1";
export const EXPECTED_LICENSE_BYTES = 1091;
export const EXPECTED_LICENSE_SHA256 =
  "d3026d549cf68ab7355bcfa85877bf8f845b3334a7efbfdc63936432fb34ff0e";
export const EXPECTED_NOTICES_BYTES = 2729;
export const EXPECTED_NOTICES_SHA256 =
  "1b18b986225f8a85fa7fcaf191f0118bb297ba8c6f027b669e4b79828c9c17ed";
const MANIFEST = new URL("../public/vendor/redoc/manifest.json", import.meta.url);
const LICENSE = new URL("../public/vendor/redoc/LICENSE.txt", import.meta.url);
const NOTICES = new URL(
  "../public/vendor/redoc/redoc.standalone.js.LICENSE.txt",
  import.meta.url,
);

const EXPECTED_MANIFEST = {
  name: "Redoc standalone",
  version: "2.5.0",
  upstreamCommit: "00bc6edfc42c9cec9e453a2af4a8f5cef5e033ca",
  source: SOURCE,
  upstream: "https://github.com/Redocly/redoc/releases/tag/v2.5.0",
  sha256: EXPECTED_SHA256,
  bytes: EXPECTED_BYTES,
  license: "MIT",
  notices: "redoc.standalone.js.LICENSE.txt",
  noticesSha256: EXPECTED_NOTICES_SHA256,
  noticesBytes: EXPECTED_NOTICES_BYTES,
};

function sha256(contents) {
  return createHash("sha256").update(contents).digest("hex");
}

function assertManifest(contents) {
  const manifest = JSON.parse(contents);
  if (JSON.stringify(manifest) !== JSON.stringify(EXPECTED_MANIFEST)) {
    throw new Error("Redoc manifest does not match the reviewed artifact metadata");
  }
}

function assertLicense(contents) {
  if (contents.length !== EXPECTED_LICENSE_BYTES) {
    throw new Error(
      `Redoc MIT license has ${contents.length} bytes; expected ${EXPECTED_LICENSE_BYTES}`,
    );
  }
  if (sha256(contents) !== EXPECTED_LICENSE_SHA256) {
    throw new Error("Redoc MIT license SHA-256 does not match upstream v2.5.0");
  }
}

function assertBundle(contents) {
  if (contents.length !== EXPECTED_BYTES) {
    throw new Error(
      `Redoc bundle has ${contents.length} bytes; expected ${EXPECTED_BYTES}`,
    );
  }
  if (sha256(contents) !== EXPECTED_SHA256) {
    throw new Error("Redoc bundle SHA-256 does not match the reviewed artifact");
  }
}

function assertNotices(contents) {
  if (contents.length !== EXPECTED_NOTICES_BYTES) {
    throw new Error(
      `Redoc third-party notices have ${contents.length} bytes; expected ${EXPECTED_NOTICES_BYTES}`,
    );
  }
  if (sha256(contents) !== EXPECTED_NOTICES_SHA256) {
    throw new Error(
      "Redoc third-party notices SHA-256 does not match the v2.5.0 bundle companion",
    );
  }
}

async function verifySupportingFiles() {
  assertManifest(await readFile(MANIFEST, "utf8"));
  assertLicense(await readFile(LICENSE));
  assertNotices(await readFile(NOTICES));
}

async function check() {
  await verifySupportingFiles();
  assertBundle(await readFile(TARGET));
}

export async function update({ fetchImpl = fetch, target = TARGET } = {}) {
  await verifySupportingFiles();

  const response = await fetchImpl(SOURCE, { redirect: "error" });
  if (response.status !== 200) {
    throw new Error(`Redoc bundle download returned HTTP ${response.status}`);
  }

  const bundle = Buffer.from(await response.arrayBuffer());
  assertBundle(bundle);

  await mkdir(new URL(".", target), { recursive: true });
  await writeFile(target, bundle);
}

async function main(command) {
  if (command !== "--check" && command !== "--update") {
    console.error("Usage: node scripts/vendor-redoc.mjs --check|--update");
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
