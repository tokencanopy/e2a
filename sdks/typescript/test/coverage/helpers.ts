// Shared helpers for the live ergonomic-coverage suite (test/e2e*.test.ts).
// Mirrors tests/e2e-prod/harness/fixtures.ts in spirit, scaled down to what
// this suite needs.
import { randomBytes } from "node:crypto";

export interface LiveEnv {
  baseUrl: string;
  apiKey: string;
  agentEmail: string;
  sharedDomain: string;
  sinkEmail: string;
}

/** Env is aligned with the contract runner + the Python live test (E2A_TEST_*
 *  naming) — see test/e2e.test.ts's header comment. Returns null (never
 *  throws) when creds are absent so callers can `describe.skipIf(!live)`. */
export function loadLiveEnv(): LiveEnv | null {
  const baseUrl = process.env.E2A_TEST_BASE_URL || "";
  const apiKey = process.env.E2A_TEST_API_KEY || "";
  const agentEmail = process.env.E2A_TEST_AGENT_EMAIL || "";
  if (!baseUrl || !apiKey || !agentEmail) return null;
  const sharedDomain = agentEmail.split("@")[1] ?? "";
  const sinkEmail = process.env.E2A_TEST_SINK_EMAIL || "success@simulator.amazonses.com";
  return { baseUrl, apiKey, agentEmail, sharedDomain, sinkEmail };
}

const RUN_ID = randomBytes(3).toString("hex");

export function runId(): string {
  return RUN_ID;
}

export function uniqueSlug(prefix = "sdkcov"): string {
  return `${prefix}-${RUN_ID}-${randomBytes(3).toString("hex")}`;
}

export function uniqueSubject(label: string): string {
  return `[sdk-coverage-${RUN_ID}] ${label} ${Date.now()}`;
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Poll `fn` until it returns a truthy value or the budget elapses. */
export async function poll<T>(
  fn: () => Promise<T | undefined | null>,
  opts: { attempts?: number; delayMs?: number } = {},
): Promise<T | undefined> {
  const attempts = opts.attempts ?? 20;
  const delayMs = opts.delayMs ?? 1000;
  for (let i = 0; i < attempts; i++) {
    const result = await fn();
    if (result) return result;
    await sleep(delayMs);
  }
  return undefined;
}
