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
      <a href={href} {...rest}>
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
    expect(within(consent).getByRole("link", { name: "Terms" })).toHaveAttribute(
      "href",
      "/terms",
    );
    expect(
      within(consent).getByRole("link", { name: "Privacy policy" }),
    ).toHaveAttribute("href", "/privacy");
  });
});
