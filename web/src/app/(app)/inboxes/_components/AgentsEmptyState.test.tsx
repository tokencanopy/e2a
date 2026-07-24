import { render, screen } from "../../../../test-utils/swr";
import { AgentsEmptyState } from "./AgentsEmptyState";

jest.mock("next/link", () => {
  return function MockLink({ href, children, ...props }: { href: string; children: React.ReactNode; [key: string]: unknown }) {
    return <a href={href} {...props}>{children}</a>;
  };
});

describe("AgentsEmptyState", () => {
  it("explains the empty state and offers both creation paths", () => {
    render(<AgentsEmptyState />);
    expect(screen.getByText("No inboxes yet")).toBeInTheDocument();
    expect(
      screen.getByText(/Create an inbox — an email address for your agent/),
    ).toBeInTheDocument();
  });

  it("links to onboarding and to the domains page", () => {
    render(<AgentsEmptyState />);
    expect(
      screen.getByRole("link", { name: /Create your first inbox/ }),
    ).toHaveAttribute("href", "/get-started");
    expect(
      screen.getByRole("link", { name: "Set up a domain" }),
    ).toHaveAttribute("href", "/domains");
  });
});
