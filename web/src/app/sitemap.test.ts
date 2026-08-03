// The sitemap reads NEXT_PUBLIC_PRICING_PATH through site.ts, which resolves
// env vars at module load (Next.js inlines them at build time). Each scenario
// therefore re-imports the module tree fresh.

const ENV_KEY = "NEXT_PUBLIC_PRICING_PATH";

function loadSitemap(): () => Array<{ url: string; priority?: number }> {
  let mod: { default: () => Array<{ url: string; priority?: number }> } | undefined;
  jest.isolateModules(() => {
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    mod = require("./sitemap");
  });
  if (!mod) throw new Error("failed to load ./sitemap");
  return mod.default;
}

function urls(): string[] {
  return loadSitemap()().map((entry) => entry.url);
}

describe("sitemap", () => {
  const original = process.env[ENV_KEY];

  afterEach(() => {
    if (original === undefined) delete process.env[ENV_KEY];
    else process.env[ENV_KEY] = original;
  });

  it("omits pricing when the deployment has no pricing route", () => {
    delete process.env[ENV_KEY];
    // Staging and self-hosts don't serve /pricing — advertising it there would
    // put a 404 in the sitemap.
    expect(urls().some((u) => u.endsWith("/pricing"))).toBe(false);
  });

  it("advertises pricing at the configured path when the deployment serves one", () => {
    process.env[ENV_KEY] = "/pricing";
    const entries = loadSitemap()();
    const pricing = entries.find((e) => e.url.endsWith("/pricing"));
    expect(pricing).toBeDefined();
    // Commercial intent — should not rank below the blog.
    expect(pricing?.priority).toBeGreaterThanOrEqual(0.8);
  });

  it("includes the MCP landing page and every blog post", () => {
    delete process.env[ENV_KEY];
    const found = urls();
    expect(found.some((u) => u.endsWith("/mcp"))).toBe(true);
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    const { posts } = require("./blog/posts") as typeof import("./blog/posts");
    for (const post of posts) {
      expect(found.some((u) => u.endsWith(`/blog/${post.slug}`))).toBe(true);
    }
  });

  it("emits no duplicate URLs", () => {
    process.env[ENV_KEY] = "/pricing";
    const found = urls();
    expect(new Set(found).size).toBe(found.length);
  });
});
