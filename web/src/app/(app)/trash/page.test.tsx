// Account-wide Trash page contract.
//
// Covers: empty state, trashed-inbox rows (deleted_at + purge countdown),
// Restore → POST /v1/agents/{email}/restore, and the two-click
// "Delete forever" → DELETE …?confirm=DELETE&permanent=true flow.

import { render, screen, waitFor, fireEvent } from "../../../test-utils/swr";
import TrashPage from "./page";

jest.mock("../../components/AuthProvider", () => ({
  useAuth: () => ({ user: { email: "user@example.com" } }),
}));

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

const mockInvalidateAgentUnread = jest.fn();
jest.mock("../../../lib/swrKeys", () => {
  const actual = jest.requireActual("../../../lib/swrKeys");
  return {
    ...actual,
    invalidateAgentUnread: (email: string) => mockInvalidateAgentUnread(email),
  };
});

const mockFetch = jest.fn();
global.fetch = mockFetch;

const trashedAgent = {
  domain: "acme.com",
  email: "support@acme.com",
  name: "support",
  domain_verified: true,
  created_at: "2026-02-15T00:00:00Z",
  deleted_at: new Date(Date.now() - 5 * 24 * 3600 * 1000).toISOString(), // 5d ago
};

function mockTrash(items: unknown[]) {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url === "/v1/agents?deleted=true" && !init?.method) {
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve({ items }),
      });
    }
    return Promise.resolve({
      ok: false,
      status: 404,
      text: () => Promise.resolve("not found"),
    });
  });
}

beforeEach(() => {
  mockFetch.mockReset();
  mockInvalidateAgentUnread.mockReset();
});

describe("TrashPage", () => {
  it("shows the empty state when nothing is trashed", async () => {
    mockTrash([]);
    render(<TrashPage />);
    await waitFor(() => {
      expect(screen.getByTestId("trash-empty")).toBeInTheDocument();
    });
  });

  it("lists trashed inboxes with deletion age and purge countdown", async () => {
    mockTrash([trashedAgent]);
    render(<TrashPage />);
    await waitFor(() => {
      expect(screen.getByTestId("trash-inbox-row")).toBeInTheDocument();
    });
    expect(screen.getByText(/support@acme\.com/)).toBeInTheDocument();
    // Deleted 5 days ago with a 30-day window → 25 days left.
    expect(screen.getByText(/purges in 25d/)).toBeInTheDocument();
  });

  it("Restore POSTs to the restore endpoint", async () => {
    mockInvalidateAgentUnread.mockReturnValue(new Promise(() => {}));
    let restored = false;
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/v1/agents?deleted=true" && !init?.method) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ items: restored ? [] : [trashedAgent] }),
        });
      }
      if (
        url === "/v1/agents/support%40acme.com/restore" &&
        init?.method === "POST"
      ) {
        restored = true;
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ ...trashedAgent, deleted_at: undefined }),
        });
      }
      if (url === "/v1/agents" && !init?.method) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ items: [] }),
        });
      }
      return Promise.resolve({
        ok: false,
        status: 404,
        text: () => Promise.resolve("not found"),
      });
    });

    render(<TrashPage />);
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /^Restore$/ })).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: /^Restore$/ }));
    await waitFor(() => {
      expect(restored).toBe(true);
      expect(mockInvalidateAgentUnread).toHaveBeenCalledWith(
        "support@acme.com",
      );
    });
    await waitFor(() => {
      expect(screen.getByTestId("trash-empty")).toBeInTheDocument();
    });
  });

  it("surfaces a failed restore without invalidating unread counts", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/v1/agents?deleted=true" && !init?.method) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ items: [trashedAgent] }),
        });
      }
      if (
        url === "/v1/agents/support%40acme.com/restore" &&
        init?.method === "POST"
      ) {
        return Promise.resolve({
          ok: false,
          status: 500,
          text: () => Promise.resolve("restore exploded"),
        });
      }
      return Promise.resolve({
        ok: false,
        status: 404,
        text: () => Promise.resolve("not found"),
      });
    });

    render(<TrashPage />);
    fireEvent.click(
      await screen.findByRole("button", { name: /^Restore$/ }),
    );

    await waitFor(() => {
      expect(screen.getByText("restore exploded")).toBeInTheDocument();
    });
    expect(mockInvalidateAgentUnread).not.toHaveBeenCalled();
  });

  it("Delete forever requires a second confirming click", async () => {
    let purgedUrl: string | null = null;
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/v1/agents?deleted=true" && !init?.method) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ items: purgedUrl ? [] : [trashedAgent] }),
        });
      }
      if (init?.method === "DELETE") {
        purgedUrl = url;
        return Promise.resolve({
          ok: true,
          status: 204,
          text: () => Promise.resolve(""),
        });
      }
      return Promise.resolve({
        ok: false,
        status: 404,
        text: () => Promise.resolve("not found"),
      });
    });

    render(<TrashPage />);
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Delete forever/ }),
      ).toBeInTheDocument();
    });

    // First click only arms the button.
    fireEvent.click(screen.getByRole("button", { name: /Delete forever/ }));
    expect(purgedUrl).toBeNull();
    const confirmBtn = await screen.findByRole("button", {
      name: /Click again to confirm/,
    });

    // Second click fires the irreversible delete.
    fireEvent.click(confirmBtn);
    await waitFor(() => {
      expect(purgedUrl).toBe(
        "/v1/agents/support%40acme.com?confirm=DELETE&permanent=true",
      );
    });
    await waitFor(() => {
      expect(screen.getByTestId("trash-empty")).toBeInTheDocument();
    });
    expect(mockInvalidateAgentUnread).not.toHaveBeenCalled();
  });
  it("explains a deferred Delete forever and keeps the inbox in the trash", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/v1/agents?deleted=true" && !init?.method) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ items: [trashedAgent] }),
        });
      }
      if (init?.method === "DELETE") {
        return Promise.resolve({
          ok: true,
          status: 200,
          text: () =>
            Promise.resolve(
              JSON.stringify({
                deleted: true,
                email: "support@acme.com",
                messages_deleted: 0,
                erase_deferred: true,
                purge_after: "2026-10-28T12:00:00Z",
              }),
            ),
        });
      }
      return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve("not found") });
    });

    render(<TrashPage />);
    fireEvent.click(await screen.findByRole("button", { name: /Delete forever/ }));
    fireEvent.click(await screen.findByRole("button", { name: /Click again to confirm/ }));
    const notice = await screen.findByText(/emailed people outside e2a recently/i);
    expect(notice).toHaveTextContent(/stays in the trash until .*2026/i);
    expect(screen.getByTestId("trash-inbox-row")).toBeInTheDocument();
  });
});
