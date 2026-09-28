// SignInConsent reads NEXT_PUBLIC_PRIVACY_URL / NEXT_PUBLIC_TERMS_URL through
// lib/site, which resolves env vars at module load (Next.js inlines them at
// build time). Each scenario sets the env vars and re-imports the module
// tree fresh — see lib/site.test.ts for the same isolateModules pattern.

import { render, screen, within } from "@testing-library/react";

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

function loadSignInConsent(): typeof import("./SignInConsent") {
  let mod: typeof import("./SignInConsent") | undefined;
  jest.isolateModules(() => {
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    mod = require("./SignInConsent");
  });
  if (!mod) throw new Error("failed to load ./SignInConsent");
  return mod;
}

describe("SignInConsent", () => {
  const originalPrivacy = process.env[PRIVACY_ENV_KEY];
  const originalTerms = process.env[TERMS_ENV_KEY];

  afterEach(() => {
    if (originalPrivacy === undefined) delete process.env[PRIVACY_ENV_KEY];
    else process.env[PRIVACY_ENV_KEY] = originalPrivacy;
    if (originalTerms === undefined) delete process.env[TERMS_ENV_KEY];
    else process.env[TERMS_ENV_KEY] = originalTerms;
  });

  it("renders nothing when neither URL is configured", () => {
    delete process.env[PRIVACY_ENV_KEY];
    delete process.env[TERMS_ENV_KEY];
    const { SignInConsent } = loadSignInConsent();
    const { container } = render(<SignInConsent />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when only the privacy URL is configured", () => {
    process.env[PRIVACY_ENV_KEY] = "/privacy";
    delete process.env[TERMS_ENV_KEY];
    const { SignInConsent } = loadSignInConsent();
    const { container } = render(<SignInConsent />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when only the terms URL is configured", () => {
    delete process.env[PRIVACY_ENV_KEY];
    process.env[TERMS_ENV_KEY] = "/terms";
    const { SignInConsent } = loadSignInConsent();
    const { container } = render(<SignInConsent />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders the exact consent wording with links to both pages when both are configured", () => {
    process.env[PRIVACY_ENV_KEY] = "/privacy";
    process.env[TERMS_ENV_KEY] = "/terms";
    const { SignInConsent } = loadSignInConsent();
    render(<SignInConsent />);
    const consent = screen.getByTestId("sign-in-consent");
    expect(consent).toHaveTextContent(
      "By signing in you agree to the Terms and Privacy policy.",
    );
    expect(within(consent).getByRole("link", { name: "Terms" })).toHaveAttribute(
      "href",
      "/terms",
    );
    expect(
      within(consent).getByRole("link", { name: "Privacy policy" }),
    ).toHaveAttribute("href", "/privacy");
  });

  it("renders a same-origin path as an in-app link with no target", () => {
    process.env[PRIVACY_ENV_KEY] = "/privacy";
    process.env[TERMS_ENV_KEY] = "/terms";
    const { SignInConsent } = loadSignInConsent();
    render(<SignInConsent />);
    const consent = screen.getByTestId("sign-in-consent");
    expect(
      within(consent).getByRole("link", { name: "Terms" }),
    ).not.toHaveAttribute("target");
  });

  it("opens an absolute cross-origin legal URL in a new tab", () => {
    process.env[PRIVACY_ENV_KEY] = "https://legal.example.com/privacy";
    process.env[TERMS_ENV_KEY] = "/terms";
    const { SignInConsent } = loadSignInConsent();
    render(<SignInConsent />);
    const consent = screen.getByTestId("sign-in-consent");
    const privacyLink = within(consent).getByRole("link", {
      name: "Privacy policy",
    });
    expect(privacyLink).toHaveAttribute(
      "href",
      "https://legal.example.com/privacy",
    );
    expect(privacyLink).toHaveAttribute("target", "_blank");
    expect(privacyLink).toHaveAttribute("rel", "noopener noreferrer");

    const termsLink = within(consent).getByRole("link", { name: "Terms" });
    expect(termsLink).not.toHaveAttribute("target");
  });
});
