// LegalFooterLinks reads NEXT_PUBLIC_PRIVACY_URL / NEXT_PUBLIC_TERMS_URL
// through lib/site, which resolves env vars at module load (Next.js inlines
// them at build time). Each scenario sets the env vars and re-imports the
// module tree fresh — see lib/site.test.ts for the same isolateModules
// pattern.

import { render, screen } from "@testing-library/react";

export {};

const PRIVACY_ENV_KEY = "NEXT_PUBLIC_PRIVACY_URL";
const TERMS_ENV_KEY = "NEXT_PUBLIC_TERMS_URL";

jest.mock("next/link", () => {
  return function MockLink({
    href,
    children,
    ...props
  }: {
    href: string;
    children: React.ReactNode;
    [key: string]: unknown;
  }) {
    return (
      <a href={href} {...props}>
        {children}
      </a>
    );
  };
});

function loadLegalFooterLinks(): typeof import("./LegalFooterLinks") {
  let mod: typeof import("./LegalFooterLinks") | undefined;
  jest.isolateModules(() => {
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    mod = require("./LegalFooterLinks");
  });
  if (!mod) throw new Error("failed to load ./LegalFooterLinks");
  return mod;
}

describe("LegalFooterLinks", () => {
  const originalPrivacy = process.env[PRIVACY_ENV_KEY];
  const originalTerms = process.env[TERMS_ENV_KEY];

  afterEach(() => {
    if (originalPrivacy === undefined) delete process.env[PRIVACY_ENV_KEY];
    else process.env[PRIVACY_ENV_KEY] = originalPrivacy;
    if (originalTerms === undefined) delete process.env[TERMS_ENV_KEY];
    else process.env[TERMS_ENV_KEY] = originalTerms;
  });

  it("renders nothing on a self-host/staging build with neither URL configured", () => {
    delete process.env[PRIVACY_ENV_KEY];
    delete process.env[TERMS_ENV_KEY];
    const { LegalFooterLinks } = loadLegalFooterLinks();
    const { container } = render(<LegalFooterLinks />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders Privacy and Terms links when both are configured", () => {
    process.env[PRIVACY_ENV_KEY] = "/privacy";
    process.env[TERMS_ENV_KEY] = "/terms";
    const { LegalFooterLinks } = loadLegalFooterLinks();
    render(<LegalFooterLinks />);
    expect(screen.getByRole("link", { name: "Privacy" })).toHaveAttribute(
      "href",
      "/privacy",
    );
    expect(screen.getByRole("link", { name: "Terms" })).toHaveAttribute(
      "href",
      "/terms",
    );
  });
});
