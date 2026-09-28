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
