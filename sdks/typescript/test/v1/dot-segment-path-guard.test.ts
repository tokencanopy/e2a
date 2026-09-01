/**
 * Offline regression tests for the generated-layer dot-segment guard.
 *
 * `RequestContext`'s constructor (and its `setUrl` twin) pass a caller-built
 * path straight to `new URL()`, which silently collapses a "." or ".." path
 * segment before either `Middleware.pre()` or `RetryHttpLibrary.send()` ever
 * sees the request, so a value like `domain=".."` retargets a call meant
 * for one domain at a different, larger-scoped resource (#792, #915). The
 * generation pipeline now injects a raw-string guard into the generated
 * `http.ts` (scripts/guard-dot-segment-path.py); these tests pin that
 * behavior directly against the committed generated output, both through the
 * chokepoint itself and through a real generated `*Api.ts` call site.
 */
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, it, expect } from "vitest";
import { RequestContext, HttpMethod } from "../../src/v1/generated/http/http.js";
import { DomainsApiRequestFactory } from "../../src/v1/generated/apis/DomainsApi.js";
import { createConfiguration } from "../../src/v1/generated/configuration.js";
import { ServerConfiguration } from "../../src/v1/generated/servers.js";

const config = createConfiguration({
  baseServer: new ServerConfiguration("https://api.e2a.dev", {}),
});

describe("RequestContext rejects a dot-segment path", () => {
  it("throws on a bare '..' path segment", () => {
    expect(() => new RequestContext("https://api.e2a.dev/v1/domains/..", HttpMethod.DELETE)).toThrow(
      /unsafe ".." segment/,
    );
  });

  it("throws on a bare '.' path segment", () => {
    expect(() => new RequestContext("https://api.e2a.dev/v1/domains/.", HttpMethod.DELETE)).toThrow(
      /unsafe "\." segment/,
    );
  });

  it("does not throw on an ordinary path, even one containing dots", () => {
    const ctx = new RequestContext(
      "https://api.e2a.dev/v1/agents/partner%40fund.vc/messages",
      HttpMethod.GET,
    );
    expect(ctx.getUrl()).toBe("https://api.e2a.dev/v1/agents/partner%40fund.vc/messages");
  });

  it("does not throw on a dot inside the query string", () => {
    const ctx = new RequestContext(
      "https://api.e2a.dev/v1/domains?cursor=..%2Fetc",
      HttpMethod.GET,
    );
    expect(ctx.getUrl()).toBe("https://api.e2a.dev/v1/domains?cursor=..%2Fetc");
  });
});

describe("generated DomainsApi call site is covered end to end", () => {
  const factory = new DomainsApiRequestFactory(config);

  it("deleteDomain('..') throws instead of building a request", async () => {
    await expect(factory.deleteDomain("..", "DELETE")).rejects.toThrow(/unsafe ".." segment/);
  });

  it("deleteDomain(realDomain) still builds the expected request", async () => {
    const ctx = await factory.deleteDomain("safe-retry.example.test", "DELETE");
    expect(ctx.getUrl()).toBe("https://api.e2a.dev/v1/domains/safe-retry.example.test?confirm=DELETE");
  });
});

describe("committed generated http.ts always guards both new URL() call sites", () => {
  // Static audit across the committed output, for the same reason the
  // optional-header-param guard has one (see optional-header-params.test.ts):
  // a future generator upgrade that changes how many call sites collapse the
  // URL must fail this test, not silently ship an unguarded chokepoint.
  const httpTsPath = join(
    dirname(fileURLToPath(import.meta.url)),
    "../../src/v1/generated/http/http.ts",
  );

  it("guards every 'new URL(ensureAbsoluteUrl(url))' call site", () => {
    const lines = readFileSync(httpTsPath, "utf8").split("\n");
    const collapseSites = lines
      .map((line, i) => ({ line, i }))
      .filter(({ line }) => line.includes("new URL(ensureAbsoluteUrl(url))"));

    expect(collapseSites.length).toBeGreaterThan(0);
    for (const { line, i } of collapseSites) {
      expect(lines[i - 1].trim()).toBe("assertNoDotSegmentInPath(url);");
      void line;
    }
  });
});
