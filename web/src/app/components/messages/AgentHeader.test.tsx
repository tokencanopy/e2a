import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AgentHeader } from "./AgentHeader";
import type { DashboardAgent } from "../types";

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

const verified: DashboardAgent = {
  id: "a",
  domain: "acme.dev",
  email: "support@acme.dev",
  name: "support",
  domain_verified: true,
  created_at: "2026-05-12T00:00:00Z",
};
const unverified: DashboardAgent = {
  ...verified,
  email: "new@acme.dev",
  domain_verified: false,
};

const mockFetch = jest.fn();
beforeEach(() => {
  mockFetch.mockReset();
  global.fetch = mockFetch as unknown as typeof fetch;
});

describe("AgentHeader — test-send action (moved from the dashboard card)", () => {
  it("shows 'Send a test message' for a verified inbox", () => {
    render(<AgentHeader agent={verified} tab="messages" />);
    expect(
      screen.getByRole("button", { name: "Send a test message" }),
    ).toBeInTheDocument();
  });

  it("hides the action for an unverified inbox", () => {
    render(<AgentHeader agent={unverified} tab="messages" />);
    expect(
      screen.queryByRole("button", { name: "Send a test message" }),
    ).not.toBeInTheDocument();
  });

  // Regression guard: a long email address rendered in the identity-row <code>
  // used to overflow the header. The fix caps it at maxWidth:100% and lets it
  // wrap (wordBreak: break-all) instead of pushing past the card edge.
  it("keeps a long inbox email from overflowing the header", () => {
    const longEmail =
      "a-very-long-inbox-slug-that-keeps-going@some-very-long-subdomain.acme.dev";
    render(
      <AgentHeader
        agent={{ ...verified, email: longEmail }}
        tab="messages"
      />,
    );
    const code = screen.getByText(longEmail);
    expect(code).toHaveStyle({ wordBreak: "break-all" });
    expect(code).toHaveStyle({ maxWidth: "100%" });
  });

  it("POSTs the agent test endpoint and surfaces success", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      text: () =>
        Promise.resolve(JSON.stringify({ status: "queued", message_id: "m1" })),
    });
    const user = userEvent.setup();
    render(<AgentHeader agent={verified} tab="messages" />);

    await user.click(
      screen.getByRole("button", { name: "Send a test message" }),
    );

    await waitFor(() =>
      expect(screen.getByText("Sent ✓")).toBeInTheDocument(),
    );
    expect(mockFetch).toHaveBeenCalledWith(
      expect.stringContaining("/v1/agents/support%40acme.dev/test"),
      expect.objectContaining({ method: "POST" }),
    );
  });

  it("links the Suppressions tab to the slug the layout detects", () => {
    // The tab's slug and layout.tsx's detectTab branch must agree, or the tab
    // renders inactive on its own page. Pin both ends here.
    render(<AgentHeader agent={verified} tab="suppressions" />);

    const tab = screen.getByRole("link", { name: "Suppressions" });
    expect(tab).toHaveAttribute(
      "href",
      "/inboxes/suppressions?email=support%40acme.dev",
    );
    expect(tab).toHaveAttribute("aria-current", "page");
    // Sibling tabs stay present and inactive.
    expect(screen.getByRole("link", { name: "Inbox" })).not.toHaveAttribute("aria-current");
  });
});
