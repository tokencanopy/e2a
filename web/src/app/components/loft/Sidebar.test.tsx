import { render, screen } from "@testing-library/react";
import { Sidebar } from "./Sidebar";

// Mock the routing + auth + pending-count dependencies the Sidebar
// reaches into. Without these, the component can't render in jsdom
// because `usePathname`, `useAuth`, and `usePendingCount` all assume
// runtime context that doesn't exist in a unit test.

let mockPathname = "/inboxes";
jest.mock("next/navigation", () => ({
  usePathname: () => mockPathname,
}));

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

jest.mock("../AuthProvider", () => ({
  useAuth: () => ({
    user: {
      id: "usr_test",
      email: "alice@example.com",
      name: "Alice",
      created_at: "2026-04-01T10:00:00Z",
    },
    loading: false,
    signOut: jest.fn(),
    setUser: jest.fn(),
  }),
}));

let mockPendingCount: number | null = null;
jest.mock("../hooks/usePendingCount", () => ({
  usePendingCount: () => mockPendingCount,
}));

let mockUnreadCount: { count: number; more: boolean } | null = null;
jest.mock("../hooks/useUnreadCount", () => ({
  useUnreadCount: () => mockUnreadCount,
}));

beforeEach(() => {
  mockPathname = "/inboxes";
  mockPendingCount = null;
  mockUnreadCount = null;
});

function navBadge(href: string): Element | null {
  return document.querySelector(`a[href="${href}"] [data-nav-badge]`);
}

// The Sidebar's NAV_ITEMS array is the canonical source of truth for
// what the user can reach from the global chrome. These tests pin the
// **set** + **order** of entries so a future drive-by edit (like the
// one that recently dropped the Webhook secrets entry while leaving
// its icon definition behind) gets caught at test time, not by a user
// noticing the sidebar looks short.

describe("Sidebar — nav entries", () => {
  it("renders every expected nav item with its href", () => {
    render(<Sidebar />);
    // Find each nav link by its target href (more specific than label —
    // "Agents" partial-matches the brand link "e2a — Email for AI agents").
    const expected: Array<{ label: string; href: string }> = [
      { label: "Get started", href: "/get-started" },
      { label: "Inboxes", href: "/inboxes" },
      { label: "Pending", href: "/reviews" },
      { label: "Contacts", href: "/contacts" },
      { label: "Templates", href: "/templates" },
      { label: "Domains", href: "/domains" },
      { label: "API keys", href: "/api-keys" },
      { label: "Webhooks", href: "/webhooks" },
    ];
    for (const { label, href } of expected) {
      const link = document.querySelector(`a[href="${href}"]`);
      expect(link).not.toBeNull();
      expect(link?.textContent ?? "").toContain(label);
    }
  });

  it("renders nav items in the canonical order", () => {
    render(<Sidebar />);
    const allLinks = screen
      .getAllByRole("link")
      .map((a) => a.getAttribute("href"));
    // Filter to the nav hrefs only (skip the logo + user-card links).
    const navHrefs = [
      "/get-started",
      "/inboxes",
      "/reviews",
      "/contacts",
      "/templates",
      "/domains",
      "/api-keys",
      "/webhooks",
    ];
    const orderInDOM = allLinks.filter((h) => h && navHrefs.includes(h));
    expect(orderInDOM).toEqual(navHrefs);
  });

  it("marks the matching nav item active by pathname", () => {
    mockPathname = "/webhooks";
    render(<Sidebar />);
    const webhooks = document.querySelector(`a[href="/webhooks"]`);
    expect(webhooks).toHaveAttribute("aria-current", "page");
    // Sanity: a sibling nav item is NOT active.
    const apiKeys = document.querySelector(`a[href="/api-keys"]`);
    expect(apiKeys).not.toHaveAttribute("aria-current", "page");
  });

  it("marks /reviews/anything active under the Pending entry (matchPrefix)", () => {
    // Next's usePathname() strips the query string, so simulate a true
    // subpath here (e.g. /reviews/review). The matchPrefix
    // flag on the Pending nav item makes the prefix match active.
    mockPathname = "/reviews/review";
    render(<Sidebar />);
    const pending = document.querySelector(`a[href="/reviews"]`);
    expect(pending).toHaveAttribute("aria-current", "page");
  });

  it("marks Templates active on the /templates/edit detail view (matchPrefix)", () => {
    mockPathname = "/templates/edit";
    render(<Sidebar />);
    const templates = document.querySelector(`a[href="/templates"]`);
    expect(templates).toHaveAttribute("aria-current", "page");
  });

  it("marks Agents active when the user is on a per-agent screen under /inboxes/*", () => {
    // The per-agent inbox lives at /inboxes/messages. The
    // Agents nav item (href=/inboxes) declares matchPrefixes:
    // ["/inboxes"] so it stays lit on those routes.
    mockPathname = "/inboxes/messages";
    render(<Sidebar />);
    const agents = document.querySelector(`a[href="/inboxes"]`);
    expect(agents).toHaveAttribute("aria-current", "page");
    // Pending must NOT also light up — it's a sibling top-level feature.
    const pending = document.querySelector(`a[href="/reviews"]`);
    expect(pending).not.toHaveAttribute("aria-current", "page");
  });

  it("does NOT mark Agents active on /reviews (matchPrefixes scoped to /inboxes)", () => {
    mockPathname = "/reviews";
    render(<Sidebar />);
    const agents = document.querySelector(`a[href="/inboxes"]`);
    expect(agents).not.toHaveAttribute("aria-current", "page");
  });

  it("does not show an Inboxes badge while unread count is unknown or zero", () => {
    const { rerender } = render(<Sidebar />);
    expect(navBadge("/inboxes")).toBeNull();

    mockUnreadCount = { count: 0, more: false };
    rerender(<Sidebar />);
    expect(navBadge("/inboxes")).toBeNull();
  });

  it("shows a positive unread count only in the Inboxes link", () => {
    mockUnreadCount = { count: 7, more: false };
    render(<Sidebar />);

    expect(navBadge("/inboxes")).toHaveTextContent("7");
    expect(navBadge("/reviews")).toBeNull();
  });

  it("shows 99+ when the unread result says more messages exist", () => {
    mockUnreadCount = { count: 99, more: true };
    render(<Sidebar />);

    expect(navBadge("/inboxes")).toHaveTextContent("99+");
  });

  it("shows Inboxes and Pending badges with their own values", () => {
    mockUnreadCount = { count: 12, more: false };
    mockPendingCount = 3;
    render(<Sidebar />);

    expect(navBadge("/inboxes")).toHaveTextContent("12");
    expect(navBadge("/reviews")).toHaveTextContent("3");
  });

  it("shows the pending count badge only when > 0", () => {
    mockPendingCount = 0;
    const { rerender } = render(<Sidebar />);
    expect(navBadge("/reviews")).toBeNull();
    // Re-render with a real count → badge appears in the Pending link.
    mockPendingCount = 3;
    rerender(<Sidebar />);
    expect(navBadge("/reviews")).toHaveTextContent("3");
  });
});

// Pin the active-state contract for the bottom-of-sidebar links
// (Settings, Send feedback). These used to diverge — Settings had full
// active styling, Feedback had none — so a user on /feedback would see
// no nav-highlight cue at all. Pinning both via aria-current=page so
// the asymmetry can't reappear.
describe("Sidebar — bottom-section active state", () => {
  it("highlights Settings when pathname is /settings", () => {
    mockPathname = "/settings";
    render(<Sidebar />);
    const settings = document.querySelector(`a[href="/settings"]`);
    expect(settings).toHaveAttribute("aria-current", "page");
    // Sibling Feedback link must NOT be active.
    const feedback = document.querySelector(`a[href="/feedback"]`);
    expect(feedback).not.toHaveAttribute("aria-current", "page");
  });

  it("highlights Send feedback when pathname is /feedback", () => {
    mockPathname = "/feedback";
    render(<Sidebar />);
    const feedback = document.querySelector(`a[href="/feedback"]`);
    expect(feedback).toHaveAttribute("aria-current", "page");
    // Sibling Settings link must NOT be active.
    const settings = document.querySelector(`a[href="/settings"]`);
    expect(settings).not.toHaveAttribute("aria-current", "page");
  });

  it("leaves both bottom links unmarked when pathname is elsewhere", () => {
    mockPathname = "/inboxes";
    render(<Sidebar />);
    expect(document.querySelector(`a[href="/settings"]`))
      .not.toHaveAttribute("aria-current", "page");
    expect(document.querySelector(`a[href="/feedback"]`))
      .not.toHaveAttribute("aria-current", "page");
  });
});
