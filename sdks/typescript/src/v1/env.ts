// env.ts — shared environment-based base-URL resolution.
//
// Both E2AClient and WSListener need to agree on "which API host do we talk
// to when the caller didn't pass one explicitly", so this logic lives in its
// own module rather than in client.ts: client.ts constructs a WSStream (see
// ws.ts's `WSStream`), so ws.ts importing resolveBaseUrl FROM client.ts would
// create an import cycle. Extracting it here keeps both call sites — and any
// future one — using exactly the same resolution and the same one-shot
// deprecation warning for E2A_BASE_URL.

export function envVar(name: string): string | undefined {
  if (typeof process !== "undefined" && process.env && process.env[name]) return process.env[name];
  return undefined;
}

let warnedBaseUrlDeprecated = false;

// resolveBaseUrl reads the API host. Canonical is E2A_API_URL — the same
// concept the server names with E2A_API_URL (its externally visible API base).
// E2A_BASE_URL is the name the SDKs shipped with; still honoured so published
// integrations keep working, with a one-shot deprecation note shared across
// every caller of this function (module-level flag, not per-caller).
export function resolveBaseUrl(): string | undefined {
  const canonical = envVar("E2A_API_URL");
  if (canonical) return canonical;
  const legacy = envVar("E2A_BASE_URL");
  if (legacy && !warnedBaseUrlDeprecated) {
    warnedBaseUrlDeprecated = true;
    console.warn(
      "[e2a] E2A_BASE_URL is deprecated — rename it to E2A_API_URL. " +
        "The old name still works for now but will be dropped.",
    );
  }
  return legacy;
}

/**
 * Terminal fallback shared by every SDK entry point that resolves a base URL
 * (E2AClient, WSListener): the hosted product's API host. Not a self-host
 * default to fail closed on — this SDK is a client library instantiated
 * directly by the caller's own code, the same shape as any other hosted
 * API's SDK defaulting to that provider's endpoint, and it's documented as
 * such on every options type below. Self-hosters override it with
 * `baseUrl` or `E2A_API_URL`.
 */
export const DEFAULT_BASE_URL = "https://api.e2a.dev";
