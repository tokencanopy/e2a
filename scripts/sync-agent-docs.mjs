#!/usr/bin/env node

import { mkdir, readFile, writeFile } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const AGENT_DOC_MIRRORS = [
  ["plugins/e2a/docs/setup.md", "web/public/setup.md"],
  ["plugins/e2a/docs/auth.md", "web/public/auth.md"],
  ["plugins/e2a/docs/sdk.md", "web/public/sdk.md"],
  ["plugins/e2a/docs/templates.md", "web/public/templates.md"],
  ["plugins/e2a/docs/llms.txt", "web/public/llms.txt"],
];

// llms.txt is an index: it tells a model which documents exist and where to
// fetch them. That costs a retrieval round trip the model may not take, and
// some crawlers never follow the links at all. llms-full.txt is the whole
// corpus inlined so a single fetch is enough to answer from.
//
// Derived, not mirrored — it has no canonical source file of its own, so it is
// built here and committed like the mirrors, which keeps `--check` honest.
export const LLMS_FULL_TARGET = "web/public/llms-full.txt";

export const LLMS_FULL_SOURCES = [
  ["plugins/e2a/docs/setup.md", "https://e2a.dev/setup.md"],
  ["plugins/e2a/docs/auth.md", "https://e2a.dev/auth.md"],
  ["plugins/e2a/docs/sdk.md", "https://e2a.dev/sdk.md"],
  ["plugins/e2a/docs/templates.md", "https://e2a.dev/templates.md"],
];

const LLMS_FULL_HEADER = `# e2a — full documentation

> e2a is an authenticated email gateway for AI agents: it gives an agent its
> own verified email inbox to send, receive, reply, and forward, with SPF/DKIM
> verification on inbound. Connect over a hosted MCP server (OAuth 2.1, no API
> key), the REST API, or the TypeScript/Python SDKs.

This file inlines the full e2a documentation set so it can be read in one
fetch. The per-document originals are linked above each section, and the
complete API contract is at https://e2a.dev/v1/openapi.yaml. Source:
https://github.com/tokencanopy/e2a (Apache-2.0).
`;

/** Builds the llms-full.txt body from the canonical docs. */
export function composeLlmsFull(documents) {
  const sections = documents.map(
    ({ url, body }) => `<!-- source: ${url} -->\n\n${body.trim()}\n`,
  );
  return `${LLMS_FULL_HEADER}\n---\n\n${sections.join("\n---\n\n")}`;
}

const usage = "usage: node scripts/sync-agent-docs.mjs [--check]";

export function parseArgs(args) {
  if (args.length === 0) return { check: false };
  if (args.length === 1 && args[0] === "--check") return { check: true };
  if (args.length === 1) throw new Error(`unknown option: ${args[0]}\n${usage}`);
  throw new Error(usage);
}

async function readCanonical(repoRoot, source) {
  try {
    return await readFile(join(repoRoot, source));
  } catch (error) {
    if (error?.code === "ENOENT") {
      throw new Error(`missing canonical agent doc: ${source}`);
    }
    throw error;
  }
}

/**
 * Writes `expected` to `target`, or records a mismatch when checking.
 * Returns a mismatch string, or null when the target is already correct.
 */
async function reconcile({ repoRoot, target, expected, check, describe, log }) {
  let existing;
  try {
    existing = await readFile(join(repoRoot, target));
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
  }

  if (existing?.equals(expected)) return null;

  if (check) {
    return `${existing === undefined ? "missing" : "stale"} ${describe}: ${target}`;
  }

  const targetPath = join(repoRoot, target);
  await mkdir(dirname(targetPath), { recursive: true });
  await writeFile(targetPath, expected);
  log(`wrote ${target}`);
  return null;
}

export async function syncAgentDocs({ repoRoot, check, log = console.log }) {
  const mismatches = [];

  for (const [source, target] of AGENT_DOC_MIRRORS) {
    const canonical = await readCanonical(repoRoot, source);
    const mismatch = await reconcile({
      repoRoot,
      target,
      expected: canonical,
      check,
      describe: "hosted agent doc",
      log: () => log(`synced ${source} -> ${target}`),
    });
    if (mismatch) mismatches.push(mismatch);
  }

  const documents = [];
  for (const [source, url] of LLMS_FULL_SOURCES) {
    documents.push({
      url,
      body: (await readCanonical(repoRoot, source)).toString("utf8"),
    });
  }
  const llmsFull = await reconcile({
    repoRoot,
    target: LLMS_FULL_TARGET,
    expected: Buffer.from(composeLlmsFull(documents), "utf8"),
    check,
    describe: "generated corpus",
    log,
  });
  if (llmsFull) mismatches.push(llmsFull);

  if (mismatches.length > 0) {
    throw new Error(mismatches.join("\n"));
  }
}

const scriptPath = fileURLToPath(import.meta.url);
const isMain = process.argv[1] && resolve(process.argv[1]) === scriptPath;

if (isMain) {
  try {
    const options = parseArgs(process.argv.slice(2));
    const repoRoot = resolve(dirname(scriptPath), "..");
    await syncAgentDocs({ repoRoot, ...options });
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  }
}
