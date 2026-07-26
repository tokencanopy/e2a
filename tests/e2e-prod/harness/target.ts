import { writeFileSync, mkdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { isProductionTarget } from "./env.ts";

// Target-resolution recorder — the un-fudgeable signal event_coverage_gate.py
// (and coverage_gate.py, for deleteSuppression) use to decide whether a run
// requires or merely allowlists the handful of event types / operations only
// production's real infrastructure (SES delivery feedback, a real external
// MX) can produce.
//
// Deliberately NOT an operator-supplied flag (an env var or CLI switch could
// be set wrongly, independent of what the suite actually talked to) — it's
// derived from the SAME apiUrl every request in the process actually used,
// through the SAME hostname allowlist (env.ts's isProductionTarget) that
// gates the destructive prod opt-in in the first place. If the harness
// talked to production, this says so; if it didn't, it can't.
//
// Mirrors harness/coverage.ts's per-pid shard pattern: `node --test` runs
// each suite *.test.ts file in its own process, so one shard per process
// unions cleanly across a whole run. Written once per process (guarded by
// `recorded`), at ApiClient construction time — every suite file (staging
// and prod-only alike) constructs one, so a shard exists whenever any suite
// ran. The `pretest`/`pretest:prod` steps clear this directory first, so a
// stale shard from a prior run's target can never leak forward and disguise
// a later run's actual environment.
const TARGET_DIR = fileURLToPath(new URL("../reports/target/", import.meta.url));

let recorded = false;

export function recordTarget(apiUrl: string): void {
  if (recorded) return;
  recorded = true;
  try {
    mkdirSync(TARGET_DIR, { recursive: true });
    const payload = { apiUrl, isProd: isProductionTarget(apiUrl) };
    writeFileSync(`${TARGET_DIR}${process.pid}.json`, JSON.stringify(payload));
  } catch {
    /* best-effort: target resolution is advisory to the gate, must never fail a suite */
  }
}
