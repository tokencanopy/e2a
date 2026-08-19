import { readFileSync, readdirSync, writeFileSync } from "node:fs";
import { basename, join } from "node:path";

interface Finding {
  severity: "info" | "warn" | "fail";
  suite: string;
  test: string;
  message: string;
}
interface Report {
  timestamp: string;
  counts: { info: number; warn: number; fail: number };
  findings: Finding[];
}

const dir = "./reports";

// The scan MUST be recursive. Suite reports land at reports/<suite>.json, but a
// suite whose SUITE constant contains a slash lands in a SUBDIRECTORY, because
// writeReport() does mkdirSync(dirname(path), { recursive: true }). Every suite
// in suites/prod/ sets SUITE = "prod/NN-name", so all of them write to
// reports/prod/*.json — and the original non-recursive readdirSync never saw a
// single one. Every fail() in every prod-only suite was therefore decorative:
// precisely the failure mode the gate at the bottom of this file exists to
// prevent, happening to the suites that cover what staging structurally cannot
// (real inbound MX, SES delivery feedback, sender identity).
//
// The scan must ALSO be shape-aware. reports/ holds five other shard families —
// coverage/, event-coverage/, mcp-coverage/, response-samples/, target/ —
// written for the Python gates with entirely different shapes. Recursing without
// a shape check would throw on r.findings, or worse, silently spread garbage.
function isSuiteReport(value: unknown): value is Report {
  const r = value as Report | null;
  return (
    !!r &&
    Array.isArray(r.findings) &&
    !!r.counts &&
    typeof r.counts.fail === "number"
  );
}

const candidates = readdirSync(dir, { recursive: true, encoding: "utf-8" }).filter(
  (f) => f.endsWith(".json") && basename(f) !== "consolidated.json",
);

const all: Finding[] = [];
const suiteFiles: string[] = [];
const skipped: string[] = [];
for (const f of candidates) {
  const parsed: unknown = JSON.parse(readFileSync(join(dir, f), "utf-8"));
  if (!isSuiteReport(parsed)) {
    skipped.push(f);
    continue;
  }
  suiteFiles.push(f);
  all.push(...parsed.findings);
}

// Report what was skipped rather than dropping it silently — a suite report that
// stops matching the shape must not vanish the way reports/prod/ did.
if (skipped.length > 0) {
  console.log(`Skipped ${skipped.length} non-suite-report JSON file(s) (other gates' shards):`);
  for (const f of skipped) console.log(`  - ${f}`);
  console.log("");
}

// Fail-closed on no data. "Nothing to consolidate" must never read as "green":
// that is indistinguishable from the bug this change fixes, and it is the same
// discipline the Python coverage gates apply when they find no shards.
if (suiteFiles.length === 0) {
  console.error(
    `consolidate: no suite reports found under ${dir}/ — refusing to report success. ` +
      `Either no suite ran, or writeReport() output moved again.`,
  );
  process.exit(2);
}

const bySeverity = {
  fail: all.filter((f) => f.severity === "fail"),
  warn: all.filter((f) => f.severity === "warn"),
  info: all.filter((f) => f.severity === "info"),
};

const out = {
  timestamp: new Date().toISOString(),
  total_findings: all.length,
  counts: { fail: bySeverity.fail.length, warn: bySeverity.warn.length, info: bySeverity.info.length },
  fail: bySeverity.fail,
  warn: bySeverity.warn,
  info: bySeverity.info,
};
writeFileSync(join(dir, "consolidated.json"), JSON.stringify(out, null, 2));

console.log(`Consolidated ${all.length} findings from ${suiteFiles.length} suite report(s):`);
console.log(`  FAIL: ${bySeverity.fail.length}`);
console.log(`  WARN: ${bySeverity.warn.length}`);
console.log(`  INFO: ${bySeverity.info.length}`);
console.log("");
if (bySeverity.fail.length > 0) {
  console.log("=== FAIL ===");
  for (const f of bySeverity.fail) console.log(`  [${f.suite}] ${f.test}: ${f.message}`);
  console.log("");
}
if (bySeverity.warn.length > 0) {
  console.log("=== WARN ===");
  for (const f of bySeverity.warn) console.log(`  [${f.suite}] ${f.test}: ${f.message}`);
  console.log("");
}
console.log("=== INFO (highlights) ===");
for (const f of bySeverity.info) console.log(`  [${f.suite}] ${f.test}: ${f.message}`);

// Gate: any recorded FAIL finding fails the run. Suites deliberately record
// several findings and keep going (fail() must not throw mid-test), so the
// exit-code decision lives here at consolidation — the documented design is
// "the gate MUST read the JSON". Without this, every fail() in every suite is
// decorative: a pipeline that runs the suites and never consolidates (or
// consolidates but ignores the exit code) reports green over real failures.
if (bySeverity.fail.length > 0) {
  console.error(`\nconsolidate: ${bySeverity.fail.length} FAIL finding(s) recorded — failing the run.`);
  process.exit(1);
}
