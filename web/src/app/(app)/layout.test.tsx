import { render, screen, waitFor, act, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import AppLayout from "./layout";

jest.mock("next/link", () => {
  return function MockLink({ href, children, ...rest }: { href: string; children: React.ReactNode; [k: string]: unknown }) {
    return <a href={href} {...rest}>{children}</a>;
  };
});

let mockAuth: {
  user: { id: string; email: string; name: string; created_at: string } | null;
  loading: boolean;
};
jest.mock("../components/AuthProvider", () => ({
  useAuth: () => mockAuth,
}));

// Keep the layout test focused on the layout itself: the sidebar is mocked
// with two plain links (they double as the drawer's focusable elements),
// and the polling owner — which has its own SWR machinery — is a no-op.
jest.mock("../components/loft/Sidebar", () => ({
  Sidebar: ({ className }: { className?: string }) => (
    // preventDefault keeps jsdom from attempting a real navigation (and
    // logging "Not implemented: navigation") when tests click the links.
    <nav className={className}>
      <a href="/inboxes" onClick={(e) => e.preventDefault()}>Inboxes</a>
      <a href="/domains" onClick={(e) => e.preventDefault()}>Domains</a>
    </nav>
  ),
}));
jest.mock("../components/swr/PendingPollingOwner", () => ({
  PendingPollingOwner: () => null,
}));

const signedIn = {
  user: { id: "usr_1", email: "alice@example.com", name: "Alice", created_at: "2026-01-01T00:00:00Z" },
  loading: false,
};

beforeEach(() => {
  mockAuth = signedIn;
  document.body.style.overflow = "";
  window.history.replaceState(null, "", "/");
});

async function openDrawer() {
  const trigger = screen.getByRole("button", { name: "Open menu" });
  await userEvent.click(trigger);
  const dialog = await screen.findByRole("dialog", { name: "Navigation" });
  // The first focusable element is focused via requestAnimationFrame.
  await waitFor(() => expect(within(dialog).getByText("Inboxes")).toHaveFocus());
  return { trigger, dialog };
}

describe("(app) layout — auth gates", () => {
  it("shows a loading state while the session resolves", () => {
    mockAuth = { user: null, loading: true };
    render(<AppLayout><div>page content</div></AppLayout>);
    expect(screen.getByText("Loading...")).toBeInTheDocument();
    expect(screen.queryByText("page content")).not.toBeInTheDocument();
  });

  it("shows a sign-in prompt instead of the page when signed out", () => {
    mockAuth = { user: null, loading: false };
    render(<AppLayout><div>page content</div></AppLayout>);
    expect(screen.getByText("Sign in to access this page.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Sign in with Google" }))
      .toHaveAttribute("href", "/api/auth/login");
    expect(screen.queryByText("page content")).not.toBeInTheDocument();
  });

  it("preserves a review deep link through sign-in", async () => {
    mockAuth = { user: null, loading: false };
    window.history.replaceState(null, "", "/reviews?id=msg_held");

    render(<AppLayout><div>page content</div></AppLayout>);

    await waitFor(() =>
      expect(screen.getByRole("link", { name: "Sign in with Google" }))
        .toHaveAttribute(
          "href",
          "/api/auth/login?return_to=%2Freviews%3Fid%3Dmsg_held",
        ),
    );
  });

  it("renders children on the authenticated app surface", () => {
    const { container } = render(<AppLayout><div>page content</div></AppLayout>);
    expect(screen.getByText("page content")).toBeInTheDocument();
    expect(container.querySelector("[data-app-surface]")).not.toBeNull();
  });
});

describe("(app) layout — mobile navigation drawer", () => {
  it("opens the drawer as a modal dialog, traps initial focus, and locks body scroll", async () => {
    render(<AppLayout><div>page content</div></AppLayout>);
    const { trigger, dialog } = await openDrawer();

    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(trigger).toHaveAttribute("aria-expanded", "true");
    expect(document.body.style.overflow).toBe("hidden");
  });

  it("closes on Escape, restores focus to the trigger, and unlocks scroll", async () => {
    render(<AppLayout><div>page content</div></AppLayout>);
    const { trigger } = await openDrawer();

    await userEvent.keyboard("{Escape}");

    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Navigation" })).not.toBeInTheDocument(),
    );
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(trigger).toHaveFocus();
    expect(document.body.style.overflow).toBe("");
  });

  it("closes when the backdrop is clicked", async () => {
    render(<AppLayout><div>page content</div></AppLayout>);
    await openDrawer();

    await userEvent.click(screen.getByRole("button", { name: "Close menu" }));
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Navigation" })).not.toBeInTheDocument(),
    );
  });

  it("closes when a navigation link inside the drawer is clicked", async () => {
    render(<AppLayout><div>page content</div></AppLayout>);
    const { dialog } = await openDrawer();

    await userEvent.click(within(dialog).getByText("Domains"));
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Navigation" })).not.toBeInTheDocument(),
    );
  });

  it("wraps Tab from the last focusable element back to the first", async () => {
    render(<AppLayout><div>page content</div></AppLayout>);
    const { dialog } = await openDrawer();

    const last = within(dialog).getByText("Domains");
    act(() => last.focus());
    await userEvent.keyboard("{Tab}");
    expect(within(dialog).getByText("Inboxes")).toHaveFocus();
  });

  it("wraps Shift+Tab from the first focusable element back to the last", async () => {
    render(<AppLayout><div>page content</div></AppLayout>);
    const { dialog } = await openDrawer();

    // Focus starts on the first element (Inboxes) via the rAF in the effect.
    expect(within(dialog).getByText("Inboxes")).toHaveFocus();
    await userEvent.keyboard("{Shift>}{Tab}{/Shift}");
    expect(within(dialog).getByText("Domains")).toHaveFocus();
  });
});
