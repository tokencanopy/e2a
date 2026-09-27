/**
 * Hosted-build branch of the Privacy/Terms footer links and sign-in consent
 * line: NEXT_PUBLIC_PRIVACY_URL / NEXT_PUBLIC_TERMS_URL are inlined at build
 * time and read at module load (lib/site.PRIVACY_URL / TERMS_URL), so this
 * file sets both env vars BEFORE importing the page. The unset/self-host
 * branch (no links, no consent line) is asserted in page.test.tsx, where
 * jest runs with the env vars absent — see page.pricing-nav.hosted.test.tsx
 * for the same pattern applied to NEXT_PUBLIC_PRICING_PATH.
 */
process.env.NEXT_PUBLIC_PRIVACY_URL = "/privacy";
process.env.NEXT_PUBLIC_TERMS_URL = "/terms";

import { render, screen, within } from "@testing-library/react";
import Home from "./page";

// Legal links must never render through next/link's <Link> — see the
// comment on legalFooterLinks() in lib/site.ts for why (same-origin legal
// paths can resolve outside this Next app, and <Link> prefetches the
// route's RSC payload regardless). This mock tags anything that DOES go
// through <Link> with a marker attribute so a Link-rendered legal link is
// distinguishable from a plain <a> even though both render as <a> tags.
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
      <a href={href} {...props} data-next-link="true">
        {children}
      </a>
    );
  };
});

jest.mock("./components/AuthProvider", () => ({
  useAuth: () => ({ user: null, loading: false, signOut: jest.fn() }),
}));

describe("Privacy/Terms links and sign-in consent (hosted build)", () => {
  it("renders Privacy and Terms in the footer", () => {
    render(<Home />);
    const footer = screen.getByRole("contentinfo");
    expect(within(footer).getByRole("link", { name: "Privacy" })).toHaveAttribute(
      "href",
      "/privacy",
    );
    expect(within(footer).getByRole("link", { name: "Terms" })).toHaveAttribute(
      "href",
      "/terms",
    );
  });

  it("renders footer Privacy/Terms as plain anchors, never next/link's <Link>", () => {
    // Same-origin as this Next app on paper ("/privacy", "/terms"), but on
    // the hosted deployment these resolve to a static Caddy route, not a
    // Next page. <Link> would prefetch that route's RSC payload and 404 —
    // regression coverage for the #1051 follow-up bug.
    render(<Home />);
    const footer = screen.getByRole("contentinfo");
    expect(
      within(footer).getByRole("link", { name: "Privacy" }),
    ).not.toHaveAttribute("data-next-link");
    expect(
      within(footer).getByRole("link", { name: "Terms" }),
    ).not.toHaveAttribute("data-next-link");
  });

  it("renders the sign-in consent line beneath the nav Sign in link with exact wording", () => {
    render(<Home />);
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
});
