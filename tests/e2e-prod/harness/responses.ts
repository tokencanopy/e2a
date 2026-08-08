import { writeFileSync, mkdirSync } from "node:fs";
import { fileURLToPath } from "node:url";

// Response-sample recorder — the raw material for response_schema_gate.py.
//
// The suites assert hand-picked body fields; nothing here (or anywhere in the
// zero-dependency suite) validates a live response against the OpenAPI response
// schemas. Doing that in-process would need a JSON-Schema validator, i.e. an
// npm dependency the suite deliberately does not have — so the split is: the
// SUITE RECORDS, the GATE VALIDATES. This module captures every ApiClient
// response as an (method, path, status, body) sample; the Python gate maps each
// sample to its operationId, resolves the spec's response schema, and validates
// there (where CI already installs Python deps for coverage_gate.py).
//
// Unlike harness/coverage.ts this records ALL statuses, not just 2xx — the
// spec's per-op `default` response documents the error envelope, so every 401
// auth-probe and 422 validation-rejection the suites already provoke is a free
// conformance check on the error contract. The 2xx-only filter over in
// coverage.ts is about not over-claiming COVERAGE; it stays untouched — this is
// a separate channel with a separate question ("was the body well-shaped?",
// not "did the op run?").
//
// Sample kinds:
//   json     — body parsed as JSON; recorded verbatim for validation.
//   empty    — zero-length body. The gate decides whether that's legal for the
//              documented response (in today's spec it never is: every response
//              documents application/json content).
//   nonjson  — non-empty body that failed JSON.parse; only a bounded prefix is
//              recorded, for diagnosis. Always suspicious against this spec.
//   oversized — body above SAMPLE_MAX_BYTES; recorded without the body so one
//              huge attachment payload can't bloat the shard. The gate counts
//              these as skipped, never as passes.
//
// Shards live in reports/response-samples/ (own SUBDIR, same reasoning as
// reports/coverage/: consolidate.ts reads reports/*.json as {findings} objects
// and would choke on a bare array). One shard per suite-file process, flushed
// on 'exit' (same guarantees and same safe-false-FAIL SIGKILL caveat as
// coverage.ts); `pretest` clears the dir so a prior run's samples can't leak
// into a later run's verdict.
const SAMPLES_DIR = fileURLToPath(new URL("../reports/response-samples/", import.meta.url));

// Bound on the RAW body size we embed in a shard. Big enough for every real
// list/object response the suites see; small enough that an attachment-data
// body can't turn a shard into a multi-megabyte artifact.
const SAMPLE_MAX_BYTES = 262144; // 256 KiB

export interface ResponseSample {
  method: string;
  path: string;
  status: number;
  contentType: string;
  kind: "json" | "empty" | "nonjson" | "oversized";
  body?: unknown; // present only for kind "json"
  rawPrefix?: string; // present only for kind "nonjson", bounded
  rawLength?: number; // present for "nonjson" and "oversized"
}

const samples: ResponseSample[] = [];
let installed = false;

export function recordResponse(
  method: string,
  pathname: string,
  status: number,
  parsedBody: unknown,
  raw: string,
  contentType: string,
): void {
  const base = { method: method.toUpperCase(), path: pathname, status, contentType };
  if (raw.length === 0) {
    samples.push({ ...base, kind: "empty" });
  } else if (raw.length > SAMPLE_MAX_BYTES) {
    samples.push({ ...base, kind: "oversized", rawLength: raw.length });
  } else if (parsedBody === null && !isJsonNull(raw)) {
    samples.push({ ...base, kind: "nonjson", rawPrefix: raw.slice(0, 200), rawLength: raw.length });
  } else {
    samples.push({ ...base, kind: "json", body: parsedBody });
  }
  if (!installed) {
    installed = true;
    process.on("exit", () => {
      if (samples.length === 0) return;
      try {
        mkdirSync(SAMPLES_DIR, { recursive: true });
        writeFileSync(`${SAMPLES_DIR}${process.pid}.json`, JSON.stringify(samples));
      } catch {
        /* best-effort: sampling is advisory, must never fail a suite */
      }
    });
  }
}

// ApiClient parses leniently (parse failure → body null), so a null parsedBody
// is ambiguous between "the literal JSON null" and "not JSON at all". Only the
// raw text can disambiguate.
function isJsonNull(raw: string): boolean {
  return raw.trim() === "null";
}
