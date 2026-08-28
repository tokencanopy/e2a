/**
 * Hosted-build branch of the nav Pricing link: NEXT_PUBLIC_PRICING_PATH is
 * inlined at build time and read at module load (lib/site.PRICING_PATH), so
 * this file sets the env BEFORE importing the page. The unset/self-host
 * branch (no Pricing link at all) is asserted in page.test.tsx, where jest
 * runs with the env var absent.
 */
process.env.NEXT_PUBLIC_PRICING_PATH = "/pricing";

import { render, screen } from "@testing-library/react";
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

describe("nav Pricing link (hosted build)", () => {
  it("renders and targets the configured path", () => {
    render(<Home />);
    const link = screen.getByRole("link", { name: "Pricing" });
    expect(link).toHaveAttribute("href", "/pricing");
    // Same-site navigation: unlike the Resources links, Pricing must NOT
    // open a new tab.
    expect(link).not.toHaveAttribute("target");
  });
});
