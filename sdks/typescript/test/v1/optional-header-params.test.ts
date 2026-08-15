/**
 * Offline regression tests for optional header parameters in the generated
 * request factories.
 *
 * The `typescript` OpenAPI generator emits setHeaderParam(...) unconditionally
 * for every header param. For an omitted OPTIONAL param that stored `undefined`
 * on the RequestContext, and the fetch layer's Headers.set coerced it to the
 * literal string "undefined" on the wire — so `contacts.update` /
 * `contacts.setOutreach` without an etag always hit a real server as a
 * conditional request (`If-Match: undefined`) and failed with 412
 * precondition_failed (first observed live: e2a-ops release-pipeline run
 * 30612956986, test/e2e-contacts.test.ts). The generation pipeline now wraps
 * optional header emissions in `if (param !== undefined)` guards
 * (scripts/guard-optional-header-params.py); these tests pin that behavior
 * WITHOUT a live server, so a regression fails ordinary unit CI instead of
 * only the staging gate.
 */
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, it, expect } from "vitest";
import { ContactsApiRequestFactory } from "../../src/v1/generated/apis/ContactsApi.js";
import { DomainsApiRequestFactory } from "../../src/v1/generated/apis/DomainsApi.js";
import { createConfiguration } from "../../src/v1/generated/configuration.js";
import { ServerConfiguration } from "../../src/v1/generated/servers.js";

const config = createConfiguration({
  baseServer: new ServerConfiguration("http://contract.invalid", {}),
});
const factory = new ContactsApiRequestFactory(config);
const domainsFactory = new DomainsApiRequestFactory(config);

function headerNames(headers: Record<string, string>): string[] {
  return Object.keys(headers).map((name) => name.toLowerCase());
}

describe("optional If-Match header (generated request factories)", () => {
  it("updateContact without an etag sends NO If-Match header", async () => {
    const ctx = await factory.updateContact("partner@fund.vc", { displayName: "P" });
    expect(headerNames(ctx.getHeaders())).not.toContain("if-match");
  });

  it("updateContact with an etag sends exactly that etag", async () => {
    const ctx = await factory.updateContact("partner@fund.vc", { displayName: "P" }, 'W/"c42"');
    expect(ctx.getHeaders()["If-Match"]).toBe('W/"c42"');
  });

  it("upsertEngagement without an etag sends NO If-Match header (first enrolment must not be conditional)", async () => {
    const ctx = await factory.upsertEngagement(
      "raise@agents.e2a.dev",
      "partner@fund.vc",
      { stage: "contacted" },
    );
    expect(headerNames(ctx.getHeaders())).not.toContain("if-match");
  });

  it("upsertEngagement with an etag sends exactly that etag", async () => {
    const ctx = await factory.upsertEngagement(
      "raise@agents.e2a.dev",
      "partner@fund.vc",
      { stage: "contacted" },
      'W/"e7"',
    );
    expect(ctx.getHeaders()["If-Match"]).toBe('W/"e7"');
  });
});

describe("deleteDomain generated positional compatibility", () => {
  it("keeps Configuration third and appends Idempotency-Key fourth", async () => {
    // Configuration was the third argument before Idempotency-Key existed.
    // A generated-header insertion must not reinterpret existing callers'
    // transport options as a string header.
    await expect(
      domainsFactory.deleteDomain("safe-retry.example.test", "DELETE", config),
    ).resolves.toBeDefined();

    const ctx = await domainsFactory.deleteDomain(
      "safe-retry.example.test",
      "DELETE",
      config,
      "domain-delete-operation-1",
    );
    expect(ctx.getHeaders()["Idempotency-Key"]).toBe("domain-delete-operation-1");
  });
});

describe("generated APIs never emit an optional header param unconditionally", () => {
  // Static audit across ALL committed generated request factories: every
  // serialized header emission must either target a required param, be wrapped
  // in an `if (param !== undefined)` guard, or be the deliberate
  // Idempotency-Key stub (retry.ts depends on that stub for POST retry-safety
  // gating and key minting — see scripts/guard-optional-header-params.py).
  const apisDir = join(
    dirname(fileURLToPath(import.meta.url)),
    "../../src/v1/generated/apis",
  );

  it("holds for every committed generated API file", () => {
    const offenders: string[] = [];
    for (const file of readdirSync(apisDir).filter((f) => f.endsWith("Api.ts"))) {
      const lines = readFileSync(join(apisDir, file), "utf8").split("\n");
      let optionalParams = new Set<string>();
      for (let i = 0; i < lines.length; i++) {
        const sig = lines[i].match(/public async \w+\(([^)]*)\): Promise<RequestContext>/);
        if (sig) {
          optionalParams = new Set(
            [...sig[1].matchAll(/(\w+)\?:/g)].map((m) => m[1]),
          );
          continue;
        }
        const header = lines[i].match(
          /^\s*requestContext\.setHeaderParam\("([^"]+)", ObjectSerializer\.serialize\((\w+),/,
        );
        if (!header) continue;
        const [, name, param] = header;
        if (name.toLowerCase() === "idempotency-key") continue;
        if (!optionalParams.has(param)) continue;
        if (lines[i - 1]?.trim() === `if (${param} !== undefined) {`) continue;
        offenders.push(`${file}:${i + 1} sets optional header "${name}" unconditionally`);
      }
    }
    expect(offenders).toEqual([]);
  });
});
