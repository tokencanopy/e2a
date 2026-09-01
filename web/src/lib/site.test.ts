// site.ts reads NEXT_PUBLIC_* at module load (Next.js inlines them at build
// time), so each scenario sets the env var and re-imports the module fresh.

// Module marker: keeps the top-level ENV_KEY file-scoped (two test files
// declare the same const name; script-scope files would collide as globals).
export {};

const ENV_KEY = "NEXT_PUBLIC_E2A_SIGN_IN_URL";

function loadSite(): typeof import("./site") {
  let site: typeof import("./site") | undefined;
  jest.isolateModules(() => {
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    site = require("./site");
  });
  if (!site) throw new Error("failed to load ./site");
  return site;
}

describe("site config — sign-in door", () => {
  const original = process.env[ENV_KEY];

  afterEach(() => {
    if (original === undefined) delete process.env[ENV_KEY];
    else process.env[ENV_KEY] = original;
  });

  it("defaults to the legacy Google door when NEXT_PUBLIC_E2A_SIGN_IN_URL is unset", () => {
    delete process.env[ENV_KEY];
    const site = loadSite();
    expect(site.SIGN_IN_URL).toBe("/api/auth/login");
    expect(site.SIGN_IN_LABEL).toBe("Sign in with Google");
    expect(site.signInURLWithReturnTo("/oauth2/authorize")).toBe(
      "/api/auth/login?return_to=%2Foauth2%2Fauthorize",
    );
  });

  it("identifies the TokenCanopy door when the hosted OIDC route is set", () => {
    process.env[ENV_KEY] = "/api/auth/oidc/login";
    const site = loadSite();
    expect(site.SIGN_IN_URL).toBe("/api/auth/oidc/login");
    expect(site.SIGN_IN_LABEL).toBe("Sign in with TokenCanopy");
    expect(site.LEGACY_SIGN_IN_URL).toBe("/api/auth/login");
    expect(
      site.signInURLWithReturnTo(
        "/oauth2/authorize?client_id=mcp_abc&state=opaque",
      ),
    ).toBe(
      "/api/auth/oidc/login?return_to=%2Foauth2%2Fauthorize%3Fclient_id%3Dmcp_abc%26state%3Dopaque",
    );
  });
});
