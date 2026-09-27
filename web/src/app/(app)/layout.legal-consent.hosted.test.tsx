/**
 * Hosted-build branch of the dashboard's signed-out sign-in consent line:
 * NEXT_PUBLIC_PRIVACY_URL / NEXT_PUBLIC_TERMS_URL are inlined at build time
 * and read at module load (lib/site.PRIVACY_URL / TERMS_URL), so this file
 * sets both env vars BEFORE importing AppLayoutClient. The unset/self-host
 * branch (no consent line) is asserted in layout.test.tsx, where jest runs
 * with the env vars absent.
 */
process.env.NEXT_PUBLIC_PRIVACY_URL = "/privacy";
process.env.NEXT_PUBLIC_TERMS_URL = "/terms";

import { render, screen, within } from "@testing-library/react";
import AppLayout from "./AppLayoutClient";

// Legal links must never render through next/link's <Link> — see the
// comment on LegalAnchor in SignInConsent.tsx for why (same-origin legal
// paths can resolve outside this Next app, and <Link> prefetches the
// route's RSC payload regardless). This mock tags anything that DOES go
// through <Link> with a marker attribute so a Link-rendered legal link is
// distinguishable from a plain <a> even though both render as <a> tags.
jest.mock("next/link", () => {
  return function MockLink({
    href,
    children,
    ...rest
  }: {
    href: string;
    children: React.ReactNode;
    [k: string]: unknown;
  }) {
    return (
      <a href={href} {...rest} data-next-link="true">
        {children}
      </a>
    );
  };
});

jest.mock("next/navigation", () => ({
  usePathname: () => "/inboxes",
  useRouter: () => ({ replace: jest.fn(), push: jest.fn(), back: jest.fn() }),
}));

jest.mock("../components/AuthProvider", () => ({
  useAuth: () => ({ user: null, loading: false }),
}));

jest.mock("../components/loft/Sidebar", () => ({
  Sidebar: () => null,
}));
jest.mock("../components/swr/PendingPollingOwner", () => ({
  PendingPollingOwner: () => null,
}));

describe("(app) layout — sign-in consent (hosted build)", () => {
  it("renders the consent line beneath the sign-in links when both legal URLs are configured", () => {
    render(
      <AppLayout>
        <div>page content</div>
      </AppLayout>,
    );
    const consent = screen.getByTestId("sign-in-consent");
    expect(consent).toHaveTextContent(
      "By signing in you agree to the Terms and Privacy policy.",
    );
    const termsLink = within(consent).getByRole("link", { name: "Terms" });
    expect(termsLink).toHaveAttribute("href", "/terms");
    const privacyLink = within(consent).getByRole("link", {
      name: "Privacy policy",
    });
    expect(privacyLink).toHaveAttribute("href", "/privacy");

    // Same-origin on paper ("/terms", "/privacy"), but on the hosted
    // deployment these resolve to static Caddy routes, not Next pages.
    // <Link> would prefetch that route's RSC payload and 404 — regression
    // coverage for the #1051 follow-up bug.
    expect(termsLink).not.toHaveAttribute("data-next-link");
    expect(privacyLink).not.toHaveAttribute("data-next-link");
  });
});
