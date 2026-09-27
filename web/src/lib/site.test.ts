// site.ts reads NEXT_PUBLIC_* at module load (Next.js inlines them at build
// time), so each scenario sets the env var and re-imports the module fresh.

// Module marker: keeps the top-level ENV_KEY file-scoped (two test files
// declare the same const name; script-scope files would collide as globals).
export {};

const SIGN_IN_ENV_KEY = "NEXT_PUBLIC_E2A_SIGN_IN_URL";
const SITE_URL_ENV_KEY = "NEXT_PUBLIC_SITE_URL";

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
  const originalSignInURL = process.env[SIGN_IN_ENV_KEY];
  const originalSiteURL = process.env[SITE_URL_ENV_KEY];

  afterEach(() => {
    if (originalSignInURL === undefined) delete process.env[SIGN_IN_ENV_KEY];
    else process.env[SIGN_IN_ENV_KEY] = originalSignInURL;
    if (originalSiteURL === undefined) delete process.env[SITE_URL_ENV_KEY];
    else process.env[SITE_URL_ENV_KEY] = originalSiteURL;
  });

  it("defaults to the legacy Google door when NEXT_PUBLIC_E2A_SIGN_IN_URL is unset", () => {
    delete process.env[SIGN_IN_ENV_KEY];
    const site = loadSite();
    expect(site.SIGN_IN_URL).toBe("/api/auth/login");
    expect(site.SIGN_IN_LABEL).toBe("Sign in with Google");
    expect(site.signInURLWithReturnTo("/oauth2/authorize")).toBe(
      "/api/auth/login?return_to=%2Foauth2%2Fauthorize",
    );
  });

  it("identifies the TokenCanopy door only for the hosted site build", () => {
    process.env[SITE_URL_ENV_KEY] = "https://e2a.dev";
    process.env[SIGN_IN_ENV_KEY] = "/api/auth/oidc/login";
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
    expect(
      site.signInURLWithReturnTo(
        "/oauth2/authorize?client_id=mcp_abc&state=opaque",
        site.LEGACY_SIGN_IN_URL,
      ),
    ).toBe(
      "/api/auth/login?return_to=%2Foauth2%2Fauthorize%3Fclient_id%3Dmcp_abc%26state%3Dopaque",
    );
  });

  it("keeps the generic OIDC route provider-neutral for self-hosters", () => {
    process.env[SITE_URL_ENV_KEY] = "https://mail.example.com";
    process.env[SIGN_IN_ENV_KEY] = "/api/auth/oidc/login";
    const site = loadSite();

    expect(site.SIGN_IN_URL).toBe("/api/auth/oidc/login");
    expect(site.SIGN_IN_LABEL).toBe("Sign in");
  });
});

describe("site config — legal links", () => {
  const PRIVACY_ENV_KEY = "NEXT_PUBLIC_PRIVACY_URL";
  const TERMS_ENV_KEY = "NEXT_PUBLIC_TERMS_URL";
  const originalPrivacy = process.env[PRIVACY_ENV_KEY];
  const originalTerms = process.env[TERMS_ENV_KEY];
  const originalSiteURL = process.env[SITE_URL_ENV_KEY];

  afterEach(() => {
    if (originalPrivacy === undefined) delete process.env[PRIVACY_ENV_KEY];
    else process.env[PRIVACY_ENV_KEY] = originalPrivacy;
    if (originalTerms === undefined) delete process.env[TERMS_ENV_KEY];
    else process.env[TERMS_ENV_KEY] = originalTerms;
    if (originalSiteURL === undefined) delete process.env[SITE_URL_ENV_KEY];
    else process.env[SITE_URL_ENV_KEY] = originalSiteURL;
  });

  it("defaults both URLs to empty and hides both footer links", () => {
    delete process.env[PRIVACY_ENV_KEY];
    delete process.env[TERMS_ENV_KEY];
    const site = loadSite();
    expect(site.PRIVACY_URL).toBe("");
    expect(site.TERMS_URL).toBe("");
    expect(site.legalFooterLinks()).toEqual([]);
  });

  it("includes only Privacy when only the privacy URL is configured", () => {
    process.env[PRIVACY_ENV_KEY] = "/privacy";
    delete process.env[TERMS_ENV_KEY];
    const site = loadSite();
    expect(site.legalFooterLinks()).toEqual([
      { label: "Privacy", href: "/privacy", external: false },
    ]);
  });

  it("includes only Terms when only the terms URL is configured", () => {
    delete process.env[PRIVACY_ENV_KEY];
    process.env[TERMS_ENV_KEY] = "/terms";
    const site = loadSite();
    expect(site.legalFooterLinks()).toEqual([
      { label: "Terms", href: "/terms", external: false },
    ]);
  });

  it("includes Privacy then Terms, in that order, when both are configured", () => {
    process.env[PRIVACY_ENV_KEY] = "/privacy";
    process.env[TERMS_ENV_KEY] = "/terms";
    const site = loadSite();
    expect(site.legalFooterLinks()).toEqual([
      { label: "Privacy", href: "/privacy", external: false },
      { label: "Terms", href: "/terms", external: false },
    ]);
  });

  it("treats a same-origin path as internal", () => {
    delete process.env[SITE_URL_ENV_KEY];
    const site = loadSite();
    expect(site.isExternalURL("/privacy")).toBe(false);
  });

  it("treats an absolute URL on a different origin as external", () => {
    process.env[SITE_URL_ENV_KEY] = "https://e2a.dev";
    process.env[PRIVACY_ENV_KEY] = "https://legal.example.com/privacy";
    delete process.env[TERMS_ENV_KEY];
    const site = loadSite();
    expect(site.legalFooterLinks()).toEqual([
      {
        label: "Privacy",
        href: "https://legal.example.com/privacy",
        external: true,
      },
    ]);
  });

  it("treats an absolute URL matching SITE_URL's own origin as internal", () => {
    process.env[SITE_URL_ENV_KEY] = "https://e2a.dev";
    process.env[TERMS_ENV_KEY] = "https://e2a.dev/terms";
    delete process.env[PRIVACY_ENV_KEY];
    const site = loadSite();
    expect(site.legalFooterLinks()).toEqual([
      { label: "Terms", href: "https://e2a.dev/terms", external: false },
    ]);
  });

  it("treats malformed input as internal rather than throwing", () => {
    delete process.env[SITE_URL_ENV_KEY];
    const site = loadSite();
    expect(site.isExternalURL("not a url")).toBe(false);
  });
});
